package relay

import (
	"errors"
	"strings"
	"testing"
)

// The audit log must be able to tell "never reached upstream" from "reached
// upstream, response lost". The retry gate keys off wroteMeaningfulDownstream (a
// fact about the client), so the per-attempt message is the only place that fact
// can be recorded without changing the retry decision.
func TestAttemptAuditMessageMarksExecutedUpstream(t *testing.T) {
	// Never sent: the message must stay exactly the error text, with no claim that
	// the upstream ever saw the request.
	notSent := errors.New("failed to send request: dial tcp 127.0.0.1:9: connect: connection refused")
	if got := attemptAuditMessage(false, notSent); got != notSent.Error() {
		t.Fatalf("attempt that never reached upstream must not claim otherwise, got %q", got)
	}

	// Upstream answered, then this attempt failed locally (e.g. the response could
	// not be transformed): the log must record that the request was executed there,
	// and must still carry the original error text.
	got := attemptAuditMessage(true, errors.New("failed to transform outbound response"))
	if !strings.Contains(got, "executed upstream") {
		t.Fatalf("expected the upstream-executed note, got %q", got)
	}
	if !strings.Contains(got, "failed to transform outbound response") {
		t.Fatalf("expected the original error text to be preserved, got %q", got)
	}

	// Upstream answered but there is no error text to append to: the fact must still
	// be recorded rather than silently dropped.
	if got := attemptAuditMessage(true, nil); strings.TrimSpace(got) == "" {
		t.Fatal("expected a note when the upstream answered but no error text exists")
	}
}
