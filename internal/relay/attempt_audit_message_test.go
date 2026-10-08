package relay

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

// The audit log must be able to tell "never reached upstream" from "reached
// upstream, response lost" from "upstream explicitly rejected the request". The
// retry gate keys off wroteMeaningfulDownstream (a fact about the client), so the
// per-attempt message is the only place that fact can be recorded without
// changing the retry decision.

// containsExecutedUpstreamClaim reports whether msg still carries the OLD audit
// claim that the request was executed upstream. Because the rejection note is
// worded "the request was not run upstream", a bare substring check for
// "executed upstream" is unambiguous: only a message that really makes the claim
// (the original note) can match it.
func containsExecutedUpstreamClaim(msg string) bool {
	return strings.Contains(msg, "executed upstream")
}

func TestAttemptAuditMessageMarksExecutedUpstream(t *testing.T) {
	// Never sent: the message must stay exactly the error text, with no claim that
	// the upstream ever saw the request.
	notSent := errors.New("failed to send request: dial tcp 127.0.0.1:9: connect: connection refused")
	if got := attemptAuditMessage(false, 0, notSent); got != notSent.Error() {
		t.Fatalf("attempt that never reached upstream must not claim otherwise, got %q", got)
	}
	if got := attemptAuditMessage(false, http.StatusBadRequest, notSent); got != notSent.Error() {
		t.Fatalf("not-responded attempt must stay the bare error text even with a status, got %q", got)
	}

	// Upstream answered with a 2xx, then this attempt failed locally (the response
	// could not be transformed): the log must record that the request was executed
	// there, and must still carry the original error text.
	got := attemptAuditMessage(true, http.StatusOK, errors.New("failed to transform outbound response"))
	if !strings.Contains(got, "executed upstream") {
		t.Fatalf("expected the upstream-executed note, got %q", got)
	}
	if !strings.Contains(got, "failed to transform outbound response") {
		t.Fatalf("expected the original error text to be preserved, got %q", got)
	}

	// Upstream answered (here a 5xx) but there is no error text to append to: the
	// fact must still be recorded rather than silently dropped.
	if got := attemptAuditMessage(true, http.StatusServiceUnavailable, nil); strings.TrimSpace(got) == "" {
		t.Fatal("expected a note when the upstream answered but no error text exists")
	}
}

// A deterministic client rejection (4xx except 429) means the upstream explicitly
// refused this request: nothing was executed there. The note must say so and must
// never claim "executed upstream".
func TestAttemptAuditMessageDeterministicRejection(t *testing.T) {
	err := errors.New("Upstream request failed (status 400, code octopus_upstream_bad_request).")
	for _, status := range []int{http.StatusBadRequest, http.StatusUnprocessableEntity, http.StatusNotFound} {
		got := attemptAuditMessage(true, status, err)
		if containsExecutedUpstreamClaim(got) {
			t.Fatalf("status %d rejection must not claim the request was executed upstream, got %q", status, got)
		}
		if !strings.Contains(got, "upstream rejected this request (the request was not run upstream)") {
			t.Fatalf("status %d rejection must carry the rejected note, got %q", status, got)
		}
		if !strings.Contains(got, err.Error()) {
			t.Fatalf("status %d rejection must preserve the original error text, got %q", status, got)
		}
	}
	// Rejection with no error text: the rejected note stands alone.
	if got := attemptAuditMessage(true, http.StatusBadRequest, nil); got != "upstream rejected this request (the request was not run upstream)" {
		t.Fatalf("expected the bare rejected note when there is no error text, got %q", got)
	}
}

// 429 / 5xx / status 0 (transient stream failure) / 2xx (stream died mid-flight)
// are NOT deterministic rejections: the request reached the upstream and the
// response was lost, so the original "executed upstream" note stays.
func TestAttemptAuditMessageKeepsExecutedUpstreamNote(t *testing.T) {
	err := errors.New("failed to transform outbound response")
	streamErr := errors.New("stream ended before any content was produced")
	for _, tc := range []struct {
		name   string
		status int
		err    error
	}{
		{"rate limited", http.StatusTooManyRequests, err},
		{"server error", http.StatusServiceUnavailable, err},
		{"no-status transient stream failure", 0, streamErr},
		{"stream broke after 200", http.StatusOK, err},
	} {
		got := attemptAuditMessage(true, tc.status, tc.err)
		if !strings.Contains(got, "executed upstream") {
			t.Fatalf("%s (status %d) must keep the executed-upstream note, got %q", tc.name, tc.status, got)
		}
	}
}

// Production regression: an unofficial client hit gpt-6-astra and the upstream
// answered 400 with this exact body — an explicit rejection, nothing executed.
func TestAttemptAuditMessageProductionGpt6AstraRejection(t *testing.T) {
	upErr := newUpstreamError(http.StatusBadRequest, []byte(`{"error":{"message":"Unsupported value: 'none' is not supported with the 'gpt-6-astra' model. Supported values are: 'low', 'medium', 'high', 'xhigh', and 'max'.","type":"invalid_request_error","param":"reasoning_effort","code":"unsupported_value"}}`))
	got := attemptAuditMessage(true, upErr.StatusCode(), upErr)
	if containsExecutedUpstreamClaim(got) {
		t.Fatalf("deterministic 400 rejection must not claim the request was executed upstream, got %q", got)
	}
	if !strings.Contains(got, "upstream rejected this request (the request was not run upstream)") {
		t.Fatalf("expected the rejected note, got %q", got)
	}
	if !strings.Contains(got, "Supported values are: 'low', 'medium', 'high', 'xhigh', and 'max'.") {
		t.Fatalf("expected the upstream body summary to be preserved, got %q", got)
	}
}
