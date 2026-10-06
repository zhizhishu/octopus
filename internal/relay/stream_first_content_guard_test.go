package relay

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	openaiOutbound "github.com/bestruirui/octopus/internal/transformer/outbound/openai"
)

// chatContentEvent is a real content chunk: this is what disarms the guard.
func chatContentEvent(text string) string {
	return `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":123,"model":"gpt-5.5","choices":[{"index":0,"delta":{"content":"` + text + `"}}]}` + "\n\n"
}

// The first-content guard on the real streaming path (handleStreamResponse) had no
// test coverage at all before this file: every existing first-token test drove
// handleResponse or handleStreamResponseAsNonStream. That matters because the
// stream path is the one production traffic uses, and the invariant that makes the
// guard work — an opener must NOT renew it — was therefore unguarded.
//
// Proven on a real instance (isolated build of 8ca14d3, instrumented fake upstream):
//   * guard OFF + opener drip          -> request hung until the client's own deadline
//   * guard ON  + opener drip          -> every attempt cut at the budget (~10s)
//   * guard ON  + content then 25s drip-> ran the full 25s, never cut
//   * upstream never sends headers     -> nothing in the relay cut it (separate gap)
// These tests pin the first three behaviours so a later refactor cannot silently
// lose them.

// dripUntilBlocked writes payload repeatedly to w until the reader side goes away
// (the guard closes the body) or the deadline passes. Errors are expected and
// ignored: a closed pipe is exactly the success path for the cutting tests.
func dripUntilBlocked(w io.Writer, payload string, every, max time.Duration) {
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		if _, err := io.WriteString(w, payload); err != nil {
			return
		}
		time.Sleep(every)
	}
}

// An upstream that only ever sends openers must be cut at the budget: openers do
// not count as progress, and they must not push the deadline back.
func TestStreamFirstContentGuardCutsOpenerOnlyStream(t *testing.T) {
	rec := httptest.NewRecorder()
	ra, c := newChatPreludeAttempt(rec)
	ra.firstTokenTimeOutSec = 1

	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		dripUntilBlocked(pw, chatRoleDeltaEvent("1"), 150*time.Millisecond, 6*time.Second)
	}()

	response := &http.Response{
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:   pr,
	}

	start := time.Now()
	err := ra.handleStreamResponse(c.Request.Context(), response, &openaiOutbound.ChatOutbound{})
	elapsed := time.Since(start)

	if err == nil || !strings.Contains(err.Error(), "first token timeout") {
		t.Fatalf("opener-only stream must be cut by the first-content guard, got err=%v after %s", err, elapsed)
	}
	// With renewal it would have run the writer's full 6s. Allow generous slack for
	// a loaded machine while still proving the deadline was not pushed back.
	if elapsed > 3*time.Second {
		t.Fatalf("guard must fire at the budget (~1s), not be renewed by openers; took %s", elapsed)
	}
	if body := rec.Body.String(); body != "" {
		t.Fatalf("nothing meaningful was sent, so nothing may reach the client, got %q", body)
	}
}

// Content is progress: once a real token arrives the guard must be disarmed, so a
// legitimately slow long answer is never truncated by it.
func TestStreamFirstContentGuardDisarmedByContent(t *testing.T) {
	rec := httptest.NewRecorder()
	ra, c := newChatPreludeAttempt(rec)
	ra.firstTokenTimeOutSec = 1

	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		// Opener first (buffered), then real content well before the 1s budget.
		if _, err := io.WriteString(pw, chatRoleDeltaEvent("1")); err != nil {
			return
		}
		if _, err := io.WriteString(pw, chatContentEvent("first")); err != nil {
			return
		}
		// Keep streaming content far past the budget: the guard must stay disarmed.
		dripUntilBlocked(pw, chatContentEvent("more"), 150*time.Millisecond, 2500*time.Millisecond)
		// Terminate cleanly so the caller sees a normal completion.
		_, _ = io.WriteString(pw, `data: [DONE]`+"\n\n")
	}()

	response := &http.Response{
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:   pr,
	}

	start := time.Now()
	err := ra.handleStreamResponse(c.Request.Context(), response, &openaiOutbound.ChatOutbound{})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("content disarms the guard; a long answer must not be cut, got %v after %s", err, elapsed)
	}
	if elapsed < 2*time.Second {
		t.Fatalf("stream should have run past the 1s budget, took only %s", elapsed)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"content":"first"`) {
		t.Fatalf("content must reach the client, got %q", body)
	}
	if strings.Contains(body, "first token timeout") {
		t.Fatalf("no timeout error may appear in a healthy stream, got %q", body)
	}
}

// Zero means "disabled", and that contract must not drift: an opener-only upstream
// is allowed to keep the request open (the client's own deadline is in charge).
func TestStreamFirstContentGuardDisabledWhenBudgetIsZero(t *testing.T) {
	rec := httptest.NewRecorder()
	ra, c := newChatPreludeAttempt(rec)
	ra.firstTokenTimeOutSec = 0

	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		dripUntilBlocked(pw, chatRoleDeltaEvent("1"), 150*time.Millisecond, 1500*time.Millisecond)
		_, _ = io.WriteString(pw, `data: [DONE]`+"\n\n")
	}()

	response := &http.Response{
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:   pr,
	}

	start := time.Now()
	err := ra.handleStreamResponse(c.Request.Context(), response, &openaiOutbound.ChatOutbound{})
	elapsed := time.Since(start)

	// Budget 0 must not arm the guard. The stream still ends on its own under the
	// pre-existing "ended without internal response" contract (an opener-only stream
	// carries no answer) — that is a different rule and is not what this pins.
	if err != nil && strings.Contains(err.Error(), "first token timeout") {
		t.Fatalf("budget 0 must not arm the guard, got %v", err)
	}
	if elapsed < time.Second {
		t.Fatalf("budget 0 must not cut the stream early, took only %s", elapsed)
	}
}
