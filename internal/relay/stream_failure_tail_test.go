package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	"github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/gin-gonic/gin"
)

// --- R3b: restoreClientStreamSse must preserve SSE "\n\n" event separators ---

// TestRestoreClientStreamSseKeepsEventSeparator proves defect F3: after
// splitClientSseEvents cuts on "\n\n", the per-blob text no longer carries the
// trailing blank line, so the comment/heartbeat branch must re-add it. Without the
// fix two consecutive events are concatenated into one broken block
// (": pingdata: {...}") and the client's SSE parser never sees the second event.
func TestRestoreClientStreamSseKeepsEventSeparator(t *testing.T) {
	setupRedactDB(t)

	engine, err := sharedRedactEngine()
	if err != nil {
		t.Fatal(err)
	}
	session, err := engine.NewSession("E", "openai_chat", false)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	secret := "a@example.com"
	token, err := session.RedactText(secret)
	if err != nil {
		t.Fatal(err)
	}
	if !redactTokenRe.MatchString(token) {
		t.Fatalf("expected a placeholder token, got %q", token)
	}

	ra := &relayAttempt{redactSession: session, redactApplied: true}
	data := `data: {"choices":[{"delta":{"content":"` + token + `"}}]}`
	in := ": ping\n\n" + data + "\n\n"

	out, err := ra.restoreClientStreamSse([]byte(in))
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, ": ping\n\n") {
		t.Fatalf("comment event lost its blank-line separator (F3): %q", got)
	}
	if strings.Contains(got, token) {
		t.Fatalf("placeholder leaked instead of being restored: %q", got)
	}
	if !strings.Contains(got, secret) {
		t.Fatalf("restored secret missing: %q", got)
	}
}

// --- R3c: handleNonStreamResponseAsStream must flush the redact tail buffer ---

// TestHandleNonStreamResponseAsStreamFlushesRedactTail proves defect F4: when the
// synthesized stream's last data event ends with a HALF placeholder, the restorer
// buffers that whole channel (it can only know at finish() that no closing half is
// coming). The fallback path never calls finish(), so the buffered text — including
// the real "hello " prefix it was coalesced with — is silently dropped. The test
// drives the fallback path with a redact session and asserts the buffered tail
// reaches the client.
func TestHandleNonStreamResponseAsStreamFlushesRedactTail(t *testing.T) {
	setupRedactDB(t)

	engine, err := sharedRedactEngine()
	if err != nil {
		t.Fatal(err)
	}
	session, err := engine.NewSession("E", "openai_chat", false)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	secret := "a@example.com"
	token, err := session.RedactText(secret)
	if err != nil {
		t.Fatal(err)
	}
	if !redactTokenRe.MatchString(token) {
		t.Fatalf("expected a placeholder token, got %q", token)
	}
	// Half of the placeholder sits at the very tail of the model text: the restorer
	// holds the channel awaiting the closing half that never arrives.
	half := token[:30]
	content := "hello " + half

	body, err := json.Marshal(map[string]any{
		"id":      "chatcmpl-fb",
		"object":  "chat.completion",
		"model":   "upstream-model",
		"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
	})
	if err != nil {
		t.Fatal(err)
	}

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}

	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	stream := true
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			c:               c,
			inboundType:     inbound.InboundTypeOpenAIChat,
			inAdapter:       inbound.Get(inbound.InboundTypeOpenAIChat),
			internalRequest: &model.InternalLLMRequest{Model: "upstream-model", Stream: &stream},
		},
		channel:       &dbmodel.Channel{Type: outbound.OutboundTypeOpenAIChat},
		redactSession: session,
		redactApplied: true,
	}

	if err := ra.handleNonStreamResponseAsStream(context.Background(), resp, outbound.Get(outbound.OutboundTypeOpenAIChat)); err != nil {
		t.Fatalf("fallback path returned error: %v", err)
	}

	got := rec.Body.String()
	if !strings.Contains(got, "hello ") {
		t.Fatalf("redact tail buffer dropped the real content text (F4): %q", got)
	}
}

// --- R3a: committed CHAT stream failure must emit an in-band error frame ---

// TestChatStreamErrorAfterContentEmitsErrorFrame proves defect F5: once an OpenAI
// chat stream has committed real content, a later upstream failure cannot fail over,
// so it must be surfaced in-band (exactly what the Responses/Anthropic branches of
// the committed switch already did). Before the fix the chat client received a silent
// truncated stream — partial content, no error, no [DONE].
func TestChatStreamErrorAfterContentEmitsErrorFrame(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			`data: {"id":"chatcmpl_err","object":"chat.completion.chunk","model":"chat-error-model","choices":[{"index":0,"delta":{"role":"assistant","content":"partial"},"finish_reason":null}]}` + "\n\n" +
				`data: {"error":{"message":"upstream boom after content","type":"upstream_error"}}` + "\n\n"))
	}))
	t.Cleanup(upstream.Close)

	channel := dbmodel.Channel{
		Name:         "Chat-Error-After-Content",
		Type:         outbound.OutboundTypeOpenAIChat,
		Enabled:      true,
		BaseUrls:     []dbmodel.BaseUrl{{URL: upstream.URL}},
		Keys:         []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "chat-key"}},
		Model:        "chat-error-model",
		ModelMapping: map[string]string{"chat-error-after-content": "chat-error-model"},
		Priority:     1,
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"chat-error-after-content","stream":true,"messages":[{"role":"user","content":"ping"}]}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	Handler(inbound.InboundTypeOpenAIChat, c)

	body := rec.Body.String()
	if !strings.Contains(body, "partial") {
		t.Fatalf("expected partial content before error, got %s", body)
	}
	if !strings.Contains(body, `"error"`) || !strings.Contains(body, "upstream_error") {
		t.Fatalf("committed chat stream failure must emit an in-band error frame (F5), got %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("committed chat stream failure must terminate the client stream with [DONE], got %s", body)
	}
}

// --- R4 (F9): an unreadable master switch must be treated as ON (fail-closed) ---

// TestRedactGlobalEnabledReadErrorFailsClosed proves defect F9: when the master
// switch setting cannot be read, protection must not silently fall OFF. The two
// read-error shapes are (a) a never-configured key — SettingGetBool returns
// (false, "setting not found"), NOT (default, nil) — and (b) an unparseable stored
// value (strconv.ParseBool error); both hit redactGlobalEnabled's error branch. The
// test injects shape (b) (the relay test package cannot delete a cache key) and
// asserts the switch is treated as ON, while the per-channel opt-in still gates and
// an explicit OFF is still honoured.
func TestRedactGlobalEnabledReadErrorFailsClosed(t *testing.T) {
	setupRedactDB(t)

	mkReq := func(sticky bool) *relayRequest {
		r := &relayRequest{
			inboundType:     inbound.InboundTypeOpenAIChat,
			internalRequest: &model.InternalLLMRequest{RawRequest: []byte(`{"messages":[]}`)},
		}
		r.redactRequired = sticky
		return r
	}

	// Unreadable master switch: strconv.ParseBool rejects the stored value, so
	// SettingGetBool returns (false, err).
	if err := op.SettingSetString(dbmodel.SettingKeyRedactEnabled, "not-a-bool"); err != nil {
		t.Fatal(err)
	}

	// Read error + channel opt-in => treated as ON.
	withOptIn := &relayAttempt{relayRequest: mkReq(false), channel: &dbmodel.Channel{RedactEnabled: true, RedactFlags: "E"}}
	if !withOptIn.inboundRedactActive() {
		t.Fatal("read failure must fail CLOSED: opted-in channel must stay active")
	}
	// ... but the opt-in still gates: no opt-in stays inactive (no cleartext leak is
	// introduced for channels that never asked for redaction).
	noOptIn := &relayAttempt{relayRequest: mkReq(false), channel: &dbmodel.Channel{RedactEnabled: false, RedactFlags: "E"}}
	if noOptIn.inboundRedactActive() {
		t.Fatal("read failure must not activate a channel that did not opt in")
	}
	// Sticky requests are unaffected by the read failure.
	sticky := &relayAttempt{relayRequest: mkReq(true), channel: &dbmodel.Channel{RedactEnabled: false, RedactFlags: "E"}}
	if !sticky.inboundRedactActive() {
		t.Fatal("sticky protection must hold regardless of the master-switch read failure")
	}

	// An explicit, readable OFF is still honoured (fail-closed only covers UNDECIDABLE).
	if err := op.SettingSetString(dbmodel.SettingKeyRedactEnabled, "false"); err != nil {
		t.Fatal(err)
	}
	explicitOff := &relayAttempt{relayRequest: mkReq(false), channel: &dbmodel.Channel{RedactEnabled: true, RedactFlags: "E"}}
	if explicitOff.inboundRedactActive() {
		t.Fatal("explicit master-off must keep a new request inactive")
	}
	// Explicit readable ON + opt-in sanity.
	if err := op.SettingSetString(dbmodel.SettingKeyRedactEnabled, "true"); err != nil {
		t.Fatal(err)
	}
	explicitOn := &relayAttempt{relayRequest: mkReq(false), channel: &dbmodel.Channel{RedactEnabled: true, RedactFlags: "E"}}
	if !explicitOn.inboundRedactActive() {
		t.Fatal("explicit master-on + opt-in must be active")
	}
}
