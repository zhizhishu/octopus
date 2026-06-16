package gemini

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

func TestMessagesOutboundTransformStreamSkipsEmptyEventData(t *testing.T) {
	outbound := &MessagesOutbound{}

	resp, err := outbound.TransformStream(context.Background(), []byte(" \n\t "))
	if err != nil {
		t.Fatalf("expected empty stream data to be skipped, got error: %v", err)
	}
	if resp != nil {
		t.Fatalf("expected empty stream data to return nil response, got %#v", resp)
	}
}

func TestConvertLLMToGeminiRequestMapsToolResultIDToFunctionName(t *testing.T) {
	content := "use lookup"
	toolResult := `{"ok":true}`
	req := &model.InternalLLMRequest{
		Model: "gemini-test",
		Messages: []model.Message{
			{
				Role:    "user",
				Content: model.MessageContent{Content: &content},
			},
			{
				Role: "assistant",
				ToolCalls: []model.ToolCall{{
					ID:    "call_lookup_1",
					Type:  "function",
					Index: 0,
					Function: model.FunctionCall{
						Name:      "lookup",
						Arguments: `{"q":"octopus"}`,
					},
				}},
			},
			{
				Role:       "tool",
				ToolCallID: stringPtr("call_lookup_1"),
				Content:    model.MessageContent{Content: &toolResult},
			},
		},
	}

	converted := convertLLMToGeminiRequest(req)
	if len(converted.Contents) != 3 {
		t.Fatalf("expected 3 contents, got %#v", converted.Contents)
	}
	parts := converted.Contents[2].Parts
	if len(parts) != 1 || parts[0].FunctionResponse == nil {
		t.Fatalf("expected one function response part, got %#v", parts)
	}
	if parts[0].FunctionResponse.Name != "lookup" {
		t.Fatalf("expected Gemini functionResponse name lookup, got %q", parts[0].FunctionResponse.Name)
	}
}

func TestConvertLLMToGeminiRequestPreservesJSONSchemaResponseFormat(t *testing.T) {
	content := "return structured data"
	req := &model.InternalLLMRequest{
		Model: "gemini-test",
		Messages: []model.Message{{
			Role:    "user",
			Content: model.MessageContent{Content: &content},
		}},
		ResponseFormat: &model.ResponseFormat{
			Type:       "json_schema",
			JSONSchema: json.RawMessage(`{"name":"result","schema":{"type":"object","properties":{"ok":{"type":"boolean"},"tags":{"type":"array","items":{"type":"string"}}},"required":["ok"]}}`),
		},
	}

	converted := convertLLMToGeminiRequest(req)
	if converted.GenerationConfig == nil {
		t.Fatalf("expected generation config")
	}
	if converted.GenerationConfig.ResponseMimeType != "application/json" {
		t.Fatalf("expected application/json response MIME type, got %q", converted.GenerationConfig.ResponseMimeType)
	}
	schema := converted.GenerationConfig.ResponseSchema
	if schema == nil {
		t.Fatalf("expected Gemini response schema")
	}
	if schema.Type != "OBJECT" {
		t.Fatalf("expected OBJECT schema type, got %#v", schema)
	}
	if schema.Properties["ok"] == nil || schema.Properties["ok"].Type != "BOOLEAN" {
		t.Fatalf("expected ok BOOLEAN property, got %#v", schema.Properties)
	}
	if schema.Properties["tags"] == nil || schema.Properties["tags"].Type != "ARRAY" || schema.Properties["tags"].Items == nil || schema.Properties["tags"].Items.Type != "STRING" {
		t.Fatalf("expected tags ARRAY of STRING property, got %#v", schema.Properties)
	}
	if len(schema.Required) != 1 || schema.Required[0] != "ok" {
		t.Fatalf("expected required ok, got %#v", schema.Required)
	}
}

func TestMessagesOutboundUsesCamelSystemInstructionAndMergesQuery(t *testing.T) {
	system := "be concise"
	user := "hello"
	stream := true
	req := &model.InternalLLMRequest{
		Model:  "gemini-test",
		Stream: &stream,
		Messages: []model.Message{
			{Role: "system", Content: model.MessageContent{Content: &system}},
			{Role: "user", Content: model.MessageContent{Content: &user}},
		},
		Query: url.Values{
			"foo": []string{"bar"},
			"key": []string{"client-key"},
			"alt": []string{"json"},
		},
	}

	httpReq, err := (&MessagesOutbound{}).TransformRequest(context.Background(), req, "https://example.test/api/provider/gemini?base=1", "server-key")
	if err != nil {
		t.Fatalf("transform request: %v", err)
	}
	if got := httpReq.Header.Get("Accept"); got != "text/event-stream" {
		t.Fatalf("expected SSE accept header, got %q", got)
	}
	q := httpReq.URL.Query()
	if q.Get("base") != "1" || q.Get("foo") != "bar" || q.Get("key") != "server-key" || q.Get("alt") != "sse" {
		t.Fatalf("unexpected query: %s", httpReq.URL.RawQuery)
	}
	var body map[string]any
	if err := json.NewDecoder(httpReq.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if _, ok := body["systemInstruction"]; !ok {
		t.Fatalf("expected camelCase systemInstruction in body: %#v", body)
	}
	if _, ok := body["system_instruction"]; ok {
		t.Fatalf("did not expect snake_case system_instruction in body: %#v", body)
	}
	if !strings.Contains(httpReq.URL.Path, "/api/provider/gemini/v1beta/models/gemini-test:streamGenerateContent") {
		t.Fatalf("unexpected upstream path: %q", httpReq.URL.Path)
	}
}

func stringPtr(value string) *string {
	return &value
}
