package gemini

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

func TestGenerateContentInboundTransformRequest(t *testing.T) {
	ctx := WithRequestOptions(context.Background(), "gemini-request", false)
	inbound := &GenerateContentInbound{}

	req, err := inbound.TransformRequest(ctx, []byte(`{
		"system_instruction":{"parts":[{"text":"be concise"}]},
		"contents":[
			{"role":"user","parts":[{"text":"hello"}]},
			{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{"q":"octopus"}}}]},
			{"role":"user","parts":[{"functionResponse":{"name":"lookup","response":{"ok":true}}}]}
		],
		"generationConfig":{"temperature":0.2,"maxOutputTokens":32,"stopSequences":["END"],"responseMimeType":"application/json"},
		"tools":[{"functionDeclarations":[{"name":"lookup","description":"search","parameters":{"type":"object"}}]}],
		"toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["lookup"]}}
	}`))
	if err != nil {
		t.Fatalf("transform request: %v", err)
	}
	if req.Model != "gemini-request" {
		t.Fatalf("unexpected model: %q", req.Model)
	}
	if req.RawAPIFormat != model.APIFormatGeminiContents {
		t.Fatalf("unexpected raw format: %s", req.RawAPIFormat)
	}
	if len(req.Messages) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(req.Messages))
	}
	if req.Messages[0].Role != "system" || req.Messages[0].Content.Content == nil || *req.Messages[0].Content.Content != "be concise" {
		t.Fatalf("unexpected system message: %+v", req.Messages[0])
	}
	if req.Messages[1].Role != "user" || req.Messages[1].Content.Content == nil || *req.Messages[1].Content.Content != "hello" {
		t.Fatalf("unexpected user message: %+v", req.Messages[1])
	}
	if len(req.Messages[2].ToolCalls) != 1 || req.Messages[2].ToolCalls[0].Function.Name != "lookup" {
		t.Fatalf("expected assistant tool call, got %+v", req.Messages[2].ToolCalls)
	}
	if req.Messages[3].Role != "tool" || req.Messages[3].ToolCallID == nil || *req.Messages[3].ToolCallID != req.Messages[2].ToolCalls[0].ID {
		t.Fatalf("expected tool response message, got %+v", req.Messages[3])
	}
	if req.Messages[3].ToolCallName == nil || *req.Messages[3].ToolCallName != "lookup" {
		t.Fatalf("expected tool response to keep Gemini function name, got %+v", req.Messages[3].ToolCallName)
	}
	if req.MaxTokens == nil || *req.MaxTokens != 32 {
		t.Fatalf("expected max tokens 32, got %+v", req.MaxTokens)
	}
	if req.ResponseFormat == nil || req.ResponseFormat.Type != "json_object" {
		t.Fatalf("expected json response format, got %+v", req.ResponseFormat)
	}
	if len(req.Tools) != 1 || req.Tools[0].Function.Name != "lookup" {
		t.Fatalf("expected one function tool, got %+v", req.Tools)
	}
	if req.ToolChoice == nil || req.ToolChoice.NamedToolChoice == nil || req.ToolChoice.NamedToolChoice.Function.Name != "lookup" {
		t.Fatalf("expected named tool choice, got %+v", req.ToolChoice)
	}
}

func TestGenerateContentInboundAcceptsCamelSystemInstruction(t *testing.T) {
	ctx := WithRequestOptions(context.Background(), "gemini-request", false)
	inbound := &GenerateContentInbound{}

	req, err := inbound.TransformRequest(ctx, []byte(`{
		"systemInstruction":{"parts":[{"text":"be concise"}]},
		"contents":[{"role":"user","parts":[{"text":"hello"}]}]
	}`))
	if err != nil {
		t.Fatalf("transform request: %v", err)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("expected system + user messages, got %d", len(req.Messages))
	}
	if req.Messages[0].Role != "system" || req.Messages[0].Content.Content == nil || *req.Messages[0].Content.Content != "be concise" {
		t.Fatalf("unexpected system instruction: %#v", req.Messages[0])
	}
}

func TestGenerateContentInboundTransformResponse(t *testing.T) {
	inbound := &GenerateContentInbound{}
	text := "hello back"
	body, err := inbound.TransformResponse(context.Background(), &model.InternalLLMResponse{
		Model:  "upstream-model",
		Object: "chat.completion",
		Choices: []model.Choice{{
			Index: 0,
			Message: &model.Message{
				Role: "assistant",
				Content: model.MessageContent{
					Content: &text,
				},
			},
		}},
		Usage: &model.Usage{
			PromptTokens:     3,
			CompletionTokens: 2,
			TotalTokens:      5,
		},
	})
	if err != nil {
		t.Fatalf("transform response: %v", err)
	}

	var resp model.GeminiGenerateContentResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal gemini response: %v", err)
	}
	if resp.ModelVersion != "upstream-model" {
		t.Fatalf("unexpected model version: %q", resp.ModelVersion)
	}
	if len(resp.Candidates) != 1 || resp.Candidates[0].Content == nil || len(resp.Candidates[0].Content.Parts) != 1 {
		t.Fatalf("unexpected candidates: %+v", resp.Candidates)
	}
	if resp.Candidates[0].Content.Parts[0].Text != "hello back" {
		t.Fatalf("unexpected text: %q", resp.Candidates[0].Content.Parts[0].Text)
	}
	if resp.UsageMetadata == nil || resp.UsageMetadata.TotalTokenCount != 5 {
		t.Fatalf("unexpected usage: %+v", resp.UsageMetadata)
	}
}
