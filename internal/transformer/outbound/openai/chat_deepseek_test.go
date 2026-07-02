package openai

import (
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

// responseFormatType extracts the "type" string from the "response_format"
// field of a marshalled chat request body. Returns ("", false) when the field
// is absent.
func responseFormatType(t *testing.T, payload map[string]any) (string, bool) {
	t.Helper()
	raw, ok := payload["response_format"]
	if !ok {
		return "", false
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("response_format is not an object: %#v", raw)
	}
	typ, _ := obj["type"].(string)
	return typ, true
}

// TestDeepSeekJSONSchemaDowngradedToJSONObject verifies that a json_schema
// response_format is replaced with json_object for DeepSeek models.
func TestDeepSeekJSONSchemaDowngradedToJSONObject(t *testing.T) {
	payload := chatRequestBody(t, &model.InternalLLMRequest{
		Model: "deepseek-chat",
		ResponseFormat: &model.ResponseFormat{
			Type:       "json_schema",
			JSONSchema: []byte(`{"name":"answer","schema":{"type":"object"}}`),
		},
		Messages: userMessages(),
	})

	typ, ok := responseFormatType(t, payload)
	if !ok {
		t.Fatalf("expected response_format in payload, got %#v", payload)
	}
	if typ != "json_object" {
		t.Fatalf("expected type json_object after downgrade, got %q", typ)
	}
	// The json_schema detail must be dropped — DeepSeek would reject it.
	if rf, ok := payload["response_format"].(map[string]any); ok {
		if _, hasSchema := rf["json_schema"]; hasSchema {
			t.Fatalf("json_schema key must be absent after downgrade, got %#v", rf)
		}
	}
}

// TestDeepSeekJSONObjectUnchanged verifies that a json_object response_format
// is passed through unmodified for DeepSeek models.
func TestDeepSeekJSONObjectUnchanged(t *testing.T) {
	payload := chatRequestBody(t, &model.InternalLLMRequest{
		Model: "deepseek-chat",
		ResponseFormat: &model.ResponseFormat{
			Type: "json_object",
		},
		Messages: userMessages(),
	})

	typ, ok := responseFormatType(t, payload)
	if !ok {
		t.Fatalf("expected response_format in payload, got %#v", payload)
	}
	if typ != "json_object" {
		t.Fatalf("expected json_object unchanged, got %q", typ)
	}
}

// TestNonDeepSeekJSONSchemaUnchanged verifies that a json_schema
// response_format is transparently forwarded for non-DeepSeek models.
func TestNonDeepSeekJSONSchemaUnchanged(t *testing.T) {
	payload := chatRequestBody(t, &model.InternalLLMRequest{
		Model: "gpt-4o",
		ResponseFormat: &model.ResponseFormat{
			Type:       "json_schema",
			JSONSchema: []byte(`{"name":"answer","schema":{"type":"object"}}`),
		},
		Messages: userMessages(),
	})

	typ, ok := responseFormatType(t, payload)
	if !ok {
		t.Fatalf("expected response_format in payload, got %#v", payload)
	}
	if typ != "json_schema" {
		t.Fatalf("expected json_schema preserved for non-deepseek model, got %q", typ)
	}
	rf := payload["response_format"].(map[string]any)
	if _, hasSchema := rf["json_schema"]; !hasSchema {
		t.Fatalf("json_schema key must be preserved for non-deepseek model, got %#v", rf)
	}
}
