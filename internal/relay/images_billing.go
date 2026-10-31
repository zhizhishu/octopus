package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"strconv"
	"strings"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/price"
	"github.com/zhizhishu/octopus/internal/relay/bodycache"
)

// 生图计费口径（同步 /v1/images/* 与异步 /v1/images/*/async 共用这一份）。
//
// 照抄的行业做法：
//   - new-api（relay/helper/price.go ModelPriceHelper / ModelPriceHelperPerCall，
//     service/image_billing.go PrepareImageBillingForRequest）：模型配了固定价
//     （ModelPrice）就按「单价 × 张数 n × 分组倍率」收，不再看 token；没配固定价就按
//     倍率（token）算。两样都没配 → 拒绝（"model price not configured"），只有
//     自用模式才放行。
//   - sub2api（service/gateway_usage_billing.go calculateImageCost）：配了按张价就按
//     「张数 × 单价」，否则按上游回的 token 用量算。
//   - one-api（relay/controller/image.go）：同样是「单价 × n」。
//
// 落到 octopus 的价格行（model.LLMPrice）上就是：
//   - PerCall > 0 → 按张：PerCall × n × 访问计划倍率，token 用量只做展示；
//   - 否则 → 按 token：上游 usage × Input/Output/Cache 单价 × 倍率（同步路径原有算法）。

// imageBillingModePerImage / imageBillingModeToken 是异步任务行上冻结的计费方式。
// 空值 = 改动前提交的存量任务：只认提交时的预扣，不做差额结算。
const (
	imageBillingModePerImage = "per_image"
	imageBillingModeToken    = "token"
)

// imageRequestCountMax 只是防呆上限（OpenAI 自己最多 10 张）。超过的 n 照样算钱，
// 但不让一个离谱的数字把单价乘到天上去——上游本来也会拒绝。
const imageRequestCountMax = 128

// imageRequestCount 读请求里的 n（要生成几张图），缺省/非法一律按 1。
func imageRequestCount(payload map[string]any) int {
	if payload == nil {
		return 1
	}
	return imageCountFromValue(payload["n"])
}

func imageCountFromValue(v any) int {
	n := 1
	switch vv := v.(type) {
	case float64:
		n = int(vv)
	case int:
		n = vv
	case string:
		if parsed, err := strconv.Atoi(strings.TrimSpace(vv)); err == nil {
			n = parsed
		}
	case json.Number:
		if parsed, err := vv.Int64(); err == nil {
			n = int(parsed)
		}
	}
	if n < 1 {
		return 1
	}
	if n > imageRequestCountMax {
		return imageRequestCountMax
	}
	return n
}

// imagePerImagePrice 返回计费模型的按张单价（0 = 没配按张价）。
func imagePerImagePrice(billingModel string) float64 {
	p := price.GetLLMPrice(billingModel)
	if p == nil || p.PerCall <= 0 {
		return 0
	}
	return p.PerCall
}

// imageModelPriced 报告计费模型有没有任何价格（token 价或按张价）。
// 与 price.GetLLMPrice 同一个判据：全 0 的占位行不算有价。
func imageModelPriced(billingModel string) bool {
	return price.GetLLMPrice(billingModel) != nil
}

// imagePerImageCost = 单价 × 张数 × 访问计划倍率。
func imagePerImageCost(unit float64, count int, snapshot model.AccessPlanBillingSnapshot) float64 {
	if unit <= 0 {
		return 0
	}
	if count < 1 {
		count = 1
	}
	multiplier := snapshot.FinalMultiplier
	if multiplier <= 0 {
		multiplier = 1
	}
	return unit * float64(count) * multiplier
}

// applyPerImageCost 把一条已经有 token 用量（或没有）的计费快照改写成「按张」：
// 费用全部落在 FinalOutputCost / OutputCost 上，token 数保留只做展示。
func applyPerImageCost(snapshot *model.AccessPlanBillingSnapshot, stats *model.StatsMetrics, cost float64) {
	snapshot.FinalInputCost = 0
	snapshot.FinalCacheReadCost = 0
	snapshot.FinalCacheWriteCost = 0
	snapshot.FinalOutputCost = cost
	stats.InputCost = 0
	stats.OutputCost = cost
}

// ApplyImagePricing 在同步生图成功后按统一口径定价：配了按张价就按张收，
// 否则保持 SetUsageFromImages 算出来的 token 费用。count 返回请求的张数 n
// （只在真要按张收时才调用：multipart 要把请求体再读一遍才拿得到 n）。
func (m *imagesRelayMetrics) ApplyImagePricing(actualModel string, count func() int) {
	if m == nil {
		return
	}
	snapshot := m.currentBillingSnapshot(actualModel)
	unit := imagePerImagePrice(snapshot.BillingModelName)
	if unit <= 0 {
		return
	}
	n := 1
	if count != nil {
		n = count()
	}
	applyPerImageCost(&snapshot, &m.Stats, imagePerImageCost(unit, n, snapshot))
	m.BillingSnapshot = snapshot
}

// imageModelPriceNotConfiguredMessage 是异步生图遇到没价模型时给客户端的说明。
// 照 new-api 的 relay.model_price_not_configured：说清是哪个模型、找谁解决。
func imageModelPriceNotConfiguredMessage(modelName string) string {
	return fmt.Sprintf("模型 %s 还没有配置价格，不能提交异步生图任务，请联系管理员在模型价格里填写 token 价或按次价；"+
		"model %s has no price configured, so async image tasks are not available for it. "+
		"Please ask the administrator to set a token price or a per-call price.", modelName, modelName)
}

// multipartImageCount 从 multipart 请求体里读 n（edits 走 multipart）。读不到按 1。
func multipartImageCount(bc *bodycache.BodyCache, boundary string) int {
	if bc == nil || boundary == "" {
		return 1
	}
	r, err := bc.NewReader()
	if err != nil {
		return 1
	}
	defer r.Close()
	mr := multipart.NewReader(r, boundary)
	for {
		part, err := mr.NextPart()
		if err != nil {
			// io.EOF = 没有 n 字段；其它错误 = 读不下去。两种都按默认 1 张。
			return 1
		}
		if part.FormName() == "n" {
			b, _ := io.ReadAll(io.LimitReader(part, 16))
			_ = part.Close()
			return imageCountFromValue(string(b))
		}
		_, _ = io.Copy(io.Discard, part)
		_ = part.Close()
	}
}

// ImageTaskSettlement 是一条异步生图任务完成时的最终价格。
type ImageTaskSettlement struct {
	// Cost 是这一单最终收的钱（写回 image_tasks.cost，进日志）。
	Cost float64
	// Delta = Cost - 提交时的预扣：> 0 补扣，< 0 退回，0 不动钱。
	Delta float64

	usage       *imagesUsage
	snapshot    model.AccessPlanBillingSnapshot
	hasSnapshot bool
}

// ImageTaskSettle 按任务行上冻结的计费口径算出最终价格（纯计算，不动钱）。
// body 是上游原始响应（用量从里面的 usage 读，b64 图像数据不会被整体解析）。
//
// 与 new-api service/task_polling.go settleTaskBillingOnComplete 同一套规则：
//   - 按张（per_image）：保持预扣，不结算（new-api：PerCallBilling 跳过差额结算）；
//   - 按 token：用上游回的 usage 重算（new-api：RecalculateTaskQuotaByTokens），
//     与预扣的差额补扣或退回；上游没回 usage 就只能按 0 收，并在日志里标「上游没给用量」，
//     与同步图片同一口径；
//   - 存量任务（BillingMode 为空）：只认预扣，不结算。
func ImageTaskSettle(task model.ImageTask, body []byte) ImageTaskSettlement {
	s := ImageTaskSettlement{Cost: task.Cost}
	scanner := newUsageScanner()
	scanner.Feed(body)
	s.usage = scanner.Usage()

	if strings.TrimSpace(task.BillingSnapshot) != "" {
		if err := json.Unmarshal([]byte(task.BillingSnapshot), &s.snapshot); err == nil {
			s.hasSnapshot = true
		}
	}

	switch task.BillingMode {
	case imageBillingModeToken:
		if !s.hasSnapshot {
			// 冻结的价丢了（不该发生）：宁可不改账，也不拿现价瞎算。
			return s
		}
		if s.usage != nil {
			applyImagesUsageCost(&s.snapshot, s.usage)
		}
		s.Cost = s.snapshot.FinalInputCost + s.snapshot.FinalOutputCost + s.snapshot.FinalCacheReadCost + s.snapshot.FinalCacheWriteCost
		s.Delta = s.Cost - task.Cost
	case imageBillingModePerImage:
		if s.hasSnapshot {
			stats := model.StatsMetrics{}
			applyPerImageCost(&s.snapshot, &stats, task.Cost)
		}
	}
	return s
}

// Charge 把差额落到用户账上（补扣或退回）。只能由赢得终态 CAS 的那一方调用一次。
func (s ImageTaskSettlement) Charge(ctx context.Context, userID int) error {
	switch {
	case s.Delta > 0:
		return op.UserRecordUsage(userID, s.Delta, ctx)
	case s.Delta < 0:
		return op.UserRefundUsage(userID, -s.Delta, ctx)
	default:
		return nil
	}
}
