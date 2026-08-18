package relay

import (
	"strings"
	"testing"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	openaiOutbound "github.com/bestruirui/octopus/internal/transformer/outbound/openai"
)

// Cursor may call /v1/chat then continue on /v1/responses with the chat completion
// id as previous_response_id. That id is not a real Responses store id, so octopus
// must rebuild history from the local transcript and drop the cursor before the
// stateful upstream would 400 on "previous_response_not_found".
func TestChatToResponsesCrossEndpointRebuildsFromChatSourcedCursor(t *testing.T) {
	clearResponsesSessionCacheForTest()

	chatID := "chatcmpl-cross-endpoint-1"
	chatReq := &transformerModel.InternalLLMRequest{
		Model: "grok-4.5",
		Messages: []transformerModel.Message{
			{Role: "user", Content: transformerModel.MessageContent{Content: strPtr("Secret is CHAT77. Reply OKD only.")}},
		},
	}
	chatRA := &relayAttempt{
		relayRequest: &relayRequest{
			inboundType:     inbound.InboundTypeOpenAIChat,
			internalRequest: chatReq,
			apiKeyID:        42,
			userID:          7,
		},
		channel: &dbmodel.Channel{ID: 11, Type: outbound.OutboundTypeOpenAIChat},
		usedKey: dbmodel.ChannelKey{ID: 12},
	}
	chatResp := &transformerModel.InternalLLMResponse{
		ID: chatID,
		Choices: []transformerModel.Choice{{
			Message: &transformerModel.Message{
				Role:    "assistant",
				Content: transformerModel.MessageContent{Content: strPtr("OKD")},
			},
		}},
	}
	chatRA.recordChatSessionFromInbound(chatResp)

	owner, ok := responsesSessionOwner(chatID)
	if !ok {
		t.Fatalf("chat completion id must be recorded as a session cursor")
	}
	if owner.source != responseSessionSourceChat {
		t.Fatalf("chat-minted session source = %q, want %q", owner.source, responseSessionSourceChat)
	}
	history, hasHistory := responsesSessionTranscript(chatID, 42, 7)
	if !hasHistory || len(history) < 2 {
		t.Fatalf("chat session must store request+assistant transcript, got %#v", history)
	}

	respReq := &transformerModel.InternalLLMRequest{
		Model:              "grok-4.5",
		PreviousResponseID: &chatID,
		Messages: []transformerModel.Message{
			{Role: "user", Content: transformerModel.MessageContent{Content: strPtr("What is the secret? Reply code only.")}},
		},
	}
	respRA := &relayAttempt{
		relayRequest: &relayRequest{
			inboundType:     inbound.InboundTypeOpenAIResponse,
			internalRequest: respReq,
			apiKeyID:        42,
			userID:          7,
		},
		channel: &dbmodel.Channel{ID: 11, Type: outbound.OutboundTypeOpenAIResponse},
		usedKey: dbmodel.ChannelKey{ID: 12},
	}
	respRA.prepareResponsesSessionCursor(&openaiOutbound.ResponseOutbound{})

	if respReq.PreviousResponseID != nil {
		t.Fatalf("chat-sourced previous_response_id must be dropped before upstream, got %#v", *respReq.PreviousResponseID)
	}
	if !respRA.chatHistoryRebuilt {
		t.Fatalf("chat-sourced cursor must rebuild history from transcript")
	}
	if len(respReq.Messages) < 3 {
		t.Fatalf("rebuilt messages too short: %#v", respReq.Messages)
	}
	joined := ""
	for _, msg := range respReq.Messages {
		if msg.Content.Content != nil {
			joined += *msg.Content.Content + " | "
		}
	}
	for _, needle := range []string{"CHAT77", "OKD", "What is the secret"} {
		if !strings.Contains(joined, needle) {
			t.Fatalf("rebuilt history missing %q, got %q", needle, joined)
		}
	}
}

// Pre-f902fed DB rows keep source="" after AutoMigrate. A chatcmpl_* id with a
// local transcript must still rebuild history instead of being forwarded upstream.
func TestEmptySourceChatCompletionIDStillRebuilds(t *testing.T) {
	clearResponsesSessionCacheForTest()

	chatID := "chatcmpl-legacy-empty-source"
	chatReq := &transformerModel.InternalLLMRequest{
		Model: "glm-5.2",
		Messages: []transformerModel.Message{
			{Role: "user", Content: transformerModel.MessageContent{Content: strPtr("Remember LEGACY42.")}},
		},
	}
	chatRA := &relayAttempt{
		relayRequest: &relayRequest{
			inboundType:     inbound.InboundTypeOpenAIChat,
			internalRequest: chatReq,
			apiKeyID:        9,
			userID:          3,
		},
		channel: &dbmodel.Channel{ID: 5, Type: outbound.OutboundTypeOpenAIChat},
		usedKey: dbmodel.ChannelKey{ID: 6},
	}
	chatRA.recordChatSessionFromInbound(&transformerModel.InternalLLMResponse{
		ID: chatID,
		Choices: []transformerModel.Choice{{
			Message: &transformerModel.Message{
				Role:    "assistant",
				Content: transformerModel.MessageContent{Content: strPtr("noted")},
			},
		}},
	})

	// Simulate a pre-migration row: source column empty, transcript still present.
	// recordResponsesSessionOwned("") defaults empty source to "responses", so poke
	// the in-memory store directly (matches AutoMigrate backfill of old rows).
	responsesSessionStore.Lock()
	entry, ok := responsesSessionStore.items[chatID]
	if !ok {
		responsesSessionStore.Unlock()
		t.Fatalf("expected session owner in memory")
	}
	entry.source = ""
	responsesSessionStore.items[chatID] = entry
	responsesSessionStore.Unlock()
	if hist, has := responsesSessionTranscript(chatID, 9, 3); !has || len(hist) < 2 {
		t.Fatalf("transcript missing after source wipe: %#v", hist)
	}

	respReq := &transformerModel.InternalLLMRequest{
		Model:              "glm-5.2",
		PreviousResponseID: &chatID,
		Messages: []transformerModel.Message{
			{Role: "user", Content: transformerModel.MessageContent{Content: strPtr("What did I say?")}},
		},
	}
	respRA := &relayAttempt{
		relayRequest: &relayRequest{
			inboundType:     inbound.InboundTypeOpenAIResponse,
			internalRequest: respReq,
			apiKeyID:        9,
			userID:          3,
		},
		channel: &dbmodel.Channel{ID: 5, Type: outbound.OutboundTypeOpenAIChat},
		usedKey: dbmodel.ChannelKey{ID: 6},
	}
	respRA.prepareResponsesSessionCursor(&openaiOutbound.ResponseOutbound{})
	if respReq.PreviousResponseID != nil {
		t.Fatalf("empty-source chatcmpl id must be dropped, got %#v", *respReq.PreviousResponseID)
	}
	if !respRA.chatHistoryRebuilt {
		t.Fatalf("empty-source chatcmpl id must rebuild history")
	}
	joined := ""
	for _, msg := range respReq.Messages {
		if msg.Content.Content != nil {
			joined += *msg.Content.Content + " "
		}
	}
	if !strings.Contains(joined, "LEGACY42") {
		t.Fatalf("rebuilt history missing LEGACY42, got %q", joined)
	}
}
