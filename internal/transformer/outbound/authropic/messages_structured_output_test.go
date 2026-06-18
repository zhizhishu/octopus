package authropic

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
)

func decodeAnthropicBody(t *testing.T, req *transformerModel.InternalLLMRequest) map[string]json.RawMessage {
	t.Helper()
	httpReq, err := (&MessageOutbound{}).TransformRequest(context.Background(), req, "https://api.anthropic.com", "test-key")
	if err != nil {
		t.Fatalf("TransformRequest returned error: %v", err)
	}
	body, err := io.ReadAll(httpReq.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode anthropic body: %v\n%s", err, string(body))
	}
	return decoded
}

func structuredOutputUserMessage() []transformerModel.Message {
	content := "return structured data"
	return []transformerModel.Message{{
		Role:    "user",
		Content: transformerModel.MessageContent{Content: &content},
	}}
}

// Chat-style json_schema response_format ({"name","schema","strict"} wrapper)
// crossing into an Anthropic channel must surface as output_config.format with
// the schema (and name) preserved.
func TestOpenAIChatJSONSchemaMapsToAnthropicOutputConfig(t *testing.T) {
	maxTokens := int64(64)
	req := &transformerModel.InternalLLMRequest{
		Model:     "claude-opus-4-8",
		MaxTokens: &maxTokens,
		Messages:  structuredOutputUserMessage(),
		ResponseFormat: &transformerModel.ResponseFormat{
			Type:       "json_schema",
			JSONSchema: json.RawMessage(`{"name":"result","strict":true,"schema":{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}}`),
		},
	}

	body := decodeAnthropicBody(t, req)
	rawOC, ok := body["output_config"]
	if !ok {
		t.Fatalf("expected output_config in body: %v", body)
	}

	var oc struct {
		Format json.RawMessage `json:"format"`
	}
	if err := json.Unmarshal(rawOC, &oc); err != nil {
		t.Fatalf("decode output_config: %v", err)
	}
	formatText := string(oc.Format)
	for _, want := range []string{`"type":"json_schema"`, `"name":"result"`, `"schema"`, `"ok"`} {
		if !strings.Contains(formatText, want) {
			t.Fatalf("expected format to contain %s, got %s", want, formatText)
		}
	}
	// The conservative mapping must not leak the OpenAI-only "strict" flag into
	// Anthropic's format.
	if strings.Contains(formatText, "strict") {
		t.Fatalf("did not expect strict flag in anthropic format, got %s", formatText)
	}
}

// Responses-style json_schema carries a bare schema object (name/schema are
// peers in the Responses payload, so only the schema reaches the internal
// model). It must still map to output_config.format with the schema embedded.
func TestOpenAIResponsesBareSchemaMapsToAnthropicOutputConfig(t *testing.T) {
	maxTokens := int64(64)
	req := &transformerModel.InternalLLMRequest{
		Model:     "claude-opus-4-8",
		MaxTokens: &maxTokens,
		Messages:  structuredOutputUserMessage(),
		ResponseFormat: &transformerModel.ResponseFormat{
			Type:       "json_schema",
			JSONSchema: json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}`),
		},
	}

	body := decodeAnthropicBody(t, req)
	rawOC, ok := body["output_config"]
	if !ok {
		t.Fatalf("expected output_config in body: %v", body)
	}

	var oc struct {
		Format json.RawMessage `json:"format"`
	}
	if err := json.Unmarshal(rawOC, &oc); err != nil {
		t.Fatalf("decode output_config: %v", err)
	}
	formatText := string(oc.Format)
	for _, want := range []string{`"type":"json_schema"`, `"schema"`, `"answer"`} {
		if !strings.Contains(formatText, want) {
			t.Fatalf("expected format to contain %s, got %s", want, formatText)
		}
	}
}

// A native Anthropic output_config (Claude Code's own) must always win and must
// never be overwritten by a cross-protocol json_schema mapping.
func TestNativeAnthropicOutputConfigNotOverwrittenByResponseFormat(t *testing.T) {
	maxTokens := int64(64)
	req := &transformerModel.InternalLLMRequest{
		Model:                 "claude-opus-4-8",
		MaxTokens:             &maxTokens,
		Messages:              structuredOutputUserMessage(),
		AnthropicOutputConfig: json.RawMessage(`{"effort":"high","format":{"type":"json_schema","schema":{"type":"object","properties":{"title":{"type":"string"}},"required":["title"]}}}`),
		// A competing OpenAI-style schema that must be ignored.
		ResponseFormat: &transformerModel.ResponseFormat{
			Type:       "json_schema",
			JSONSchema: json.RawMessage(`{"name":"intruder","schema":{"type":"object","properties":{"evil":{"type":"boolean"}}}}`),
		},
	}

	body := decodeAnthropicBody(t, req)
	rawOC, ok := body["output_config"]
	if !ok {
		t.Fatalf("expected output_config in body: %v", body)
	}
	ocText := string(rawOC)
	if !strings.Contains(ocText, `"effort":"high"`) || !strings.Contains(ocText, `"title"`) {
		t.Fatalf("expected native output_config preserved, got %s", ocText)
	}
	if strings.Contains(ocText, "intruder") || strings.Contains(ocText, "evil") {
		t.Fatalf("native output_config must not be overwritten by response_format, got %s", ocText)
	}
}

// Non-json_schema response formats (json_object/text) must not synthesize an
// Anthropic output_config.
func TestNonSchemaResponseFormatDoesNotSynthesizeOutputConfig(t *testing.T) {
	maxTokens := int64(64)
	req := &transformerModel.InternalLLMRequest{
		Model:     "claude-opus-4-8",
		MaxTokens: &maxTokens,
		Messages:  structuredOutputUserMessage(),
		ResponseFormat: &transformerModel.ResponseFormat{
			Type: "json_object",
		},
	}

	body := decodeAnthropicBody(t, req)
	if _, ok := body["output_config"]; ok {
		t.Fatalf("did not expect output_config for json_object response format: %v", body)
	}
}
