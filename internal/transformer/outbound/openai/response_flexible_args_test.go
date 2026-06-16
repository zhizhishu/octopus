package openai

import (
	"context"
	"testing"
)

// Some second-hand relay gateways send function-call `arguments` as a raw JSON
// object instead of the OpenAI-spec stringified JSON. The stream transform must
// tolerate that and re-serialize it to a string rather than failing the whole
// stream event with "cannot unmarshal object into ... arguments of type string".
func TestResponseOutboundTransformStreamToleratesObjectArguments(t *testing.T) {
	outbound := &ResponseOutbound{}

	if _, err := outbound.TransformStream(context.Background(), []byte(`{
		"type":"response.output_item.added",
		"output_index":0,
		"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"lookup"}
	}`)); err != nil {
		t.Fatalf("output_item.added returned error: %v", err)
	}

	done, err := outbound.TransformStream(context.Background(), []byte(`{
		"type":"response.output_item.done",
		"output_index":0,
		"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"lookup","arguments":{"q":"hello","n":2}}
	}`))
	if err != nil {
		t.Fatalf("object-shaped item.arguments must not fail the stream event, got error: %v", err)
	}
	if done == nil || len(done.Choices) != 1 || done.Choices[0].Delta == nil || len(done.Choices[0].Delta.ToolCalls) != 1 {
		t.Fatalf("expected one tool-call chunk, got %#v", done)
	}
	got := done.Choices[0].Delta.ToolCalls[0].Function.Arguments
	if got != `{"q":"hello","n":2}` {
		t.Fatalf("expected object arguments re-serialized to compact JSON string, got %q", got)
	}
}

// Compliant string arguments must keep working unchanged.
func TestResponseOutboundTransformStreamKeepsStringArguments(t *testing.T) {
	outbound := &ResponseOutbound{}

	if _, err := outbound.TransformStream(context.Background(), []byte(`{
		"type":"response.output_item.added",
		"output_index":0,
		"item":{"id":"fc_2","type":"function_call","call_id":"call_2","name":"lookup"}
	}`)); err != nil {
		t.Fatalf("output_item.added returned error: %v", err)
	}

	done, err := outbound.TransformStream(context.Background(), []byte(`{
		"type":"response.output_item.done",
		"output_index":0,
		"item":{"id":"fc_2","type":"function_call","call_id":"call_2","name":"lookup","arguments":"{\"q\":\"hi\"}"}
	}`))
	if err != nil {
		t.Fatalf("string item.arguments returned error: %v", err)
	}
	got := done.Choices[0].Delta.ToolCalls[0].Function.Arguments
	if got != `{"q":"hi"}` {
		t.Fatalf("expected verbatim string arguments, got %q", got)
	}
}

// A non-stream Responses body with object-shaped output[].arguments must also
// parse instead of failing the whole response unmarshal.
func TestResponsesItemUnmarshalToleratesObjectArguments(t *testing.T) {
	var item ResponsesItem
	if err := item.UnmarshalJSON([]byte(`{"type":"function_call","call_id":"c1","name":"f","arguments": { "a" : 1 }}`)); err != nil {
		t.Fatalf("object arguments must not fail ResponsesItem unmarshal, got: %v", err)
	}
	if item.Arguments != `{"a":1}` {
		t.Fatalf("expected re-serialized arguments, got %q", item.Arguments)
	}
	if item.CallID != "c1" || item.Name != "f" || item.Type != "function_call" {
		t.Fatalf("other fields must be preserved, got %#v", item)
	}
}
