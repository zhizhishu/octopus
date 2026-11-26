package handlers

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/modeltest"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/relay/balancer"
	"github.com/zhizhishu/octopus/internal/server/middleware"
	"github.com/zhizhishu/octopus/internal/server/resp"
	"github.com/zhizhishu/octopus/internal/server/router"
	"github.com/zhizhishu/octopus/internal/utils/log"
	"github.com/zhizhishu/octopus/internal/utils/safe"
)

// ── 熔断探测（circuit probe）· 接线层 ───────────────────────────────────────
//
// 三件事都在这里，且只在这里：
//
//  1. 启动一条**独立、串行**的探测 worker（全局同时只有一条探测车道，由调度器的
//     车道保证；这里只负责按节拍驱动它，并在有到期的审计探测时让出车道）。
//  2. 把两侧的接口对接起来：A 侧 modeltest.RunCircuitProbe 真正去打一道可判对错的
//     题，B 侧 balancer 决定探谁、什么时候探、探完怎么改熔断状态。中间只搬纯数据。
//  3. 对外的三条管理接口（沿用 /api/v1/channel 的 Auth + AdminOnly，和渠道其它接口
//     同一套鉴权）。
//
// 有意不做的事：不写 relay 审计日志、不写用户统计、不记账（探测不是用户请求，见
// modeltest 的出站口径）；不回显任何凭据或上游原文（历史里只有我们自己出的题面里的
// 数字，答案只留第一段数字）。

const (
	// circuitProbeTickInterval 是探测 worker 的空转节拍。探测本身是"冷却尾段才有意义"
	// 的动作，节拍比它密没有任何收益，只会白打库；比它疏又会让到期的探测迟迟不跑。
	circuitProbeTickInterval = 15 * time.Second
	// circuitProbeLookupBudget 是接线层读一次渠道的超时（与审计跟查同量级）。
	circuitProbeLookupBudget = 5 * time.Second

	// circuitProbeEntryStateOpen / circuitProbeEntryStateHeld 是 balancer.CircuitEntryView
	// 里“真的在挡路、归这里手动探测管”的两种状态（与 web 端 ChannelCircuitEntry 的取值
	// 同一套字符串）。
	circuitProbeEntryStateOpen = "open"
	circuitProbeEntryStateHeld = "held"
)

func init() {
	router.NewGroupRouter("/api/v1/channel").
		Use(middleware.Auth()).
		Use(middleware.AdminOnly()).
		AddRoute(
			router.NewRoute("/probe", http.MethodPost).
				Use(middleware.RequireJSON()).
				Handle(startChannelProbe),
		).
		AddRoute(
			router.NewRoute("/probes/:id", http.MethodGet).
				Handle(getChannelProbe),
		).
		AddRoute(
			router.NewRoute("/probe-stats", http.MethodGet).
				Handle(getChannelProbeStats),
		)
}

// ── worker ──────────────────────────────────────────────────────────────

var circuitProbeWorkerStarted bool

// circuitProbeWake 是「刚有人手动入队，去看一眼」的唤醒信号。
//
// 为什么需要它：手动探测的生命周期很短（冷却尾段里往往只剩几十秒窗口），只靠 15s
// 空转节拍会眼睁睁错过。入队成功后叫一声，worker 立刻跑一轮。
//
// 容量 1：连续入队多个目标只合并成一次唤醒（一条车道一轮本来就只跑一条，剩下的由
// RunDueCircuitProbes 在同一轮里串行跑完）；缓冲 1 也保证车道忙时那次唤醒不会丢
// ——循环回到 select 会立刻取到它。消费方只有 worker 自己那条串行循环，所以不会
// 多出第二个探测执行者（不会出现并发探测）。
var circuitProbeWake = make(chan struct{}, 1)

// signalCircuitProbeWake 叫一声 worker。已经有待处理的唤醒就合并，永不阻塞。
func signalCircuitProbeWake() {
	select {
	case circuitProbeWake <- struct{}{}:
	default:
	}
}

// StartCircuitProbeWorker 在服务启动时拉起熔断探测 worker。可重复调用。
//
// 位置与 StartModelAuditQueueWorker 同级（server.Start 之后）：探测队列与审计队列
// 都要等数据库/缓存就绪，也都必须在任何入队请求到达之前就在跑。
func StartCircuitProbeWorker() {
	if circuitProbeWorkerStarted {
		return
	}
	circuitProbeWorkerStarted = true
	safe.SafeGo("circuit-probe-worker", func() {
		circuitProbeLoop(context.Background())
	})
}

func circuitProbeLoop(ctx context.Context) {
	log.Debugf("circuit probe worker started")
	applyCircuitProbeConfig()
	ticker := time.NewTicker(circuitProbeTickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Debugf("circuit probe worker stopped")
			return
		case <-ticker.C:
			circuitProbeTick(ctx)
		case <-circuitProbeWake:
			// 手动探测刚入队：同一个串行循环里立刻跑一轮，不等下一个节拍。
			circuitProbeTick(ctx)
		}
	}
}

// circuitProbeTick 是一次心跳：先把口径与回调刷新进调度器，再串行跑完当前到期的探测。
//
// 串行是刻意的：每次探测都是一条真实的上游请求，同时探多个渠道等于让上游在一瞬间
// 承受成倍压力，也把"这一次是不是渠道的问题"搅浑。车道由调度器独占（global one active），
// 所以哪怕以后有人把节拍调密，也不会并发。
func circuitProbeTick(ctx context.Context) {
	applyCircuitProbeConfig()
	balancer.RunDueCircuitProbes(ctx)
}

// applyCircuitProbeConfig 把设置与回调刷进调度器（调度器每次 tick 前读一次即可）。
func applyCircuitProbeConfig() {
	balancer.ConfigureCircuitProbe(circuitProbeConfig(), circuitProbeCallbacks())
}

// 探测的出厂口径。它们必须与 model.DefaultSettings 里种下的值一致，而且**读设置
// 失败时也要用它们兜底**：balancer 把 <=0 的额度当成“不限”，一次库抖动如果落成 0，
// 就等于把「每天 200 次」悄悄改成「无上限」——这种默认值不能靠“零值恰好是对的”。
const (
	defaultCircuitProbeDailyCap        = 200
	defaultCircuitProbeChannelDailyCap = 20
	defaultCircuitProbeMaxStreak       = 3
)

// circuitProbeConfig 从设置读一遍口径。
//
// 两道开关读不到就是关（零值即关，读失败绝不会把花钱功能打开）；额度与连续上限
// 读不到/库上是不合法值时回到出场口径，而不是零值（零值在 balancer 里意味着“不限”）。
func circuitProbeConfig() balancer.CircuitProbeConfig {
	cfg := balancer.CircuitProbeConfig{
		GlobalDailyCap:  defaultCircuitProbeDailyCap,
		ChannelDailyCap: defaultCircuitProbeChannelDailyCap,
		MaxStreak:       defaultCircuitProbeMaxStreak,
	}
	if value, err := op.SettingGetBool(model.SettingKeyCircuitProbeEnabled); err == nil {
		cfg.Enabled = value
	}
	if value, err := op.SettingGetBool(model.SettingKeyCircuitProbeEmbeddingsEnabled); err == nil {
		cfg.EmbeddingsEnabled = value
	}
	if value, err := op.SettingGetInt(model.SettingKeyCircuitProbeDailyCap); err == nil && value >= 0 {
		cfg.GlobalDailyCap = value
	}
	if value, err := op.SettingGetInt(model.SettingKeyCircuitProbeChannelDailyCap); err == nil && value >= 0 {
		cfg.ChannelDailyCap = value
	}
	if value, err := op.SettingGetInt(model.SettingKeyCircuitProbeMaxConsecutiveFailures); err == nil && value > 0 {
		cfg.MaxStreak = value
	}
	return cfg
}

func circuitProbeCallbacks() balancer.CircuitProbeCallbacks {
	return balancer.CircuitProbeCallbacks{
		ResolveTarget:         resolveCircuitProbeTarget,
		ChannelEnabled:        circuitProbeChannelEnabled,
		ChannelProbeOverrides: circuitProbeChannelProbeOverrides,
		AuditPending:          circuitProbeAuditPending,
		RunProbe:              runCircuitProbeProbe,
	}
}

// circuitProbeChannelEnabled 回答"这个渠道的**自动**探测开关开着没有"。渠道不存在/读失败按关。
func circuitProbeChannelEnabled(channelID int) bool {
	ctx, cancel := context.WithTimeout(context.Background(), circuitProbeLookupBudget)
	defer cancel()
	channel, err := op.ChannelGet(channelID, ctx)
	if err != nil || channel == nil {
		return false
	}
	return channel.ProbeEnabled
}

// circuitProbeChannelProbeOverrides 读一个渠道的探测覆写。渠道不存在/读失败 = 没有覆写；
// 负值（正常不该出现）按"沿用全局"返回 0。
func circuitProbeChannelProbeOverrides(channelID int) balancer.CircuitProbeChannelOverrides {
	ctx, cancel := context.WithTimeout(context.Background(), circuitProbeLookupBudget)
	defer cancel()
	channel, err := op.ChannelGet(channelID, ctx)
	if err != nil || channel == nil {
		return balancer.CircuitProbeChannelOverrides{}
	}
	overrides := balancer.CircuitProbeChannelOverrides{
		DailyCap:  channel.ProbeDailyCap,
		MaxStreak: channel.ProbeMaxFailures,
	}
	return overrides
}

// circuitProbeChannelDailyCap 返回这个渠道**生效**的每日上限（渠道覆写>0 时用它，否则 globalCap）。
// 渠道不存在/读失败按 globalCap。getChannelProbe / getChannelProbeStats 回显 cap 用它，
// 让页面显示的"每天最多"和调度实际生效的上限是同一个数。
func circuitProbeChannelDailyCap(channelID int, globalCap int) int {
	if channelID <= 0 {
		return globalCap
	}
	ctx, cancel := context.WithTimeout(context.Background(), circuitProbeLookupBudget)
	defer cancel()
	channel, err := op.ChannelGet(channelID, ctx)
	if err != nil || channel == nil {
		return globalCap
	}
	if channel.ProbeDailyCap > 0 {
		return channel.ProbeDailyCap
	}
	return globalCap
}

// circuitProbeChannelDailyCaps 一次建表：channelID -> 这个渠道生效的每日上限。
// 只把覆写>0 的渠道写进表（其余条目本来就该落到全局口径）；渠道列表读失败时
// 返回空表（调用方逐行退回全局 cap），不把一次读失败放大成 500 或全零。
func circuitProbeChannelDailyCaps(globalCap int) map[int]int {
	caps := map[int]int{}
	ctx, cancel := context.WithTimeout(context.Background(), circuitProbeLookupBudget)
	defer cancel()
	channels, err := op.ChannelList(ctx)
	if err != nil {
		log.Warnf("circuit probe: read channel probe caps: %v", err)
		return caps
	}
	for _, channel := range channels {
		if channel.ProbeDailyCap > 0 {
			caps[channel.ID] = channel.ProbeDailyCap
		}
	}
	return caps
}

// circuitProbeAuditPending 回答"审计那边是不是有到点该跑的活"。
//
// 只读一次带索引的 pending 计数，不碰任何审计语义：定时抽检与可疑日志跟查都落在
// model_audit_results 的 pending 行上（next_retry_at 为空或已到点才是"现在就该跑"），
// 这里问的就是这个既成事实。有活时唯一的探测车道让给审计——审计是运营商/日志证据
// 驱动的钱，优先级高于自动探测。
func circuitProbeAuditPending() bool {
	handle := db.GetDB()
	if handle == nil {
		return false
	}
	var pending int64
	if err := handle.Model(&model.ModelAuditResult{}).
		Where("status = ?", model.AuditResultPending).
		Where("next_retry_at IS NULL OR next_retry_at <= ?", time.Now()).
		Count(&pending).Error; err != nil {
		// 读失败不当作"有活"：那会让自动探测在一次库抖动之后永久停摆，
		// 而它本来也只影响"这一轮要不要让路"。
		log.Warnf("circuit probe: read pending model audits: %v", err)
		return false
	}
	return pending > 0
}

// runCircuitProbeProbe 真正去打一次探测：A 侧 modeltest 用与渠道检测同一身身份出站。
//
// 死线交给 modeltest：调用方 ctx 到点算"调用方取消"（不计渠道故障），探针自己的
// 死线到点才算渠道故障。这里再加一道钟会把前者伪装成后者，让一个健康渠道被记失败。
func runCircuitProbeProbe(ctx context.Context, claim balancer.CircuitProbeClaim) balancer.CircuitProbeReport {
	lookupCtx, cancel := context.WithTimeout(ctx, circuitProbeLookupBudget)
	channel, err := op.ChannelGet(claim.Target.ChannelID, lookupCtx)
	cancel()
	if err != nil || channel == nil {
		// 准入之后渠道没了（被删/被停）：这一次不发，也不算渠道故障。
		return balancer.CircuitProbeReport{Outcome: modeltest.CircuitProbeOutcomeSkipped}
	}
	// 渠道配置了 ProbeModel 时换它打上游；不改 claim.Target——熔断条目身份保持，
	// 只换实际发送的模型名。
	probeModel := strings.TrimSpace(claim.Target.Model)
	if m := strings.TrimSpace(channel.ProbeModel); m != "" {
		probeModel = m
	}
	result := modeltest.RunCircuitProbe(
		ctx,
		*channel,
		claim.Target.KeyID,
		probeModel,
		claim.Route.Endpoint,
		claim.Route.Capability,
	)
	return balancer.CircuitProbeReport{
		Recovered:    result.Recovered,
		Attributable: result.Attributable,
		StatusCode:   result.StatusCode,
		RetryAfter:   result.RetryAfter,
		Prompt:       result.Prompt,
		Answer:       result.Answer,
		Duration:     result.Duration,
		Outcome:      result.Outcome,
	}
}

// ── 目标与作用域 ────────────────────────────────────────────────────────

// resolveCircuitProbeTarget 回答调度方"该用哪个上游端点探这个确切的目标"。
//
// 目标自带熔断条目自己的作用域（Endpoint/Capability 原样抄自熔断键），这里只把它翻成
// 探测端点，不再去熔断表里“找第一条同 channel/key/model 的条目”——同一个三元组可能同时
// 挂着 chat / messages / responses / 嵌入几个作用域，拿第一条去探会打错形状，甚至把一个
// 本该能探的普通文本条目判成不可探。
//
// 三道路由/模型闸门都在这里，且不接受任何豁免（手动探测也不）：
//   - 渠道存在且启用；
//   - 那把密钥存在、启用、有凭据；
//   - 模型是这个渠道**模型配置里确切的那个上游名**（selected models 或映射后的上游名）。
//     探测打的就是真实转发会发出去的那个名字；不做映射、不模糊匹配、不接受调用方自己编的
//     模型名——否则任何人都能借探测往上游发一次任意模型的请求。
func resolveCircuitProbeTarget(target balancer.CircuitProbeTarget) (balancer.CircuitProbeRoute, bool) {
	probeModel := strings.TrimSpace(target.Model)
	if target.ChannelID <= 0 || target.KeyID <= 0 || probeModel == "" {
		return balancer.CircuitProbeRoute{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), circuitProbeLookupBudget)
	defer cancel()
	channel, err := op.ChannelGet(target.ChannelID, ctx)
	if err != nil || channel == nil || !channel.Enabled {
		return balancer.CircuitProbeRoute{}, false
	}
	if _, ok := circuitProbeKey(*channel, target.KeyID); !ok {
		// 密钥不存在 / 被停用 / 没凭据：路由与密钥的启用判定不接受任何豁免。
		return balancer.CircuitProbeRoute{}, false
	}
	// 覆写语义：渠道配置了 ProbeModel 时，它**代替**熔断条目里的模型打上游；条目里的
	// 模型是既存事实，不再要求它仍在渠道模型配置里——校验只对着实际发送的那个名字。
	if m := strings.TrimSpace(channel.ProbeModel); m != "" {
		probeModel = m
	}
	if !circuitProbeServesModel(*channel, probeModel) {
		return balancer.CircuitProbeRoute{}, false
	}
	endpoint, capability, ok := op.CircuitProbeEndpointForScope(target.Endpoint, target.Capability)
	if !ok {
		return balancer.CircuitProbeRoute{}, false
	}
	return balancer.CircuitProbeRoute{Endpoint: endpoint, Capability: capability}, true
}

// circuitProbeServesModel 报告这个渠道的**模型配置**里有没有这个确切的上游模型名。
//
// 判据只看渠道自己：没被重映射的 selected models（它们自己就是上游名）与 ModelMapping
// 的目标值（映射后的上游名）。被重映射的请求名**不**算——那个名字到了上游会被换成别的
// 模型，拿它去探测等于绕开映射发一个渠道从不会真实发出的请求。没弄模糊匹配、没有兜底
// 猜测：一次探测不能被当成“任意模型名都能打上游”的入口。
func circuitProbeServesModel(ch model.Channel, upstreamModel string) bool {
	upstreamModel = strings.TrimSpace(upstreamModel)
	if upstreamModel == "" {
		return false
	}
	for _, name := range model.ChannelSelectedModelNames(ch) {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if mapped := strings.TrimSpace(ch.ModelMapping[name]); mapped != "" && !strings.EqualFold(mapped, name) {
			// 这个请求名会被映射成别的上游名：它自己不会出现在熔断键上。
			continue
		}
		if strings.EqualFold(name, upstreamModel) {
			return true
		}
	}
	for _, mapped := range ch.ModelMapping {
		if strings.TrimSpace(mapped) != "" && strings.EqualFold(strings.TrimSpace(mapped), upstreamModel) {
			return true
		}
	}
	return false
}

// circuitProbeKey 找这个渠道下**指定的**那一把密钥：必须存在、启用、带凭据。
//
// 有意不看冷却/隔离状态：探测的目的正是"这把被判死的 key 现在还能不能打"。
func circuitProbeKey(ch model.Channel, keyID int) (model.ChannelKey, bool) {
	if keyID <= 0 {
		return model.ChannelKey{}, false
	}
	for _, key := range ch.Keys {
		if key.ID != keyID {
			continue
		}
		if !key.Enabled || strings.TrimSpace(key.ChannelKey) == "" {
			return model.ChannelKey{}, false
		}
		return key, true
	}
	return model.ChannelKey{}, false
}

// circuitProbeTargets 列出这个 tuple 在熔断表里**现存的具体条目**，每个作用域一条
// （顺序取自 ListChannelCircuits，去重后仍然稳定）。
//
// 手动探测只探这些，且只探 OPEN / 运营商 hold 的：它们是“这个渠道真的被挡住了”的事实。
// 半开条目归真实流量自己的试探，不归这里；而一个健康渠道 + 调用方自己编的模型名
// **不能**变成往上游发一次探测请求的入口。
func circuitProbeTargets(ch model.Channel, keyID int, probeModel string) []balancer.CircuitProbeTarget {
	probeModel = strings.TrimSpace(probeModel)
	targets := make([]balancer.CircuitProbeTarget, 0, 2)
	seen := make(map[string]struct{}, 2)
	for _, entry := range balancer.ListChannelCircuits(ch.ID) {
		if entry.KeyID != keyID || entry.Model != probeModel {
			continue
		}
		switch entry.State {
		case circuitProbeEntryStateOpen, circuitProbeEntryStateHeld:
		default:
			// 半开（真实流量的试探）与其它状态不归手动探测管。
			continue
		}
		scopeKey := entry.Endpoint + "\x00" + entry.Capability
		if _, ok := seen[scopeKey]; ok {
			continue
		}
		seen[scopeKey] = struct{}{}
		targets = append(targets, balancer.CircuitProbeTarget{
			ChannelID:  ch.ID,
			KeyID:      keyID,
			Model:      probeModel,
			Endpoint:   entry.Endpoint,
			Capability: entry.Capability,
		})
	}
	return targets
}

// ── 对外接口 ────────────────────────────────────────────────────────────

// channelCircuitView 是 /channel/circuits/:id 的一行：原有字段一个不少、一个不改，
// 只多一个 probe。字段是内嵌的，所以 JSON 里仍然是同一层级的 key_id/model/state/…，
// 老前端（只认那些字段）不会因为多了一个 probe 而坏掉。
type channelCircuitView struct {
	balancer.CircuitEntryView
	// Probe 是这一条熔断条目的探测视图。半开/已过冷却的条目不在调度方的候选里
	// （那条路由真实流量自己处理），此时没有探测状态可说，字段就不出现。
	Probe *balancer.CircuitProbeProbeView `json:"probe,omitempty"`
}

type circuitProbeEntryKey struct {
	keyID      int
	model      string
	endpoint   string
	capability string
}

// channelCircuitsWithProbe 把熔断条目列表与探测视图合成一份（保持原有数组形态与顺序）。
func channelCircuitsWithProbe(channelID int) []channelCircuitView {
	entries := balancer.ListChannelCircuits(channelID)
	probes := make(map[circuitProbeEntryKey]balancer.CircuitProbeProbeView)
	for _, entry := range balancer.SnapshotCircuitProbe(channelID).Entries {
		probes[circuitProbeEntryKey{entry.KeyID, entry.Model, entry.Endpoint, entry.Capability}] = entry.Probe
	}
	views := make([]channelCircuitView, 0, len(entries))
	for _, entry := range entries {
		view := channelCircuitView{CircuitEntryView: entry}
		if probe, ok := probes[circuitProbeEntryKey{entry.KeyID, entry.Model, entry.Endpoint, entry.Capability}]; ok {
			copied := probe
			view.Probe = &copied
		}
		views = append(views, view)
	}
	return views
}

// channelProbeTotals 是当日计数 + 生效的那道限额。cap 原样回显设置值（0 = 不限），
// 让页面自己决定怎么显示"不限"。
type channelProbeTotals struct {
	Used      int `json:"used"`
	Cap       int `json:"cap"`
	Recovered int `json:"recovered"`
	Failed    int `json:"failed"`
	Skipped   int `json:"skipped"`
}

func circuitProbeTotalsWithCap(totals balancer.CircuitProbeTotals, limit int) channelProbeTotals {
	return channelProbeTotals{
		Used:      totals.Used,
		Cap:       limit,
		Recovered: totals.Recovered,
		Failed:    totals.Failed,
		Skipped:   totals.Skipped,
	}
}

type channelProbeDetailResponse struct {
	ChannelID    int                               `json:"channel_id"`
	Day          string                            `json:"day"`
	Totals       channelProbeTotals                `json:"totals"`
	GlobalTotals channelProbeTotals                `json:"global_totals"`
	History      []balancer.CircuitProbeHistoryRow `json:"history"`
}

// channelProbeStatsChannel 是全局视图里的一行渠道计数。字段与 channelProbeTotals 一样，
// 但是**展开写**的：内嵌一个未导出类型虽然能序列化，却把“JSON 里是不是同一层”这事
// 变成了一个要每次去查的细节；契约上的形状应该是显式的。
type channelProbeStatsChannel struct {
	ChannelID int `json:"channel_id"`
	Used      int `json:"used"`
	Cap       int `json:"cap"`
	Recovered int `json:"recovered"`
	Failed    int `json:"failed"`
	Skipped   int `json:"skipped"`
}

func newChannelProbeStatsChannel(channelID int, totals balancer.CircuitProbeTotals, limit int) channelProbeStatsChannel {
	withCap := circuitProbeTotalsWithCap(totals, limit)
	return channelProbeStatsChannel{
		ChannelID: channelID,
		Used:      withCap.Used,
		Cap:       withCap.Cap,
		Recovered: withCap.Recovered,
		Failed:    withCap.Failed,
		Skipped:   withCap.Skipped,
	}
}

type channelProbeStatsResponse struct {
	Day      string                     `json:"day"`
	Totals   channelProbeTotals         `json:"totals"`
	Channels []channelProbeStatsChannel `json:"channels"`
}

type channelProbeStartResponse struct {
	State string `json:"state"`
	// NextInSeconds 是"下一次自动探测还差多少秒"。手动请求刚被接受时是 0（已经在队列里）。
	NextInSeconds int `json:"next_in_seconds"`
	// RemainingSeconds 只在被拒时给：等待类拒绝（wait_upstream）时它是有意义的
	// 剩余秒数，其它拒绝里是 0。
	RemainingSeconds *int `json:"remaining_seconds,omitempty"`
}

func getChannelProbe(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id <= 0 {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidParam)
		return
	}
	cfg := circuitProbeConfig()
	capEffective := circuitProbeChannelDailyCap(id, cfg.ChannelDailyCap)
	view := balancer.SnapshotCircuitProbe(id)
	history := view.History
	if history == nil {
		history = []balancer.CircuitProbeHistoryRow{}
	}
	resp.Success(c, channelProbeDetailResponse{
		ChannelID:    id,
		Day:          view.Day,
		Totals:       circuitProbeTotalsWithCap(view.Totals, capEffective),
		GlobalTotals: circuitProbeTotalsWithCap(view.GlobalTotals, cfg.GlobalDailyCap),
		History:      history,
	})
}

func getChannelProbeStats(c *gin.Context) {
	cfg := circuitProbeConfig()
	view := balancer.SnapshotCircuitProbeGlobal()
	// 每渠道生效上限一次建表：覆盖>0 用覆写，其余（含读失败）全局口径——
	// 页面上显示的"每天最多"必须和调度实际生效的是同一个数。
	caps := circuitProbeChannelDailyCaps(cfg.ChannelDailyCap)
	channels := make([]channelProbeStatsChannel, 0, len(view.Channels))
	for _, channel := range view.Channels {
		cap, ok := caps[channel.ChannelID]
		if !ok {
			cap = cfg.ChannelDailyCap
		}
		channels = append(channels, newChannelProbeStatsChannel(channel.ChannelID, channel.CircuitProbeTotals, cap))
	}
	resp.Success(c, channelProbeStatsResponse{
		Day:      view.Day,
		Totals:   circuitProbeTotalsWithCap(view.Totals, cfg.GlobalDailyCap),
		Channels: channels,
	})
}

// startChannelProbe 是运营商手动探测：只越过自动探测的两道闸门（总开关 + 渠道开关）
// 与抖动；路由/密钥启用、模型归属、目标资格（必须是一条真的在挡路的 OPEN/hold 条目）、
// 当日额度、上游等待与"全局只有一条探测车道"一条都不越过。
//
// 拒绝也回 200 + 结构化 body（state/next_in_seconds/remaining_seconds）：这不是
// 服务端错误，是"现在不能探，原因是这个"。把拒绝塞进 4xx/5xx 会让页面只能弹一句
// 通用错误，看不出到底是在等上游还是额度用完了。
func startChannelProbe(c *gin.Context) {
	var request struct {
		ID         int     `json:"id"`
		KeyID      int     `json:"key_id"`
		Model      string  `json:"model"`
		Endpoint   *string `json:"endpoint"`
		Capability string  `json:"capability"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	request.Model = strings.TrimSpace(request.Model)
	if request.ID <= 0 || request.KeyID <= 0 || request.Model == "" {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidParam)
		return
	}
	channel, err := op.ChannelGet(request.ID, c.Request.Context())
	if err != nil || channel == nil {
		resp.Error(c, http.StatusBadRequest, "channel not found")
		return
	}

	// 1) 上游还在要求等待：不问哪一条，先按等待回报。这是唯一带真实剩余秒数的拒绝，
	//    也是唯一一条能告诉运营商“等多久再来”的回答。
	if remaining, cause := balancer.CircuitProbeWaitRemaining(channel.ID, request.KeyID, request.Model); remaining > 0 {
		log.Debugf("circuit probe manual start deferred: channel=%d key=%d model=%s cause=%s remaining=%s",
			channel.ID, request.KeyID, request.Model, cause, remaining)
		respondChannelProbeRejection(c, balancer.CircuitProbeRejection{
			State:            balancer.CircuitProbeStateWaitUpstream,
			RemainingSeconds: int((remaining + time.Second - 1) / time.Second),
		})
		return
	}

	targets := circuitProbeTargets(*channel, request.KeyID, request.Model)
	if request.Endpoint != nil {
		endpoint := strings.TrimSpace(*request.Endpoint)
		capability := strings.TrimSpace(request.Capability)
		exact := targets[:0]
		for _, target := range targets {
			if target.Endpoint == endpoint && target.Capability == capability {
				exact = append(exact, target)
			}
		}
		targets = exact
	}
	if len(targets) != 1 {
		respondChannelProbeRejection(c, balancer.CircuitProbeRejection{State: balancer.CircuitProbeStateDisabled})
		return
	}
	target := targets[0]
	if rejection, ok := balancer.CircuitProbeAdmission(time.Now(), target, true); !ok {
		respondChannelProbeRejection(c, rejection)
		return
	}
	if !balancer.QueueManualCircuitProbe(target) {
		respondChannelProbeRejection(c, balancer.CircuitProbeRejection{State: balancer.CircuitProbeStateDisabled})
		return
	}
	signalCircuitProbeWake()
	resp.Success(c, channelProbeStartResponse{
		State:         balancer.CircuitProbeStateScheduled,
		NextInSeconds: 0,
	})
}

func respondChannelProbeRejection(c *gin.Context, rejection balancer.CircuitProbeRejection) {
	state := rejection.State
	if state == "" {
		state = balancer.CircuitProbeStateDisabled
	}
	remaining := rejection.RemainingSeconds
	resp.Success(c, channelProbeStartResponse{
		State:            state,
		NextInSeconds:    0,
		RemainingSeconds: &remaining,
	})
}
