package gemini

import (
	"encoding/json"
	"testing"

	"github.com/zhizhishu/octopus/internal/transformer/model"
)

func TestGeminiNativeTextSignatureSurvivesRequestRoundTrip(t *testing.T) {
	original := &model.GeminiGenerateContentRequest{
		Contents: []*model.GeminiContent{{
			Role:  "model",
			Parts: []*model.GeminiPart{{Text: "previous answer", ThoughtSignature: "gemini-origin-sig"}},
		}},
	}
	messages := convertGeminiMessages(original)
	if len(messages) != 1 {
		t.Fatalf("messages=%d", len(messages))
	}
	sig, ok := model.GeminiThoughtSignature(messages[0].ReasoningSignature)
	if !ok || sig != "gemini-origin-sig" {
		t.Fatalf("native signature lost: %q", sig)
	}
	reproduced := convertMessageToGeminiContent(&messages[0])
	if len(reproduced.Parts) != 1 || reproduced.Parts[0].ThoughtSignature != "gemini-origin-sig" {
		t.Fatalf("roundtrip signature changed: %#v", reproduced.Parts)
	}
}

func TestGeminiNativeParallelToolSignatureOnlyOnFirstCall(t *testing.T) {
	original := &model.GeminiGenerateContentRequest{
		Contents: []*model.GeminiContent{{
			Role: "model",
			Parts: []*model.GeminiPart{
				{FunctionCall: &model.GeminiFunctionCall{Name: "first", Args: map[string]interface{}{}}, ThoughtSignature: "first-call-signature"},
				{FunctionCall: &model.GeminiFunctionCall{Name: "second", Args: map[string]interface{}{}}},
			},
		}},
	}
	messages := convertGeminiMessages(original)
	if len(messages) != 1 || len(messages[0].ToolCalls) != 2 {
		t.Fatalf("lost calls: %#v", messages)
	}
	result := convertMessageToGeminiContent(&messages[0])
	if len(result.Parts) != 2 || result.Parts[0].ThoughtSignature != "first-call-signature" || result.Parts[1].ThoughtSignature != "" {
		t.Fatalf("parallel signature placement changed: %#v", result.Parts)
	}
}

func TestGeminiNativeStreamAggregationKeepsSignature(t *testing.T) {
	sig := model.TagGeminiThoughtSignature("stream-signature")
	word := "reply"
	inbound := &GenerateContentInbound{
		streamChunks: []*model.InternalLLMResponse{
			{Choices: []model.Choice{{Index: 0, Delta: &model.Message{Role: "assistant", ReasoningSignature: &sig}}}},
			{Choices: []model.Choice{{Index: 0, Delta: &model.Message{Content: model.MessageContent{Content: &word}}}}},
		},
	}
	result, err := inbound.GetInternalResponse(nil)
	if err != nil || result == nil || len(result.Choices) != 1 {
		t.Fatalf("bad aggregate %#v %v", result, err)
	}
	actual, ok := model.GeminiThoughtSignature(result.Choices[0].Message.ReasoningSignature)
	if !ok || actual != "stream-signature" {
		t.Fatalf("stream signature dropped: %q", actual)
	}
	encoded, err := json.Marshal(convertInternalResponseToGemini(result, false))
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) == 0 {
		t.Fatal("empty result")
	}
}
