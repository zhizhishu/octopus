package relay

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/relay/balancer"
	"github.com/bestruirui/octopus/internal/relay/intervention"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

// withAutoRescueCap shortens the automatic-recovery cap for one test so the 300s ceiling
// can be exercised without a real 300s wait. Restored on cleanup.
func withAutoRescueCap(t *testing.T, cap time.Duration) {
	t.Helper()
	prev := autoRescueCap
	autoRescueCap = cap
	t.Cleanup(func() { autoRescueCap = prev })
}

// newPlainChannel registers an ordinary circuit-breaker channel so the rescue path is
// driven by the automatic intervention hold, never the no-breaker branch.
func newPlainChannel(t *testing.T, upstream string) {
	t.Helper()
	channel := dbmodel.Channel{
		Name:         "plain-stall-channel",
		Type:         outbound.OutboundTypeOpenAIChat,
		Enabled:      true,
		BaseUrls:     []dbmodel.BaseUrl{{URL: upstream}},
		Keys:         []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "the-key"}},
		Model:        "upstream-model",
		ModelMapping: map[string]string{"request-model": "upstream-model"},
		Priority:     1,
	}
	if err := op.ChannelCreate(&channel, context.Background()); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	balancer.ResetChannel(channel.ID)
}

// TestAutoRescueCapAppliesToPlainChannel proves a plain-channel automatic rescue is capped
// at autoRescueCap from the first rescuable failure and does NOT borrow the 1800s operator
// hold timeout: the relay stops cleanly with octopus_rescue_timeout after ~cap.
func TestAutoRescueCapAppliesToPlainChannel(t *testing.T) {
	setupRescueDeadlineDB(t)
	withAutoRescueCap(t, 3*time.Second)
	if err := op.SettingSetString(dbmodel.SettingKeyRelayInterventionEnabled, "true"); err != nil {
		t.Fatalf("enable intervention: %v", err)
	}
	// relay_intervention_timeout_seconds stays at a large value; it must be inert here.
	if err := op.SettingSetString(dbmodel.SettingKeyRelayInterventionTimeoutSec, "1800"); err != nil {
		t.Fatalf("set operator hold: %v", err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream boom"}}`))
	}))
	t.Cleanup(func() {
		upstream.CloseClientConnections()
		upstream.Close()
	})
	newPlainChannel(t, upstream.URL)

	type result struct {
		code int
		body string
	}
	resCh := make(chan result, 1)
	startedAt := time.Now()
	go func() {
		rec, c := newChatGin()
		Handler(inbound.InboundTypeOpenAIChat, c)
		resCh <- result{code: rec.Code, body: rec.Body.String()}
	}()

	select {
	case res := <-resCh:
		if elapsed := time.Since(startedAt); elapsed > 6*time.Second {
			t.Fatalf("plain-channel rescue ran %v; the 1800s operator hold must not apply", elapsed)
		}
		if res.code != http.StatusGatewayTimeout || !strings.Contains(res.body, "octopus_rescue_timeout") {
			t.Fatalf("expected a clean octopus_rescue_timeout terminal, got status %d body %q", res.code, res.body)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("plain-channel rescue did not stop at the automatic-recovery cap")
	}
	if got := len(intervention.List()); got != 0 {
		t.Fatalf("expected empty intervention registry, got %d", got)
	}
}

// TestRecoveredBodySurvivesRescueDeadline proves that once a rescue retry starts producing
// real content the automatic-recovery deadline is released: the body is delivered in full
// even though it outlives the cap, with no error frame and no truncation.
func TestRecoveredBodySurvivesRescueDeadline(t *testing.T) {
	setupRescueDeadlineDB(t)
	withAutoRescueCap(t, 3*time.Second)

	var mu sync.Mutex
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"upstream boom"}}`))
			return
		}
		// The rescue retry: stream a body that keeps flowing well past the 3s cap.
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		writeContent := func(text string) {
			_, _ = fmt.Fprintf(w, "data: {\"id\":\"chatcmpl_ok\",\"object\":\"chat.completion.chunk\",\"created\":123,\"model\":\"upstream-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q}}]}\n\n", text)
			if flusher != nil {
				flusher.Flush()
			}
		}
		writeContent("start")
		for i := 1; i <= 5; i++ {
			time.Sleep(900 * time.Millisecond)
			writeContent(fmt.Sprintf("chunk%d", i))
		}
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl_ok\",\"object\":\"chat.completion.chunk\",\"created\":123,\"model\":\"upstream-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}))
	t.Cleanup(func() {
		upstream.CloseClientConnections()
		upstream.Close()
	})
	newRescueChannel(t, upstream.URL)

	type result struct{ body string }
	resCh := make(chan result, 1)
	go func() {
		rec, c := newChatGin()
		Handler(inbound.InboundTypeOpenAIChat, c)
		resCh <- result{body: rec.Body.String()}
	}()

	select {
	case res := <-resCh:
		if !strings.Contains(res.body, "[DONE]") {
			t.Fatalf("recovered body must complete with [DONE], got %q", res.body)
		}
		if strings.Contains(res.body, "octopus_rescue_timeout") || strings.Contains(res.body, "automatic rescue timed out") {
			t.Fatalf("the rescue deadline cut a recovered body: %q", res.body)
		}
		for i := 1; i <= 5; i++ {
			if !strings.Contains(res.body, fmt.Sprintf("chunk%d", i)) {
				t.Fatalf("streamed body was truncated: missing chunk%d in %q", i, res.body)
			}
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("relay did not deliver the recovered body")
	}
	if got := len(intervention.List()); got != 0 {
		t.Fatalf("expected empty intervention registry, got %d", got)
	}
}

// TestNoNewAttemptStartedAfterRescueDeadline proves the deadline stops the loop cleanly:
// after the handler returns at the cap, no further upstream attempt is fired.
func TestNoNewAttemptStartedAfterRescueDeadline(t *testing.T) {
	setupRescueDeadlineDB(t)
	withAutoRescueCap(t, 2*time.Second)

	var mu sync.Mutex
	calls := 0
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
			return
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() {
		close(release)
		upstream.CloseClientConnections()
		upstream.Close()
	})
	newRescueChannel(t, upstream.URL)

	done := make(chan struct{})
	go func() {
		_, c := newChatGin()
		Handler(inbound.InboundTypeOpenAIChat, c)
		close(done)
	}()

	if id := waitForPending(t, 5*time.Second); id == "" {
		t.Fatalf("no pending intervention was registered")
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("relay did not stop at the automatic-recovery cap")
	}

	mu.Lock()
	after := calls
	mu.Unlock()
	time.Sleep(1200 * time.Millisecond)
	mu.Lock()
	still := calls
	mu.Unlock()
	if still != after {
		t.Fatalf("a new upstream attempt was started after the rescue deadline: %d -> %d", after, still)
	}
}
