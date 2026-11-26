package balancer

import (
	"context"
	"math/rand"
	"strings"
	"time"
)

// ── 熔断探测（breaker probe）· 调度方 ──────────────────────────────────────
//
// A 侧（internal/modeltest）只回答"这一次探测对不对、算不算渠道故障"；本包回答
// "什么时候探谁、探完怎么改熔断状态"。两侧只用纯数据结构对接：本包不 import
// modeltest（modeltest 已经 import relay，反向即成环），接线层负责把
// modeltest.CircuitProbeResult 搬成 CircuitProbeReport、把 CircuitProbeClaim
// 翻成 RunCircuitProbe 的实参。
//
// 对外可见的硬口径（改动前先想清楚）：
//
//   - 只探"真的处于 OPEN（或被人为 hold 的、确切的 channel/keyID>0/model）条目"。
//     半开和已过冷却的条目都不归这里管：这里不臆造半开，也不抢真实流量的半开探测。
//   - 作用域必须是"已知的普通文本端点"。图片/视频/音频端点或能力位一律不探；
//     没有作用域（老口径的 channel:key:model）或作用域不认识，同样不探——那等于
//     在不知道形状的情况下往上游发一道算术题。
//   - 嵌入端点默认不探，只有调用方显式 opt-in 才探。
//   - 同一段冷却（generation）只探一次，而且只在冷却尾段排：剩余 > 60s 不算到期、
//     不可领（也不写任何持久的下次时间）；剩余进入 [10s, 60s] 后**只挑一次**提前量
//     lead ∈ [10s, min(60s, remaining)]（均匀分布），due = expiry - lead，必须落在
//     到期之前（剩余恰好 10s 时 due = 现在）；剩余 < 10s 不再新排，但已经挑好的 due
//     只要还没过到期，晚到的 tick/claim 仍然可以领。到期之后一律不再开跑。
//   - 真实 BeginRuntimeAttempt 流量会让该 tuple 跳过（10 分钟）并清零连续未恢复计数。
//     探测本身不算真实流量（探测不走 BeginRuntimeAttempt）。
//   - 探测失败只做**探测专用**的 trip+1（冷却按 base<<(trip-1) 变长），且只对"准入时
//     那一条、结算时还没变过"的 OPEN/hold 条目生效；成功只走确切的
//     ResetScoped(channel, key, model)。真实流量的 RecordFailureWithStatusScoped 与
//     IsTrippedScoped（Open→HalfOpen 到期判定）一个字都没改，也不被本包影响。
//   - 渠道级覆写（每日上限/连续失败上限）非 0 时按渠道生效，0=沿用全局；全站每日上限
//     与嵌入开关仍是全局。

const (
	// probeJitterMin/probeJitterMax 是冷却尾段的**提前量**窗口。剩余冷却 <=probeJitterMax 时，
	// 在 [probeJitterMin, min(probeJitterMax, remaining)] 里挑一次 lead，due = expiry - lead；
	// 挑一次就记住（见 probeEntryState.due）。
	probeJitterMin = 10 * time.Second
	probeJitterMax = 60 * time.Second

	// probeTrafficSkipWindow 内该确切 tuple 有真实流量就不再探测：探测的收益远小于
	// 打断一条正在给用户干活的冷门 key/model。
	probeTrafficSkipWindow = 10 * time.Minute
	// probeTrafficRetention 是真实流量时间戳的保留期，超过就机会式丢弃（这份时间戳
	// 不进检查点，重启后从头记）。
	probeTrafficRetention = 15 * time.Minute

	// probeHistoryRingSize 是每个渠道保留的探测历史条数（最新在前）。
	probeHistoryRingSize = 20

	// probeLaneTimeout 是单次探测占住车道的上限。接线层忘了 Complete 时，车道不能
	// 永久锁死；超过这个时长就当那次探测丢了（不记结论，只把车道放开）。
	probeLaneTimeout = 5 * time.Minute

	// probeFallback429Wait 是"429 但上游没给 Retry-After"时探测车道兜底等多久。
	// 真实流量的软冷却仍是它自己的 5s 起步（recordFailure 一字未改）；探测车道更保守，
	// 免得对着一个正在限流的 key 反复出题。
	probeFallback429Wait = 30 * time.Second

	// DefaultCircuitProbeMaxStreak 是没配置时的"连续未恢复"上限。
	DefaultCircuitProbeMaxStreak = 3

	// probeAnswerLimit/probePromptLimit 是历史里落盘的答案/题面长度上限。
	probeAnswerLimit = 32
	probePromptLimit = 120
)

// CircuitProbeState* 是给 UI / 日志用的稳定状态取值（per-entry 的 probe.state）。
const (
	// CircuitProbeStateScheduled 已排期（next_in_seconds 为距离到期的秒数）。
	CircuitProbeStateScheduled = "scheduled"
	// CircuitProbeStateRunning 探测正在跑。
	CircuitProbeStateRunning = "running"
	// CircuitProbeStateSkippedTraffic 该 tuple 最近有真实流量，本轮不探。
	CircuitProbeStateSkippedTraffic = "skipped_traffic"
	// CircuitProbeStateWaitUpstream 上游要求的等待（Retry-After / 回落 429）未到期。
	CircuitProbeStateWaitUpstream = "wait_upstream"
	// CircuitProbeStateNotProbedMedia 媒体作用域、未知作用域、或没 opt-in 的嵌入端点。
	CircuitProbeStateNotProbedMedia = "not_probed_media"
	// CircuitProbeStateDisabled 通道/密钥不可用、总开关或渠道开关关掉、或当日额度用尽。
	CircuitProbeStateDisabled = "disabled"
	// CircuitProbeStateStopped 连续未恢复达上限，或剩余冷却已不足以塞进一次探测。
	CircuitProbeStateStopped = "stopped"
)

// 上游等待的成因（与运营商 hold 的 rule 分开记：hold 是人定的策略，wait 是上游要的）。
const (
	// CircuitProbeWaitCauseRetryAfter 上游给的 Retry-After（429/503 等）。
	CircuitProbeWaitCauseRetryAfter = "retry_after"
	// CircuitProbeWaitCauseFallback429 回落链路上撞到的 429。
	CircuitProbeWaitCauseFallback429 = "fallback_429"
)

// CircuitProbeTarget 是"确切这一个"探测对象：渠道 + 密钥 + 上游模型，外加该条目
// 在熔断表里的作用域（endpoint/capability 原样抄自熔断键）。
type CircuitProbeTarget struct {
	ChannelID  int    `json:"channel_id"`
	KeyID      int    `json:"key_id"`
	Model      string `json:"model"`
	Endpoint   string `json:"endpoint,omitempty"`
	Capability string `json:"capability,omitempty"`
}

// key 是该目标在熔断表里的键（与 circuitScopedKey 同构，作用域为空时与 circuitKey 相同）。
func (t CircuitProbeTarget) key() string {
	return circuitScopedKey(t.ChannelID, t.KeyID, t.Model, t.Endpoint, t.Capability)
}

// tupleKey 是"渠道:密钥:模型"三元组（忽略作用域），用于真实流量与连续计数。
func (t CircuitProbeTarget) tupleKey() string {
	return circuitKey(t.ChannelID, t.KeyID, t.Model)
}

// CircuitProbeRoute 是接线层对"该用哪个上游端点探"的回答（与 modeltest 的 endpoint
// 参数同一套口径），与熔断条目的作用域是两件事，不要混用。
type CircuitProbeRoute struct {
	Endpoint   string
	Capability string
}

// CircuitProbeReport 是 A 侧 modeltest.CircuitProbeResult 的搬运形态。接线层按字段
// 一对一填好即可；本包不解上游原文，也不碰凭据。
type CircuitProbeReport struct {
	Recovered    bool
	Attributable bool
	StatusCode   int
	RetryAfter   time.Duration
	Prompt       string
	Answer       string
	Duration     time.Duration
	Outcome      string
}

// CircuitProbeConfig 是接线层每次 tick 前刷新的口径（来自设置）。零值 = 全关。
type CircuitProbeConfig struct {
	// Enabled 是自动探测的总开关（手动/审计探测不受它约束）。
	Enabled bool
	// EmbeddingsEnabled 是嵌入端点的显式 opt-in，默认 false。
	EmbeddingsEnabled bool
	// MaxStreak 是连续多少次探测未恢复后停止自动探测该 tuple；<=0 用默认值。
	MaxStreak int
	// GlobalDailyCap 是全天（UTC+8）全局探测次数上限；<=0 表示不限。
	GlobalDailyCap int
	// ChannelDailyCap 是全天（UTC+8）单渠道探测次数上限；<=0 表示不限。
	ChannelDailyCap int
}

// maxStreak 返回生效的连续未恢复上限。
func (c CircuitProbeConfig) maxStreak() int {
	if c.MaxStreak > 0 {
		return c.MaxStreak
	}
	return DefaultCircuitProbeMaxStreak
}

// CircuitProbeChannelOverrides 是一个渠道对全局探测口径的覆写（接线层从渠道字段读来）。
// <=0 都表达「沿用全局」——渠道级没有"不限"，0 是沿用，负值（正常不该出现）也按沿处理。
type CircuitProbeChannelOverrides struct {
	DailyCap  int
	MaxStreak int
}

// maxStreakFor 返回这个渠道生效的连续未恢复上限；渠道覆写 <=0 时沿用全局口径。
func (c CircuitProbeConfig) maxStreakFor(o CircuitProbeChannelOverrides) int {
	if o.MaxStreak > 0 {
		return o.MaxStreak
	}
	return c.maxStreak()
}

// channelDailyCapFor 返回这个渠道生效的每日上限；渠道覆写 <=0 时沿用全局 ChannelDailyCap。
func (c CircuitProbeConfig) channelDailyCapFor(o CircuitProbeChannelOverrides) int {
	if o.DailyCap > 0 {
		return o.DailyCap
	}
	if c.ChannelDailyCap < 0 {
		return 0
	}
	return c.ChannelDailyCap
}

// CircuitProbeCallbacks 是接线层注入的查询/执行口。
type CircuitProbeCallbacks struct {
	// ResolveTarget 回答"确切这一个目标（渠道+密钥+模型+熔断作用域）现在还在不在、
	// 能不能发、用哪个上游端点探"。返回 false 表示目标不存在或已停用（此时条目
	// 标记为 disabled）。
	//
	// 参数是整个 CircuitProbeTarget（含 Endpoint/Capability）：同一个 channel/key/model
	// 可能同时挂着 chat / messages / responses / 嵌入几个不同作用域的条目，接线层需要
	// 知道问的是哪一条，才能把探测形状对到那条自己失败过的端点上，而不是随便挑一条。
	ResolveTarget func(target CircuitProbeTarget) (CircuitProbeRoute, bool)
	// ChannelEnabled 是该渠道的自动探测开关（手动探测不受它约束）。
	ChannelEnabled func(channelID int) bool
	// AuditPending 回答"接线层那边是否有待办/到期的运营商审计探测"。为真时自动探测
	// 让出唯一的探测车道。
	AuditPending func() bool
	// RunProbe 真正去打一次探测（接线层在这里调 modeltest.RunCircuitProbe）。
	RunProbe func(ctx context.Context, claim CircuitProbeClaim) CircuitProbeReport
	// Now 是时钟，nil 表示 time.Now()。
	Now func() time.Time
	// Jitter 挑选冷却尾段的抖动，nil 表示默认实现（真随机）。
	Jitter func(remaining time.Duration) (time.Duration, bool)
	// ChannelProbeOverrides 回答"这个渠道有没有渠道级探测口径覆写"。nil = 所有渠道都
	// 没有覆写（存量对接方零改动）。接线层保证实现快速返回（它会在决策/准入里按渠道调用）。
	ChannelProbeOverrides func(channelID int) CircuitProbeChannelOverrides
}

// CircuitProbeClaim 是一次已经准入、可以开跑的探测。ID 用于 Complete 对账。
type CircuitProbeClaim struct {
	ID int64 `json:"id"`
	// Target 是确切的渠道/密钥/模型 + 熔断条目自带的作用域。
	Target CircuitProbeTarget `json:"target"`
	// Route 是这次探测实际要用的上游端点/能力（接线层给的）。
	Route CircuitProbeRoute `json:"route"`
	// Generation 是这一段冷却的代号；条目在这期间变了，结论就作废。
	Generation string `json:"generation"`
	// FromHold 表示这次探测的对象是运营商 hold 行（而不是熔断条目）。
	FromHold bool `json:"from_hold,omitempty"`
	// EntrySeq 是准入时那一条共享状态的写序号（circuitEntry/holdEntry 的 writeSeq）。
	// 结算时在锁内比对它：不同就是已经被别人换过代，一律不改状态。
	// 这是**唯一**的不可变身份凭据（不再用"剩余冷却 > 0"或时间戳毫秒去猜）。
	EntrySeq uint64 `json:"entry_seq,omitempty"`
	// ExpiryUnixMs / TripCount / LastFailureUnixMs 只是可读快照（日志/调试用），
	// 不参与任何刷新判定。
	ExpiryUnixMs      int64 `json:"expiry_unix_ms,omitempty"`
	TripCount         int   `json:"trip_count,omitempty"`
	LastFailureUnixMs int64 `json:"last_failure_unix_ms,omitempty"`
	// Manual 表示这次探测来自运营商手动/审计请求。
	Manual bool `json:"manual,omitempty"`
	// StartedAt 是占住车道的时间。
	StartedAt time.Time `json:"started_at"`
}

// CircuitProbeRejection 是一次准入拒绝的口径。State 取值与 per-entry 视图同一套；
// 等待类拒绝（wait_upstream）带剩余秒数。
type CircuitProbeRejection struct {
	State            string `json:"state"`
	RemainingSeconds int    `json:"remaining_seconds"`
}

// CircuitProbeTotals 是当日（UTC+8）的探测计数。
type CircuitProbeTotals struct {
	Used      int `json:"used"`
	Recovered int `json:"recovered"`
	Failed    int `json:"failed"`
	Skipped   int `json:"skipped"`
}

// CircuitProbeProbeView 是 per-entry 的探测状态。
type CircuitProbeProbeView struct {
	State         string `json:"state"`
	NextInSeconds int    `json:"next_in_seconds"`
}

// CircuitProbeEntryView 是熔断表里一条非关闭条目的探测视图。
type CircuitProbeEntryView struct {
	ChannelID  int                   `json:"channel_id"`
	KeyID      int                   `json:"key_id"`
	Model      string                `json:"model"`
	Endpoint   string                `json:"endpoint,omitempty"`
	Capability string                `json:"capability,omitempty"`
	Probe      CircuitProbeProbeView `json:"probe"`
}

// CircuitProbeHistoryRow 是探测历史的一行。answer 只存"安全数字"，绝不存上游散文
// 或任何凭据；prompt 只存我们自己生成题面里安全的那部分。
type CircuitProbeHistoryRow struct {
	Timestamp  string `json:"timestamp"`
	Model      string `json:"model"`
	KeyID      int    `json:"key_id"`
	Prompt     string `json:"prompt"`
	Answer     string `json:"answer"`
	DurationMS int64  `json:"duration_ms"`
	Result     string `json:"result"`
}

// CircuitProbeChannelView 是给接线层/UI 的一个渠道的快照：per-entry 视图 + 渠道与
// 全局的当日计数 + 最近 20 条历史（最新在前）。
type CircuitProbeChannelView struct {
	ChannelID    int                      `json:"channel_id"`
	Day          string                   `json:"day"`
	Totals       CircuitProbeTotals       `json:"totals"`
	GlobalTotals CircuitProbeTotals       `json:"global_totals"`
	Entries      []CircuitProbeEntryView  `json:"entries"`
	History      []CircuitProbeHistoryRow `json:"history"`
}

// CircuitProbeChannelTotals 是全局视图里的一行渠道计数。
type CircuitProbeChannelTotals struct {
	ChannelID int `json:"channel_id"`
	CircuitProbeTotals
}

// CircuitProbeGlobalView 是全局视图：当日汇总 + 每渠道当日计数。
type CircuitProbeGlobalView struct {
	Day      string                      `json:"day"`
	Totals   CircuitProbeTotals          `json:"totals"`
	Channels []CircuitProbeChannelTotals `json:"channels"`
}

// ── 作用域分类（纯函数）───────────────────────────────────────────────────

// probeScopeKind 是一个 endpoint/capability 作用域的可探性。
type probeScopeKind int

const (
	// probeScopeUnknown 没有作用域，或作用域不在已知表里：不探。
	probeScopeUnknown probeScopeKind = iota
	// probeScopeText 已知的普通文本端点：可探（默认允许）。
	probeScopeText
	// probeScopeEmbedding 嵌入端点：只有显式 opt-in 才可探。
	probeScopeEmbedding
	// probeScopeMedia 图片/视频/音频类端点或能力位：一律不探。
	probeScopeMedia
)

// probeMediaTokens 是媒体/非文本作用域的特征词。命中即不探：这些端点要的是另一种
// 形状的请求，用一道算术题判对错没有意义，硬探只会把错误记到渠道账上。
var probeMediaTokens = []string{
	"image", "images", "video", "videos", "audio", "music", "midjourney",
	"realtime", "tts", "stt", "transcri", "speech", "sora", "veo",
}

// probeEmbeddingTokens 是嵌入作用域的特征词。
var probeEmbeddingTokens = []string{"embedding", "embeddings"}

// probeIsTextEndpoint 判断端点名是否是已知的普通文本端点。白名单而不是黑名单：不认识的
// 端点一律按"不知道该发什么形状"处理，不探。
//
// 同一张表盖两套名字：熔断条目里的 relay 口径（chat / responses / messages /
// gemini_generate_content …）与接线层给探测用的端点口径（openai_chat /
// openai_responses / anthropic_messages …，与 modeltest.normalizeEndpoint 一致）。
// 嵌入与图片的名字先被 probeMediaTokens / probeEmbeddingTokens 拦下，不到这里。
func probeIsTextEndpoint(endpoint string) bool {
	switch endpoint {
	case "chat", "chat_completions", "openai_chat",
		"responses", "openai_responses",
		"messages", "anthropic_messages",
		"gemini_generate_content", "gemini_stream_generate_content",
		"generate_content", "stream_generate_content":
		return true
	default:
		return false
	}
}

// probeContainsToken 判断 value 是否命中 tokens 中任一特征词。
func probeContainsToken(value string, tokens []string) bool {
	for _, token := range tokens {
		if strings.Contains(value, token) {
			return true
		}
	}
	return false
}

// classifyProbeScope 把熔断条目的 endpoint/capability 作用域分成四类。
//
// 顺序要紧：先看媒体（能力位里的 image/video/audio 比端点名更能说明形状），再看
// "压根没作用域"，最后才判文本/嵌入。没作用域的老口径条目（channel:key:model）与
// 不认识的作用域都归 unknown —— 我们不知道这道请求是什么形状，就不往上打题。
func classifyProbeScope(endpoint, capability string) probeScopeKind {
	ep := strings.ToLower(strings.TrimSpace(endpoint))
	capability = strings.ToLower(strings.TrimSpace(capability))

	for _, token := range strings.Split(capability, "+") {
		if probeContainsToken(strings.TrimSpace(token), probeMediaTokens) {
			return probeScopeMedia
		}
	}
	if ep != "" && probeContainsToken(ep, probeMediaTokens) {
		return probeScopeMedia
	}
	if ep == "" {
		return probeScopeUnknown
	}
	for _, token := range strings.Split(capability, "+") {
		if probeContainsToken(strings.TrimSpace(token), probeEmbeddingTokens) {
			return probeScopeEmbedding
		}
	}
	if probeContainsToken(ep, probeEmbeddingTokens) {
		return probeScopeEmbedding
	}
	if probeIsTextEndpoint(ep) {
		return probeScopeText
	}
	return probeScopeUnknown
}

// ── 抖动（纯函数，方便单测）───────────────────────────────────────────────

// defaultCircuitProbeJitter 挑的是**提前量** lead：距离到期还有多久就把探测发出去。
//
// 窗口是 [probeJitterMin, min(probeJitterMax, remaining)]（闭区间，均匀）：
//   - remaining 恰好 = probeJitterMin（10s）：窗口只剩一个点，lead = 10s，due = 现在；
//   - remaining < probeJitterMin：窗口为空，不可行（不新排）；
//   - remaining > probeJitterMax：上限就是 60s（调用方本来也不会在这时调用）。
//
// 返回的 lead 永远 <= remaining，所以 due = expiry - lead 永远不晚于到期。
func defaultCircuitProbeJitter(remaining time.Duration) (time.Duration, bool) {
	if remaining < probeJitterMin {
		return 0, false
	}
	upper := remaining
	if upper > probeJitterMax {
		upper = probeJitterMax
	}
	span := upper - probeJitterMin
	lead := probeJitterMin
	if span > 0 {
		lead += time.Duration(rand.Int63n(int64(span) + 1))
	}
	return lead, true
}

// circuitProbeJitter 走接线层注入的实现，没注入就用默认实现。
func circuitProbeJitter(cb CircuitProbeCallbacks, remaining time.Duration) (time.Duration, bool) {
	if cb.Jitter != nil {
		return cb.Jitter(remaining)
	}
	return defaultCircuitProbeJitter(remaining)
}
