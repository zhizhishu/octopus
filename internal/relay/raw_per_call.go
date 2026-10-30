package relay

import (
	"github.com/zhizhishu/octopus/internal/price"
	"github.com/zhizhishu/octopus/internal/utils/log"
)

// applyRawProtocolPerCallCost 给一次成功、但上游响应里没有 token 用量的 raw 调用
// 计入「按次」费用。只有 RawProtocolOptions.PerCallBilling 打开的端点会走到这里
// （目前是 Grok 语音 /v1/tts、/v1/stt）。
//
// 口径与 applyVideoCallCost 完全一致（同一个 LLMPrice.PerCall 字段、同一套访问计划
// 倍率），刻意不另起一套：
//   - PerCall 为 0（默认）→ 不收费，只留一条警告，让「在收费却算不出钱」能被发现；
//   - PerCall > 0 → 每次成功收这么多，乘访问计划倍率。
//
// 为什么不照 sub2api 按字符数 / 音频时长计费：octopus 的价格表没有「每百万字符」
// 「每小时音频」这类单位，硬塞进 Input/Output 单价会让价格页的含义失真；按次是
// 现有媒体端点（视频、异步图片）已经在用、管理员已经会配的那一种。
func applyRawProtocolPerCallCost(metrics *rawProtocolRelayMetrics, actualModel string) {
	if metrics == nil {
		return
	}
	modelPrice := price.GetLLMPrice(actualModel)
	if modelPrice == nil || modelPrice.PerCall <= 0 {
		log.Warnf("raw protocol %s per-call cost: model %q has no per_call price, billing 0 for this request "+
			"(configure per_call on the model price to bill this endpoint)", metrics.Protocol, actualModel)
		return
	}
	snapshot := metrics.currentBillingSnapshot(actualModel)
	multiplier := snapshot.FinalMultiplier
	if multiplier <= 0 {
		multiplier = 1
	}
	metrics.BillingSnapshot = snapshot
	metrics.Stats.OutputCost = modelPrice.PerCall * multiplier
}
