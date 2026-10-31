package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/price"
	"github.com/zhizhishu/octopus/internal/utils/log"
)

// /v1/realtime 会话中途结算与"用完断开"。
//
// 问题：会话只在结束时结算一次，一通长语音可以把余额花到远低于 0（UserRecordUsage 把余额截在 0，
// 超出的部分直接白送）。用户口径："用完断开"。
//
// 做法照业界：
//   - 准入先查：middleware.APIKeyAuth 的 UserCanRelay（与所有端点同一道闸）挡住余额 <= 0；
//     这里再补一道"够不够第一份"（按分钟计价时至少要够 1 秒音频），以及 key 的花费上限。
//     sub2api handler/grok_audio.go GrokRealtime 也是先 CheckBillingEligibility、再按 1 个音频单位
//     reserveInflightBalance。
//   - 中途分段扣：按 token 计价时每个 response.done 扣一次（new-api relay/channel/openai/relay_realtime.go
//     preConsumeUsage → service/quota.go PreWssConsumeQuota，余额不够就报错断开）；
//     按分钟计价时每满一个结算周期（默认 1 分钟）扣一次已经过去的时长，第一次出现音频时立刻起算。
//   - 不够下一份就断：先给客户端发一条 OpenAI 形态的 error 事件（new-api relay/helper/common.go WssError
//     的 {"type":"error","event_id":…,"error":{…}}），再用 1008 Policy Violation 关两边
//     （new-api relay/websocket.go 里 wsmanager 强制断开用的也是 ClosePolicyViolation）。
//   - 管理员不扣费（UserRecordUsage 对管理员直接返回），因此永不断开。
//   - 收尾只补差：会话结束时照旧按整段会话算总价，减去中途已经扣掉的部分再扣（rawProtocolRelayMetrics.PreCharged），
//     不会重复扣。
//
// "下一份"：按分钟计价时是最小计费粒度 1 秒音频；余额只够几秒时把下次结算挪到"刚好花完"的时刻，
// 到点结算后再断，正好用完而不是提前一分钟断。按 token 计价时一次回复的价格事先不知道，
// 用上一次回复的价格估（new-api 也是回复结束后才知道这一笔够不够）。

// quotaReservationContextKey 与 middleware.QuotaReservationContextKey 同值（grok_realtime_cutoff_test.go 里有断言）。
const quotaReservationContextKey = "quota_reservation"

// grokRealtimeBillingInterval 是按分钟计价时的中途结算周期（也是按 token 计价时复查余额的周期）。
// 定义成 var 只为测试可压低，生产路径从不写它。
var grokRealtimeBillingInterval = time.Minute

// grokRealtimeMinUnit 是按分钟计价时"下一份"的大小：剩下的钱连 1 秒音频都不够就断。
var grokRealtimeMinUnit = time.Second

const (
	grokRealtimeCutoffCodeBalance = "insufficient_quota"
	grokRealtimeCutoffCodeKeyCap  = "api_key_spend_limit_reached"

	grokRealtimeCutoffMsgBalance = "Your balance has run out, so this realtime session was ended. Top up your balance and reconnect to continue."
	grokRealtimeCutoffMsgKeyCap  = "This API key has reached its spending limit, so this realtime session was ended. Raise the key's limit or use another key to continue."
	// close 帧原因最多 123 字节，用短句。
	grokRealtimeCloseReasonBalance = "balance exhausted"
	grokRealtimeCloseReasonKeyCap  = "api key spend limit reached"

	grokRealtimeChargeEpsilon = 1e-12
)

// grokRealtimeCutoff 是"钱不够了"这一种会话终止原因。
type grokRealtimeCutoff struct {
	Code        string
	Message     string
	CloseReason string
	Detail      string // 只进日志
}

func (c *grokRealtimeCutoff) Error() string {
	if c == nil {
		return ""
	}
	return c.Code + ": " + c.Detail
}

func newGrokRealtimeBalanceCutoff(detail string) *grokRealtimeCutoff {
	return &grokRealtimeCutoff{Code: grokRealtimeCutoffCodeBalance, Message: grokRealtimeCutoffMsgBalance, CloseReason: grokRealtimeCloseReasonBalance, Detail: detail}
}

func newGrokRealtimeKeyCapCutoff(detail string) *grokRealtimeCutoff {
	return &grokRealtimeCutoff{Code: grokRealtimeCutoffCodeKeyCap, Message: grokRealtimeCutoffMsgKeyCap, CloseReason: grokRealtimeCloseReasonKeyCap, Detail: detail}
}

// grokRealtimeErrorEvent 是发给客户端的 OpenAI Realtime error 事件。
func grokRealtimeErrorEvent(cut *grokRealtimeCutoff) []byte {
	b, _ := json.Marshal(map[string]any{
		"type":     "error",
		"event_id": fmt.Sprintf("evt_octopus_%d", time.Now().UnixNano()),
		"error": map[string]any{
			"type":    grokRealtimeCutoffCodeBalance,
			"code":    cut.Code,
			"message": cut.Message,
			"param":   nil,
		},
	})
	return b
}

// grokRealtimePricing 是一次会话的计价口径（与 applyGrokRealtimeCost 同一套判断）。
type grokRealtimePricing struct {
	perMinute bool
	// perMinuteCost 已乘倍率。
	perMinuteCost float64
}

func grokRealtimePricingFor(metrics *rawProtocolRelayMetrics, actualModel string) grokRealtimePricing {
	snapshot := metrics.buildBillingSnapshot(actualModel, nil)
	modelPrice := price.GetLLMPrice(snapshot.BillingModelName)
	if modelPrice == nil || modelPrice.PerCall <= 0 {
		return grokRealtimePricing{}
	}
	multiplier := snapshot.FinalMultiplier
	if multiplier <= 0 {
		multiplier = 1
	}
	return grokRealtimePricing{perMinute: true, perMinuteCost: modelPrice.PerCall * multiplier}
}

// grokRealtimeCostSoFar 按"会话此刻就结束"算总价，与收尾的 applyGrokRealtimeCost 同口径，
// 所以中途扣掉的累计额 <= 收尾总价，收尾补差永远 >= 0。
func grokRealtimeCostSoFar(metrics *rawProtocolRelayMetrics, actualModel string, usage imagesUsage, usageSeen, audio bool, elapsed time.Duration) float64 {
	pricing := grokRealtimePricingFor(metrics, actualModel)
	if pricing.perMinute {
		minutes := grokRealtimeBillableMinutes(&grokRealtimeSession{AudioObserved: audio, Duration: elapsed})
		return pricing.perMinuteCost * minutes
	}
	if !usageSeen {
		return 0
	}
	snapshot := metrics.buildBillingSnapshot(actualModel, &usage)
	return snapshot.FinalInputCost + snapshot.FinalCacheReadCost + snapshot.FinalCacheWriteCost + snapshot.FinalOutputCost
}

// grokRealtimeBiller 负责一次会话的中途扣费与断开判定。只在 billing goroutine 里调用 settle；
// Charged 由主 goroutine 在 billing goroutine 退出后读。mu 兜住两者，-race 下也干净。
type grokRealtimeBiller struct {
	ctx         context.Context
	metrics     *rawProtocolRelayMetrics
	actualModel string
	userID      int
	apiKeyID    int
	reservation float64
	keyMaxCost  float64

	mu    sync.Mutex
	admin bool
	// guest：访客 Key（归属管理员，不扣余额），但 Key 上限照样要中途结算断开。
	guest    bool
	charged  float64
	lastUnit float64
}

// newGrokRealtimeBiller：没有用户（userID <= 0）时返回 nil，会话照旧只在结束时结算。
func newGrokRealtimeBiller(c *gin.Context, metrics *rawProtocolRelayMetrics, actualModel string) *grokRealtimeBiller {
	if metrics == nil || metrics.UserID <= 0 {
		return nil
	}
	b := &grokRealtimeBiller{
		ctx:         context.WithoutCancel(c.Request.Context()),
		metrics:     metrics,
		actualModel: actualModel,
		userID:      metrics.UserID,
		apiKeyID:    metrics.APIKeyID,
		reservation: c.GetFloat64(quotaReservationContextKey),
	}
	if user, err := op.UserGet(b.userID); err == nil && user.IsAdmin() {
		b.admin = true
	}
	b.guest = isGuestKeyRequest(c)
	if b.apiKeyID > 0 {
		if key, err := op.APIKeyGet(b.apiKeyID, b.ctx); err == nil {
			b.keyMaxCost = key.MaxCost
		}
	}
	return b
}

// Charged 是中途已经扣掉的累计额。
func (b *grokRealtimeBiller) Charged() float64 {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.charged
}

// settle 把"到此刻的总价 - 已扣"补扣掉，再判断剩下的够不够下一份。
// 返回 cutoff != nil 表示该断；否则 next 是下一次结算最晚该在多久之后。
func (b *grokRealtimeBiller) settle(usage imagesUsage, usageSeen, audio bool, elapsed time.Duration) (*grokRealtimeCutoff, time.Duration) {
	next := grokRealtimeBillingInterval
	total := grokRealtimeCostSoFar(b.metrics, b.actualModel, usage, usageSeen, audio, elapsed)

	b.mu.Lock()
	delta := total - b.charged
	admin := b.admin
	b.mu.Unlock()

	if delta > grokRealtimeChargeEpsilon && !admin {
		if err := op.UserRecordUsage(b.userID, delta, b.ctx); err != nil {
			// 没扣成就不记已扣：收尾会把它一并补上，不会漏也不会重。
			log.Warnf("realtime incremental charge failed: user=%d amount=%f err=%v", b.userID, delta, err)
		} else {
			b.mu.Lock()
			b.charged += delta
			b.lastUnit = delta
			b.mu.Unlock()
		}
	}
	if admin {
		return b.guestKeyCapCutoff(total), next
	}

	pricing := grokRealtimePricingFor(b.metrics, b.actualModel)
	// 下一份多少钱：按分钟 → 1 秒音频（还没出现音频时不计时，不会产生费用）；按 token → 上一次回复的价。
	var unit, perSecond float64
	if pricing.perMinute {
		perSecond = pricing.perMinuteCost / 60
		if audio {
			unit = perSecond * grokRealtimeMinUnit.Seconds()
		}
	} else {
		b.mu.Lock()
		unit = b.lastUnit
		b.mu.Unlock()
	}

	remaining, isAdmin, err := op.UserRelayRemaining(b.userID, b.ctx)
	if err != nil {
		// 查不到余额（缓存/库瞬时错误）不当成"用完"断人，收尾结算照旧。
		log.Warnf("realtime balance check failed: user=%d err=%v", b.userID, err)
		return nil, next
	}
	if isAdmin {
		b.mu.Lock()
		b.admin = true
		b.mu.Unlock()
		return b.guestKeyCapCutoff(total), next
	}
	// 自己在准入时占的在途预留不算别人的占用。
	remaining += b.reservation
	charged := b.Charged()
	if remaining <= grokRealtimeChargeEpsilon || remaining < unit {
		return newGrokRealtimeBalanceCutoff(fmt.Sprintf("user=%d remaining=%f next_unit=%f charged=%f", b.userID, remaining, unit, charged)), 0
	}
	limitLeft := math.Inf(1)
	if b.keyMaxCost > 0 {
		stats := op.StatsAPIKeyGet(b.apiKeyID)
		limitLeft = b.keyMaxCost - (stats.StatsMetrics.InputCost + stats.StatsMetrics.OutputCost + charged)
		if limitLeft <= grokRealtimeChargeEpsilon || limitLeft < unit {
			return newGrokRealtimeKeyCapCutoff(fmt.Sprintf("api_key=%d max_cost=%f left=%f next_unit=%f charged=%f", b.apiKeyID, b.keyMaxCost, limitLeft, unit, charged)), 0
		}
	}
	if pricing.perMinute && audio && perSecond > 0 {
		// 只够不到一个周期：把下次结算挪到刚好花完的时刻，到点扣完再断。
		left := math.Min(remaining, limitLeft)
		untilEmpty := time.Duration(left / perSecond * float64(time.Second))
		if untilEmpty < next {
			next = untilEmpty
		}
	}
	if next < grokRealtimeMinUnit {
		next = grokRealtimeMinUnit
	}
	return nil, next
}

// guestKeyCapCutoff：访客 Key（归属管理员）不看余额，只看这把 Key 的花费上限。
// sessionCost 是本次会话到此刻的总价（管理员不扣费，b.charged 恒为 0，所以用总价而不是已扣）。
// 不是访客 Key、或没设上限时返回 nil（与原来管理员「永不断开」一致）。
func (b *grokRealtimeBiller) guestKeyCapCutoff(sessionCost float64) *grokRealtimeCutoff {
	if b == nil || !b.guest || b.keyMaxCost <= 0 || b.apiKeyID <= 0 {
		return nil
	}
	stats := op.StatsAPIKeyGet(b.apiKeyID)
	left := b.keyMaxCost - (stats.StatsMetrics.InputCost + stats.StatsMetrics.OutputCost + sessionCost)
	if left <= grokRealtimeChargeEpsilon {
		return newGrokRealtimeKeyCapCutoff(fmt.Sprintf("guest api_key=%d max_cost=%f left=%f session=%f", b.apiKeyID, b.keyMaxCost, left, sessionCost))
	}
	return nil
}

// grokRealtimePreflight 是升级前的"够不够第一份"检查；返回非 nil 表示拒绝（HTTP 402）。
// 余额 <= 0 已经被 APIKeyAuth 的 UserCanRelay 挡在门外，这里同口径再查一次（测试与非中间件调用方也安全），
// 并补上按分钟计价至少要够 1 秒音频、以及 key 花费上限。
func grokRealtimePreflight(c *gin.Context, metrics *rawProtocolRelayMetrics, billingModel string) *grokRealtimeCutoff {
	b := newGrokRealtimeBiller(c, metrics, billingModel)
	if b == nil {
		return nil
	}
	if b.admin {
		return b.guestKeyCapCutoff(0)
	}
	remaining, isAdmin, err := op.UserRelayRemaining(b.userID, b.ctx)
	if err != nil || isAdmin {
		return nil
	}
	remaining += b.reservation
	var unit float64
	if pricing := grokRealtimePricingFor(metrics, billingModel); pricing.perMinute {
		unit = pricing.perMinuteCost / 60 * grokRealtimeMinUnit.Seconds()
	}
	if remaining <= grokRealtimeChargeEpsilon || remaining < unit {
		return newGrokRealtimeBalanceCutoff(fmt.Sprintf("user=%d remaining=%f first_unit=%f", b.userID, remaining, unit))
	}
	if b.keyMaxCost > 0 {
		stats := op.StatsAPIKeyGet(b.apiKeyID)
		left := b.keyMaxCost - (stats.StatsMetrics.InputCost + stats.StatsMetrics.OutputCost)
		if left <= grokRealtimeChargeEpsilon || left < unit {
			return newGrokRealtimeKeyCapCutoff(fmt.Sprintf("api_key=%d max_cost=%f left=%f first_unit=%f", b.apiKeyID, b.keyMaxCost, left, unit))
		}
	}
	return nil
}

// grokRealtimePreflightStatus 拒绝时的 HTTP 状态：余额不足与 APIKeyAuth 一致 402；key 上限与 APIKeyAuth 一致 401。
func grokRealtimePreflightStatus(cut *grokRealtimeCutoff) int {
	if cut != nil && cut.Code == grokRealtimeCutoffCodeKeyCap {
		return http.StatusUnauthorized
	}
	return http.StatusPaymentRequired
}

// grokRealtimeSendCutoff 先发 error 事件，再用 1008 关两边。调用方持有 clientWriteMu 之外的一切；
// 这里自己拿 clientWriteMu，保证不与上游→客户端的泵并发写数据帧。
func grokRealtimeSendCutoff(client, upstream *websocket.Conn, clientWriteMu *sync.Mutex, cut *grokRealtimeCutoff) {
	deadline := time.Now().Add(grokRealtimeCloseGrace)
	clientWriteMu.Lock()
	_ = client.SetWriteDeadline(deadline)
	_ = client.WriteMessage(websocket.TextMessage, grokRealtimeErrorEvent(cut))
	clientWriteMu.Unlock()
	msg := websocket.FormatCloseMessage(websocket.ClosePolicyViolation, cut.CloseReason)
	_ = client.WriteControl(websocket.CloseMessage, msg, deadline)
	_ = upstream.WriteControl(websocket.CloseMessage, msg, deadline)
}
