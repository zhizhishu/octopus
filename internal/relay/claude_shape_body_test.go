package relay

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

// A genuine Claude CLI sends thinking / context_management / output_config on every request
// (22/22 in the archived golden captures). A non-CLI caller does not, so the relay used to emit a
// body that was missing keys the upstream expects from a CLI-shaped client. These tests pin the
// completion, the values, and — just as important — everything it must NOT touch.

func claudeShapeAttempt(t *testing.T, userAgent string, channelType outbound.OutboundType) *relayAttempt {
	t.Helper()
	gin.SetMode(gin.TestMode)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = req
	text := "hi"
	streaming := true
	return &relayAttempt{
		relayRequest: &relayRequest{
			c: c,
			internalRequest: &model.InternalLLMRequest{
				Model:    "claude-sonnet-example",
				Stream:   &streaming,
				Messages: []model.Message{{Role: "user", Content: model.MessageContent{Content: &text}}},
			},
			requestModel: "claude-sonnet-example",
		},
		channel: &dbmodel.Channel{Type: channelType},
	}
}

func TestNonCLICallerGetsTheClaudeCLIShapeTopLevelKeys(t *testing.T) {
	setupRescueDeadlineDB(t)
	if err := op.SettingSetString(dbmodel.SettingKeyRelayClaudeCLIShapeKeys, "true"); err != nil {
		t.Fatalf("enable switch: %v", err)
	}

	ra := claudeShapeAttempt(t, "python-requests/2.31.0", outbound.OutboundTypeAnthropic)
	ra.ensureClaudeCLIShapeTopLevelKeys()

	// Values are the measured golden ones; comparing bytes keeps an invented value from passing.
	if got, want := string(ra.internalRequest.AnthropicThinking), `{"type":"adaptive","display":"omitted"}`; got != want {
		t.Errorf("thinking = %s, want %s", got, want)
	}
	if got, want := string(ra.internalRequest.AnthropicContextManagement), `{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]}`; got != want {
		t.Errorf("context_management = %s, want %s", got, want)
	}
	if got, want := string(ra.internalRequest.AnthropicOutputConfig), `{"effort":"high"}`; got != want {
		t.Errorf("output_config = %s, want %s", got, want)
	}
}

// The point of the whole exercise: what actually leaves for an Anthropic upstream.
func TestNonCLICallerOutboundCarriesTheCLIShapedKeyOrder(t *testing.T) {
	setupRescueDeadlineDB(t)
	if err := op.SettingSetString(dbmodel.SettingKeyRelayClaudeCLIShapeKeys, "true"); err != nil {
		t.Fatalf("enable switch: %v", err)
	}

	ra := claudeShapeAttempt(t, "python-requests/2.31.0", outbound.OutboundTypeAnthropic)
	ra.ensureClaudeCLIShapeTopLevelKeys()

	body := outboundBody(t, ra)
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("outbound body is not JSON: %v (%s)", err, body)
	}
	for _, key := range []string{"thinking", "context_management", "output_config"} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("outbound body is missing %q: %s", key, body)
		}
	}
	// 2026-10-10 大领导口径更新: safeguards 也补(真 CLI 22 份抓包里 12 份带它 = 54.5%, 隔次出现;
	// 值取黄金样本, 只有 platform 从我们对外宣称的指纹派生)。tool_choice/temperature/top_p 仍不补(0/22)。
	if _, ok := decoded["safeguards"]; !ok {
		t.Errorf("outbound body is missing \"safeguards\" (the golden CLI samples carry it in 12/22 captures): %s", body)
	} else if !strings.Contains(string(body), `"platform":"`+strings.ToLower(settingString(dbmodel.SettingKeyClaudeHeaderOS, dbmodel.DefaultClaudeHeaderOS))+`"`) {
		t.Errorf("safeguards.platform must be the lowercased advertised platform (a genuine CLI writes its GOOS in lowercase): %s", body)
	}
	for _, key := range []string{"tool_choice", "temperature", "top_p"} {
		if _, ok := decoded[key]; ok {
			t.Errorf("outbound body must not add %q (a real CLI sends it in 0%% of captures): %s", key, body)
		}
	}
	// The measured CLI order, with `system` present because the relay injects the agent-identity
	// system block for a non-CLI caller (a separate, pre-existing behaviour).
	if got, want := topLevelKeyOrder(t, body), []string{"model", "messages", "system", "max_tokens", "thinking", "context_management", "safeguards", "output_config", "stream"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("outbound key order = %v, want the CLI order %v", got, want)
	}
}

func TestCLIShapedCallerBodyIsLeftAlone(t *testing.T) {
	setupRescueDeadlineDB(t)
	if err := op.SettingSetString(dbmodel.SettingKeyRelayClaudeCLIShapeKeys, "true"); err != nil {
		t.Fatalf("enable switch: %v", err)
	}

	ra := claudeShapeAttempt(t, "claude-cli/2.1.294 (external, sdk-cli)", outbound.OutboundTypeAnthropic)
	ra.ensureClaudeCLIShapeTopLevelKeys()
	if len(ra.internalRequest.AnthropicThinking) != 0 || len(ra.internalRequest.AnthropicContextManagement) != 0 || len(ra.internalRequest.AnthropicOutputConfig) != 0 {
		t.Fatalf("a CLI-shaped caller owns its shape; the relay must not add anything (thinking=%s context=%s output=%s)",
			ra.internalRequest.AnthropicThinking, ra.internalRequest.AnthropicContextManagement, ra.internalRequest.AnthropicOutputConfig)
	}
}

func TestClaudeShapeCompletionRespectsTheSwitchAndCallerValues(t *testing.T) {
	setupRescueDeadlineDB(t)

	// Switch off: exactly what the caller asked for goes out, nothing added.
	if err := op.SettingSetString(dbmodel.SettingKeyRelayClaudeCLIShapeKeys, "false"); err != nil {
		t.Fatalf("disable switch: %v", err)
	}
	off := claudeShapeAttempt(t, "python-requests/2.31.0", outbound.OutboundTypeAnthropic)
	off.ensureClaudeCLIShapeTopLevelKeys()
	if len(off.internalRequest.AnthropicThinking) != 0 || len(off.internalRequest.AnthropicContextManagement) != 0 || len(off.internalRequest.AnthropicOutputConfig) != 0 {
		t.Fatalf("with the switch off nothing may be added")
	}

	if err := op.SettingSetString(dbmodel.SettingKeyRelayClaudeCLIShapeKeys, "true"); err != nil {
		t.Fatalf("enable switch: %v", err)
	}
	// Caller-supplied values survive: the completion fills gaps, it does not overwrite.
	keep := claudeShapeAttempt(t, "python-requests/2.31.0", outbound.OutboundTypeAnthropic)
	keep.internalRequest.AnthropicThinking = json.RawMessage(`{"type":"enabled","budget_tokens":4096}`)
	keep.internalRequest.AnthropicOutputConfig = json.RawMessage(`{"effort":"low"}`)
	keep.ensureClaudeCLIShapeTopLevelKeys()
	if got := string(keep.internalRequest.AnthropicThinking); got != `{"type":"enabled","budget_tokens":4096}` {
		t.Errorf("caller thinking was overwritten: %s", got)
	}
	if got := string(keep.internalRequest.AnthropicOutputConfig); got != `{"effort":"low"}` {
		t.Errorf("caller output_config was overwritten: %s", got)
	}
	if len(keep.internalRequest.AnthropicContextManagement) == 0 {
		t.Errorf("the absent member must still be filled")
	}

	// Another protocol's channel is none of this function's business.
	other := claudeShapeAttempt(t, "python-requests/2.31.0", outbound.OutboundTypeOpenAIChat)
	other.ensureClaudeCLIShapeTopLevelKeys()
	if len(other.internalRequest.AnthropicThinking) != 0 {
		t.Errorf("a non-Anthropic channel must be untouched")
	}
}

// A non-CLI caller can express thinking/effort through the internal ReasoningEffort field instead
// of a raw Anthropic object. The outbound derives the same members from it, so the completion must
// not pre-empt that with the golden constants — otherwise the caller's effort silently becomes high.
func TestClaudeShapeCompletionDoesNotOverrideCallerEffort(t *testing.T) {
	setupRescueDeadlineDB(t)
	if err := op.SettingSetString(dbmodel.SettingKeyRelayClaudeCLIShapeKeys, "true"); err != nil {
		t.Fatalf("enable switch: %v", err)
	}

	ra := claudeShapeAttempt(t, "python-requests/2.31.0", outbound.OutboundTypeAnthropic)
	ra.internalRequest.ReasoningEffort = "medium"
	ra.internalRequest.AdaptiveThinking = true
	ra.ensureClaudeCLIShapeTopLevelKeys()

	if len(ra.internalRequest.AnthropicThinking) != 0 || len(ra.internalRequest.AnthropicOutputConfig) != 0 {
		t.Fatalf("the caller's own effort form must be left to the outbound: thinking=%s output=%s",
			ra.internalRequest.AnthropicThinking, ra.internalRequest.AnthropicOutputConfig)
	}
	if len(ra.internalRequest.AnthropicContextManagement) == 0 {
		t.Fatalf("the still-absent member must be filled")
	}
	body := string(outboundBody(t, ra))
	if !strings.Contains(body, `"effort":"medium"`) {
		t.Errorf("the caller's effort must survive: %s", body)
	}
	if strings.Contains(body, `"display":"omitted"`) {
		t.Errorf("the golden thinking must not pre-empt the caller's effort form: %s", body)
	}
}

func outboundBody(t *testing.T, ra *relayAttempt) []byte {
	t.Helper()
	req, err := outbound.Get(outbound.OutboundTypeAnthropic).TransformRequest(
		context.Background(), ra.internalRequest, "http://upstream.example", "sk-test")
	if err != nil {
		t.Fatalf("outbound TransformRequest: %v", err)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read outbound body: %v", err)
	}
	return body
}

// topLevelKeyOrder reads the member order straight out of the emitted bytes, so a reordering
// cannot hide behind a decoded map. At depth 1 the decoder alternates key, value, key, value —
// tracking that alternation is what keeps a string *value* (e.g. the model name) out of the list.
func topLevelKeyOrder(t *testing.T, body []byte) []string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(string(body)))
	if _, err := dec.Token(); err != nil { // the root '{'
		t.Fatalf("decode body: %v", err)
	}
	var order []string
	depth := 1
	expectKey := true
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("decode token: %v", err)
		}
		switch v := tok.(type) {
		case json.Delim:
			if v == '{' || v == '[' {
				depth++
				continue
			}
			depth--
			if depth == 1 {
				// A container acting as a value just finished.
				expectKey = true
			}
			if depth == 0 {
				return order
			}
		case string:
			if depth != 1 {
				continue
			}
			if expectKey {
				order = append(order, v)
				expectKey = false
			} else {
				expectKey = true
			}
		default:
			if depth == 1 {
				expectKey = true
			}
		}
	}
	return order
}
