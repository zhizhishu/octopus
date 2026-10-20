package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/relay/balancer"
	"github.com/zhizhishu/octopus/internal/transformer/inbound"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// newNoBreakerChannel registers a DisableCircuitBreaker channel — the kind that retries
// like a direct client and is paced by the fixed one-second rescue round.
func newNoBreakerChannel(t *testing.T, upstream, name string, priority int, noBreaker bool) int {
	t.Helper()
	// Mirrors newPlainChannel (which the rescue-window tests use): the group's channel
	// model is the upstream name while the caller asks for "request-model", exactly like
	// newChatGin() does.
	channel := dbmodel.Channel{
		Name:                  name,
		Type:                  outbound.OutboundTypeOpenAIChat,
		Enabled:               true,
		DisableCircuitBreaker: noBreaker,
		BaseUrls:              []dbmodel.BaseUrl{{URL: upstream}},
		Keys:                  []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "the-key"}},
		Model:                 "upstream-model",
		ModelMapping:          map[string]string{"request-model": "upstream-model"},
		Priority:              priority,
	}
	if err := op.ChannelCreate(&channel, context.Background()); err != nil {
		t.Fatalf("create no-breaker channel: %v", err)
	}
	balancer.ResetChannel(channel.ID)
	return channel.ID
}

// TestRescuePacesAtOneSecondAndStaysBounded: one channel that always fails fast is retried
// on the fixed one-second rhythm, never back-to-back, and the whole rescue stays inside the
// request ceiling and ends with an explicit failure rather than a silent timeout.
func TestRescuePacesAtOneSecondAndStaysBounded(t *testing.T) {
	setupRescueDeadlineDB(t)
	withAutoRescueCap(t, 12*time.Second)
	withRequestCeiling(t, 12*time.Second)
	if err := op.SettingSetString(dbmodel.SettingKeyRelayInterventionEnabled, "true"); err != nil {
		t.Fatalf("enable intervention: %v", err)
	}

	var mu sync.Mutex
	var hits []time.Time
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, time.Now())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream boom"}}`))
	}))
	t.Cleanup(func() {
		upstream.CloseClientConnections()
		upstream.Close()
	})
	newNoBreakerChannel(t, upstream.URL, "always-502", 1, true)

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

	var res result
	select {
	case res = <-resCh:
	case <-time.After(30 * time.Second):
		t.Fatalf("the rescue loop never ended; the window/ceiling must stop it")
	}
	elapsed := time.Since(startedAt)

	mu.Lock()
	stamps := append([]time.Time(nil), hits...)
	mu.Unlock()

	if len(stamps) < 3 {
		t.Fatalf("expected the rescue to retry a few times before giving up, got %d upstream requests", len(stamps))
	}
	// Every dispatch is a fixed one-second round apart — never a hot loop.
	for i := 1; i < len(stamps); i++ {
		if gap := stamps[i].Sub(stamps[i-1]); gap < 800*time.Millisecond {
			t.Fatalf("the rescue retried %s after the previous round; it must be paced at ~1s (series=%v)", gap, stamps)
		}
	}
	if elapsed > 20*time.Second {
		t.Fatalf("the request ceiling (12s) must bound the whole rescue, took %s", elapsed)
	}
	if res.code == http.StatusOK {
		t.Fatalf("a channel that always fails must not end as a success, got %d %q", res.code, res.body)
	}
	// The caller must be told something explicit rather than being left to time out.
	if !strings.Contains(res.body, "error") && !strings.Contains(res.body, "retry") {
		t.Fatalf("expected an explicit failure body, got %d %q", res.code, res.body)
	}
	t.Logf("rescue rounds (12s cap): %d dispatches", len(stamps))
}

// TestSingleTransientFailureStillRescuesToSuccess pins the capability that must NOT
// regress: one transient failure on the first channel still ends as a successful answer
// from another channel.
func TestSingleTransientFailureStillRescuesToSuccess(t *testing.T) {
	setupRescueDeadlineDB(t)
	setIdleSetting(t, dbmodel.SettingKeyFirstTokenTimeOutDefault, "30")
	setIdleSetting(t, dbmodel.SettingKeyRelayStreamDataTimeoutSec, "300")

	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}))
	t.Cleanup(flaky.Close)
	// The failing channel is a no-breaker one, i.e. the eager path.
	newNoBreakerChannel(t, flaky.URL, "flaky-first", 1, true)

	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(chatContentEvent("RESCUED-AFTER-ONE-FAILURE")))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	t.Cleanup(healthy.Close)
	newNoBreakerChannel(t, healthy.URL, "healthy-second", 2, false)

	rec, c := newChatGin()
	Handler(inbound.InboundTypeOpenAIChat, c)

	body := rec.Body.String()
	if !strings.Contains(body, "RESCUED-AFTER-ONE-FAILURE") {
		t.Fatalf("one transient failure must still be rescued to a real answer, got %q", body)
	}
}

// TestRescueRoundBackoffIsFixedOneSecond pins the schedule itself: every round waits the
// same one second, regardless of round number or whether a no-breaker channel was seen.
func TestRescueRoundBackoffIsFixedOneSecond(t *testing.T) {
	for _, eager := range []bool{true, false} {
		for _, round := range []int{1, 2, 3, 6, 20} {
			if got := rescueRoundBackoff(round, eager); got != time.Second {
				t.Fatalf("rescueRoundBackoff(%d, eager=%v) = %s, want 1s", round, eager, got)
			}
		}
	}
}
