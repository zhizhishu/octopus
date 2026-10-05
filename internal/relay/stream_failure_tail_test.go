package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// --- R3c: the non-stream→stream fallback must flush the redact tail buffer ---

// runFallbackWithHeldContent drives handleNonStreamResponseAsStream (the anthropic
// stream→non-stream retry fallback) with a redact session whose single assistant
// message holds "hello " + content(token) as its tail. The restorer buffers that
// channel (it can only know at finish() that no closing half is coming), so the
// fallback's end-of-stream flush is the only thing that can emit it.
func runFallbackWithHeldContent(t *testing.T, content func(token string) string) (rec *httptest.ResponseRecorder, token string, err error) {
	t.Helper()
	setupRedactDB(t)

	engine, cerr := sharedRedactEngine()
	if cerr != nil {
		t.Fatal(cerr)
	}
	session, cerr := engine.NewSession("E", "openai_chat", false)
	if cerr != nil {
		t.Fatal(cerr)
	}
	t.Cleanup(session.Close)

	token, cerr = session.RedactText("a@example.com")
	if cerr != nil {
		t.Fatal(cerr)
	}
	if !redactTokenRe.MatchString(token) {
		t.Fatalf("expected a placeholder token, got %q", token)
	}

	body, cerr := json.Marshal(map[string]any{
		"id":      "chatcmpl-fb",
		"object":  "chat.completion",
		"model":   "upstream-model",
		"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": content(token)}, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
	})
	if cerr != nil {
		t.Fatal(cerr)
	}

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}

	gin.SetMode(gin.TestMode)
	rec = httptest.NewRecorder()
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
	err = ra.handleNonStreamResponseAsStream(context.Background(), resp, outbound.Get(outbound.OutboundTypeOpenAIChat))
	return rec, token, err
}

// S (safe tail): a placeholder-SHAPED literal that matches no registered token must
// still succeed with the real content flushed byte-complete before the terminal
// marker — the original F4 regression (buffered tail silently dropped) must stay
// covered, not just replaced by a nil→non-nil flip.
func TestNonStreamToStreamFallbackEmitsSafeLiteralTail(t *testing.T) {
	rec, token, err := runFallbackWithHeldContent(t, func(tok string) string {
		return "hello " + nonRegisteredPlaceholderLiteral(t, tok)
	})
	if err != nil {
		t.Fatalf("a safe literal tail must NOT fail the fallback: %v", err)
	}
	body := rec.Body.String()
	lit := nonRegisteredPlaceholderLiteral(t, token)

	if !strings.Contains(body, "hello ") {
		t.Fatalf("fallback dropped the real content coalesced with the tail (F4): %q", body)
	}
	if got := strings.Count(body, lit); got != 1 {
		t.Fatalf("safe literal tail must reach the client exactly once, byte-complete, got %d: %q", got, body)
	}
	if leaksRegisteredTokenFragment(body, token) {
		t.Fatalf("registered token fragment leaked to client: %q", body)
	}
	// Normal completion must still be intact (the flush must not eat the terminal).
	// This function writes the finish chunk itself; the caller appends "data: [DONE]".
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("successful fallback must keep its terminal marker (F4 regression): %q", body)
	}
}

// H (fail-closed): when the tail is a HALF of THIS session's REGISTERED placeholder,
// finish() fails explicitly and the fallback must fail closed — no fragment of the
// registered token on the wire, no success termination, and a non-nil error so the
// caller surfaces the failure instead of a truncated silent success.
func TestNonStreamToStreamFallbackHalfTokenFailsClosed(t *testing.T) {
	rec, token, err := runFallbackWithHeldContent(t, func(tok string) string {
		return "hello " + tok[:30]
	})
	body := rec.Body.String()
	t.Logf("fail-closed body=%q err=%v", body, err)

	if err == nil {
		t.Fatalf("a half registered placeholder at finish must fail the fallback explicitly, got nil error: %q", body)
	}
	if leaksRegisteredTokenFragment(body, token) {
		t.Fatalf("half registered token leaked to client: %q", body)
	}
	if strings.Contains(body, "data: [DONE]") || strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("fail-closed must not emit a success termination: %q", body)
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

// --- R3d: a committed stream failure must not silently drop the buffered redact tail ---

// minRegisteredTokenFragment is the shortest run of THIS session's registered token
// treated as an unambiguous leak. It must stay above len("{{Redact:") (9) so a
// legitimate literal that merely shares the placeholder opening is not flagged.
const minRegisteredTokenFragment = 13

// leaksRegisteredTokenFragment reports whether wire contains the session's registered
// token, or a long-enough prefix of it (>= minRegisteredTokenFragment). It is scoped
// to the token registered by THIS session, so a literal that merely looks like a
// placeholder (e.g. "{{Redact:" + unrelated hex) is NOT a false positive — unlike a
// naive strings.Contains(wire, "{{Redact:") ban, which would reject legitimate text.
func leaksRegisteredTokenFragment(wire, token string) bool {
	if token == "" {
		return false
	}
	for n := len(token); n >= minRegisteredTokenFragment; n-- {
		if strings.Contains(wire, token[:n]) {
			return true
		}
	}
	return false
}

// nonRegisteredPlaceholderLiteral derives a placeholder-shaped literal from a real
// token: same length/format, but the FIRST hex digit is flipped, so it shares only
// the 9-char "{{Redact:" opening with the token and matches NO registered token. The
// restorer must therefore treat it as ordinary literal text (a SAFE tail).
func nonRegisteredPlaceholderLiteral(t *testing.T, token string) string {
	t.Helper()
	if len(token) < 57 || !redactTokenRe.MatchString(token) {
		t.Fatalf("bad token fixture: %q", token)
	}
	flipped := byte('0')
	if token[9] == '0' {
		flipped = 'f'
	}
	lit := token[:9] + string(flipped) + token[10:57]
	if strings.HasPrefix(token, lit) {
		t.Fatalf("fixture invalid: literal is still a registered prefix: %q", lit)
	}
	if leaksRegisteredTokenFragment(lit, token) {
		t.Fatalf("fixture invalid: literal embeds a long registered prefix: %q", lit)
	}
	if !strings.HasPrefix(lit, "{{Redact:") {
		t.Fatalf("fixture invalid: literal must look like a placeholder: %q", lit)
	}
	return lit
}

// buildCommittedRedactFailureAttempt builds an already-committed Chat stream whose
// redaction restorer is holding real model text ("hello " prefix + caller-supplied
// tail) in a single channel. It mirrors the stream loop: a token-free safe block is
// emitted first (committing the stream, so failover is no longer possible), then the
// held event is fed through restoreClientStreamSse — which holds the whole channel
// back (returns ""). closeSession lets a test break the restorer.
func buildCommittedRedactFailureAttempt(t *testing.T, heldContent func(token string) string) (ra *relayAttempt, rec *httptest.ResponseRecorder, token string, closeSession func()) {
	t.Helper()
	setupRedactDB(t)

	engine, err := sharedRedactEngine()
	if err != nil {
		t.Fatal(err)
	}
	session, err := engine.NewSession("E", "openai_chat", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(session.Close)

	secret := "a@example.com"
	token, err = session.RedactText(secret)
	if err != nil {
		t.Fatal(err)
	}
	if !redactTokenRe.MatchString(token) {
		t.Fatalf("expected a placeholder token, got %q", token)
	}
	if len(token) < 30 {
		t.Fatalf("token too short to split into a half-placeholder fixture: %q", token)
	}

	gin.SetMode(gin.TestMode)
	rec = httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	ra = &relayAttempt{
		relayRequest:  &relayRequest{c: c, inboundType: inbound.InboundTypeOpenAIChat},
		redactSession: session,
		redactApplied: true,
	}

	// 1) token-free safe block: the restorer passes it straight through, so it reaches
	// the client and commits the stream exactly like a real content chunk would.
	safe, err := ra.restoreClientStreamSse([]byte(`data: {"choices":[{"delta":{"content":"safe "}}]}` + "\n\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(safe) == 0 {
		t.Fatal("token-free block should pass straight through")
	}
	if _, err := ra.c.Writer.Write(safe); err != nil {
		t.Fatal(err)
	}
	ra.wroteMeaningfulDownstream = true

	// 2) held event: the restorer buffers the whole channel back.
	content := heldContent(token)
	body, err := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"delta": map[string]any{"content": content}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	held, err := ra.restoreClientStreamSse([]byte("data: " + string(body) + "\n\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 0 {
		t.Fatalf("fixture invariant: the tail must be buffered, not emitted, got %q", held)
	}
	return ra, rec, token, session.Close
}

// S (safe tail): a committed failure must flush the pending restored tail BEFORE the
// in-band error frame — otherwise the real "hello " text the restorer was holding is
// silently lost. The tail here is a placeholder-SHAPED literal that matches no
// registered token, so the restorer can safely emit it verbatim. The already-emitted
// block and the terminal [DONE] must not be duplicated, and the terminal frame must
// be the ordinary upstream error (the restorer did not fail).
func TestCommittedStreamFailureFlushesSafeLiteralTailBeforeError(t *testing.T) {
	ra, rec, token, _ := buildCommittedRedactFailureAttempt(t, func(tok string) string {
		return "hello " + nonRegisteredPlaceholderLiteral(t, tok)
	})

	// Proof that the OLD (flush-less) failure path dropped the content: before the
	// terminal failure the held tail is not on the wire at all, so it can only appear
	// because the fix flushes it.
	if pre := rec.Body.String(); strings.Contains(pre, "hello ") {
		t.Fatalf("fixture invariant: buffered tail must not be on the wire before the flush: %q", pre)
	}

	writeCommittedStreamFailure(ra, errors.New("failed to transform stream event: upstream sent a bad frame"))

	body := rec.Body.String()
	safeIdx := strings.Index(body, "safe ")
	if safeIdx < 0 {
		t.Fatalf("already-committed block lost: %q", body)
	}
	if got := strings.Count(body, "safe "); got != 1 {
		t.Fatalf("committed block must not be duplicated, got %d: %q", got, body)
	}
	helloIdx := strings.Index(body, "hello ")
	if helloIdx < 0 {
		t.Fatalf("safe literal tail dropped on committed failure: %q", body)
	}
	errIdx := strings.Index(body, `"error"`)
	if errIdx < 0 {
		t.Fatalf("missing in-band error frame: %q", body)
	}
	if !(safeIdx < helloIdx && helloIdx < errIdx) {
		t.Fatalf("order must be committed block -> flushed tail -> error frame: %q", body)
	}
	if got := strings.Count(body, "data: [DONE]"); got != 1 {
		t.Fatalf("terminal [DONE] must appear exactly once, got %d: %q", got, body)
	}
	// Scoped leak check: no fragment of THIS session's registered token, while the
	// literal placeholder-shaped text is expected and allowed.
	if leaksRegisteredTokenFragment(body, token) {
		t.Fatalf("registered token fragment leaked to client: %q", body)
	}
	if strings.Contains(body, streamRestoreErrorCode) {
		t.Fatalf("a safe tail must not be reported as a restore failure: %q", body)
	}
}

// H (fail-closed): a HALF of THIS session's REGISTERED placeholder at finish cannot be
// split from the real "hello " prefix it was coalesced with, so NOTHING unrestored may
// be emitted. The pending fragment is dropped and the terminal frame reports the fixed,
// secret-free restore-error identifiers (never the raw upstream error), exactly once.
// "hello " is deliberately NOT asserted present: fail-closed may drop it, and the
// frame must not claim the content is complete.
func TestCommittedStreamFailureHalfRegisteredTokenFailsClosed(t *testing.T) {
	ra, rec, token, _ := buildCommittedRedactFailureAttempt(t, func(tok string) string {
		return "hello " + tok[:30]
	})

	writeCommittedStreamFailure(ra, errors.New("failed to transform stream event: boom"))

	body := rec.Body.String()
	if leaksRegisteredTokenFragment(body, token) {
		t.Fatalf("DEPENDENT on redact core: a half REGISTERED placeholder at Finish must NOT be emitted, but "+
			"the current core still coalesce-emits it; this stays red until B lands fail-closed Finish: %q", body)
	}
	if got := strings.Count(body, "data: [DONE]"); got != 1 {
		t.Fatalf("terminal [DONE] must appear exactly once, got %d: %q", got, body)
	}
	if !strings.Contains(body, streamRestoreErrorCode) || !strings.Contains(body, streamRestoreErrorMessage) {
		t.Fatalf("must fail explicitly with the fixed restore-error identifiers: %q", body)
	}
	if !strings.Contains(body, "safe ") {
		t.Fatalf("already-emitted block must remain on the wire: %q", body)
	}
	if strings.Contains(body, "failed to transform") || strings.Contains(body, "boom") {
		t.Fatalf("the raw upstream error must not be surfaced: %q", body)
	}
}

// A client abort means the client already hung up: the shared failure writer must add
// ZERO new bytes — no tail flush and no error frame.
func TestCommittedStreamFailureClientAbortAddsNoBytes(t *testing.T) {
	ra, rec, _, _ := buildCommittedRedactFailureAttempt(t, func(tok string) string {
		return "hello " + tok[:30]
	})
	before := rec.Body.String()

	writeCommittedStreamFailure(ra, context.Canceled)

	after := rec.Body.String()
	if after != before {
		t.Fatalf("client abort must add zero bytes; before=%q after=%q", before, after)
	}
	if strings.Contains(after, "hello ") || strings.Contains(after, `"error"`) {
		t.Fatalf("client abort must neither flush the tail nor write an error frame: %q", after)
	}
}

// A broken restorer (its buffer may hold unrestored placeholders / raw tokens) must
// NOT be flushed: skip the tail, but still fail explicitly in-protocol with the fixed,
// secret-free restore-error identifiers.
func TestCommittedStreamFailureBrokenRestorerLeaksNoToken(t *testing.T) {
	ra, rec, token, closeSession := buildCommittedRedactFailureAttempt(t, func(tok string) string {
		return "hello " + tok[:30]
	})
	closeSession() // breaks the restorer: Finish() now returns an error

	writeCommittedStreamFailure(ra, errors.New("failed to transform stream event: boom"))

	body := rec.Body.String()
	if leaksRegisteredTokenFragment(body, token) || strings.Contains(body, "hello ") {
		t.Fatalf("broken restorer must not emit buffered/raw tokens: %q", body)
	}
	if !strings.Contains(body, `"error"`) || !strings.Contains(body, streamRestoreErrorCode) {
		t.Fatalf("broken restorer must surface the fixed restore-error frame: %q", body)
	}
	if strings.Contains(body, "failed to transform") {
		t.Fatalf("the raw upstream error must not be surfaced: %q", body)
	}
	if !strings.Contains(body, "safe ") {
		t.Fatalf("already-emitted block must remain on the wire: %q", body)
	}
}
