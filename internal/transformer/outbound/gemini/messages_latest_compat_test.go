package gemini

import (
	"github.com/zhizhishu/octopus/internal/transformer/model"
	"testing"
)

func TestModernGeminiStripsSamplingAndPrefersCompletionTokens(t *testing.T) {
	user := "hi"
	temp, p := 0.7, 0.9
	max := int64(2048)
	maxComp := int64(4096)
	for _, tc := range []struct {
		model  string
		modern bool
	}{
		{"gemini-3.8-flash", false}, {"gemini-3.7-flash", false}, {"gemini-4-carbon", true}, {"gemini-2.5-pro", false},
	} {
		t.Run(tc.model, func(t *testing.T) {
			req := convertLLMToGeminiRequest(&model.InternalLLMRequest{
				Model: tc.model, Messages: []model.Message{{Role: "user", Content: model.MessageContent{Content: &user}}},
				Temperature: &temp, TopP: &p, MaxTokens: &max, MaxCompletionTokens: &maxComp,
				ReasoningEffort: "medium", TransformerMetadata: map[string]string{"gemini_top_k": "17"},
			})
			cfg := req.GenerationConfig
			if cfg == nil || cfg.MaxOutputTokens != int(maxComp) {
				t.Fatalf("completion cap dropped %#v", cfg)
			}
			hasSampling := cfg.Temperature != nil || cfg.TopP != nil || cfg.TopK != nil
			if hasSampling == tc.modern {
				t.Fatalf("model %s bad sampling %#v", tc.model, cfg)
			}
			if geminiThreeOrFour(tc.model) {
				if cfg.ThinkingConfig == nil || cfg.ThinkingConfig.ThinkingLevel != "MEDIUM" || cfg.ThinkingConfig.ThinkingBudget != nil {
					t.Fatalf("Gemini modern thinking invalid %#v", cfg.ThinkingConfig)
				}
			}
		})
	}
}

func TestGemini3FunctionCallIDsPreservedThroughChatBridge(t *testing.T) {
	user, args, toolReply := "find", "{}", "result"
	callID := "backend-call-42"
	request := convertLLMToGeminiRequest(&model.InternalLLMRequest{Model: "gemini-3.8-flash", Messages: []model.Message{
		{Role: "user", Content: model.MessageContent{Content: &user}},
		{Role: "assistant", ToolCalls: []model.ToolCall{{Type: "function", ID: callID, Function: model.FunctionCall{Name: "lookup", Arguments: args}}}},
		{Role: "tool", ToolCallID: &callID, Content: model.MessageContent{Content: &toolReply}},
	}})
	if len(request.Contents) != 3 {
		t.Fatalf("missing turns: %#v", request.Contents)
	}
	fc := request.Contents[1].Parts[0].FunctionCall
	fr := request.Contents[2].Parts[0].FunctionResponse
	if fc == nil || fr == nil || fc.ID != callID || fr.ID != callID || fc.Name != fr.Name {
		t.Fatalf("functionCall/functionResponse id/name pair broken: %#v %#v", fc, fr)
	}
	response := convertGeminiToLLMResponse(&model.GeminiGenerateContentResponse{
		Candidates: []*model.GeminiCandidate{{Content: &model.GeminiContent{Role: "model", Parts: []*model.GeminiPart{
			{FunctionCall: &model.GeminiFunctionCall{ID: callID, Name: "lookup", Args: map[string]interface{}{}}},
		}}}},
	}, false)
	if len(response.Choices) == 0 || len(response.Choices[0].Message.ToolCalls) != 1 || response.Choices[0].Message.ToolCalls[0].ID != callID {
		t.Fatalf("upstream function call id lost in conversion: %#v", response)
	}
}
