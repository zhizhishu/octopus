package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/relay/balancer"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

// newNoBreakerChannel registers a DisableCircuitBreaker channel — the kind that used to
// put the rescue loop on a flat 1s rhythm (138 attempts inside one 300s window in
// production). Its breaker is off by design, so nothing else throttles the retries.
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

// TestRescueBackoffGrowsInsteadOfHotLooping is the production defect: one channel that
// always fails fast, retried in place with a flat rhythm until the budget died. The
// schedule must now grow, and the whole thing must stay inside the request ceiling.
func TestRescueBackoffGrowsInsteadOfHotLooping(t *testing.T) {
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
		t.Fatalf("the rescue loop never ended; a growing backoff must stop it")
	}
	elapsed := time.Since(startedAt)

	mu.Lock()
	stamps := append([]time.Time(nil), hits...)
	mu.Unlock()

	if len(stamps) < 3 {
		t.Fatalf("expected the rescue to retry a few times before giving up, got %d upstream requests", len(stamps))
	}
	if len(stamps) > 8 {
		t.Fatalf("a 12s window with a growing backoff must not fit %d attempts (the hot loop this pins measured 138 per 300s)", len(stamps))
	}
	intervals := make([]time.Duration, 0, len(stamps)-1)
	for i := 1; i < len(stamps); i++ {
		intervals = append(intervals, stamps[i].Sub(stamps[i-1]))
	}
	first, last := intervals[0], intervals[len(intervals)-1]
	if last < 2*first {
		t.Fatalf("the retry intervals must grow (exponential + jitter), got first=%s last=%s series=%v", first, last, intervals)
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
	t.Logf("rescue retry intervals (12s cap): %v", intervals)
}

// TestSingleTransientFailureStillRescuesToSuccess pins the capability that must NOT
// regress while adding backoff: one transient failure on the first channel still ends as a
// successful answer from another channel.
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
	// The failing channel is a no-breaker one, i.e. the eager path that used to hot-loop.
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

// TestRescueRotationDropsRepeatedlyFailingChannel pins the rotation rule: after the
// configured number of consecutive failures a channel is excluded, and when that leaves no
// candidate the loop must stop instead of retrying forever.
func TestRescueRotationDropsRepeatedlyFailingChannel(t *testing.T) {
	attemptsFor := func(id int, name string) []dbmodel.ChannelAttempt {
		return []dbmodel.ChannelAttempt{{ChannelID: id, ChannelName: name}}
	}

	rotation := newRescueRotation()
	for round := 1; round < maxRescueRoundsPerSameChannel; round++ {
		if exhausted, _, _, _ := rotation.noteRound(attemptsFor(7, "bad")); exhausted {
			t.Fatalf("round %d must not exhaust the channel yet", round)
		}
	}
	exhausted, id, name, rounds := rotation.noteRound(attemptsFor(7, "bad"))
	if !exhausted || id != 7 || rounds != maxRescueRoundsPerSameChannel {
		t.Fatalf("round %d must drop channel 7, got exhausted=%v id=%d rounds=%d", maxRescueRoundsPerSameChannel, exhausted, id, rounds)
	}
	if name != "bad" {
		t.Fatalf("expected the channel name to be reported for the switch log, got %q", name)
	}
	// Re-reporting the same channel must not log a second switch.
	if again, _, _, _ := rotation.noteRound(attemptsFor(7, "bad")); again {
		t.Fatalf("the switch must be reported exactly once per channel")
	}

	group := dbmodel.Group{Items: []dbmodel.GroupItem{{ChannelID: 7}, {ChannelID: 9}}}
	filtered, anyLeft := rotation.exclude(group)
	if !anyLeft || len(filtered.Items) != 1 || filtered.Items[0].ChannelID != 9 {
		t.Fatalf("the exhausted channel must be dropped from the candidate list, got %+v", filtered.Items)
	}
	onlyBad := dbmodel.Group{Items: []dbmodel.GroupItem{{ChannelID: 7}}}
	if _, anyLeft := rotation.exclude(onlyBad); anyLeft {
		t.Fatalf("with every candidate exhausted the loop must be told to stop")
	}

	// A different channel resets the streak: three failures must be CONSECUTIVE.
	fresh := newRescueRotation()
	sequence := []int{7, 7, 9, 7, 9}
	for _, chID := range sequence {
		if exhausted, _, _, _ := fresh.noteRound(attemptsFor(chID, "x")); exhausted {
			t.Fatalf("channel %d must not be dropped from a non-consecutive sequence %v", chID, sequence)
		}
	}
}

// TestRescueRoundBackoffKeepsEagerStartAndGrows pins the schedule itself: the first rounds
// stay eager (a no-breaker channel is meant to retry like a direct client), and the series
// then grows towards the 15s ceiling.
func TestRescueRoundBackoffKeepsEagerStartAndGrows(t *testing.T) {
	for _, eager := range []bool{true, false} {
		first := rescueRoundBackoff(1, eager)
		if first < 800*time.Millisecond || first > 1200*time.Millisecond {
			t.Fatalf("round 1 must stay about a second (eager=%v), got %s", eager, first)
		}
		late := rescueRoundBackoff(6, eager)
		if late < 12*time.Second || late > 18*time.Second {
			t.Fatalf("the schedule must grow to the 15s cap by round 6 (eager=%v), got %s", eager, late)
		}
		if late <= first {
			t.Fatalf("the schedule must grow, got first=%s late=%s", first, late)
		}
		// The no-breaker eager rounds are the only difference between the two modes.
		if eager {
			if second := rescueRoundBackoff(2, true); second > 1200*time.Millisecond {
				t.Fatalf("a no-breaker channel keeps its eager second round, got %s", second)
			}
			if third := rescueRoundBackoff(3, true); third < 1600*time.Millisecond {
				t.Fatalf("eagerness must stop after %d rounds, round 3 got %s", noBreakerEagerRounds, third)
			}
		}
	}
}
