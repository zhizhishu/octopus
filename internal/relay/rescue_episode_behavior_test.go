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
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	"github.com/gin-gonic/gin"
)

// resetRescueEpisodes swaps the process-wide episode gate for a fresh store and restores
// the previous pointer on cleanup. The gate is GLOBAL and every handler test now drives it
// via acquireRescueEpisode; without this reset, a test that exhausts the shared
// api=0/user=0/"ping" episode would suppress it for every sibling test that resends the
// same message (they would then skip their rescue and fail). Called from the shared DB
// setups (setupRescueDeadlineDB / setupRelayErrorDB) so every rescue-driving test starts
// from a clean, isolated gate.
func resetRescueEpisodes(t *testing.T) {
	t.Helper()
	prev := rescueEpisodes
	rescueEpisodes = newRescueEpisodeStore(rescueEpisodeCapacity, rescueEpisodeIdleTTL)
	t.Cleanup(func() { rescueEpisodes = prev })
}

// episodeUpstream is a fake upstream whose failure schedule can be flipped mid-test. It
// counts every dispatch so a test can prove "how many upstream requests did this turn
// cause" — the observable proxy for "did a machine rescue run".
type episodeUpstream struct {
	mu       sync.Mutex
	srv      *httptest.Server
	hits     int
	failures int // answers the first N requests with 502; -1 == always fail
	marker   string
}

func newEpisodeUpstream(t *testing.T, failures int, marker string) *episodeUpstream {
	t.Helper()
	u := &episodeUpstream{failures: failures, marker: marker}
	u.srv = httptest.NewServer(http.HandlerFunc(u.serve))
	t.Cleanup(func() {
		u.srv.CloseClientConnections()
		u.srv.Close()
	})
	return u
}

func (u *episodeUpstream) serve(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	u.hits++
	n := u.hits
	fail := u.failures < 0 || n <= u.failures
	marker := u.marker
	u.mu.Unlock()

	if fail {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream boom"}}`))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = w.Write([]byte(chatContentEvent(marker)))
	_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl_ok\",\"object\":\"chat.completion.chunk\",\"created\":123,\"model\":\"upstream-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func (u *episodeUpstream) url() string { return u.srv.URL }
func (u *episodeUpstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hits
}
func (u *episodeUpstream) setFailures(n int) {
	u.mu.Lock()
	u.failures = n
	u.mu.Unlock()
}

// newEpisodeGin builds a streaming chat request carrying EXACTLY the given user text, so a
// test can tell a same-message resend from a new user message.
func newEpisodeGin(userText string, clientCtx context.Context) (*httptest.ResponseRecorder, *gin.Context) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := fmt.Sprintf(`{"model":"request-model","stream":true,"messages":[{"role":"user","content":%q}]}`, userText)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if clientCtx != nil {
		req = req.WithContext(clientCtx)
	}
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")
	return rec, c
}

type episodeResult struct {
	code    int
	body    string
	elapsed time.Duration
}

// runEpisodeRequest drives one full Handler request for the given user text and waits for
// it to finish (bounded by timeout).
func runEpisodeRequest(t *testing.T, userText string, timeout time.Duration) episodeResult {
	t.Helper()
	rec, c := newEpisodeGin(userText, nil)
	done := make(chan struct{})
	started := time.Now()
	go func() {
		Handler(inbound.InboundTypeOpenAIChat, c)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatalf("request for %q did not finish within %s", userText, timeout)
	}
	return episodeResult{code: rec.Code, body: rec.Body.String(), elapsed: time.Since(started)}
}

// startEpisodeRequest launches a Handler request in the background so a test can cancel
// the client (or poll upstream hits) while it is still running.
func startEpisodeRequest(userText string, clientCtx context.Context) (*httptest.ResponseRecorder, chan struct{}) {
	rec, c := newEpisodeGin(userText, clientCtx)
	done := make(chan struct{})
	go func() {
		Handler(inbound.InboundTypeOpenAIChat, c)
		close(done)
	}()
	return rec, done
}

func waitForHits(t *testing.T, u *episodeUpstream, atLeast int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if u.count() >= atLeast {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("upstream never reached %d dispatches within %s (got %d)", atLeast, timeout, u.count())
}

// TestEpisodeExhaustionMakesSameMessageFast: once a message has exhausted its ONE rescue,
// the next several requests carrying that SAME message must be answered fast — one normal
// forward, no rescue — instead of burning another 300s window each.
func TestEpisodeExhaustionMakesSameMessageFast(t *testing.T) {
	setupRescueDeadlineDB(t)
	withAutoRescueCap(t, 2*time.Second)

	upstream := newEpisodeUpstream(t, -1, "never") // always 502
	newNoBreakerChannel(t, upstream.url(), "always-502", 1, true)

	first := runEpisodeRequest(t, "ping", 10*time.Second)
	if first.code == http.StatusOK {
		t.Fatalf("an always-failing upstream must not end as 200, got %q", first.body)
	}
	if upstream.count() < 2 {
		t.Fatalf("the first turn must actually rescue (>=2 dispatches), got %d", upstream.count())
	}

	for i := 0; i < 5; i++ {
		before := upstream.count()
		res := runEpisodeRequest(t, "ping", 5*time.Second)
		delta := upstream.count() - before
		if res.elapsed > 1500*time.Millisecond {
			t.Fatalf("same-message request %d took %s; an exhausted episode must not rescue again", i+1, res.elapsed)
		}
		if delta > 2 {
			t.Fatalf("same-message request %d caused %d upstream dispatches; an exhausted episode forwards once", i+1, delta)
		}
	}
}

// TestEpisodeNormalSuccessRestores: a suppressed episode is re-opened by ANY full success,
// so a later failing request with the same message rescues again.
func TestEpisodeNormalSuccessRestores(t *testing.T) {
	setupRescueDeadlineDB(t)
	withAutoRescueCap(t, 2*time.Second)

	upstream := newEpisodeUpstream(t, -1, "RESTORED") // always 502 first
	newNoBreakerChannel(t, upstream.url(), "flip-flop", 1, true)

	first := runEpisodeRequest(t, "ping", 10*time.Second)
	if first.code == http.StatusOK {
		t.Fatalf("the exhausting turn unexpectedly succeeded: %q", first.body)
	}

	// Now let the upstream succeed. The suppressed request forwards once and succeeds,
	// which must restore the episode.
	upstream.setFailures(upstream.count())
	restore := runEpisodeRequest(t, "ping", 10*time.Second)
	if restore.code != http.StatusOK || !strings.Contains(restore.body, "RESTORED") {
		t.Fatalf("the suppressed request must still forward its normal candidate and succeed, got %d %q", restore.code, restore.body)
	}

	// The episode is restored: a failing same-message request rescues again.
	upstream.setFailures(-1)
	before := upstream.count()
	third := runEpisodeRequest(t, "ping", 10*time.Second)
	if upstream.count()-before < 2 {
		t.Fatalf("a restored episode must rescue again, but only %d upstream dispatch happened", upstream.count()-before)
	}
	if third.code == http.StatusOK {
		t.Fatalf("the always-failing upstream must not end as 200, got %q", third.body)
	}
}

// TestEpisodeNewUserMessageRestores: a genuinely new user message is a different episode,
// so it rescues even while the previous message is still suppressed.
func TestEpisodeNewUserMessageRestores(t *testing.T) {
	setupRescueDeadlineDB(t)
	withAutoRescueCap(t, 2*time.Second)

	upstream := newEpisodeUpstream(t, -1, "never")
	newNoBreakerChannel(t, upstream.url(), "always-502", 1, true)

	_ = runEpisodeRequest(t, "ping", 10*time.Second) // exhausts the "ping" episode

	before := upstream.count()
	next := runEpisodeRequest(t, "ping-pong", 10*time.Second)
	if upstream.count()-before < 2 {
		t.Fatalf("a new user message must start its own rescue, but only %d dispatch happened", upstream.count()-before)
	}
	if next.code == http.StatusOK {
		t.Fatalf("an always-failing upstream must not end as 200, got %q", next.body)
	}
}

// TestEpisodeZeroBudgetDisablesRescue: with the rescue budget at 0 the machine rescue is
// off entirely — one dispatch, no hold, fast return.
func TestEpisodeZeroBudgetDisablesRescue(t *testing.T) {
	setupRescueDeadlineDB(t)
	if err := op.SettingSetString(dbmodel.SettingKeyRelayNoBreakerRetryBudgetSec, "0"); err != nil {
		t.Fatalf("zero rescue budget: %v", err)
	}

	upstream := newEpisodeUpstream(t, -1, "never")
	newNoBreakerChannel(t, upstream.url(), "always-502", 1, true)

	res := runEpisodeRequest(t, "ping", 10*time.Second)
	if res.elapsed > 3*time.Second {
		t.Fatalf("budget=0 must not hold/rescue, but the request took %s", res.elapsed)
	}
	if got := upstream.count(); got != 1 {
		t.Fatalf("budget=0 must dispatch exactly once, got %d", got)
	}
	if res.code == http.StatusOK {
		t.Fatalf("an always-failing upstream must not end as 200, got %q", res.body)
	}
}

// TestEpisodeSingleChannelRescuedPastThreeFailures is the contract that replaced channel
// rotation: ONE channel that keeps failing is retried well past the old 3-failure cutoff
// and is still rescued once it finally succeeds.
func TestEpisodeSingleChannelRescuedPastThreeFailures(t *testing.T) {
	setupRescueDeadlineDB(t)
	withAutoRescueCap(t, 12*time.Second)

	// First six dispatches fail; the seventh succeeds — a single channel, no rotation.
	upstream := newEpisodeUpstream(t, 6, "SEVENTH-TRY")
	newNoBreakerChannel(t, upstream.url(), "single-channel", 1, true)

	res := runEpisodeRequest(t, "ping", 20*time.Second)
	if upstream.count() < 7 {
		t.Fatalf("the same channel must be retried past 3 failures; only %d dispatch happened", upstream.count())
	}
	if res.code != http.StatusOK || !strings.Contains(res.body, "SEVENTH-TRY") {
		t.Fatalf("the seventh dispatch should deliver content, got %d %q", res.code, res.body)
	}
}

// TestEpisodeClientCancelDoesNotSuppress: a CLIENT cancel is not an exhaustion — the
// episode must still rescue the next time the same message arrives.
func TestEpisodeClientCancelDoesNotSuppress(t *testing.T) {
	setupRescueDeadlineDB(t)
	withAutoRescueCap(t, 30*time.Second)

	upstream := newEpisodeUpstream(t, -1, "never")
	newNoBreakerChannel(t, upstream.url(), "always-502", 1, true)

	// Turn 1: start a rescue, then hang up the client beneath it.
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	_, done1 := startEpisodeRequest("ping", ctx1)
	waitForHits(t, upstream, 2, 5*time.Second)
	cancel1()
	select {
	case <-done1:
	case <-time.After(5 * time.Second):
		t.Fatalf("the handler did not exit after the client canceled")
	}

	// Turn 2: the same message must still be allowed to rescue (client cancel ≠ exhausted).
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	before := upstream.count()
	_, done2 := startEpisodeRequest("ping", ctx2)
	waitForHits(t, upstream, before+2, 5*time.Second)
	cancel2()
	select {
	case <-done2:
	case <-time.After(5 * time.Second):
		t.Fatalf("the second handler did not exit after the client canceled")
	}
}
