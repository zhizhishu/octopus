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

	"github.com/gin-gonic/gin"
	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/relay/balancer"
	"github.com/zhizhishu/octopus/internal/transformer/inbound"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// 渠道级硬限流闸门（balancer.AdmitChannelGate）的 relay 级验收：走真实 Handler + httptest
// 上游，不发任何真实外部请求。矩阵：
//
//	a. 同渠道两个不同模型，RPM=3：3 次成功落地后，第 4 个请求不发该渠道上游，转另一渠道；
//	   CurrentRPM 读到 3（整分钟桶、只数成功）。
//	b. 并发=2，5 个慢请求并发：httptest 观察到的最大并发 ≤2。
//	c. 全满：3s 内有空位则成功；否则 429 + code channel_rate_limited + Retry-After 头；
//	   且等待总时长≈3s 不是 N×3s。
//	d. 被跳过渠道熔断/冷却/健康状态不变。
//	e. 竞速每个 contender 都占名额。
//	f. 失败/取消后并发位释放（inFlight 回 0）。
//	g. MaxConcurrent=0 且 RPMLimit=0 行为不变。
//	h. QueueSize=0（不排队）立即 429。
//	i. 排队时长按渠道 QueueTimeoutMs。
//	j. 排队人数按渠道合计（跨模型/跨 key）。
//	k. RPM 只数成功：上游失败不消耗名额，失败后同一分钟仍能继续准入（对齐 sub2api 在
//	   Forward 成功后才计数）。
//	l. CurrentRPM 是只读读数：未限流渠道也能读到本分钟成功数，且探测不影响准入。

// gateUpstream 是带命中/在途计数的 httptest 上游，回非流式 chat.completion 成功形状；
// status != 200 时回该状态的上游错误体；slow > 0 先睡再应答（制造在途窗口/慢请求）。
type gateUpstream struct {
	*httptest.Server

	mu sync.Mutex

	hits         int
	inflight     int64
	maxInflight  int64
	slow         time.Duration
	status       int
	responseBody string
}

func (u *gateUpstream) hitCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hits
}

func (u *gateUpstream) observedMaxInflight() int64 {
	return atomic.LoadInt64(&u.maxInflight)
}

func newGateUpstream(t *testing.T, status int, slow time.Duration) *gateUpstream {
	t.Helper()
	u := &gateUpstream{slow: slow, status: status, responseBody: `{
		"id":"chatcmpl-gate-test",
		"object":"chat.completion",
		"created":123,
		"model":"gate-upstream-model",
		"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
	}`}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.hits++
		u.mu.Unlock()
		current := atomic.AddInt64(&u.inflight, 1)
		for {
			peak := atomic.LoadInt64(&u.maxInflight)
			if current <= peak || atomic.CompareAndSwapInt64(&u.maxInflight, peak, current) {
				break
			}
		}
		defer atomic.AddInt64(&u.inflight, -1)
		if u.status != http.StatusOK {
			http.Error(w, `{"error":{"message":"upstream boom"}}`, u.status)
			return
		}
		if u.slow > 0 {
			time.Sleep(u.slow)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(u.responseBody))
	}))
	t.Cleanup(u.Server.Close)
	return u
}

func setupRelayChannelGateDB(t *testing.T) context.Context {
	t.Helper()
	ctx := setupRelayKeyRetryDB(t)
	// 渠道闸门状态是进程全局（按 channelID 的 sync.Map）；同包用例都从 ID=1 造渠道，
	// 残留会让下个用例莫名其妙被拒，所以每个用例前后都清一次。
	balancer.ResetChannelGates()
	t.Cleanup(balancer.ResetChannelGates)
	return ctx
}

// occupyChannelGate 用测试直接占住一个渠道的并发位（与 Handler 共享同一进程的闸门状态）。
func occupyChannelGate(t *testing.T, channelID, maxConcurrent int) func() {
	t.Helper()
	release, reason := balancer.AdmitChannelGate(context.Background(), channelID, maxConcurrent, 0, 0, nil)
	if reason != "" {
		t.Fatalf("occupy gate of channel %d: %q", channelID, reason)
	}
	return release
}

// doGateChatRequest：非流式 chat 请求打到 Handler。返回状态码 / 响应体 / 响应头。
func doGateChatRequest(modelName string) (int, string, http.Header) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"`+modelName+`",
		"messages":[{"role":"user","content":"ping"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")
	Handler(inbound.InboundTypeOpenAIChat, c)
	return rec.Code, rec.Body.String(), rec.Header()
}

// —— a：同渠道两个不同模型，RPM=3：3 次成功落地后，第 4 个请求不发该渠道上游 ——
func TestChannelGateRPMSharedAcrossModelsOnSameChannel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayChannelGateDB(t)

	upA := newGateUpstream(t, http.StatusOK, 0)
	upB := newGateUpstream(t, http.StatusOK, 0)
	channelA := dbmodel.Channel{
		Name:     "gate-rpm-a",
		Type:     outbound.OutboundTypeOpenAIChat,
		Enabled:  true,
		Model:    "gate-model-a,gate-model-b",
		RPMLimit: 3,
		BaseUrls: []dbmodel.BaseUrl{{URL: upA.URL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "a-key"}},
		Priority: 1,
	}
	if err := op.ChannelCreate(&channelA, ctx); err != nil {
		t.Fatalf("create channel A: %v", err)
	}
	channelB := dbmodel.Channel{
		Name:     "gate-rpm-b",
		Type:     outbound.OutboundTypeOpenAIChat,
		Enabled:  true,
		Model:    "gate-model-a,gate-model-b",
		BaseUrls: []dbmodel.BaseUrl{{URL: upB.URL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "b-key"}},
		Priority: 2,
	}
	if err := op.ChannelCreate(&channelB, ctx); err != nil {
		t.Fatalf("create channel B: %v", err)
	}

	// 前 3 个请求（模型 a）：默认路由 fill_first，命中渠道 A，成功落地后把 A 的整分钟桶
	// 推到 RPM 上限 3。
	for i := 0; i < 3; i++ {
		code, body, _ := doGateChatRequest("gate-model-a")
		if code != http.StatusOK || !strings.Contains(body, `"chat.completion"`) {
			t.Fatalf("request %d must succeed via channel A, got %d: %s", i+1, code, body)
		}
	}
	if hits := upA.hitCount(); hits != 3 {
		t.Fatalf("expected 3 upstream hits on channel A, got %d", hits)
	}
	if hits := upB.hitCount(); hits != 0 {
		t.Fatalf("channel B should not be hit yet, got %d", hits)
	}
	// 当前分钟读数 = 3 次成功（整分钟桶、只数成功）。
	if got := balancer.ChannelGateCurrentRPM(channelA.ID); got != 3 {
		t.Fatalf("channel A current-minute rpm = %d, want 3 after 3 successes", got)
	}

	// 第 4 个请求（同渠道的另一个模型 b）：A 的 RPM 满了一律不发 A，转到 B。
	code, body, _ := doGateChatRequest("gate-model-b")
	if code != http.StatusOK || !strings.Contains(body, `"chat.completion"`) {
		t.Fatalf("4th request must fail over to channel B with 200, got %d: %s", code, body)
	}
	if strings.Contains(body, "channel_rate_limited") {
		t.Fatalf("another channel has room; request 4 must not be rejected, got: %s", body)
	}
	if hits := upA.hitCount(); hits != 3 {
		t.Fatalf("4th request must NOT reach channel A upstream, its hits = %d", hits)
	}
	if hits := upB.hitCount(); hits != 1 {
		t.Fatalf("4th request must be served by channel B, its hits = %d", hits)
	}
}

// —— b：并发=2，5 个慢请求并发：httptest 观察到的最大并发 ≤2 ——
func TestChannelGateConcurrencyCapHoldsUnderBurst(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayChannelGateDB(t)

	upX := newGateUpstream(t, http.StatusOK, 300*time.Millisecond)
	upY := newGateUpstream(t, http.StatusOK, 0) // 溢出渠道：不限流，接住排不上队的请求
	channelX := dbmodel.Channel{
		Name:          "gate-conc-x",
		Type:          outbound.OutboundTypeOpenAIChat,
		Enabled:       true,
		Model:         "gate-model-conc",
		MaxConcurrent: 2,
		BaseUrls:      []dbmodel.BaseUrl{{URL: upX.URL}},
		Keys:          []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "x-key"}},
		Priority:      1,
	}
	if err := op.ChannelCreate(&channelX, ctx); err != nil {
		t.Fatalf("create channel X: %v", err)
	}
	channelY := dbmodel.Channel{
		Name:     "gate-conc-y",
		Type:     outbound.OutboundTypeOpenAIChat,
		Enabled:  true,
		Model:    "gate-model-conc",
		BaseUrls: []dbmodel.BaseUrl{{URL: upY.URL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "y-key"}},
		Priority: 2,
	}
	if err := op.ChannelCreate(&channelY, ctx); err != nil {
		t.Fatalf("create channel Y: %v", err)
	}

	var wg sync.WaitGroup
	codes := make([]int, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			codes[idx], _, _ = doGateChatRequest("gate-model-conc")
		}(i)
	}
	wg.Wait()

	for i, code := range codes {
		if code != http.StatusOK {
			t.Fatalf("burst request %d must succeed (admit or spill), got %d", i+1, code)
		}
	}
	// 硬上限就是硬上限：无论路由怎么挤，渠道 X 观察到的最大在途并发都不得超过 2。
	if got := upX.observedMaxInflight(); got > 2 {
		t.Fatalf("observed upstream concurrency %d exceeds MaxConcurrent=2", got)
	}
	if got := upX.hitCount(); got < 2 {
		t.Fatalf("expected the two slots to be used on channel X, hits = %d", got)
	}
	if got := upY.hitCount(); got < 1 {
		t.Fatalf("requests beyond the cap must spill to channel Y, hits = %d", got)
	}
}

// —— c1：全满后排队（渠道配了 QueueSize>0），默认 3s 内有空位则成功 ——
func TestChannelGateRequeueSucceedsWhenSlotOpens(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayChannelGateDB(t)

	upX := newGateUpstream(t, http.StatusOK, 0)
	channelX := dbmodel.Channel{
		Name:          "gate-requeue-x",
		Type:          outbound.OutboundTypeOpenAIChat,
		Enabled:       true,
		Model:         "gate-model-requeue",
		MaxConcurrent: 1,
		// 排队是显式开关：QueueSize>0 才会在整轮全拒后统一排一次队。
		// QueueTimeoutMs=0 走默认 3s。
		QueueSize: 1,
		BaseUrls:  []dbmodel.BaseUrl{{URL: upX.URL}},
		Keys:      []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "x-key"}},
		Priority:  1,
	}
	if err := op.ChannelCreate(&channelX, ctx); err != nil {
		t.Fatalf("create channel X: %v", err)
	}

	// 占住唯一并发位，600ms 后释放——请求必须在这 3s 排队预算内等到空位并成功。
	release := occupyChannelGate(t, channelX.ID, 1)
	time.AfterFunc(600*time.Millisecond, release)

	start := time.Now()
	code, body, _ := doGateChatRequest("gate-model-requeue")
	elapsed := time.Since(start)

	if code != http.StatusOK {
		t.Fatalf("request must succeed once the slot frees within the 3s budget, got %d: %s (elapsed %v)", code, body, elapsed)
	}
	if elapsed < 500*time.Millisecond {
		t.Fatalf("request must actually wait for the freed slot, elapsed %v too short", elapsed)
	}
	if elapsed >= balancer.ChannelGateQueueTimeoutDefault {
		t.Fatalf("a slot freed at 600ms; the request must not burn the whole 3s default budget, elapsed %v", elapsed)
	}
	if hits := upX.hitCount(); hits != 1 {
		t.Fatalf("expected exactly one real upstream attempt after the requeue, hits = %d", hits)
	}
}

// —— c2：全满且 3s 内无空位——429 + code channel_rate_limited + Retry-After 头；
// 等待总时长≈该渠道的 QueueTimeoutMs（单次排队，双 key 渠道也绝不 N×预算）——
func TestChannelGateAllFullReturns429AfterSingleQueue(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayChannelGateDB(t)

	upX := newGateUpstream(t, http.StatusOK, 0)
	channelX := dbmodel.Channel{
		Name:          "gate-full-x",
		Type:          outbound.OutboundTypeOpenAIChat,
		Enabled:       true,
		Model:         "gate-model-full",
		MaxConcurrent: 1,
		QueueSize:     1,
		BaseUrls:      []dbmodel.BaseUrl{{URL: upX.URL}},
		Keys: []dbmodel.ChannelKey{
			{Enabled: true, ChannelKey: "x-key-1"},
			{Enabled: true, ChannelKey: "x-key-2"},
		},
		Priority: 1,
	}
	if err := op.ChannelCreate(&channelX, ctx); err != nil {
		t.Fatalf("create channel X: %v", err)
	}

	// 占住唯一并发位并持有超过 3s：请求必须一次性排 3s 后放弃，而不是每个候选各等 3s
	//（双 key 渠道的旧语义会 ≥6s，那正是要修错的地方）。
	release := occupyChannelGate(t, channelX.ID, 1)
	defer time.AfterFunc(6*time.Second, release)

	var wg sync.WaitGroup
	var (
		elapsed time.Duration
		code    int
		body    string
		header  http.Header
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		start := time.Now()
		code, body, header = doGateChatRequest("gate-model-full")
		elapsed = time.Since(start)
	}()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatalf("all-full request must return after the single 3s queue, not hang")
	}

	if code != http.StatusTooManyRequests {
		t.Fatalf("all-full exhaustion must return 429, got %d: %s", code, body)
	}
	if !strings.Contains(body, "channel_rate_limited") {
		t.Fatalf("429 body must carry code channel_rate_limited, got: %s", body)
	}
	if got := header.Get("Retry-After"); got != "1" {
		t.Fatalf("429 must carry Retry-After: 1, got %q", got)
	}
	if elapsed < 2800*time.Millisecond {
		t.Fatalf("the queue must last ≈3s, elapsed %v too short", elapsed)
	}
	if elapsed > 4800*time.Millisecond {
		t.Fatalf("the queue must be ONE 3s budget (never per-candidate waits), elapsed %v", elapsed)
	}
	if hits := upX.hitCount(); hits != 0 {
		t.Fatalf("no request may reach the full channel's upstream, hits = %d", hits)
	}
}

// —— d：被跳过渠道熔断/冷却/健康状态不变 ——
func TestChannelGateSkipsLeaveBreakerAndCooldownUntouched(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayChannelGateDB(t)
	// 熔断阈值 1：任何一次「真实失败」都会立刻烧断——被跳过（skipped）若被错误计成失败，
	// 下面的渠道 X 早就烧断、第二个请求会被 503 circuit_open 挡回。
	if err := op.SettingSetString(dbmodel.SettingKeyCircuitBreakerThreshold, "1"); err != nil {
		t.Fatalf("set circuit threshold: %v", err)
	}

	upX := newGateUpstream(t, http.StatusOK, 0)
	upY := newGateUpstream(t, http.StatusOK, 0)
	channelX := dbmodel.Channel{
		Name:          "gate-skip-x",
		Type:          outbound.OutboundTypeOpenAIChat,
		Enabled:       true,
		Model:         "gate-model-skip",
		MaxConcurrent: 1,
		BaseUrls:      []dbmodel.BaseUrl{{URL: upX.URL}},
		Keys:          []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "x-key"}},
		Priority:      1,
	}
	if err := op.ChannelCreate(&channelX, ctx); err != nil {
		t.Fatalf("create channel X: %v", err)
	}
	channelY := dbmodel.Channel{
		Name:     "gate-skip-y",
		Type:     outbound.OutboundTypeOpenAIChat,
		Enabled:  true,
		Model:    "gate-model-skip",
		BaseUrls: []dbmodel.BaseUrl{{URL: upY.URL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "y-key"}},
		Priority: 2,
	}
	if err := op.ChannelCreate(&channelY, ctx); err != nil {
		t.Fatalf("create channel Y: %v", err)
	}

	// 占住 X 的唯一并发位：请求 1 在 X 上被跳过（skipped），落到 Y。
	release := occupyChannelGate(t, channelX.ID, 1)
	code, body, _ := doGateChatRequest("gate-model-skip")
	if code != http.StatusOK {
		t.Fatalf("request 1 must be served by channel Y, got %d: %s", code, body)
	}
	if hits := upX.hitCount(); hits != 0 {
		t.Fatalf("request 1 must not reach channel X upstream, hits = %d", hits)
	}
	if hits := upY.hitCount(); hits != 1 {
		t.Fatalf("request 1 must be served by channel Y, hits = %d", hits)
	}

	// 跳过不留痕：X 没有熔断/冷却/在途记账（0 次尝试、0 在途、没烧断、健康键数不变）。
	keys, err := op.ChannelGet(channelX.ID, ctx)
	if err != nil {
		t.Fatalf("get channel X: %v", err)
	}
	snap := balancer.SnapshotRoutingRuntime(channelX.ID, "gate-model-skip", keys.Keys, false)
	if snap.RecentRequestCount != 0 {
		t.Fatalf("a gate skip must not consume RPM budget, recent = %d", snap.RecentRequestCount)
	}
	if snap.InFlight != 0 {
		t.Fatalf("a gate skip must not leave telemetry in-flight, got %d", snap.InFlight)
	}
	if snap.CircuitTripped {
		t.Fatalf("a gate skip must not trip the circuit breaker")
	}
	if snap.HealthyKeyCount <= 0 {
		t.Fatalf("gate skip must not bench the key, healthy = %d", snap.HealthyKeyCount)
	}

	// 释放后，下一个请求应原样用 X（没有熔断/冷却污染），X 正常命中。
	release()
	code, body, _ = doGateChatRequest("gate-model-skip")
	if code != http.StatusOK {
		t.Fatalf("request 2 must take channel X again (no breaker/cooldown pollution), got %d: %s", code, body)
	}
	if hits := upX.hitCount(); hits != 1 {
		t.Fatalf("request 2 must reach channel X upstream, hits = %d", hits)
	}
}

// —— e：竞速每个 contender 都占名额 ——
func TestChannelGateRaceEachContenderTakesItsSlot(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayChannelGateDB(t)

	upX := newGateUpstream(t, http.StatusOK, 250*time.Millisecond)
	channelX := dbmodel.Channel{
		Name:          "gate-race-x",
		Type:          outbound.OutboundTypeOpenAIChat,
		Enabled:       true,
		Model:         "gate-model-race",
		RaceMode:      true,
		MaxConcurrent: 2, // 两个 contender + 每人一个名额 = 正好到顶
		BaseUrls:      []dbmodel.BaseUrl{{URL: upX.URL}},
		Keys: []dbmodel.ChannelKey{
			{Enabled: true, ChannelKey: "race-key-1"},
			{Enabled: true, ChannelKey: "race-key-2"},
		},
		Priority: 1,
	}
	if err := op.ChannelCreate(&channelX, ctx); err != nil {
		t.Fatalf("create channel X: %v", err)
	}

	code, body, _ := doGateChatRequest("gate-model-race")
	if code != http.StatusOK {
		t.Fatalf("race request must succeed, got %d: %s", code, body)
	}
	// 竞速波里的两个 contender 各自占了一个并发位（并发位必须都被真实占住），两个都
	// 同时打到了上游——哪怕输家随后被 raced_out 取消，位也确实占过。任何一个 contender
	// 被「整渠道只算一个名额」之类的方式挡住，这里都会翻车。
	if hits := upX.hitCount(); hits != 2 {
		t.Fatalf("both race contenders must occupy a slot and hit the upstream, hits = %d", hits)
	}
	if got := upX.observedMaxInflight(); got != 2 {
		t.Fatalf("both contenders must hold their slots concurrently, maxInflight = %d", got)
	}
}

// —— f：失败/取消后并发位释放（inFlight 回 0）——
func TestChannelGateSlotReleasedAfterFailureAndCancel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayChannelGateDB(t)

	// 失败释放：X（并发=1，上游 500）失败后失败转移，闸门位必须归还——下个请求还能占 X。
	upXFail := newGateUpstream(t, http.StatusInternalServerError, 0)
	upY := newGateUpstream(t, http.StatusOK, 0)
	channelX := dbmodel.Channel{
		Name:          "gate-release-x",
		Type:          outbound.OutboundTypeOpenAIChat,
		Enabled:       true,
		Model:         "gate-model-release",
		MaxConcurrent: 1,
		BaseUrls:      []dbmodel.BaseUrl{{URL: upXFail.URL}},
		Keys:          []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "x-key"}},
		Priority:      1,
	}
	if err := op.ChannelCreate(&channelX, ctx); err != nil {
		t.Fatalf("create channel X: %v", err)
	}
	channelY := dbmodel.Channel{
		Name:     "gate-release-y",
		Type:     outbound.OutboundTypeOpenAIChat,
		Enabled:  true,
		Model:    "gate-model-release",
		BaseUrls: []dbmodel.BaseUrl{{URL: upY.URL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "y-key"}},
		Priority: 2,
	}
	if err := op.ChannelCreate(&channelY, ctx); err != nil {
		t.Fatalf("create channel Y: %v", err)
	}

	code, body, _ := doGateChatRequest("gate-model-release")
	if code != http.StatusOK {
		t.Fatalf("request 1 should fail over to Y with 200, got %d: %s", code, body)
	}
	if hits := upXFail.hitCount(); hits != 1 {
		t.Fatalf("request 1 should hit the failing upstream X once, got %d", hits)
	}
	if !balancer.ChannelGateHasRoom(channelX.ID, 1, 0) {
		t.Fatalf("after the failed attempt returns, X's concurrency slot must be released (inFlight back to 0)")
	}
	code, _, _ = doGateChatRequest("gate-model-release")
	if code != http.StatusOK {
		t.Fatalf("request 2 should again enter channel X (slot released after failure), got %d", code)
	}
	if hits := upXFail.hitCount(); hits != 2 {
		t.Fatalf("request 2 must be admitted into channel X once more, hits = %d", hits)
	}
	if !balancer.ChannelGateHasRoom(channelX.ID, 1, 0) {
		t.Fatalf("after request 2 returns, X's slot must be released again")
	}
}

// TestChannelGateSlotReleasedAfterClientCancel：客户端中途取消后，闸门位必须归还。
func TestChannelGateSlotReleasedAfterClientCancel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayChannelGateDB(t)

	upX := newGateUpstream(t, http.StatusOK, 5*time.Second) // 慢上游：取消发生在它应答之前
	channelX := dbmodel.Channel{
		Name:          "gate-cancel-x",
		Type:          outbound.OutboundTypeOpenAIChat,
		Enabled:       true,
		Model:         "gate-model-cancel",
		MaxConcurrent: 1,
		BaseUrls:      []dbmodel.BaseUrl{{URL: upX.URL}},
		Keys:          []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "x-key"}},
		Priority:      1,
	}
	if err := op.ChannelCreate(&channelX, ctx); err != nil {
		t.Fatalf("create channel X: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"gate-model-cancel",
		"messages":[{"role":"user","content":"ping"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	reqCtx, cancel := context.WithCancel(req.Context())
	defer cancel()
	time.AfterFunc(400*time.Millisecond, cancel)
	req = req.WithContext(reqCtx)
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	done := make(chan struct{})
	go func() {
		Handler(inbound.InboundTypeOpenAIChat, c)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("canceled request must end quickly, Handler did not return")
	}
	if hits := upX.hitCount(); hits != 1 {
		t.Fatalf("the canceled attempt must still have reached the upstream, hits = %d", hits)
	}
	if !balancer.ChannelGateHasRoom(channelX.ID, 1, 0) {
		t.Fatalf("after the client canceled mid-flight, the concurrency slot must be released (inFlight back to 0)")
	}
}

// —— g：MaxConcurrent=0 且 RPMLimit=0 行为不变 ——
func TestChannelGateUnlimitedChannelBehaviorUnchanged(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayChannelGateDB(t)

	upX := newGateUpstream(t, http.StatusOK, 0)
	channelX := dbmodel.Channel{
		Name:     "gate-plain-x",
		Type:     outbound.OutboundTypeOpenAIChat,
		Enabled:  true,
		Model:    "gate-model-plain",
		BaseUrls: []dbmodel.BaseUrl{{URL: upX.URL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "x-key"}},
		Priority: 1,
	}
	if err := op.ChannelCreate(&channelX, ctx); err != nil {
		t.Fatalf("create channel X: %v", err)
	}
	if !balancer.ChannelGateHasRoom(channelX.ID, 0, 0) {
		t.Fatalf("an unlimited channel must always report room")
	}

	// 顺序 6 发 + 并发 5 发：没有任何上限时必须全部照常通过（若闸门错误接管了不限渠道，
	// 这批请求里至少会出现 429 channel_rate_limited）。
	for i := 0; i < 6; i++ {
		code, body, _ := doGateChatRequest("gate-model-plain")
		if code != http.StatusOK || strings.Contains(body, "channel_rate_limited") {
			t.Fatalf("sequential request %d must succeed as before, got %d: %s", i+1, code, body)
		}
	}
	var wg sync.WaitGroup
	codes := make([]int, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			codes[idx], _, _ = doGateChatRequest("gate-model-plain")
		}(i)
	}
	wg.Wait()
	for i, code := range codes {
		if code != http.StatusOK {
			t.Fatalf("burst request %d must succeed on an unlimited channel, got %d", i+1, code)
		}
	}
	if hits := upX.hitCount(); hits != 11 {
		t.Fatalf("unlimited channel must carry every request, upstream hits = %d", hits)
	}
}

// —— h：QueueSize=0（不排队）立即 429，不吃默认 3s ——
func TestChannelGateQueueDisabledReturns429WithoutWaiting(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayChannelGateDB(t)

	upX := newGateUpstream(t, http.StatusOK, 0)
	channelX := dbmodel.Channel{
		Name:          "gate-noqueue-x",
		Type:          outbound.OutboundTypeOpenAIChat,
		Enabled:       true,
		Model:         "gate-model-noqueue",
		MaxConcurrent: 1,
		// 刻意不配 QueueSize：0 = 不排队。
		BaseUrls: []dbmodel.BaseUrl{{URL: upX.URL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "x-key"}},
		Priority: 1,
	}
	if err := op.ChannelCreate(&channelX, ctx); err != nil {
		t.Fatalf("create channel X: %v", err)
	}

	release := occupyChannelGate(t, channelX.ID, 1)
	defer release()

	start := time.Now()
	code, body, _ := doGateChatRequest("gate-model-noqueue")
	elapsed := time.Since(start)

	if code != http.StatusTooManyRequests {
		t.Fatalf("a full channel without a queue must answer 429 at once, got %d: %s", code, body)
	}
	if !strings.Contains(body, "channel_rate_limited") {
		t.Fatalf("429 body must carry code channel_rate_limited, got: %s", body)
	}
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("QueueSize=0 must not wait at all, elapsed %v", elapsed)
	}
	if hits := upX.hitCount(); hits != 0 {
		t.Fatalf("no request may reach the full channel's upstream, hits = %d", hits)
	}
}

// —— i：排队时长按渠道的 QueueTimeoutMs 走（0 才是默认 3s）——
func TestChannelGateQueueTimeoutFollowsChannelConfig(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayChannelGateDB(t)

	upX := newGateUpstream(t, http.StatusOK, 0)
	channelX := dbmodel.Channel{
		Name:           "gate-queue-timeout-x",
		Type:           outbound.OutboundTypeOpenAIChat,
		Enabled:        true,
		Model:          "gate-model-queue-timeout",
		MaxConcurrent:  1,
		QueueSize:      1,
		QueueTimeoutMs: 800,
		BaseUrls:       []dbmodel.BaseUrl{{URL: upX.URL}},
		Keys:           []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "x-key"}},
		Priority:       1,
	}
	if err := op.ChannelCreate(&channelX, ctx); err != nil {
		t.Fatalf("create channel X: %v", err)
	}

	release := occupyChannelGate(t, channelX.ID, 1)
	defer release()

	start := time.Now()
	code, body, _ := doGateChatRequest("gate-model-queue-timeout")
	elapsed := time.Since(start)

	if code != http.StatusTooManyRequests {
		t.Fatalf("the slot never frees, the queued request must end with 429, got %d: %s", code, body)
	}
	if !strings.Contains(body, "channel_rate_limited") {
		t.Fatalf("429 body must carry code channel_rate_limited, got: %s", body)
	}
	if elapsed < 700*time.Millisecond {
		t.Fatalf("the queue must last the configured 800ms, elapsed %v too short", elapsed)
	}
	if elapsed > 2500*time.Millisecond {
		t.Fatalf("the queue must honour the configured 800ms (not the 3s default), elapsed %v", elapsed)
	}
	if hits := upX.hitCount(); hits != 0 {
		t.Fatalf("no request may reach the full channel's upstream, hits = %d", hits)
	}
}

// —— j：排队人数按渠道合计（跨模型/跨 key）：队满时后来的请求立即拒 ——
func TestChannelGateQueueSharedAcrossModelsAndKeys(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayChannelGateDB(t)

	upX := newGateUpstream(t, http.StatusOK, 0)
	channelX := dbmodel.Channel{
		Name:           "gate-queue-shared-x",
		Type:           outbound.OutboundTypeOpenAIChat,
		Enabled:        true,
		Model:          "gate-model-q-a,gate-model-q-b",
		MaxConcurrent:  1,
		QueueSize:      1,
		QueueTimeoutMs: 1500,
		BaseUrls:       []dbmodel.BaseUrl{{URL: upX.URL}},
		Keys: []dbmodel.ChannelKey{
			{Enabled: true, ChannelKey: "x-key-1"},
			{Enabled: true, ChannelKey: "x-key-2"},
		},
		Priority: 1,
	}
	if err := op.ChannelCreate(&channelX, ctx); err != nil {
		t.Fatalf("create channel X: %v", err)
	}

	release := occupyChannelGate(t, channelX.ID, 1)

	// 请求 1 在队里（渠道并发=1 被占满，QueueSize=1 让它排上）。
	type gateResult struct {
		code    int
		body    string
		elapsed time.Duration
	}
	firstDone := make(chan gateResult, 1)
	go func() {
		start := time.Now()
		code, body, _ := doGateChatRequest("gate-model-q-a")
		firstDone <- gateResult{code: code, body: body, elapsed: time.Since(start)}
	}()
	// 排队是进程内状态，用一小段时间把「请求 1 已经排上队」等出来；请求 1 自己的超时是
	// 1500ms，300ms 余量远大于它的入队耗时。
	time.Sleep(300 * time.Millisecond)

	// 请求 2 走同渠道的另一个模型（另一个 key）：排队人数按渠道合计，队里已有 1 个
	//（QueueSize=1），所以它必须立刻被拒，而不是也排 1500ms。
	start := time.Now()
	code2, body2, _ := doGateChatRequest("gate-model-q-b")
	secondElapsed := time.Since(start)
	if code2 != http.StatusTooManyRequests {
		t.Fatalf("a full per-channel queue must reject the second model/key at once, got %d: %s", code2, body2)
	}
	if !strings.Contains(body2, "channel_rate_limited") {
		t.Fatalf("429 body must carry code channel_rate_limited, got: %s", body2)
	}
	if secondElapsed > 700*time.Millisecond {
		t.Fatalf("the second request must not queue behind the first (队列按渠道合计), elapsed %v", secondElapsed)
	}

	// 空出一个并发位：队里的请求 1 必须被放行。
	release()
	select {
	case got := <-firstDone:
		if got.code != http.StatusOK || !strings.Contains(got.body, `"chat.completion"`) {
			t.Fatalf("the queued request must succeed once a slot frees, got %d: %s", got.code, got.body)
		}
		if got.elapsed < 250*time.Millisecond {
			t.Fatalf("the queued request must actually have waited for the freed slot, elapsed %v", got.elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the queued request never returned")
	}
	if hits := upX.hitCount(); hits != 1 {
		t.Fatalf("only the queued request may reach the upstream, hits = %d", hits)
	}
}

// —— k：RPM 只数成功——上游失败不消耗名额，同一分钟内仍可继续准入 ——
func TestChannelGateRPMCountsOnlySuccessesAcrossFailures(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayChannelGateDB(t)

	upFail := newGateUpstream(t, http.StatusInternalServerError, 0)
	upOK := newGateUpstream(t, http.StatusOK, 0)
	channelX := dbmodel.Channel{
		Name:     "gate-rpm-success-x",
		Type:     outbound.OutboundTypeOpenAIChat,
		Enabled:  true,
		Model:    "gate-model-rpm-success",
		RPMLimit: 1,
		BaseUrls: []dbmodel.BaseUrl{{URL: upFail.URL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "x-key"}},
		Priority: 1,
	}
	if err := op.ChannelCreate(&channelX, ctx); err != nil {
		t.Fatalf("create channel X: %v", err)
	}
	channelY := dbmodel.Channel{
		Name:     "gate-rpm-success-y",
		Type:     outbound.OutboundTypeOpenAIChat,
		Enabled:  true,
		Model:    "gate-model-rpm-success",
		BaseUrls: []dbmodel.BaseUrl{{URL: upOK.URL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "y-key"}},
		Priority: 2,
	}
	if err := op.ChannelCreate(&channelY, ctx); err != nil {
		t.Fatalf("create channel Y: %v", err)
	}

	// 请求 1：X 被准入（本分钟还是 0 次成功），上游 500 → 失败转移，落到 Y 成功。
	code, body, _ := doGateChatRequest("gate-model-rpm-success")
	if code != http.StatusOK {
		t.Fatalf("request 1 must fail over to channel Y with 200, got %d: %s", code, body)
	}
	if hits := upFail.hitCount(); hits != 1 {
		t.Fatalf("request 1 should hit the failing upstream X once, got %d", hits)
	}
	if hits := upOK.hitCount(); hits != 1 {
		t.Fatalf("request 1 must be served by channel Y, hits = %d", hits)
	}
	// 失败的尝试不计入 X 的本分钟成功数（对齐 sub2api：Forward 成功之后才计数）。
	if got := balancer.ChannelGateCurrentRPM(channelX.ID); got != 0 {
		t.Fatalf("a failed upstream attempt must not count towards rpm, current rpm = %d", got)
	}

	// 请求 2：X 的名额没被失败吃掉，同一分钟内仍会被准入（并发位也早已归还）。
	code, body, _ = doGateChatRequest("gate-model-rpm-success")
	if code != http.StatusOK {
		t.Fatalf("request 2 must be served (fail over to Y), got %d: %s", code, body)
	}
	if hits := upFail.hitCount(); hits != 2 {
		t.Fatalf("request 2 must be admitted into channel X again (failures do not spend rpm), hits = %d", hits)
	}
	if got := balancer.ChannelGateCurrentRPM(channelX.ID); got != 0 {
		t.Fatalf("channel X still has no success this minute, current rpm = %d", got)
	}
}

// —— l：CurrentRPM 是只读读数——不限流渠道也记本分钟成功数，探测不影响准入 ——
func TestChannelGateCurrentRPMReflectsSuccessesOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayChannelGateDB(t)

	upX := newGateUpstream(t, http.StatusOK, 0)
	channelX := dbmodel.Channel{
		Name:     "gate-current-rpm-x",
		Type:     outbound.OutboundTypeOpenAIChat,
		Enabled:  true,
		Model:    "gate-model-current-rpm",
		BaseUrls: []dbmodel.BaseUrl{{URL: upX.URL}},
		Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "x-key"}},
		Priority: 1,
	}
	if err := op.ChannelCreate(&channelX, ctx); err != nil {
		t.Fatalf("create channel X: %v", err)
	}

	// 没有任何成功之前读数是 0（渠道连闸门状态都还没有）。
	if got := balancer.ChannelGateCurrentRPM(channelX.ID); got != 0 {
		t.Fatalf("current-minute rpm before any traffic = %d, want 0", got)
	}
	for i := 0; i < 4; i++ {
		code, body, _ := doGateChatRequest("gate-model-current-rpm")
		if code != http.StatusOK {
			t.Fatalf("request %d must succeed on the unlimited channel, got %d: %s", i+1, code, body)
		}
	}
	if got := balancer.ChannelGateCurrentRPM(channelX.ID); got != 4 {
		t.Fatalf("current-minute rpm = %d, want 4 (every success counts, even without an rpm cap)", got)
	}
	// 探测不占位、不消耗名额：下一个请求照旧命中 X 并让读数继续涨。
	code, body, _ := doGateChatRequest("gate-model-current-rpm")
	if code != http.StatusOK {
		t.Fatalf("request after read-only probes must succeed, got %d: %s", code, body)
	}
	if hits := upX.hitCount(); hits != 5 {
		t.Fatalf("read-only rpm probes must not consume slots, upstream hits = %d", hits)
	}
	if got := balancer.ChannelGateCurrentRPM(channelX.ID); got != 5 {
		t.Fatalf("current-minute rpm = %d, want 5", got)
	}
}
