package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

// The "two upstream requests 1ms apart for one retry" report: measured on a real instance as
// 8 upstream requests in 4 pairs (0.001s inside a pair, ~1s between pairs) for a single client
// request whose model had exactly ONE candidate channel. The cause is not a double dispatch of
// one attempt and not a transport retry — it is the access-route pool fallback: after the route
// channel fails, the relay makes a second pass over the model pool
// (shouldReturnToOriginalGroup), and with a single-candidate pool that pass re-dispatched the
// very channel that had just failed, a millisecond later. The automatic rescue loop resets
// triedReturnGroup every round, so the pair repeated each round: two upstream executions (and
// two billed completions) per retry.
//
// These tests pin the fixed behaviour: a fallback pass may only reach channels this request has
// not tried yet, a terminal answer ends all upstream traffic, and a caller cancellation takes
// the in-flight upstream request with it.

// newRouteFallbackChannel creates a channel the access route points at and the model pool also
// resolves for requestModel — the live shape that produced the pair.
func newRouteFallbackChannel(t *testing.T, ctx context.Context, upstreamURL, name, requestModel, upstreamModel string) dbmodel.Channel {
	t.Helper()
	channel := dbmodel.Channel{
		Name:   name,
		Type:   outbound.OutboundTypeOpenAIChat,
		Enabled: true,
		// The live shape: a no-breaker channel and the rescue switch on, which is what makes
		// the automatic rescue loop (the paced retry path) actually run.
		DisableCircuitBreaker: true,
		Model:        upstreamModel,
		ModelMapping: map[string]string{requestModel: upstreamModel},
		Priority:     1,
		BaseUrls:     []dbmodel.BaseUrl{{URL: upstreamURL}},
		Keys:         []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "route-key"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel %s: %v", name, err)
	}
	return channel
}

// withAccessRouteTo points the access route at one channel, with the pool fallback enabled.
func withAccessRouteTo(t *testing.T, ctx context.Context, requestModel string, channelID int, upstreamModel string) {
	t.Helper()
	plans, err := op.AccessPlanList(ctx)
	if err != nil {
		t.Fatalf("list access plans: %v", err)
	}
	var vip dbmodel.AccessPlan
	for _, plan := range plans {
		if plan.Slug == "vip" {
			vip = plan
			break
		}
	}
	if vip.ID == 0 {
		t.Fatalf("vip plan not found")
	}
	rule := dbmodel.AccessRouteRule{
		RouteProfileID: vip.RouteProfileID,
		RequestModel:   requestModel,
		FallbackMode:   dbmodel.AccessRouteFallbackReturnGroup,
	}
	if err := op.AccessRouteRuleCreate(&rule, ctx); err != nil {
		t.Fatalf("create route rule: %v", err)
	}
	if err := op.AccessRouteTargetCreate(&dbmodel.AccessRouteTarget{
		RouteRuleID:   rule.ID,
		ChannelID:     channelID,
		UpstreamModel: upstreamModel,
		Priority:      1,
		Weight:        1,
		Enabled:       true,
	}, ctx); err != nil {
		t.Fatalf("create route target: %v", err)
	}
}

// newStreamGinFor builds a streaming chat request for an arbitrary model.
func newStreamGinFor(model string) (*httptest.ResponseRecorder, *gin.Context) {
	return newStreamGinForCtx(model, context.Background())
}

func newStreamGinForCtx(model string, ctx context.Context) (*httptest.ResponseRecorder, *gin.Context) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"`+model+`",
		"stream":true,
		"messages":[{"role":"user","content":"ping"}]
	}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")
	return rec, c
}

// countingUpstream answers every request the same way and records when each one arrived.
func countingUpstream(t *testing.T, status int, body string) (*httptest.Server, func() []time.Time) {
	t.Helper()
	var mu sync.Mutex
	var stamps []time.Time
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		stamps = append(stamps, time.Now())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(func() { server.CloseClientConnections(); server.Close() })
	return server, func() []time.Time {
		mu.Lock()
		defer mu.Unlock()
		return append([]time.Time(nil), stamps...)
	}
}

// TestRouteFallbackDoesNotReDispatchAChannelThatJustFailed is the duplicate-dispatch rule: one
// retry reaches the upstream exactly once. Before the fix this measured a second request ~1ms
// after each failure — two upstream executions per pass, repeated every rescue round.
func TestRouteFallbackDoesNotReDispatchAChannelThatJustFailed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	enableAutomaticRescue(t)
	withAutoRescueCap(t, 2500*time.Millisecond)

	const requestModel = "dup-model"
	upstream, stamps := countingUpstream(t, 529, `{"error":{"message":"overloaded","type":"overloaded_error"}}`)
	channel := newRouteFallbackChannel(t, ctx, upstream.URL, "dup-route-channel", requestModel, "dup-upstream")
	withAccessRouteTo(t, ctx, requestModel, channel.ID, "dup-upstream")

	rec, c := newStreamGinFor(requestModel)
	Handler(inbound.InboundTypeOpenAIChat, c)

	measured := stamps()
	if len(measured) == 0 {
		t.Fatalf("expected at least one upstream attempt")
	}
	sub50 := subFiftyMillisecondPairs(measured)
	t.Logf("sub-50ms pairs=%d dispatches=%d series=%v", sub50, len(measured), measured)
	if sub50 != 0 {
		t.Fatalf("sub-50ms pairs=%d (must be 0): one retry must dispatch exactly once (series=%v)", sub50, measured)
	}
	for i := 1; i < len(measured); i++ {
		if gap := measured[i].Sub(measured[i-1]); gap < 200*time.Millisecond {
			t.Fatalf("two upstream requests %s apart: one retry must dispatch exactly once (series=%v)",
				gap, measured)
		}
	}
	// The pool fallback finds no untried channel, so the count is the paced round count in the
	// rescue window — not double it.
	if len(measured) > 3 {
		t.Fatalf("expected at most 3 paced attempts in 2.5s, got %d upstream requests", len(measured))
	}
	if rec.Code == http.StatusOK && !strings.Contains(rec.Body.String(), "error") {
		t.Fatalf("an always-529 channel must not be reported as success, got %d body %s", rec.Code, rec.Body.String())
	}
}

// TestNoUpstreamRequestAfterTheTerminalAnswer pins the second half: once the caller holds a
// terminal answer, the relay must not contact the upstream again — no late spill, no rescue
// round, nothing.
func TestNoUpstreamRequestAfterTheTerminalAnswer(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	enableAutomaticRescue(t)
	withAutoRescueCap(t, 1200*time.Millisecond)

	const requestModel = "late-model"
	upstream, stamps := countingUpstream(t, 529, `{"error":{"message":"overloaded","type":"overloaded_error"}}`)
	channel := newRouteFallbackChannel(t, ctx, upstream.URL, "late-route-channel", requestModel, "late-upstream")
	withAccessRouteTo(t, ctx, requestModel, channel.ID, "late-upstream")

	rec, c := newStreamGinFor(requestModel)
	Handler(inbound.InboundTypeOpenAIChat, c)

	after := len(stamps())
	body := rec.Body.String()
	// Nothing was delivered, so a terminal JSON error is the correct answer here; what matters
	// is that the caller HAS a terminal answer before we check for a late upstream request.
	if rec.Code < 400 && !strings.Contains(body, "[DONE]") {
		t.Fatalf("the caller must end with either a terminal stream or an error, got %d body %s", rec.Code, body)
	}
	time.Sleep(1500 * time.Millisecond)
	if later := len(stamps()); later != after {
		t.Fatalf("the upstream was contacted %d more time(s) after the caller's terminal answer", later-after)
	}
}

// TestCallerCancellationStopsTheUpstreamRequest pins the third: a client that goes away takes
// the in-flight upstream request with it, and the rescue loop starts no replacement.
func TestCallerCancellationStopsTheUpstreamRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	enableAutomaticRescue(t)
	withAutoRescueCap(t, 5*time.Second)

	const requestModel = "cancel-model"
	var mu sync.Mutex
	var stamps []time.Time
	upstreamGone := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		stamps = append(stamps, time.Now())
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Never answer: hold until the caller (through the relay) goes away.
		<-r.Context().Done()
		select {
		case upstreamGone <- struct{}{}:
		default:
		}
	}))
	t.Cleanup(func() { upstream.CloseClientConnections(); upstream.Close() })

	channel := newRouteFallbackChannel(t, ctx, upstream.URL, "cancel-route-channel", requestModel, "cancel-upstream")
	withAccessRouteTo(t, ctx, requestModel, channel.ID, "cancel-upstream")

	reqCtx, cancelReq := context.WithCancel(context.Background())
	defer cancelReq()
	rec, c := newStreamGinForCtx(requestModel, reqCtx)

	done := make(chan struct{})
	go func() {
		Handler(inbound.InboundTypeOpenAIChat, c)
		close(done)
	}()
	time.AfterFunc(400*time.Millisecond, cancelReq)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("the relay must stop when the caller's context is canceled")
	}
	select {
	case <-upstreamGone:
	case <-time.After(3 * time.Second):
		t.Fatalf("the upstream request must be canceled when the caller goes away (hits=%d body=%s)",
			len(stamps), rec.Body.String())
	}

	mu.Lock()
	after := len(stamps)
	mu.Unlock()
	time.Sleep(700 * time.Millisecond)
	mu.Lock()
	later := len(stamps)
	mu.Unlock()
	if later != after {
		t.Fatalf("the rescue loop started %d more upstream request(s) after the caller canceled", later-after)
	}
}

// subFiftyMillisecondPairs is the named metric of this batch: how many pairs of consecutive
// upstream requests arrived less than 50ms apart. One retry must send exactly once, so the
// measured value has to be 0 — it is asserted, not just logged.
func subFiftyMillisecondPairs(stamps []time.Time) int {
	pairs := 0
	for i := 1; i < len(stamps); i++ {
		if stamps[i].Sub(stamps[i-1]) < 50*time.Millisecond {
			pairs++
		}
	}
	return pairs
}

// enableAutomaticRescue mirrors the instance this batch was measured on: the automatic rescue
// loop is on (it is the default in production) rather than only the operator hold being shown.
func enableAutomaticRescue(t *testing.T) {
	t.Helper()
	if err := op.SettingSetString(dbmodel.SettingKeyRelayInterventionEnabled, "true"); err != nil {
		t.Fatalf("enable automatic rescue: %v", err)
	}
}
