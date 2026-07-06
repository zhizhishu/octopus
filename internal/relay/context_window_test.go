package relay

import (
	"net/http"
	"testing"
)

// TestMatchContextWindowText locks the phrase matcher across every upstream octopus
// proxies — including native Anthropic ("prompt is too long") and Gemini token-count
// wording that carry NEITHER "context window" nor "context length" — and guards
// against false positives on ordinary errors.
func TestMatchContextWindowText(t *testing.T) {
	positives := []string{
		"context_length_exceeded",                                              // OpenAI code
		"context_too_large",                                                    // DeepSeek code
		"this model's maximum context length is 8192 tokens",                   // OpenAI message
		"prompt is too long: 201015 tokens > 200000 maximum",                   // native Anthropic
		"the input token count (1050000) exceeds the maximum number of tokens", // Gemini
		"context window exceeded for this request",                             // generic window
		"the context length is too long",                                       // generic length
	}
	for _, s := range positives {
		if !matchContextWindowText(s) {
			t.Errorf("expected context-window match for %q", s)
		}
	}

	negatives := []string{
		"",
		"rate limit exceeded, please slow down",
		"invalid request: model not found",
		"unauthorized: bad api key",
		"the model context is helpful here", // "context" without an exceeded verb
		"request too large: body exceeds 10mb",
		"you exceeded your monthly quota",
	}
	for _, s := range negatives {
		if matchContextWindowText(s) {
			t.Errorf("did not expect context-window match for %q", s)
		}
	}
}

// TestIsContextWindowError verifies the end-to-end classifier over real upstream
// JSON bodies from each provider, and that it is strictly gated on a 400 status so
// a 5xx/429 body mentioning "context" is never misread.
func TestIsContextWindowError(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"openai code field", 400, `{"error":{"code":"context_length_exceeded","message":"..."}}`, true},
		{"openai message", 400, `{"error":{"message":"This model's maximum context length is 8192 tokens, however you requested 9000."}}`, true},
		{"deepseek", 400, `{"error":{"code":"context_too_large","message":"context too large"}}`, true},
		{"native anthropic", 400, `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 201015 tokens > 200000 maximum"}}`, true},
		{"gemini", 400, `{"error":{"code":400,"message":"The input token count (1050000) exceeds the maximum number of tokens allowed (1048576).","status":"INVALID_ARGUMENT"}}`, true},
		{"plain bad request", 400, `{"error":{"message":"model not found"}}`, false},
		{"context word in 500", 500, `{"error":{"message":"maximum context length exceeded"}}`, false},
		{"context word in 429", 429, `{"error":{"message":"context_length_exceeded"}}`, false},
	}
	for _, c := range cases {
		err := &upstreamError{statusCode: c.status, body: c.body}
		if got := isContextWindowError(err); got != c.want {
			t.Errorf("%s: isContextWindowError = %v, want %v", c.name, got, c.want)
		}
	}

	// A non-upstream error must never be classified as a context-window error.
	if isContextWindowError(&localRelayError{status: 400, message: "prompt is too long"}) {
		t.Errorf("localRelayError must not be classified as context-window error")
	}
}

// TestRelayErrorResponseContextWindow: a context-window 400 must surface to the
// client as a clean 400 with the context_length_exceeded code — NOT masked as the
// default 502 that a plain upstream 400 becomes.
func TestRelayErrorResponseContextWindow(t *testing.T) {
	ctxErr := &upstreamError{statusCode: http.StatusBadRequest, code: "octopus_upstream_bad_request",
		body: `{"error":{"code":"context_length_exceeded"}}`}
	status, code, _ := relayErrorResponse(ctxErr)
	if status != http.StatusBadRequest {
		t.Fatalf("context-window error must stay 400, got %d", status)
	}
	if code != "context_length_exceeded" {
		t.Fatalf("expected context_length_exceeded code, got %q", code)
	}

	// Contrast: a plain upstream 400 is still masked as 502 (default redact policy).
	plain := &upstreamError{statusCode: http.StatusBadRequest, code: "octopus_upstream_bad_request",
		body: `{"error":{"message":"model not found"}}`}
	if status, _, _ := relayErrorResponse(plain); status != http.StatusBadGateway {
		t.Fatalf("plain 400 should mask to 502 by default, got %d", status)
	}
}

// TestShouldRecordBreakerFailureContextWindow: a context-window overflow is a client
// error and must NOT count toward the channel circuit breaker (else 10 oversized
// prompts trip a perfectly healthy channel), while a plain upstream 400 still counts.
func TestShouldRecordBreakerFailureContextWindow(t *testing.T) {
	ctxErr := &upstreamError{statusCode: http.StatusBadRequest,
		body: `{"error":{"code":"context_length_exceeded"}}`}
	if shouldRecordBreakerFailure(http.StatusBadRequest, ctxErr) {
		t.Fatalf("context-window error must not be counted toward the breaker")
	}

	plain := &upstreamError{statusCode: http.StatusBadRequest,
		body: `{"error":{"message":"model not found"}}`}
	if !shouldRecordBreakerFailure(http.StatusBadRequest, plain) {
		t.Fatalf("plain upstream 400 must still count toward the breaker")
	}
}
