package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/transformer/inbound"
	"github.com/zhizhishu/octopus/internal/transformer/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// A native /v1/messages body carrying {"type":"enabled","budget_tokens":N} for an adaptive-only
// model is a guaranteed upstream 400 ("claude-opus-5-5 requires adaptive thinking"). The relay may
// rewrite it ONLY for a caller positively known not to be the Claude Code CLI; a CLI-shaped caller
// (claude-cli/ UA or claude-code beta) must reach the upstream byte-identical.
func runAdaptiveCallerStep(t *testing.T, headers map[string]string) (*relayAttempt, map[string]json.RawMessage) {
	return runAdaptiveCallerStepWith(t, headers, "claude-opus-5-5", `{"type":"enabled","budget_tokens":10000}`)
}

func runAdaptiveCallerStepWith(t *testing.T, headers map[string]string, modelName, thinking string) (*relayAttempt, map[string]json.RawMessage) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	for k, v := range headers {
		httpReq.Header.Set(k, v)
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httpReq

	text := "ping"
	stream := true
	req := &model.InternalLLMRequest{
		Model:             modelName,
		RawAPIFormat:      model.APIFormatAnthropicMessage,
		Stream:            &stream,
		Messages:          []model.Message{{Role: "user", Content: model.MessageContent{Content: &text}}},
		AnthropicThinking: json.RawMessage(thinking),
	}
	ra := &relayAttempt{
		relayRequest: &relayRequest{c: c, internalRequest: req, inboundType: inbound.InboundTypeAnthropic, requestModel: modelName},
		channel:      &dbmodel.Channel{Type: outbound.OutboundTypeAnthropic, Cloak: dbmodel.ChannelCloak{Mode: "never"}},
	}
	if err := ra.applyTransformOptionsWithInboundSetter(false); err != nil {
		t.Fatalf("applyTransformOptions: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(outboundBody(t, ra), &decoded); err != nil {
		t.Fatalf("decode outbound body: %v", err)
	}
	return ra, decoded
}

func TestAdaptiveOnlyNativeEnabledCLICallerUntouched(t *testing.T) {
	setupRescueDeadlineDB(t)
	for name, headers := range map[string]map[string]string{
		"claude-cli UA":    {"User-Agent": "claude-cli/2.1.296 (external, cli)"},
		"claude-code beta": {"User-Agent": "node", "Anthropic-Beta": "claude-code-20250219,interleaved-thinking-2025-05-14"},
		"UA and beta both": {"User-Agent": "claude-cli/2.1.296 (external, sdk-cli)", "Anthropic-Beta": "claude-code-20250219"},
	} {
		t.Run(name, func(t *testing.T) {
			ra, body := runAdaptiveCallerStep(t, headers)
			if ra.internalRequest.TransformOptions.ClaudeNonCLICaller {
				t.Fatal("CLI-shaped caller classified as non-CLI")
			}
			if got := string(body["thinking"]); got != `{"type":"enabled","budget_tokens":10000}` {
				t.Fatalf("CLI caller's thinking was rewritten: %s", got)
			}
			if _, ok := body["output_config"]; ok {
				t.Fatalf("output_config synthesised for a CLI caller: %s", body["output_config"])
			}
		})
	}
}

func TestAdaptiveOnlyNativeEnabledNonCLICallerRewritten(t *testing.T) {
	setupRescueDeadlineDB(t)
	ra, body := runAdaptiveCallerStep(t, map[string]string{"User-Agent": "python-requests/2.31.0"})
	if !ra.internalRequest.TransformOptions.ClaudeNonCLICaller {
		t.Fatal("non-CLI caller not classified")
	}
	if got := string(body["thinking"]); got != `{"type":"adaptive"}` {
		t.Fatalf("thinking = %s, want adaptive", got)
	}
	if got := string(body["output_config"]); got != `{"effort":"high"}` {
		t.Fatalf("output_config = %s, want effort high (budget 10000)", got)
	}
}

// Native raw disabled on a model that cannot turn thinking off: rewritten to the model's own
// off form only for a positively non-CLI caller; CLI-shaped callers keep disabled verbatim.
// (outboundBody targets a non-official base, so the api.anthropic.com drop does not apply.)
func TestAdaptiveOnlyNativeDisabledByCaller(t *testing.T) {
	setupRescueDeadlineDB(t)
	cli := map[string]string{"User-Agent": "claude-cli/2.1.296 (external, cli)"}
	plain := map[string]string{"User-Agent": "python-requests/2.31.0"}
	for _, tc := range []struct {
		name, model      string
		headers          map[string]string
		wantThinking     string
		wantOutputConfig string
	}{
		{"CLI opus-5-5", "claude-opus-5-5", cli, `{"type":"disabled"}`, ""},
		{"CLI sonnet-5-5", "claude-sonnet-5-5", cli, `{"type":"disabled"}`, ""},
		{"non-CLI opus-5-5", "claude-opus-5-5", plain, `{"type":"adaptive"}`, `{"effort":"low"}`},
		{"non-CLI sonnet-5-5", "claude-sonnet-5-5", plain, `{"type":"between_tools"}`, `{"effort":"low"}`},
		{"non-CLI opus-4-5", "claude-opus-4-5", plain, `{"type":"disabled"}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, body := runAdaptiveCallerStepWith(t, tc.headers, tc.model, `{"type":"disabled"}`)
			if got := string(body["thinking"]); got != tc.wantThinking {
				t.Fatalf("thinking = %s, want %s", got, tc.wantThinking)
			}
			if got := string(body["output_config"]); got != tc.wantOutputConfig {
				t.Fatalf("output_config = %s, want %s", got, tc.wantOutputConfig)
			}
		})
	}
}
