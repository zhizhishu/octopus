package relay

import (
	"net/http"
	"testing"
	"time"
)

func TestParseRetryAfterSeconds(t *testing.T) {
	d, ok := parseRetryAfter("30")
	if !ok || d != 30*time.Second {
		t.Fatalf("expected 30s, got %v ok=%v", d, ok)
	}
}

func TestParseRetryAfterRejectsInvalid(t *testing.T) {
	for _, v := range []string{"", "0", "-5", "abc", "  "} {
		if d, ok := parseRetryAfter(v); ok {
			t.Fatalf("expected %q to be rejected, got %v", v, d)
		}
	}
}

func TestParseRetryAfterHTTPDate(t *testing.T) {
	future := time.Now().Add(45 * time.Second).UTC().Format(http.TimeFormat)
	d, ok := parseRetryAfter(future)
	if !ok || d <= 0 || d > 50*time.Second {
		t.Fatalf("expected ~45s from HTTP-date, got %v ok=%v", d, ok)
	}
}

func TestRetryAfterFromHeader(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "12")
	d, ok := retryAfterFromHeader(h)
	if !ok || d != 12*time.Second {
		t.Fatalf("expected 12s from header, got %v ok=%v", d, ok)
	}
	if _, ok := retryAfterFromHeader(nil); ok {
		t.Fatalf("nil header should report no retry-after")
	}
}

func TestRetryAfterFromErrorRoundTrips(t *testing.T) {
	upErr := newUpstreamError(http.StatusTooManyRequests, []byte(`{"error":{"code":"rate_limited"}}`))
	if _, ok := retryAfterFromError(upErr); ok {
		t.Fatalf("upstream error without retry-after should report none")
	}
	upErr.retryAfter = 25 * time.Second
	upErr.hasRetryAfter = true
	d, ok := retryAfterFromError(upErr)
	if !ok || d != 25*time.Second {
		t.Fatalf("expected 25s carried by upstream error, got %v ok=%v", d, ok)
	}
}
