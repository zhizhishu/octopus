package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	model "github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

func newChatResponsesAttempt(req *model.InternalLLMRequest, channelType outbound.OutboundType) *relayAttempt {
	return &relayAttempt{
		relayRequest: &relayRequest{
			inboundType:     inbound.InboundTypeOpenAIResponse,
			internalRequest: req,
		},
		channel: &dbmodel.Channel{Type: channelType},
	}
}

// B: a chat channel rebuilds the prior turn's full history from the local
// transcript store, so an increment-only continuation keeps all context.
func TestBridgeResponsesHistoryForChatRebuildsFromTranscript(t *testing.T) {
	clearResponsesSessionCacheForTest()
	previous := "resp_chat_parent"
	recordResponsesSessionTranscript(previous, []model.Message{
		{Role: "user", Content: model.MessageContent{Content: strPtr("first question")}},
		{Role: "assistant", Content: model.MessageContent{Content: strPtr("first answer")}},
	})
	req := &model.InternalLLMRequest{
		Model:              "grok-4.5",
		RawAPIFormat:       model.APIFormatOpenAIResponse,
		PreviousResponseID: &previous,
		Messages: []model.Message{
			{Role: "user", Content: model.MessageContent{Content: strPtr("next question")}},
		},
	}
	ra := newChatResponsesAttempt(req, outbound.OutboundTypeOpenAIChat)

	if err := ra.bridgeResponsesHistoryForChat(); err != nil {
		t.Fatalf("rebuild should succeed, got error: %v", err)
	}
	if req.PreviousResponseID != nil {
		t.Fatalf("previous_response_id must be cleared after rebuild, got %#v", req.PreviousResponseID)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("expected rebuilt history(2) + increment(1) = 3 messages, got %d", len(req.Messages))
	}
	if txt := messageTextContent(req.Messages[0].Content); txt != "first question" {
		t.Fatalf("rebuilt history must lead, got first message %q", txt)
	}
	if txt := messageTextContent(req.Messages[len(req.Messages)-1].Content); txt != "next question" {
		t.Fatalf("incremental turn must trail, got last message %q", txt)
	}
}

// A: when no transcript is available on a chat channel, refuse loudly instead of
// forwarding a context-stripped turn — and classify the rejection so it never
// charges the channel breaker.
func TestBridgeResponsesHistoryForChatErrorsWhenTranscriptMissing(t *testing.T) {
	clearResponsesSessionCacheForTest()
	missing := "resp_never_stored"
	req := &model.InternalLLMRequest{
		Model:              "grok-4.5",
		RawAPIFormat:       model.APIFormatOpenAIResponse,
		PreviousResponseID: &missing,
		Messages: []model.Message{
			{Role: "user", Content: model.MessageContent{Content: strPtr("orphan turn")}},
		},
	}
	ra := newChatResponsesAttempt(req, outbound.OutboundTypeOpenAIChat)

	err := ra.bridgeResponsesHistoryForChat()
	if err == nil {
		t.Fatalf("missing transcript on a chat channel must not silently proceed; expected an error")
	}
	if !isRequestInvalidUpstreamError(err) {
		t.Fatalf("rejection must classify as request-invalid, got %v", err)
	}
	if shouldRecordBreakerFailure(http.StatusBadRequest, err) {
		t.Fatalf("a request-shape rejection must not charge the circuit breaker")
	}
	if req.PreviousResponseID == nil {
		t.Fatalf("a rejected request must be left untouched (previous_response_id preserved)")
	}
}

// tier1 full-replay clients send no previous_response_id: the bridge is a no-op.
func TestBridgeResponsesHistoryForChatSkipsWithoutPreviousResponseID(t *testing.T) {
	clearResponsesSessionCacheForTest()
	req := &model.InternalLLMRequest{
		Model:        "grok-4.5",
		RawAPIFormat: model.APIFormatOpenAIResponse,
		Messages: []model.Message{
			{Role: "user", Content: model.MessageContent{Content: strPtr("full replay turn")}},
		},
	}
	ra := newChatResponsesAttempt(req, outbound.OutboundTypeOpenAIChat)

	if err := ra.bridgeResponsesHistoryForChat(); err != nil {
		t.Fatalf("tier1 (no previous_response_id) must be untouched, got error: %v", err)
	}
	if len(req.Messages) != 1 {
		t.Fatalf("tier1 messages must be unchanged, got %d", len(req.Messages))
	}
}

// A client that already carries the assistant history is self-sufficient: no
// rebuild, and (crucially) no rejection.
func TestBridgeResponsesHistoryForChatSkipsWhenClientCarriesHistory(t *testing.T) {
	clearResponsesSessionCacheForTest()
	previous := "resp_client_carried"
	req := &model.InternalLLMRequest{
		Model:              "grok-4.5",
		RawAPIFormat:       model.APIFormatOpenAIResponse,
		PreviousResponseID: &previous,
		Messages: []model.Message{
			{Role: "user", Content: model.MessageContent{Content: strPtr("q1")}},
			{Role: "assistant", Content: model.MessageContent{Content: strPtr("a1")}},
			{Role: "user", Content: model.MessageContent{Content: strPtr("q2")}},
		},
	}
	ra := newChatResponsesAttempt(req, outbound.OutboundTypeOpenAIChat)

	if err := ra.bridgeResponsesHistoryForChat(); err != nil {
		t.Fatalf("a client that already carries history must not be rejected, got error: %v", err)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("client-carried messages must be unchanged, got %d", len(req.Messages))
	}
}

// The grok/codex agentic case: after its first tool call a codex-style client sends ONLY
// the function_call_output (role "tool") every turn and relies on previous_response_id for
// the rest. The bridge must rebuild the prior turn — including the assistant message that
// issued the matching tool_call — so the upstream sees a coherent
// [user, assistant(tool_call), tool(output)] sequence instead of an orphan result. This is
// the exact turn shape the old responsesMessagesContainToolOutput skip silently forwarded
// context-free (a 200 with total history loss).
func TestBridgeResponsesHistoryForChatRebuildsToolOutputContinuation(t *testing.T) {
	clearResponsesSessionCacheForTest()
	previous := "resp_toolcall_parent"
	recordResponsesSessionTranscript(previous, []model.Message{
		{Role: "user", Content: model.MessageContent{Content: strPtr("read alpha.py")}},
		{Role: "assistant", ToolCalls: []model.ToolCall{{
			ID:       "call_alpha",
			Type:     "function",
			Function: model.FunctionCall{Name: "read_file", Arguments: `{"path":"alpha.py"}`},
		}}},
	})
	req := &model.InternalLLMRequest{
		Model:              "grok-4.5",
		RawAPIFormat:       model.APIFormatOpenAIResponse,
		PreviousResponseID: &previous,
		Messages: []model.Message{
			{Role: "tool", ToolCallID: strPtr("call_alpha"), Content: model.MessageContent{Content: strPtr("print('alpha')")}},
		},
	}
	ra := newChatResponsesAttempt(req, outbound.OutboundTypeOpenAIChat)

	if err := ra.bridgeResponsesHistoryForChat(); err != nil {
		t.Fatalf("tool-output continuation must rebuild, got error: %v", err)
	}
	if req.PreviousResponseID != nil {
		t.Fatalf("previous_response_id must be cleared after rebuild, got %#v", req.PreviousResponseID)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("expected rebuilt [user, assistant(tool_call), tool(output)] = 3 messages, got %d", len(req.Messages))
	}
	assistant := req.Messages[1]
	if len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].ID != "call_alpha" {
		t.Fatalf("rebuilt history must retain the assistant tool_call the output answers, got %#v", assistant.ToolCalls)
	}
	last := req.Messages[2]
	if last.Role != "tool" || last.ToolCallID == nil || *last.ToolCallID != "call_alpha" {
		t.Fatalf("the tool output must trail the rebuilt history with its call_id intact, got role=%q id=%v", last.Role, last.ToolCallID)
	}
}

// Safety property: a tool-output continuation whose transcript is gone must fail LOUD
// (deterministic invalid_request_error, not charged to the breaker) rather than silently
// forwarding an orphan tool result — the silent-200 context loss this bridge exists to stop.
func TestBridgeResponsesHistoryForChatToolOutputMissingTranscriptRejects(t *testing.T) {
	clearResponsesSessionCacheForTest()
	missing := "resp_toolcall_gone"
	req := &model.InternalLLMRequest{
		Model:              "grok-4.5",
		RawAPIFormat:       model.APIFormatOpenAIResponse,
		PreviousResponseID: &missing,
		Messages: []model.Message{
			{Role: "tool", ToolCallID: strPtr("call_x"), Content: model.MessageContent{Content: strPtr("orphan result")}},
		},
	}
	ra := newChatResponsesAttempt(req, outbound.OutboundTypeOpenAIChat)

	err := ra.bridgeResponsesHistoryForChat()
	if err == nil {
		t.Fatalf("a tool-output continuation with no transcript must not silently forward; expected an error")
	}
	if !isRequestInvalidUpstreamError(err) {
		t.Fatalf("rejection must classify as request-invalid, got %v", err)
	}
	if shouldRecordBreakerFailure(http.StatusBadRequest, err) {
		t.Fatalf("a request-shape rejection must not charge the circuit breaker")
	}
	if req.PreviousResponseID == nil {
		t.Fatalf("a rejected request must be left untouched (previous_response_id preserved)")
	}
}

// The parallel-tool-call edge the reference projects (sub2api normalizeChatMessages /
// CLIProxyAPI dedupe) guard against: the prior turn issued TWO parallel tool_calls but the
// client answers only one this turn (holding the sibling for a later turn). Without the
// pairing guardrail the rebuilt history would hand the upstream an assistant with a dangling
// tool_call, which strict chat upstreams (DeepSeek/Grok) reject. The unanswered call must be
// dropped so every assistant tool_call is immediately followed by its matching reply.
func TestBridgeResponsesHistoryForChatDropsUnansweredParallelToolCall(t *testing.T) {
	clearResponsesSessionCacheForTest()
	previous := "resp_parallel_parent"
	recordResponsesSessionTranscript(previous, []model.Message{
		{Role: "user", Content: model.MessageContent{Content: strPtr("read alpha and beta")}},
		{Role: "assistant", ToolCalls: []model.ToolCall{
			{ID: "call_a", Type: "function", Function: model.FunctionCall{Name: "read_file", Arguments: `{"path":"alpha.py"}`}},
			{ID: "call_b", Type: "function", Function: model.FunctionCall{Name: "read_file", Arguments: `{"path":"beta.py"}`}},
		}},
	})
	req := &model.InternalLLMRequest{
		Model:              "grok-4.5",
		RawAPIFormat:       model.APIFormatOpenAIResponse,
		PreviousResponseID: &previous,
		Messages: []model.Message{
			{Role: "tool", ToolCallID: strPtr("call_a"), Content: model.MessageContent{Content: strPtr("print('alpha')")}},
		},
	}
	ra := newChatResponsesAttempt(req, outbound.OutboundTypeOpenAIChat)

	if err := ra.bridgeResponsesHistoryForChat(); err != nil {
		t.Fatalf("rebuild must succeed, got error: %v", err)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("expected [user, assistant(call_a), tool(call_a)] = 3 messages, got %d: %#v", len(req.Messages), req.Messages)
	}
	asst := req.Messages[1]
	if asst.Role != "assistant" || len(asst.ToolCalls) != 1 || asst.ToolCalls[0].ID != "call_a" {
		t.Fatalf("assistant must retain only the answered call_a (unanswered call_b dropped), got %#v", asst.ToolCalls)
	}
	// Invariant: every surviving assistant tool_call is immediately followed by its reply.
	for i, m := range req.Messages {
		if m.Role != "assistant" {
			continue
		}
		for j, tc := range m.ToolCalls {
			k := i + 1 + j
			if k >= len(req.Messages) || req.Messages[k].Role != "tool" || req.Messages[k].ToolCallID == nil || *req.Messages[k].ToolCallID != tc.ID {
				t.Fatalf("tool_call %q is not immediately followed by its matching reply", tc.ID)
			}
		}
	}
}

// Unit cover for the ported guardrail: unanswered tool_calls dropped, orphan replies
// dropped, matched replies pulled up to sit right after their assistant, plain messages
// (system/user) untouched.
func TestNormalizeChatToolCallPairing(t *testing.T) {
	msgs := []model.Message{
		{Role: "system", Content: model.MessageContent{Content: strPtr("sys")}},
		{Role: "user", Content: model.MessageContent{Content: strPtr("q")}},
		{Role: "assistant", ToolCalls: []model.ToolCall{
			{ID: "c1", Function: model.FunctionCall{Name: "f"}},
			{ID: "c2", Function: model.FunctionCall{Name: "g"}}, // unanswered -> dropped
		}},
		{Role: "tool", ToolCallID: strPtr("c1"), Content: model.MessageContent{Content: strPtr("r1")}},
		{Role: "tool", ToolCallID: strPtr("orphan"), Content: model.MessageContent{Content: strPtr("no owner")}}, // orphan -> dropped
	}
	out := normalizeChatToolCallPairing(msgs)
	if len(out) != 4 {
		t.Fatalf("expected [system, user, assistant(c1), tool(c1)] = 4, got %d: %#v", len(out), out)
	}
	if out[0].Role != "system" || out[1].Role != "user" {
		t.Fatalf("plain messages must be preserved in order, got %q,%q", out[0].Role, out[1].Role)
	}
	if out[2].Role != "assistant" || len(out[2].ToolCalls) != 1 || out[2].ToolCalls[0].ID != "c1" {
		t.Fatalf("unanswered c2 must be dropped, got %#v", out[2].ToolCalls)
	}
	if out[3].Role != "tool" || out[3].ToolCallID == nil || *out[3].ToolCallID != "c1" {
		t.Fatalf("c1 reply must immediately follow its assistant, got %#v", out[3])
	}
}

// Store side: chat channels must record responses transcripts so a later
// previous_response_id turn can be rebuilt.
func TestShouldBridgeResponsesHistoryEnabledForChatChannel(t *testing.T) {
	for _, ct := range []outbound.OutboundType{outbound.OutboundTypeOpenAIChat, outbound.OutboundTypeCustomOpenAIChat} {
		req := &model.InternalLLMRequest{RawAPIFormat: model.APIFormatOpenAIResponse}
		ra := newChatResponsesAttempt(req, ct)
		if !ra.shouldBridgeResponsesHistory() {
			t.Fatalf("chat channel type %v must record responses transcripts for later rebuild", ct)
		}
	}
}

// Regression guard for the shared-request PreviousResponseID leak: the first chat
// attempt inlines the prior turn's history and clears previous_response_id on the
// SHARED internalRequest; a failover attempt must rebuild the full history again,
// not silently forward only the incremental turn. Before the per-attempt reset in
// forward()'s outer loop, the second attempt saw a nil previous_response_id, skipped
// the bridge, and sent 1 message instead of the full history.
func TestOpenAIResponsesChatHistoryRebuildSurvivesFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	clearResponsesSessionCacheForTest()

	previous := "resp_failover_parent"
	recordResponsesSessionTranscript(previous, []model.Message{
		{Role: "user", Content: model.MessageContent{Content: strPtr("first question")}},
		{Role: "assistant", Content: model.MessageContent{Content: strPtr("first answer")}},
	})

	var (
		mu     sync.Mutex
		calls  int
		okMsgs int
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			// First attempt fails so the balancer fails over to the second channel.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"upstream temporarily unavailable"}}`))
			return
		}
		var body model.InternalLLMRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		okMsgs = len(body.Messages)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-failover-ok","object":"chat.completion","created":1,"model":"gpt-5.5","choices":[{"index":0,"message":{"role":"assistant","content":"ok after failover"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`))
	}))
	t.Cleanup(upstream.Close)

	mkChannel := func(name string) dbmodel.Channel {
		return dbmodel.Channel{
			Name:     name,
			Type:     outbound.OutboundTypeOpenAIChat,
			Enabled:  true,
			Cloak:    dbmodel.ChannelCloak{Mode: "never"},
			BaseUrls: []dbmodel.BaseUrl{{URL: upstream.URL}},
			Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "k"}},
		}
	}
	ch1 := mkChannel("chat-failover-first")
	ch2 := mkChannel("chat-failover-second")
	if err := op.ChannelCreate(&ch1, ctx); err != nil {
		t.Fatalf("create ch1: %v", err)
	}
	if err := op.ChannelCreate(&ch2, ctx); err != nil {
		t.Fatalf("create ch2: %v", err)
	}
	group := dbmodel.Group{Name: "failover-chat", Mode: dbmodel.GroupModeFailover}
	if err := op.GroupCreate(&group, ctx); err != nil {
		t.Fatalf("create group: %v", err)
	}
	for i, ch := range []dbmodel.Channel{ch1, ch2} {
		if err := op.GroupItemAdd(&dbmodel.GroupItem{GroupID: group.ID, ChannelID: ch.ID, ModelName: "gpt-5.5", Priority: i + 1, Weight: 1}, ctx); err != nil {
			t.Fatalf("group item: %v", err)
		}
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"failover-chat",
		"input":"next question",
		"previous_response_id":"resp_failover_parent"
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	Handler(inbound.InboundTypeOpenAIResponse, c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected success after failover, got %d body %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	got, total := okMsgs, calls
	mu.Unlock()
	if total < 2 {
		t.Fatalf("expected a failover (>=2 upstream calls), got %d", total)
	}
	// rebuilt history (user + assistant) + increment (user) = 3 messages. Before the fix
	// the failover attempt saw a nil previous_response_id and forwarded only the 1 increment.
	if got < 3 {
		t.Fatalf("failover attempt must carry the rebuilt history, got %d messages (want >=3)", got)
	}
}
