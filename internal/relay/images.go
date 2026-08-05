package relay

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"time"

	"github.com/bestruirui/octopus/internal/helper"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/price"
	"github.com/bestruirui/octopus/internal/relay/balancer"
	"github.com/bestruirui/octopus/internal/relay/bodycache"
	"github.com/bestruirui/octopus/internal/server/resp"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/bestruirui/octopus/internal/utils/log"
	"github.com/bestruirui/octopus/internal/utils/safe"
	"github.com/bestruirui/octopus/internal/utils/xurl"
	"github.com/gin-gonic/gin"
)

const imagesUpstreamErrorBodyLimit = 16 * 1024

// streamKeepaliveComment 是注入到 Images SSE 透传流的 keepalive 心跳。Images 走
// OpenAI 兼容流，与主路径 handleStreamResponse 的 OpenAI 心跳一致使用 SSE 注释行
// (":\n\n")：合法、被客户端忽略、不污染数据流、也不参与 usage/id 扫描。
var streamKeepaliveComment = []byte(":\n\n")

// ImagesHandler 是 OpenAI Images API 的统一 relay 入口。
// endpoint 形如：/images/generations、/images/edits、/images/variations（不含 /v1 前缀）。
func ImagesHandler(endpoint string, c *gin.Context) {
	ctx := c.Request.Context()

	apiKeyID := c.GetInt("api_key_id")
	userID := c.GetInt("user_id")

	// 缓存请求体，支持多次重试重放
	bc, err := bodycache.New(c.Request.Body)
	if err != nil {
		var tooLarge *bodycache.BodyTooLargeError
		if errors.As(err, &tooLarge) {
			resp.Error(c, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	defer func() {
		if cerr := bc.Close(); cerr != nil {
			log.Warnf("failed to close images body cache: %v", cerr)
		}
	}()

	contentType := c.GetHeader("Content-Type")
	isMultipart := strings.Contains(strings.ToLower(contentType), "multipart/form-data")

	// 解析 requestModel 与 stream（严格模式：model 必填）
	var (
		requestModel string
		stream       bool
		boundary     string
		jsonPayload  map[string]any
	)
	if isMultipart {
		_, params, perr := mime.ParseMediaType(contentType)
		if perr != nil {
			resp.Error(c, http.StatusBadRequest, "invalid multipart content-type")
			return
		}
		boundary = strings.TrimSpace(params["boundary"])
		if boundary == "" {
			resp.Error(c, http.StatusBadRequest, "invalid multipart boundary")
			return
		}
		m, s, perr := parseMultipartModelAndStream(bc, boundary)
		if perr != nil {
			resp.Error(c, http.StatusBadRequest, perr.Error())
			return
		}
		requestModel = m
		stream = s
	} else {
		payload, m, s, perr := parseJSONModelAndStream(bc)
		if perr != nil {
			resp.Error(c, http.StatusBadRequest, perr.Error())
			return
		}
		jsonPayload = payload
		requestModel = m
		stream = s
	}

	// supported_models 校验（复用 APIKeyAuth 注入）
	supportedModels := strings.TrimSpace(c.GetString("supported_models"))
	if !op.IsModelSupported(supportedModels, requestModel) {
		resp.Error(c, http.StatusBadRequest, "model not supported")
		return
	}

	// 获取通道分组
	routeResult, status, message, err := selectRouteGroup(c, apiKeyID, requestModel)
	if err != nil || status != 0 {
		if message == "" && err != nil {
			message = err.Error()
		}
		resp.Error(c, status, message)
		return
	}
	group := enrichGroupForSmartRouting(ctx, routeResult.Group, stream)
	// Image generation is stateless per request, so session stickiness is derived from
	// explicit client session headers only. Unlike raw_protocol we intentionally do NOT
	// add a raw-body fingerprint fallback here: a body hash would just give every
	// one-shot image request its own throwaway sticky slot with no continuity benefit,
	// so header-less image traffic keeps the api-key+model affinity (warmer channels).
	clientSession := deriveClientSessionInfo(c.Request.Header, nil)
	clientSessionKey := clientSession.Key

	// 创建迭代器（策略排序 + 粘性优先）
	stickyEnabled := routeStickyEnabled(group.Mode, clientSession.Source)
	iter := balancer.NewIteratorWithSession(group, apiKeyID, requestModel, clientSessionKey, stickyEnabled)
	if iter.Len() == 0 {
		resp.Error(c, http.StatusServiceUnavailable, "no available channel")
		return
	}

	// 初始化 Metrics（Images 独立，避免 b64_json 内存膨胀）
	metrics := newImagesRelayMetrics(apiKeyID, userID, c.GetString("request_ip"), requestModel, imageEndpointName(endpoint), c.Request.URL.Path)
	metrics.SetAccessPlan(routeResult.AccessPlan, routeResult.AccessRouteRule, routeResult.AccessRouteUsed)
	metrics.SetClientSession(clientSession)
	metrics.RequestContent = buildImagesRequestContentForLog(isMultipart, bc, jsonPayload)

	var (
		lastErr          error
		allAttempts      []model.ChannelAttempt
		triedReturnGroup bool
	)

runIterator:
	for iter.Next() {
		select {
		case <-ctx.Done():
			log.Infof("request context canceled, stopping retry")
			metrics.Save(ctx, false, context.Canceled, append(allAttempts, iter.Attempts()...))
			return
		default:
		}

		item := iter.Item()

		// 获取通道
		channel, err := op.ChannelGet(item.ChannelID, ctx)
		if err != nil {
			log.Warnf("failed to get channel %d: %v", item.ChannelID, err)
			iter.Skip(item.ChannelID, 0, fmt.Sprintf("channel_%d", item.ChannelID), fmt.Sprintf("channel not found: %v", err))
			lastErr = err
			continue
		}
		if !channel.Enabled {
			iter.Skip(channel.ID, 0, channel.Name, "channel disabled")
			continue
		}

		// Images can either be OpenAI-compatible pass-through (OpenAI/Custom/xAI-style
		// routers) or Gemini native adaptation (generateContent / Imagen predict).
		if !isImagesEndpointCompatibleChannelType(channel.Type) {
			iter.Skip(channel.ID, 0, channel.Name, fmt.Sprintf("unsupported channel type: %d", channel.Type))
			continue
		}

		availableKeys := channel.GetAvailableChannelKeys()
		if len(availableKeys) == 0 {
			iter.Skip(channel.ID, 0, channel.Name, "no available key")
			continue
		}
		if stickyKeyID := iter.StickyKeyIDForCurrentChannel(channel.ID); stickyKeyID > 0 {
			availableKeys = balancer.PrioritizeChannelKeysByHealth(availableKeys, channel.ID, item.ModelName, stickyKeyID)
		} else {
			availableKeys = balancer.PrioritizeChannelKeysByHealth(availableKeys, channel.ID, item.ModelName, 0)
		}

		for keyIndex, usedKey := range availableKeys {
			// 熔断检查（熔断 key 使用 actualModel=item.ModelName）
			if iter.SkipCircuitBreak(channel.ID, usedKey.ID, channel.Name) {
				continue
			}

			log.Infof("images request model %s, mode: %d, forwarding to channel: %s model: %s (attempt %d/%d, key %d/%d, sticky=%t, stream=%t)",
				requestModel, group.Mode, channel.Name, item.ModelName,
				iter.Index()+1, iter.Len(), keyIndex+1, len(availableKeys), iter.IsSticky(), stream)

			span := iter.StartAttempt(channel.ID, usedKey.ID, channel.Name)
			recordAttemptProxy(span, channel)

			// 尝试一次转发
			var (
				statusCode int
				written    bool
				usage      *imagesUsage
				upstreamCT string
				fwdErr     error
			)
			func() {
				finishRuntimeAttempt := balancer.BeginRuntimeAttempt(channel.ID, usedKey.ID, item.ModelName)
				defer finishRuntimeAttempt()
				if channel.Type == outbound.OutboundTypeGemini {
					statusCode, written, usage, upstreamCT, fwdErr = geminiImagesAttempt(ctx, endpoint, c, isMultipart, jsonPayload, stream, channel, usedKey.ChannelKey, metrics, item.ModelName)
				} else {
					statusCode, written, usage, upstreamCT, fwdErr = imagesAttempt(ctx, endpoint, c, bc, isMultipart, boundary, jsonPayload, stream, channel, usedKey.ChannelKey, group.FirstTokenTimeOut, metrics, item.ModelName)
				}
			}()

			usedAt := time.Now().Unix()

			if fwdErr == nil {
				// ====== 成功 ======
				metrics.ActualModel = item.ModelName
				if usage != nil {
					metrics.SetUsageFromImages(item.ModelName, *usage)
				}
				metrics.ResponseContent = buildImagesResponseContentForLog(stream, upstreamCT, usage)

				costDelta := metrics.Stats.InputCost + metrics.Stats.OutputCost
				op.ChannelKeyRecordUse(usedKey, statusCode, usedAt, costDelta)

				span.End(model.AttemptSuccess, statusCode, "")
				balancer.RecordRuntimeSuccess(channel.ID, usedKey.ID, item.ModelName, balancer.AttemptRuntimeMetrics{
					Duration:     span.Duration(),
					FirstToken:   firstTokenDurationSince(metrics.FirstToken, span.StartedAt()),
					OutputTokens: metrics.Stats.OutputToken,
					Stream:       stream,
				})

				// Channel 维度统计
				op.StatsChannelUpdate(channel.ID, model.StatsMetrics{
					WaitTime:       span.Duration().Milliseconds(),
					RequestSuccess: 1,
				})

				// 熔断器：记录成功
				balancer.RecordSuccess(channel.ID, usedKey.ID, item.ModelName)
				// 会话保持：更新粘性记录
				if stickyEnabled {
					balancer.SetStickyWithSessionKey(apiKeyID, requestModel, clientSessionKey, channel.ID, usedKey.ID)
				}

				metrics.Save(ctx, true, nil, append(allAttempts, iter.Attempts()...))
				return
			}

			// ====== 失败 ======
			recordStatusCode := attemptStatusCode(statusCode, fwdErr)
			op.ChannelKeyRecordUse(usedKey, recordStatusCode, usedAt, 0)
			span.End(model.AttemptFailed, recordStatusCode, auditErrorMessage(fwdErr))

			// Channel 维度统计
			breakerCounted := shouldRecordBreakerFailure(recordStatusCode, fwdErr)
			if breakerCounted {
				retryAfter, _ := retryAfterFromError(fwdErr)
				balancer.RecordRuntimeFailure(channel.ID, usedKey.ID, item.ModelName, recordStatusCode, span.Duration(), retryAfter)
			}
			channelStats := model.StatsMetrics{WaitTime: span.Duration().Milliseconds()}
			if breakerCounted {
				channelStats.RequestFailed = 1
			}
			op.StatsChannelUpdate(channel.ID, channelStats)

			// 熔断器：记录失败
			if breakerCounted {
				balancer.RecordFailureWithStatus(channel.ID, usedKey.ID, item.ModelName, recordStatusCode)
			}

			if written || c.Writer.Written() {
				metrics.Save(ctx, false, fwdErr, append(allAttempts, iter.Attempts()...))
				return
			}

			lastErr = fmt.Errorf("channel %s failed: %w", channel.Name, fwdErr)
			if !shouldTryNextChannelKey(recordStatusCode) {
				break
			}
		}
	}

	// 所有通道都失败
	allAttempts = append(allAttempts, iter.Attempts()...)
	if shouldReturnToOriginalGroup(routeResult, triedReturnGroup) {
		triedReturnGroup = true
		fallbackGroup, err := op.GroupGetEnabledMap(requestModel, ctx)
		if err != nil {
			lastErr = err
		} else {
			fallbackGroup = enrichGroupForSmartRouting(ctx, fallbackGroup, stream)
			stickyEnabled = routeStickyEnabled(fallbackGroup.Mode, clientSession.Source)
			fallbackIter := balancer.NewIteratorWithSession(fallbackGroup, apiKeyID, requestModel, clientSessionKey, stickyEnabled)
			if fallbackIter.Len() > 0 {
				group = fallbackGroup
				iter = fallbackIter
				goto runIterator
			}
		}
	}

	finalErr := lastErr
	if finalErr == nil {
		finalErr = routeSelectionErrorFromAttempts(allAttempts)
	}
	metrics.Save(ctx, false, finalErr, allAttempts)
	status, code, message := relayErrorResponse(finalErr)
	resp.ErrorWithCode(c, status, code, message)
}

type imagesUsage struct {
	InputTokens              int                        `json:"input_tokens"`
	OutputTokens             int                        `json:"output_tokens"`
	PromptTokens             int                        `json:"prompt_tokens"`
	CompletionTokens         int                        `json:"completion_tokens"`
	TotalTokens              int                        `json:"total_tokens"`
	PromptTokensDetails      *imagesPromptTokensDetails `json:"prompt_tokens_details,omitempty"`
	CacheCreationInputTokens int                        `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int                        `json:"cache_read_input_tokens,omitempty"`
	InputTokensDetails       *imagesInputTokensDetails  `json:"input_tokens_details,omitempty"`
	OutputTokensDetails      *imagesOutputTokensDetails `json:"output_tokens_details,omitempty"`
	CacheDetails             *imagesGenericCacheDetails `json:"cache_details,omitempty"`
}

type imagesPromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type imagesInputTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type imagesOutputTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

type imagesGenericCacheDetails struct {
	ReadTokens  int `json:"read_tokens"`
	WriteTokens int `json:"write_tokens"`
}

func (u imagesUsage) InputTokenCount() int {
	if u.InputTokens > 0 {
		return u.InputTokens
	}
	return u.PromptTokens
}

func (u imagesUsage) OutputTokenCount() int {
	if u.OutputTokens > 0 {
		return u.OutputTokens
	}
	return u.CompletionTokens
}

func (u imagesUsage) CacheReadTokenCount() int {
	if u.CacheReadInputTokens > 0 {
		return u.CacheReadInputTokens
	}
	if u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens > 0 {
		return u.PromptTokensDetails.CachedTokens
	}
	if u.InputTokensDetails != nil && u.InputTokensDetails.CachedTokens > 0 {
		return u.InputTokensDetails.CachedTokens
	}
	if u.CacheDetails != nil && u.CacheDetails.ReadTokens > 0 {
		return u.CacheDetails.ReadTokens
	}
	return 0
}

func (u imagesUsage) CacheWriteTokenCount() int {
	if u.CacheCreationInputTokens > 0 {
		return u.CacheCreationInputTokens
	}
	if u.CacheDetails != nil && u.CacheDetails.WriteTokens > 0 {
		return u.CacheDetails.WriteTokens
	}
	return 0
}

func (u imagesUsage) HasSeparateCacheInputTokens() bool {
	return u.CacheReadInputTokens > 0 || u.CacheCreationInputTokens > 0
}

func (u imagesUsage) CacheInputTokenCount() int {
	input := u.InputTokenCount()
	cacheTokens := u.CacheReadTokenCount() + u.CacheWriteTokenCount()
	if u.HasSeparateCacheInputTokens() {
		return input + cacheTokens
	}
	if input < cacheTokens {
		return cacheTokens
	}
	return input
}

type imagesRelayMetrics struct {
	APIKeyID        int
	UserID          int
	RequestIP       string
	RequestModel    string
	RequestEndpoint string
	RequestPath     string
	ActualModel     string
	StartTime       time.Time
	FirstToken      time.Time

	Stats model.StatsMetrics

	RequestContent  string
	ResponseContent string
	AccessPlan      *model.AccessPlan
	AccessRouteRule *model.AccessRouteRule
	AccessRouteUsed bool
	BillingSnapshot model.AccessPlanBillingSnapshot
	SessionKey      string
	SessionSource   string
	UsageSeen       bool
}

func newImagesRelayMetrics(apiKeyID int, userID int, requestIP string, requestModel string, requestEndpoint string, requestPath string) *imagesRelayMetrics {
	return &imagesRelayMetrics{
		APIKeyID:        apiKeyID,
		UserID:          userID,
		RequestIP:       requestIP,
		RequestModel:    requestModel,
		RequestEndpoint: cleanRelayEndpointName(requestEndpoint),
		RequestPath:     strings.TrimSpace(requestPath),
		StartTime:       time.Now(),
	}
}

func imageEndpointName(endpoint string) string {
	name := strings.Trim(strings.TrimSpace(endpoint), "/")
	if strings.HasPrefix(name, "images/") {
		name = strings.TrimPrefix(name, "images/")
	}
	if name == "" {
		return "images"
	}
	return "images_" + strings.ReplaceAll(name, "/", "_")
}

func (m *imagesRelayMetrics) SetFirstTokenTime(t time.Time) {
	if m.FirstToken.IsZero() {
		m.FirstToken = t
	}
}

func (m *imagesRelayMetrics) SetAccessPlan(plan *model.AccessPlan, rule *model.AccessRouteRule, routeUsed bool) {
	m.AccessPlan = plan
	m.AccessRouteRule = rule
	m.AccessRouteUsed = routeUsed
}

func (m *imagesRelayMetrics) SetClientSession(info clientSessionInfo) {
	m.SessionKey = info.Key
	m.SessionSource = info.Source
}

func (m *imagesRelayMetrics) SetUsageFromImages(actualModel string, u imagesUsage) {
	m.ActualModel = actualModel
	m.UsageSeen = true
	m.Stats.InputToken = int64(u.InputTokenCount())
	m.Stats.OutputToken = int64(u.OutputTokenCount())
	m.Stats.CacheHitToken = int64(u.CacheReadTokenCount())
	m.Stats.CacheWriteToken = int64(u.CacheWriteTokenCount())
	m.Stats.CacheInputToken = int64(u.CacheInputTokenCount())

	m.BillingSnapshot = m.buildBillingSnapshot(actualModel, &u)
	m.Stats.InputCost = m.BillingSnapshot.FinalInputCost + m.BillingSnapshot.FinalCacheReadCost + m.BillingSnapshot.FinalCacheWriteCost
	m.Stats.OutputCost = m.BillingSnapshot.FinalOutputCost
}

func (m *imagesRelayMetrics) currentBillingSnapshot(actualModel string) model.AccessPlanBillingSnapshot {
	if m.BillingSnapshot.BillingModelName != "" || m.BillingSnapshot.AccessPlanID != 0 {
		return m.BillingSnapshot
	}
	return m.buildBillingSnapshot(actualModel, nil)
}

func (m *imagesRelayMetrics) buildBillingSnapshot(actualModel string, usage *imagesUsage) model.AccessPlanBillingSnapshot {
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
	if m.AccessRouteRule != nil && m.AccessRouteRule.BillingModelSource != "" {
		billingSource = m.AccessRouteRule.BillingModelSource
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
	inputTokens := usage.InputTokenCount()
	cacheReadTokens := usage.CacheReadTokenCount()
	cacheWriteTokens := usage.CacheWriteTokenCount()
	if !usage.HasSeparateCacheInputTokens() {
		if inputTokens < cacheReadTokens+cacheWriteTokens {
			inputTokens = cacheReadTokens + cacheWriteTokens
		}
		inputTokens -= cacheReadTokens
		if inputTokens < 0 {
			inputTokens = 0
		}
	}
	snapshot.FinalInputCost = float64(inputTokens) * snapshot.BaseInputPrice * snapshot.FinalMultiplier * 1e-6
	snapshot.FinalOutputCost = float64(usage.OutputTokenCount()) * snapshot.BaseOutputPrice * snapshot.FinalMultiplier * 1e-6
	snapshot.FinalCacheReadCost = float64(cacheReadTokens) * snapshot.BaseCacheReadPrice * snapshot.FinalMultiplier * 1e-6
	snapshot.FinalCacheWriteCost = float64(cacheWriteTokens) * snapshot.BaseCacheWritePrice * snapshot.FinalMultiplier * 1e-6
	return snapshot
}

func (m *imagesRelayMetrics) Save(ctx context.Context, success bool, err error, attempts []model.ChannelAttempt) {
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
	log.Infof("images relay complete: model=%s, channel=%d(%s), success=%t, duration=%dms, input_token=%d, output_token=%d, cache_hit_token=%d, cache_rate=%.2f%%, input_cost=%f, output_cost=%f, total_cost=%f, attempts=%d, error_status=%d, error_code=%s, error_strategy=%s",
		m.RequestModel, channelID, channelName, success, duration.Milliseconds(),
		m.Stats.InputToken, m.Stats.OutputToken, m.Stats.CacheHitToken, cacheHitRate(m.Stats.CacheHitToken, m.Stats.CacheInputToken)*100,
		m.Stats.InputCost, m.Stats.OutputCost, m.Stats.InputCost+m.Stats.OutputCost,
		len(attempts), errorStatus, errorCode, errorStrategy)

	m.saveLog(persistCtx, err, duration, attempts, channelID, channelName)
}

func (m *imagesRelayMetrics) saveLog(ctx context.Context, err error, duration time.Duration, attempts []model.ChannelAttempt, channelID int, channelName string) {
	actualModel := m.ActualModel
	if actualModel == "" {
		actualModel = m.RequestModel
	}
	billingSnapshot := m.currentBillingSnapshot(actualModel)

	relayLog := model.RelayLog{
		UserID:           m.UserID,
		APIKeyID:         m.APIKeyID,
		RequestIP:        m.RequestIP,
		Time:             m.StartTime.Unix(),
		RequestEndpoint:  m.RequestEndpoint,
		RequestPath:      m.RequestPath,
		RequestModelName: m.RequestModel,
		ChannelName:      channelName,
		ChannelId:        channelID,
		ActualModelName:  actualModel,
		UseTime:          int(duration.Milliseconds()),
		Attempts:         attempts,
		TotalAttempts:    len(attempts),
		SessionKey:       m.SessionKey,
		SessionSource:    m.SessionSource,
		RouteStickyHit:   routeStickyHit(attempts),
		RequestContent:   m.RequestContent,
		ResponseContent:  m.ResponseContent,
	}
	applyBillingSnapshotToRelayLog(&relayLog, billingSnapshot)

	if apiKey, getErr := op.APIKeyGet(m.APIKeyID, ctx); getErr == nil {
		relayLog.RequestAPIKeyName = apiKey.Name
	}
	if user, getErr := op.UserGet(m.UserID); getErr == nil {
		relayLog.UserName = user.Username
	}

	// 首字时间
	if !m.FirstToken.IsZero() {
		relayLog.Ftut = int(m.FirstToken.Sub(m.StartTime).Milliseconds())
	}

	// Usage
	relayLog.UsageSource, relayLog.UsageMissingReason = usageAuditFromStats(m.Stats, m.UsageSeen, err, false)
	if m.Stats.InputToken > 0 || m.Stats.OutputToken > 0 {
		relayLog.InputTokens = int(m.Stats.InputToken)
		relayLog.OutputTokens = int(m.Stats.OutputToken)
		relayLog.CacheHitTokens = int(m.Stats.CacheHitToken)
		relayLog.CacheWriteTokens = int(m.Stats.CacheWriteToken)
		relayLog.CacheInputTokens = int(m.Stats.CacheInputToken)
		relayLog.CacheHitRate = cacheHitRate(m.Stats.CacheHitToken, m.Stats.CacheInputToken)
		relayLog.Cost = m.Stats.InputCost + m.Stats.OutputCost
	}

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

func buildImagesRequestContentForLog(isMultipart bool, bc *bodycache.BodyCache, jsonPayload map[string]any) string {
	if isMultipart {
		// multipart 可能包含图片文件，避免落库
		return fmt.Sprintf(`{"content_type":"multipart/form-data","size_bytes":%d,"note":"multipart request content omitted for storage"}`, bc.Size())
	}
	if jsonPayload == nil {
		return ""
	}
	b, err := json.Marshal(jsonPayload)
	if err != nil {
		return ""
	}
	return truncateString(string(b), 8*1024)
}

func buildImagesResponseContentForLog(stream bool, upstreamCT string, usage *imagesUsage) string {
	if usage == nil {
		return ""
	}
	// 不记录 b64_json，仅记录 usage
	type respForLog struct {
		Stream      bool         `json:"stream"`
		ContentType string       `json:"content_type,omitempty"`
		Usage       *imagesUsage `json:"usage,omitempty"`
		Note        string       `json:"note,omitempty"`
	}
	obj := respForLog{
		Stream:      stream,
		ContentType: upstreamCT,
		Usage:       usage,
		Note:        "image data omitted for storage",
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return ""
	}
	return string(b)
}

func truncateString(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}

func parseJSONModelAndStream(bc *bodycache.BodyCache) (payload map[string]any, modelName string, stream bool, err error) {
	r, err := bc.NewReader()
	if err != nil {
		return nil, "", false, err
	}
	defer r.Close()

	body, err := io.ReadAll(r)
	if err != nil {
		return nil, "", false, err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, "", false, errors.New("empty body")
	}

	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, "", false, errors.New("invalid json")
	}

	rawModel, ok := m["model"]
	if !ok {
		return nil, "", false, errors.New("model is required")
	}
	modelStr, ok := rawModel.(string)
	if !ok || strings.TrimSpace(modelStr) == "" {
		return nil, "", false, errors.New("model is required")
	}

	stream = false
	if v, ok := m["stream"]; ok {
		switch vv := v.(type) {
		case bool:
			stream = vv
		case string:
			stream = strings.EqualFold(strings.TrimSpace(vv), "true")
		case float64:
			stream = vv != 0
		}
	}

	return m, strings.TrimSpace(modelStr), stream, nil
}

func parseMultipartModelAndStream(bc *bodycache.BodyCache, boundary string) (modelName string, stream bool, err error) {
	r, err := bc.NewReader()
	if err != nil {
		return "", false, err
	}
	defer r.Close()

	mr := multipart.NewReader(r, boundary)

	stream = false
	for {
		part, err := mr.NextPart()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return "", false, err
		}

		name := part.FormName()
		if name == "" {
			_, _ = io.Copy(io.Discard, part)
			_ = part.Close()
			continue
		}

		switch name {
		case "model":
			b, _ := io.ReadAll(io.LimitReader(part, 1024))
			modelName = strings.TrimSpace(string(b))
		case "stream":
			b, _ := io.ReadAll(io.LimitReader(part, 16))
			stream = strings.EqualFold(strings.TrimSpace(string(b)), "true")
		default:
			_, _ = io.Copy(io.Discard, part)
		}
		_ = part.Close()
	}

	if strings.TrimSpace(modelName) == "" {
		return "", false, errors.New("model is required")
	}
	return modelName, stream, nil
}

func imagesAttempt(
	ctx context.Context,
	endpoint string,
	c *gin.Context,
	bc *bodycache.BodyCache,
	isMultipart bool,
	boundary string,
	jsonPayload map[string]any,
	stream bool,
	channel *model.Channel,
	channelKey string,
	firstTokenTimeOutSec int,
	metrics *imagesRelayMetrics,
	actualModel string,
) (statusCode int, written bool, usage *imagesUsage, upstreamCT string, err error) {
	// 构建 URL。Images 是 OpenAI-compatible API，规范化 base URL 时会清理
	// /v1 后缀；这里必须重新补 canonical /v1/images/*，否则规范化后的
	// https://api.openai.com 会被误打到 /images/*。
	baseURL := channel.GetBaseUrl()
	targetURL, err := xurl.JoinOpenAIPath(baseURL, "/v1"+endpoint)
	if err != nil {
		return 0, false, nil, "", fmt.Errorf("failed to build upstream image url: %w", err)
	}
	parsedURL, err := url.Parse(targetURL)
	if err != nil {
		return 0, false, nil, "", fmt.Errorf("failed to parse upstream image url: %w", err)
	}

	var bodyReader io.Reader
	var contentType string

	if isMultipart {
		pr, pw := io.Pipe()
		mw := multipart.NewWriter(pw)
		contentType = mw.FormDataContentType()
		bodyReader = pr

		safe.SafeGo("images-multipart-writer", func() {
			src, err := bc.NewReader()
			if err != nil {
				_ = pw.CloseWithError(err)
				return
			}
			defer src.Close()

			if err := copyMultipartReplaceModel(src, boundary, mw, actualModel); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
			// 先关闭 multipart.Writer 写入结束 boundary，再关闭 pipe writer
			if err := mw.Close(); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
			_ = pw.Close()
		})
	} else {
		// JSON：仅改写 model 字段，其余保持不变
		// 注意：每次尝试都重新 marshal 生成 body，确保可重试重建
		if jsonPayload == nil {
			return 0, false, nil, "", errors.New("nil json payload")
		}
		if isGrokImagesModel(actualModel) {
			// Grok image generation has no streaming variant; the normalized
			// payload drops "stream", so the local stream flag must follow suit.
			// Otherwise stream=true would route into proxySSE while the upstream
			// replies with application/json, failing with a non-SSE content-type
			// error instead of returning the image via proxyNonStream.
			jsonPayload = normalizeGrokImagesPayload(jsonPayload)
			stream = false
		}
		if isAgnesImagesModel(actualModel) {
			// Agnes is an OpenAI /v1/images/generations variant that expects
			// response_format/image nested inside extra_body. Reshape a standard
			// inbound payload in place; other fields (size/ratio/n/…) are left as
			// the caller sent them. Streaming is untouched.
			normalizeAgnesImagesPayload(jsonPayload)
		}
		jsonPayload["model"] = actualModel
		b, err := json.Marshal(jsonPayload)
		if err != nil {
			return 0, false, nil, "", fmt.Errorf("failed to marshal json: %w", err)
		}
		bodyReader = bytes.NewReader(b)
		contentType = "application/json"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "", bodyReader)
	if err != nil {
		return 0, false, nil, "", fmt.Errorf("failed to create request: %w", err)
	}
	req.URL = parsedURL
	req.Method = http.MethodPost

	// Header 透传：复制下游 header，过滤 hop-by-hop 与鉴权相关
	copyHeadersToUpstream(req, c, channel, channelKey, contentType, stream)

	// 发送请求
	httpClient, err := helper.ChannelHttpClient(channel)
	if err != nil {
		return 0, false, nil, "", err
	}

	respUp, err := httpClient.Do(req)
	if err != nil {
		return 0, false, nil, "", fmt.Errorf("failed to send request: %w", err)
	}
	defer respUp.Body.Close()

	upstreamCT = respUp.Header.Get("Content-Type")

	// stream=true：逐行解析 event/data/空行边界透传
	if stream {
		if respUp.StatusCode < 200 || respUp.StatusCode >= 300 {
			b, _ := io.ReadAll(io.LimitReader(respUp.Body, upstreamErrorBodyLimit))
			upErr := newUpstreamError(respUp.StatusCode, b)
			log.Warnf("images upstream returned non-2xx: status=%d, code=%s, strategy=%s", upErr.StatusCode(), upErr.ErrorCode(), upErr.Strategy())
			return respUp.StatusCode, false, nil, upstreamCT, upErr
		}
		u, w, err := proxySSE(ctx, c, respUp, firstTokenTimeOutSec, metrics, false)
		return respUp.StatusCode, w, u, upstreamCT, err
	}

	// 非流式：2xx 透传，否则读取限长错误体用于错误信息与重试判定
	if respUp.StatusCode < 200 || respUp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(respUp.Body, upstreamErrorBodyLimit))
		upErr := newUpstreamError(respUp.StatusCode, b)
		log.Warnf("images upstream returned non-2xx: status=%d, code=%s, strategy=%s", upErr.StatusCode(), upErr.ErrorCode(), upErr.Strategy())
		return respUp.StatusCode, false, nil, upstreamCT, upErr
	}

	u, w, err := proxyNonStream(c, respUp)
	return respUp.StatusCode, w, u, upstreamCT, err
}

func copyHeadersToUpstream(req *http.Request, c *gin.Context, channel *model.Channel, channelKey string, contentType string, stream bool) {
	for k, values := range c.Request.Header {
		if !shouldForwardClientHeader(k) {
			continue
		}
		for _, v := range values {
			req.Header.Add(k, v)
		}
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+channelKey)

	if len(channel.CustomHeader) > 0 {
		for _, h := range channel.CustomHeader {
			req.Header.Set(h.HeaderKey, h.HeaderValue)
		}
	}
	// The raw/image/video paths have no applyHeaderDefaults, so without this they
	// went out with Go's default http-client UA (a "this is a Go relay" tell).
	// Apply the same unified non-CLI UA the main relay path uses. IfMissing: an
	// operator CustomHeader UA (set just above) wins, and on the /responses/compact
	// codex sub-path the caller force-overrides this with the codex UA afterwards.
	setHeaderIfMissing(req.Header, "User-Agent", genericUAForChannel(channel))
}

func copyMultipartReplaceModel(src io.Reader, boundary string, dst *multipart.Writer, newModel string) error {
	mr := multipart.NewReader(src, boundary)

	for {
		part, err := mr.NextPart()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}

		hdr := make(textproto.MIMEHeader, len(part.Header))
		for k, vv := range part.Header {
			cp := make([]string, len(vv))
			copy(cp, vv)
			hdr[k] = cp
		}

		pw, err := dst.CreatePart(hdr)
		if err != nil {
			_ = part.Close()
			return err
		}

		if part.FormName() == "model" && part.FileName() == "" {
			// 丢弃原值，写入替换后的 model（继续复制后续 part）
			_, _ = io.Copy(io.Discard, part)
			_, werr := io.WriteString(pw, newModel)
			_ = part.Close()
			if werr != nil {
				return werr
			}
			continue
		}

		_, err = io.Copy(pw, part)
		_ = part.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// proxyNonStream 将上游非流式响应原样透传到下游，同时尽量提取 usage（避免解析巨大 b64_json）。
func proxyNonStream(c *gin.Context, respUp *http.Response) (*imagesUsage, bool, error) {
	usage, written, _, err := proxyNonStreamWithResponseID(c, respUp)
	return usage, written, err
}

func proxyNonStreamWithResponseID(c *gin.Context, respUp *http.Response) (*imagesUsage, bool, string, error) {
	ct := respUp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}
	c.Header("Content-Type", ct)
	c.Status(respUp.StatusCode)

	scanner := newUsageScanner()
	idScanner := newResponsesIDScanner()

	buf := make([]byte, 32*1024)
	for {
		n, rerr := respUp.Body.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			scanner.Feed(chunk)
			idScanner.Feed(chunk)
			if _, werr := c.Writer.Write(chunk); werr != nil {
				return scanner.Usage(), true, idScanner.ID(), werr
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			return scanner.Usage(), c.Writer.Written(), idScanner.ID(), rerr
		}
	}

	return scanner.Usage(), c.Writer.Written(), idScanner.ID(), nil
}

// proxySSE 将上游 SSE 逐行解析 event/data/空行并透传到下游；首事件计为 FirstTokenTime；支持 FirstTokenTimeOut 切换。
type firstTokenMetrics interface {
	SetFirstTokenTime(time.Time)
}

func proxySSE(ctx context.Context, c *gin.Context, respUp *http.Response, firstTokenTimeOutSec int, metrics firstTokenMetrics, scanAllData bool) (*imagesUsage, bool, error) {
	usage, written, _, err := proxySSEWithOptions(ctx, c, respUp, firstTokenTimeOutSec, metrics, proxySSEOptions{ScanAllData: scanAllData})
	return usage, written, err
}

type proxySSEOptions struct {
	ScanAllData             bool
	CaptureResponsesID      bool
	StopOnResponsesTerminal bool
}

func proxySSEWithOptions(ctx context.Context, c *gin.Context, respUp *http.Response, firstTokenTimeOutSec int, metrics firstTokenMetrics, options proxySSEOptions) (*imagesUsage, bool, string, error) {
	if ct := respUp.Header.Get("Content-Type"); ct != "" && !strings.Contains(strings.ToLower(ct), "text/event-stream") {
		b, _ := io.ReadAll(io.LimitReader(respUp.Body, imagesUpstreamErrorBodyLimit))
		return nil, false, "", fmt.Errorf("upstream returned non-SSE content-type %q for stream request: %s", ct, string(b))
	}

	// 设置 SSE 响应头
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	type lineResult struct {
		line []byte
		err  error
		eof  bool
	}

	results := make(chan lineResult, 1)
	// done lets a reader parked on the cap-1 send exit when this consumer returns early
	// (first-token / idle timeout, client disconnect) instead of leaking for the process
	// lifetime — same guard as handleStreamResponse's SSE reader.
	done := make(chan struct{})
	defer close(done)
	safe.SafeGo("images-sse-reader", func() {
		defer close(results)
		br := bufio.NewReaderSize(respUp.Body, 64*1024)
		for {
			line, err := readLineLimited(br, maxSSEEventSize)
			if err != nil {
				if errors.Is(err, io.EOF) {
					select {
					case results <- lineResult{eof: true}:
					case <-done:
					}
					return
				}
				select {
				case results <- lineResult{err: err}:
				case <-done:
				}
				return
			}
			select {
			case results <- lineResult{line: line}:
			case <-done:
				return
			}
		}
	})

	var firstTokenTimer *time.Timer
	var firstTokenC <-chan time.Time
	if firstTokenTimeOutSec > 0 {
		firstTokenTimer = time.NewTimer(time.Duration(firstTokenTimeOutSec) * time.Second)
		firstTokenC = firstTokenTimer.C
		defer func() {
			if firstTokenTimer != nil {
				firstTokenTimer.Stop()
			}
		}()
	}

	// keepalive 心跳：与主路径 handleStreamResponse 一致，按配置间隔向下游写一个
	// 合法且不污染流的 SSE 注释心跳（OpenAI 兼容流为 ":\n\n"），并 Flush。
	var keepaliveC <-chan time.Time
	var keepaliveTicker *time.Ticker
	keepaliveInterval := currentStreamKeepaliveInterval()
	if keepaliveInterval > 0 {
		keepaliveTicker = time.NewTicker(keepaliveInterval)
		keepaliveC = keepaliveTicker.C
		defer keepaliveTicker.Stop()
	}
	// 上游空闲超时：每收到一行 SSE 就 reset；相邻 event 间隔过久则断上游并返回
	// 带 octopus_upstream_stream_timeout 语义的错误（与主路径一致）。
	var dataTimeoutC <-chan time.Time
	var dataTimeoutTimer *time.Timer
	dataIntervalTimeout := currentStreamDataIntervalTimeout()
	if dataIntervalTimeout > 0 {
		dataTimeoutTimer = time.NewTimer(dataIntervalTimeout)
		dataTimeoutC = dataTimeoutTimer.C
		defer dataTimeoutTimer.Stop()
	}
	resetDataTimeout := func() {
		if dataTimeoutTimer == nil {
			return
		}
		if !dataTimeoutTimer.Stop() {
			select {
			case <-dataTimeoutTimer.C:
			default:
			}
		}
		dataTimeoutTimer.Reset(dataIntervalTimeout)
	}

	var (
		firstWrite        = true
		currentEvent      string
		completedScanner  = newUsageScanner()
		idScanner         = newResponsesIDScanner()
		stopAfterBoundary bool
		lastWriteAt       = time.Now()
	)

	for {
		select {
		case <-ctx.Done():
			log.Infof("client disconnected, stopping stream")
			_ = respUp.Body.Close()
			if err := ctx.Err(); err != nil {
				return completedScanner.Usage(), c.Writer.Written(), idScanner.ID(), fmt.Errorf("client disconnected during stream: %w", err)
			}
			return completedScanner.Usage(), c.Writer.Written(), idScanner.ID(), errors.New("client disconnected during stream")

		case <-firstTokenC:
			log.Warnf("first token timeout (%ds), switching channel", firstTokenTimeOutSec)
			_ = respUp.Body.Close()
			return completedScanner.Usage(), c.Writer.Written(), idScanner.ID(), fmt.Errorf("first token timeout (%ds)", firstTokenTimeOutSec)

		case <-keepaliveC:
			// 仅在距离上次下游写入已超过一个心跳间隔时才注入，避免与正常数据流叠加；
			// 注释心跳不参与 usage/id 扫描，也不改 firstWrite/firstToken 计时。
			if time.Since(lastWriteAt) >= keepaliveInterval {
				if _, werr := c.Writer.Write(streamKeepaliveComment); werr != nil {
					return completedScanner.Usage(), true, idScanner.ID(), werr
				}
				c.Writer.Flush()
				lastWriteAt = time.Now()
			}

		case <-dataTimeoutC:
			log.Warnf("stream data interval timeout (%s), closing upstream stream", dataIntervalTimeout)
			_ = respUp.Body.Close()
			return completedScanner.Usage(), c.Writer.Written(), idScanner.ID(), &localRelayError{
				status:   http.StatusGatewayTimeout,
				code:     "octopus_upstream_stream_timeout",
				strategy: "stream_data_interval_timeout;upstream_forwarded=true",
				message:  fmt.Sprintf("upstream stream timed out waiting for SSE event (%s)", dataIntervalTimeout),
			}

		case r, ok := <-results:
			if !ok {
				return completedScanner.Usage(), c.Writer.Written(), idScanner.ID(), nil
			}
			if r.eof {
				return completedScanner.Usage(), c.Writer.Written(), idScanner.ID(), nil
			}
			if r.err != nil {
				return completedScanner.Usage(), c.Writer.Written(), idScanner.ID(), fmt.Errorf("failed to read stream line: %w", r.err)
			}
			// 收到一行上游 SSE：重置空闲超时（与主路径 resetDataTimeout 一致）。
			resetDataTimeout()

			line := r.line
			trimmed := bytes.TrimRight(line, "\r\n")
			terminalPayloadLine := false
			if len(trimmed) == 0 {
				// 空行：事件边界
				currentEvent = ""
			} else if bytes.HasPrefix(trimmed, []byte("event:")) {
				currentEvent = strings.TrimSpace(string(trimmed[len("event:"):]))
			} else if bytes.HasPrefix(trimmed, []byte("data:")) {
				// 仅在 completed 事件上尝试提取 usage（避免解析/分配巨大 b64_json）
				payload := bytes.TrimSpace(trimmed[len("data:"):])
				if options.ScanAllData || currentEvent == "image_generation.completed" || bytes.Contains(payload, []byte(`"type":"image_generation.completed"`)) {
					completedScanner.Feed(payload)
				}
				if options.CaptureResponsesID {
					idScanner.FeedJSONPayload(payload)
				}
				payloadTerminal := responsesSSETerminalPayload("", payload)
				eventTerminal := responsesSSETerminalPayload(currentEvent, payload)
				if options.StopOnResponsesTerminal && (payloadTerminal || eventTerminal) {
					stopAfterBoundary = true
					terminalPayloadLine = payloadTerminal || eventTerminal
				}
			}

			if _, werr := c.Writer.Write(line); werr != nil {
				return completedScanner.Usage(), true, idScanner.ID(), werr
			}
			c.Writer.Flush()
			lastWriteAt = time.Now()

			if firstWrite {
				metrics.SetFirstTokenTime(time.Now())
				firstWrite = false
				if firstTokenTimer != nil {
					if !firstTokenTimer.Stop() {
						select {
						case <-firstTokenTimer.C:
						default:
						}
					}
					firstTokenTimer = nil
					firstTokenC = nil
				}
			}
			if stopAfterBoundary {
				if len(trimmed) == 0 {
					_ = respUp.Body.Close()
					return completedScanner.Usage(), c.Writer.Written(), idScanner.ID(), nil
				}
				if terminalPayloadLine {
					if _, werr := c.Writer.Write([]byte("\n")); werr != nil {
						return completedScanner.Usage(), true, idScanner.ID(), werr
					}
					c.Writer.Flush()
					_ = respUp.Body.Close()
					return completedScanner.Usage(), c.Writer.Written(), idScanner.ID(), nil
				}
			}
		}
	}
}

func readLineLimited(br *bufio.Reader, limit int) ([]byte, error) {
	var out []byte
	for {
		part, err := br.ReadSlice('\n')
		out = append(out, part...)
		if len(out) > limit {
			return nil, fmt.Errorf("sse line exceeds limit %d bytes", limit)
		}
		if err == nil {
			return out, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		// 允许返回已读部分 + err（调用方按 err 处理）
		return out, err
	}
}

type responsesIDScanner struct {
	buf bytes.Buffer
	id  string
	max int
}

func newResponsesIDScanner() *responsesIDScanner {
	return &responsesIDScanner{max: 256 * 1024}
}

func (s *responsesIDScanner) Feed(p []byte) {
	if s == nil || s.id != "" || len(p) == 0 {
		return
	}
	if s.buf.Len()+len(p) > s.max {
		return
	}
	_, _ = s.buf.Write(p)
	s.FeedJSONPayload(s.buf.Bytes())
}

func (s *responsesIDScanner) FeedJSONPayload(payload []byte) {
	if s == nil || s.id != "" || len(payload) == 0 || len(payload) > s.max {
		return
	}
	if id := responsesIDFromPayload(payload); id != "" {
		s.id = id
	}
}

func (s *responsesIDScanner) ID() string {
	if s == nil {
		return ""
	}
	return s.id
}

func responsesIDFromPayload(payload []byte) string {
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 || bytes.HasPrefix(payload, []byte("[DONE]")) {
		return ""
	}
	var envelope struct {
		ID       string `json:"id"`
		Response *struct {
			ID string `json:"id"`
		} `json:"response"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return ""
	}
	if id := strings.TrimSpace(envelope.ID); id != "" {
		return id
	}
	if envelope.Response != nil {
		return strings.TrimSpace(envelope.Response.ID)
	}
	return ""
}

func responsesSSETerminalPayload(event string, payload []byte) bool {
	if strings.HasPrefix(strings.TrimSpace(string(payload)), "[DONE]") {
		return true
	}
	event = strings.TrimSpace(event)
	switch event {
	case "response.completed", "response.failed", "response.incomplete":
		return true
	}
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return false
	}
	switch strings.TrimSpace(envelope.Type) {
	case "response.completed", "response.failed", "response.incomplete":
		return true
	default:
		return false
	}
}

type usageScanner struct {
	matchIdx       int
	waitForObject  bool
	collecting     bool
	braceDepth     int
	inString       bool
	escape         bool
	buf            bytes.Buffer
	usage          *imagesUsage
	done           bool
	maxCollectSize int
}

func newUsageScanner() *usageScanner {
	return &usageScanner{maxCollectSize: 64 * 1024}
}

// Feed 逐字节扫描输入，定位 "usage":{...} 并仅解析 usage 子对象。
// 该实现用于避免整体 json.Unmarshal 造成 b64_json 巨大内存分配。
func (s *usageScanner) Feed(p []byte) {
	if s.done || len(p) == 0 {
		return
	}
	const pat = `"usage":`

	for _, b := range p {
		if s.done {
			return
		}

		if s.collecting {
			if s.buf.Len() >= s.maxCollectSize {
				s.collecting = false
				s.done = true
				return
			}
			s.buf.WriteByte(b)

			if s.inString {
				if s.escape {
					s.escape = false
				} else if b == '\\' {
					s.escape = true
				} else if b == '"' {
					s.inString = false
				}
				continue
			}

			if b == '"' {
				s.inString = true
				continue
			}

			switch b {
			case '{':
				s.braceDepth++
			case '}':
				s.braceDepth--
				if s.braceDepth == 0 {
					var u imagesUsage
					if err := json.Unmarshal(s.buf.Bytes(), &u); err == nil {
						s.usage = &u
					}
					s.done = true
					s.collecting = false
					return
				}
			}
			continue
		}

		if s.waitForObject {
			if b == '{' {
				s.collecting = true
				s.braceDepth = 1
				s.buf.Reset()
				s.buf.WriteByte('{')
				s.inString = false
				s.escape = false
				s.waitForObject = false
				continue
			}
			// 跳过空白，遇到其他字符则放弃
			if b == ' ' || b == '\t' || b == '\n' || b == '\r' {
				continue
			}
			s.waitForObject = false
			continue
		}

		// 匹配 "usage":
		if b == pat[s.matchIdx] {
			s.matchIdx++
			if s.matchIdx == len(pat) {
				s.waitForObject = true
				s.matchIdx = 0
			}
			continue
		}

		// 失败回退：若当前字符可能是 pat[0]，则 matchIdx=1
		if b == pat[0] {
			s.matchIdx = 1
		} else {
			s.matchIdx = 0
		}
	}
}

func (s *usageScanner) Usage() *imagesUsage {
	return s.usage
}
