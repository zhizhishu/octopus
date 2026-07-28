package relay

import (
	"net/http"
	"testing"

	dbmodel "github.com/bestruirui/octopus/internal/model"
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
