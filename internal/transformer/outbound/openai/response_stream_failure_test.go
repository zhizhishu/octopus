package openai

import (
	"context"
	"strings"
	"testing"
)

// The Responses failure flow used to be reported as a clean stream: the
// response.failed / error terminal events returned only an error-finish chunk, so
// modeltest judged partial text ahead of them a success and an otherwise-empty
// stream a bogus "completed but empty", while relay accounting recorded
// success=true. These regression tests pin the minimal fix: a terminal failure
// must terminate TransformStream with an error that carries the preserved,
// redacted upstream reason, so the existing failure paths record a failure.

// TestResponsesStreamFailureReasonResponseFailedError pins reason extraction for
// the canonical response.failed event carrying response.error code/message.
func TestResponseStreamFailureEnvelopeVariants(t *testing.T) {
	cases := []string{
		`{"type":"error","code":429,"message":"generation cut off"}`,
		`{"type":"error","error":{"code":429,"message":"generation cut off"}}`,
		`{"type":"response.completed","response":{"id":"resp_1","object":"response","output":[],"error":{"code":"upstream_error","message":"generation cut off"}}}`,
		`{"type":"response.completed","response":{"status":"error","error":{"message":"generation cut off"}}}`,
	}
	for _, payload := range cases {
		adapter := &ResponseOutbound{}
		if _, err := adapter.TransformStream(context.Background(), []byte(payload)); err == nil || !strings.Contains(err.Error(), "generation cut off") {
			t.Fatalf("failure envelope %s returned %v", payload, err)
		}
	}
}

func TestResponseStreamFailureReasonResponseFailedError(t *testing.T) {
	reason := responsesStreamFailureReason([]byte(`{
		"type":"response.failed",
		"response":{
			"id":"resp_1","object":"response","model":"gpt-5.5","status":"failed",
			"output":[],
			"error":{"code":"upstream_error","message":"the model hit a capacity wall"}
		}
	}`))
	if !strings.Contains(reason, "the model hit a capacity wall") ||
		!strings.Contains(reason, "upstream_error") {
		t.Fatalf("expected response.error code/message preserved, got %q", reason)
	}
}

// TestResponsesStreamFailureReasonTopLevelError pins the bare "error" event shape
// with a nested `error` object.
func TestResponseStreamFailureReasonTopLevelError(t *testing.T) {
	reason := responsesStreamFailureReason([]byte(`{
		"type":"error",
		"error":{"type":"invalid_request_error","code":"bad_input","message":"debounced too fast"}
	}`))
	if !strings.Contains(reason, "debounced too fast") ||
		!strings.Contains(reason, "bad_input") {
		t.Fatalf("expected nested error message/code preserved, got %q", reason)
	}
}

// TestResponsesStreamFailureReasonFlattenedCodeMessage pins the flattened
// top-level code/message fallback, including a numeric status code.
func TestResponseStreamFailureReasonFlattenedCodeMessage(t *testing.T) {
	reason := responsesStreamFailureReason([]byte(`{
		"type":"error",
		"code":429,
		"message":"too many concurrent turns"
	}`))
	if !strings.Contains(reason, "too many concurrent turns") ||
		!strings.Contains(reason, "429") {
		t.Fatalf("expected flattened code/message preserved (numeric code kept), got %q", reason)
	}
}

// TestResponsesStreamFailureReasonGenericNoDetail ensures a failure without any
// error detail still reports a failure (never a clean stop / empty completion).
func TestResponseStreamFailureReasonGenericNoDetail(t *testing.T) {
	reason := responsesStreamFailureReason([]byte(`{"type":"response.failed"}`))
	if reason != "upstream stream failed without error details" {
		t.Fatalf("expected generic fallback, got %q", reason)
	}
}

// TestResponseStreamFailureReasonRedactsSecret ensures xredact.Secrets strips any
// credential a provider error body might echo before it reaches error text.
func TestResponseStreamFailureReasonRedactsSecret(t *testing.T) {
	reason := responsesStreamFailureReason([]byte(`{
		"type":"error",
		"error":{"code":"auth","message":"invalid key Bearer sk-abcdef0123456789"}
	}`))
	if strings.Contains(reason, "sk-abcdef0123456789") ||
		strings.Contains(reason, "Bearer sk-abcdef0123456789") {
		t.Fatalf("provider secret must be redacted from the failure reason, got %q", reason)
	}
	if !strings.Contains(reason, "[redacted]") {
		t.Fatalf("expected the redaction marker to remain, got %q", reason)
	}
}

// TestResponseStreamFailureTerminatesWithError turns the whole TransformStream
// path: a terminal response.failed (even after partial content has been streamed)
// must stop the stream with an error carrying the preserved reason, so partial
// text can never pass as success that way.
func TestResponseStreamFailureTerminatesWithError(t *testing.T) {
	outbound := &ResponseOutbound{}

	// Partial text first: a success in the old flow would have been possible.
	chunk, err := outbound.TransformStream(context.Background(), []byte(`{
		"type":"response.output_text.delta","delta":"partial answer"
	}`))
	if err != nil {
		t.Fatalf("partial delta should not error: %v", err)
	}
	if chunk == nil || len(chunk.Choices) != 1 || chunk.Choices[0].Delta == nil ||
		chunk.Choices[0].Delta.Content.Content == nil {
		t.Fatalf("expected partial delta chunk, got %#v", chunk)
	}

	// The terminal failure event must abort the stream as an error.
	_, err = outbound.TransformStream(context.Background(), []byte(`{
		"type":"response.failed",
		"response":{
			"id":"resp_1","object":"response","model":"gpt-5.5","status":"failed",
			"output":[],
			"error":{"code":"upstream_error","message":"generation cut off"}
		}
	}`))
	if err == nil {
		t.Fatal("response.failed after partial text must terminate the stream as an error")
	}
	if !strings.Contains(err.Error(), "generation cut off") {
		t.Fatalf("expected the redacted reason preserved in the error, got: %v", err)
	}
}

// TestResponseCompletedFailedStatusTerminatesWithError pins the
// response.completed-with-status=failed case: it must also error (not emit a clean
// error-finish chunk that lets accounting record success).
func TestResponseCompletedFailedStatusTerminatesWithError(t *testing.T) {
	outbound := &ResponseOutbound{}
	_, err := outbound.TransformStream(context.Background(), []byte(`{
		"type":"response.completed",
		"response":{
			"id":"resp_1","object":"response","model":"gpt-5.5","status":"failed",
			"output":[],
			"error":{"code":"upstream_error","message":"completed then rejected"}
		}
	}`))
	if err == nil {
		t.Fatal("response.completed with status failed must terminate as an error")
	}
	if !strings.Contains(err.Error(), "completed then rejected") {
		t.Fatalf("expected the redacted reason preserved, got: %v", err)
	}
}

// TestResponseCompletedNormalRecoveryStillWorks guards that the normal
// response.completed path (status "completed") still performs the terminal output
// recovery and clean success finish instead of being misclassified as a failure.
func TestResponseCompletedNormalRecoveryStillWorks(t *testing.T) {
	outbound := &ResponseOutbound{}
	resp, err := outbound.TransformStream(context.Background(), []byte(`{
		"type":"response.completed",
		"response":{
			"id":"resp_1","object":"response","model":"gpt-5.5","status":"completed",
			"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}]
		}
	}`))
	if err != nil {
		t.Fatalf("normal response.completed must not error: %v", err)
	}
	if resp == nil || len(resp.Choices) != 1 || resp.Choices[0].FinishReason == nil ||
		*resp.Choices[0].FinishReason != "stop" {
		t.Fatalf("expected clean stop finish, got %#v", resp)
	}
}
