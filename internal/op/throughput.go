package op

import (
	"sort"

	"github.com/zhizhishu/octopus/internal/model"
)

// 单请求「生成速度 tok/s」的有效性闸门（2026-10-27 astra 裁决，库内实测驱动）。
//
// 公式本身没错：tps = output_tokens ÷ (use_time − ftut)。错的是把**不可采信的
// 生成窗口**也拿去算——实测记录：库里有 ftut 落在 use_time 前 1ms 的行，1ms 的
// 窗口让 5874 个 token 算成 5,874,000 tok/s，3 天均值被拉到 10453 tok/s。
// 这只是「窗口不可采信」的观测；本文件**不推断**那一行为什么把首字拖到最后
// （缓冲、排队、重试、思考 token 都只是可能，单靠阈值证不出来）。
//
// 所以不能按 is_stream 卡（IsStream 记的是客户端意图，判不出上游是否缓冲，
// metrics.go:978），只能按「生成窗口是否可采信」卡。不合格的正确处理是**不采样**，
// 不是退回全程平均冒充生成速度；日志卡片对应显示一个中性的「TPS —」加原因，
// 而不是把这个指标藏起来（藏起来会让人以为这一行没有速度概念）。
const (
	// tpsMinOutputTokens：输出太少的样本，观测窗口不足以代表生成速度
	// （短样本里非生成开销占多少无法区分），比值不稳。
	tpsMinOutputTokens = 100
	// tpsMinUseTimeMs：总耗时不足 1s 的请求没有可分辨的窗口。
	tpsMinUseTimeMs = 1000
	// tpsMinGenMs：生成窗口不足 1s 时，1ms 的窗口误差就是千倍的速度误差。
	tpsMinGenMs = 1000
	// tpsMinGenShare：生成窗口至少要覆盖总耗时的 25%（等价 ftut ≤ 75%×总耗时）。
	// 防的就是"ftut 几乎贴着总耗时、窗口只剩零头"的形态。
	tpsMinGenShare = 0.25
	// tpsMaxPlausible：超过这个值的样本一律不采样（当前观测不足以采信）。
	// 不去区分是测量问题还是真值，也不拿它反推上游行为。
	tpsMaxPlausible = 1000.0
)

// relayLogTPSSample 把一条日志折算成一个可采信的 tok/s 样本；不合格返回 false。
// 首页健康、模型排行、（经 API 的）日志卡片三处共用同一套拒绝条件，
// 任何一处改阈值都必须三处同步改测试。
//
// 失败判定用全局唯一的分桶 model.ClassifyRelayLogSeverity（error / error_code /
// error_status≥400 才算失败）——只有 error 不采样，warn（重试/故障转移后成功）照常
// 采样。前端 web/src/components/modules/log/throughput.ts 用的是同一个分桶
// （classifyRelayLogSeverity），两边对同一条行的结论因此一致：管理员载荷带 error、
// 用户摘要载荷只有 error_code/error_status，结论不再分叉。
func relayLogTPSSample(relayLog model.RelayLog) (float64, bool) {
	if model.ClassifyRelayLogSeverity(relayLog) == model.RelayLogSeverityError {
		return 0, false
	}
	if relayLog.OutputTokens < tpsMinOutputTokens {
		return 0, false
	}
	if relayLog.UseTime < tpsMinUseTimeMs {
		return 0, false
	}
	if relayLog.Ftut <= 0 || relayLog.Ftut >= relayLog.UseTime {
		return 0, false
	}
	genMs := relayLog.UseTime - relayLog.Ftut
	if genMs < tpsMinGenMs {
		return 0, false
	}
	if float64(genMs) < float64(relayLog.UseTime)*tpsMinGenShare {
		return 0, false
	}
	tps := float64(relayLog.OutputTokens) / (float64(genMs) / 1000)
	if tps <= 0 || tps > tpsMaxPlausible {
		return 0, false
	}
	return tps, true
}

// tpsMedian 取逐请求 tok/s 的中位数。
//
// 为什么不是旧的「总 token ÷ 总窗口」（ratio-of-sums）：一个 1ms 的坏窗口能给
// 几千个 token 全权重，单样本即可把均值推到百万级。中位数把"窗口刚好合格但
// 极短"的残留样本也挡住。调用方必须同时展示有效样本数——1 个样本的中位数
// 不该被当成结论。
func tpsMedian(samples []float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	sorted := make([]float64, len(samples))
	copy(sorted, samples)
	sort.Float64s(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}
