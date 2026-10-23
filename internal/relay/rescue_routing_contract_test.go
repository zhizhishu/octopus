package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/relay/balancer"
	"github.com/zhizhishu/octopus/internal/transformer/inbound"
	transformermodel "github.com/zhizhishu/octopus/internal/transformer/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// rescue_routing_contract_test.go pins WHERE the machine rescue may act and where it must not.
// The rescue is allowed to hold a request open and re-walk the canvas; it is never allowed to
// change the routing itself. The contracts pinned here:
//
//   - two opt-in (RaceMode) channels in ONE hard tier really run CONCURRENTLY, and the race is
//     won by the channel that produced real generated content — a contender that only returned
//     headers plus a 2xx does not win (the winner rule is content, not a status code);
//   - a lower hard tier is never started while a higher tier is still in flight;
//   - a serial (non-opt-in) channel that sits INSIDE a tier ends the grouped race right there:
//     it is not pulled into a peer's race wave, and no lower tier runs concurrently with it;
//   - a suppressed episode (the same user message already exhausted without recovering) still
//     routes and still races — the episode gate removes only the automatic RESCUE — and a turn
//     that really succeeds restores the episode so a later failure rescues again;
//   - the circuit-breaker key filter is applied per channel+key scope inside the race too,
//     except on DisableCircuitBreaker channels, which keep their key;
//   - every rescue round rebuilds the same canvas order, so the tier contract holds in EVERY
//     round and not only in the first sweep.
//
// Method: one neutral fixture per case (unique request model, one access route whose fallback
// is disabled so a failed route can never re-dispatch the same channel, one httptest upstream
// per channel recording arrival time + bearer key). Concurrency is witnessed by atomics, and
// each witness is shaped so that it cannot produce a false positive:
//
//   - rpcBarrier latches only once EVERY participant has arrived, and is asserted where the
//     dispatches MUST overlap — a serialized implementation fails instead of passing;
//   - rpcConcurrencyProbe parks each dispatch while it is counted in-flight, and is asserted
//     where the dispatches MUST NOT overlap — a serial walk never overlaps it;
//   - rpcTierWatch counts higher-tier attempts as finished BEFORE their response is handed
//     back, so the relay can never be observed "descending early" by a timing accident.
//
// Every blocking wait is bounded, so a failing case reports an assertion instead of hanging
// httptest.Server.Close.

const (
	rpcBarrierTimeout = 3 * time.Second
	rpcParkWindow     = 150 * time.Millisecond
)

// rpcBarrier is the overlap witness for dispatches that MUST be concurrent: it latches only
// after `want` participants have arrived. wait() is bounded and deliberately returns nothing —
// the assertion is on reached(), so a serialized implementation fails the test rather than
// blocking it.
type rpcBarrier struct {
	want     int32
	arrived  atomic.Int32
	done     atomic.Bool
	timedOut atomic.Bool
}

func newRPCBarrier(want int) *rpcBarrier { return &rpcBarrier{want: int32(want)} }

func (b *rpcBarrier) arrive() {
	if b.arrived.Add(1) >= b.want {
		b.done.Store(true)
	}
}

func (b *rpcBarrier) wait() {
	deadline := time.Now().Add(rpcBarrierTimeout)
	for !b.done.Load() && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	// Giving up is sticky: a straggler that arrives afterwards must not be able to make the
	// missing concurrency look observed.
	if !b.done.Load() {
		b.timedOut.Store(true)
	}
}

// reached reports that every participant arrived AND that nobody had to wait the barrier out,
// so a serialized implementation can never satisfy it.
func (b *rpcBarrier) reached() bool   { return b.done.Load() && !b.timedOut.Load() }
func (b *rpcBarrier) arrivals() int32 { return b.arrived.Load() }

// rpcConcurrencyProbe is the overlap witness for dispatches that MUST NOT be concurrent. A
// handler parks for a bounded window while its dispatch is counted in-flight, so a peer the
// relay starts at the same time registers as a genuine overlap; a serial walk cannot: the next
// attempt only starts after this one has already answered.
type rpcConcurrencyProbe struct {
	window     time.Duration
	inFlight   atomic.Int32
	peak       atomic.Int32
	overlapped atomic.Bool
}

func newRPCConcurrencyProbe(window time.Duration) *rpcConcurrencyProbe {
	return &rpcConcurrencyProbe{window: window}
}

func (p *rpcConcurrencyProbe) enter() {
	current := p.inFlight.Add(1)
	if current > 1 {
		p.overlapped.Store(true)
	}
	for {
		peak := p.peak.Load()
		if current <= peak || p.peak.CompareAndSwap(peak, current) {
			return
		}
	}
}

func (p *rpcConcurrencyProbe) park() { time.Sleep(p.window) }
func (p *rpcConcurrencyProbe) exit() { p.inFlight.Add(-1) }

func (p *rpcConcurrencyProbe) max() int32 { return p.peak.Load() }

// rpcTierWatch counts attempts of a HIGHER tier as started/finished so a lower-tier dispatch can
// prove the whole higher tier had already come back. finish() must be called BEFORE the handler
// hands its response back: the relay can only move on after it has received that response, so
// the "has it finished" observation can never race the relay and can never report a tier jump
// that did not happen.
type rpcTierWatch struct {
	started  atomic.Int32
	finished atomic.Int32
	// seq hands every event a strictly increasing number at the moment it happens, so
	// "did the lower tier arrive after the higher tier settled?" can be answered WITHOUT
	// comparing wall-clock timestamps.
	seq atomic.Int64
	mu  sync.Mutex
	// done holds the seq of every finished higher-tier attempt (not a time.Time).
	done []int64
	// lowSeq records the seq of every lower-tier dispatch.
	lowMu  sync.Mutex
	lowSeq []int64
}

func (w *rpcTierWatch) enter() { w.started.Add(1) }

func (w *rpcTierWatch) finish() {
	w.mu.Lock()
	w.done = append(w.done, w.seq.Add(1))
	w.mu.Unlock()
	w.finished.Add(1)
}

func (w *rpcTierWatch) noteLow() int64 {
	at := w.seq.Add(1)
	w.lowMu.Lock()
	w.lowSeq = append(w.lowSeq, at)
	w.lowMu.Unlock()
	return at
}

func (w *rpcTierWatch) lastLow() (int64, bool) {
	w.lowMu.Lock()
	defer w.lowMu.Unlock()
	if len(w.lowSeq) == 0 {
		return 0, false
	}
	return w.lowSeq[len(w.lowSeq)-1], true
}

func (w *rpcTierWatch) lowCount() int {
	w.lowMu.Lock()
	defer w.lowMu.Unlock()
	return len(w.lowSeq)
}

// settled reports that at least one higher-tier attempt ran and that every started attempt has
// finished — i.e. the higher tier is completely done and a lower tier may legitimately start.
func (w *rpcTierWatch) settled() bool {
	started := w.started.Load()
	return started > 0 && started == w.finished.Load()
}

func (w *rpcTierWatch) latestFinishSeq() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	var latest int64
	for _, at := range w.done {
		if at > latest {
			latest = at
		}
	}
	return latest
}

// rpcNeutralSetup initializes the shared relay test DB (which also resets the episode gate) and
// returns both the VIP route-profile id and its plan slug.
func rpcNeutralSetup(t *testing.T) (context.Context, int, string) {
	t.Helper()
	ctx := setupRelayErrorDB(t)
	plans, err := op.AccessPlanList(ctx)
	if err != nil {
		t.Fatalf("list access plans: %v", err)
	}
	for _, plan := range plans {
		if plan.Slug == "vip" && plan.RouteProfileID > 0 {
			return ctx, plan.RouteProfileID, plan.Slug
		}
	}
	t.Fatalf("vip access plan / route profile not found")
	return nil, 0, ""
}

// rpcDisableRescue turns the machine rescue off (budget 0) for the routing cases: they pin the
// normal canvas walk, and a rescue round would only add dispatches without adding evidence.
func rpcDisableRescue(t *testing.T) {
	t.Helper()
	if err := op.SettingSetString(dbmodel.SettingKeyRelayNoBreakerRetryBudgetSec, "0"); err != nil {
		t.Fatalf("disable the rescue budget: %v", err)
	}
}

func rpcBoolPtr(v bool) *bool { return &v }

// rpcChannelWithPriority creates one enabled channel with an explicit per-channel priority
// (the「渠道优先级」the balancer reads back as ChannelPriority) and its own key(s). The upstream
// model is fixed so the target also satisfies the route's availability check without a
// ModelMapping detour.
func rpcChannelWithPriority(t *testing.T, ctx context.Context, name, upstreamURL string, raceMode, noBreaker bool, channelPriority int, keyValues ...string) dbmodel.Channel {
	t.Helper()
	keys := make([]dbmodel.ChannelKey, 0, len(keyValues))
	for _, kv := range keyValues {
		keys = append(keys, dbmodel.ChannelKey{Enabled: true, ChannelKey: kv})
	}
	channel := dbmodel.Channel{
		Name:                  name,
		Type:                  outbound.OutboundTypeOpenAIChat,
		Enabled:               true,
		RaceMode:              raceMode,
		RaceKeyConcurrency:    2,
		DisableCircuitBreaker: noBreaker,
		BaseUrls:              []dbmodel.BaseUrl{{URL: upstreamURL}},
		Keys:                  keys,
		Model:                 "upstream-model",
		Priority:              channelPriority,
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel %s: %v", name, err)
	}
	balancer.ResetChannel(channel.ID)
	return channel
}

// rpcChannel is the default-priority (0, i.e. "no channel priority configured") fixture.
func rpcChannel(t *testing.T, ctx context.Context, name, upstreamURL string, raceMode, noBreaker bool, keyValues ...string) dbmodel.Channel {
	t.Helper()
	return rpcChannelWithPriority(t, ctx, name, upstreamURL, raceMode, noBreaker, 0, keyValues...)
}

// rpcRule creates the access-route rule for one case's unique request model. The mode is set
// explicitly (mode 0 would be normalized to fill-first) and the fallback is disabled so the
// pool pass can never duplicate a dispatch of a channel that just failed.
func rpcRule(t *testing.T, ctx context.Context, profileID int, model string, mode dbmodel.GroupMode) dbmodel.AccessRouteRule {
	t.Helper()
	rule := dbmodel.AccessRouteRule{
		RouteProfileID: profileID,
		RequestModel:   model,
		Mode:           mode,
		FallbackMode:   dbmodel.AccessRouteFallbackNone,
	}
	if err := op.AccessRouteRuleCreate(&rule, ctx); err != nil {
		t.Fatalf("create route rule %s: %v", model, err)
	}
	return rule
}

func rpcTarget(t *testing.T, ctx context.Context, ruleID, channelID, priority int) {
	t.Helper()
	if err := op.AccessRouteTargetCreate(&dbmodel.AccessRouteTarget{
		RouteRuleID:   ruleID,
		ChannelID:     channelID,
		UpstreamModel: "upstream-model",
		Priority:      priority,
		Weight:        1,
		Enabled:       true,
	}, ctx); err != nil {
		t.Fatalf("create route target (channel %d, priority %d): %v", channelID, priority, err)
	}
}

// rpcVerifyMode reads the EFFECTIVE mode back through the very lookup the relay performs, so a
// case can never believe it exercises one strategy while the router resolved another. The route
// path returns a mode-locked group whose Mode comes straight from the rule.
func rpcVerifyMode(t *testing.T, ctx context.Context, profileID int, model string, want dbmodel.GroupMode) {
	t.Helper()
	plans, err := op.AccessPlanList(ctx)
	if err != nil {
		t.Fatalf("list access plans: %v", err)
	}
	for i := range plans {
		if plans[i].RouteProfileID != profileID {
			continue
		}
		group, _, ok, err := op.AccessPlanGroupForModel(&plans[i], model, ctx)
		if err != nil || !ok {
			t.Fatalf("resolve route group for %s: ok=%v err=%v", model, ok, err)
		}
		if group.Mode != want {
			t.Fatalf("effective routing mode for %s = %d, want %d", model, group.Mode, want)
		}
		if !group.ModeLocked {
			t.Fatalf("a route group must be mode-locked, otherwise the fleet-wide default could override the case's mode")
		}
		return
	}
	t.Fatalf("route profile %d not found while verifying %s", profileID, model)
}

// rpcTierItem builds the candidate the balancer would see for one target: the item priority
// comes from the target, the channel priority from the channel.
func rpcTierItem(channelID, itemPriority, channelPriority int) dbmodel.GroupItem {
	return dbmodel.GroupItem{ChannelID: channelID, ModelName: "upstream-model", Priority: itemPriority, ChannelPriority: channelPriority}
}

func rpcAssertTier(t *testing.T, mode dbmodel.GroupMode, a, b dbmodel.GroupItem, wantSame bool) {
	t.Helper()
	if got := balancer.SameRoutingTier(mode, a, b); got != wantSame {
		t.Fatalf("SameRoutingTier(mode=%d, %+v, %+v) = %v, want %v", mode, a, b, got, wantSame)
	}
}

// rpcHit is one observed upstream dispatch.
type rpcHit struct {
	at    time.Time
	token string
}

type rpcUpstream struct {
	mu    sync.Mutex
	srv   *httptest.Server
	serve func(w http.ResponseWriter, r *http.Request, n int)
	hits  []rpcHit
}

func rpcNewUpstream(t *testing.T, serve func(w http.ResponseWriter, r *http.Request, n int)) *rpcUpstream {
	t.Helper()
	u := &rpcUpstream{serve: serve}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.hits = append(u.hits, rpcHit{at: time.Now(), token: rpcBearerToken(r)})
		n := len(u.hits)
		fn := u.serve
		u.mu.Unlock()
		fn(w, r, n)
	}))
	t.Cleanup(func() {
		u.srv.CloseClientConnections()
		u.srv.Close()
	})
	return u
}

func (u *rpcUpstream) url() string { return u.srv.URL }

func (u *rpcUpstream) setServe(fn func(w http.ResponseWriter, r *http.Request, n int)) {
	u.mu.Lock()
	u.serve = fn
	u.mu.Unlock()
}

func (u *rpcUpstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.hits)
}

func (u *rpcUpstream) snapshot() []rpcHit {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]rpcHit(nil), u.hits...)
}

func (u *rpcUpstream) tokens() []string {
	out := make([]string, 0)
	for _, hit := range u.snapshot() {
		out = append(out, hit.token)
	}
	return out
}

func (u *rpcUpstream) times() []time.Time {
	out := make([]time.Time, 0)
	for _, hit := range u.snapshot() {
		out = append(out, hit.at)
	}
	return out
}

func rpcBearerToken(r *http.Request) string {
	value := strings.TrimSpace(r.Header.Get("Authorization"))
	return strings.TrimSpace(strings.TrimPrefix(value, "Bearer "))
}

// rpcFailImmediately is the "this channel is broken" behaviour: an immediate upstream error.
// It takes no request, so handlers that already recorded their own bookkeeping stay readable.
func rpcFailImmediately(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadGateway)
	_, _ = w.Write([]byte(`{"error":{"message":"upstream boom"}}`))
}

// rpcStreamContent delivers REAL generated content, the only thing that may win a race.
func rpcStreamContent(w http.ResponseWriter, r *http.Request, marker string) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = w.Write([]byte(chatContentEvent(marker)))
	_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl_ok\",\"object\":\"chat.completion.chunk\",\"created\":123,\"model\":\"upstream-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func rpcLater(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// TestRescueRoutingRaceSameTierChannelsRunConcurrently: two opt-in channels in one hard tier
// must really overlap, and the winner is the one that produced real content — a channel that
// only answers headers plus a 2xx first does not win the race.
func TestRescueRoutingRaceSameTierChannelsRunConcurrently(t *testing.T) {
	ctx, profileID, _ := rpcNeutralSetup(t)
	const model = "rescue-race-tier1"
	rpcDisableRescue(t)

	barrier := newRPCBarrier(2)

	// A delivers real content; B only ever answers headers + a 2xx and then holds the
	// connection open without body. Both wait at the barrier, so neither can answer before the
	// other has been dispatched.
	a := rpcNewUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) {
		barrier.arrive()
		barrier.wait()
		rpcStreamContent(w, r, "CONTENT-WINNER")
	})
	b := rpcNewUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) {
		barrier.arrive()
		barrier.wait()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-time.After(rpcBarrierTimeout):
		}
	})

	chA := rpcChannel(t, ctx, "race1-a", a.url(), true, false, "race1-a-key")
	chB := rpcChannel(t, ctx, "race1-b", b.url(), true, false, "race1-b-key")
	rule := rpcRule(t, ctx, profileID, model, dbmodel.GroupModeSpreadTiered)
	rpcTarget(t, ctx, rule.ID, chA.ID, 1)
	rpcTarget(t, ctx, rule.ID, chB.ID, 1)
	rpcVerifyMode(t, ctx, profileID, model, dbmodel.GroupModeSpreadTiered)
	rpcAssertTier(t, dbmodel.GroupModeSpreadTiered, rpcTierItem(chA.ID, 1, 0), rpcTierItem(chB.ID, 1, 0), true)

	rec, ginCtx := newStreamGinFor(model)
	Handler(inbound.InboundTypeOpenAIChat, ginCtx)

	if !barrier.reached() {
		t.Fatalf("two same-tier opt-in channels must run concurrently, but only %d of %d dispatches arrived: the race never happened", barrier.arrivals(), 2)
	}
	if a.count() != 1 || b.count() != 1 {
		t.Fatalf("both same-tier channels must be dispatched exactly once, got a=%d b=%d", a.count(), b.count())
	}
	if body := rec.Body.String(); !strings.Contains(body, "CONTENT-WINNER") {
		t.Fatalf("the channel that produced real content must win, got status %d body %q", rec.Code, body)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("a decided race must end as 200, got %d", rec.Code)
	}
}

// TestRescueRoutingRaceHigherTierFailsThenDescendsToLowerTier: the higher tier is fully tried and
// fully finished before the lower tier is dispatched — a lower tier may never be started while a
// higher-tier attempt is still in flight.
func TestRescueRoutingRaceHigherTierFailsThenDescendsToLowerTier(t *testing.T) {
	ctx, profileID, _ := rpcNeutralSetup(t)
	const model = "rescue-race-tier2"
	rpcDisableRescue(t)

	barrier := newRPCBarrier(2)
	watch := &rpcTierWatch{}
	var violation atomic.Bool

	// Both higher-tier channels must be in flight together, then each stays busy for a moment
	// before failing. The finish is recorded BEFORE the failure is handed back, so a lower-tier
	// dispatch can prove the higher tier was already done.
	highHandler := func(w http.ResponseWriter, r *http.Request, n int) {
		watch.enter()
		barrier.arrive()
		barrier.wait()
		time.Sleep(60 * time.Millisecond)
		watch.finish()
		rpcFailImmediately(w)
	}
	highA := rpcNewUpstream(t, highHandler)
	highB := rpcNewUpstream(t, highHandler)
	low := rpcNewUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) {
		watch.noteLow()
		if !barrier.reached() || !watch.settled() {
			violation.Store(true)
		}
		rpcStreamContent(w, r, "LOW-TIER-DELIVERED")
	})

	chHighA := rpcChannel(t, ctx, "race2-high-a", highA.url(), true, false, "race2-high-a-key")
	chHighB := rpcChannel(t, ctx, "race2-high-b", highB.url(), true, false, "race2-high-b-key")
	chLow := rpcChannel(t, ctx, "race2-low", low.url(), true, false, "race2-low-key")

	rule := rpcRule(t, ctx, profileID, model, dbmodel.GroupModeSpreadTiered)
	// With the tiered strategy the LARGER item priority is tried first, so the two channels that
	// must go first share priority 2 and the lower tier sits at 1.
	rpcTarget(t, ctx, rule.ID, chHighA.ID, 2)
	rpcTarget(t, ctx, rule.ID, chHighB.ID, 2)
	rpcTarget(t, ctx, rule.ID, chLow.ID, 1)
	rpcVerifyMode(t, ctx, profileID, model, dbmodel.GroupModeSpreadTiered)
	rpcAssertTier(t, dbmodel.GroupModeSpreadTiered, rpcTierItem(chHighA.ID, 2, 0), rpcTierItem(chHighB.ID, 2, 0), true)
	rpcAssertTier(t, dbmodel.GroupModeSpreadTiered, rpcTierItem(chHighA.ID, 2, 0), rpcTierItem(chLow.ID, 1, 0), false)

	rec, ginCtx := newStreamGinFor(model)
	Handler(inbound.InboundTypeOpenAIChat, ginCtx)

	if !barrier.reached() {
		t.Fatalf("both higher-tier channels must be tried together, only %d of %d dispatches arrived", barrier.arrivals(), 2)
	}
	if violation.Load() {
		t.Fatalf("the lower tier was dispatched while the higher tier was still unfinished (tier jump)")
	}
	if highA.count() == 0 || highB.count() == 0 {
		t.Fatalf("both higher-tier channels must be attempted, got a=%d b=%d", highA.count(), highB.count())
	}
	if low.count() != 1 {
		t.Fatalf("the lower tier must be tried exactly once after the higher tier finished, got %d", low.count())
	}
	// 🔴 用**原子序号**比较先后，不用墙上时钟。
	// 原实现比较 time.Time，而 Windows 的 time.Now() 分辨率约 0.5-15ms：两次紧邻的
	// 事件会拿到**完全相同**的时间戳，`After()` 返回 false，于是一个正确的实现被判成
	// "低tier抢跑"。实测复现：-count=20 里挂 2 次，失败信息里两个时间戳连单调时钟位
	// （m=+8.150541701）都一模一样 —— 那不是顺序错，是时钟精度不够。
	lowSeq, ok := watch.lastLow()
	if !ok {
		t.Fatalf("the lower tier was never dispatched")
	}
	if highSeq := watch.latestFinishSeq(); lowSeq <= highSeq {
		t.Fatalf("the lower tier was dispatched (seq %d) before the higher tier finished (seq %d)", lowSeq, highSeq)
	}
	if body := rec.Body.String(); !strings.Contains(body, "LOW-TIER-DELIVERED") {
		t.Fatalf("the lower tier's content must reach the caller, got status %d body %q", rec.Code, body)
	}
}

// TestRescueRoutingSerialChannelEndsTheRaceTier: a serial (non-opt-in) channel inside a tier is
// not a tier boundary, but it must END the grouped race — it is never pulled into a peer's race
// wave, and the lower tier still waits for the whole higher tier.
func TestRescueRoutingSerialChannelEndsTheRaceTier(t *testing.T) {
	ctx, profileID, _ := rpcNeutralSetup(t)
	const model = "rescue-race-serial-gap"
	rpcDisableRescue(t)

	// Each handler parks while its dispatch is counted in-flight, so a concurrent dispatch shows
	// up as an overlap while a serial walk cannot produce one.
	probe := newRPCConcurrencyProbe(rpcParkWindow)
	park := func(w http.ResponseWriter, r *http.Request, fail bool) {
		probe.enter()
		probe.park()
		defer probe.exit()
		if fail {
			rpcFailImmediately(w)
			return
		}
		rpcStreamContent(w, r, "C-DELIVERED")
	}
	raise := rpcNewUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) { park(w, r, true) })
	serial := rpcNewUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) { park(w, r, true) })
	upC := rpcNewUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) { park(w, r, false) })

	// A(opt-in) and B(serial) share ONE hard tier; C is the next, lower tier.
	chA := rpcChannel(t, ctx, "race3-a", raise.url(), true, false, "race3-a-key")
	chB := rpcChannel(t, ctx, "race3-b", serial.url(), false, false, "race3-b-key")
	chC := rpcChannel(t, ctx, "race3-c", upC.url(), true, false, "race3-c-key")

	rule := rpcRule(t, ctx, profileID, model, dbmodel.GroupModeSpreadTiered)
	rpcTarget(t, ctx, rule.ID, chA.ID, 2)
	rpcTarget(t, ctx, rule.ID, chB.ID, 2)
	rpcTarget(t, ctx, rule.ID, chC.ID, 1)
	rpcVerifyMode(t, ctx, profileID, model, dbmodel.GroupModeSpreadTiered)
	rpcAssertTier(t, dbmodel.GroupModeSpreadTiered, rpcTierItem(chA.ID, 2, 0), rpcTierItem(chB.ID, 2, 0), true)
	rpcAssertTier(t, dbmodel.GroupModeSpreadTiered, rpcTierItem(chB.ID, 2, 0), rpcTierItem(chC.ID, 1, 0), false)

	rec, ginCtx := newStreamGinFor(model)
	Handler(inbound.InboundTypeOpenAIChat, ginCtx)

	if probe.overlapped.Load() {
		t.Fatalf("two channels were in flight at the same time (peak %d): a serial peer inside the tier must end the race and the lower tier must wait for the whole higher tier", probe.max())
	}
	aTimes, bTimes, cTimes := raise.times(), serial.times(), upC.times()
	if len(aTimes) != 1 || len(bTimes) != 1 || len(cTimes) != 1 {
		t.Fatalf("each candidate must be walked exactly once, got a=%d b=%d c=%d", len(aTimes), len(bTimes), len(cTimes))
	}
	if tierDone := rpcLater(aTimes[0], bTimes[0]); !tierDone.Before(cTimes[0]) {
		t.Fatalf("the lower tier must be tried only after the whole higher tier failed: tier1 latest=%s c=%s", tierDone, cTimes[0])
	}
	if body := rec.Body.String(); !strings.Contains(body, "C-DELIVERED") {
		t.Fatalf("the third candidate's content must reach the caller, got status %d body %q", rec.Code, body)
	}
}

// TestRescueRoutingSuppressedEpisodeStillRacesAndRecovers: a suppressed episode removes only the
// automatic rescue. The turn still routes and still races; a turn that really succeeds restores
// the episode, so a later failure rescues again.
func TestRescueRoutingSuppressedEpisodeStillRacesAndRecovers(t *testing.T) {
	ctx, profileID, _ := rpcNeutralSetup(t)
	const model = "rescue-race-episode"
	enableAutomaticRescue(t)
	withAutoRescueCap(t, 2*time.Second)

	exhaustBarrier := newRPCBarrier(2)
	var exhaustCount atomic.Int32
	exhaustServe := func(w http.ResponseWriter, r *http.Request, n int) {
		exhaustCount.Add(1)
		// Count before answering: the relay may only move on after this response, so the count
		// can never lag behind the dispatches the relay has already made.
		exhaustBarrier.arrive()
		exhaustBarrier.wait()
		rpcFailImmediately(w)
	}
	upA := rpcNewUpstream(t, exhaustServe)
	upB := rpcNewUpstream(t, exhaustServe)

	// No-breaker channels: this case pins the episode gate only. On an ordinary channel the
	// per-key cooldown left by the exhausting turn would throttle the next dispatch, which the
	// independent breaker cases already cover.
	chA := rpcChannel(t, ctx, "race4-a", upA.url(), true, true, "race4-a-key")
	chB := rpcChannel(t, ctx, "race4-b", upB.url(), true, true, "race4-b-key")
	rule := rpcRule(t, ctx, profileID, model, dbmodel.GroupModeSpreadTiered)
	rpcTarget(t, ctx, rule.ID, chA.ID, 1)
	rpcTarget(t, ctx, rule.ID, chB.ID, 1)
	rpcVerifyMode(t, ctx, profileID, model, dbmodel.GroupModeSpreadTiered)

	// 1) Exhaust the episode for this user message with the short cap.
	exhaust, exhaustCtx := newStreamGinFor(model)
	Handler(inbound.InboundTypeOpenAIChat, exhaustCtx)
	if !exhaustBarrier.reached() {
		t.Fatalf("the exhausting turn must still race its same-tier channels, only %d arrivals", exhaustBarrier.arrivals())
	}
	if exhaust.Code == http.StatusOK {
		t.Fatalf("an always-failing upstream must not end as 200, got %q", exhaust.Body.String())
	}
	if exhaustCount.Load() < 3 {
		t.Fatalf("the exhausting turn must actually rescue (a first sweep plus at least one rescue round), got %d dispatches", exhaustCount.Load())
	}

	// 2) Resend the SAME message while suppressed. Both peers answer only once the other is
	//    already in flight, so a serialized implementation leaves the barrier unlatched.
	restoreBarrier := newRPCBarrier(2)
	restoreServe := func(w http.ResponseWriter, r *http.Request, n int) {
		restoreBarrier.arrive()
		restoreBarrier.wait()
		rpcStreamContent(w, r, "RESTORED")
	}
	upA.setServe(restoreServe)
	upB.setServe(restoreServe)
	restore, restoreCtx := newStreamGinFor(model)
	Handler(inbound.InboundTypeOpenAIChat, restoreCtx)
	if !restoreBarrier.reached() {
		t.Fatalf("a suppressed episode must still RACE its same-tier channels, only %d arrivals: the episode gate may only remove the rescue", restoreBarrier.arrivals())
	}
	if body := restore.Body.String(); restore.Code != http.StatusOK || !strings.Contains(body, "RESTORED") {
		t.Fatalf("the suppressed turn must forward the race's content, got status %d body %q", restore.Code, body)
	}

	// 3) Fail again: the success above restored the episode, so the rescue runs again.
	finalBarrier := newRPCBarrier(2)
	finalCount := atomic.Int32{}
	finalServe := func(w http.ResponseWriter, r *http.Request, n int) {
		finalCount.Add(1)
		finalBarrier.arrive()
		finalBarrier.wait()
		rpcFailImmediately(w)
	}
	upA.setServe(finalServe)
	upB.setServe(finalServe)
	started := time.Now()
	final, finalCtx := newStreamGinFor(model)
	Handler(inbound.InboundTypeOpenAIChat, finalCtx)
	elapsed := time.Since(started)

	if !finalBarrier.reached() {
		t.Fatalf("a restored episode must still race, only %d arrivals", finalBarrier.arrivals())
	}
	if finalCount.Load() < 3 {
		t.Fatalf("a restored episode must rescue again (a first sweep plus at least one rescue round), got %d dispatches", finalCount.Load())
	}
	if elapsed > 5*time.Second {
		t.Fatalf("the restored rescue must stay inside the short 2s cap, took %s", elapsed)
	}
	if final.Code == http.StatusOK {
		t.Fatalf("an always-failing upstream must not end as 200, got %q", final.Body.String())
	}
}

// rpcTripKey trips one key in the EXACT breaker scope the relay filters with: the runtime model,
// the endpoint and the capability key derived by the same helper the relay uses. It records
// exactly the configured threshold number of failures and then self-checks that the scope really
// is open.
func rpcTripKey(t *testing.T, channelID, keyID int) {
	t.Helper()
	// A streaming chat request has capability "stream": routingCapabilityKey only adds the 1M
	// and embedding markers, neither of which this fixture has.
	capability := routingCapabilityKey(&transformermodel.InternalLLMRequest{Stream: rpcBoolPtr(true)}, nil)
	threshold, err := op.SettingGetInt(dbmodel.SettingKeyCircuitBreakerThreshold)
	if err != nil || threshold <= 0 {
		t.Fatalf("read the circuit breaker threshold: %v (value %d)", err, threshold)
	}
	for i := 0; i < threshold; i++ {
		// Status 500 is in no policy table, so no threshold FLOOR raises the bar.
		balancer.RecordFailureWithStatusScoped(channelID, keyID, "upstream-model", "chat", capability, http.StatusInternalServerError)
	}
	if tripped, _ := balancer.IsTrippedScoped(channelID, keyID, "upstream-model", "chat", capability); !tripped {
		t.Fatalf("key %d of channel %d did not trip in scope (model=upstream-model endpoint=chat capability=%q threshold=%d)",
			keyID, channelID, capability, threshold)
	}
}

// TestRescueRoutingTrippedKeyScopingInRace: the key filter is per channel+key scope inside the
// race too — a tripped key of an ordinary channel is benched while the SAME key state on a
// DisableCircuitBreaker channel still forwards.
func TestRescueRoutingTrippedKeyScopingInRace(t *testing.T) {
	ctx, profileID, _ := rpcNeutralSetup(t)
	const model = "rescue-race-breaker"
	rpcDisableRescue(t)
	if err := op.SettingSetString(dbmodel.SettingKeyCircuitBreakerThreshold, "2"); err != nil {
		t.Fatalf("set the breaker threshold: %v", err)
	}

	// Both channels answer only once the other is already in flight, so "both were dispatched"
	// is proven instead of assumed (an immediate winner may legitimately cancel its peer).
	barrier := newRPCBarrier(2)
	ordinary := rpcNewUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) {
		barrier.arrive()
		barrier.wait()
		rpcStreamContent(w, r, "ORDINARY")
	})
	noBreaker := rpcNewUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) {
		barrier.arrive()
		barrier.wait()
		rpcStreamContent(w, r, "NO-BREAKER")
	})

	chOrdinary := rpcChannel(t, ctx, "race5-ordinary", ordinary.url(), true, false, "race5-ordinary-tripped", "race5-ordinary-healthy")
	chNoBreaker := rpcChannel(t, ctx, "race5-no-breaker", noBreaker.url(), true, true, "race5-no-breaker-tripped")

	rule := rpcRule(t, ctx, profileID, model, dbmodel.GroupModeSpreadTiered)
	rpcTarget(t, ctx, rule.ID, chOrdinary.ID, 1)
	rpcTarget(t, ctx, rule.ID, chNoBreaker.ID, 1)
	rpcVerifyMode(t, ctx, profileID, model, dbmodel.GroupModeSpreadTiered)

	var ordinaryTripped, noBreakerTripped int
	for _, key := range chOrdinary.Keys {
		if key.ChannelKey == "race5-ordinary-tripped" {
			ordinaryTripped = key.ID
		}
	}
	for _, key := range chNoBreaker.Keys {
		if key.ChannelKey == "race5-no-breaker-tripped" {
			noBreakerTripped = key.ID
		}
	}
	if ordinaryTripped == 0 || noBreakerTripped == 0 {
		t.Fatalf("channel key ids were not populated: ordinary=%d no-breaker=%d", ordinaryTripped, noBreakerTripped)
	}
	if ordinaryTripped == noBreakerTripped {
		t.Fatalf("each channel owns its own key row, but both ids are %d", ordinaryTripped)
	}
	rpcTripKey(t, chOrdinary.ID, ordinaryTripped)
	rpcTripKey(t, chNoBreaker.ID, noBreakerTripped)

	rec, ginCtx := newStreamGinFor(model)
	Handler(inbound.InboundTypeOpenAIChat, ginCtx)

	if !barrier.reached() {
		t.Fatalf("both channels must still be dispatched (the tripped ordinary key must not remove its channel), only %d arrivals", barrier.arrivals())
	}
	for _, token := range ordinary.tokens() {
		if token == "race5-ordinary-tripped" {
			t.Fatalf("the tripped ordinary key must never be used, tokens=%v", ordinary.tokens())
		}
	}
	if !strings.Contains(strings.Join(noBreaker.tokens(), ","), "race5-no-breaker-tripped") {
		t.Fatalf("a tripped key on a DisableCircuitBreaker channel must still forward, tokens=%v", noBreaker.tokens())
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("the race must end as 200, got %d body %q", rec.Code, rec.Body.String())
	}
}

// TestRescueRoutingRescueRoundsPreserveTierOrder: the tier contract is not a first-sweep
// accident. Every rescue round walks the same canvas order, so the higher tier is fully settled
// before the lower tier is dispatched in EVERY round.
func TestRescueRoutingRescueRoundsPreserveTierOrder(t *testing.T) {
	ctx, profileID, _ := rpcNeutralSetup(t)
	enableAutomaticRescue(t)
	withAutoRescueCap(t, 3*time.Second)

	cases := []struct {
		name                string
		mode                dbmodel.GroupMode
		highChannelPriority int
		lowChannelPriority  int
		highItemPriority    int
		lowItemPriority     int
	}{
		// Tiered spread: the item priority is the outer boundary and the larger number goes first.
		{name: "spread-tiered", mode: dbmodel.GroupModeSpreadTiered, highItemPriority: 2, lowItemPriority: 1},
		// Round-robin: the item priority is deliberately NOT a boundary; only the channel
		// priority is. The lower tier therefore carries the LARGER item priority to prove the
		// item number is ignored.
		{name: "spread", mode: dbmodel.GroupModeSpread, lowChannelPriority: 5, highItemPriority: 1, lowItemPriority: 2},
		// Fill-first: channel priority first, item priority second.
		{name: "fill-first", mode: dbmodel.GroupModeFillFirst, highItemPriority: 1, lowItemPriority: 2},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model := "rescue-race-waves-" + tc.name

			watch := &rpcTierWatch{}
			var violation atomic.Bool
			highServe := func(w http.ResponseWriter, r *http.Request, n int) {
				watch.enter()
				// Recorded BEFORE the failure is handed back, so a lower-tier dispatch can prove
				// the higher tier had already settled.
				watch.finish()
				rpcFailImmediately(w)
			}
			highA := rpcNewUpstream(t, highServe)
			highB := rpcNewUpstream(t, highServe)
			// Fails on the first round and delivers content on the second, so the round in which
			// the tier order was re-derived is observable.
			low := rpcNewUpstream(t, func(w http.ResponseWriter, r *http.Request, n int) {
				watch.noteLow()
				// Each round must have settled its OWN two higher-tier attempts (one key each), so a
				// previous round's settled state cannot stand in for this one. No ordering within
				// the higher tier is assumed.
				if !watch.settled() || watch.finished.Load() < int32(2*n) {
					violation.Store(true)
				}
				if n >= 2 {
					rpcStreamContent(w, r, "ROUND-2-CONTENT")
					return
				}
				rpcFailImmediately(w)
			})

			// No-breaker channels so the rescue round is driven by the no-breaker budget, which is
			// the branch these channels are meant to exercise.
			chHighA := rpcChannelWithPriority(t, ctx, tc.name+"-high-a", highA.url(), true, true, tc.highChannelPriority, tc.name+"-high-a-key")
			chHighB := rpcChannelWithPriority(t, ctx, tc.name+"-high-b", highB.url(), true, true, tc.highChannelPriority, tc.name+"-high-b-key")
			chLow := rpcChannelWithPriority(t, ctx, tc.name+"-low", low.url(), true, true, tc.lowChannelPriority, tc.name+"-low-key")

			rule := rpcRule(t, ctx, profileID, model, tc.mode)
			rpcTarget(t, ctx, rule.ID, chHighA.ID, tc.highItemPriority)
			rpcTarget(t, ctx, rule.ID, chHighB.ID, tc.highItemPriority)
			rpcTarget(t, ctx, rule.ID, chLow.ID, tc.lowItemPriority)
			rpcVerifyMode(t, ctx, profileID, model, tc.mode)
			rpcAssertTier(t, tc.mode, rpcTierItem(chHighA.ID, tc.highItemPriority, tc.highChannelPriority), rpcTierItem(chHighB.ID, tc.highItemPriority, tc.highChannelPriority), true)
			rpcAssertTier(t, tc.mode, rpcTierItem(chHighA.ID, tc.highItemPriority, tc.highChannelPriority), rpcTierItem(chLow.ID, tc.lowItemPriority, tc.lowChannelPriority), false)

			rec, ginCtx := newStreamGinFor(model)
			Handler(inbound.InboundTypeOpenAIChat, ginCtx)

			if violation.Load() {
				t.Fatalf("a rescue round dispatched the lower tier while the higher tier was still unfinished: the canvas order was not rebuilt")
			}
			if n := watch.lowCount(); n < 2 {
				t.Fatalf("the rescue must run at least two rounds, the lower tier was dispatched %d time(s)", n)
			}
			// latestFinishSeq() spans every round, so only the LAST round's lower-tier dispatch can
			// be required to follow all of them. Sequence numbers, not wall clock — see the note in
			// the preceding test about Windows time.Now() resolution.
			lastLow, _ := watch.lastLow()
			if highSeq := watch.latestFinishSeq(); lastLow <= highSeq {
				t.Fatalf("the lower tier was dispatched (seq %d) before the higher tier settled (seq %d)", lastLow, highSeq)
			}
			if body := rec.Body.String(); !strings.Contains(body, "ROUND-2-CONTENT") {
				t.Fatalf("the second-round rescue must deliver content, got status %d body %q", rec.Code, body)
			}
		})
	}
}
