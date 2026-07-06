package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/bestruirui/octopus/internal/helper"
	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/relay/balancer"
	"github.com/bestruirui/octopus/internal/relay/grouplimit"
	"github.com/bestruirui/octopus/internal/server/resp"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	"github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	openaiOutbound "github.com/bestruirui/octopus/internal/transformer/outbound/openai"
	"github.com/bestruirui/octopus/internal/utils/log"
	"github.com/gin-gonic/gin"
	"github.com/tmaxmax/go-sse"
)

// Handler 处理入站请求并转发到上游服务
func Handler(inboundType inbound.InboundType, c *gin.Context) {
	// 解析请求
	internalRequest, inAdapter, err := parseRequest(inboundType, c)
	if err != nil {
		return
	}
	supportedModels := c.GetString("supported_models")
	anthropicAliases := prepareAnthropicModelCompatibility(inboundType, internalRequest)
	if !isSupportedRequestModel(supportedModels, internalRequest.Model, anthropicAliases) {
		resp.Error(c, http.StatusBadRequest, "model not supported")
		return
	}

	requestModel := internalRequest.Model
	apiKeyID := c.GetInt("api_key_id")
	userID := c.GetInt("user_id")
	clientSession := deriveManagedClientSessionInfo(c.Request.Header, internalRequest)
	clientSessionKey := clientSession.Key

	// 获取通道分组
	routeResult, status, message, err := selectRouteGroup(c, apiKeyID, requestModel, anthropicAliases...)
	if err != nil || status != 0 {
		if message == "" && err != nil {
			message = err.Error()
		}
		resp.Error(c, status, message)
		return
	}
	preferStreamRouting := internalRequestPrefersStream(internalRequest)
	group := enrichGroupForSmartRouting(c.Request.Context(), routeResult.Group, preferStreamRouting)

	// 分组级限流闸：模型已路由到本组、无处可铺，到顶硬拒(429)以保护上游不被打满。
	// 渠道级 RPM/并发是软降档(把流量铺到别的渠道)，分组级则是整组吞吐的硬上限。
	// gate 只管初始解析出的组；release 经 defer 在请求结束(任何退出路径)释放在途计数。
	if group.MaxConcurrent > 0 || group.RPMLimit > 0 {
		releaseGroupSlot, ok, reason := grouplimit.Acquire(group.ID, group.MaxConcurrent, group.RPMLimit)
		if !ok {
			resp.Error(c, http.StatusTooManyRequests, reason)
			return
		}
		defer releaseGroupSlot()
	}

	// 创建迭代器（策略排序 + 粘性优先）。stickyEnabled 按「分组模式 + 会话来源」分级：
	// 轮询/负载均衡模式下纯优化型会话(prompt_cache_key / oct 自造指纹)不 sticky、走真轮询，
	// previous_response_id / 线程会话等需正确性的来源仍 sticky。填充优先模式全程 sticky。
	stickyEnabled := routeStickyEnabled(group.Mode, clientSession.Source)
	iter := balancer.NewIteratorWithSession(group, apiKeyID, requestModel, clientSessionKey, stickyEnabled)
	iter.PrioritizeChannels(nativeProtocolChannelIDs(c.Request.Context(), inboundType, group.Items))
	prioritizeResponsesSessionOwner(c.Request.Context(), iter, internalRequest, apiKeyID, userID)
	if iter.Len() == 0 {
		resp.Error(c, http.StatusServiceUnavailable, "no available channel")
		return
	}

	// 初始化 Metrics
	metrics := NewRelayMetrics(apiKeyID, userID, c.GetString("request_ip"), requestModel, internalRequest)
	requestEndpoint := endpointNameForInbound(inboundType, c.Request.URL.Path)
	metrics.SetRequestEndpoint(requestEndpoint, c.Request.URL.Path)
	metrics.SetAccessPlan(routeResult.AccessPlan, routeResult.AccessRouteRule, routeResult.AccessRouteUsed)
	metrics.SetClientSession(clientSession)
	baseMessages := append([]model.Message(nil), internalRequest.Messages...)

	// 请求级上下文
	req := &relayRequest{
		c:                   c,
		inboundType:         inboundType,
		inAdapter:           inAdapter,
		internalRequest:     internalRequest,
		metrics:             metrics,
		apiKeyID:            apiKeyID,
		userID:              userID,
		requestModel:        requestModel,
		clientSessionKey:    clientSessionKey,
		clientSessionSource: clientSession.Source,
		stickyEnabled:       stickyEnabled,
		iter:                iter,
	}

	var (
		lastErr          error
		allAttempts      []dbmodel.ChannelAttempt
		triedReturnGroup bool
		contextWindowErr error // 命中上下文超长: 停止跨渠道遍历、也不再试 fallback group
	)

runIterator:
	req.iter = iter
	for iter.Next() {
		select {
		case <-c.Request.Context().Done():
			log.Infof("request context canceled, stopping retry")
			metrics.Save(c.Request.Context(), false, context.Canceled, append(allAttempts, iter.Attempts()...))
			return
		default:
		}

		item := iter.Item()

		// 获取通道
		channel, err := op.ChannelGet(item.ChannelID, c.Request.Context())
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
		if internalRequest.IsImageGenerationRequest() && !isImageGenerationRequestCompatibleChannelType(channel.Type) {
			iter.Skip(channel.ID, 0, channel.Name, fmt.Sprintf("channel type not compatible with image generation request: %d", channel.Type))
			continue
		}

		// A DisableCircuitBreaker channel forwards every request like a direct client:
		// take ALL enabled keys ignoring cooldown/quarantine so a key that just returned
		// 429/5xx/401 is never benched — the client keeps its retry claw on the upstream.
		var availableKeys []dbmodel.ChannelKey
		if channel.DisableCircuitBreaker {
			availableKeys = channel.GetAllEnabledChannelKeys()
		} else {
			availableKeys = channel.GetAvailableChannelKeys()
		}
		if len(availableKeys) == 0 {
			// On the final candidate channel there is no peer left to spill over to, so a
			// hot-path throttle that held back a briefly-cooling key would black the route
			// out — a synthetic "no available channel" that breaks a CLI's retry loop where
			// hitting the upstream directly would just return a retryable 429. Fall back to
			// the unthrottled key set so the request still reaches the upstream and the
			// client sees the real 429/200. The circuit breaker stays the backstop.
			// (A DisableCircuitBreaker channel already took every enabled key above, so it
			// only lands here with genuinely no usable key — nothing left to fall back to.)
			if !channel.DisableCircuitBreaker && iter.Index() == iter.Len()-1 {
				availableKeys = channel.GetAvailableChannelKeysLastResort()
			}
			if len(availableKeys) == 0 {
				iter.Skip(channel.ID, 0, channel.Name, "no available key")
				continue
			}
		}
		preferredKeyID := 0
		if ownerKeyID := previousResponsesOwnerKeyForChannelOwned(c.Request.Context(), internalRequest, channel.ID, apiKeyID, userID); ownerKeyID > 0 {
			preferredKeyID = ownerKeyID
		} else if stickyKeyID := iter.StickyKeyIDForCurrentChannel(channel.ID); stickyKeyID > 0 {
			preferredKeyID = stickyKeyID
		}
		availableKeys = balancer.PrioritizeChannelKeysByHealth(availableKeys, channel.ID, item.ModelName, preferredKeyID)

		// 出站适配器
		outAdapter := outbound.Get(channel.Type)
		if outAdapter == nil {
			iter.Skip(channel.ID, 0, channel.Name, fmt.Sprintf("unsupported channel type: %d", channel.Type))
			continue
		}

		// 类型兼容性检查
		if internalRequest.IsEmbeddingRequest() && !outbound.IsEmbeddingChannelType(channel.Type) {
			iter.Skip(channel.ID, 0, channel.Name, "channel type not compatible with embedding request")
			continue
		}
		if internalRequest.IsChatRequest() && !outbound.IsChatChannelType(channel.Type) {
			iter.Skip(channel.ID, 0, channel.Name, "channel type not compatible with chat request")
			continue
		}

		// 设置实际模型
		internalRequest.Model = item.ModelName

		for keyIndex, usedKey := range availableKeys {
			internalRequest.Messages = append([]model.Message(nil), baseMessages...)
			promptSnapshot := applyPromptOverrides(internalRequest, routeResult.AccessPlan, routeResult.AccessRouteRule, channel)
			if len(promptSnapshot.Sources) > 0 {
				clearResponsesRawPromptShape(internalRequest)
			}
			metrics.SetPromptOverrideSnapshot(promptSnapshot)

			// 熔断检查
			// 无熔断渠道跳过短路: 每个请求都照常尝试转发上游(像直连一样)，永不被 503 circuit_open 挡回。
			capabilityKey := routingCapabilityKey(internalRequest, channel)
			if !channel.DisableCircuitBreaker && iter.SkipCircuitBreakScoped(channel.ID, usedKey.ID, channel.Name, requestEndpoint, capabilityKey) {
				continue
			}

			log.Infof("request model %s, mode: %d, forwarding to channel: %s model: %s (attempt %d/%d, key %d/%d, sticky=%t)",
				requestModel, group.Mode, channel.Name, item.ModelName,
				iter.Index()+1, iter.Len(), keyIndex+1, len(availableKeys), iter.IsSticky())

			// 构造尝试级上下文 -- 只写变化的 4 个字段
			ra := &relayAttempt{
				relayRequest:         req,
				outAdapter:           outAdapter,
				channel:              channel,
				usedKey:              usedKey,
				firstTokenTimeOutSec: group.FirstTokenTimeOut,
			}

			result := ra.attempt()
			for transientTry := 0; !result.Success && !result.Written && result.Retryable && transientTry < maxTransientStreamRetries; transientTry++ {
				log.Warnf("retrying transient empty upstream stream on channel %s key %d (try %d/%d): %v",
					channel.Name, usedKey.ID, transientTry+1, maxTransientStreamRetries, result.Err)
				internalRequest.Messages = append([]model.Message(nil), baseMessages...)
				if snap := applyPromptOverrides(internalRequest, routeResult.AccessPlan, routeResult.AccessRouteRule, channel); len(snap.Sources) > 0 {
					clearResponsesRawPromptShape(internalRequest)
				}
				ra = &relayAttempt{
					relayRequest:         req,
					outAdapter:           outAdapter,
					channel:              channel,
					usedKey:              usedKey,
					firstTokenTimeOutSec: group.FirstTokenTimeOut,
				}
				result = ra.attempt()
			}
			if result.Success {
				metrics.Save(c.Request.Context(), true, nil, append(allAttempts, iter.Attempts()...))
				return
			}
			if result.Written {
				metrics.Save(c.Request.Context(), false, result.Err, append(allAttempts, iter.Attempts()...))
				return
			}
			lastErr = result.Err
			if result.Fatal {
				// Context-window overflow: no other channel/key will accept the same
				// oversized payload — stop iterating and return the 400 as-is.
				contextWindowErr = result.Err
				break
			}
			if !result.Retryable && !shouldTryNextChannelKey(result.StatusCode) {
				break
			}
		}
		if contextWindowErr != nil {
			break
		}
	}

	// 所有通道都失败
	allAttempts = append(allAttempts, iter.Attempts()...)
	if contextWindowErr == nil && shouldReturnToOriginalGroup(routeResult, triedReturnGroup) {
		triedReturnGroup = true
		fallbackGroup, err := op.GroupGetEnabledMap(requestModel, c.Request.Context())
		if err != nil {
			lastErr = err
		} else {
			fallbackGroup = enrichGroupForSmartRouting(c.Request.Context(), fallbackGroup, preferStreamRouting)
			fallbackSticky := routeStickyEnabled(fallbackGroup.Mode, clientSession.Source)
			fallbackIter := balancer.NewIteratorWithSession(fallbackGroup, apiKeyID, requestModel, clientSessionKey, fallbackSticky)
			fallbackIter.PrioritizeChannels(nativeProtocolChannelIDs(c.Request.Context(), inboundType, fallbackGroup.Items))
			prioritizeResponsesSessionOwner(c.Request.Context(), fallbackIter, internalRequest, apiKeyID, userID)
			if fallbackIter.Len() > 0 {
				group = fallbackGroup
				iter = fallbackIter
				req.stickyEnabled = fallbackSticky
				internalRequest.Model = requestModel
				goto runIterator
			}
		}
	}

	finalErr := lastErr
	if finalErr == nil {
		finalErr = routeSelectionErrorFromAttempts(allAttempts)
	}
	metrics.Save(c.Request.Context(), false, finalErr, allAttempts)
	status, code, message := relayErrorResponse(finalErr)
	resp.ErrorWithCode(c, status, code, message)
}

// attempt 统一管理一次通道尝试的完整生命周期
func (ra *relayAttempt) attempt() attemptResult {
	span := ra.iter.StartAttempt(ra.channel.ID, ra.usedKey.ID, ra.channel.Name)
	span.SetRouteScope(ra.metrics.RequestEndpoint, ra.routingCapabilityKey())
	finishRuntimeAttempt := balancer.BeginRuntimeAttempt(ra.channel.ID, ra.usedKey.ID, ra.internalRequest.Model)
	defer finishRuntimeAttempt()

	// 转发请求
	statusCode, fwdErr := ra.forward(span)

	usedAt := time.Now().Unix()

	if fwdErr == nil {
		// ====== 成功 ======
		ra.collectResponse()
		costDelta := ra.metrics.Stats.InputCost + ra.metrics.Stats.OutputCost
		op.ChannelKeyRecordUse(ra.usedKey, statusCode, usedAt, costDelta)

		span.End(dbmodel.AttemptSuccess, statusCode, "")
		balancer.RecordRuntimeSuccess(ra.channel.ID, ra.usedKey.ID, ra.internalRequest.Model, balancer.AttemptRuntimeMetrics{
			Duration:     span.Duration(),
			FirstToken:   firstTokenDurationSince(ra.metrics.FirstTokenTime, span.StartedAt()),
			OutputTokens: ra.metrics.Stats.OutputToken,
			Stream:       internalRequestPrefersStream(ra.internalRequest),
		})

		// Channel 维度统计
		op.StatsChannelUpdate(ra.channel.ID, dbmodel.StatsMetrics{
			WaitTime:       span.Duration().Milliseconds(),
			RequestSuccess: 1,
		})

		// 熔断器：记录成功
		balancer.RecordSuccessScoped(ra.channel.ID, ra.usedKey.ID, ra.internalRequest.Model, ra.metrics.RequestEndpoint, ra.routingCapabilityKey())
		// 会话保持：更新粘性记录（仅当该请求参与 sticky；轮询模式纯优化型会话不写，保证轮转）
		if ra.stickyEnabled {
			balancer.SetStickyWithSessionKey(ra.apiKeyID, ra.requestModel, ra.clientSessionKey, ra.channel.ID, ra.usedKey.ID)
		}

		ra.metrics.ParamOverride = paramOverrideValue(ra.channel.ParamOverride)

		return attemptResult{Success: true, StatusCode: statusCode}
	}

	// ====== 失败 ======
	recordStatusCode := attemptStatusCode(statusCode, fwdErr)
	op.ChannelKeyRecordUse(ra.usedKey, recordStatusCode, usedAt, 0)
	span.End(dbmodel.AttemptFailed, recordStatusCode, auditErrorMessage(fwdErr))

	breakerCounted := shouldRecordBreakerFailure(recordStatusCode, fwdErr)
	// A DisableCircuitBreaker channel never accumulates circuit/runtime failure state: a
	// burst of upstream errors must neither trip its breaker nor soft-cool its keys
	// (either would short-circuit later requests and defeat "forward every request like a
	// direct client"). Cost tracking + 401 quarantine (ChannelKeyRecordUse above) and the
	// failure stat below still run for operator visibility; only the health/routing
	// governors are suppressed.
	recordChannelHealth := breakerCounted && !ra.channel.DisableCircuitBreaker
	if recordChannelHealth {
		retryAfter, _ := retryAfterFromError(fwdErr)
		balancer.RecordRuntimeFailure(ra.channel.ID, ra.usedKey.ID, ra.internalRequest.Model, recordStatusCode, span.Duration(), retryAfter)
	}

	// Channel 维度统计
	channelStats := dbmodel.StatsMetrics{WaitTime: span.Duration().Milliseconds()}
	if breakerCounted {
		channelStats.RequestFailed = 1
	}
	op.StatsChannelUpdate(ra.channel.ID, channelStats)

	// 熔断器：记录失败
	// Do not let downstream disconnects or caller-side timeouts poison channel health.
	if recordChannelHealth {
		balancer.RecordFailureWithStatusScoped(ra.channel.ID, ra.usedKey.ID, ra.internalRequest.Model, ra.metrics.RequestEndpoint, ra.routingCapabilityKey(), recordStatusCode)
		if ra.iter != nil && ra.iter.IsStickyChannel(ra.channel.ID) {
			balancer.ClearStickyWithSessionKey(ra.apiKeyID, ra.requestModel, ra.clientSessionKey)
			log.Infof("cleared sticky route for api key %d model %s after channel %s failure", ra.apiKeyID, ra.requestModel, ra.channel.Name)
		}
	} else {
		log.Infof("skip circuit breaker failure count for channel %s model %s: %v", ra.channel.Name, ra.internalRequest.Model, fwdErr)
	}

	ra.metrics.ParamOverride = paramOverrideValue(ra.channel.ParamOverride)

	written := ra.c.Writer.Written()
	if written {
		switch ra.inboundType {
		case inbound.InboundTypeOpenAIResponse:
			if message := responsesStreamFailureMessage(fwdErr); message != "" {
				writeResponsesFailedSSE(ra.c, ra.requestModel, "upstream_error", message)
			}
		case inbound.InboundTypeAnthropic:
			if message := anthropicStreamFailureMessage(fwdErr); message != "" {
				writeAnthropicErrorSSE(ra.c, "api_error", message)
			}
		}
		ra.collectResponse()
	}
	return attemptResult{
		Success:    false,
		Written:    written,
		Err:        fmt.Errorf("channel %s failed: %w", ra.channel.Name, fwdErr),
		StatusCode: recordStatusCode,
		Retryable:  !written && isRetryableUpstreamStreamError(fwdErr),
		Fatal:      isContextWindowError(fwdErr),
	}
}

// parseRequest 解析并验证入站请求
func parseRequest(inboundType inbound.InboundType, c *gin.Context) (*model.InternalLLMRequest, model.Inbound, error) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return nil, nil, err
	}

	inAdapter := inbound.Get(inboundType)
	internalRequest, err := inAdapter.TransformRequest(c.Request.Context(), body)
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return nil, nil, err
	}

	// Pass through the original query parameters
	internalRequest.Query = c.Request.URL.Query()
	internalRequest.RawRequest = append([]byte(nil), body...)

	if err := internalRequest.Validate(); err == nil && requestHasNoEffectiveInput(internalRequest) {
		err = errors.New("either messages or input is required")
		if maybeHandleCursorEmptyAnthropicProbe(c, inboundType, internalRequest, body, err) {
			return nil, nil, err
		}
		if maybeHandleCursorEmptyOpenAIProbe(c, inboundType, internalRequest, body, err) {
			return nil, nil, err
		}
		saveClientValidationRelayLog(c.Request.Context(), c, inboundType, internalRequest, body, err)
		resp.ErrorWithCode(c, http.StatusBadRequest, clientValidationErrorCode(err), clientValidationErrorMessage(err))
		return nil, nil, err
	} else if err != nil {
		if maybeHandleCursorEmptyAnthropicProbe(c, inboundType, internalRequest, body, err) {
			return nil, nil, err
		}
		if maybeHandleCursorEmptyOpenAIProbe(c, inboundType, internalRequest, body, err) {
			return nil, nil, err
		}
		saveClientValidationRelayLog(c.Request.Context(), c, inboundType, internalRequest, body, err)
		resp.ErrorWithCode(c, http.StatusBadRequest, clientValidationErrorCode(err), clientValidationErrorMessage(err))
		return nil, nil, err
	}

	return internalRequest, inAdapter, nil
}

// forward 转发请求到上游服务
func (ra *relayAttempt) forward(span *balancer.AttemptSpan) (int, error) {
	ctx := ra.c.Request.Context()
	originalInternalRequest := cloneInternalRequestForRetry(ra.internalRequest)
	ra.applyTransformOptions()

	outAdapter := ra.outAdapter
	fallbackTried := false
	responsesCursorRecoveryTried := false
	responsesEncryptedContentRecoveryTried := false
	dropResponsesSessionCursor := false
	dropResponsesEncryptedContent := false
	forceNonStreamUpstream := ra.shouldPreferAnthropicNonStreamUpstream()
	streamAsNonStreamTried := forceNonStreamUpstream
	upstreamPaths := make([]string, 0, 2)
	if forceNonStreamUpstream {
		log.Infof("anthropic stream request for channel %s model %s will use non-stream upstream and re-emit SSE", ra.channel.Name, ra.internalRequest.Model)
	}

retryWithAdapter:

	originalStream := ra.internalRequest.Stream
	originalPreviousResponseID := ra.internalRequest.PreviousResponseID
	originalMessages := append([]model.Message(nil), ra.internalRequest.Messages...)
	originalResponsesInputRaw := cloneRawJSONMessage(ra.internalRequest.ResponsesInputRaw)
	forwardedPreviousResponseID := originalPreviousResponseID
	if dropResponsesSessionCursor {
		if originalPreviousResponseID != nil && ra.shouldBridgePlainResponsesCodexHistory() {
			ra.applyPlainResponsesCodexHistoryForPreviousResponseID(*originalPreviousResponseID)
		}
		ra.internalRequest.PreviousResponseID = nil
		forwardedPreviousResponseID = nil
	} else {
		ra.prepareResponsesSessionCursor(outAdapter)
		forwardedPreviousResponseID = ra.internalRequest.PreviousResponseID
	}
	if dropResponsesEncryptedContent {
		stripResponsesEncryptedContent(ra.internalRequest)
	} else {
		ra.prepareResponsesEncryptedContent(outAdapter)
	}
	forceResponsesStreamUpstream := ra.shouldForceOpenAIResponsesStreamUpstream(outAdapter) && !forceNonStreamUpstream
	forceAnthropicStreamUpstream := ra.shouldForceAnthropicStreamUpstream() && !forceNonStreamUpstream
	if forceResponsesStreamUpstream || forceAnthropicStreamUpstream {
		stream := true
		ra.internalRequest.Stream = &stream
	} else if forceNonStreamUpstream {
		stream := false
		ra.internalRequest.Stream = &stream
	}
	if ra.shouldBridgeImageGenerationToImages() {
		ra.internalRequest.Stream = originalStream
		return ra.forwardImageGenerationViaImages(ctx, func(upstreamPath string) {
			if upstreamPath == "" || span == nil {
				return
			}
			if len(upstreamPaths) == 0 || upstreamPaths[len(upstreamPaths)-1] != upstreamPath {
				upstreamPaths = append(upstreamPaths, upstreamPath)
			}
			span.SetUpstreamPath(strings.Join(upstreamPaths, " -> "))
		})
	}

	// 构建出站请求
	outboundRequest, err := outAdapter.TransformRequest(
		ctx,
		ra.internalRequest,
		ra.outboundBaseURL(),
		ra.usedKey.ChannelKey,
	)
	ra.internalRequest.Stream = originalStream
	ra.internalRequest.PreviousResponseID = originalPreviousResponseID
	ra.internalRequest.Messages = originalMessages
	ra.internalRequest.ResponsesInputRaw = originalResponsesInputRaw
	if err != nil {
		log.Warnf("failed to create request: %v", err)
		return 0, fmt.Errorf("failed to create request: %w", err)
	}
	if outboundRequest != nil && outboundRequest.URL != nil && span != nil {
		upstreamPath := outboundRequest.URL.EscapedPath()
		if upstreamPath == "" {
			upstreamPath = outboundRequest.URL.Path
		}
		if upstreamPath != "" && (len(upstreamPaths) == 0 || upstreamPaths[len(upstreamPaths)-1] != upstreamPath) {
			upstreamPaths = append(upstreamPaths, upstreamPath)
		}
		span.SetUpstreamPath(strings.Join(upstreamPaths, " -> "))
	}

	// 应用 ParamOverride 到请求体
	if ra.channel.ParamOverride != nil && *ra.channel.ParamOverride != "" {
		body, err := io.ReadAll(outboundRequest.Body)
		if err != nil {
			return 0, fmt.Errorf("failed to read body: %w", err)
		}
		restoreBody := func() {
			outboundRequest.Body = io.NopCloser(bytes.NewBuffer(body))
			outboundRequest.GetBody = func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(body)), nil
			}
			outboundRequest.ContentLength = int64(len(body))
		}

		var bodyMap map[string]any
		if err := json.Unmarshal(body, &bodyMap); err != nil {
			log.Warnf("failed to unmarshal request body: %v, skipping param_override", err)
			restoreBody()
		} else {
			var override map[string]any
			if err := json.Unmarshal([]byte(*ra.channel.ParamOverride), &override); err != nil {
				log.Warnf("failed to unmarshal param_override: %v, skipping", err)
				restoreBody()
			} else {
				maps.Copy(bodyMap, override)
				modifiedBody, err := json.Marshal(bodyMap)
				if err != nil {
					log.Warnf("failed to marshal modified body: %v, skipping param_override", err)
					restoreBody()
				} else {
					outboundRequest.Body = io.NopCloser(bytes.NewBuffer(modifiedBody))
					outboundRequest.GetBody = func() (io.ReadCloser, error) {
						return io.NopCloser(bytes.NewReader(modifiedBody)), nil
					}
					outboundRequest.ContentLength = int64(len(modifiedBody))
				}
			}
		}
	}

	// 复制请求头
	ra.copyHeaders(outboundRequest)

	// 发送请求
	response, err := ra.sendRequest(outboundRequest)
	if err != nil {
		return 0, fmt.Errorf("failed to send request: %w", err)
	}
	defer response.Body.Close()

	// 检查响应状态
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, err := io.ReadAll(io.LimitReader(response.Body, upstreamErrorBodyLimit))
		if err != nil {
			return response.StatusCode, fmt.Errorf("failed to read response body: %w", err)
		}
		upErr := newUpstreamError(response.StatusCode, body)
		if d, ok := retryAfterFromHeader(response.Header); ok {
			upErr.retryAfter = d
			upErr.hasRetryAfter = true
		}
		log.Warnf("upstream returned non-2xx: status=%d, code=%s, strategy=%s", upErr.StatusCode(), upErr.ErrorCode(), upErr.Strategy())
		if !responsesCursorRecoveryTried && forwardedPreviousResponseID != nil && (ra.shouldRecoverOpenAIResponsesPreviousResponseNotFound(response.StatusCode, upErr) || ra.shouldRecoverSynthesizedCodexResponsesCursor(response.StatusCode, upErr)) {
			responsesCursorRecoveryTried = true
			dropResponsesSessionCursor = true
			_ = response.Body.Close()
			log.Infof("openai responses upstream could not find previous_response_id on channel %s; retrying same key once without cursor", ra.channel.Name)
			goto retryWithAdapter
		}
		if !responsesEncryptedContentRecoveryTried && ra.shouldRecoverOpenAIResponsesInvalidEncryptedContent(response.StatusCode, upErr) {
			responsesEncryptedContentRecoveryTried = true
			dropResponsesEncryptedContent = true
			_ = response.Body.Close()
			log.Infof("openai responses upstream rejected encrypted reasoning content on channel %s; retrying same key once without encrypted content", ra.channel.Name)
			goto retryWithAdapter
		}
		if !streamAsNonStreamTried && ra.shouldFallbackAnthropicStreamToNonStream(response.StatusCode) {
			streamAsNonStreamTried = true
			forceNonStreamUpstream = true
			_ = response.Body.Close()
			log.Infof("anthropic stream upstream returned status %d on channel %s; retrying same key as non-stream and re-emitting SSE", response.StatusCode, ra.channel.Name)
			goto retryWithAdapter
		}
		if !fallbackTried && ra.shouldFallbackOpenAIResponsesToChat(response.StatusCode, upErr) {
			chatAdapter := outbound.Get(outbound.OutboundTypeOpenAIChat)
			if chatAdapter != nil {
				fallbackTried = true
				outAdapter = chatAdapter
				ra.internalRequest = cloneInternalRequestForRetry(originalInternalRequest)
				_ = response.Body.Close()
				log.Infof("openai responses upstream returned compatibility status %d on channel %s; retrying same key via chat completions", response.StatusCode, ra.channel.Name)
				goto retryWithAdapter
			}
		}
		return response.StatusCode, upErr
	}

	// 处理响应
	if forceNonStreamUpstream {
		if err := ra.handleNonStreamResponseAsStream(ctx, response, outAdapter); err != nil {
			return response.StatusCode, err
		}
		return response.StatusCode, nil
	}
	if (forceResponsesStreamUpstream || forceAnthropicStreamUpstream) && (originalStream == nil || !*originalStream) {
		if err := ra.handleStreamResponseAsNonStream(ctx, response, outAdapter); err != nil {
			return response.StatusCode, err
		}
		return response.StatusCode, nil
	}
	if forceResponsesStreamUpstream || forceAnthropicStreamUpstream || (ra.internalRequest.Stream != nil && *ra.internalRequest.Stream) {
		if err := ra.handleStreamResponse(ctx, response, outAdapter); err != nil {
			return response.StatusCode, err
		}
		return response.StatusCode, nil
	}
	if err := ra.handleResponse(ctx, response, outAdapter); err != nil {
		return response.StatusCode, err
	}
	return response.StatusCode, nil
}

func cloneInternalRequestForRetry(req *model.InternalLLMRequest) *model.InternalLLMRequest {
	if req == nil {
		return nil
	}
	clone := *req
	clone.Messages = append([]model.Message(nil), req.Messages...)
	clone.Tools = append([]model.Tool(nil), req.Tools...)
	clone.Include = append([]string(nil), req.Include...)
	clone.ResponsesInputRaw = cloneRawJSONMessage(req.ResponsesInputRaw)
	clone.ResponsesToolsRaw = cloneRawJSONMessages(req.ResponsesToolsRaw)
	clone.ResponsesToolChoiceRaw = cloneRawJSONMessage(req.ResponsesToolChoiceRaw)
	clone.ResponsesTextRaw = cloneRawJSONMessage(req.ResponsesTextRaw)
	clone.ClientMetadata = cloneRawJSONMessage(req.ClientMetadata)
	if req.Metadata != nil {
		clone.Metadata = maps.Clone(req.Metadata)
	}
	return &clone
}

func internalRequestPrefersStream(req *model.InternalLLMRequest) bool {
	return req != nil && req.Stream != nil && *req.Stream
}

func firstTokenDurationSince(firstToken time.Time, startedAt time.Time) time.Duration {
	if firstToken.IsZero() || startedAt.IsZero() || firstToken.Before(startedAt) {
		return 0
	}
	return firstToken.Sub(startedAt)
}

func cloneRawJSONMessage(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	out := make([]byte, len(raw))
	copy(out, raw)
	return out
}

func cloneRawJSONMessages(items []json.RawMessage) []json.RawMessage {
	if len(items) == 0 {
		return nil
	}
	out := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		out = append(out, cloneRawJSONMessage(item))
	}
	return out
}

func (ra *relayAttempt) outboundBaseURL() string {
	if ra == nil || ra.channel == nil {
		return ""
	}
	if ra.channel.Type == outbound.OutboundTypeCustomOpenAIChat {
		return ra.channel.GetOpenAIChatBaseUrl()
	}
	return ra.channel.GetBaseUrl()
}

func shouldTryNextChannelKey(statusCode int) bool {
	return statusCode == http.StatusTooManyRequests
}

// maxTransientStreamRetries bounds how many times one channel+key is retried
// in-place when the upstream opens a stream then ends it without any content.
// Such empty streams are transient upstream hiccups; nothing was written
// downstream yet, so an immediate same-target retry is safe and recovers the
// turn instead of failing it outright when there is no redundant channel.
const maxTransientStreamRetries = 2

// isRetryableUpstreamStreamError reports whether a forward failure is the
// transient "upstream opened a stream then ended it empty" case, which is safe
// to retry as long as nothing has been written downstream.
func isRetryableUpstreamStreamError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "upstream stream ended without internal response")
}

func previousResponsesOwnerKeyForChannel(ctx context.Context, req *model.InternalLLMRequest, channelID int) int {
	return previousResponsesOwnerKeyForChannelOwned(ctx, req, channelID, 0, 0)
}

func previousResponsesOwnerKeyForChannelOwned(ctx context.Context, req *model.InternalLLMRequest, channelID, reqTokenID, reqUserID int) int {
	if req == nil || req.PreviousResponseID == nil {
		return 0
	}
	return responsesOwnerKeyForChannel(ctx, *req.PreviousResponseID, channelID, reqTokenID, reqUserID)
}

func prioritizeAvailableChannelKey(keys []dbmodel.ChannelKey, preferredID int) []dbmodel.ChannelKey {
	if preferredID == 0 || len(keys) < 2 {
		return keys
	}
	idx := -1
	for i, key := range keys {
		if key.ID == preferredID {
			idx = i
			break
		}
	}
	if idx <= 0 {
		return keys
	}
	prioritized := make([]dbmodel.ChannelKey, len(keys))
	prioritized[0] = keys[idx]
	copy(prioritized[1:idx+1], keys[0:idx])
	copy(prioritized[idx+1:], keys[idx+1:])
	return prioritized
}

func shouldRecordBreakerFailure(_ int, err error) bool {
	if err == nil {
		return false
	}
	if isClientAbortError(err) {
		return false
	}
	if isRetryableUpstreamStreamError(err) {
		// Transient empty-stream failures are retried in place (maxTransientStreamRetries),
		// so counting each retry toward the breaker / runtime health would trip a
		// healthy channel ~3x sooner and skew capacity ranking. Let the retry +
		// final request error surface it instead of poisoning channel health.
		return false
	}
	if isContextWindowError(err) {
		// Deterministic client error (prompt exceeds the model's context window):
		// counting it would trip the channel's breaker on perfectly healthy capacity
		// and block later well-sized requests. Never charge it to channel health.
		return false
	}
	return true
}

func (ra *relayAttempt) shouldFallbackAnthropicStreamToNonStream(statusCode int) bool {
	if ra == nil || ra.internalRequest == nil {
		return false
	}
	if ra.channel == nil || ra.channel.Type != outbound.OutboundTypeAnthropic {
		return false
	}
	if ra.internalRequest.Stream == nil || !*ra.internalRequest.Stream {
		return false
	}
	switch statusCode {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, 520:
		return true
	default:
		return false
	}
}

func (ra *relayAttempt) shouldPreferAnthropicNonStreamUpstream() bool {
	// Prefer the real client streaming shape first. CPA/CLIProxyAPI 1M routes can
	// take longer than common 60s reverse-proxy idle limits before a non-stream
	// response returns headers, which looks like "context canceled" downstream.
	// Keep the compatibility retry in shouldFallbackAnthropicStreamToNonStream
	// for upstreams that explicitly reject stream requests.
	return false
}

func (ra *relayAttempt) shouldForceOpenAIResponsesStreamUpstream(outAdapter model.Outbound) bool {
	if ra == nil {
		return false
	}
	switch outAdapter.(type) {
	case *openaiOutbound.ResponseOutbound:
		return true
	default:
		return false
	}
}

// shouldForceAnthropicStreamUpstream forces a streaming upstream request for
// Claude-Code-cloaked Anthropic channels even when the client asked for a
// non-stream response. Real Claude Code always streams, and some relays (e.g.
// AnyRouter) refuse non-stream requests on gated models (opus) outright — a
// non-stream upstream call is risk-rejected before the business layer. octopus
// streams upstream to stay claude-code-shaped, then aggregates the SSE back into a
// single non-stream JSON response for the client (handleStreamResponseAsNonStream),
// so non-CLI/non-stream clients still get served. Gated on the same cloak switch as
// the claude-code header defaults, so an explicit cloak=off opts out.
func (ra *relayAttempt) shouldForceAnthropicStreamUpstream() bool {
	if ra == nil || ra.channel == nil {
		return false
	}
	if ra.channel.Type != outbound.OutboundTypeAnthropic {
		return false
	}
	return shouldApplyChannelCloak(ra.channel.Cloak)
}

func (ra *relayAttempt) shouldRecoverOpenAIResponsesInvalidEncryptedContent(statusCode int, err error) bool {
	if ra == nil || err == nil || ra.internalRequest == nil {
		return false
	}
	if statusCode != http.StatusBadRequest {
		return false
	}
	if ra.inboundType != inbound.InboundTypeOpenAIResponse || ra.channel == nil || ra.channel.Type != outbound.OutboundTypeOpenAIResponse {
		return false
	}
	if !requestHasResponsesEncryptedContent(ra.internalRequest) {
		return false
	}
	var upErr *upstreamError
	if !errors.As(err, &upErr) {
		return false
	}
	return isOpenAIResponsesInvalidEncryptedContent(upErr.Body())
}

func (ra *relayAttempt) shouldRecoverOpenAIResponsesPreviousResponseNotFound(statusCode int, err error) bool {
	if ra == nil || err == nil || ra.internalRequest == nil || ra.internalRequest.PreviousResponseID == nil {
		return false
	}
	if ra.inboundType != inbound.InboundTypeOpenAIResponse || ra.channel == nil || ra.channel.Type != outbound.OutboundTypeOpenAIResponse {
		return false
	}
	if !ra.responsesRequestSafeForCursorRecovery() {
		return false
	}
	var upErr *upstreamError
	if !errors.As(err, &upErr) {
		return false
	}
	switch statusCode {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity:
		return isOpenAIResponsesPreviousResponseNotFound(upErr.Body())
	default:
		return false
	}
}

func (ra *relayAttempt) shouldRecoverSynthesizedCodexResponsesCursor(statusCode int, err error) bool {
	if ra == nil || err == nil || ra.internalRequest == nil || ra.internalRequest.PreviousResponseID == nil {
		return false
	}
	if statusCode != http.StatusBadRequest {
		return false
	}
	if ra.inboundType != inbound.InboundTypeOpenAIResponse || ra.channel == nil || ra.channel.Type != outbound.OutboundTypeOpenAIResponse {
		return false
	}
	if ra.inboundLooksLikeCodexClient() {
		return false
	}
	if !ra.shouldUseCodexFingerprint() || !ra.responsesRequestSafeForSynthesizedCursorRecovery() || requestHasResponsesEncryptedContent(ra.internalRequest) {
		return false
	}
	var upErr *upstreamError
	return errors.As(err, &upErr)
}

func (ra *relayAttempt) responsesRequestSafeForSynthesizedCursorRecovery() bool {
	if ra == nil || ra.internalRequest == nil {
		return false
	}
	if ra.responsesRequestSafeForCursorRecovery() {
		return true
	}
	if !responsesMessagesContainToolOutput(ra.internalRequest.Messages) {
		return false
	}
	if ra.internalRequest.PreviousResponseID == nil {
		return false
	}
	history, ok := responsesSessionTranscript(*ra.internalRequest.PreviousResponseID, ra.apiKeyID, ra.userID)
	return ok && len(history) > 0
}

func (ra *relayAttempt) responsesRequestSafeForCursorRecovery() bool {
	if ra == nil || ra.internalRequest == nil {
		return false
	}
	for _, message := range ra.internalRequest.Messages {
		if strings.EqualFold(strings.TrimSpace(message.Role), "tool") {
			return false
		}
	}
	return true
}

func isOpenAIResponsesPreviousResponseNotFound(body string) bool {
	normalized := strings.ToLower(strings.TrimSpace(body))
	if normalized == "" {
		return false
	}
	if strings.Contains(normalized, "previous_response_not_found") ||
		strings.Contains(normalized, "previous response not found") ||
		strings.Contains(normalized, "previous_response_id") && strings.Contains(normalized, "not found") ||
		strings.Contains(normalized, "no response found") ||
		strings.Contains(normalized, "could not find response") {
		return true
	}
	return false
}

func isOpenAIResponsesInvalidEncryptedContent(body string) bool {
	normalized := strings.ToLower(strings.TrimSpace(body))
	if normalized == "" {
		return false
	}
	return strings.Contains(normalized, "invalid_encrypted_content") ||
		strings.Contains(normalized, "encrypted content could not be decrypted") ||
		strings.Contains(normalized, "encrypted content could not be verified") ||
		strings.Contains(normalized, "could not be decrypted or parsed")
}

func (ra *relayAttempt) shouldFallbackOpenAIResponsesToChat(statusCode int, err error) bool {
	if ra == nil || err == nil {
		return false
	}
	if ra.inboundType != inbound.InboundTypeOpenAIResponse || ra.channel.Type != outbound.OutboundTypeOpenAIResponse {
		return false
	}
	if !ra.responsesRequestSafeForChatFallback() {
		return false
	}
	var upErr *upstreamError
	if !errors.As(err, &upErr) {
		return false
	}
	switch upErr.StatusCode() {
	case http.StatusForbidden:
		return isOpenAIResponsesProxyCompatibilityError(upErr.Body())
	case http.StatusBadRequest, http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusUnprocessableEntity:
		return isOpenAIResponsesEndpointUnsupportedError(upErr.StatusCode(), upErr.Body())
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, 520:
		return true
	default:
		compatibleStatus := statusCode == http.StatusBadRequest ||
			statusCode == http.StatusNotFound ||
			statusCode == http.StatusMethodNotAllowed ||
			statusCode == http.StatusUnprocessableEntity ||
			statusCode == http.StatusBadGateway ||
			statusCode == http.StatusServiceUnavailable ||
			statusCode == http.StatusGatewayTimeout ||
			statusCode == 520
		return compatibleStatus && isOpenAIResponsesEndpointUnsupportedError(statusCode, upErr.Body())
	}
}

func isOpenAIResponsesProxyCompatibilityError(body string) bool {
	normalized := strings.ToLower(strings.TrimSpace(body))
	if normalized == "" {
		return false
	}
	if strings.Contains(normalized, "invalid_api_key") ||
		strings.Contains(normalized, "invalid api key") ||
		strings.Contains(normalized, "unauthorized") ||
		strings.Contains(normalized, "permission denied") {
		return false
	}
	return strings.Contains(normalized, "bad_response_status_code") ||
		strings.Contains(normalized, "insufficient account balance") ||
		strings.Contains(normalized, "response api") ||
		strings.Contains(normalized, "responses endpoint")
}

func (ra *relayAttempt) responsesRequestSafeForChatFallback() bool {
	if ra == nil || ra.internalRequest == nil {
		return false
	}
	if ra.internalRequest.IsImageGenerationRequest() {
		return false
	}
	for _, modality := range ra.internalRequest.Modalities {
		switch strings.ToLower(strings.TrimSpace(modality)) {
		case "image", "audio":
			return false
		}
	}
	for _, message := range ra.internalRequest.Messages {
		if message.Audio != nil || len(message.Images) > 0 {
			return false
		}
		for _, part := range message.Content.MultipleContent {
			partType := strings.ToLower(strings.TrimSpace(part.Type))
			if partType != "" && partType != "text" {
				return false
			}
			if part.ImageURL != nil || part.Audio != nil || part.File != nil {
				return false
			}
		}
	}
	for _, tool := range ra.internalRequest.Tools {
		toolType := strings.ToLower(strings.TrimSpace(tool.Type))
		if tool.ImageGeneration != nil || toolType == "image_generation" {
			return false
		}
		if toolType != "" && toolType != "function" {
			return false
		}
	}
	return true
}

func isOpenAIResponsesEndpointUnsupportedError(statusCode int, body string) bool {
	normalized := strings.ToLower(strings.TrimSpace(body))
	if normalized == "" {
		return false
	}

	hasEndpointSignal := strings.Contains(normalized, "responses") ||
		strings.Contains(normalized, "/v1/responses") ||
		strings.Contains(normalized, "response api") ||
		strings.Contains(normalized, "response endpoint")

	hasUnsupportedSignal := strings.Contains(normalized, "not support") ||
		strings.Contains(normalized, "unsupported") ||
		strings.Contains(normalized, "not implemented") ||
		strings.Contains(normalized, "unknown url") ||
		strings.Contains(normalized, "unknown endpoint") ||
		strings.Contains(normalized, "invalid endpoint") ||
		strings.Contains(normalized, "no route") ||
		strings.Contains(normalized, "route not found") ||
		strings.Contains(normalized, "not found") ||
		strings.Contains(normalized, "method not allowed")

	if hasEndpointSignal && hasUnsupportedSignal {
		return true
	}

	switch statusCode {
	case http.StatusNotFound:
		return strings.Contains(normalized, "404 page not found") ||
			strings.Contains(normalized, "cannot post /v1/responses") ||
			strings.Contains(normalized, "no route")
	case http.StatusMethodNotAllowed:
		return strings.Contains(normalized, "method not allowed") ||
			strings.Contains(normalized, "cannot post /v1/responses")
	default:
		return false
	}
}

func (ra *relayAttempt) applyTransformOptions() {
	ra.internalRequest.TransformOptions.AnthropicAutoCacheControl = false
	// Channel cloak mode "never" disables Claude identity simulation end to end:
	// header defaults are already gated by shouldApplyChannelCloak; this flag carries
	// the same decision into the Anthropic outbound transformer so it skips the
	// synthetic billing-header / agent-identity system blocks too.
	ra.internalRequest.TransformOptions.SuppressClaudeIdentity = !shouldApplyChannelCloak(ra.channel.Cloak)

	if openAIPromptCacheKeyChannel(ra.channel.Type) {
		if enabled, err := op.SettingGetBool(dbmodel.SettingKeyOpenAIAutoPromptCacheKey); err == nil {
			convRoot := ""
			if ra.internalRequest != nil && ra.internalRequest.PreviousResponseID != nil {
				convRoot = responsesConversationRootForRequest(ra.context(), *ra.internalRequest.PreviousResponseID, ra.apiKeyID, ra.userID)
			}
			applyOpenAIAutoPromptCacheKeyWithSession(ra.internalRequest, ra.channel.Type, ra.userID, ra.apiKeyID, ra.channel.Cloak.ProfileID, ra.requestModel, convRoot, enabled)
		}
	}
	ra.prepareCodexRequestFingerprint()
	ra.prepareCodexRequestShape()
	ra.ensureClaudeMetadataUserID()

	if ra.channel.Type != outbound.OutboundTypeAnthropic {
		return
	}

	// Plain responses clients (e.g. Cursor) targeting a Claude channel rely on
	// previous_response_id, which Anthropic does not support. Replay the prior
	// turn's history into messages so multi-turn conversations continue.
	ra.bridgeResponsesHistoryForAnthropic()

	if ra.channel.AnthropicContext1M {
		ra.internalRequest.TransformOptions.AnthropicOneMillionBeta = true
	}
	ra.prepareClaudeOneMillionPlainClientShape()

	enabled, err := op.SettingGetBool(dbmodel.SettingKeyAnthropicAutoCacheControl)
	if err != nil {
		return
	}
	ra.internalRequest.TransformOptions.AnthropicAutoCacheControl = enabled
}

func (ra *relayAttempt) routingCapabilityKey() string {
	if ra == nil {
		return ""
	}
	return routingCapabilityKey(ra.internalRequest, ra.channel)
}

func routingCapabilityKey(req *model.InternalLLMRequest, channel *dbmodel.Channel) string {
	if req == nil {
		return ""
	}
	capabilities := make([]string, 0, 3)
	if req.Stream != nil && *req.Stream {
		capabilities = append(capabilities, "stream")
	}
	if channel != nil && channel.AnthropicContext1M {
		capabilities = append(capabilities, "anthropic_context_1m")
	} else if model.AnthropicRequestWantsOneMillionBeta(req) {
		capabilities = append(capabilities, "anthropic_context_1m")
	}
	if req.IsEmbeddingRequest() {
		capabilities = append(capabilities, "embedding")
	}
	return strings.Join(capabilities, "+")
}

func (ra *relayAttempt) prepareClaudeOneMillionPlainClientShape() {
	if ra == nil || ra.internalRequest == nil || !model.AnthropicRequestWantsOneMillionBeta(ra.internalRequest) {
		return
	}
	if isNativeAnthropicClaudeShape(ra.internalRequest) {
		return
	}
	// Only the functional 1M runtime shape (reasoning effort / auto-compact context
	// management) belongs here. The Claude identity — metadata.user_id and the
	// agent-identity system block — is injected exclusively by the cloak-gated paths
	// (ensureClaudeMetadataUserID and the Anthropic outbound transformer's
	// convertSystemPrompt), so it is suppressed correctly when cloak mode is "never".
	// Re-synthesising identity here would both leak it under cloak=never and duplicate
	// those canonical builders, so this path deliberately does not touch it.
	applyClaudeOneMillionRuntimeShape(ra.internalRequest)
}

func isNativeAnthropicClaudeShape(req *model.InternalLLMRequest) bool {
	if req == nil || req.RawAPIFormat != model.APIFormatAnthropicMessage {
		return false
	}
	if rawJSONPresentRelay(req.AnthropicThinking) ||
		rawJSONPresentRelay(req.AnthropicOutputConfig) ||
		rawJSONPresentRelay(req.AnthropicContextManagement) {
		return true
	}
	if req.Metadata != nil && strings.TrimSpace(req.Metadata["user_id"]) != "" && messagesContainSystemPrompt(req.Messages) {
		return true
	}
	return false
}

func rawJSONPresentRelay(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return trimmed != "" && trimmed != "null"
}

func applyClaudeOneMillionRuntimeShape(req *model.InternalLLMRequest) {
	if req == nil {
		return
	}
	if effort := claudeCLIReasoningEffort(); effort != "" && req.ReasoningEffort == "" && !req.AdaptiveThinking {
		req.ReasoningEffort = effort
		req.AdaptiveThinking = true
	}
	if settingBool(dbmodel.SettingKeyClaudeCLIAutoCompact, false) && len(req.AnthropicContextManagement) == 0 {
		req.AnthropicContextManagement = json.RawMessage(`{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]}`)
	}
}

func claudeCLIReasoningEffort() string {
	effort := strings.ToLower(strings.TrimSpace(settingString(dbmodel.SettingKeyClaudeCLIReasoningEffort, "auto")))
	switch effort {
	case "", "auto", "off", "false", "disabled":
		// auto / unset: do not inject a forced effort, follow the client request as-is.
		return ""
	case "low", "medium", "high":
		return effort
	default:
		return ""
	}
}

func messagesContainSystemPrompt(messages []model.Message) bool {
	for _, msg := range messages {
		switch strings.ToLower(strings.TrimSpace(msg.Role)) {
		case "system", "developer":
			return true
		}
	}
	return false
}

// copyHeaders 复制请求头，过滤 hop-by-hop 头
func (ra *relayAttempt) copyHeaders(outboundRequest *http.Request) {
	for key, values := range ra.c.Request.Header {
		if !shouldForwardClientHeader(key) {
			continue
		}
		if strings.EqualFold(key, "Accept") && strings.TrimSpace(outboundRequest.Header.Get("Accept")) != "" {
			continue
		}
		for _, value := range values {
			outboundRequest.Header.Set(key, value)
		}
	}
	ra.applyHeaderDefaults(outboundRequest)
	if len(ra.channel.CustomHeader) > 0 {
		for _, header := range ra.channel.CustomHeader {
			outboundRequest.Header.Set(header.HeaderKey, header.HeaderValue)
		}
	}
	// Re-assert the codex "no Accept-Encoding" fingerprint invariant AFTER CustomHeader.
	// The request builder sets none and both transports run DisableCompression, so the
	// ONLY way the header can reappear on the Responses path is an operator-configured
	// per-channel CustomHeader (the shipped openaiPython preset even carries
	// `Accept-Encoding: identity`). Strip it so a stray/preset override can't silently
	// re-introduce exactly the tell the codex fix removed.
	enforceCodexNoAcceptEncoding(ra.channel.Type, outboundRequest.Header)
}

// enforceCodexNoAcceptEncoding strips Accept-Encoding from a codex (OpenAI Responses)
// outbound request. Genuine Codex CLI (reqwest) sends none; claude and other channel
// types are left untouched (claude legitimately advertises `gzip, deflate, br, zstd`).
func enforceCodexNoAcceptEncoding(channelType outbound.OutboundType, header http.Header) {
	if channelType == outbound.OutboundTypeOpenAIResponse {
		header.Del("Accept-Encoding")
	}
}

// sendRequest 发送 HTTP 请求
func (ra *relayAttempt) sendRequest(req *http.Request) (*http.Response, error) {
	httpClient, err := helper.ChannelHttpClient(ra.channel)
	if err != nil {
		log.Warnf("failed to get http client: %v", err)
		return nil, err
	}

	response, err := helper.DoPreserveMethodRedirect(httpClient, req)
	if err != nil {
		log.Warnf("failed to send request: %v", err)
		return nil, err
	}

	return response, nil
}

// handleStreamResponse 处理流式响应
func (ra *relayAttempt) handleStreamResponse(ctx context.Context, response *http.Response, outAdapter model.Outbound) error {
	if ct := response.Header.Get("Content-Type"); ct != "" && !strings.Contains(strings.ToLower(ct), "text/event-stream") {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 16*1024))
		return fmt.Errorf("upstream returned non-SSE content-type %q for stream request: %s", ct, string(body))
	}

	// We advertise a real claude-cli Accept-Encoding, so decompress any upstream
	// Content-Encoding before the SSE reader (no-op on the common identity path).
	if err := unwrapResponseEncoding(response); err != nil {
		return fmt.Errorf("failed to unwrap upstream response encoding: %w", err)
	}

	// 设置 SSE 响应头
	ra.c.Header("Content-Type", "text/event-stream")
	ra.c.Header("Cache-Control", "no-cache")
	ra.c.Header("Connection", "keep-alive")
	ra.c.Header("X-Accel-Buffering", "no")

	firstToken := true

	type sseReadResult struct {
		eventType string
		data      string
		err       error
	}
	results := make(chan sseReadResult, 1)
	go func() {
		defer close(results)
		readCfg := &sse.ReadConfig{MaxEventSize: maxSSEEventSize}
		for ev, err := range sse.Read(response.Body, readCfg) {
			if err != nil {
				results <- sseReadResult{err: err}
				return
			}
			results <- sseReadResult{eventType: ev.Type, data: ev.Data}
		}
	}()

	var firstTokenTimer *time.Timer
	var firstTokenC <-chan time.Time
	if firstToken && ra.firstTokenTimeOutSec > 0 {
		firstTokenTimer = time.NewTimer(time.Duration(ra.firstTokenTimeOutSec) * time.Second)
		firstTokenC = firstTokenTimer.C
		defer func() {
			if firstTokenTimer != nil {
				firstTokenTimer.Stop()
			}
		}()
	}

	var keepaliveC <-chan time.Time
	var keepaliveTicker *time.Ticker
	keepaliveInterval := currentStreamKeepaliveInterval()
	if keepaliveInterval > 0 {
		keepaliveTicker = time.NewTicker(keepaliveInterval)
		keepaliveC = keepaliveTicker.C
		defer keepaliveTicker.Stop()
	}
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
	lastWriteAt := time.Now()
	streamDoneSeen := false
	upstreamResponsesCompletedSeen := false
	upstreamTerminalSeen := false
	sawUpstreamCompletion := false
	seenMeaningfulChunk := false
	requireResponsesCompleted := ra.requiresUpstreamResponsesCompleted(outAdapter)
	streamTerminalSeen := func() bool {
		if !seenMeaningfulChunk {
			return false
		}
		if streamDoneSeen {
			return true
		}
		if upstreamTerminalSeen {
			return true
		}
		return requireResponsesCompleted && upstreamResponsesCompletedSeen
	}
	writeStreamData := func(data []byte) error {
		if len(data) == 0 {
			return nil
		}
		if _, err := ra.c.Writer.Write(data); err != nil {
			log.Infof("client disconnected during stream write: %v", err)
			return fmt.Errorf("client disconnected during stream write: %w", err)
		}
		ra.c.Writer.Flush()
		lastWriteAt = time.Now()
		return nil
	}
	writeSynthesizedDone := func() error {
		if streamDoneSeen || !ra.shouldSynthesizeStreamDone() {
			return nil
		}
		data, err := ra.synthesizeStreamDone(ctx)
		if err != nil {
			return err
		}
		if err := writeStreamData(data); err != nil {
			return err
		}
		streamDoneSeen = true
		return nil
	}

	if data, err := ra.streamPreludeData(ctx, outAdapter); err != nil {
		return err
	} else if len(data) > 0 {
		if err := writeStreamData(data); err != nil {
			return err
		}
		if firstToken {
			ra.setFirstTokenTime(time.Now())
			firstToken = false
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
	}

	for {
		select {
		case <-ctx.Done():
			log.Infof("client disconnected, stopping stream")
			if streamTerminalSeen() {
				log.Infof("client disconnected after stream terminal event; treating stream as completed")
				return nil
			}
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("client disconnected during stream: %w", err)
			}
			return errors.New("client disconnected during stream")
		case <-firstTokenC:
			log.Warnf("first token timeout (%ds), switching channel", ra.firstTokenTimeOutSec)
			_ = response.Body.Close()
			return fmt.Errorf("first token timeout (%ds)", ra.firstTokenTimeOutSec)
		case <-keepaliveC:
			if time.Since(lastWriteAt) >= keepaliveInterval {
				if err := writeStreamData(ra.streamKeepaliveData()); err != nil {
					return err
				}
			}
		case <-dataTimeoutC:
			log.Warnf("stream data interval timeout (%s), closing upstream stream", dataIntervalTimeout)
			_ = response.Body.Close()
			return &localRelayError{
				status:   http.StatusGatewayTimeout,
				code:     "octopus_upstream_stream_timeout",
				strategy: "stream_data_interval_timeout;upstream_forwarded=true",
				message:  fmt.Sprintf("upstream stream timed out waiting for SSE event (%s)", dataIntervalTimeout),
			}
		case r, ok := <-results:
			if !ok {
				log.Infof("stream end")
				if !seenMeaningfulChunk && !sawUpstreamCompletion {
					return errors.New("upstream stream ended without internal response")
				}
				if !streamDoneSeen && ra.shouldSynthesizeStreamDone() {
					data, err := ra.synthesizeStreamDone(ctx)
					if err != nil {
						return err
					}
					if err := writeStreamData(data); err != nil {
						return err
					}
				}
				return nil
			}
			if r.err != nil {
				log.Warnf("failed to read event: %v", r.err)
				return fmt.Errorf("failed to read stream event: %w", r.err)
			}
			resetDataTimeout()
			eventType := responsesStreamEventType(r.data)
			if eventType == "response.completed" {
				upstreamResponsesCompletedSeen = true
			}
			isTerminalEvent := upstreamStreamTerminalEvent(r.eventType, r.data)
			if !isTerminalEvent && ra.shouldTreatResponsesToolCallDoneAsTerminal(outAdapter, r.data) {
				isTerminalEvent = true
			}
			if isTerminalEvent {
				upstreamTerminalSeen = true
			}
			if upstreamStreamCompletedEvent(r.eventType, r.data) {
				sawUpstreamCompletion = true
			}
			if requireResponsesCompleted && responsesStreamEventIsPrelude(eventType) {
				if _, err := outAdapter.TransformStream(ctx, []byte(r.data)); err != nil {
					log.Warnf("failed to transform responses stream prelude: %v", err)
					return fmt.Errorf("failed to transform responses stream prelude: %w", err)
				}
				continue
			}
			isDoneEvent := strings.HasPrefix(strings.TrimSpace(r.data), "[DONE]")
			if isDoneEvent {
				streamDoneSeen = true
				if !seenMeaningfulChunk && !sawUpstreamCompletion {
					return errors.New("upstream stream ended without internal response")
				}
			}

			data, internalStream, err := ra.transformStreamChunk(ctx, r.data, outAdapter)
			if internalStream != nil && internalStream.Object != "[DONE]" && internalStreamHasMeaningfulResponse(internalStream) {
				seenMeaningfulChunk = true
			}
			if err != nil {
				return &localRelayError{
					status:   http.StatusBadGateway,
					code:     "octopus_upstream_stream_error",
					strategy: "stream_transform_error;upstream_forwarded=true",
					message:  fmt.Sprintf("failed to transform stream event: %v", err),
				}
			}
			if len(data) == 0 {
				if isStreamKeepaliveEvent(r.eventType, r.data) {
					if writeErr := writeStreamData(ra.streamKeepaliveData()); writeErr != nil {
						return writeErr
					}
				}
				if isTerminalEvent && seenMeaningfulChunk {
					if writeErr := writeSynthesizedDone(); writeErr != nil {
						return writeErr
					}
					return nil
				}
				continue
			}
			if firstToken {
				ra.setFirstTokenTime(time.Now())
				firstToken = false
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

			if err := writeStreamData(data); err != nil {
				if streamTerminalSeen() && isClientAbortError(err) {
					log.Infof("client disconnected while writing terminal stream data; treating stream as completed")
					return nil
				}
				return err
			}
			if streamTerminalSeen() {
				if err := writeSynthesizedDone(); err != nil {
					return err
				}
				return nil
			}
		}
	}
}

func (ra *relayAttempt) streamPreludeData(ctx context.Context, outAdapter model.Outbound) ([]byte, error) {
	if ra == nil || ra.inAdapter == nil || ra.internalRequest == nil {
		return nil, nil
	}
	if ra.inboundType != inbound.InboundTypeOpenAIResponse {
		return nil, nil
	}
	switch outAdapter.(type) {
	case *openaiOutbound.ChatOutbound, *openaiOutbound.CustomChatOutbound:
	default:
		return nil, nil
	}
	if ra.internalRequest.Stream == nil || !*ra.internalRequest.Stream {
		return nil, nil
	}
	return ra.inAdapter.TransformStream(ctx, &model.InternalLLMResponse{
		ID:      fmt.Sprintf("resp_%d", time.Now().UnixNano()),
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   ra.requestModel,
	})
}

func (ra *relayAttempt) shouldSynthesizeStreamDone() bool {
	if ra == nil {
		return false
	}
	switch ra.inboundType {
	case inbound.InboundTypeOpenAIChat, inbound.InboundTypeOpenAIResponse, inbound.InboundTypeAnthropic:
		return true
	default:
		return false
	}
}

func (ra *relayAttempt) synthesizeStreamDone(ctx context.Context) ([]byte, error) {
	if ra == nil || ra.inAdapter == nil {
		return nil, nil
	}
	return ra.inAdapter.TransformStream(ctx, &model.InternalLLMResponse{Object: "[DONE]"})
}

func (ra *relayAttempt) handleStreamResponseAsNonStream(ctx context.Context, response *http.Response, outAdapter model.Outbound) error {
	if ct := response.Header.Get("Content-Type"); ct != "" && !strings.Contains(strings.ToLower(ct), "text/event-stream") {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 16*1024))
		return fmt.Errorf("upstream returned non-SSE content-type %q for forced responses stream request: %s", ct, string(body))
	}

	// Decompress any upstream Content-Encoding before the SSE reader (see the
	// stream handler above); no-op on the common identity path.
	if err := unwrapResponseEncoding(response); err != nil {
		return fmt.Errorf("failed to unwrap upstream response encoding: %w", err)
	}

	type sseReadResult struct {
		data string
		err  error
	}
	results := make(chan sseReadResult, 1)
	go func() {
		defer close(results)
		readCfg := &sse.ReadConfig{MaxEventSize: maxSSEEventSize}
		for ev, err := range sse.Read(response.Body, readCfg) {
			if err != nil {
				results <- sseReadResult{err: err}
				return
			}
			results <- sseReadResult{data: ev.Data}
		}
	}()

	// No downstream client stream here, so we only guard against a stalled or
	// ping-only upstream with the same idle cutoff handleStreamResponse uses.
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

	streamDoneSeen := false
	seenMeaningfulChunk := false
	sawUpstreamCompletion := false
	requireResponsesCompleted := ra.requiresUpstreamResponsesCompleted(outAdapter)
readLoop:
	for {
		select {
		case <-ctx.Done():
			_ = response.Body.Close()
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("client disconnected during forced responses stream: %w", err)
			}
			return errors.New("client disconnected during forced responses stream")
		case <-dataTimeoutC:
			log.Warnf("forced responses stream data interval timeout (%s), closing upstream stream", dataIntervalTimeout)
			_ = response.Body.Close()
			return &localRelayError{
				status:   http.StatusGatewayTimeout,
				code:     "octopus_upstream_stream_timeout",
				strategy: "stream_data_interval_timeout;upstream_forwarded=true",
				message:  fmt.Sprintf("upstream stream timed out waiting for SSE event (%s)", dataIntervalTimeout),
			}
		case r, ok := <-results:
			if !ok {
				break readLoop
			}
			if r.err != nil {
				return fmt.Errorf("failed to read stream event: %w", r.err)
			}
			resetDataTimeout()
			eventType := responsesStreamEventType(r.data)
			if requireResponsesCompleted && responsesStreamEventIsPrelude(eventType) {
				if _, err := outAdapter.TransformStream(ctx, []byte(r.data)); err != nil {
					return fmt.Errorf("failed to transform responses stream prelude: %w", err)
				}
				continue
			}
			if upstreamStreamCompletedEvent(eventType, r.data) || ra.shouldTreatResponsesToolCallDoneAsTerminal(outAdapter, r.data) {
				sawUpstreamCompletion = true
			}
			isDoneEvent := strings.HasPrefix(strings.TrimSpace(r.data), "[DONE]")
			if isDoneEvent {
				streamDoneSeen = true
				if !seenMeaningfulChunk && !sawUpstreamCompletion {
					return errors.New("upstream stream ended without internal response")
				}
			}

			_, internalStream, err := ra.transformStreamChunk(ctx, r.data, outAdapter)
			if err != nil {
				return err
			}
			if internalStream != nil && internalStream.Object != "[DONE]" && internalStreamHasMeaningfulResponse(internalStream) {
				seenMeaningfulChunk = true
				if ra.metrics != nil && ra.metrics.FirstTokenTime.IsZero() {
					ra.setFirstTokenTime(time.Now())
				}
			}
		}
	}
	if !seenMeaningfulChunk && !sawUpstreamCompletion {
		return errors.New("upstream stream ended without internal response")
	}
	if !streamDoneSeen && ra.shouldSynthesizeStreamDone() {
		if _, err := ra.synthesizeStreamDone(ctx); err != nil {
			return err
		}
	}

	internalResponse, err := ra.inAdapter.GetInternalResponse(ctx)
	if err != nil {
		return fmt.Errorf("failed to aggregate forced responses stream: %w", err)
	}
	if internalResponse == nil {
		return errors.New("upstream stream ended without internal response")
	}
	inResponse, err := ra.inAdapter.TransformResponse(ctx, internalResponse)
	if err != nil {
		return fmt.Errorf("failed to transform forced responses stream: %w", err)
	}
	ra.c.Data(http.StatusOK, "application/json", inResponse)
	return nil
}

func (ra *relayAttempt) handleNonStreamResponseAsStream(ctx context.Context, response *http.Response, outAdapter model.Outbound) error {
	limitUpstreamResponseBody(response)
	internalResponse, err := outAdapter.TransformResponse(ctx, response)
	if err != nil {
		log.Warnf("failed to transform fallback non-stream response: %v", err)
		return fmt.Errorf("failed to transform fallback non-stream response: %w", err)
	}

	ra.c.Header("Content-Type", "text/event-stream")
	ra.c.Header("Cache-Control", "no-cache")
	ra.c.Header("Connection", "keep-alive")
	ra.c.Header("X-Accel-Buffering", "no")
	ra.c.Header("X-Octopus-Stream-Fallback", "non-stream-upstream")

	for _, chunk := range internalResponseToStreamChunks(internalResponse) {
		data, err := ra.inAdapter.TransformStream(ctx, chunk)
		if err != nil {
			log.Warnf("failed to transform fallback stream: %v", err)
			return fmt.Errorf("failed to transform fallback stream: %w", err)
		}
		if len(data) == 0 {
			continue
		}
		if ra.metrics != nil && ra.metrics.FirstTokenTime.IsZero() {
			ra.setFirstTokenTime(time.Now())
		}
		if _, err := ra.c.Writer.Write(data); err != nil {
			log.Infof("client disconnected during fallback stream write: %v", err)
			return fmt.Errorf("client disconnected during fallback stream write: %w", err)
		}
		ra.c.Writer.Flush()
	}
	log.Infof("stream fallback emitted non-stream upstream response")
	return nil
}

func internalResponseToStreamChunks(resp *model.InternalLLMResponse) []*model.InternalLLMResponse {
	if resp == nil {
		return nil
	}

	chunks := make([]*model.InternalLLMResponse, 0, len(resp.Choices)+2)
	base := func() *model.InternalLLMResponse {
		return &model.InternalLLMResponse{
			ID:                resp.ID,
			Object:            "chat.completion.chunk",
			Created:           resp.Created,
			Model:             resp.Model,
			SystemFingerprint: resp.SystemFingerprint,
			ServiceTier:       resp.ServiceTier,
		}
	}

	start := base()
	if resp.Usage != nil {
		start.Usage = resp.Usage
	}
	chunks = append(chunks, start)

	for _, choice := range resp.Choices {
		if choice.Message != nil {
			for _, delta := range messageToSyntheticDeltas(choice.Index, choice.Message) {
				chunk := base()
				chunk.Choices = []model.Choice{delta}
				chunks = append(chunks, chunk)
			}
		}
		finishReason := choice.FinishReason
		if finishReason == nil {
			stop := "stop"
			finishReason = &stop
		}
		finish := base()
		finish.Choices = []model.Choice{{
			Index:        choice.Index,
			Delta:        &model.Message{Role: "assistant"},
			FinishReason: finishReason,
		}}
		chunks = append(chunks, finish)
	}

	if resp.Usage != nil {
		usage := base()
		usage.Usage = resp.Usage
		chunks = append(chunks, usage)
	}
	return chunks
}

func messageToSyntheticDeltas(index int, msg *model.Message) []model.Choice {
	if msg == nil {
		return nil
	}
	deltas := make([]model.Choice, 0, 4)
	// Carry reasoning content together with its Anthropic thinking signature in the
	// same delta. The Anthropic inbound stream synthesizer only emits a
	// signature_delta while the thinking content block is still open (it does not
	// open one itself), so the signature must accompany the reasoning chunk;
	// otherwise the rebuilt thinking block loses its signature and the next turn is
	// rejected by Anthropic. A signature with no reasoning text is still forwarded
	// so it is not silently dropped.
	reasoning := strings.TrimSpace(deref(msg.ReasoningContent))
	signature := strings.TrimSpace(deref(msg.ReasoningSignature))
	if reasoning != "" || signature != "" {
		reasoningDelta := &model.Message{Role: "assistant"}
		if reasoning != "" {
			text := *msg.ReasoningContent
			reasoningDelta.ReasoningContent = &text
		}
		if signature != "" {
			sig := *msg.ReasoningSignature
			reasoningDelta.ReasoningSignature = &sig
		}
		deltas = append(deltas, model.Choice{
			Index: index,
			Delta: reasoningDelta,
		})
	}
	if text := messageText(msg); text != "" {
		deltas = append(deltas, model.Choice{
			Index: index,
			Delta: &model.Message{
				Role: "assistant",
				Content: model.MessageContent{
					Content: &text,
				},
			},
		})
	}
	// Preserve image parts (msg.Images plus any image parts inlined in
	// MultipleContent). Without this, an Anthropic stream that falls back to a
	// non-stream upstream loses generated images when re-synthesized as a stream.
	// The downstream OpenAI inbound merges Delta.Images / Delta.Content.MultipleContent
	// back into the aggregated message.
	if images := messageImageParts(msg); len(images) > 0 {
		deltas = append(deltas, model.Choice{
			Index: index,
			Delta: &model.Message{
				Role:   "assistant",
				Images: images,
			},
		})
	}
	if len(msg.ToolCalls) > 0 {
		copied := append([]model.ToolCall(nil), msg.ToolCalls...)
		deltas = append(deltas, model.Choice{
			Index: index,
			Delta: &model.Message{
				Role:      "assistant",
				ToolCalls: copied,
			},
		})
	}
	if len(deltas) == 0 {
		deltas = append(deltas, model.Choice{
			Index: index,
			Delta: &model.Message{Role: "assistant"},
		})
	}
	return deltas
}

func messageText(msg *model.Message) string {
	if msg == nil {
		return ""
	}
	if msg.Content.Content != nil {
		return *msg.Content.Content
	}
	var builder strings.Builder
	for _, part := range msg.Content.MultipleContent {
		if strings.EqualFold(strings.TrimSpace(part.Type), "text") && part.Text != nil {
			builder.WriteString(*part.Text)
		}
	}
	return builder.String()
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// messageImageParts collects image content parts carried by a message so the
// non-stream -> stream synthesizer does not drop generated images. It includes
// both msg.Images (where providers like Gemini-via-OpenAI place generated
// images) and any image parts inlined in Content.MultipleContent.
func messageImageParts(msg *model.Message) []model.MessageContentPart {
	if msg == nil {
		return nil
	}
	parts := make([]model.MessageContentPart, 0, len(msg.Images)+len(msg.Content.MultipleContent))
	parts = append(parts, msg.Images...)
	for _, part := range msg.Content.MultipleContent {
		if isImagePart(part) {
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		return nil
	}
	return parts
}

func isImagePart(part model.MessageContentPart) bool {
	if part.ImageURL != nil {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(part.Type)) {
	case "image", "image_url":
		return true
	default:
		return false
	}
}

func (ra *relayAttempt) transformStreamData(ctx context.Context, data string, outAdapter model.Outbound) ([]byte, error) {
	out, _, err := ra.transformStreamChunk(ctx, data, outAdapter)
	return out, err
}

func (ra *relayAttempt) transformStreamChunk(ctx context.Context, data string, outAdapter model.Outbound) ([]byte, *model.InternalLLMResponse, error) {
	internalStream, err := outAdapter.TransformStream(ctx, []byte(data))
	if err != nil {
		log.Warnf("failed to transform stream: %v", err)
		return nil, nil, err
	}
	if internalStream == nil {
		return nil, nil, nil
	}

	inStream, err := ra.inAdapter.TransformStream(ctx, internalStream)
	if err != nil {
		log.Warnf("failed to transform stream: %v", err)
		return nil, internalStream, err
	}

	return inStream, internalStream, nil
}

func (ra *relayAttempt) setFirstTokenTime(t time.Time) {
	if ra == nil || ra.metrics == nil {
		return
	}
	ra.metrics.SetFirstTokenTime(t)
}

func (ra *relayAttempt) requiresUpstreamResponsesCompleted(outAdapter model.Outbound) bool {
	if ra == nil || ra.inboundType != inbound.InboundTypeOpenAIResponse {
		return false
	}
	switch outAdapter.(type) {
	case *openaiOutbound.ResponseOutbound:
		return true
	default:
		return false
	}
}

func responsesStreamEventIsPrelude(eventType string) bool {
	switch strings.TrimSpace(eventType) {
	case "response.created", "response.in_progress":
		return true
	default:
		return false
	}
}

func internalStreamHasMeaningfulResponse(resp *model.InternalLLMResponse) bool {
	if resp == nil || resp.Object == "[DONE]" {
		return false
	}
	if usageHasMeaningfulResponse(resp.Usage) {
		return true
	}
	if len(resp.EmbeddingData) > 0 {
		return true
	}
	for _, choice := range resp.Choices {
		if choice.Message != nil && messageHasMeaningfulResponse(choice.Message) {
			return true
		}
		if choice.Delta != nil && messageHasMeaningfulResponse(choice.Delta) {
			return true
		}
	}
	return false
}

func usageHasMeaningfulResponse(usage *model.Usage) bool {
	if usage == nil {
		return false
	}
	if usage.CompletionTokens > 0 {
		return true
	}
	if usage.TotalTokens > 0 && usage.PromptTokens == 0 && usage.CacheCreationInputTokens == 0 && usage.PromptTokensDetails == nil {
		return true
	}
	if usage.CompletionTokensDetails != nil && usage.CompletionTokensDetails.ReasoningTokens > 0 {
		return true
	}
	return false
}

func messageHasMeaningfulResponse(msg *model.Message) bool {
	if msg == nil {
		return false
	}
	if strings.TrimSpace(messageText(msg)) != "" || strings.TrimSpace(msg.GetReasoningContent()) != "" || strings.TrimSpace(msg.Refusal) != "" {
		return true
	}
	if len(msg.ToolCalls) > 0 || len(msg.Images) > 0 || msg.Audio != nil {
		return true
	}
	return false
}

func responsesStreamEventType(data string) string {
	data = strings.TrimSpace(data)
	if data == "" || strings.HasPrefix(data, "[DONE]") {
		return ""
	}
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(data), &envelope); err != nil {
		return ""
	}
	return strings.TrimSpace(envelope.Type)
}

func (ra *relayAttempt) shouldTreatResponsesToolCallDoneAsTerminal(outAdapter model.Outbound, data string) bool {
	if ra == nil || ra.internalRequest == nil || ra.inboundType != inbound.InboundTypeOpenAIResponse {
		return false
	}
	switch outAdapter.(type) {
	case *openaiOutbound.ResponseOutbound:
	default:
		return false
	}
	if ra.internalRequest.ParallelToolCalls == nil || *ra.internalRequest.ParallelToolCalls {
		return false
	}
	return responsesToolCallDoneEvent(data)
}

func responsesToolCallDoneEvent(data string) bool {
	data = strings.TrimSpace(data)
	if data == "" || strings.HasPrefix(data, "[DONE]") {
		return false
	}
	var envelope struct {
		Type string `json:"type"`
		Item *struct {
			Type string `json:"type"`
		} `json:"item,omitempty"`
	}
	if err := json.Unmarshal([]byte(data), &envelope); err != nil {
		return false
	}
	if strings.TrimSpace(envelope.Type) != "response.output_item.done" || envelope.Item == nil {
		return false
	}
	switch strings.TrimSpace(envelope.Item.Type) {
	case "tool_call", "function_call", "local_shell_call", "tool_search_call", "custom_tool_call", "mcp_tool_call":
		return true
	default:
		return false
	}
}

func upstreamStreamTerminalEvent(eventType string, data string) bool {
	if strings.HasPrefix(strings.TrimSpace(data), "[DONE]") {
		return true
	}
	switch strings.TrimSpace(eventType) {
	case "message_stop", "response.completed":
		return true
	}
	switch responsesStreamEventType(data) {
	case "message_stop", "response.completed":
		return true
	default:
		return false
	}
}

// upstreamStreamCompletedEvent reports a REAL terminal completion from the
// upstream (message_stop / response.completed), excluding the [DONE] sentinel.
// [DONE] only marks the SSE channel closing, not a successful completion, so it
// must not be treated as "the model completed" when deciding whether an
// otherwise content-less stream is a legitimate empty completion vs a dropped one.
func upstreamStreamCompletedEvent(eventType string, data string) bool {
	if strings.HasPrefix(strings.TrimSpace(data), "[DONE]") {
		return false
	}
	switch strings.TrimSpace(eventType) {
	case "message_stop", "response.completed":
		return true
	}
	switch responsesStreamEventType(data) {
	case "message_stop", "response.completed":
		return true
	default:
		return false
	}
}

func isStreamKeepaliveEvent(eventType string, data string) bool {
	if strings.EqualFold(strings.TrimSpace(eventType), "ping") {
		return true
	}
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(data), &envelope); err != nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(envelope.Type), "ping")
}

func (ra *relayAttempt) streamKeepaliveData() []byte {
	if ra != nil && ra.inboundType == inbound.InboundTypeAnthropic {
		return []byte("event: ping\ndata: {\"type\":\"ping\"}\n\n")
	}
	return []byte(":\n\n")
}

// handleResponse 处理非流式响应
func (ra *relayAttempt) handleResponse(ctx context.Context, response *http.Response, outAdapter model.Outbound) error {
	limitUpstreamResponseBody(response)
	internalResponse, err := outAdapter.TransformResponse(ctx, response)
	if err != nil {
		log.Warnf("failed to transform response: %v", err)
		return fmt.Errorf("failed to transform outbound response: %w", err)
	}

	inResponse, err := ra.inAdapter.TransformResponse(ctx, internalResponse)
	if err != nil {
		log.Warnf("failed to transform response: %v", err)
		return fmt.Errorf("failed to transform inbound response: %w", err)
	}

	ra.c.Data(http.StatusOK, "application/json", inResponse)
	return nil
}

// collectResponse 收集响应信息
func (ra *relayAttempt) collectResponse() {
	internalResponse, err := ra.inAdapter.GetInternalResponse(ra.c.Request.Context())
	if err != nil || internalResponse == nil {
		return
	}

	ra.metrics.SetInternalResponse(internalResponse, ra.internalRequest.Model)
	ra.recordResponsesSessionFromInbound(internalResponse)
}

func paramOverrideValue(ptr *string) string {
	if ptr == nil || *ptr == "" {
		return ""
	}
	return *ptr
}

func endpointNameForInbound(inboundType inbound.InboundType, requestPath string) string {
	switch inboundType {
	case inbound.InboundTypeOpenAIChat:
		return "chat"
	case inbound.InboundTypeOpenAIResponse:
		return "responses"
	case inbound.InboundTypeAnthropic:
		return "messages"
	case inbound.InboundTypeOpenAIEmbedding:
		return "embeddings"
	case inbound.InboundTypeGemini:
		if strings.Contains(requestPath, ":streamGenerateContent") {
			return "gemini_stream_generate_content"
		}
		return "gemini_generate_content"
	default:
		return cleanRelayEndpointName(requestPath)
	}
}
