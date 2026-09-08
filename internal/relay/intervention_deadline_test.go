package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bestruirui/octopus/internal/db"
	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/relay/balancer"
	"github.com/bestruirui/octopus/internal/relay/intervention"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/gin-gonic/gin"
)

// setupRescueDeadlineDB initializes the DB/cache for the Handler rescue tests and
// disables the stream keepalive so a stalled upstream is only ever unblocked by the
// rescue budget / abort / client disconnect — never by downstream heartbeats.
func setupRescueDeadlineDB(t *testing.T) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	balancer.ResetRuntimeTelemetry()
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "octopus.db"), false); err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
	})
	if err := op.InitCache(); err != nil {
		t.Fatalf("init cache: %v", err)
	}
	if err := op.SettingSetString(dbmodel.SettingKeyRelayStreamKeepaliveSec, "0"); err != nil {
		t.Fatalf("disable keepalive: %v", err)
	}
}

// newRescueChannel registers a DisableCircuitBreaker channel backed by the given
// upstream. DisableCircuitBreaker routes the request onto the machine-first automatic
// rescue path (sawNoBreakerChannel).
func newRescueChannel(t *testing.T, upstream string) {
	t.Helper()
	channel := dbmodel.Channel{
		Name:                  "rescue-stall-channel",
		Type:                  outbound.OutboundTypeOpenAIChat,
		Enabled:               true,
		DisableCircuitBreaker: true,
		BaseUrls:              []dbmodel.BaseUrl{{URL: upstream}},
		Keys:                  []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "the-key"}},
		Model:                 "upstream-model",
		ModelMapping:          map[string]string{"request-model": "upstream-model"},
		Priority:              1,
	}
	if err := op.ChannelCreate(&channel, context.Background()); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	balancer.ResetChannel(channel.ID)
}

// newChatGin builds a streaming chat request against the relay and returns the gin
// context plus recorder the caller drives Handler with.
func newChatGin() (*httptest.ResponseRecorder, *gin.Context) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"request-model",
		"stream":true,
		"messages":[{"role":"user","content":"ping"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")
	return rec, c
}

// waitForPending polls the intervention registry until a held chat request appears.
func waitForPending(t *testing.T, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, snap := range intervention.List() {
			if snap.Endpoint == "chat" {
				return snap.ID
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	return ""
}

// newStallUpstream returns an upstream that 500s the first request (engaging machine
// rescue) then stalls every retry until release is closed or the request context dies.
// retryInFlight closes the moment the first retry request reaches the handler, so a test
// waits on it instead of sleep-polling a call counter.
func newStallUpstream(release chan struct{}) (*httptest.Server, <-chan struct{}) {
	var mu sync.Mutex
	calls := 0
	retryInFlight := make(chan struct{})
	var closeOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if first {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"upstream boom"}}`))
			return
		}
		closeOnce.Do(func() { close(retryInFlight) })
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	return server, retryInFlight
}

// TestHeldRequestCancelsInFlightRetryAtBudget proves the core fix: a held request whose
// fresh rescue retry stalls against the upstream is canceled by the rescue budget (NOT
// left waiting for the stalled attempt to return), the pending is removed when the
// handler exits, and the descent returns a terminal error instead of hanging.
func TestHeldRequestCancelsInFlightRetryAtBudget(t *testing.T) {
	setupRescueDeadlineDB(t)
	if err := op.SettingSetString(dbmodel.SettingKeyRelayNoBreakerRetryBudgetSec, "2"); err != nil {
		t.Fatalf("set budget: %v", err)
	}

	release := make(chan struct{})
	upstream, retryInFlight := newStallUpstream(release)
	newRescueChannel(t, upstream.URL)
	t.Cleanup(func() {
		close(release)
		upstream.CloseClientConnections()
		upstream.Close()
	})

	done := make(chan struct{})
	go func() {
		_, c := newChatGin()
		Handler(inbound.InboundTypeOpenAIChat, c)
		close(done)
	}()

	if pendingID := waitForPending(t, 2*time.Second); pendingID == "" {
		t.Fatalf("no pending intervention was registered")
	}
	<-retryInFlight // a 1s-cadence retry is now blocked against the stalled upstream

	select {
	case <-done:
		// Returned without a stalled-attempt hang — the budget canceled the in-flight retry.
	case <-time.After(6 * time.Second):
		t.Fatalf("relay did not return after rescue budget; in-flight retry was not canceled")
	}
	if got := len(intervention.List()); got != 0 {
		t.Fatalf("expected empty intervention registry after handler exit, got %d", got)
	}
}

// TestAbortDuringInFlightRetryCancelsImmediately proves that an operator abort issued
// while a rescue retry is blocked against a stalled upstream cancels the active attempt
// immediately (via Pending.CancelInternal) and removes the pending — it does not wait
// for the stalled attempt or the budget to pass.
func TestAbortDuringInFlightRetryCancelsImmediately(t *testing.T) {
	setupRescueDeadlineDB(t)
	if err := op.SettingSetString(dbmodel.SettingKeyRelayNoBreakerRetryBudgetSec, "30"); err != nil {
		t.Fatalf("set budget: %v", err)
	}

	release := make(chan struct{})
	upstream, retryInFlight := newStallUpstream(release)
	newRescueChannel(t, upstream.URL)
	t.Cleanup(func() {
		close(release)
		upstream.CloseClientConnections()
		upstream.Close()
	})

	done := make(chan struct{})
	start := time.Now()
	go func() {
		_, c := newChatGin()
		Handler(inbound.InboundTypeOpenAIChat, c)
		close(done)
	}()

	pendingID := waitForPending(t, 5*time.Second)
	if pendingID == "" {
		t.Fatalf("no pending intervention was registered")
	}
	<-retryInFlight // the abort now targets an active upstream attempt, not a wait round

	if err := intervention.Resolve(pendingID, intervention.Resolution{Action: intervention.ActionAbort}); err != nil {
		t.Fatalf("Resolve(ActionAbort) failed: %v", err)
	}

	select {
	case <-done:
		// Handler exited after the abort canceled the in-flight attempt.
	case <-time.After(3 * time.Second):
		t.Fatalf("relay did not exit promptly after abort; in-flight retry was not canceled")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("handler took too long to abort: %v", elapsed)
	}
	if got := len(intervention.List()); got != 0 {
		t.Fatalf("expected empty intervention registry after abort, got %d", got)
	}
}

// TestSuccessfulRescueCleansPending proves that when a rescue retry finally succeeds, the
// request completes with a 200 stream and the intervention pending is removed once the
// handler exits (an un-cleaned pending would strand the operator log page).
func TestSuccessfulRescueCleansPending(t *testing.T) {
	setupRescueDeadlineDB(t)
	if err := op.SettingSetString(dbmodel.SettingKeyRelayNoBreakerRetryBudgetSec, "30"); err != nil {
		t.Fatalf("set budget: %v", err)
	}

	var mu sync.Mutex
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		fail := calls == 1
		mu.Unlock()
		if fail {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"upstream boom"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl_ok\",\"object\":\"chat.completion.chunk\",\"created\":123,\"model\":\"upstream-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl_ok\",\"object\":\"chat.completion.chunk\",\"created\":123,\"model\":\"upstream-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(func() {
		upstream.CloseClientConnections()
		upstream.Close()
	})
	newRescueChannel(t, upstream.URL)

	type result struct {
		code int
		body string
	}
	resCh := make(chan result, 1)
	go func() {
		rec, c := newChatGin()
		Handler(inbound.InboundTypeOpenAIChat, c)
		resCh <- result{code: rec.Code, body: rec.Body.String()}
	}()

	if id := waitForPending(t, 5*time.Second); id == "" {
		t.Fatalf("no pending intervention was registered")
	}
	select {
	case res := <-resCh:
		if res.code != http.StatusOK || !strings.Contains(res.body, "data: [DONE]") {
			t.Fatalf("expected successful SSE stream, got status %d body %q", res.code, res.body)
		}
	case <-time.After(8 * time.Second):
		t.Fatalf("relay did not complete the successful rescue in time")
	}
	if got := len(intervention.List()); got != 0 {
		t.Fatalf("expected empty intervention registry after successful rescue, got %d", got)
	}
}

// TestTrueClientDisconnectHonored proves that when the raw client hangs up, the held
// request is torn down and its pending removed — a client disconnect must not be
// mis-labeled as an upstream/rescue failure or left waiting for the budget.
func TestTrueClientDisconnectHonored(t *testing.T) {
	setupRescueDeadlineDB(t)
	if err := op.SettingSetString(dbmodel.SettingKeyRelayNoBreakerRetryBudgetSec, "30"); err != nil {
		t.Fatalf("set budget: %v", err)
	}
	if err := op.SettingSetString(dbmodel.SettingKeyRelayInterventionEnabled, "true"); err != nil {
		t.Fatalf("enable intervention: %v", err)
	}

	release := make(chan struct{})
	upstream, retryInFlight := newStallUpstream(release)
	newRescueChannel(t, upstream.URL)
	t.Cleanup(func() {
		close(release)
		upstream.CloseClientConnections()
		upstream.Close()
	})

	clientCtx, cancelClient := context.WithCancel(context.Background())
	defer cancelClient()
	_, c := newChatGin()
	c.Request = c.Request.WithContext(clientCtx)

	done := make(chan struct{})
	go func() {
		Handler(inbound.InboundTypeOpenAIChat, c)
		close(done)
	}()

	if id := waitForPending(t, 5*time.Second); id == "" {
		t.Fatalf("no pending intervention was registered")
	}
	<-retryInFlight // a rescue retry is in flight; hang up the client beneath it

	cancelClient()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("relay did not exit after client disconnected")
	}
	if got := len(intervention.List()); got != 0 {
		t.Fatalf("expected empty intervention registry after client disconnect, got %d", got)
	}
}
