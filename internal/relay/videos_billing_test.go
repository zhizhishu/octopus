package relay

import (
	"testing"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/price"
)

// 视频生成必须能算出非零成本，否则它是最贵的端点却完全免费。
//
// 回归背景：relay/videos.go 的成功分支只写 ActualModel 和 ResponseContent，
// 从不给 metrics.Stats 落成本。于是 metrics.Save 收到的 InputCost+OutputCost 恒为 0，
// op.UserRecordUsage 第一行 `if cost <= 0 { return nil }` 直接返回 —— 余额、月卡、
// 单 key 的 MaxCost 三个口子全都不动。持任意 key 可无限刷视频。
//
// 为什么按次而不是按 token：视频上游的创建响应只有 task_id/id，没有任何 token
// 用量字段（见 extractVideoTaskIDs）。按 token 单价算出来必然还是 0。
func TestVideoCallCostBillsConfiguredPerCallPrice(t *testing.T) {
	const modelName = "video-percall-test-model"
	price.SetLLMPrice(modelName, model.LLMPrice{PerCall: 0.5})
	t.Cleanup(func() { price.SetLLMPrice(modelName, model.LLMPrice{}) })

	metrics := newRawProtocolRelayMetrics(1, 1, "127.0.0.1", modelName, "videos", "/v1/videos")
	applyVideoCallCost(metrics, modelName)

	if got := metrics.Stats.OutputCost; got <= 0 {
		t.Fatalf("配了 per_call=0.5 却算出成本 %v：视频计费没生效", got)
	}
	if want := 0.5; metrics.Stats.OutputCost != want {
		t.Fatalf("成本 = %v, want %v（倍率 1 时应等于 per_call 原值）", metrics.Stats.OutputCost, want)
	}
}

// 没配 per_call 时必须保持原有行为（不计费），不能因为这次改动突然开始扣钱。
func TestVideoCallCostStaysFreeWithoutPerCallPrice(t *testing.T) {
	const modelName = "video-no-percall-test-model"
	price.SetLLMPrice(modelName, model.LLMPrice{Input: 3, Output: 15})
	t.Cleanup(func() { price.SetLLMPrice(modelName, model.LLMPrice{}) })

	metrics := newRawProtocolRelayMetrics(1, 1, "127.0.0.1", modelName, "videos", "/v1/videos")
	applyVideoCallCost(metrics, modelName)

	if metrics.Stats.OutputCost != 0 || metrics.Stats.InputCost != 0 {
		t.Fatalf("没配 per_call 却产生了费用 input=%v output=%v：会突然改变存量账单",
			metrics.Stats.InputCost, metrics.Stats.OutputCost)
	}
}

// nil metrics 不能 panic（防御性：调用点将来可能传空）。
func TestVideoCallCostNilMetrics(t *testing.T) {
	applyVideoCallCost(nil, "whatever")
}
