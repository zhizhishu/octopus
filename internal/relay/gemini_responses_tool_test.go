package relay

import (
	"context"
	"testing"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	"github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/samber/lo"
)

func TestGeminiChannelBridgesResponsesHistory(t *testing.T) {
	ctx := context.Background()
	_ = ctx

	ra := &relayAttempt{
		relayRequest: &relayRequest{
			inboundType: inbound.InboundTypeOpenAIResponse,
			internalRequest: &model.InternalLLMRequest{
				Model: "gemini-2.5-flash",
			},
		},
		channel: &dbmodel.Channel{
			ID:   50,
			Type: outbound.OutboundTypeGemini,
		},
	}

	if !ra.shouldBridgeResponsesHistory() {
		t.Fatalf("expected shouldBridgeResponsesHistory to be true for Gemini channel")
	}

	if !openAIChatOutboundChannel(ra.channel.Type) {
		t.Fatalf("expected openAIChatOutboundChannel to return true for Gemini channel")
	}

	// Test transcript restoration with previous_response_id
	prevID := "resp_gemini_test_turn1"
	ra.apiKeyID = 1
	ra.userID = 1
	transcript := []model.Message{
		{
			Role:    "user",
			Content: model.MessageContent{Content: lo.ToPtr("call tool now")},
		},
		{
			Role: "assistant",
			ToolCalls: []model.ToolCall{
				{
					ID:   "call_search_1",
					Type: "function",
					Function: model.FunctionCall{
						Name:      "search",
						Arguments: `{"query":"golang"}`,
					},
				},
			},
		},
	}
	recordResponsesSessionTranscriptOwned(prevID, transcript, nil, ra.apiKeyID, ra.userID)

	// Turn 2 request coming from responses client with tool output only
	replyContent := `{"results":["found"]}`
	ra.internalRequest = &model.InternalLLMRequest{
		Model:              "gemini-2.5-flash",
		PreviousResponseID: &prevID,
		Messages: []model.Message{
			{
				Role:       "tool",
				ToolCallID: lo.ToPtr("call_search_1"),
				Content:    model.MessageContent{Content: &replyContent},
			},
		},
	}

	err := ra.bridgeResponsesHistoryForChat()
	if err != nil {
		t.Fatalf("unexpected error bridging history: %v", err)
	}

	if !ra.chatHistoryRebuilt {
		t.Fatalf("expected chatHistoryRebuilt to be true")
	}

	if len(ra.internalRequest.Messages) != 3 {
		t.Fatalf("expected 3 messages after rebuild, got %d: %+v", len(ra.internalRequest.Messages), ra.internalRequest.Messages)
	}

	if ra.internalRequest.Messages[0].Role != "user" {
		t.Fatalf("expected msg 0 user, got %s", ra.internalRequest.Messages[0].Role)
	}
	if ra.internalRequest.Messages[1].Role != "assistant" || len(ra.internalRequest.Messages[1].ToolCalls) != 1 {
		t.Fatalf("expected msg 1 assistant with tool_call, got %+v", ra.internalRequest.Messages[1])
	}
	if ra.internalRequest.Messages[2].Role != "tool" {
		t.Fatalf("expected msg 2 tool output, got %s", ra.internalRequest.Messages[2].Role)
	}
}
