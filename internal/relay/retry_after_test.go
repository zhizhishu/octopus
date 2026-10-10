package relay

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
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

func TestUpstreamRetryAfterReportsZeroWithoutHint(t *testing.T) {
	if got := upstreamRetryAfter(newUpstreamError(http.StatusTooManyRequests, []byte(`{}`))); got != 0 {
		t.Fatalf("hint-less upstream error should report zero, got %v", got)
	}
	upErr := newUpstreamError(http.StatusTooManyRequests, []byte(`{}`))
	upErr.retryAfter = 9 * time.Second
	upErr.hasRetryAfter = true
	if got := upstreamRetryAfter(upErr); got != 9*time.Second {
		t.Fatalf("expected the carried 9s hint, got %v", got)
	}
}

// A provider that answers "come back later" states how long to wait. A client routed
// through the relay must receive that same pacing hint; dropping it leaves claude-code /
// codex nothing to wait on, so a brief rate-limit degenerates into a self-inflicted
// hammering loop that makes the relay look far worse than the raw provider.
func TestWriteUpstreamRetryAfterHintForwardsPacing(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name       string
		httpStatus int
		hint       time.Duration
		hasHint    bool
		wantHeader string
	}{
		{name: "429 forwards whole seconds", httpStatus: http.StatusTooManyRequests, hint: 42 * time.Second, hasHint: true, wantHeader: "42"},
		{name: "503 forwards whole seconds", httpStatus: http.StatusServiceUnavailable, hint: 7 * time.Second, hasHint: true, wantHeader: "7"},
		{name: "sub-second rounds up, never down", httpStatus: http.StatusTooManyRequests, hint: 1500 * time.Millisecond, hasHint: true, wantHeader: "2"},
		{name: "no hint means no header", httpStatus: http.StatusTooManyRequests, wantHeader: ""},
		{name: "client error is not a pacing signal", httpStatus: http.StatusBadRequest, hint: 30 * time.Second, hasHint: true, wantHeader: ""},
		{name: "gateway error is not a pacing signal", httpStatus: http.StatusInternalServerError, hint: 30 * time.Second, hasHint: true, wantHeader: ""},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		upErr := newUpstreamError(tc.httpStatus, []byte(`{"error":{"message":"slow down"}}`))
		if tc.hasHint {
			upErr.retryAfter = tc.hint
			upErr.hasRetryAfter = true
		}
		writeUpstreamRetryAfterHint(c, tc.httpStatus, upErr)
		if got := rec.Header().Get("Retry-After"); got != tc.wantHeader {
			t.Errorf("%s: Retry-After = %q, want %q", tc.name, got, tc.wantHeader)
		}
	}
}

// Hintless failures must leave the header unset rather than inventing a wait the
// provider never asked for.
func TestWriteUpstreamRetryAfterHintIgnoresHintlessErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	writeUpstreamRetryAfterHint(c, http.StatusTooManyRequests, nil)
	writeUpstreamRetryAfterHint(c, http.StatusTooManyRequests, errors.New("plain failure"))
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Fatalf("expected no Retry-After for hintless errors, got %q", got)
	}
}
