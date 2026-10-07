package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/relay/balancer"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

// TestRescueDeadlineTightensOnHoldReentry proves the ONE automatic-recovery window is
// re-derived (and can only tighten) on every hold re-entry (R12). The deadline math and
// its AfterFunc currently run only when the hold context is first created
// (interventionCtx == nil); on re-entry (machine rescue retry / operator channel
// switch -> goto runIterator -> failure -> hold again) the deadline is NOT recomputed,
// so a clamp that tightened in the meantime (settings or branch change) never applies.
//
// Shape: a no-breaker channel whose upstream always answers 429, no-breaker budget
// initially 10s. First hold arms the deadline at T0+10s. The test then tightens the
// budget to 1s while the first 1s machine-rescue backoff is running. At the re-entry
// (~T0+1.1s) the recomputed deadline is T0+1s — already elapsed — so the relay must
// finish with octopus_rescue_timeout ~1s after the first failure. Without the fix the
// stale T0+10s timer keeps the relay retrying until ~T0+10s.
func TestRescueDeadlineTightensOnHoldReentry(t *testing.T) {
	setupRescueDeadlineDB(t)
	withAutoRescueCap(t, 10*time.Second)
	if err := op.SettingSetString(dbmodel.SettingKeyRelayNoBreakerRetryBudgetSec, "10"); err != nil {
		t.Fatalf("set no-breaker budget: %v", err)
	}

	var calls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		// Always 429: rescuable, so the hold loop keeps machine-retrying every second.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"busy"}}`))
	}))
	t.Cleanup(func() {
		upstream.CloseClientConnections()
		upstream.Close()
	})

	channel := dbmodel.Channel{
		Name:                  "tighten-channel",
		Type:                  outbound.OutboundTypeOpenAIChat,
		Enabled:               true,
		DisableCircuitBreaker: true,
		BaseUrls:              []dbmodel.BaseUrl{{URL: upstream.URL}},
		Keys:                  []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "the-key"}},
		Model:                 "upstream-model",
		ModelMapping:          map[string]string{"request-model": "upstream-model"},
		Priority:              1,
	}
	if err := op.ChannelCreate(&channel, context.Background()); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	balancer.ResetChannel(channel.ID)

	startedAt := time.Now()
	type outcome struct {
		code int
		body string
	}
	outCh := make(chan outcome, 1)
	go func() {
		rec, c := newChatGin()
		Handler(inbound.InboundTypeOpenAIChat, c)
		outCh <- outcome{code: rec.Code, body: rec.Body.String()}
	}()

	// The first hold (and its 10s deadline timer) is established before this returns;
	// tightening the setting now can only matter if the re-entry re-derives the deadline.
	if id := waitForPending(t, 5*time.Second); id == "" {
		t.Fatalf("no pending intervention was registered")
	}
	if err := op.SettingSetString(dbmodel.SettingKeyRelayNoBreakerRetryBudgetSec, "1"); err != nil {
		t.Fatalf("tighten no-breaker budget: %v", err)
	}

	select {
	case out := <-outCh:
		elapsed := time.Since(startedAt)
		if !strings.Contains(out.body, "octopus_rescue_timeout") {
			t.Fatalf("expected octopus_rescue_timeout terminal, got status %d body %q", out.code, out.body)
		}
		if elapsed > 5*time.Second {
			t.Fatalf("hold re-entry did not tighten the deadline: relay ran %v (want ~1-2s after the tightened 1s budget; the stale 10s timer kept it alive), body %q", elapsed, out.body)
		}
		if got := atomic.LoadInt32(&calls); got > 4 {
			t.Fatalf("too many upstream attempts (%d): the stale deadline kept machine-retrying past the tightened budget", got)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("relay did not return after the tightened budget; deadline is only computed once per hold")
	}
}
