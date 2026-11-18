package gemini

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zhizhishu/octopus/internal/transformer/model"
)

func TestGeminiFourRequestFamilyControls(t *testing.T) {
	text := "ping"
	budgetDynamic := int64(-1)
	cases := []struct {
		name, modelName, effort, wantLevel string
		budget                             *int64
		wantThinking                       bool
	}{
		{"argon-default", "gemini-4-argon", "", "", nil, false},
		{"carbon-high", "gemini-4-carbon", "high", "HIGH", nil, true},
		{"hydrogen-xhigh", "gemini-4-hydrogen", "xhigh", "XHIGH", nil, true},
		{"argon-max-clamp", "gemini-4-argon", "max", "HIGH", nil, true},
		{"modern-dynamic-budget", "gemini-4-argon", "", "", &budgetDynamic, false},
		{"gemini-3-pro-medium", "gemini-3-pro-preview", "medium", "HIGH", nil, true},
		{"gemini-3-pro-image", "gemini-3-pro-image-preview", "high", "", nil, false},
		{"gemini-3.1-flash-lite-image", "gemini-3.1-flash-lite-image-preview", "low", "MINIMAL", nil, true},
		{"gemini-3.8-flash-minimal", "gemini-3.8-flash", "minimal", "LOW", nil, true},
		{"gemini-3.5-flash-minimal", "gemini-3.5-flash", "minimal", "MINIMAL", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := convertLLMToGeminiRequest(&model.InternalLLMRequest{
				Model: tc.modelName, ReasoningEffort: tc.effort, ReasoningBudget: tc.budget,
				Messages: []model.Message{{Role: "user", Content: model.MessageContent{Content: &text}}},
			})
			var level string
			if req.GenerationConfig != nil && req.GenerationConfig.ThinkingConfig != nil {
				level = req.GenerationConfig.ThinkingConfig.ThinkingLevel
				if req.GenerationConfig.ThinkingConfig.ThinkingBudget != nil {
					t.Fatal("modern Gemini emitted thinkingBudget")
				}
			}
			if level != tc.wantLevel || (level != "") != tc.wantThinking {
				t.Fatalf("thinking level %q, want %q", level, tc.wantLevel)
			}
		})
	}
}

func TestGeminiFourDropsLegacySamplingKnobs(t *testing.T) {
	text := "ping"
	temperature, topP, topK := 0.2, 0.8, "20"
	for _, name := range []string{"gemini-4-carbon", "gemini-2.5-pro"} {
		t.Run(name, func(t *testing.T) {
			req := convertLLMToGeminiRequest(&model.InternalLLMRequest{
				Model:       name,
				Messages:    []model.Message{{Role: "user", Content: model.MessageContent{Content: &text}}},
				Temperature: &temperature, TopP: &topP,
				TransformerMetadata: map[string]string{"gemini_top_k": topK},
			})
			body, err := json.Marshal(req)
			if err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"temperature", "topP", "topK"} {
				has := strings.Contains(string(body), `"`+key+`"`)
				if has != (name == "gemini-2.5-pro") {
					t.Fatalf("%s presence=%t for model=%s body=%s", key, has, name, body)
				}
			}
		})
	}
}

func TestGeminiFourUpstreamSignaturePersistsForTextAndParallelTools(t *testing.T) {
	for _, toolCalls := range []bool{false, true} {
		response := &model.GeminiGenerateContentResponse{
			Candidates: []*model.GeminiCandidate{
				{Content: &model.GeminiContent{Role: "model", Parts: []*model.GeminiPart{
					{Text: "result", ThoughtSignature: "real-gemini-signature"},
				}}},
			},
		}
		if toolCalls {
			response.Candidates[0].Content.Parts = []*model.GeminiPart{
				{FunctionCall: &model.GeminiFunctionCall{Name: "first", Args: map[string]interface{}{}}, ThoughtSignature: "real-gemini-signature"},
				{FunctionCall: &model.GeminiFunctionCall{Name: "second", Args: map[string]interface{}{}}},
			}
		}
		for _, streaming := range []bool{false, true} {
			converted := convertGeminiToLLMResponse(response, streaming)
			if len(converted.Choices) != 1 {
				t.Fatal("missing converted choice")
			}
			var message *model.Message
			if streaming {
				message = converted.Choices[0].Delta
			} else {
				message = converted.Choices[0].Message
			}
			if message == nil {
				t.Fatal("missing converted message")
			}
			got, ok := model.GeminiThoughtSignature(message.ReasoningSignature)
			if !ok || got != "real-gemini-signature" {
				t.Fatalf("signature lost streaming=%v toolCalls=%v got=%q", streaming, toolCalls, got)
			}
			message.Role = "assistant"
			req := convertLLMToGeminiRequest(&model.InternalLLMRequest{Model: "gemini-4-carbon", Messages: []model.Message{*message}})
			if len(req.Contents) != 1 || len(req.Contents[0].Parts) == 0 {
				t.Fatalf("replay empty: %#v", req)
			}
			if req.Contents[0].Parts[0].ThoughtSignature != "real-gemini-signature" {
				t.Fatalf("replay lost signature: %#v", req.Contents[0].Parts)
			}
			if toolCalls && len(req.Contents[0].Parts) > 1 && req.Contents[0].Parts[1].ThoughtSignature != "" {
				t.Fatalf("replay duplicated parallel signature: %#v", req.Contents[0].Parts)
			}
		}
	}
}
