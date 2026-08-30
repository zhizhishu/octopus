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

// TestDeepSeekResponseFormatMatrix locks the conservative response_format behavior:
// - All DeepSeek models downgrade json_schema until a specific family is verified live.
// - Non-DeepSeek models preserve json_schema without modification.
// - json_object response format is preserved across all models.
func TestDeepSeekResponseFormatMatrix(t *testing.T) {
	testCases := []struct {
		name           string
		model          string
		inputFormat    string
		wantType       string
		wantJSONSchema bool
	}{
		// 1. Reasoners / R-series: must downgrade json_schema to json_object
		{
			name:           "deepseek-reasoner downgrades json_schema",
			model:          "deepseek-reasoner",
			inputFormat:    "json_schema",
			wantType:       "json_object",
			wantJSONSchema: false,
		},
		{
			name:           "deepseek-r1 downgrades json_schema",
			model:          "deepseek-r1",
			inputFormat:    "json_schema",
			wantType:       "json_object",
			wantJSONSchema: false,
		},
		{
			name:           "deepseek-r2 downgrades json_schema",
			model:          "deepseek-r2",
			inputFormat:    "json_schema",
			wantType:       "json_object",
			wantJSONSchema: false,
		},
		{
			name:           "deepseek-v4-reasoner downgrades json_schema",
			model:          "deepseek-v4-reasoner",
			inputFormat:    "json_schema",
			wantType:       "json_object",
			wantJSONSchema: false,
		},
		{
			name:           "deepseek-v4-thinking downgrades json_schema",
			model:          "deepseek-v4-thinking",
			inputFormat:    "json_schema",
			wantType:       "json_object",
			wantJSONSchema: false,
		},
		// 2. Legacy / unknown DeepSeek models: conservative downgrade to json_object
		{
			name:           "legacy deepseek-chat downgrades json_schema",
			model:          "deepseek-chat",
			inputFormat:    "json_schema",
			wantType:       "json_object",
			wantJSONSchema: false,
		},
		{
			name:           "legacy deepseek-coder downgrades json_schema",
			model:          "deepseek-coder",
			inputFormat:    "json_schema",
			wantType:       "json_object",
			wantJSONSchema: false,
		},
		{
			name:           "unknown deepseek-custom downgrades json_schema",
			model:          "deepseek-custom",
			inputFormat:    "json_schema",
			wantType:       "json_object",
			wantJSONSchema: false,
		},
		// 3. New DeepSeek names remain conservative until upstream support is verified.
		{
			name:           "deepseek-v4 downgrades json_schema",
			model:          "deepseek-v4",
			inputFormat:    "json_schema",
			wantType:       "json_object",
			wantJSONSchema: false,
		},
		{
			name:           "deepseek-v4-pro downgrades json_schema",
			model:          "deepseek-v4-pro",
			inputFormat:    "json_schema",
			wantType:       "json_object",
			wantJSONSchema: false,
		},
		{
			name:           "deepseek-v4-flash downgrades json_schema",
			model:          "deepseek-v4-flash",
			inputFormat:    "json_schema",
			wantType:       "json_object",
			wantJSONSchema: false,
		},
		{
			name:           "deepseek-ai/deepseek-v4-pro provider prefix downgrades json_schema",
			model:          "deepseek-ai/deepseek-v4-pro",
			inputFormat:    "json_schema",
			wantType:       "json_object",
			wantJSONSchema: false,
		},
		// 4. Non-DeepSeek models: untouched json_schema
		{
			name:           "non-deepseek gpt-4o preserves json_schema",
			model:          "gpt-4o",
			inputFormat:    "json_schema",
			wantType:       "json_schema",
			wantJSONSchema: true,
		},
		{
			name:           "non-deepseek qwen-max preserves json_schema",
			model:          "qwen-max",
			inputFormat:    "json_schema",
			wantType:       "json_schema",
			wantJSONSchema: true,
		},
		// 5. json_object format is untouched across all models
		{
			name:           "deepseek-reasoner keeps json_object",
			model:          "deepseek-reasoner",
			inputFormat:    "json_object",
			wantType:       "json_object",
			wantJSONSchema: false,
		},
		{
			name:           "deepseek-chat keeps json_object",
			model:          "deepseek-chat",
			inputFormat:    "json_object",
			wantType:       "json_object",
			wantJSONSchema: false,
		},
		{
			name:           "deepseek-v4 keeps json_object",
			model:          "deepseek-v4",
			inputFormat:    "json_object",
			wantType:       "json_object",
			wantJSONSchema: false,
		},
		{
			name:           "gpt-4o keeps json_object",
			model:          "gpt-4o",
			inputFormat:    "json_object",
			wantType:       "json_object",
			wantJSONSchema: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			req := &model.InternalLLMRequest{
				Model: tc.model,
				ResponseFormat: &model.ResponseFormat{
					Type: tc.inputFormat,
				},
				Messages: userMessages(),
			}
			if tc.inputFormat == "json_schema" {
				req.ResponseFormat.JSONSchema = []byte(`{"name":"answer","schema":{"type":"object"}}`)
			}

			payload := chatRequestBody(t, req)
			typ, ok := responseFormatType(t, payload)
			if !ok {
				t.Fatalf("expected response_format in payload, got %#v", payload)
			}
			if typ != tc.wantType {
				t.Fatalf("expected response_format type %q, got %q", tc.wantType, typ)
			}

			rf, ok := payload["response_format"].(map[string]any)
			if !ok {
				t.Fatalf("response_format is not map: %#v", payload["response_format"])
			}
			_, hasSchema := rf["json_schema"]
			if hasSchema != tc.wantJSONSchema {
				t.Fatalf("expected json_schema present=%v, got present=%v (rf: %#v)", tc.wantJSONSchema, hasSchema, rf)
			}
		})
	}
}
