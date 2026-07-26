package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/price"
	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/utils/log"
	"github.com/bestruirui/octopus/internal/utils/tokenizer"
)

const metricsPersistTimeout = 30 * time.Second

// RelayMetrics 负责最终的日志收集与持久化
type RelayMetrics struct {
	APIKeyID        int
	UserID          int
	RequestIP       string
	RequestModel    string
	RequestEndpoint string
	RequestPath     string
	StartTime       time.Time

	// 首 Token 时间
	FirstTokenTime time.Time

	// 诊断(仅 diagnostic_mode 消费): 发出上游请求 / 收到上游响应头 的时刻(仅最终尝试)。
	// ResponseHeaderTime-RequestSentTime = "等响应头"(首包)耗时——现有 metrics 缺这段,
	// 首包卡死(上游连上却不发头)时只有它能把"卡在哪一层"照出来。
	RequestSentTime    time.Time
	ResponseHeaderTime time.Time

	// 请求和响应内容
	InternalRequest  *transformerModel.InternalLLMRequest
	InternalResponse *transformerModel.InternalLLMResponse

	// 统计指标
	ActualModel   string
	Stats         model.StatsMetrics
	SessionKey    string
	SessionSource string

	// ChannelKeyRemark 记录最终(成功/已提交)尝试所用渠道 Key 的备注, 仅供日志展示。
	// 重试会切换 Key, 因此由 relay 在成功/已写出下游的那一刻回填最终 Key 的备注。
	ChannelKeyRemark string

	// 参数覆盖
	ParamOverride   string
	AccessPlan      *model.AccessPlan
	AccessRouteRule *model.AccessRouteRule
	AccessRouteUsed bool
	BillingSnapshot model.AccessPlanBillingSnapshot
	PromptSnapshot  promptOverrideSnapshot
}

func NewRelayMetrics(apiKeyID int, userID int, requestIP string, requestModel string, req *transformerModel.InternalLLMRequest) *RelayMetrics {
	return &RelayMetrics{
		APIKeyID:        apiKeyID,
		UserID:          userID,
		RequestIP:       requestIP,
		RequestModel:    requestModel,
		StartTime:       time.Now(),
		InternalRequest: req,
	}
}

func (m *RelayMetrics) SetFirstTokenTime(t time.Time) {
	m.FirstTokenTime = t
}

func (m *RelayMetrics) SetAccessPlan(plan *model.AccessPlan, rule *model.AccessRouteRule, routeUsed bool) {
	m.AccessPlan = plan
	m.AccessRouteRule = rule
	m.AccessRouteUsed = routeUsed
}

func (m *RelayMetrics) SetRequestEndpoint(endpoint string, path string) {
	m.RequestEndpoint = cleanRelayEndpointName(endpoint)
	m.RequestPath = strings.TrimSpace(path)
}

func (m *RelayMetrics) SetClientSession(info clientSessionInfo) {
	m.SessionKey = info.Key
	m.SessionSource = info.Source
}

func (m *RelayMetrics) SetPromptOverrideSnapshot(snapshot promptOverrideSnapshot) {
	m.PromptSnapshot = snapshot
}

func (m *RelayMetrics) SetInternalResponse(resp *transformerModel.InternalLLMResponse, actualModel string) {
	m.InternalResponse = resp
	m.ActualModel = actualModel

	if resp == nil {
		return
	}

	usage := resp.Usage
	// Local usage estimate. When the upstream omits usage entirely, or reports zero
	// completion tokens on a response that actually delivered content, count the
	// tokens locally via the tokenizer so a successful response is not logged/billed
	// as 0 tokens — mirroring new-api's patchGeminiZeroCompletionUsage (local
	// ResponseText2Usage). The estimate is applied only to the local Stats/billing
	// used for logging; resp.Usage is left untouched so usageAuditFromInternalResponse
	// still flags usage_missing_reason (the count is estimated, not upstream-authoritative).
	if usage == nil || usage.CompletionTokens == 0 {
		if est := estimateCompletionTokens(resp, actualModel); est > 0 {
			estimated := &transformerModel.Usage{}
			if usage != nil {
				*estimated = *usage
			}
			estimated.CompletionTokens = int64(est)
			if estimated.PromptTokens == 0 && m.InternalRequest != nil {
				estimated.PromptTokens = int64(estimatePromptTokens(m.InternalRequest, actualModel))
			}
			usage = estimated
		}
	}

	if usage == nil {
		return
	}

	m.Stats.InputToken = usage.PromptTokens
	m.Stats.OutputToken = usage.CompletionTokens
	m.Stats.CacheHitToken, m.Stats.CacheWriteToken, m.Stats.CacheInputToken = usageCacheStats(usage)

	if usage.PromptTokensDetails == nil {
		usage.PromptTokensDetails = &transformerModel.PromptTokensDetails{
			CachedTokens: 0,
		}
	}
	m.BillingSnapshot = m.buildBillingSnapshot(actualModel, usage)
	m.Stats.InputCost = m.BillingSnapshot.FinalInputCost + m.BillingSnapshot.FinalCacheReadCost + m.BillingSnapshot.FinalCacheWriteCost
	m.Stats.OutputCost = m.BillingSnapshot.FinalOutputCost
}

// estimateCompletionTokens counts the tokens of a response's assistant output
// (text + reasoning + tool-call arguments) via the local tokenizer, used only as a
// fallback when the upstream did not report completion tokens.
func estimateCompletionTokens(resp *transformerModel.InternalLLMResponse, model string) int {
	if resp == nil {
		return 0
	}
	var b strings.Builder
	for _, ch := range resp.Choices {
		if ch.Message != nil {
			writeMessageTextForEstimate(&b, ch.Message)
		}
	}
	if b.Len() == 0 {
		return 0
	}
	return tokenizer.CountTokens(b.String(), model)
}

// estimatePromptTokens counts the tokens of a request's messages via the local
// tokenizer, used only when the upstream reported no prompt tokens alongside a
// locally-estimated completion.
func estimatePromptTokens(req *transformerModel.InternalLLMRequest, model string) int {
	if req == nil {
		return 0
	}
	var b strings.Builder
	for i := range req.Messages {
		writeMessageTextForEstimate(&b, &req.Messages[i])
	}
	if b.Len() == 0 {
		return 0
	}
	return tokenizer.CountTokens(b.String(), model)
}

func writeMessageTextForEstimate(b *strings.Builder, msg *transformerModel.Message) {
	if msg == nil {
		return
	}
	if msg.Content.Content != nil {
		b.WriteString(*msg.Content.Content)
		b.WriteByte('\n')
	}
	for _, p := range msg.Content.MultipleContent {
		if p.Text != nil {
			b.WriteString(*p.Text)
			b.WriteByte('\n')
		}
	}
	if rc := msg.GetReasoningContent(); rc != "" {
		b.WriteString(rc)
		b.WriteByte('\n')
	}
	for _, tc := range msg.ToolCalls {
		b.WriteString(tc.Function.Name)
		b.WriteString(tc.Function.Arguments)
		b.WriteByte('\n')
	}
}

func (m *RelayMetrics) currentBillingSnapshot(actualModel string) model.AccessPlanBillingSnapshot {
	if m.BillingSnapshot.BillingModelName != "" || m.BillingSnapshot.AccessPlanID != 0 {
		return m.BillingSnapshot
	}
	return m.buildBillingSnapshot(actualModel, nil)
}

func (m *RelayMetrics) buildBillingSnapshot(actualModel string, usage *transformerModel.Usage) model.AccessPlanBillingSnapshot {
	upstreamModel := strings.TrimSpace(actualModel)
	if upstreamModel == "" {
		upstreamModel = m.RequestModel
	}

	snapshot := model.AccessPlanBillingSnapshot{
		DefaultMultiplier: 1,
		ModelMultiplier:   1,
		FinalMultiplier:   1,
	}

	billingSource := model.AccessBillingModelSourceRequest
	if m.AccessPlan != nil {
		snapshot.AccessPlanID = m.AccessPlan.ID
		snapshot.AccessPlanSlug = m.AccessPlan.Slug
		snapshot.AccessPlanName = m.AccessPlan.DisplayName
		if m.AccessPlan.RouteProfile != nil {
			snapshot.RouteProfileID = m.AccessPlan.RouteProfile.ID
			snapshot.RouteProfileName = m.AccessPlan.RouteProfile.Name
		}
		if m.AccessPlan.BillingProfile != nil {
			snapshot.BillingProfileID = m.AccessPlan.BillingProfile.ID
			snapshot.BillingProfileName = m.AccessPlan.BillingProfile.Name
			snapshot.DefaultMultiplier = positiveMultiplier(m.AccessPlan.BillingProfile.DefaultMultiplier)
		}
	}
	if m.AccessRouteRule != nil {
		if m.AccessRouteRule.BillingModelSource != "" {
			billingSource = m.AccessRouteRule.BillingModelSource
		}
	}

	switch billingSource {
	case model.AccessBillingModelSourceRequest:
		snapshot.BillingModelName = m.RequestModel
	case model.AccessBillingModelSourceOverride:
		if m.AccessRouteRule != nil && strings.TrimSpace(m.AccessRouteRule.BillingModelOverride) != "" {
			snapshot.BillingModelName = strings.TrimSpace(m.AccessRouteRule.BillingModelOverride)
		} else {
			snapshot.BillingModelName = upstreamModel
		}
	default:
		snapshot.BillingModelName = upstreamModel
	}
	if snapshot.BillingModelName == "" {
		snapshot.BillingModelName = m.RequestModel
	}
	snapshot.BillingModelSource = billingSource

	if m.AccessPlan != nil && m.AccessPlan.BillingProfile != nil {
		snapshot.ModelMultiplier = billingModelMultiplier(m.AccessPlan.BillingProfile.ModelRules, snapshot.BillingModelName)
	}
	snapshot.FinalMultiplier = snapshot.DefaultMultiplier * snapshot.ModelMultiplier

	modelPrice := price.GetLLMPrice(snapshot.BillingModelName)
	if modelPrice == nil {
		return snapshot
	}
	snapshot.BaseInputPrice = modelPrice.Input
	snapshot.BaseOutputPrice = modelPrice.Output
	snapshot.BaseCacheReadPrice = modelPrice.CacheRead
	snapshot.BaseCacheWritePrice = modelPrice.CacheWrite

	if usage == nil {
		return snapshot
	}
	if usage.PromptTokensDetails == nil {
		usage.PromptTokensDetails = &transformerModel.PromptTokensDetails{}
	}

	inputTokens := usage.PromptTokens
	cacheReadTokens := usage.PromptTokensDetails.CachedTokens
	cacheWriteTokens := usage.CacheCreationInputTokens
	if !usage.AnthropicUsage && !usage.SeparateCacheInputTokens {
		inputTokens -= cacheReadTokens
		if inputTokens < 0 {
			inputTokens = 0
		}
	}

	snapshot.FinalInputCost = float64(inputTokens) * snapshot.BaseInputPrice * snapshot.FinalMultiplier * 1e-6
	snapshot.FinalOutputCost = float64(usage.CompletionTokens) * snapshot.BaseOutputPrice * snapshot.FinalMultiplier * 1e-6
	snapshot.FinalCacheReadCost = float64(cacheReadTokens) * snapshot.BaseCacheReadPrice * snapshot.FinalMultiplier * 1e-6
	snapshot.FinalCacheWriteCost = float64(cacheWriteTokens) * snapshot.BaseCacheWritePrice * snapshot.FinalMultiplier * 1e-6
	return snapshot
}

func billingModelMultiplier(rules []model.AccessBillingModelRule, billingModel string) float64 {
	for _, rule := range rules {
		if !rule.Enabled {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(rule.ModelName), strings.TrimSpace(billingModel)) {
			return positiveMultiplier(rule.Multiplier)
		}
	}
	return 1
}

func positiveMultiplier(value float64) float64 {
	if value <= 0 {
		return 1
	}
	return value
}

func applyBillingSnapshotToRelayLog(relayLog *model.RelayLog, snapshot model.AccessPlanBillingSnapshot) {
	if relayLog == nil {
		return
	}
	relayLog.AccessPlanID = snapshot.AccessPlanID
	relayLog.AccessPlanSlug = snapshot.AccessPlanSlug
	relayLog.AccessPlanName = snapshot.AccessPlanName
	relayLog.RouteProfileID = snapshot.RouteProfileID
	relayLog.RouteProfileName = snapshot.RouteProfileName
	relayLog.BillingProfileID = snapshot.BillingProfileID
	relayLog.BillingProfileName = snapshot.BillingProfileName
	relayLog.BillingModel = snapshot.BillingModelName
	relayLog.BaseInputPrice = snapshot.BaseInputPrice
	relayLog.BaseOutputPrice = snapshot.BaseOutputPrice
	relayLog.BaseCacheReadPrice = snapshot.BaseCacheReadPrice
	relayLog.BaseCacheWritePrice = snapshot.BaseCacheWritePrice
	relayLog.DefaultMultiplier = snapshot.DefaultMultiplier
	relayLog.ModelMultiplier = snapshot.ModelMultiplier
	relayLog.FinalMultiplier = snapshot.FinalMultiplier
	relayLog.FinalInputCost = snapshot.FinalInputCost
	relayLog.FinalOutputCost = snapshot.FinalOutputCost
	relayLog.FinalCacheReadCost = snapshot.FinalCacheReadCost
	relayLog.FinalCacheWriteCost = snapshot.FinalCacheWriteCost
}

func (m *RelayMetrics) Save(ctx context.Context, success bool, err error, attempts []model.ChannelAttempt) {
	duration := time.Since(m.StartTime)
	persistCtx, cancel := metricsPersistContext()
	defer cancel()

	globalStats := model.StatsMetrics{
		WaitTime:        duration.Milliseconds(),
		InputToken:      m.Stats.InputToken,
		OutputToken:     m.Stats.OutputToken,
		InputCost:       m.Stats.InputCost,
		OutputCost:      m.Stats.OutputCost,
		CacheHitToken:   m.Stats.CacheHitToken,
		CacheWriteToken: m.Stats.CacheWriteToken,
		CacheInputToken: m.Stats.CacheInputToken,
	}
	if success {
		globalStats.RequestSuccess = 1
	} else if !isClientAbortError(err) {
		globalStats.RequestFailed = 1
	}

	channelID, channelName := finalChannel(attempts)
	op.StatsTotalUpdate(globalStats)
	op.StatsHourlyUpdate(globalStats)
	op.StatsDailyUpdate(persistCtx, globalStats)
	op.StatsAPIKeyUpdate(m.APIKeyID, globalStats)
	op.StatsChannelUpdate(channelID, globalStats)
	if err := op.UserRecordRelayIP(m.UserID, m.RequestIP, m.StartTime.Unix(), persistCtx); err != nil {
		log.Warnf("failed to record user relay ip: %v", err)
	}
	if success {
		if err := op.UserRecordUsage(m.UserID, globalStats.InputCost+globalStats.OutputCost, persistCtx); err != nil {
			log.Warnf("failed to record user usage: %v", err)
		}
	}

	errorStatus, errorCode, errorStrategy, _ := relayErrorDetails(err)
	log.Infof("relay complete: model=%s, channel=%d(%s), success=%t, duration=%dms, input_token=%d, output_token=%d, cache_hit_token=%d, cache_rate=%.2f%%, input_cost=%f, output_cost=%f, total_cost=%f, attempts=%d, error_status=%d, error_code=%s, error_strategy=%s",
		m.RequestModel, channelID, channelName, success, duration.Milliseconds(),
		m.Stats.InputToken, m.Stats.OutputToken,
		m.Stats.CacheHitToken, cacheHitRate(m.Stats.CacheHitToken, m.Stats.CacheInputToken)*100,
		m.Stats.InputCost, m.Stats.OutputCost, m.Stats.InputCost+m.Stats.OutputCost,
		len(attempts), errorStatus, errorCode, errorStrategy)

	m.logDiagnostics(success, duration, attempts, channelID, channelName)

	m.saveLog(persistCtx, err, duration, attempts, channelID, channelName)
}

// logDiagnostics 在 diagnostic_mode 开启时额外打印一行各阶段耗时 + 逐次尝试原因,
// 便于排障"卡在哪一层"(等响应头/首字/生成)。关闭时的每请求成本仅一次(带缓存的)设置读取,
// 与 debug_load_balancer 同源。所有字段皆为内部计时/下游日志, 不触上游出站字节(shape 安全)。
// header_wait / first_token / gen_start 为 -1 表示该阶段未测到(如请求在拿到响应头前就失败)。
func (m *RelayMetrics) logDiagnostics(success bool, duration time.Duration, attempts []model.ChannelAttempt, channelID int, channelName string) {
	enabled, err := op.SettingGetBool(model.SettingKeyDiagnosticMode)
	if err != nil || !enabled {
		return
	}

	headerWaitMs := int64(-1)
	if !m.RequestSentTime.IsZero() && !m.ResponseHeaderTime.IsZero() {
		headerWaitMs = m.ResponseHeaderTime.Sub(m.RequestSentTime).Milliseconds()
	}
	firstTokenMs := int64(-1)
	if !m.FirstTokenTime.IsZero() {
		firstTokenMs = m.FirstTokenTime.Sub(m.StartTime).Milliseconds()
	}
	genStartMs := int64(-1)
	if !m.FirstTokenTime.IsZero() && !m.ResponseHeaderTime.IsZero() {
		genStartMs = m.FirstTokenTime.Sub(m.ResponseHeaderTime).Milliseconds()
	}

	var b strings.Builder
	for i, a := range attempts {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "#%d:ch%d(%s)/%s/%dms", a.AttemptNum, a.ChannelID, a.ChannelName, a.Status, a.Duration)
		if msg := strings.TrimSpace(a.Msg); msg != "" {
			fmt.Fprintf(&b, "[%s]", msg)
		}
	}

	log.Infof("[diag] model=%s channel=%d(%s) success=%t total=%dms header_wait=%dms first_token=%dms gen_start=%dms attempts=%d | %s",
		m.RequestModel, channelID, channelName, success, duration.Milliseconds(),
		headerWaitMs, firstTokenMs, genStartMs, len(attempts), b.String())
}

func metricsPersistContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), metricsPersistTimeout)
}

func finalChannel(attempts []model.ChannelAttempt) (int, string) {
	var lastID int
	var lastName string
	for i := len(attempts) - 1; i >= 0; i-- {
		a := attempts[i]
		if a.Status == model.AttemptSuccess {
			return a.ChannelID, a.ChannelName
		}
		if lastID == 0 && a.ChannelID != 0 {
			lastID = a.ChannelID
			lastName = a.ChannelName
		}
	}
	return lastID, lastName
}

func routeStickyHit(attempts []model.ChannelAttempt) bool {
	for _, attempt := range attempts {
		if attempt.Sticky && attempt.Status == model.AttemptSuccess {
			return true
		}
	}
	return false
}

func usageAuditFromInternalResponse(resp *transformerModel.InternalLLMResponse, err error) (string, string) {
	if resp != nil && resp.Usage != nil {
		hitTokens, writeTokens, _ := usageCacheStats(resp.Usage)
		if resp.Usage.PromptTokens == 0 && resp.Usage.CompletionTokens == 0 && hitTokens == 0 && writeTokens == 0 {
			return model.RelayLogUsageSourceUpstream, model.RelayLogUsageMissingReasonZeroUsageReported
		}
		return model.RelayLogUsageSourceUpstream, ""
	}
	if isClientAbortError(err) {
		return model.RelayLogUsageSourceNoUsage, model.RelayLogUsageMissingReasonClientAborted
	}
	if resp == nil {
		return model.RelayLogUsageSourceNoUsage, model.RelayLogUsageMissingReasonNoInternalResponse
	}
	return model.RelayLogUsageSourceNoUsage, model.RelayLogUsageMissingReasonUpstreamUsageMissing
}

func usageAuditFromStats(stats model.StatsMetrics, usageSeen bool, err error, opaque bool) (string, string) {
	if usageSeen {
		if stats.InputToken == 0 && stats.OutputToken == 0 && stats.CacheHitToken == 0 && stats.CacheWriteToken == 0 {
			return model.RelayLogUsageSourceUpstream, model.RelayLogUsageMissingReasonZeroUsageReported
		}
		return model.RelayLogUsageSourceUpstream, ""
	}
	if isClientAbortError(err) {
		return model.RelayLogUsageSourceNoUsage, model.RelayLogUsageMissingReasonClientAborted
	}
	if opaque {
		return model.RelayLogUsageSourceNoUsage, model.RelayLogUsageMissingReasonOpaqueResponse
	}
	return model.RelayLogUsageSourceNoUsage, model.RelayLogUsageMissingReasonUpstreamUsageMissing
}

func cleanRelayEndpointName(endpoint string) string {
	endpoint = strings.TrimSpace(endpoint)
	endpoint = strings.TrimPrefix(endpoint, "/")
	endpoint = strings.TrimPrefix(endpoint, "v1/")
	endpoint = strings.TrimPrefix(endpoint, "v1beta/")
	endpoint = strings.Trim(endpoint, "/")
	endpoint = strings.ReplaceAll(endpoint, ":", "_")
	endpoint = strings.ReplaceAll(endpoint, "/", "_")
	if endpoint == "" {
		return "unknown"
	}
	return endpoint
}

func (m *RelayMetrics) saveLog(ctx context.Context, err error, duration time.Duration, attempts []model.ChannelAttempt, channelID int, channelName string) {
	actualModel := m.ActualModel
	if actualModel == "" {
		actualModel = m.RequestModel
	}
	billingSnapshot := m.currentBillingSnapshot(actualModel)

	relayLog := model.RelayLog{
		UserID:                m.UserID,
		APIKeyID:              m.APIKeyID,
		RequestIP:             m.RequestIP,
		Time:                  m.StartTime.Unix(),
		RequestEndpoint:       m.RequestEndpoint,
		RequestPath:           m.RequestPath,
		RequestModelName:      m.RequestModel,
		ChannelName:           channelName,
		ChannelId:             channelID,
		ChannelKeyRemark:      m.ChannelKeyRemark,
		ActualModelName:       actualModel,
		UseTime:               int(duration.Milliseconds()),
		Attempts:              attempts,
		TotalAttempts:         len(attempts),
		SessionKey:            m.SessionKey,
		SessionSource:         m.SessionSource,
		RouteStickyHit:        routeStickyHit(attempts),
		IsStream:              m.InternalRequest != nil && m.InternalRequest.Stream != nil && *m.InternalRequest.Stream,
		PromptOverrideMode:    string(m.PromptSnapshot.Mode),
		PromptOverrideSources: append([]string(nil), m.PromptSnapshot.Sources...),
	}
	applyBillingSnapshotToRelayLog(&relayLog, billingSnapshot)

	// 推理强度: InternalRequest 是与转发链共享的指针, 此处读到的是经 gpt-5.6 自动抬升等
	// 归一化后的有效值(见 transformer/model/model.go:ReasoningEffort)。reasoning_effort 是
	// 客户端自由透传字段(Validate 不校验), 必须按列宽 size:32 截断——否则一个超长值会让
	// PG/MySQL 批量 INSERT 失败, 而 flush 失败的坏行不排空, 会卡死整个 relay 日志落库批次。
	// 纯审计快照, 截断无害; 已知 effort(high/low/max/…) 远短于 32。
	if m.InternalRequest != nil {
		effort := m.InternalRequest.ReasoningEffort
		if r := []rune(effort); len(r) > 32 {
			effort = string(r[:32])
		}
		relayLog.ReasoningEffort = effort
	}

	if apiKey, getErr := op.APIKeyGet(m.APIKeyID, ctx); getErr == nil {
		relayLog.RequestAPIKeyName = apiKey.Name
	}
	// 用户名: 与 APIKeyGet 同样按 id 反查, 命中失败则留空(不阻断日志写入)。
	if user, getErr := op.UserGet(m.UserID); getErr == nil {
		relayLog.UserName = user.Username
	}

	// 首字时间
	if !m.FirstTokenTime.IsZero() {
		relayLog.Ftut = int(m.FirstTokenTime.Sub(m.StartTime).Milliseconds())
	}

	// Usage
	relayLog.UsageSource, relayLog.UsageMissingReason = usageAuditFromInternalResponse(m.InternalResponse, err)
	if m.InternalResponse != nil && m.InternalResponse.Usage != nil {
		relayLog.InputTokens = int(m.InternalResponse.Usage.PromptTokens)
		relayLog.OutputTokens = int(m.InternalResponse.Usage.CompletionTokens)
		cacheHit, cacheWrite, cacheInput := usageCacheStats(m.InternalResponse.Usage)
		relayLog.CacheHitTokens = int(cacheHit)
		relayLog.CacheWriteTokens = int(cacheWrite)
		relayLog.CacheWrite5mTokens = int(m.InternalResponse.Usage.CacheCreation5mInputTokens)
		relayLog.CacheWrite1hTokens = int(m.InternalResponse.Usage.CacheCreation1hInputTokens)
		relayLog.CacheInputTokens = int(cacheInput)
		relayLog.CacheHitRate = cacheHitRate(cacheHit, cacheInput)
		relayLog.Cost = m.Stats.InputCost + m.Stats.OutputCost
	}

	// 请求内容
	if m.InternalRequest != nil {
		reqJSON, jsonErr := json.Marshal(m.InternalRequest)
		if jsonErr != nil {
			relayLog.RequestContent = string(reqJSON)
		} else if m.ParamOverride == "" {
			relayLog.RequestContent = string(reqJSON)
		} else {
			var reqMap map[string]any
			if err := json.Unmarshal(reqJSON, &reqMap); err != nil {
				relayLog.RequestContent = string(reqJSON)
			} else {
				var override map[string]any
				if err := json.Unmarshal([]byte(m.ParamOverride), &override); err != nil {
					relayLog.RequestContent = string(reqJSON)
				} else {
					maps.Copy(reqMap, override)
					if finalJSON, err := json.Marshal(reqMap); err != nil {
						relayLog.RequestContent = string(reqJSON)
					} else {
						relayLog.RequestContent = string(finalJSON)
					}
				}
			}
		}
	}

	// 响应内容
	if m.InternalResponse != nil {
		respForLog := m.filterResponseForLog(m.InternalResponse)
		if respJSON, jsonErr := json.Marshal(respForLog); jsonErr == nil {
			if m.InternalResponse.Usage != nil && m.InternalResponse.Usage.AnthropicUsage {
				respStr := string(respJSON)
				old := `"usage":{`
				insert := fmt.Sprintf(`"usage":{"cache_creation_input_tokens":%d,`, m.InternalResponse.Usage.CacheCreationInputTokens)
				respJSON = []byte(strings.Replace(respStr, old, insert, 1))
			}
			relayLog.ResponseContent = string(respJSON)
		}
	}

	// 错误信息
	if err != nil {
		relayLog.Error = errSafeMessage(err)
		if status, code, strategy, ok := relayErrorDetails(err); ok {
			relayLog.ErrorStatus = status
			relayLog.ErrorCode = code
			relayLog.ErrorStrategy = strategy
		}
	}

	if logErr := op.RelayLogAdd(ctx, relayLog); logErr != nil {
		log.Warnf("failed to save relay log: %v", logErr)
	}
}

func usageCacheStats(usage *transformerModel.Usage) (hitTokens int64, writeTokens int64, inputTokens int64) {
	if usage == nil {
		return 0, 0, 0
	}
	if usage.PromptTokensDetails != nil {
		hitTokens = usage.PromptTokensDetails.CachedTokens
	}
	writeTokens = usage.CacheCreationInputTokens
	inputTokens = usage.PromptTokens
	if usage.AnthropicUsage || usage.SeparateCacheInputTokens {
		inputTokens += hitTokens + writeTokens
	}
	if inputTokens < hitTokens+writeTokens {
		inputTokens = hitTokens + writeTokens
	}
	return hitTokens, writeTokens, inputTokens
}

func cacheHitRate(hitTokens, inputTokens int64) float64 {
	if hitTokens <= 0 || inputTokens <= 0 {
		return 0
	}
	return float64(hitTokens) / float64(inputTokens)
}

// filterResponseForLog 创建响应的浅拷贝，过滤掉 images、MultipleContent 中的图片数据和 Audio.Data 以减少存储压力
func (m *RelayMetrics) filterResponseForLog(resp *transformerModel.InternalLLMResponse) *transformerModel.InternalLLMResponse {
	if resp == nil {
		return nil
	}

	filterMsg := func(msg *transformerModel.Message) *transformerModel.Message {
		if msg == nil {
			return nil
		}
		c := *msg
		c.Images = nil
		if len(c.Content.MultipleContent) > 0 {
			parts := make([]transformerModel.MessageContentPart, 0, len(c.Content.MultipleContent))
			for _, p := range c.Content.MultipleContent {
				if p.Type == "image_url" && p.ImageURL != nil {
					parts = append(parts, transformerModel.MessageContentPart{
						Type:     "image_url",
						ImageURL: &transformerModel.ImageURL{URL: "[image data omitted for storage]"},
					})
				} else {
					parts = append(parts, p)
				}
			}
			c.Content = transformerModel.MessageContent{Content: c.Content.Content, MultipleContent: parts}
		}
		if c.Audio != nil && c.Audio.Data != "" {
			a := *c.Audio
			a.Data = "[audio data omitted for storage]"
			c.Audio = &a
		}
		return &c
	}

	filtered := *resp
	filtered.Choices = make([]transformerModel.Choice, len(resp.Choices))
	for i, choice := range resp.Choices {
		filtered.Choices[i] = choice
		filtered.Choices[i].Message = filterMsg(choice.Message)
		filtered.Choices[i].Delta = filterMsg(choice.Delta)
	}
	return &filtered
}
