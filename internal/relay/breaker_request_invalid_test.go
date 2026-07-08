package relay

import (
	"net/http"
	"testing"
)

// TestRequestInvalid4xxDoesNotChargeChannelBreaker guards the failover-pool fix.
//
// A deterministic request-shape rejection (the upstream refused THIS request's
// body) must NOT count toward a channel's circuit breaker: charging it benches a
// perfectly healthy channel for a client/gateway-shape mismatch and shrinks the
// failover pool — exactly how a strict channel (ele-deepseek) got benched by a
// malformed parallel-tool-call request. Genuine channel-health failures (5xx,
// timeouts) must still trip the breaker. Mirrors CLIProxyAPI's isRequestInvalidError.
func TestRequestInvalid4xxDoesNotChargeChannelBreaker(t *testing.T) {
	cases := []struct {
		name       string
		statusCode int
		body       string
		wantCharge bool // true => counts toward the circuit breaker (channel health)
	}{
		{
			// The DeepSeek/ele-deepseek serde rejection of a malformed body — the
			// misleading "ChatCompletionToolChoiceOption" deserialize 400.
			name:       "deepseek deserialize 400",
			statusCode: http.StatusBadRequest,
			body:       `{"error":{"message":"Failed to deserialize the JSON body into the target type: tool_choice: data did not match any variant of untagged enum ChatCompletionToolChoiceOption at line 1 column 352359"}}`,
			wantCharge: false,
		},
		{
			name:       "openai invalid_request_error 400",
			statusCode: http.StatusBadRequest,
			body:       `{"error":{"type":"invalid_request_error","message":"Invalid schema for text.format"}}`,
			wantCharge: false,
		},
		{
			name:       "gemini INVALID_ARGUMENT 400",
			statusCode: http.StatusBadRequest,
			body:       `{"error":{"code":400,"message":"Request contains an invalid argument.","status":"INVALID_ARGUMENT"}}`,
			wantCharge: false,
		},
		{
			name:       "unprocessable entity 422 invalid_request_error",
			statusCode: http.StatusUnprocessableEntity,
			body:       `{"error":{"type":"invalid_request_error","message":"messages: field required"}}`,
			wantCharge: false,
		},
		{
			// An opaque 400 with no request-shape marker still counts: it may be a
			// channel-side quirk we should not silently keep hammering forever.
			name:       "opaque 400 without marker",
			statusCode: http.StatusBadRequest,
			body:       `{"error":{"message":"bad gateway upstream"}}`,
			wantCharge: true,
		},
		{
			// Genuine channel-health failures must still trip the breaker.
			name:       "server 502",
			statusCode: http.StatusBadGateway,
			body:       `{"error":{"message":"upstream unavailable"}}`,
			wantCharge: true,
		},
		{
			name:       "server 500 non-json",
			statusCode: http.StatusInternalServerError,
			body:       `internal error`,
			wantCharge: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := newUpstreamError(c.statusCode, []byte(c.body))
			if got := shouldRecordBreakerFailure(c.statusCode, err); got != c.wantCharge {
				t.Fatalf("shouldRecordBreakerFailure(%d) = %v, want %v (body=%s)", c.statusCode, got, c.wantCharge, c.body)
			}
		})
	}
}
