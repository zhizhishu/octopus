package relay

// Lifecycle/redaction regression tests for the relay-side audit fixes:
//   #2  race must pin request-level protection on the PARENT before racers start,
//   #3a the three history bridges must mark historyBridged so the redaction fast
//       path cannot treat a clean current turn as a clean whole request,
//   #3b applyInboundRedaction must run AFTER every history merge,
//   #5  ResponsesInstructions must be captured/rolled back per attempt,
//   #6  collectResponse must restore the internal response text before it is
//       recorded as assistant history.
//
// Each case inverts one diagnostic sample from _artifacts/redact-audit-review_test.go
// (those asserted the BUGGY behavior; these assert the fixed behavior). Reuses the
// existing helpers from redact_test.go / codex_shape_test.go / protocol_conversion_test.go.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/relay/balancer"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	"github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/gin-gonic/gin"
)

// #2: an ALL-racers-failed race must leave the request-level protection pinned on the
// parent, so the next (opted-out) channel still redacts instead of forwarding cleartext.
// Inverts TestAuditReviewRaceLosesProtection.
func TestRelayRedactRacePreservesProtection(t *testing.T) {
	setupRedactDB(t)

	var mu sync.Mutex
	var raceBodies []string
	var fallbackBody string

	failUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		raceBodies = append(raceBodies, string(b))
		mu.Unlock()
		http.Error(w, `{"error":{"message":"boom"}}`, http.StatusInternalServerError)
	}))
	t.Cleanup(failUpstream.Close)

	fallbackUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		body := string(b)
		token := redactTokenRe.FindString(body)
		mu.Lock()
		fallbackBody = body
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"chatcmpl-race-fallback","object":"chat.completion","created":1,"model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"echo back ` + token + `"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(fallbackUpstream.Close)

	raceChannel := dbmodel.Channel{
		Name:               "redact-race-all-fail",
		Type:               outbound.OutboundTypeOpenAIChat,
		Enabled:            true,
		BaseUrls:           []dbmodel.BaseUrl{{URL: failUpstream.URL}},
		Keys:               []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "race-key-a"}, {Enabled: true, ChannelKey: "race-key-b"}},
		Model:              "upstream-model",
		ModelMapping:       map[string]string{"request-model": "upstream-model"},
		Priority:           1,
		RaceMode:           true,
		RaceKeyConcurrency: 2,
		RedactEnabled:      true,
		RedactFlags:        "E",
	}
	if err := op.ChannelCreate(&raceChannel, t.Context()); err != nil {
		t.Fatalf("create race channel: %v", err)
	}
	balancer.ResetChannel(raceChannel.ID)

	newRedactChannelFull(t, "redact-race-fallback", fallbackUpstream.URL, outbound.OutboundTypeOpenAIChat, false, "", 2)

	engine := newRedactGinEngine()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"request-model","messages":[{"role":"user","content":"my contact is a@example.com"}]}`))
	engine.ServeHTTP(rec, c.Request)

	mu.Lock()
	defer mu.Unlock()
	if len(raceBodies) == 0 {
		t.Fatalf("race was not exercised (no raced upstream body captured)")
	}
	if strings.Contains(raceBodies[0], "a@example.com") {
		t.Fatalf("race attempt on the opted-in channel must redact: %s", raceBodies[0])
	}
	if fallbackBody == "" {
		t.Fatalf("expected failover to the opted-out fallback channel")
	}
	if strings.Contains(fallbackBody, "a@example.com") {
		t.Fatalf("all-race-failed must keep the request protected: fallback got cleartext: %s", fallbackBody)
	}
	if !redactTokenRe.MatchString(fallbackBody) && !strings.Contains(fallbackBody, "Sensitive values are redacted") {
		t.Fatalf("fallback must carry a placeholder/notice (sticky protection): %s", fallbackBody)
	}
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "a@example.com") {
		t.Fatalf("client must get the restored secret after race+sticky failover: status=%d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "{{Redact:") {
		t.Fatalf("placeholder leaked to the client: %s", rec.Body.String())
	}
}

// #2: a racer must inherit the parent's request-level protection (the parent is pinned
// at the race call site; the racer's isolated copy carries the bool). Inverts
// TestAuditReviewRacerLocalProtection's "parent=false" observation by proving an
// OPT-OUT race channel still redacts once the parent is already sticky.
func TestRelayRedactRacerInheritsParentProtection(t *testing.T) {
	setupRedactDB(t)

	req := &relayRequest{
		inboundType: inbound.InboundTypeOpenAIChat,
		internalRequest: &model.InternalLLMRequest{
			Model:      "test-model",
			RawRequest: []byte(`{"messages":[{"role":"user","content":"a@example.com"}]}`),
			Messages:   []model.Message{{Role: "user", Content: model.MessageContent{Content: strPtr("a@example.com")}}},
		},
		// Simulates the parent already pinned by a prior attempt (or by the race call site).
		redactRequired: true,
	}
	rec := httptest.NewRecorder()
	req.c, _ = gin.CreateTestContext(rec)
	req.c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	// Opt-out channel: only the inherited sticky keeps a racer protected.
	ch := &dbmodel.Channel{
		Type:          outbound.OutboundTypeOpenAIChat,
		RedactEnabled: false,
		BaseUrls:      []dbmodel.BaseUrl{{URL: "https://upstream.example"}},
	}
	ra, _, _, err := prepareRacerAttempt(context.Background(), req, ch, dbmodel.ChannelKey{ChannelKey: "synthetic"}, outbound.Get(outbound.OutboundTypeOpenAIChat))
	if err != nil {
		t.Fatal(err)
	}
	defer ra.redactClose()

	if ra.relayRequest == req {
		t.Fatalf("racer must use an isolated request copy, not the parent")
	}
	if !ra.redactApplied {
		t.Fatalf("racer must inherit the parent's sticky protection and redact the opt-out wire")
	}
}

// #3a: with the notice disabled and a clean current turn, a Codex-shaped responses
// channel whose prior-turn history was bridged must STILL be scanned (historyBridged
// exempts the fast path). Inverts TestAuditReviewCodexHistoryNoticeOff.
func TestRelayRedactCodexHistoryRedactedWhenNoticeOff(t *testing.T) {
	setupRedactDB(t)
	clearResponsesSessionCacheForTest()
	if err := op.SettingSetString(dbmodel.SettingKeyRedactNoticeEnabled, "false"); err != nil {
		t.Fatalf("disable notice: %v", err)
	}

	previous := "resp_redact_history_parent"
	recordResponsesSessionTranscript(previous, []model.Message{
		{Role: "user", Content: model.MessageContent{Content: strPtr("old contact is old@example.com")}},
		{Role: "assistant", Content: model.MessageContent{Content: strPtr("remembered")}},
	})

	var mu sync.Mutex
	var sent string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		sent = string(b)
		mu.Unlock()
		writeCodexShapeResponsesSSEWithText(w, "resp_redact_history_next", "upstream-model", "ok")
	}))
	t.Cleanup(up.Close)

	newRedactChannelFull(t, "redact-history-codex", up.URL, outbound.OutboundTypeOpenAIResponse, true, "E", 1)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"request-model","previous_response_id":"`+previous+`","input":"continue"}`))
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	Handler(inbound.InboundTypeOpenAIResponse, c)

	mu.Lock()
	body := sent
	mu.Unlock()
	if body == "" {
		t.Fatalf("upstream was not reached")
	}
	if strings.Contains(body, "old@example.com") {
		t.Fatalf("bridged prior-turn history leaked to upstream (fast path not exempted): %s", body)
	}
	if !redactTokenRe.MatchString(body) {
		t.Fatalf("bridged prior-turn history must be redacted to a placeholder: %s", body)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
}

// #3b: a chat-sourced previous_response_id grafts the prior transcript in
// prepareResponsesSessionCursor, which runs BEFORE the (relocated) redaction pass, so
// the ready-to-send wire carries placeholders for BOTH the current and the historical
// email. Inverts TestAuditReviewChatCursorHistoryAfterRedaction.
func TestRelayRedactChatCursorHistoryBridgedRedacted(t *testing.T) {
	setupRedactDB(t)
	clearResponsesSessionCacheForTest()

	previous := "chatcmpl_redact_late_history"
	recordResponsesSessionOwned(context.Background(), previous, 101, 102, 0, 0, "audit-root", responseSessionSourceChat)
	recordResponsesSessionTranscript(previous, []model.Message{
		{Role: "user", Content: model.MessageContent{Content: strPtr("old@example.com")}},
		{Role: "assistant", Content: model.MessageContent{Content: strPtr("remembered")}},
	})

	body := []byte(`{"model":"test-model","previous_response_id":"` + previous + `","input":"new@example.com"}`)
	adapter := inbound.Get(inbound.InboundTypeOpenAIResponse)
	ir, err := adapter.TransformRequest(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	ir.RawRequest = body

	req := &relayRequest{inboundType: inbound.InboundTypeOpenAIResponse, internalRequest: ir}
	rec := httptest.NewRecorder()
	req.c, _ = gin.CreateTestContext(rec)
	req.c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	ch := &dbmodel.Channel{
		Type:          outbound.OutboundTypeOpenAIResponse,
		RedactEnabled: true,
		RedactFlags:   "E",
		Cloak:         dbmodel.ChannelCloak{Mode: "never"},
		BaseUrls:      []dbmodel.BaseUrl{{URL: "https://upstream.example"}},
	}
	ra, wire, _, err := prepareRacerAttempt(context.Background(), req, ch, dbmodel.ChannelKey{ChannelKey: "synthetic"}, outbound.Get(outbound.OutboundTypeOpenAIResponse))
	if err != nil {
		t.Fatal(err)
	}
	defer ra.redactClose()
	defer wire.Body.Close()

	sent, err := io.ReadAll(wire.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sent), "old@example.com") {
		t.Fatalf("cursor-bridged historical email leaked to the wire: %s", sent)
	}
	if strings.Contains(string(sent), "new@example.com") {
		t.Fatalf("current-turn email leaked to the wire: %s", sent)
	}
}

// #5: attempt A redacts the top-level instructions (flags E), fails, and attempt B
// (flags S, which does not cover an email) must send B's OWN re-derived instructions —
// never A's placeholder. Inverts TestAuditReviewInstructionsNotRolledBack.
func TestRelayRedactInstructionsRolledBackAcrossAttempts(t *testing.T) {
	setupRedactDB(t)

	var mu sync.Mutex
	var first, second string
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var v struct {
			Instructions string `json:"instructions"`
		}
		if err := json.Unmarshal(body, &v); err != nil {
			t.Error(err)
		}
		mu.Lock()
		first = v.Instructions
		mu.Unlock()
		http.Error(w, `{"error":{"message":"boom"}}`, http.StatusInternalServerError)
	}))
	t.Cleanup(a.Close)
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var v struct {
			Instructions string `json:"instructions"`
		}
		if err := json.Unmarshal(body, &v); err != nil {
			t.Error(err)
		}
		mu.Lock()
		second = v.Instructions
		mu.Unlock()
		writeCodexShapeResponsesSSEWithText(w, "resp_redact_instr_b", "upstream-model", v.Instructions)
	}))
	t.Cleanup(b.Close)

	newRedactChannelFull(t, "redact-instr-a", a.URL, outbound.OutboundTypeOpenAIResponse, true, "E", 1)
	newRedactChannelFull(t, "redact-instr-b", b.URL, outbound.OutboundTypeOpenAIResponse, true, "S", 2)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"request-model","instructions":"contact a@example.com","input":"hello"}`))
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	Handler(inbound.InboundTypeOpenAIResponse, c)

	mu.Lock()
	f, s := first, second
	mu.Unlock()
	if f == "" || s == "" {
		t.Fatalf("both attempts must reach upstream: first=%q second=%q", f, s)
	}
	if !redactTokenRe.MatchString(f) {
		t.Fatalf("attempt A (email flags) must have redacted the instructions: %q", f)
	}
	if redactTokenRe.MatchString(s) {
		t.Fatalf("attempt B must NOT carry attempt A's placeholder: %q", s)
	}
	if s != "contact a@example.com" {
		t.Fatalf("attempt B must send B's own re-derived instructions, got %q", s)
	}
	if strings.Contains(rec.Body.String(), "{{Redact:") {
		t.Fatalf("client must not see a placeholder: %s", rec.Body.String())
	}
}

// #6: the assistant response stored as conversation history must hold the RESTORED
// original text, matching what the client received. Inverts
// TestAuditReviewStoredAssistantNotRestored.
func TestRelayRedactStoredAssistantRestored(t *testing.T) {
	setupRedactDB(t)
	clearResponsesSessionCacheForTest()

	const responseID = "resp_redact_stored_reply"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		token := redactTokenRe.FindString(string(b))
		writeCodexShapeResponsesSSEWithText(w, responseID, "upstream-model", token)
	}))
	t.Cleanup(up.Close)

	newRedactChannelFull(t, "redact-stored", up.URL, outbound.OutboundTypeOpenAIResponse, true, "E", 1)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"request-model","input":"a@example.com"}`))
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	Handler(inbound.InboundTypeOpenAIResponse, c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "a@example.com") {
		t.Fatalf("client must receive the restored email: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "{{Redact:") {
		t.Fatalf("placeholder leaked to the client: %s", rec.Body.String())
	}

	history, ok := responsesSessionTranscript(responseID, 0, 0)
	if !ok {
		t.Fatalf("expected the response transcript to be stored")
	}
	var storedAssistant string
	for _, m := range history {
		if m.Role == "assistant" && m.Content.Content != nil {
			storedAssistant += *m.Content.Content
		}
	}
	if storedAssistant == "" {
		t.Fatalf("no assistant text stored in the transcript: %#v", history)
	}
	if redactTokenRe.MatchString(storedAssistant) {
		t.Fatalf("stored assistant history must hold the RESTORED original, got a placeholder: %q", storedAssistant)
	}
	if !strings.Contains(storedAssistant, "a@example.com") {
		t.Fatalf("stored assistant history must contain the original email, got %q", storedAssistant)
	}
}
