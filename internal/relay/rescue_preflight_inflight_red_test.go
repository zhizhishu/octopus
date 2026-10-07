package relay

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/relay/balancer"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

// TestRescuePreflightInflight proves an attempt that starts BEFORE the intervention
// hold is built (a pre-hold candidate on a later key) is still bounded by the ONE
// automatic-recovery window (R10). Before the fix, beginControlledAttempt created the
// attempt context as a plain WithCancel child of the client context, so a pre-hold
// attempt could hang on a stalled upstream far past autoRescueCap: the rescue deadline
// timer does not exist yet and cannot cancel anything, and the key-loop sweep guard
// (rescueDeadlineExpired) only runs BETWEEN attempts, never during one.
//
// Shape: key1 answers 429 immediately (pins recoveryStartedAt = T1 and advances the
// key loop — shouldTryNextChannelKey only lets 429 through to the next key), key2
// then hangs on a black-holed upstream. With autoRescueCap=3s the relay must finish
// ~T1+3s (the deadline cancels the preflight attempt in flight) and the upstream must
// observe key2's request context being canceled. Without the fix the preflight
// attempt hangs the full 30s and the relay never returns within the cap.
func TestRescuePreflightInflight(t *testing.T) {
	setupRescueDeadlineDB(t)
	withAutoRescueCap(t, 3*time.Second)

	var mu sync.Mutex
	activeConns := 0
	var secondReachedAtNano int64
	secondExit := make(chan string, 1)

	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), "preflight-second") {
			atomic.StoreInt64(&secondReachedAtNano, time.Now().UnixNano())
			// Drain the body so net/http starts its background connection read;
			// without this the server-side request context never observes the
			// client-side abort (Go only detects disconnects once the body is
			// consumed), and the cancellation below would be invisible.
			_, _ = io.Copy(io.Discard, r.Body)
			select {
			case <-r.Context().Done():
				secondExit <- "canceled"
			case <-time.After(30 * time.Second):
				secondExit <- "timeout"
			}
			return
		}
		// First key: fast 429 — a rescuable failure that pins the recovery window
		// and makes the key loop advance to the preflight (second) key.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"busy"}}`))
	}))
	upstream.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		mu.Lock()
		switch state {
		case http.StateNew:
			activeConns++
		case http.StateClosed, http.StateHijacked:
			activeConns--
		}
		mu.Unlock()
	}
	upstream.Start()
	t.Cleanup(func() {
		upstream.CloseClientConnections()
		upstream.Close()
	})

	// One no-breaker channel with two keys: the second key is the pre-hold candidate.
	channel := dbmodel.Channel{
		Name:                  "preflight-channel",
		Type:                  outbound.OutboundTypeOpenAIChat,
		Enabled:               true,
		DisableCircuitBreaker: true,
		BaseUrls:              []dbmodel.BaseUrl{{URL: upstream.URL}},
		Keys: []dbmodel.ChannelKey{
			{Enabled: true, ChannelKey: "preflight-first"},
			{Enabled: true, ChannelKey: "preflight-second"},
		},
		Model:        "upstream-model",
		ModelMapping: map[string]string{"request-model": "upstream-model"},
		Priority:     1,
	}
	if err := op.ChannelCreate(&channel, context.Background()); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	balancer.ResetChannel(channel.ID)

	type outcome struct {
		code    int
		body    string
		endNano int64
	}
	outCh := make(chan outcome, 1)
	go func() {
		rec, c := newChatGin()
		Handler(inbound.InboundTypeOpenAIChat, c)
		outCh <- outcome{code: rec.Code, body: rec.Body.String(), endNano: time.Now().UnixNano()}
	}()

	select {
	case out := <-outCh:
		// 1) The relay returned within the window: handler end minus the moment the
		// preflight attempt reached the upstream must be ~cap (the deadline fires at
		// recoveryStartedAt+cap and the preflight starts ~immediately after T1).
		reached := time.Unix(0, atomic.LoadInt64(&secondReachedAtNano))
		if reached.IsZero() {
			t.Fatalf("the preflight (second-key) attempt never reached the upstream; body=%q", out.body)
		}
		if elapsed := time.Duration(out.endNano - reached.UnixNano()); elapsed > 3*time.Second+1500*time.Millisecond {
			t.Fatalf("preflight attempt outlived the rescue cap: ran %v past autoRescueCap=3s (status %d, body %q)", elapsed, out.code, out.body)
		}
		// 2) The upstream must have observed the preflight attempt being canceled —
		// not left hanging until the 30s upstream-side timeout.
		select {
		case reason := <-secondExit:
			if reason != "canceled" {
				t.Fatalf("preflight upstream wait ended by %s; the rescue cap must cancel the pre-hold in-flight attempt", reason)
			}
		case <-time.After(2 * time.Second):
			mu.Lock()
			conns := activeConns
			mu.Unlock()
			t.Fatalf("preflight upstream request was never canceled (active upstream conns: %d)", conns)
		}
	case <-time.After(12 * time.Second):
		t.Fatalf("relay did not return within autoRescueCap; the pre-hold in-flight attempt is not bounded by the rescue window")
	}
}
