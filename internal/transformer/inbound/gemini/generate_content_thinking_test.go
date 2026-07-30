package gemini

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

// Fix 1: a Gemini part with thought==true must land on the message's
// ReasoningContent, while a sibling non-thought text part stays normal content.
func TestGeminiInboundThoughtPartMapsToReasoning(t *testing.T) {
	ctx := WithRequestOptions(context.Background(), "gemini-request", false)
	inbound := &GenerateContentInbound{}

	req, err := inbound.TransformRequest(ctx, []byte(`{
		"contents":[
			{"role":"model","parts":[
				{"text":"let me think about it","thought":true},
				{"text":"the visible answer"}
			]}
		]
	}`))
	if err != nil {
		t.Fatalf("transform request: %v", err)
	}
	if len(req.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d: %#v", len(req.Messages), req.Messages)
	}
	msg := req.Messages[0]
	if msg.Role != "assistant" {
		t.Fatalf("expected assistant role, got %q", msg.Role)
	}
	if msg.GetReasoningContent() != "let me think about it" {
		t.Fatalf("expected thought part on ReasoningContent, got %q", msg.GetReasoningContent())
	}
	if msg.Content.Content == nil || *msg.Content.Content != "the visible answer" {
		t.Fatalf("expected non-thought text to stay as content, got %#v", msg.Content)
	}
	// The thought text must NOT leak into plain content.
	if msg.Content.Content != nil && *msg.Content.Content == "let me think about it" {
		t.Fatalf("thought text leaked into content: %#v", msg.Content)
	}
}

// Fix 2: an assistant functionCall carrying a thoughtSignature must preserve it
// on Message.ReasoningSignature so the outbound path can replay it.
func TestGeminiInboundFunctionCallThoughtSignaturePreserved(t *testing.T) {
	ctx := WithRequestOptions(context.Background(), "gemini-request", false)
	inbound := &GenerateContentInbound{}

	req, err := inbound.TransformRequest(ctx, []byte(`{
		"contents":[
			{"role":"model","parts":[
				{"functionCall":{"name":"lookup","args":{"q":"octopus"}},"thoughtSignature":"sig-abc-123"}
			]}
		]
	}`))
	if err != nil {
		t.Fatalf("transform request: %v", err)
	}
	if len(req.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d: %#v", len(req.Messages), req.Messages)
	}
	msg := req.Messages[0]
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Function.Name != "lookup" {
		t.Fatalf("expected one lookup tool call, got %#v", msg.ToolCalls)
	}
	// The thoughtSignature is preserved Gemini-tagged so a cross-protocol replay
	// cannot emit it as another provider's signature; it decodes back to the raw.
	if raw, ok := model.GeminiThoughtSignature(msg.ReasoningSignature); !ok || raw != "sig-abc-123" {
		t.Fatalf("expected Gemini-tagged thoughtSignature decoding to raw, got %#v", msg.ReasoningSignature)
	}
}

// Fix 3: generationConfig.thinkingConfig.thinkingLevel -> ReasoningEffort (lowercased),
// thinkingConfig.includeThoughts -> TransformerMetadata["gemini_include_thoughts"] ("true"/"false"),
// and generationConfig.responseSchema -> ResponseFormat{Type:"json_schema", JSONSchema:<schema>}
// rather than a bare json_object.
func TestGeminiInboundThinkingConfigMapsEffortIncludeAndResponseSchema(t *testing.T) {
	t.Run("high_include_true_with_schema", func(t *testing.T) {
		ctx := WithRequestOptions(context.Background(), "gemini-request", false)
		inbound := &GenerateContentInbound{}

		req, err := inbound.TransformRequest(ctx, []byte(`{
			"contents":[{"role":"user","parts":[{"text":"hi"}]}],
			"generationConfig":{
				"thinkingConfig":{"thinkingLevel":"HIGH","includeThoughts":true},
				"responseSchema":{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}
			}
		}`))
		if err != nil {
			t.Fatalf("transform request: %v", err)
		}
		if req.ReasoningEffort != "high" {
			t.Fatalf("expected lowercased reasoning effort 'high', got %q", req.ReasoningEffort)
		}
		if got := req.TransformerMetadata["gemini_include_thoughts"]; got != "true" {
			t.Fatalf("expected gemini_include_thoughts=true, got %q", got)
		}
		if req.ResponseFormat == nil || req.ResponseFormat.Type != "json_schema" {
			t.Fatalf("expected json_schema response format, got %#v", req.ResponseFormat)
		}
		if len(req.ResponseFormat.JSONSchema) == 0 {
			t.Fatalf("expected non-empty marshaled JSONSchema")
		}
		var schema model.GeminiSchema
		if err := json.Unmarshal(req.ResponseFormat.JSONSchema, &schema); err != nil {
			t.Fatalf("unmarshal JSONSchema: %v", err)
		}
		if schema.Type != "object" {
			t.Fatalf("expected schema type 'object', got %q", schema.Type)
		}
		if schema.Properties["answer"] == nil || schema.Properties["answer"].Type != "string" {
			t.Fatalf("expected answer string property in schema, got %#v", schema.Properties)
		}
	})

	t.Run("low_include_false_no_schema", func(t *testing.T) {
		ctx := WithRequestOptions(context.Background(), "gemini-request", false)
		inbound := &GenerateContentInbound{}

		req, err := inbound.TransformRequest(ctx, []byte(`{
			"contents":[{"role":"user","parts":[{"text":"hi"}]}],
			"generationConfig":{
				"thinkingConfig":{"thinkingLevel":"Low","includeThoughts":false}
			}
		}`))
		if err != nil {
			t.Fatalf("transform request: %v", err)
		}
		if req.ReasoningEffort != "low" {
			t.Fatalf("expected lowercased reasoning effort 'low', got %q", req.ReasoningEffort)
		}
		if got := req.TransformerMetadata["gemini_include_thoughts"]; got != "false" {
			t.Fatalf("expected gemini_include_thoughts=false, got %q", got)
		}
		// No responseSchema and no application/json mime -> no response format.
		if req.ResponseFormat != nil {
			t.Fatalf("expected no response format without schema/mime, got %#v", req.ResponseFormat)
		}
	})
}
