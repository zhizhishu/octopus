package relay

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestQuotaExhausted429FastFailsWithoutChangingRateLimitRescue(t *testing.T) {
	testCases := []struct {
		name           string
		body           string
		withRetryAfter bool
		wantRescue     bool
	}{
		{
			name:       "provider_quota_exhausted",
			body:       `{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}`,
			wantRescue: false,
		},
		{
			name:       "provider_code_insufficient_quota",
			body:       `{"error":{"code":"insufficient_quota","message":"Insufficient account quota"}}`,
			wantRescue: false,
		},
		{
			name:       "rate_limited_with_concurrency",
			body:       `{"error":{"message":"Rate limit reached on concurrent requests: Limit 1, Used 1, Requested 1. Please try again in 5s."}}`,
			wantRescue: true,
		},
		{
			name:       "plain_429",
			body:       `{"error":{"code":429,"message":"Too many requests"}}`,
			wantRescue: true,
		},
		{
			name:           "quota_message_with_explicit_retry_after",
			body:           `{"error":{"message":"Resource has been exhausted (e.g. check quota)."}}`,
			withRetryAfter: true,
			wantRescue:     true,
		},
		{
			name:       "quota_message_with_retry_instruction",
			body:       `{"error":{"message":"Resource has been exhausted (e.g. check quota). Please try again in 5s."}}`,
			wantRescue: true,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := newUpstreamError(http.StatusTooManyRequests, []byte(tc.body))
			if tc.withRetryAfter {
				err.retryAfter = 5 * time.Second
				err.hasRetryAfter = true
			}
			if got := isEligibleForInterventionRescue(err); got != tc.wantRescue {
				t.Fatalf("rescue=%v, want=%v", got, tc.wantRescue)
			}
			wrapped := fmt.Errorf("forward: %w", err)
			if got := isEligibleForInterventionRescue(wrapped); got != tc.wantRescue {
				t.Fatalf("wrapped rescue=%v, want=%v", got, tc.wantRescue)
			}
		})
	}
}

func TestQuotaRejectionStillPreservesOriginalUpstreamStatus(t *testing.T) {
	err := newUpstreamError(http.StatusTooManyRequests, []byte(`{"error":{"message":"Resource has been exhausted (e.g. check quota)."}}`))
	if !carriesUpstreamStatus(err) {
		t.Fatal("provider's 429 must retain an upstream status for Chat/Responses error writers")
	}
	if err.StatusCode() != http.StatusTooManyRequests {
		t.Fatal("provider's status must never be remapped to 499 or 504")
	}
}
