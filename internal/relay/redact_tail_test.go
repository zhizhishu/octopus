package relay

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	openaiInbound "github.com/bestruirui/octopus/internal/transformer/inbound/openai"
	openaiOutbound "github.com/bestruirui/octopus/internal/transformer/outbound/openai"
	"github.com/gin-gonic/gin"
)

// TestRelayRedactRestoreFailureTailEmitsValidJSONError closes matrix gap D01: the
// existing TestRelayRedactAggregatedRestoreFailureFailsClosed drives the attempt
// handler directly and proves the restore failure fails closed (error returned, head
// committed, no placeholder written) — but it never reaches the REQUEST-level tail
// (relay.go:807-830), so "what the client ultimately receives" was unverified. Its
// "no {{Redact:" assertion is vacuous on its own because the handler writes NOTHING
// after the keepalive whitespace, so the substring is absent for free.
//
// A request-level deterministic restore failure is NOT reachable without a production
// hook: the attempt's *redact.Session is private to relayAttempt (created inside
// applyInboundRedaction, closed only at attempt teardown by redactClose via the
// `defer ra.redactClose()` at relay.go:904), and redact.Session.RestoreJSONBody
// (internal/redact/api.go:431) errors ONLY when the session is closed — a JSON-valid
// body carrying an unknown placeholder is passed through, never thrown. So no
// hook-free path can make the request loop's restore step fail, and this test instead
// joins the two real, in-repo halves of the delivery: the actual handler's
// restore-failure error (from a closed session) is fed through the same
// relayErrorResponse + writeNonStreamJSONError the request tail uses, on the SAME
// gin context the keepalive already committed, and the resulting client bytes are
// asserted to be a valid JSON error object with no placeholder and no spliced SSE.
//
// This is the honest degradation the task allows: it locks the tail contract that a
// real restore failure reaches, without inventing a production hook to force the
// request loop itself into that state.
func TestRelayRedactRestoreFailureTailEmitsValidJSONError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupRedactDB(t)
	if err := op.SettingSetString(dbmodel.SettingKeyRelayStreamKeepaliveSec, "1"); err != nil {
		t.Fatalf("set keepalive setting: %v", err)
	}
	if err := op.SettingSetString(dbmodel.SettingKeyRelayStreamDataTimeoutSec, "30"); err != nil {
		t.Fatalf("set stream data timeout setting: %v", err)
	}

	engine, err := sharedRedactEngine()
	if err != nil {
		t.Fatal(err)
	}
	session, err := engine.NewSession("E", "openai_chat", false)
	if err != nil {
		t.Fatal(err)
	}
	token, err := session.RedactText("a@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !redactTokenRe.MatchString(token) {
		t.Fatalf("expected a placeholder token, got %q", token)
	}
	// Deterministic restore failure with no production hook: a closed session errors
	// instead of silently passing the placeholder through. Production never closes the
	// session mid-flight; the same fail-closed branch guards a genuine core throw.
	session.Close()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	ra := &relayAttempt{
		relayRequest: &relayRequest{
			c:            c,
			inboundType:  inbound.InboundTypeOpenAIResponse,
			inAdapter:    &openaiInbound.ResponseInbound{},
			requestModel: "gpt-5.5",
		},
		redactSession: session,
		redactApplied: true,
	}

	pr, pw := io.Pipe()
	defer pr.Close()
	response := &http.Response{
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:   pr,
	}
	// Prelude, then "reason" past the 1s keepalive (flushes "\n", committing the head
	// and setting wroteNonStreamJSONKeepalive), then the terminal completion carrying
	// the placeholder.
	go func() {
		_, _ = io.WriteString(pw, `data: {"type":"response.created","response":{"id":"resp_1","object":"response","created_at":123,"model":"gpt-5.5","status":"in_progress","output":[]}}`+"\n\n")
		time.Sleep(1500 * time.Millisecond)
		_, _ = io.WriteString(pw, `data: {"type":"response.output_text.delta","delta":"`+token+`"}`+"\n\n")
		_, _ = io.WriteString(pw, `data: {"type":"response.completed","response":{"id":"resp_1","object":"response","created_at":123,"model":"gpt-5.5","status":"completed","output":[{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"`+token+`"}]}],"usage":{"input_tokens":2,"input_tokens_details":{"cached_tokens":0},"output_tokens":1,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":3}}}`+"\n\n")
		_, _ = io.WriteString(pw, `data: [DONE]`+"\n\n")
		_ = pw.Close()
	}()

	attemptErr := ra.handleStreamResponseAsNonStream(c.Request.Context(), response, &openaiOutbound.ResponseOutbound{}, 0)
	if attemptErr == nil {
		t.Fatalf("restore failure after keepalive commit must fail closed, got nil error (body=%q)", rec.Body.String())
	}
	if !strings.Contains(attemptErr.Error(), "redact: restored aggregated inbound response failed") {
		t.Fatalf("unexpected handler error: %v", attemptErr)
	}
	// Fixture invariants: the blank-line keepalive committed the head and flipped the
	// flag, and the unrecovered body was NOT written (no placeholder on the wire yet).
	if !ra.wroteNonStreamJSONKeepalive {
		t.Fatalf("expected the keepalive path to have committed the response head")
	}
	if !c.Writer.Written() {
		t.Fatalf("keepalive must have committed c.Writer")
	}
	if strings.Contains(rec.Body.String(), "{{Redact:") {
		t.Fatalf("unrecovered placeholder leaked to the client: %q", rec.Body.String())
	}

	// --- The exact request-level tail (relay.go:807-830). With the head already
	// committed and wroteNonStreamJSONKeepalive true, the all-channels-failed path maps
	// the terminal error to a public code/message and appends an in-band JSON error body
	// instead of splicing an SSE frame into the application/json stream. ---
	_, code, message := relayErrorResponse(attemptErr)
	if code != "octopus_all_channels_failed" {
		t.Fatalf("a restore failure must map to the generic all-channels-failed code, got %q", code)
	}
	writeNonStreamJSONError(c, code, message)

	// The client's final bytes: keepalive whitespace + one JSON error object.
	raw := rec.Body.String()
	var parsed map[string]any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("client body must be valid JSON, got %q: %v", raw, err)
	}
	errObj, ok := parsed["error"].(map[string]any)
	if !ok {
		t.Fatalf("client body must carry a top-level error object, got %q", raw)
	}
	if errObj["message"] != message {
		t.Fatalf("error.message mismatch: got %v want %q (body=%q)", errObj["message"], message, raw)
	}
	if errObj["type"] != "upstream_error" {
		t.Fatalf("error.type mismatch: got %v want %q (body=%q)", errObj["type"], "upstream_error", raw)
	}
	if errObj["code"] != code {
		t.Fatalf("error.code mismatch: got %v want %q (body=%q)", errObj["code"], code, raw)
	}
	// No placeholder may survive, and nothing SSE-shaped may be spliced into the JSON.
	if strings.Contains(raw, "{{Redact:") {
		t.Fatalf("placeholder leaked to the client: %q", raw)
	}
	if strings.Contains(raw, "data:") || strings.Contains(raw, "event:") {
		t.Fatalf("SSE frame spliced into a non-stream JSON body: %q", raw)
	}
	// Everything before the JSON payload is insignificant JSON whitespace only (the
	// keepalive's "\n"), so the whole body is one valid document for the client parser.
	if trimmed := strings.TrimLeft(raw, "\n\r\t "); !strings.HasPrefix(trimmed, "{") {
		t.Fatalf("leading bytes must be insignificant JSON whitespace only: %q", raw)
	}
}
