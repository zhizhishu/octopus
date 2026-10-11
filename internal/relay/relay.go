package relay

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/textproto"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bestruirui/octopus/internal/helper"
	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/relay/balancer"
	"github.com/bestruirui/octopus/internal/relay/intervention"
	"github.com/bestruirui/octopus/internal/server/resp"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	"github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	openaiOutbound "github.com/bestruirui/octopus/internal/transformer/outbound/openai"
	"github.com/bestruirui/octopus/internal/utils/log"
	"github.com/bestruirui/octopus/internal/utils/safe"
	"github.com/gin-gonic/gin"
	"github.com/tmaxmax/go-sse"
)

// Machine rescue keeps retrying until the request's total timeout or an explicit
// operator abort — there is no round cap. Capped exponential backoff (1s→2s→4s→8s→15s)
// prevents hammering; the request's own context deadline provides the safety valve.
// A Codex/Claude CLI that sits quietly waiting for a valid response will never see an
// "awaiting operator" intervention prompt — the machine keeps trying every available
// route group until one works or the clock runs out.
const maxRelayInterventionRounds = 0 // 0 = unlimited; machine rescue never exhausts

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
		writeRelayErrorPreStream(c, inboundType, http.StatusBadRequest, "invalid_request_error", "model_not_supported", "model not supported")
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
		saveRouteSelectionFailureRelayLog(c.Request.Context(), c, inboundType, internalRequest, routeSelectionUnconfigured, status, message)
		writeRelayErrorPreStream(c, inboundType, status, "api_error", "", message)
		return
	}
	preferStreamRouting := internalRequestPrefersStream(internalRequest)
	group := enrichGroupForSmartRouting(c.Request.Context(), routeResult.Group, preferStreamRouting)

	// 创建迭代器（策略排序 + 粘性优先）。stickyEnabled 按「分组模式 + 会话来源」分级：
	// 轮询/负载均衡模式下纯优化型会话(prompt_cache_key / oct 自造指纹)不 sticky、走真轮询，
	// previous_response_id / 线程会话等需正确性的来源仍 sticky。填充优先模式全程 sticky。
	stickyEnabled := routeStickyEnabled(group.Mode, clientSession.Source)
	iter := balancer.NewIteratorWithSession(group, apiKeyID, requestModel, clientSessionKey, stickyEnabled)
	iter.PrioritizeChannels(nativeProtocolChannelIDs(c.Request.Context(), inboundType, group.Items))
	prioritizeResponsesSessionOwner(c.Request.Context(), iter, internalRequest, apiKeyID, userID)
	if iter.Len() == 0 {
		saveRouteSelectionFailureRelayLog(c.Request.Context(), c, inboundType, internalRequest, routeSelectionNoCandidates, http.StatusServiceUnavailable, "no available channel")
		writeRelayErrorPreStream(c, inboundType, http.StatusServiceUnavailable, "api_error", "no_available_channel", "no available channel")
		return
	}

	// 初始化 Metrics
	metrics := NewRelayMetrics(apiKeyID, userID, c.GetString("request_ip"), requestModel, internalRequest)
	// 客户端识别: 原样快照调用端 User-Agent(落库时 512 截断)。纯观测字段——
	// 出站 UA 仍由 header 默认值统一覆盖, 这里只回答"谁在调用"。
	metrics.RequestUserAgent = c.Request.UserAgent()
	requestEndpoint := endpointNameForInbound(inboundType, c.Request.URL.Path)
	metrics.SetRequestEndpoint(requestEndpoint, c.Request.URL.Path)
	metrics.SetAccessPlan(routeResult.AccessPlan, routeResult.AccessRouteRule, routeResult.AccessRouteUsed)
	metrics.SetClientSession(clientSession)

	// 派生可被管理员或内部终止的请求上下文
	clientRequest := c.Request
	relayCtx, relayCancel := context.WithCancel(clientRequest.Context())
	originalRequest := clientRequest.WithContext(relayCtx)
	c.Request = originalRequest

	// 创建实时请求状态，立即推送 "running" 给 SSE 订阅者
	requestState := newRequestStateWithCancel(requestModel, requestEndpoint, userID, apiKeyID, relayCancel)

	baseMessages := append([]model.Message(nil), internalRequest.Messages...)
	// The responses history bridges (chat / Anthropic) and codex shape clear
	// PreviousResponseID / ResponsesInputRaw on the SHARED internalRequest once they
	// inline a prior turn's history. Capture the client's originals so every
	// retry/failover attempt is rebuilt from the real request rather than a
	// context-stripped one — without this, a 2nd attempt sees a nil previous_response_id,
	// skips the bridge entirely, and silently forwards only the incremental turn.
	basePreviousResponseID := internalRequest.PreviousResponseID
	baseResponsesInputRaw := cloneRawJSONMessage(internalRequest.ResponsesInputRaw)
	// baseResponsesInstructions is the attempt-start copy of the Responses top-level
	// instructions. applyInboundRedaction rewrites it to placeholders on the SHARED
	// internalRequest, so each attempt/key/transient retry must be rebuilt from this
	// original (pointer kept verbatim: nil stays nil, suppressCodexHoistedContext's
	// non-nil "" sentinel stays non-nil) — otherwise a failed attempt's placeholder
	// instructions ride into the next channel's session, whose flags may not cover the
	// token (audit #5).
	baseResponsesInstructions := internalRequest.ResponsesInstructions

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
		requestState:        requestState,
		totalTimeoutSec:     group.TotalTimeOut,
	}
	// 第三道钟（见 request_total_timeout.go）：整次请求的绝对时长上限。死线在此刻钉死——
	// 之后任何事件、换家、救援回合都只能消耗剩下的部分，无法重置或延长它。
	if budget := resolvedRequestTotalBudget(req.totalTimeoutSec); budget > 0 {
		req.totalBudget = budget
		req.totalDeadline = time.Now().Add(budget)
		// Allocate the clock with the deadline, never lazily: the ceiling's timer callbacks
		// and the streaming loop read content progress through it from different goroutines,
		// so a nil-to-value transition at first cut would be a race.
		req.totalClock = &relayTotalClock{}
	}

	var (
		lastErr            error
		retryAfterFloor    time.Duration // 距上次「已按上游 Retry-After 等待」以来见到的最大 Retry-After（0 = 上游没给）
		allAttempts        []dbmodel.ChannelAttempt
		triedReturnGroup   bool
		interventionRounds int
		fatalClientErr     error // 确定性客户端错误(上下文超长/请求体非法): 停止遍历与救援, 原样透传真实 4xx
		requestSucceeded   bool  // 重试前的 lastErr 可能仍非 nil，终态需按真实成功结果判断

		// Intervention state lifted above runIterator to ensure exactly one pending ID,
		// one context/cancel, one keepalive goroutine, and single cleanup across the full request lifecycle.
		pendingInterventionID     string
		interventionCtx           context.Context
		interventionCancel        context.CancelFunc
		stopInterventionKeepalive func()
		interventionRegistered    bool
		sawNoBreakerChannel       bool
		operatorRescueRequested   bool

		// ONE automatic-recovery window (see rescue_window.go). recoveryStartedAt pins the
		// first rescuable failure; every later retry / branch switch (plain<->no-breaker<->
		// operator) / channel change / operator click shares it, so the cap can only be
		// tightened, never reset. rescueFired marks the cancel as a deadline (rescue_timeout)
		// rather than an operator abort (rescue_stopped). rescueExpired records that the window
		// elapsed mid-sweep, so the original-group fallback is skipped and no new attempt starts.
		recoveryStartedAt time.Time
		rescueFired       = &atomic.Bool{}
		rescueExpired     bool
	)
	// The attempt context is disposable; the client/rescue deadline remains its parent.
	beginControlledAttempt := func() func() {
		parent := c.Request
		attemptCtx, cancel := context.WithCancel(parent.Context())
		// R10: an attempt started BEFORE the intervention hold exists (a pre-hold candidate
		// on a later key) would otherwise be a plain WithCancel child of the client context
		// with no server-side clock — the rescue deadline timer is not armed yet and cannot
		// cancel it, and the sweep guard only runs between attempts. Pre-hold attempts are
		// therefore bounded by a stoppable cancel timer at recoveryStartedAt+autoRescueCap
		// so the ONE automatic-recovery window holds for the whole request. Attempts inside
		// the hold already sit under interventionCtx (its timer fires at the deadline, and
		// armRescueDeadline re-arms it tighten-only on every hold re-entry); the pre-hold
		// cancel timer is stopped by releaseRescueDeadline, so a recovered delivery that
		// commits mid-attempt survives past the cap.
		var stopAttemptDeadline func()
		if !recoveryStartedAt.IsZero() && interventionCtx == nil {
			stopAttemptDeadline = req.armAttemptDeadlineCancel(recoveryStartedAt.Add(autoRescueCap), cancel)
		}
		c.Request = parent.WithContext(attemptCtx)
		requestState.bindAttemptCancel(cancel, internalRequestPrefersStream(internalRequest))
		return func() {
			requestState.bindAttemptCancel(nil, false)
			c.Request = parent
			if stopAttemptDeadline != nil {
				stopAttemptDeadline()
			}
			cancel()
		}
	}
	// paceRetryWait honours the upstream's Retry-After before another round of attempts
	// starts. retryAfterFloor is the largest Retry-After seen since the last paced wait: a
	// sweep may mix targets, and a 429 that asked for 2s must not be re-hit just because the
	// attempt after it (a 500 that asked for nothing) overwrote the record. Cross-channel
	// failover WITHIN one sweep is deliberately NOT paced — a different channel is not the
	// throttled one, and switching instantly is the whole point of having a pool. Returns
	// false when the wait was cut short by a cancel (client gone / rescue deadline), leaving
	// the caller's liveness checks to terminate cleanly.
	paceRetryWait := func(ctx context.Context) bool {
		if retryAfterFloor <= 0 {
			return true
		}
		wait := retryAfterFloor
		retryAfterFloor = 0
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return true
		}
	}
	defer func() {
		if stopInterventionKeepalive != nil {
			stopInterventionKeepalive()
		}
		if interventionRegistered && pendingInterventionID != "" {
			intervention.Cancel(pendingInterventionID)
		}
		// Inspect the request context before cleanup cancellation changes it.
		// Rescue cleanup is not an operator/client cancellation.
		// 请求结束时标记最终状态（先判断终态）
		if requestSucceeded {
			requestState.markSuccess()
		} else if relayCtx.Err() != nil {
			requestState.markCanceled()
		} else if lastErr != nil {
			requestState.markFailed(lastErr.Error())
		} else {
			requestState.markFailed("request ended without a successful upstream response")
		}
		req.releaseRescueDeadline()
		if interventionCancel != nil {
			interventionCancel()
		}
		c.Request = clientRequest
		relayCancel()
	}()

runIterator:
	req.iter = iter
attemptChannels:
	for iter.Next() {
		if clientGone, stopErr := relayStop(originalRequest.Context(), c.Request.Context(), interventionCtx, rescueFired); clientGone {
			log.Infof("client request context canceled, stopping retry")
			metrics.Save(originalRequest.Context(), false, originalRequest.Context().Err(), append(allAttempts, iter.Attempts()...))
			return
		} else if stopErr != nil {
			lastErr = stopErr
			break attemptChannels
		}
		if rescueDeadlineExpired(recoveryStartedAt, time.Now()) {
			// The single automatic-recovery window already elapsed: start no further attempt.
			// Flow on to the hold block, which opens an immediately-expired window and
			// terminates cleanly with octopus_rescue_timeout.
			rescueExpired = true
			break attemptChannels
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
		if channel.DisableCircuitBreaker {
			sawNoBreakerChannel = true
		}
		if internalRequest.IsImageGenerationRequest() && !isImageGenerationRequestCompatibleChannelType(channel.Type) {
			iter.Skip(channel.ID, 0, channel.Name, fmt.Sprintf("channel type not compatible with image generation request: %d", channel.Type))
			continue
		}

		// A DisableCircuitBreaker channel forwards every request like a direct client:
		// take ALL enabled keys ignoring cooldown/quarantine so a key that just returned
		// 429/5xx/401 is never benched — the client keeps its retry claw on the upstream.
		var availableKeys []dbmodel.ChannelKey
		if req.interventionKeyID > 0 {
			// Manual intervention is an explicit operator choice: try that enabled key even
			// if the automatic health filters would still keep it cooling down.
			availableKeys = channel.GetAllEnabledChannelKeys()
		} else if channel.DisableCircuitBreaker {
			availableKeys = channel.GetAllEnabledChannelKeys()
		} else {
			availableKeys = channel.GetAvailableChannelKeys()
		}
		if req.interventionKeyID > 0 {
			selectedKeys := make([]dbmodel.ChannelKey, 0, 1)
			for _, candidateKey := range availableKeys {
				if candidateKey.ID == req.interventionKeyID {
					selectedKeys = append(selectedKeys, candidateKey)
					break
				}
			}
			availableKeys = selectedKeys
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
			if req.interventionKeyID == 0 && !channel.DisableCircuitBreaker && iter.Index() == iter.Len()-1 {
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

		// The breaker check below keys on the candidate's model name while attempt()
		// records failures under the mapped upstream name. Remap the candidate so the
		// check and the record share one key; otherwise the breaker never trips for a
		// channel whose upstream model differs from the client alias.
		if mapped, ok := channel.ModelMapping[internalRequest.Model]; ok && mapped != "" {
			iter.RemapCurrentModel(mapped)
		}

		// 多 Key 竞速模式分支：仅限普通文本 relay 且未被 intervention 指定单 Key、可用 Key >= 2
		if canChannelRace(req, channel, availableKeys) {
			// Single-threaded pre-race setup:
			// 1. Update inbound adapter's ThinkingToContent once before goroutines start.
			if setter, ok := req.inAdapter.(interface{ SetThinkingToContent(bool) }); ok {
				setter.SetThinkingToContent(channel.ThinkingToContent)
			}
			// 2. Prepare immutable template internalRequest on main thread.
			internalRequest.Model = item.ModelName
			internalRequest.Messages = append([]model.Message(nil), baseMessages...)
			internalRequest.PreviousResponseID = basePreviousResponseID
			internalRequest.ResponsesInputRaw = cloneRawJSONMessage(baseResponsesInputRaw)
			internalRequest.ResponsesInstructions = baseResponsesInstructions
			promptSnapshot := applyPromptOverrides(internalRequest, routeResult.AccessPlan, routeResult.AccessRouteRule, channel)
			if len(promptSnapshot.Sources) > 0 {
				clearResponsesRawPromptShape(internalRequest)
			}
			metrics.SetPromptOverrideSnapshot(promptSnapshot)

			capabilityKey := routingCapabilityKey(internalRequest, channel)
			log.Infof("request model %s, channel %s entering race mode with %d available keys (delay=%dms, sticky=%t)",
				requestModel, channel.Name, len(availableKeys), channel.RaceDelayMs, iter.IsSticky())

			// #2: pin request-level protection on the PARENT before any racer starts.
			// Each racer's `isolatedReq := *req` VALUE-COPIES redactRequired, so a racer's
			// own applyInboundRedaction never reaches the parent. Pinning here keeps the
			// parent sticky even when ALL racers fail, so the next (possibly opted-out)
			// channel's attempt still redacts instead of sending cleartext. The winner
			// handoff needs no extra plumbing: it reads the parent request's field.
			if req.redactRequired || (redactGlobalEnabled() && channel.RedactEnabled) {
				req.redactRequired = true
			}

			raceStartedAt := time.Now()
			requestState.startRound(channel.Name, item.ModelName)
			endAttempt := beginControlledAttempt()
			result, remainingKeys := runChannelRace(
				req,
				channel,
				availableKeys,
				outAdapter,
				requestEndpoint,
				capabilityKey,
				group.FirstTokenTimeOut,
			)
			endAttempt()
			recoveryStartedAt = markRecoveryStart(recoveryStartedAt, result.Err)
			if result.Err == nil {
				req.releaseRescueDeadline()
			}
			if requestState.consumeRescueRequest() && !result.Success && !result.Written {
				operatorRescueRequested = true
				lastErr = errRunningRequestRescue
				requestState.finishRound(lastErr.Error(), time.Since(raceStartedAt).Milliseconds())
				break attemptChannels
			}
			raceLatency := time.Since(raceStartedAt).Milliseconds()
			if result.Success {
				requestState.finishRound("", raceLatency)
			} else if result.Err != nil {
				requestState.finishRound(result.Err.Error(), raceLatency)
			} else {
				requestState.finishRound("unknown error", raceLatency)
			}

			if result.Success {
				requestSucceeded = true
				metrics.Save(c.Request.Context(), true, nil, append(allAttempts, iter.Attempts()...))
				return
			}
			if result.Written {
				req.wroteBusinessData = true
				metrics.Save(c.Request.Context(), false, result.Err, append(allAttempts, iter.Attempts()...))
				return
			}
			lastErr = result.Err
			retryAfterFloor = max(retryAfterFloor, result.RetryAfter)
			if result.Fatal {
				fatalClientErr = result.Err
				break
			}
			if len(remainingKeys) > 0 && shouldTryNextChannelKey(result.StatusCode) {
				availableKeys = remainingKeys
			} else {
				continue
			}
		}

		for keyIndex, usedKey := range availableKeys {
			if clientGone, stopErr := relayStop(originalRequest.Context(), c.Request.Context(), interventionCtx, rescueFired); clientGone {
				log.Infof("client request context canceled, stopping retry")
				metrics.Save(originalRequest.Context(), false, originalRequest.Context().Err(), append(allAttempts, iter.Attempts()...))
				return
			} else if stopErr != nil {
				lastErr = stopErr
				break attemptChannels
			}
			if rescueDeadlineExpired(recoveryStartedAt, time.Now()) {
				rescueExpired = true
				break attemptChannels
			}
			// Reset the route model before each key attempt: applyModelMapping mutates
			// internalRequest.Model to the mapped upstream name during the prior attempt,
			// so without this a second key would see the already-mapped name, miss the
			// mapping, leave modelMapped=false, and skip the client-name restore — leaking
			// the upstream model name to the client.
			internalRequest.Model = item.ModelName
			internalRequest.Messages = append([]model.Message(nil), baseMessages...)
			internalRequest.PreviousResponseID = basePreviousResponseID
			internalRequest.ResponsesInputRaw = cloneRawJSONMessage(baseResponsesInputRaw)
			internalRequest.ResponsesInstructions = baseResponsesInstructions
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

			// 记录本轮开始尝试（推送实时状态）
			attemptStartTime := time.Now()
			requestState.startRound(channel.Name, item.ModelName)

			// 构造尝试级上下文 -- 只写变化的 4 个字段
			ra := &relayAttempt{
				relayRequest:         req,
				outAdapter:           outAdapter,
				channel:              channel,
				usedKey:              usedKey,
				firstTokenTimeOutSec: group.FirstTokenTimeOut,
			}

			endAttempt := beginControlledAttempt()
			result := ra.attempt()
			endAttempt()
			recoveryStartedAt = markRecoveryStart(recoveryStartedAt, result.Err)
			if result.Err == nil {
				req.releaseRescueDeadline()
			}
			if requestState.consumeRescueRequest() && !result.Success && !result.Written {
				operatorRescueRequested = true
				lastErr = errRunningRequestRescue
				requestState.finishRound(lastErr.Error(), time.Since(attemptStartTime).Milliseconds())
				break attemptChannels
			}
			if stopErr := rescueStopError(interventionCtx, originalRequest.Context(), rescueFired); stopErr != nil {
				result.Err = stopErr
				result.Retryable = false
			}
			attemptLatency := time.Since(attemptStartTime).Milliseconds()

			// 记录本轮结束（推送状态更新）
			if result.Success {
				requestState.finishRound("", attemptLatency)
			} else if result.Err != nil {
				requestState.finishRound(result.Err.Error(), attemptLatency)
			} else {
				requestState.finishRound("unknown error", attemptLatency)
			}
			// 坏响应（200 但一个字节都没有 / 流自然结束却没有任何内容）不再"原地立刻重发"：
			// 旧行为在这条分支里连发 2 次、间隔 1~2ms，现场锚点因此出现"同一请求体同一渠道
			// 连发 5 次、间隔无退避"。现在一律交给下面那条已经带退避、带同渠上限、并受整次
			// 请求总时长上限约束的自动救援链：节奏由其退避决定，渠道由轮换决定。
			if result.Success {
				requestSucceeded = true
				// 成功的这一次尝试所用的 Key 即最终 Key, 回填其备注供日志展示。
				metrics.ChannelKeyRemark = usedKey.Remark
				metrics.Save(c.Request.Context(), true, nil, append(allAttempts, iter.Attempts()...))
				return
			}
			if result.Written {
				req.wroteBusinessData = true
				// 已向下游提交(不可再故障转移), 记录已提交这次尝试所用 Key 的备注。
				metrics.ChannelKeyRemark = usedKey.Remark
				metrics.Save(c.Request.Context(), false, result.Err, append(allAttempts, iter.Attempts()...))
				return
			}
			lastErr = result.Err
			retryAfterFloor = max(retryAfterFloor, result.RetryAfter)
			if result.Fatal {
				// Deterministic client rejection (context-window overflow / malformed
				// request body): no other channel or key will accept the same payload —
				// stop iterating, do not start the rescue loop, and return the upstream's
				// own 4xx (real status + code + reason) as-is.
				fatalClientErr = result.Err
				break
			}
			if !result.Retryable && !shouldTryNextChannelKey(result.StatusCode) {
				break
			}
		}
		if fatalClientErr != nil {
			break
		}
	}

	// 所有通道都失败
	allAttempts = append(allAttempts, iter.Attempts()...)
	if !rescueExpired && !operatorRescueRequested && fatalClientErr == nil && shouldReturnToOriginalGroup(routeResult, triedReturnGroup) {
		triedReturnGroup = true
		fallbackGroup, err := op.GroupGetEnabledMap(requestModel, c.Request.Context())
		if err != nil {
			lastErr = err
		} else {
			fallbackGroup = enrichGroupForSmartRouting(c.Request.Context(), fallbackGroup, preferStreamRouting)
			// 回退轮不得重打本次请求已经试过的渠道: 池里只剩同一家时, 旧行为会在失败后 1ms
			// 又发一次(同一渠道两份上游执行/两份计费), 而救援循环每轮都把 triedReturnGroup
			// 重置, 于是每一轮都重复这一发——现场实测 8 次上游请求其实是 4 轮 × 2 发。
			// 回退的意义是触达"路由没试过的渠道", 已经试过的先剔掉。
			fallbackGroup, anyFallbackLeft := excludeAttemptedChannels(fallbackGroup, allAttempts)
			if !anyFallbackLeft {
				log.Infof("access-route fallback pass has no untried channel left for model %s, skipping it", requestModel)
			} else {
				fallbackSticky := routeStickyEnabled(fallbackGroup.Mode, clientSession.Source)
				fallbackIter := balancer.NewIteratorWithSession(fallbackGroup, apiKeyID, requestModel, clientSessionKey, fallbackSticky)
				fallbackIter.PrioritizeChannels(nativeProtocolChannelIDs(c.Request.Context(), inboundType, fallbackGroup.Items))
				prioritizeResponsesSessionOwner(c.Request.Context(), fallbackIter, internalRequest, apiKeyID, userID)
				if fallbackIter.Len() > 0 {
					// Spilling to the model pool re-attempts targets that have just failed, so
					// an upstream Retry-After applies here exactly as it does inside the rescue
					// loop: never re-hit a throttled upstream before it said to come back.
					if !paceRetryWait(c.Request.Context()) {
						if rescueDeadlineExpired(recoveryStartedAt, time.Now()) {
							rescueExpired = true
						}
					} else {
						group = fallbackGroup
						iter = fallbackIter
						req.stickyEnabled = fallbackSticky
						internalRequest.Model = requestModel
						goto runIterator
					}
				}
			}
		}
	}
	finalErr := lastErr
	if finalErr == nil {
		finalErr = routeSelectionErrorFromAttempts(allAttempts)
	}
	// 自动救援是所有「可恢复失败」的默认行为（硬规定：429/5xx/超时/半截流 全部要救，守
	// Retry-After、退避重试、换渠道，直到成功或预算用完）：只要还没向客户端提交任何业务内容，
	// 就一直重打。默认配置下它就是开着的（relay_intervention_enabled 默认 true），管理员仍
	// 可用该开关让普通渠道快速失败——这正是该设置原本的语义，不另造第二个开关；无熔断渠道
	// 例外，它们按预算自救（relay_no_breaker_retry_budget_seconds 就是这笔预算，
	// rescueWindowDeadline 仍把它钳在 autoRescueCap 以内）。它也刻意不挂调用端是否要流式——
	// 非流式调用端一样享受救援，只是不发 SSE 心跳（心跳会把 JSON 响应体弄脏）；这正是矩阵里
	// 6×429 被秒回 502 的根因：非流式请求根本进不了救援循环。
	rescueBudget := intervention.NoBreakerRetryBudget()
	rescuable := isRescueableHeldRequest(req, fatalClientErr, finalErr)
	// 假成功防线: 只要这次失败还救得回来, 就别再发"预内容心跳"。心跳会把响应提交成 HTTP 200,
	// 提交之后再怎么救, 收尾只能写带内错误帧 —— 调用方看到的就是 "200 + 空内容", 比报错更糟。
	// 不提交, 耗尽时才能如实回上游的真实状态码 (429/5xx 透传)。
	if rescuable && !req.wroteBusinessData {
		req.suppressPreContentKeepalive.Store(true)
	}
	autoRescue := rescuable && (intervention.Enabled() || sawNoBreakerChannel) && rescueBudget > 0
	manualIntervention := shouldHoldForOperator(req, fatalClientErr, finalErr) ||
		(operatorRescueRequested && isRescueableHeldRequest(req, fatalClientErr, finalErr))
	// Re-entry previously continued rescue blindly even when the latest attempt produced a
	// deterministic (non-rescuable / context-window / malformed-body) error or the rescue
	// budget already died — that looped forever or held a request whose error could never be
	// rescued. Continue ONLY while the current final error is still eligible AND the rescue
	// budget/context is still alive.
	rescueBudgetAlive := interventionCtx == nil || interventionCtx.Err() == nil
	clientAlive := originalRequest.Context().Err() == nil
	if clientAlive && rescueBudgetAlive && (autoRescue || manualIntervention) {
		// Heartbeats keep a STREAM client's connection "working" while the rescue retries;
		// a non-stream client's body must stay pure JSON, so it gets none (it simply waits,
		// bounded by its own client timeout and by the rescue window).
		if stopInterventionKeepalive == nil && internalRequestPrefersStream(req.internalRequest) {
			// 无熔断救援固定 1s 轮次，且心跳协程在每轮尝试开始前就被停掉（防并发写
			// gin.Writer）：2s 默认延迟在轮内永远死胎，救援期间客户端全程静默——正是
			// 该心跳特性要防的事。钳到半轮，保证每轮退避期间必发一拍。
			holdDelay := currentInterventionKeepaliveDelay()
			if sawNoBreakerChannel {
				if halfRound := time.Second / 2; holdDelay > halfRound {
					holdDelay = halfRound
				}
			}
			stopInterventionKeepalive = startDownstreamKeepaliveWithDelayUnless(c.Request.Context(), c, holdDelay, func() bool { return req.suppressPreContentKeepalive.Load() && !req.wroteBusinessData })
		}
		if interventionCtx == nil {
			// ONE automatic-recovery window, measured from the first rescuable failure and
			// capped at autoRescueCap. The operator hold timeout can only tighten it — it must
			// never lend extra life to an automatic retry. A deadline already in the past opens
			// an immediately-expired window so the loop terminates cleanly with
			// octopus_rescue_timeout.
			start := recoveryStartedAt
			if start.IsZero() {
				start = time.Now()
				recoveryStartedAt = start
			}
			interventionCtx, interventionCancel = context.WithCancel(c.Request.Context())
			defer interventionCancel()
			// Route every subsequent upstream attempt through the rescue context so an
			// in-flight attempt honors the rescue deadline / operator abort instead of
			// ignoring it and letting the Pending Resolve wait until the attempt returns.
			// forward()/race paths all read c.Request.Context(), so a single swap here
			// propagates to all retry attempts without touching the wire (header/body shape
			// is unchanged — only the request's context object changes). The raw client
			// request is retained and restored on exit so the defer's state-marking check
			// still sees the true client context.
			c.Request = c.Request.WithContext(interventionCtx)
		}
		// R12: the window is re-derived on EVERY hold entry, not only when the rescue
		// context is first created — a machine rescue retry or an operator channel switch
		// re-enters this block with interventionCtx already set, and a clamp that tightened
		// in the meantime (no-breaker budget / operator hold, both live-read) must apply.
		// armRescueDeadline is tighten-only: a recomputed deadline that is equal or later
		// never extends the current timer, and the R18 release-vs-fire mutex holds for
		// both the first arm and the re-arm.
		rescueBudget = intervention.NoBreakerRetryBudget()
		// 整次请求的绝对时长上限也参与收紧救援等待窗口: 救援环节不能比总钟活得更久, 否则
		// "到点收尾"会被 300s 的等待窗口拖过头(第三道钟见 request_total_timeout.go)。
		rescueRemaining, rescueCeilingActive := req.totalBudgetRemainingClamped()
		req.armRescueDeadline(
			rescueWindowDeadline(recoveryStartedAt, rescueBudget, autoRescue, intervention.Timeout(), rescueCeilingActive, rescueRemaining),
			rescueFired,
			interventionCancel,
		)

		lastErrStr := ""
		if finalErr != nil {
			lastErrStr = finalErr.Error()
		}

		// Registering for an operator decision is the opt-in half of this block: the switch
		// only controls whether a human can see/steer the held request. The automatic retry
		// loop below runs regardless of it.
		if intervention.Enabled() && !interventionRegistered {
			pid, regErr := intervention.Register(&intervention.Pending{
				RequestModel: requestModel,
				Endpoint:     requestEndpoint,
				Attempts:     allAttempts,
				LastError:    lastErrStr,
				Status:       intervention.StatusAutoRetrying,
				RescueRound:  interventionRounds,
				// An operator abort cancels the in-flight upstream attempt right away
				// (Resolve invokes this outside registry locks) instead of waiting for it
				// to return. It is idempotent: context.CancelFunc is safe to call more than
				// once, and this fires after the resolved decision is already delivered.
				CancelInternal: interventionCancel,
			})
			if regErr != nil {
				log.Warnf("Intervention registration failed endpoint=%s model=%s: %v", requestEndpoint, requestModel, regErr)
			} else {
				pendingInterventionID = pid
				interventionRegistered = true
				requestState.BindInterventionID(pid)
				log.Infof("Intervention held request id=%s endpoint=%s model=%s", pendingInterventionID, requestEndpoint, requestModel)
			}
		} else if interventionRegistered {
			_ = intervention.UpdateAttempts(pendingInterventionID, allAttempts, lastErrStr)
		}

		{
			// 救援轮次的节奏与轮换状态（见 rescue_retry_pacing.go）：同一渠道连续失败到上限
			// 就把它从候选里剔除，强制换渠道；退避必须真的增长，且整段救援受请求总时长上限约束。
			rotation := newRescueRotation()
			for {
				interventionRounds++
				if exhaustedNow, id, name, rounds := rotation.noteRound(allAttempts); exhaustedNow {
					log.Warnf("channel %s (id=%d) failed %d consecutive automatic-rescue rounds, dropping it and switching channel",
						name, id, rounds)
				}
				// 无熔断渠道仍保留"像直连 CLI 一样先快试"的意图(前两轮 1s)，但之后必须按
				// 指数退避增长：现场实测那版固定 1s 把 300s 救援窗口烧成同一渠道 138 次原地重试。
				backoff := rescueRoundBackoff(interventionRounds, sawNoBreakerChannel)
				// 上游明确要求"多久之后再来"时，这个节奏归它：比它更早重试只会再吃一次
				// 429/503，把一次短暂限流拖成自己打自己的循环。这里只把等待拉长到上游
				// 要求的下限；救援窗口仍由 armRescueDeadline 收口——窗口比 Retry-After
				// 短就照旧按窗口结束，不会凭空续命。floor 与本轮 backoff 取大即清零，
				// 下一轮重新只认新一轮上游给的节奏。
				if retryAfterFloor > backoff {
					backoff = retryAfterFloor
				}
				retryAfterFloor = 0
				// 整次请求的绝对时长上限同样约束救援节奏(与第三道钟共用同一预算，不另算一套)：
				// 预算用尽就退出循环，剩下的等待绝不越过上限；剩余额度不足一轮退避时按剩余额度等。
				if remaining, active := req.totalBudgetRemaining(); active {
					if remaining <= 0 {
						log.Warnf("relay request total timeout (%s) reached during automatic rescue, stopping the retry loop", req.totalBudget)
						break
					}
					if backoff > remaining {
						backoff = remaining
					}
				}
				nextRetry := time.Now().Add(backoff)
				if interventionRegistered {
					_ = intervention.UpdateStatus(pendingInterventionID, intervention.StatusAutoRetrying, interventionRounds, &nextRetry)
				}

				resolution, isPreempted, waitErr := waitRescueRound(interventionCtx, pendingInterventionID, interventionRegistered, backoff)
				if waitErr != nil {
					log.Infof("Intervention finished id=%s err=%v", pendingInterventionID, waitErr)
					break
				}

				if isPreempted {
					if resolution.Action == intervention.ActionAbort {
						// ActionAbort has already canceled the in-flight upstream attempt via
						// Pending.CancelInternal (delivered by Resolve right after the decision);
						// surface the terminal failure below. Nothing more to wait for.
						break
					}
					req.interventionKeyID = resolution.KeyID
					interventionGroup := singleChannelGroup(resolution, requestModel)
					interventionGroup = enrichGroupForSmartRouting(c.Request.Context(), interventionGroup, preferStreamRouting)
					interventionIter := balancer.NewIteratorWithSession(interventionGroup, apiKeyID, requestModel, "", false)
					interventionIter.PrioritizeChannels(nativeProtocolChannelIDs(c.Request.Context(), inboundType, interventionGroup.Items))
					if interventionIter.Len() > 0 {
						// 停止旧心跳协程，避免并发写 gin.Writer
						if stopInterventionKeepalive != nil {
							stopInterventionKeepalive()
							stopInterventionKeepalive = nil
						}
						group = interventionGroup
						iter = interventionIter
						req.stickyEnabled = false
						triedReturnGroup = false
						internalRequest.Model = requestModel
						internalRequest.Messages = append([]model.Message(nil), baseMessages...)
						internalRequest.PreviousResponseID = basePreviousResponseID
						internalRequest.ResponsesInputRaw = cloneRawJSONMessage(baseResponsesInputRaw)
						internalRequest.ResponsesInstructions = baseResponsesInstructions
						goto runIterator
					}
					// If the chosen channel had 0 items, remain in intervention loop
					continue
				}

				// Machine-first automatic rescue retry: select fresh route group
				freshRouteResult, freshStatus, _, freshErr := selectRouteGroup(c, apiKeyID, requestModel, anthropicAliases...)
				if freshErr != nil || freshStatus != 0 {
					continue
				}
				freshGroup := enrichGroupForSmartRouting(c.Request.Context(), freshRouteResult.Group, preferStreamRouting)
				// 已经连续失败到上限的渠道不再进入候选，强制换渠道而不是原地重试。
				var anyCandidateLeft bool
				freshGroup, anyCandidateLeft = rotation.exclude(freshGroup)
				if !anyCandidateLeft {
					log.Warnf("every candidate channel failed %d consecutive automatic-rescue rounds, ending the rescue loop",
						maxRescueRoundsPerSameChannel)
					break
				}
				freshSticky := routeStickyEnabled(freshGroup.Mode, clientSession.Source)
				freshIter := balancer.NewIteratorWithSession(freshGroup, apiKeyID, requestModel, clientSessionKey, freshSticky)
				freshIter.PrioritizeChannels(nativeProtocolChannelIDs(c.Request.Context(), inboundType, freshGroup.Items))
				prioritizeResponsesSessionOwner(c.Request.Context(), freshIter, internalRequest, apiKeyID, userID)
				if freshIter.Len() > 0 {
					// 停止旧心跳协程，避免并发写 gin.Writer
					if stopInterventionKeepalive != nil {
						stopInterventionKeepalive()
						stopInterventionKeepalive = nil
					}
					group = freshGroup
					iter = freshIter
					routeResult = freshRouteResult
					req.stickyEnabled = freshSticky
					req.interventionKeyID = 0
					triedReturnGroup = false
					internalRequest.Model = requestModel
					internalRequest.Messages = append([]model.Message(nil), baseMessages...)
					internalRequest.PreviousResponseID = basePreviousResponseID
					internalRequest.ResponsesInputRaw = cloneRawJSONMessage(baseResponsesInputRaw)
					internalRequest.ResponsesInstructions = baseResponsesInstructions
					_ = intervention.UpdateAttempts(pendingInterventionID, allAttempts, lastErrStr)
					goto runIterator
				}
			}
		}
	}

	finalErr = lastErr
	// R18 belt-and-suspenders: a rescue timeout may only override the final error when
	// no business data reached the client. Every current Written exit returns before this
	// line, so the override is unreachable for committed bodies today; the gate keeps any
	// future path from ever relabeling a delivered response as rescue_timeout.
	if stopErr := rescueStopError(interventionCtx, originalRequest.Context(), rescueFired); stopErr != nil && !req.wroteBusinessData && !carriesUpstreamStatus(lastErr) {
		// 救援超时标签只许覆盖"没有真实上游错误可报"的情况。每一次尝试都回了 429/5xx 时, 调用方
		// 必须看到那个真实状态码 —— 用 504 octopus_rescue_timeout 盖掉它, 等于把"上游在限流"
		// 这件事藏成一个笼统的网关超时。
		finalErr = stopErr
	}
	if finalErr == nil {
		finalErr = routeSelectionErrorFromAttempts(allAttempts)
	}
	if stopInterventionKeepalive != nil {
		stopInterventionKeepalive()
		stopInterventionKeepalive = nil
	}
	metrics.Save(originalRequest.Context(), false, finalErr, allAttempts)
	status, code, message := relayErrorResponse(finalErr)
	if c.Writer.Written() {
		// Deferred-commit kept the stream warm with heartbeats while we failed over
		// across every channel, so HTTP 200 is already committed and no content ever
		// reached the client. resp.ErrorWithCode cannot set a status on an
		// already-committed response, so deliver the failure in-band.
		if req.wroteNonStreamJSONKeepalive {
			// application/json head was committed by blank-line keepalives: append a JSON
			// error body (valid after the leading insignificant whitespace) rather than
			// splicing an SSE event into a JSON stream.
			writeNonStreamJSONError(c, code, message)
			return
		}
		switch inboundType {
		case inbound.InboundTypeOpenAIResponse:
			writeResponsesFailedSSE(c, requestModel, code, message)
		case inbound.InboundTypeAnthropic:
			writeAnthropicErrorSSE(c, "api_error", message)
		default:
			writeChatErrorSSE(c, code, message)
		}
		return
	}
	writeUpstreamRetryAfterHint(c, status, finalErr)
	writeRelayErrorPreStream(c, inboundType, status, "api_error", code, message)
}

// writeNonStreamJSONError appends a JSON error body to a non-stream (application/json)
// response whose head was already committed by blank-line keepalives. Written after the
// leading insignificant whitespace, the whole body stays valid JSON, so the client parses
// a normal error object instead of choking on a spliced SSE event.
func writeNonStreamJSONError(c *gin.Context, code, message string) {
	payload, err := json.Marshal(gin.H{"error": gin.H{"message": message, "type": "upstream_error", "code": code}})
	if err != nil {
		payload = []byte(`{"error":{"message":"upstream error","type":"upstream_error"}}`)
	}
	_, _ = c.Writer.Write(payload)
	c.Writer.Flush()
}

// recordAttemptProxy annotates a forwarding attempt with the egress route it used
// (direct, or a specific proxy incl. its scheme + host). The proxy metadata is
// app-layer channel config, not an outbound byte, so this never touches the
// upstream request's TLS/header shape. Only real forward attempts carry a route.
func recordAttemptProxy(span *balancer.AttemptSpan, channel *dbmodel.Channel) {
	if span == nil || channel == nil {
		return
	}
	if !channel.Proxy {
		span.SetProxy(false, "", "", "")
		return
	}
	info, err := helper.ChannelProxyInfoFor(channel)
	if err != nil {
		// Proxy configured but its URL is unusable — still record that a proxy was
		// intended so the log never mislabels this attempt as a direct route.
		span.SetProxy(true, info.Source, info.Scheme, info.Host)
		return
	}
	span.SetProxy(info.Used, info.Source, info.Scheme, info.Host)
}

func (ra *relayAttempt) resolveRuntimeModel() string {
	if ra == nil {
		return ""
	}
	modelName := ""
	if ra.internalRequest != nil {
		modelName = ra.internalRequest.Model
	}
	if ra.channel != nil && len(ra.channel.ModelMapping) > 0 && modelName != "" {
		if mapped, ok := ra.channel.ModelMapping[modelName]; ok && mapped != "" {
			return mapped
		}
	}
	return modelName
}

// attempt 统一管理一次通道尝试的完整生命周期
func (ra *relayAttempt) attempt() attemptResult {
	runtimeModel := ra.resolveRuntimeModel()
	span := ra.iter.StartAttempt(ra.channel.ID, ra.usedKey.ID, ra.channel.Name)
	span.SetRouteScope(ra.metrics.RequestEndpoint, ra.routingCapabilityKey())
	recordAttemptProxy(span, ra.channel)
	finishRuntimeAttempt := balancer.BeginRuntimeAttempt(ra.channel.ID, ra.usedKey.ID, runtimeModel)
	defer finishRuntimeAttempt()
	// 凭据脱敏: 每次 attempt 自己的 session (占位符映射随 attempt 丢弃), attempt 返回即释放 VM。
	defer ra.redactClose()

	// 转发请求
	statusCode, fwdErr := ra.forward(span)

	usedAt := time.Now().Unix()

	if fwdErr == nil {
		// ====== 成功 ======
		ra.collectResponse()
		costDelta := ra.metrics.Stats.InputCost + ra.metrics.Stats.OutputCost
		op.ChannelKeyRecordUse(ra.usedKey, statusCode, usedAt, costDelta)

		span.End(dbmodel.AttemptSuccess, statusCode, "")
		balancer.RecordRuntimeSuccess(ra.channel.ID, ra.usedKey.ID, runtimeModel, balancer.AttemptRuntimeMetrics{
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
		balancer.RecordSuccessScoped(ra.channel.ID, ra.usedKey.ID, runtimeModel, ra.metrics.RequestEndpoint, ra.routingCapabilityKey())
		// 会话保持：更新粘性记录（仅当该请求参与 sticky；轮询模式纯优化型会话不写，保证轮转）
		if ra.stickyEnabled {
			balancer.SetStickyWithSessionKey(ra.apiKeyID, ra.requestModel, ra.clientSessionKey, ra.channel.ID, ra.usedKey.ID)
		}

		ra.metrics.ParamOverride = paramOverrideValue(ra.channel.ParamOverride)

		return attemptResult{Success: true, StatusCode: statusCode}
	}

	// ====== 失败 ======
	// 第三道钟: 这次失败若是"整次请求预算到点"造成的取消, 换成具名错误, 让日志/指标/调用端
	// 错误都指向真实原因; 调用端自己的取消照旧保留原有归因。
	fwdErr = ra.classifyTotalTimeout(fwdErr)
	recordStatusCode := attemptStatusCode(statusCode, fwdErr)
	op.ChannelKeyRecordUse(ra.usedKey, recordStatusCode, usedAt, 0)
	span.End(dbmodel.AttemptFailed, recordStatusCode, attemptAuditMessage(ra.upstreamResponded, recordStatusCode, fwdErr))

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
		balancer.RecordRuntimeFailure(ra.channel.ID, ra.usedKey.ID, runtimeModel, recordStatusCode, span.Duration(), retryAfter)
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
		balancer.RecordFailureWithStatusScoped(ra.channel.ID, ra.usedKey.ID, runtimeModel, ra.metrics.RequestEndpoint, ra.routingCapabilityKey(), recordStatusCode)
		if ra.iter != nil && ra.iter.IsStickyChannel(ra.channel.ID) {
			balancer.ClearStickyWithSessionKey(ra.apiKeyID, ra.requestModel, ra.clientSessionKey)
			log.Infof("cleared sticky route for api key %d model %s after channel %s failure", ra.apiKeyID, ra.requestModel, ra.channel.Name)
		}
	} else {
		log.Infof("skip circuit breaker failure count for channel %s model %s: %v", ra.channel.Name, runtimeModel, fwdErr)
	}

	ra.metrics.ParamOverride = paramOverrideValue(ra.channel.ParamOverride)

	// committed reflects whether real content already reached the client. It gates
	// whole-stream failover, NOT ra.c.Writer.Written(): the deferred-commit path may
	// have flushed only ignorable SSE comment heartbeats (Written()==true) while the
	// message envelope was still buffered — that stream can still fail over cleanly.
	// Only once meaningful content is flushed do we lose the ability to switch
	// channels and must instead surface the failure as an in-band error event.
	committed := ra.wroteMeaningfulDownstream
	if committed {
		writeCommittedStreamFailure(ra, fwdErr)
		ra.collectResponse()
	}
	return attemptResult{
		Success:    false,
		Written:    committed,
		Err:        fmt.Errorf("channel %s failed: %w", ra.channel.Name, fwdErr),
		StatusCode: recordStatusCode,
		Retryable:  !committed && isRetryableUpstreamStreamError(fwdErr),
		Fatal:      isDeterministicClientRejection(fwdErr),
		RetryAfter: upstreamRetryAfter(fwdErr),
	}
}

// maxClientRequestBodyBytes caps the client request body on the chat/messages/responses
// path. A single authenticated tenant could otherwise io.ReadAll a multi-GB upload and
// balloon gateway memory (the image/video generation paths already spill to disk via
// bodycache). 512 MiB is far beyond any real text + inline-media request on these endpoints
// — even a heavy multimodal vision request with many base64 images — so it only trips on
// pathological payloads.
// Aligned with new-api MAX_REQUEST_BODY_MB default (128): reject oversized client
// bodies early instead of accepting up to 512 MiB and letting a strict upstream
// (vercel AI Gateway) 413 after the proxy already paid the parse/memory cost.
const maxClientRequestBodyBytes = 128 << 20

// parseRequest 解析并验证入站请求
func parseRequest(inboundType inbound.InboundType, c *gin.Context) (*model.InternalLLMRequest, model.Inbound, error) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxClientRequestBodyBytes)
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			resp.Error(c, http.StatusRequestEntityTooLarge, "request body too large")
			return nil, nil, err
		}
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return nil, nil, err
	}

	inAdapter := inbound.Get(inboundType)
	internalRequest, err := inAdapter.TransformRequest(c.Request.Context(), body)
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return nil, nil, err
	}

	// A genuine claude CLI carries its beta set in the Anthropic-Beta HEADER, not
	// the body-level `betas` field the inbound transformer also accepts — so for
	// anthropic inbound, merge the header values into TransformOptions.AnthropicBetas.
	// Downstream consumers (the mid-conversation-system message placement gate, the
	// outbound beta merge) then see the client's real betas; every emitter dedupes,
	// so nothing double-appends.
	if inboundType == inbound.InboundTypeAnthropic {
		for _, raw := range strings.Split(c.Request.Header.Get("Anthropic-Beta"), ",") {
			if beta := strings.TrimSpace(raw); beta != "" {
				internalRequest.TransformOptions.AnthropicBetas = append(
					internalRequest.TransformOptions.AnthropicBetas, beta)
			}
		}
	}

	// Pass through the original query parameters, but strip the client's auth params
	// (?key=/?api_key=/... = the client's Octopus key) so they never leak to the
	// upstream provider; provider-specific switches (beta/alt/...) are preserved.
	clientQuery := c.Request.URL.Query()
	for qk := range clientQuery {
		switch strings.ToLower(strings.TrimSpace(qk)) {
		case "key", "api_key", "apikey", "access_token":
			clientQuery.Del(qk)
		}
	}
	internalRequest.Query = clientQuery
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
		writeRelayErrorPreStream(c, inboundType, http.StatusBadRequest, "invalid_request_error", clientValidationErrorCode(err), clientValidationErrorMessage(err))
		return nil, nil, err
	} else if err != nil {
		if maybeHandleCursorEmptyAnthropicProbe(c, inboundType, internalRequest, body, err) {
			return nil, nil, err
		}
		if maybeHandleCursorEmptyOpenAIProbe(c, inboundType, internalRequest, body, err) {
			return nil, nil, err
		}
		saveClientValidationRelayLog(c.Request.Context(), c, inboundType, internalRequest, body, err)
		writeRelayErrorPreStream(c, inboundType, http.StatusBadRequest, "invalid_request_error", clientValidationErrorCode(err), clientValidationErrorMessage(err))
		return nil, nil, err
	}

	return internalRequest, inAdapter, nil
}

// forward 转发请求到上游服务
func (ra *relayAttempt) forward(span *balancer.AttemptSpan) (int, error) {
	ctx := ra.c.Request.Context()
	// 第三道钟的统一兜底(见 request_total_timeout.go): 把这次尝试的上游工作绑在整次请求的绝对
	// 死线上。流式路径有自己的计时器(好在到点时分辨"已交付/未交付"), 这里保证没有计时器的路径
	// (例如纯粹的流式响应体读取) 同样不会被拖到客户端自己放弃。
	ctx = ra.boundUpstreamLifetime(ctx)
	ra.responsesDowngradedToChat = false
	originalInternalRequest := cloneInternalRequestForRetry(ra.internalRequest)
	// A responses request bound for a chat channel that carries an unresolvable
	// previous_response_id is refused here (invalid_request_error) rather than
	// forwarded with its context silently stripped; see bridgeResponsesHistoryForChat.
	if err := ra.applyTransformOptions(); err != nil {
		return http.StatusBadRequest, err
	}

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
	// Capture the current top-level instructions verbatim (a *string): nil stays nil and
	// suppressCodexHoistedContext's non-nil "" sentinel stays non-nil. Rolled back after
	// TransformRequest like the other originals, so a goto re-entry and the next attempt
	// both rebuild from the ORIGINAL instructions rather than a prior pass's placeholders.
	originalResponsesInstructions := ra.internalRequest.ResponsesInstructions
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
	// 凭据脱敏（调用端侧模块）: 排序契约 — 必须在【所有历史并入点】（applyTransformOptions 的
	// chat/Anthropic 桥、dropResponsesSessionCursor 分支的 Codex 桥、prepareResponsesSessionCursor
	// 的 chat-sourced 游标 graft、以及上面的 encrypted-content 处理）之后、出站 TransformRequest
	// 之前调用，用当次渠道 flags 扫描客户端原样字节 (RawRequest)，把 Messages/ResponsesInstructions/
	// ResponsesInputRaw 换成占位符——这样并入的历史文字也一并被扫，且之后的 TransformRequest
	// 构建出的 body 自然携占位符。session 挂在 attempt 上, 每次尝试独立; 失败即 attempt 显式
	// 失败 (fail-closed), 绝不裸发原文。
	if err := ra.applyInboundRedaction(); err != nil {
		return 0, err
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

	// A rebuilt responses->chat history is stored un-normalized (pending tool_calls kept for
	// future turns); enforce the chat tool-call pairing invariant here, on the wire copy only.
	// originalMessages (captured above) is restored after TransformRequest, so the re-recorded
	// transcript keeps the full history.
	if ra.chatHistoryRebuilt {
		ra.internalRequest.Messages = normalizeChatToolCallPairing(ra.internalRequest.Messages)
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
	ra.internalRequest.ResponsesInstructions = originalResponsesInstructions
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

	// Apply the same body override contract used by synthetic channel tests.
	if err := ApplyParamOverride(outboundRequest, ra.channel.ParamOverride); err != nil {
		return 0, err
	}

	// 复制请求头
	ra.copyHeaders(outboundRequest)

	// 发送请求
	// keepalive 注入的是 SSE 心跳，只有【下游客户端】收流式时才可注入（否则污染非流式响应）。
	// 下游是否流式取决于客户端原始请求 originalStream，而非上游 force* 标志：
	// force*StreamUpstream 为真但客户端要非流式时走 handleStreamResponseAsNonStream（下游非流式），
	// 绝不能注入；originalStream 同时覆盖 handleStreamResponse 与 forceNonStreamUpstream
	// （非流上游→SSE 下游重放）两条真正的下游流式路径。
	willStream := originalStream != nil && *originalStream
	var stopFirstByteKeepalive func()
	if willStream {
		stopFirstByteKeepalive = ra.startFirstByteKeepalive(ctx)
	}
	// 诊断计时(仅 diagnostic_mode 消费, time.Now 成本可忽略): 记录发出上游请求 / 收到响应头
	// 的时刻。二者之差即"等响应头"(首包)耗时——现有 metrics 缺这段, 首包卡死时只有它能定位。
	if ra.metrics != nil {
		ra.metrics.RequestSentTime = time.Now()
	}
	response, err := ra.sendRequest(outboundRequest)
	if stopFirstByteKeepalive != nil {
		stopFirstByteKeepalive()
	}
	if err != nil {
		return 0, fmt.Errorf("failed to send request: %w", err)
	}
	// The upstream answered, so this attempt's request really was executed there (and
	// may already be billed). Recorded so a later failure on this attempt can say so in
	// the audit log; it does not change whether the relay fails over.
	ra.upstreamResponded = true
	if ra.metrics != nil {
		ra.metrics.ResponseHeaderTime = time.Now()
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
				// The pre-transform clone above dropped the channel's model mapping; re-apply
				// it so the downgraded chat wire carries the upstream model name, not the
				// client-visible one. (Auto prompt_cache_key is not re-applied here — a known
				// perf-only gap on this fallback path, not a correctness one.)
				ra.applyModelMapping()
				// The downgrade forwards over chat/completions, which keeps no server-side
				// response state. Run the chat history bridge on the swapped wire so a
				// previous_response_id turn is rebuilt from the local transcript (or refused
				// with invalid_request_error) instead of silently forwarded context-stripped.
				ra.responsesDowngradedToChat = true
				if bridgeErr := ra.bridgeResponsesHistoryForChat(); bridgeErr != nil {
					return http.StatusBadRequest, bridgeErr
				}
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
		if err := ra.handleStreamResponseAsNonStream(ctx, response, outAdapter, 0); err != nil {
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

// marshalJSONNoHTMLEscape serialises a request body the way the client's own JSON
// serialiser does. Go's json.Marshal rewrites <, > and & as \u003c / \u003e / \u0026, so a
// relayed body carrying markup, a shell `&&` or an HTML snippet leaves with different
// bytes than the client produced — a body-shape delta on exactly the large real-world
// requests where it matters. Matches the same guard already used on the Anthropic
// outbound path (outbound/authropic MessageOutbound.TransformRequest).
func marshalJSONNoHTMLEscape(value any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// ApplyParamOverride applies a channel's top-level JSON body overrides while
// preserving the original request when either JSON document is invalid. A null
// override removes the key, matching the production relay contract.
func ApplyParamOverride(req *http.Request, overrideJSON *string) error {
	if req == nil || req.Body == nil || overrideJSON == nil || strings.TrimSpace(*overrideJSON) == "" {
		return nil
	}

	originalBody, err := io.ReadAll(req.Body)
	if err != nil {
		return fmt.Errorf("failed to read request body for param_override: %w", err)
	}
	restoreBody := func(body []byte) {
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		}
		req.ContentLength = int64(len(body))
	}

	var bodyMap map[string]any
	if err := json.Unmarshal(originalBody, &bodyMap); err != nil {
		log.Warnf("failed to unmarshal request body: %v, skipping param_override", err)
		restoreBody(originalBody)
		return nil
	}
	var overrideMap map[string]any
	if err := json.Unmarshal([]byte(*overrideJSON), &overrideMap); err != nil {
		log.Warnf("failed to unmarshal param_override: %v, skipping", err)
		restoreBody(originalBody)
		return nil
	}

	applyParamOverrideMap(bodyMap, overrideMap)
	modifiedBody, err := marshalJSONNoHTMLEscape(bodyMap)
	if err != nil {
		log.Warnf("failed to marshal modified body: %v, skipping param_override", err)
		restoreBody(originalBody)
		return nil
	}
	restoreBody(modifiedBody)
	return nil
}

// applyParamOverrideMap 把渠道 param_override 合并进出站请求体：
//   - 非 null 值：覆盖/新增该键（原有语义，等价于 maps.Copy）。
//   - null 值：删除该键，用于剥离某些上游渠道不支持的参数。
//
// 例如某些第三方 /responses 中转不认 max_output_tokens，配
// {"max_output_tokens": null} 即可在发出前把该字段整个删掉，而不是发一个
// 显式 null（后者对拒绝该参数存在的上游仍会 400）。对齐参考项目 new-api
// param_override 的 delete 操作语义。
func applyParamOverrideMap(body, override map[string]any) {
	for k, v := range override {
		if v == nil {
			delete(body, k)
		} else {
			body[k] = v
		}
	}
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

// toolCallCompletionGracePeriod preserves the compatibility fallback for providers
// that never send response.completed, while allowing conforming providers to deliver
// the terminal usage event that follows response.output_item.done.
const toolCallCompletionGracePeriod = time.Second

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
	if err == nil || errors.Is(err, errRunningRequestRescue) {
		return false
	}
	if isClientAbortError(err) {
		return false
	}
	if isRetryableUpstreamStreamError(err) {
		// Transient empty-stream failures are retried by the automatic rescue loop (paced and
		// bounded per channel), not in place,
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
	if isRequestInvalidUpstreamError(err) {
		// Deterministic request-shape rejection (invalid_request_error /
		// INVALID_ARGUMENT / FAILED_PRECONDITION / body-deserialize failure): the
		// upstream refused THIS request's shape, not because the channel is unhealthy.
		// Charging it to the breaker benches a perfectly good channel for a
		// client/gateway-shape mismatch and shrinks the failover pool — exactly how a
		// strict channel (ele-deepseek) got benched by a malformed parallel-tool-call
		// request, starving the round-robin that should have routed around it. Failover
		// still tries a more lenient channel; the strict one stays available for
		// well-formed requests. Mirrors CLIProxyAPI's isRequestInvalidError.
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
// the relay) refuse non-stream requests on gated models (opus) outright — a
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
	// A codex-cloaked responses upstream is a genuine codex / OpenAI Responses
	// endpoint: it only accepts /v1/responses and rejects /v1/chat/completions with
	// 403 "codex clients may only use the OpenAI Responses protocol at /v1/responses".
	// Downgrading it to chat can never succeed — it only turns a transient
	// responses-side error (e.g. a 502/503 hiccup) into a hard 403 — so never fall
	// back for codex channels; let the balancer fail over to the next channel
	// instead. Plain (cloak=never) responses-compatible proxies still fall back.
	if ra.shouldUseCodexFingerprint() {
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

// applyModelMapping remaps ra.internalRequest.Model from the client-visible name
// to the upstream-expected name using the channel's model_mapping table.
// The original client name is always preserved in ra.requestModel (set before
// the first channel attempt and never modified). ra.modelMapped is set so that
// response transformers can restore the client-visible name in the reply.
func (ra *relayAttempt) applyModelMapping() {
	if ra.channel == nil || len(ra.channel.ModelMapping) == 0 {
		return
	}
	upstreamModel, ok := ra.channel.ModelMapping[ra.internalRequest.Model]
	if !ok || upstreamModel == "" {
		return
	}
	ra.internalRequest.Model = upstreamModel
	ra.modelMapped = true
}

func (ra *relayAttempt) applyTransformOptions() error {
	return ra.applyTransformOptionsWithInboundSetter(true)
}

func (ra *relayAttempt) applyTransformOptionsWithInboundSetter(updateInboundSetter bool) error {
	ra.applyModelMapping()
	ra.internalRequest.TransformOptions.AnthropicAutoCacheControl = false
	// Channel cloak mode "never" disables Claude identity simulation end to end:
	// header defaults are already gated by shouldApplyChannelCloak; this flag carries
	// the same decision into the Anthropic outbound transformer so it skips the
	// synthetic billing-header / agent-identity system blocks too.
	ra.internalRequest.TransformOptions.SuppressClaudeIdentity = !shouldApplyChannelCloak(ra.channel.Cloak)

	// Channel-level opt-in: fold empty-content reasoning into content on the chat
	// inbound wire (new-api thinking_to_content). Default false = no behaviour change.
	ra.internalRequest.TransformOptions.ThinkingToContent = ra.channel.ThinkingToContent
	// TransformRequest runs before channel selection, so the inbound adapter may
	// have captured ThinkingToContent=false. Push the live channel flag now if requested.
	if updateInboundSetter {
		if setter, ok := ra.inAdapter.(interface{ SetThinkingToContent(bool) }); ok {
			setter.SetThinkingToContent(ra.channel.ThinkingToContent)
		}
	}

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

	// Plain responses clients targeting a CHAT channel rely on previous_response_id,
	// which chat/completions does not support and no chat upstream persists. Rebuild
	// the prior turn's history from the local transcript store; if it can't be honored
	// bridgeResponsesHistoryForChat returns a deterministic invalid_request error so
	// the caller fails loudly instead of forwarding a context-stripped turn.
	if openAIChatOutboundChannel(ra.channel.Type) {
		ra.restoreCodexToolsForStatelessOutbound()
		if err := ra.bridgeResponsesHistoryForChat(); err != nil {
			return err
		}
	}

	if ra.channel.Type != outbound.OutboundTypeAnthropic {
		return nil
	}

	// Plain responses clients (e.g. Cursor) targeting a Claude channel rely on
	// previous_response_id, which Anthropic does not support. Replay the prior
	// turn's history into messages so multi-turn conversations continue.
	// Restore the codex client's real tools from the session BEFORE the history
	// bridge clears previous_response_id (the session is keyed by it); otherwise a
	// codex continuation reaches the stateless Anthropic upstream with no tools and
	// the mapped Claude model stalls (narrates instead of calling tools).
	ra.restoreCodexToolsForStatelessOutbound()
	ra.bridgeResponsesHistoryForAnthropic()

	if ra.channel.AnthropicContext1M {
		ra.internalRequest.TransformOptions.AnthropicOneMillionBeta = true
	}
	ra.prepareClaudePlainClientShape()
	// 补齐真 CLI 必发的三个顶层成员; 必须在 prepareClaudePlainClientShape 之后 —— 那条路径靠
	// "体内是否已有 CLI 成员" 判断调用方是不是普通客户端, 提前填值会把普通客户端的回退工具顶掉。
	ra.ensureClaudeCLIShapeTopLevelKeys()

	enabled, err := op.SettingGetBool(dbmodel.SettingKeyAnthropicAutoCacheControl)
	if err != nil {
		return nil
	}
	ra.internalRequest.TransformOptions.AnthropicAutoCacheControl = enabled
	return nil
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

func (ra *relayAttempt) prepareClaudePlainClientShape() {
	if ra == nil || ra.internalRequest == nil {
		return
	}
	plainClient := !isNativeAnthropicClaudeShape(ra.internalRequest)
	model.ApplyClaudeCodeFallbackTools(ra.internalRequest, ra.channel != nil && shouldApplyChannelCloak(ra.channel.Cloak), plainClient)
	if plainClient && model.AnthropicRequestWantsOneMillionBeta(ra.internalRequest) {
		applyClaudeOneMillionRuntimeShape(ra.internalRequest)
	}
}

func isNativeAnthropicClaudeShape(req *model.InternalLLMRequest) bool {
	if req == nil || req.RawAPIFormat != model.APIFormatAnthropicMessage {
		return false
	}
	// Claude Code title / structured-output probes carry output_config and may deliberately
	// send tools: []; preserve them exactly instead of adding fallback agent tools.
	if rawJSONPresentRelay(req.AnthropicOutputConfig) {
		return true
	}
	// Raw thinking/context_management (or ordinary client tools) is not proof: non-CLI
	// clients can legally send those fields too. Only Claude Code's own system markers
	// prove an agent turn that should be preserved byte-shaped instead of synthesized.
	if messagesContainClaudeCodeSystemPrompt(req.Messages) {
		return true
	}
	return false
}

func rawJSONPresentRelay(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return trimmed != "" && trimmed != "null"
}

// isBenignUpstreamStreamEnd 判断上游 SSE 流读取错误是否属于「自然断流」：上游没发
// data:[DONE] / 完整事件边界就关闭连接（go-sse 报 io.ErrUnexpectedEOF，错误串为
// "go-sse: unexpected end of input"）。这类算正常结束而非协议错误，应按流结束收尾，
// 而不是当失败刷 error 日志、触发换渠道。
func isBenignUpstreamStreamEnd(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	normalized := strings.ToLower(err.Error())
	return strings.Contains(normalized, "unexpected end of input") || strings.Contains(normalized, "eof")
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

func messagesContainClaudeCodeSystemPrompt(messages []model.Message) bool {
	for _, msg := range messages {
		switch strings.ToLower(strings.TrimSpace(msg.Role)) {
		case "system", "developer":
			text := ""
			if msg.Content.Content != nil {
				text = strings.TrimSpace(*msg.Content.Content)
			}
			if strings.HasPrefix(text, "x-anthropic-billing-header:") || strings.Contains(text, "built on Anthropic's Claude Agent SDK") {
				return true
			}
		}
	}
	return false
}

// copyHeaders 复制请求头，过滤 hop-by-hop 头
func (ra *relayAttempt) copyHeaders(outboundRequest *http.Request) {
	cliShapedCaller := clientIsCLIShaped(ra.c.Request)
	for key, values := range ra.c.Request.Header {
		if !shouldForwardClientHeaderForCaller(key, cliShapedCaller) {
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
// lowercaseAnthropicCLIHeaderNames 把四个 anthropic 系头名改回真 CLI 线上用的全小写。
// 实测(2026-10-10, 22 份配对抓包, 按腿分组): 真 CLI 直连 sent `anthropic-beta` /
// `anthropic-version` / `anthropic-dangerous-direct-browser-access` / `x-app` 全小写 9/9,
// 而我们的出站是 `Anthropic-Beta` / `Anthropic-Version` / `Anthropic-Dangerous-...` / `X-App` 8/8
// —— 上游可识别的非 CLI 特征(node/undici 线上就是小写, Go 的 http.Header.Set 会把头名规范化成
// 首字母大写)。只有直接写 map 键能绕过规范化, 所以放在**真正发出前的最后一步**: 上游逻辑里按规范名
// 读这些头的地方(Header.Get 走规范名)不受影响。
func lowercaseAnthropicCLIHeaderNames(h http.Header) {
	// X-Stainless-OS: 真 CLI 抓包里是 `X-Stainless-OS`(OS 全大写), 而 Go 的规范化产出
	// `X-Stainless-Os` —— 实测 21 份真 CLI 抓包 21/21 都是 `X-Stainless-OS`。这个头不在下面的
	// 小写名单里(它整体是规范大写), 单独按键名精确重写。
	if values, ok := h["X-Stainless-Os"]; ok && len(values) > 0 {
		delete(h, "X-Stainless-Os")
		h["X-Stainless-OS"] = append(h["X-Stainless-OS"], values...)
	}
	for _, wire := range []string{
		"anthropic-beta",
		"anthropic-version",
		"anthropic-dangerous-direct-browser-access",
		"x-app",
	} {
		canonical := textproto.CanonicalMIMEHeaderKey(wire)
		values, ok := h[canonical]
		if !ok || len(values) == 0 {
			continue
		}
		delete(h, canonical)
		h[wire] = append(h[wire], values...)
	}
}

func (ra *relayAttempt) sendRequest(req *http.Request) (*http.Response, error) {
	lowercaseAnthropicCLIHeaderNames(req.Header)
	httpClient, err := helper.ChannelHttpClient(ra.channel)
	if err != nil {
		log.Warnf("failed to get http client: %v", err)
		return nil, err
	}

	// 只给"等上游下发响应头"这一段加预算（默认 0 = 不启用，旧行为不变）。
	// 为什么需要：首内容守卫是在拿到响应头之后才起表的，所以"上游收下请求却一直
	// 不下发响应头"这一档此前没有任何东西兜底（隔离实例实测：设了 10s 仍挂满 60s）。
	// 作用范围严格限定在响应头：拿到头就停表，正文 SSE 继续用原 context 读，绝不被
	// 这个预算掐断；计时器只在我们这一侧，出站字节一个不改。
	// 预算触发就是一次普通的"这次尝试失败"：按 504 计入渠道健康、按既有规则换家并进入
	// 自动救援，因此救援能力不被削弱。
	if budget := currentUpstreamHeaderTimeout(); budget > 0 {
		ctx, cancel := context.WithCancel(req.Context())
		timer := time.AfterFunc(budget, cancel)
		response, err := helper.DoPreserveMethodRedirect(httpClient, req.WithContext(ctx))
		if timer.Stop() {
			// 响应头按时到达。这里刻意不调用 cancel：该 context 还要供正文流式读取
			// 使用，等父 context（本次客户端请求）结束时自然释放。
			return response, err
		}
		// 预算已经触发：cancel 已执行或即将执行，这个 context 必然被取消，因此即使拿回了
		// 响应（Stop 与回调之间的竞态窗口），它的正文也已经不可信——读它只会得到
		// context canceled。这一档统一判定为"本次尝试失败"，绝不带着一个将死的 context
		// 返回成功头，否则调用方会拿到一个必然中断的流。
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		if req.Context().Err() != nil {
			// 整次请求先结束（客户端取消/超时）：保留既有取消归因，不能被误记成上游沉默，
			// 否则会污染渠道健康统计、把客户端的问题算到上游头上。
			if err == nil {
				err = context.Canceled
			}
			return nil, err
		}
		log.Warnf("upstream header timeout (%s), switching channel", budget)
		return nil, &localRelayError{
			status:   http.StatusGatewayTimeout,
			code:     "octopus_upstream_header_timeout",
			strategy: "upstream_header_timeout;upstream_forwarded=true",
			message:  fmt.Sprintf("upstream did not send response headers within %s", budget),
		}
	}

	response, err := helper.DoPreserveMethodRedirect(httpClient, req)
	if err != nil {
		log.Warnf("failed to send request: %v", err)
		return nil, err
	}
	if response == nil || response.Body == nil {
		// C11 defensive guard: the redirect-preserving client should never return a
		// success without a body, but callers read response.Body directly (and the
		// deferred Close below would nil-panic) — fail this attempt instead.
		return nil, fmt.Errorf("upstream returned no response body")
	}

	return response, nil
}

// handleStreamResponse 处理流式响应
func (ra *relayAttempt) handleStreamResponse(ctx context.Context, response *http.Response, outAdapter model.Outbound) error {
	if ct := response.Header.Get("Content-Type"); ct != "" && !strings.Contains(strings.ToLower(ct), "text/event-stream") {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 16*1024))
		return fmt.Errorf("upstream returned non-SSE content-type %q for stream request: %s", ct, string(body))
	}

	// Some upstreams return HTTP errors (500/502/503) with Content-Type: text/event-stream
	// but a JSON error body. The SSE reader chokes on JSON, produces zero events, and the
	// relay reports the opaque "upstream stream ended without internal response". Peek the
	// first byte: if the body starts with '{' and the status is not 2xx, read it as a JSON
	// error and surface the upstream message instead of swallowing it behind a stream error.
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		br := bufio.NewReaderSize(response.Body, 4*1024)
		peek, err := br.Peek(1)
		if err == nil && len(peek) > 0 && peek[0] == '{' {
			body, _ := io.ReadAll(io.LimitReader(br, 16*1024))
			response.Body.Close()
			return fmt.Errorf("upstream %d (stream content-type) body: %s", response.StatusCode, string(body))
		}
		response.Body = io.NopCloser(br)
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
	// A small buffer lets the reader prefetch upcoming SSE events while this consumer
	// transforms/writes the current one, smoothing inter-token jitter; done-select in
	// the reader keeps early exits leak-free regardless of buffer size.
	results := make(chan sseReadResult, 8)
	// done is closed when this handler returns on ANY path (first-token / data-interval
	// timeout, client disconnect via ctx, a transform/write error, or normal stream end).
	// The reader's channel sends select on it, so a reader parked on `results <- ...` after
	// the consumer already left — an early timeout/disconnect that never drains the cap-1
	// channel — unblocks and exits instead of leaking a goroutine (and its buffered event)
	// for the process lifetime.
	done := make(chan struct{})
	defer close(done)
	safe.SafeGo("relay-sse-reader", func() {
		defer close(results)
		readCfg := &sse.ReadConfig{MaxEventSize: maxSSEEventSize}
		for ev, err := range sse.Read(response.Body, readCfg) {
			if err != nil {
				select {
				case results <- sseReadResult{err: err}:
				case <-done:
				}
				return
			}
			select {
			case results <- sseReadResult{eventType: ev.Type, data: ev.Data}:
			case <-done:
				return
			}
		}
	})

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
	// 第三道钟: 整次请求的绝对时长上限。刻意在这里只取"剩余额度", 所以它跨尝试是绝对的——
	// 换渠道/重试都继承剩下的部分, 不会重新赠送一个完整窗口; 上游持续吐字节也重置不了它。
	totalTimeoutC, stopTotalTimeout := ra.armTotalTimeout()
	// Closure, not a direct defer: when the ceiling passes while content is still flowing the
	// cut point re-arms, and the timer that then needs stopping is the new one.
	defer func() { stopTotalTimeout() }()
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
	// A few Responses-compatible providers omit response.completed after a sequential
	// tool call, so the relay historically treated output_item.done as a terminal
	// fallback. Give a conforming provider a short chance to send response.completed
	// first: that event carries authoritative usage, including cached_tokens. Returning
	// immediately at output_item.done silently discarded that usage after Codex requests
	// began consistently setting parallel_tool_calls=false.
	var toolCallCompletionTimer *time.Timer
	var toolCallCompletionC <-chan time.Time
	armToolCallCompletionFallback := func() {
		if toolCallCompletionTimer == nil {
			toolCallCompletionTimer = time.NewTimer(toolCallCompletionGracePeriod)
			toolCallCompletionC = toolCallCompletionTimer.C
			return
		}
		if !toolCallCompletionTimer.Stop() {
			select {
			case <-toolCallCompletionTimer.C:
			default:
			}
		}
		toolCallCompletionTimer.Reset(toolCallCompletionGracePeriod)
		toolCallCompletionC = toolCallCompletionTimer.C
	}
	disarmToolCallCompletionFallback := func() {
		if toolCallCompletionTimer == nil {
			return
		}
		if !toolCallCompletionTimer.Stop() {
			select {
			case <-toolCallCompletionTimer.C:
			default:
			}
		}
		toolCallCompletionC = nil
	}
	defer disarmToolCallCompletionFallback()
	lastWriteAt := time.Now()
	streamDoneSeen := false
	upstreamResponsesCompletedSeen := false
	upstreamTerminalSeen := false
	sawUpstreamCompletion := false
	seenMeaningfulChunk := false
	// upstreamFinishSeen records a real per-turn stop marker on the upstream wire
	// (choices[].finish_reason). It is a terminal marker the other flags do not
	// capture, and the truncation detector must honour it so a chat upstream that
	// ends after a finish_reason chunk (no [DONE]) is not misreported as truncated.
	upstreamFinishSeen := false
	// responsesToolCallTerminalSeen records a responses sequential-CLI tool-call-done
	// that the relay treats as the turn's terminal boundary (the grace-timer path
	// synthesizes the terminal for it). It is a legitimate end, so the truncation
	// detector must not fire after one.
	responsesToolCallTerminalSeen := false
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
	// upstreamStreamTruncated reports the half-stream truncation the two "benign end"
	// branches below would otherwise misreport as success: real content already reached
	// the client, yet the upstream produced NO terminal signal for its protocol at all.
	// The negative set is exhaustive per protocol so a legitimate end is never
	// misreported: the [DONE] sentinel (streamDoneSeen), the message_stop /
	// response.completed envelope (upstreamTerminalSeen / sawUpstreamCompletion), a chat
	// choices[].finish_reason (upstreamFinishSeen) and the responses sequential-CLI
	// tool-call-done (responsesToolCallTerminalSeen). Gemini is deliberately excluded:
	// its inbound has no in-band failure frame, and its clean-EOF [DONE] synthesis is a
	// legitimate end that flushes buffered tool calls, so its behaviour must not change.
	upstreamStreamTruncated := func() bool {
		if !ra.wroteMeaningfulDownstream {
			return false
		}
		switch ra.inboundType {
		case inbound.InboundTypeOpenAIChat, inbound.InboundTypeOpenAIResponse, inbound.InboundTypeAnthropic:
		default:
			return false
		}
		return !upstreamTerminalSeen && !streamDoneSeen && !sawUpstreamCompletion &&
			!upstreamFinishSeen && !responsesToolCallTerminalSeen
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
		// 凭据脱敏: 合成帧同样要过还原闸门。这一帧是 oct 自己造的 (上游那帧可能根本没有
		// 正文), 而它的正文/工具参数取自入站适配器的累积状态 — 累积的是上游发来的、已脱敏
		// 的值。此前只有上游来的帧过闸门 (见下面逐事件路径), 合成帧直接写下游, 于是客户端
		// 在收尾帧 (Responses response.completed / 工具参数) 里拿到的是占位符, 而同一份文本
		// 的其它副本都已还原。调用方必须在本函数之后 flush: 还原器可能因前一个事件仍在等
		// 占位符闭合而把本帧排在队尾, 只有 Finish 才会按 FIFO 吐净。
		if len(data) > 0 {
			restored, rerr := ra.restoreClientStreamSse(data)
			if rerr != nil {
				log.Errorf("redact: client-format synthesized-stream restore failed: %v", rerr)
				return &localRelayError{
					status:   http.StatusBadGateway,
					code:     "octopus_upstream_stream_error",
					strategy: "stream_restore_error;upstream_forwarded=true",
					message:  fmt.Sprintf("failed to restore synthesized stream terminal event: %v", rerr),
				}
			}
			data = restored
		}
		if err := writeStreamData(data); err != nil {
			return err
		}
		streamDoneSeen = true
		return nil
	}

	// pendingPrelude buffers the non-meaningful stream opener (message_start /
	// response.created / a bare role delta) until the first chunk with real content
	// arrives. commitPendingPrelude flushes it ahead of that content and marks the
	// stream committed. Withholding it keeps ra.wroteMeaningfulDownstream false so a
	// pre-content upstream death can still fail over — the client has seen only
	// ignorable SSE comment heartbeats, never a committed envelope.
	// A provider that streams nothing but envelope events (message_start /
	// response.created / a bare role delta) would otherwise grow pendingPrelude
	// without bound: the upstream data-interval timer is reset by every event we
	// read, and the first-token timer is opt-in (group default 0, global default
	// 0 = disabled). So neither existing clock bounds this buffer. Cap it and fail
	// the attempt instead of buffering forever.
	//
	// Sized for headroom, not for tightness: a real opener is a few hundred bytes, and
	// the cap must not fire on a provider that legitimately echoes a large prompt or
	// instructions inside its opener event. 1MiB is orders of magnitude above any
	// plausible opener while still being a hard per-request bound.
	const maxPendingPreludeBytes = 1 << 20
	var pendingPrelude []byte
	bufferPrelude := func(chunk []byte) error {
		if len(pendingPrelude)+len(chunk) > maxPendingPreludeBytes {
			_ = response.Body.Close()
			return &localRelayError{
				status:   http.StatusBadGateway,
				code:     "octopus_upstream_stream_error",
				strategy: "stream_prelude_over_limit;upstream_forwarded=false",
				message:  fmt.Sprintf("upstream stream opener exceeded %d bytes without any content", maxPendingPreludeBytes),
			}
		}
		pendingPrelude = append(pendingPrelude, chunk...)
		return nil
	}
	commitPendingPrelude := func() error {
		if ra.wroteMeaningfulDownstream {
			return nil
		}
		if err := ra.requestState.commitBusiness(); err != nil {
			return err
		}
		if len(pendingPrelude) > 0 {
			if err := writeStreamData(pendingPrelude); err != nil {
				return err
			}
			pendingPrelude = nil
		}
		ra.wroteMeaningfulDownstream = true
		ra.noteMeaningfulContent()
		// Commit reached the client: the response is recovered, so release the rescue
		// deadline and let the body stream under the client context.
		ra.releaseRescueDeadline()
		return nil
	}

	// 凭据脱敏: 收尾/终止/报错前把客户端格式还原缓冲吐净 — 缓冲里是真实模型输出。
	flushRedactRestore := func() error {
		tail, err := ra.redactFlushClientSse()
		if err != nil {
			log.Errorf("redact: flush restored stream tail failed: %v", err)
			return &localRelayError{
				status:   http.StatusBadGateway,
				code:     "octopus_upstream_stream_error",
				strategy: "stream_restore_error;upstream_forwarded=true",
				message:  fmt.Sprintf("failed to restore stream event: %v", err),
			}
		}
		if len(tail) > 0 {
			return writeStreamData(tail)
		}
		return nil
	}

	if data, err := ra.streamPreludeData(ctx, outAdapter); err != nil {
		return err
	} else if len(data) > 0 {
		// Buffer the synthesized responses-over-chat opener (response.created) rather
		// than flushing it, so it takes part in deferred commit like any other opener.
		// Deliberately do NOT arm/cancel the first-token timer here: the opener is not
		// real content, so the slow-first-token channel switch must stay armed until an
		// actual content chunk arrives.
		if err := bufferPrelude(data); err != nil {
			return err
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
		case <-totalTimeoutC:
			// 第三道钟到点。它只管"救援等待"——等首内容、等重试/换渠道；一条**正在真出内容**的流
			// 不许被它砍断(大领导 2026-10 口径)。所以先看内容是否仍在流动：仍在流动就续期再看，
			// 不流动才按"已交付⇒诚实收尾 / 未交付⇒换家救援"处置。续期信号只认实质内容，无内容
			// 事件(心跳/开场)不续期——与 bd1bfb3 的绝对时钟口径一致。
			if ra.wroteMeaningfulDownstream && !ra.meaningfulContentIdle() {
				log.Warnf("relay request total timeout (%s) reached while content is still flowing, letting the stream run", ra.totalBudget)
				totalTimeoutC, stopTotalTimeout = ra.rearmTotalTimeoutCheck()
				continue
			}
			// 上游"一直在慢慢吐字节/一直在发开场事件"时上面两道钟都不会响, 只有这里能结束它。
			_ = response.Body.Close()
			if ra.wroteMeaningfulDownstream {
				return ra.committedTotalTimeoutError()
			}
			return ra.totalTimeoutError()
		case <-toolCallCompletionC:
			if !seenMeaningfulChunk {
				return errors.New("upstream stream ended without internal response")
			}
			if err := commitPendingPrelude(); err != nil {
				return err
			}
			// 凭据脱敏: 合成帧先写 (它会进还原器, 可能被前一个未闭合的占位符挡在队尾),
			// 再 flush 才能把缓冲按 FIFO 吐净。
			if err := writeSynthesizedDone(); err != nil {
				return err
			}
			if err := flushRedactRestore(); err != nil {
				return err
			}
			return nil
		case r, ok := <-results:
			if !ok {
				log.Infof("stream end")
				if !seenMeaningfulChunk && !sawUpstreamCompletion {
					return errors.New("upstream stream ended without internal response")
				}
				// A genuine completion (terminal event / [DONE] / upstream-completed
				// seen): flush any buffered opener so the client still gets a valid —
				// if empty — turn before we finish.
				if err := commitPendingPrelude(); err != nil {
					return err
				}
				// Upstream half-stream truncation: real content already reached the client,
				// yet the upstream ended with NO terminal marker at all. Synthesizing a
				// success tail here would fake a completed turn (and record the request as a
				// success); surface the failure in-band instead. Still return nil: returning
				// an error after business data was delivered could trigger a channel swap and
				// opaque replay of the whole body, which the SOP forbids.
				if upstreamStreamTruncated() {
					// 截断不是正常收尾: 先把还原缓冲吐净, 再写带内失败帧。
					if err := flushRedactRestore(); err != nil {
						return err
					}
					ra.writeTruncatedStreamTerminal()
					log.Warnf("upstream stream truncated before terminal event; wrote in-band failure frame (inbound=%v)", ra.inboundType)
					return nil
				}
				// 凭据脱敏: 合成帧先写 (进还原器), 再 flush 把缓冲按 FIFO 吐净。
				if err := writeSynthesizedDone(); err != nil {
					return err
				}
				if err := flushRedactRestore(); err != nil {
					return err
				}
				return nil
			}
			if r.err != nil {
				// go-sse 在上游没发 data:[DONE] / 完整事件边界就断开时返回
				// io.ErrUnexpectedEOF（错误串 "unexpected end of input"）。这是「流自然
				// 结束」而非协议错误：已提交过内容就按正常结束收尾（补 [DONE]），别再
				// 弹 error、也别触发换渠道——否则下游会误以为上游失败而中止。
				if isBenignUpstreamStreamEnd(r.err) {
					if seenMeaningfulChunk || sawUpstreamCompletion {
						if err := commitPendingPrelude(); err != nil {
							return err
						}
						// Upstream half-stream truncation (see the closed-channel branch above):
						// a "benign" read end with content already delivered but no terminal
						// marker is exactly the fake-success case — write the failure frame and
						// still return nil to avoid an opaque replay after business data.
						if upstreamStreamTruncated() {
							// 截断不是正常收尾: 先把还原缓冲吐净, 再写带内失败帧。
							if err := flushRedactRestore(); err != nil {
								return err
							}
							ra.writeTruncatedStreamTerminal()
							log.Warnf("upstream stream truncated before terminal event; wrote in-band failure frame (inbound=%v)", ra.inboundType)
							return nil
						}
						// 凭据脱敏: 合成帧先写 (进还原器), 再 flush 把缓冲按 FIFO 吐净。
						if err := writeSynthesizedDone(); err != nil {
							return err
						}
						if err := flushRedactRestore(); err != nil {
							return err
						}
						return nil
					}
					return errors.New("upstream stream ended without internal response")
				}
				log.Warnf("failed to read event: %v", r.err)
				// 凭据脱敏: 已提交后报错前先吐还原缓冲, 再走现有带内错误事件机制。
				if ra.wroteMeaningfulDownstream {
					if flushErr := flushRedactRestore(); flushErr != nil {
						log.Errorf("redact: flush before stream error failed: %v", flushErr)
					}
				}
				return fmt.Errorf("failed to read stream event: %w", r.err)
			}
			resetDataTimeout()
			// Classify the event once: the terminal / tool-call-done / completed /
			// [DONE] / keepalive checks below all used to JSON-decode this same
			// payload again just to read its "type", so every event was parsed three
			// to four times. transformStreamChunk still does the one full decode that
			// actually builds the forwarded chunk.
			eventClass := classifyStreamEvent(r.data)
			eventType := eventClass.eventType
			if eventType == "response.completed" {
				upstreamResponsesCompletedSeen = true
			}
			isTerminalEvent := eventClass.isTerminal(r.eventType)
			if isTerminalEvent {
				disarmToolCallCompletionFallback()
			} else if ra.shouldTreatResponsesToolCallDoneAsTerminal(outAdapter, eventClass) {
				armToolCallCompletionFallback()
				responsesToolCallTerminalSeen = true
			}
			if isTerminalEvent {
				upstreamTerminalSeen = true
			}
			if eventClass.isCompleted(r.eventType) {
				sawUpstreamCompletion = true
			}
			if requireResponsesCompleted && responsesStreamEventIsPrelude(eventType) {
				if _, err := outAdapter.TransformStream(ctx, []byte(r.data)); err != nil {
					log.Warnf("failed to transform responses stream prelude: %v", err)
					return fmt.Errorf("failed to transform responses stream prelude: %w", err)
				}
				continue
			}
			isDoneEvent := eventClass.isDone
			if isDoneEvent {
				streamDoneSeen = true
				if !seenMeaningfulChunk && !sawUpstreamCompletion {
					return errors.New("upstream stream ended without internal response")
				}
			}

			data, internalStream, err := ra.transformStreamChunk(ctx, r.data, outAdapter)
			meaningfulNow := internalStream != nil && internalStream.Object != "[DONE]" && internalStreamHasMeaningfulResponse(internalStream)
			if meaningfulNow {
				seenMeaningfulChunk = true
			}
			if internalStreamHasFinish(internalStream) {
				upstreamFinishSeen = true
			}
			if err != nil {
				return &localRelayError{
					status:   http.StatusBadGateway,
					code:     "octopus_upstream_stream_error",
					strategy: "stream_transform_error;upstream_forwarded=true",
					message:  fmt.Sprintf("failed to transform stream event: %v", err),
				}
			}
			// 凭据脱敏: 在调用端格式产出后、写下游前逐事件还原占位符。占位符跨事件被缓冲时
			// 本次输出为空 (restore 返回空) → 不写, 待闭合半到达后再吐。
			if len(data) > 0 {
				restored, rerr := ra.restoreClientStreamSse(data)
				if rerr != nil {
					log.Errorf("redact: client-format stream restore failed: %v", rerr)
					return &localRelayError{
						status:   http.StatusBadGateway,
						code:     "octopus_upstream_stream_error",
						strategy: "stream_restore_error;upstream_forwarded=true",
						message:  fmt.Sprintf("failed to restore stream event: %v", rerr),
					}
				}
				data = restored
			}
			if len(data) == 0 {
				if eventClass.isKeepalive(r.eventType) {
					if writeErr := writeStreamData(ra.streamKeepaliveData()); writeErr != nil {
						return writeErr
					}
				}
				if isTerminalEvent && seenMeaningfulChunk {
					if writeErr := commitPendingPrelude(); writeErr != nil {
						return writeErr
					}
					// 凭据脱敏: 合成帧先进还原器 (可能被未闭合的占位符挡在队尾), 再 flush。
					if writeErr := writeSynthesizedDone(); writeErr != nil {
						return writeErr
					}
					if writeErr := flushRedactRestore(); writeErr != nil {
						return writeErr
					}
					return nil
				}
				continue
			}
			// Deferred commit: before the first real-content chunk, buffer the opener
			// (message_start / response.created / bare role delta) instead of flushing
			// it, and leave the first-token timer armed. Only ignorable comment
			// heartbeats have reached the client, so a pre-content upstream death still
			// fails over. Once content arrives, flush the buffered opener ahead of it and
			// mark the stream committed — later failures then surface to the client.
			if !ra.wroteMeaningfulDownstream && !meaningfulNow {
				if err := bufferPrelude(data); err != nil {
					return err
				}
				continue
			}
			if err := commitPendingPrelude(); err != nil {
				if streamTerminalSeen() && isClientAbortError(err) {
					log.Infof("client disconnected while writing buffered stream opener; treating stream as completed")
					return nil
				}
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

			if err := writeStreamData(data); err != nil {
				if streamTerminalSeen() && isClientAbortError(err) {
					log.Infof("client disconnected while writing terminal stream data; treating stream as completed")
					return nil
				}
				return err
			}
			if meaningfulNow {
				ra.noteMeaningfulContent()
			}
			if streamTerminalSeen() {
				// 凭据脱敏: 合成帧先写 (进还原器), 再 flush 把缓冲按 FIFO 吐净。
				if err := writeSynthesizedDone(); err != nil {
					return err
				}
				if err := flushRedactRestore(); err != nil {
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
	// Gemini is included for successful-end finalization only: its inbound
	// TransformStream treats the internal [DONE] sentinel as a flush trigger
	// for buffered tool calls and returns no [DONE] wire bytes, so a stream
	// that ends cleanly without a finish_reason frame or literal [DONE] (plain
	// EOF) still delivers complete functionCall parts instead of silently
	// dropping them. Failure paths (cancel/timeout/read error) never reach
	// this synthesis and must keep failing without flushing partial calls.
	case inbound.InboundTypeOpenAIChat, inbound.InboundTypeOpenAIResponse, inbound.InboundTypeAnthropic, inbound.InboundTypeGemini:
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

// Terminal-frame identifiers for an upstream half-stream truncation. Fixed and free
// of any upstream identity (public-repo no-leak rule): the message states only the
// protocol-level fact — the upstream ended before a terminal event — and names no
// host, account, channel or URL.
const (
	upstreamStreamTruncatedCode    = "octopus_upstream_stream_truncated"
	upstreamStreamTruncatedMessage = "upstream stream ended before a terminal event"
)

// writeTruncatedStreamTerminal surfaces an upstream half-stream truncation in the
// client's own inbound protocol. The upstream dropped the connection after real
// content had already been flushed downstream but WITHOUT any terminal marker
// (finish_reason / [DONE] / response.completed / message_stop), so the turn is
// incomplete. It reuses the same in-band failure frames as the committed-failure
// path (writeResponsesFailedSSE / writeAnthropicErrorSSE / writeChatErrorSSE) so the
// client learns the stream failed instead of being handed a synthesized success tail.
//
// It writes ONLY downstream bytes (c.Writer): no outbound header/TLS/UA/body is
// touched, so the outbound fingerprint shape is unchanged.
func (ra *relayAttempt) writeTruncatedStreamTerminal() {
	if ra == nil || ra.c == nil {
		return
	}
	switch ra.inboundType {
	case inbound.InboundTypeOpenAIResponse:
		writeResponsesFailedSSE(ra.c, ra.requestModel, upstreamStreamTruncatedCode, upstreamStreamTruncatedMessage)
	case inbound.InboundTypeAnthropic:
		writeAnthropicErrorSSE(ra.c, "api_error", upstreamStreamTruncatedMessage)
	case inbound.InboundTypeOpenAIChat:
		writeChatErrorSSE(ra.c, upstreamStreamTruncatedCode, upstreamStreamTruncatedMessage)
	}
}

// firstTokenHandoffFloor is the minimum first-token budget handed to the
// aggregation loop after an identification phase already consumed most of it:
// just enough to drain the bytes already buffered in memory (the replayed peek
// window) without racing the timer against the buffered events. It is NOT a
// fresh budget — the identification elapsed time is subtracted first.
const firstTokenHandoffFloor = 100 * time.Millisecond

// handleStreamResponseAsNonStream aggregates an upstream SSE stream into one
// complete JSON response for a non-stream client. identifyElapsed is the time
// the caller already spent identifying the response framing (the content-type
// fallback peek); it is counted against the first-token budget so the
// aggregation does not get a fresh full window on top of the identification
// wait (F8).
func (ra *relayAttempt) handleStreamResponseAsNonStream(ctx context.Context, response *http.Response, outAdapter model.Outbound, identifyElapsed time.Duration) error {
	if ct := response.Header.Get("Content-Type"); ct != "" && !strings.Contains(strings.ToLower(ct), "text/event-stream") {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 16*1024))
		return fmt.Errorf("upstream returned non-SSE content-type %q for forced responses stream request: %s", ct, string(body))
	}
	// Same JSON-error-under-SSE-content-type guard as handleStreamResponse (see rationale there).
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		br := bufio.NewReaderSize(response.Body, 4*1024)
		peek, err := br.Peek(1)
		if err == nil && len(peek) > 0 && peek[0] == '{' {
			body, _ := io.ReadAll(io.LimitReader(br, 16*1024))
			response.Body.Close()
			return fmt.Errorf("upstream %d (stream content-type) body: %s", response.StatusCode, string(body))
		}
		response.Body = io.NopCloser(br)
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
	// Same prefetch rationale as handleStreamResponse's cap-8 results channel.
	results := make(chan sseReadResult, 8)
	// See handleStreamResponse: done lets a reader parked on a full-buffer send exit when this
	// consumer returns early (idle-timeout / error) instead of leaking for the process life.
	done := make(chan struct{})
	defer close(done)
	safe.SafeGo("relay-sse-reader-nonstream", func() {
		defer close(results)
		readCfg := &sse.ReadConfig{MaxEventSize: maxSSEEventSize}
		for ev, err := range sse.Read(response.Body, readCfg) {
			if err != nil {
				select {
				case results <- sseReadResult{err: err}:
				case <-done:
				}
				return
			}
			select {
			case results <- sseReadResult{data: ev.Data}:
			case <-done:
				return
			}
		}
	})

	// A stalled or empty upstream must not hang a non-stream client until its own
	// deadline: mirror handleStreamResponse's first-token guard so we fail fast
	// (and let the balancer switch channels) when no meaningful token arrives.
	// Heartbeats / prelude events do NOT reset this — only a real token disarms it
	// below — so a ping-only stall is caught here rather than slipping past the
	// idle timeout, which every event (pings included) resets.
	var firstTokenTimer *time.Timer
	var firstTokenC <-chan time.Time
	if ra.firstTokenTimeOutSec > 0 {
		budget := time.Duration(ra.firstTokenTimeOutSec) * time.Second
		// 识别阶段已耗的时间计入首 token 预算, 不重新赠送完整窗口(F8)。
		// 识别本身受同一超时约束(超时即失败), 所以剩余额正常总是正数;
		// 边界处(识别恰好耗尽预算)只留排空内存缓冲的下限, 避免定时器和
		// 已缓冲事件赛跑。
		budget -= identifyElapsed
		if budget < firstTokenHandoffFloor {
			budget = firstTokenHandoffFloor
		}
		firstTokenTimer = time.NewTimer(budget)
		firstTokenC = firstTokenTimer.C
		defer func() {
			if firstTokenTimer != nil {
				firstTokenTimer.Stop()
			}
		}()
	}

	// No downstream client stream here, so we also guard against a stalled or
	// ping-only upstream with the same idle cutoff handleStreamResponse uses.
	var dataTimeoutC <-chan time.Time
	var dataTimeoutTimer *time.Timer
	dataIntervalTimeout := currentStreamDataIntervalTimeout()
	if dataIntervalTimeout > 0 {
		dataTimeoutTimer = time.NewTimer(dataIntervalTimeout)
		dataTimeoutC = dataTimeoutTimer.C
		defer dataTimeoutTimer.Stop()
	}
	// 第三道钟: 整次请求的绝对时长上限。刻意在这里只取"剩余额度", 所以它跨尝试是绝对的——
	// 换渠道/重试都继承剩下的部分, 不会重新赠送一个完整窗口; 上游持续吐字节也重置不了它。
	totalTimeoutC, stopTotalTimeout := ra.armTotalTimeout()
	defer stopTotalTimeout()
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
	disarmFirstToken := func() {
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
	var toolCallCompletionTimer *time.Timer
	var toolCallCompletionC <-chan time.Time
	armToolCallCompletionFallback := func() {
		if toolCallCompletionTimer == nil {
			toolCallCompletionTimer = time.NewTimer(toolCallCompletionGracePeriod)
			toolCallCompletionC = toolCallCompletionTimer.C
			return
		}
		if !toolCallCompletionTimer.Stop() {
			select {
			case <-toolCallCompletionTimer.C:
			default:
			}
		}
		toolCallCompletionTimer.Reset(toolCallCompletionGracePeriod)
		toolCallCompletionC = toolCallCompletionTimer.C
	}
	disarmToolCallCompletionFallback := func() {
		if toolCallCompletionTimer == nil {
			return
		}
		if !toolCallCompletionTimer.Stop() {
			select {
			case <-toolCallCompletionTimer.C:
			default:
			}
		}
		toolCallCompletionC = nil
	}
	defer disarmToolCallCompletionFallback()

	// The inbound adapter is shared across every channel/key retry (relayRequest.inAdapter)
	// and accumulates stream chunks until a successful GetInternalResponse clears them, so
	// a failed attempt that produced partial content would leak into the successful
	// failover attempt's aggregated body ("OLDNEW"). The keepalive below keeps the client
	// alive long enough for failover to actually land, making that leakage reachable — so
	// reset the adapter's stream state before transforming this attempt's chunks.
	if r, ok := ra.inAdapter.(interface{ ResetStreamAggregation() }); ok {
		r.ResetStreamAggregation()
	}

	// Keep the non-stream client (and any idle-timeout reverse proxy in front of it)
	// warm while a slow/reasoning upstream buffers, mirroring CLIProxyAPI's blank-line
	// keepalive. A "\n" is JSON insignificant whitespace, so it never corrupts the
	// aggregated body; it commits the response head but NOT wroteMeaningfulDownstream,
	// so failover across channels stays possible (see the all-channels-failed path).
	// X-Accel-Buffering: no tells an nginx/1Panel front proxy not to buffer the
	// heartbeats, so they actually reach the client instead of being held back.
	ra.c.Header("Content-Type", "application/json")
	ra.c.Header("X-Accel-Buffering", "no")
	var keepaliveC <-chan time.Time
	var keepaliveTicker *time.Ticker
	if keepaliveInterval := currentStreamKeepaliveInterval(); keepaliveInterval > 0 {
		keepaliveTicker = time.NewTicker(keepaliveInterval)
		keepaliveC = keepaliveTicker.C
		defer keepaliveTicker.Stop()
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
		case <-firstTokenC:
			log.Warnf("first token timeout (%ds), switching channel", ra.firstTokenTimeOutSec)
			_ = response.Body.Close()
			return fmt.Errorf("first token timeout (%ds)", ra.firstTokenTimeOutSec)
		case <-dataTimeoutC:
			log.Warnf("forced responses stream data interval timeout (%s), closing upstream stream", dataIntervalTimeout)
			_ = response.Body.Close()
			return &localRelayError{
				status:   http.StatusGatewayTimeout,
				code:     "octopus_upstream_stream_timeout",
				strategy: "stream_data_interval_timeout;upstream_forwarded=true",
				message:  fmt.Sprintf("upstream stream timed out waiting for SSE event (%s)", dataIntervalTimeout),
			}
		case <-totalTimeoutC:
			// 第三道钟到点(非流式聚合路径): 处置同上——已交付诚实收尾, 未交付换家/进救援。
			_ = response.Body.Close()
			if ra.wroteMeaningfulDownstream {
				return ra.committedTotalTimeoutError()
			}
			return ra.totalTimeoutError()
		case <-toolCallCompletionC:
			sawUpstreamCompletion = true
			break readLoop
		case <-keepaliveC:
			// Blank-line heartbeat: JSON insignificant whitespace that keeps the client
			// connection warm without committing content, so failover stays possible.
			// Mark JSON-committed BEFORE the write: a Write that commits the response head
			// then returns a partial-write error would otherwise leave the
			// all-channels-failed path splicing an SSE error into this JSON stream.
			ra.wroteNonStreamJSONKeepalive = true
			if _, werr := ra.c.Writer.Write([]byte("\n")); werr != nil {
				_ = response.Body.Close()
				return fmt.Errorf("client disconnected during forced responses stream keepalive: %w", werr)
			}
			ra.c.Writer.Flush()
		case r, ok := <-results:
			if !ok {
				break readLoop
			}
			if r.err != nil {
				return fmt.Errorf("failed to read stream event: %w", r.err)
			}
			resetDataTimeout()
			// Same single classification parse as the streaming loop above: the
			// prelude / completed / tool-call-done / [DONE] checks share one decode
			// instead of re-parsing the payload for each of them.
			eventClass := classifyStreamEvent(r.data)
			eventType := eventClass.eventType
			if requireResponsesCompleted && responsesStreamEventIsPrelude(eventType) {
				if _, err := outAdapter.TransformStream(ctx, []byte(r.data)); err != nil {
					return fmt.Errorf("failed to transform responses stream prelude: %w", err)
				}
				continue
			}
			if eventClass.isCompleted(eventType) {
				sawUpstreamCompletion = true
				disarmToolCallCompletionFallback()
				// A terminal completion ends the turn even with zero output tokens, so
				// stop the first-token guard: a prompt empty-but-legitimate finish must
				// not be misreported as a first-token timeout.
				disarmFirstToken()
			} else if ra.shouldTreatResponsesToolCallDoneAsTerminal(outAdapter, eventClass) {
				// Preserve the old compatibility fallback, but wait briefly for the
				// authoritative response.completed event and its usage payload.
				armToolCallCompletionFallback()
				disarmFirstToken()
			}
			isDoneEvent := eventClass.isDone
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
				if !seenMeaningfulChunk {
					seenMeaningfulChunk = true
					if ra.metrics != nil && ra.metrics.FirstTokenTime.IsZero() {
						ra.setFirstTokenTime(time.Now())
					}
					// First real token arrived: disarm the first-token guard so a
					// slow-but-progressing upstream is never cut mid-generation.
					disarmFirstToken()
				}
			}
			// A terminal completion carries the full response; stop reading once we have
			// it instead of blocking on a [DONE]/EOF the upstream may never send — that
			// would hang until the idle timeout, and the completion already disarmed the
			// first-token guard. The post-loop path synthesizes the missing [DONE].
			if sawUpstreamCompletion {
				break readLoop
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
	// 凭据脱敏: 聚合出的调用端格式 body 在写下游前一次性还原占位符。还原失败一律 fail-closed:
	// 头未提交时由上层 failover; 头已由空行 keepalive 提交且无更优渠道时, 请求级收尾路径
	// (relay.go writeNonStreamJSONError) 会写出带内 JSON 错误 —— 绝不把未还原的
	// {{Redact:…}} 占位符当 200 返回给调用方。
	if ra.redactSession != nil && ra.redactApplied {
		restored, rerr := ra.redactSession.RestoreJSONBody(inResponse)
		if rerr != nil {
			return fmt.Errorf("redact: restored aggregated inbound response failed: %w", rerr)
		}
		inResponse = restored
	}
	ra.wroteMeaningfulDownstream = true
	ra.releaseRescueDeadline()
	if ra.wroteNonStreamJSONKeepalive {
		// Response head already committed by blank-line keepalives; append the aggregated
		// JSON body — valid JSON after the leading insignificant whitespace.
		if _, werr := ra.c.Writer.Write(inResponse); werr != nil {
			return fmt.Errorf("failed to write aggregated forced responses body: %w", werr)
		}
		ra.c.Writer.Flush()
	} else {
		ra.c.Data(http.StatusOK, "application/json", inResponse)
	}
	return nil
}

func (ra *relayAttempt) handleNonStreamResponseAsStream(ctx context.Context, response *http.Response, outAdapter model.Outbound) error {
	limitUpstreamResponseBody(response)
	// Decompress any upstream Content-Encoding before the outbound parses the body. The
	// claude/anthropic outbound advertises gzip,deflate,br,zstd and the shared transport has
	// DisableCompression=true, so Go does not auto-decompress here. The three sibling response
	// paths (handleStreamResponse, handleStreamResponseAsNonStream, handleResponse) all unwrap;
	// this fallback path is reached when an anthropic stream is retried as non-stream after
	// 502/503/504/520, and without this line a compressed 200 JSON fails to parse, silently
	// breaking the whole stream-to-non-stream fallback feature.
	if err := unwrapResponseEncoding(response); err != nil {
		return fmt.Errorf("failed to unwrap upstream response encoding: %w", err)
	}
	internalResponse, err := outAdapter.TransformResponse(ctx, response)
	if err != nil {
		log.Warnf("failed to transform fallback non-stream response: %v", err)
		return fmt.Errorf("failed to transform fallback non-stream response: %w", err)
	}
	ra.captureUpstreamDeclaredModel(internalResponse)
	// When model_mapping remapped the request model, restore the client-visible name
	// before converting the non-stream response to SSE chunks.
	if ra.modelMapped && internalResponse != nil {
		internalResponse.Model = ra.requestModel
	}

	ra.c.Header("Content-Type", "text/event-stream")
	ra.c.Header("Cache-Control", "no-cache")
	ra.c.Header("Connection", "keep-alive")
	ra.c.Header("X-Accel-Buffering", "no")
	ra.c.Header("X-Octopus-Stream-Fallback", "non-stream-upstream")

	if err := ra.requestState.commitBusiness(); err != nil {
		return err
	}
	ra.wroteMeaningfulDownstream = true
	ra.releaseRescueDeadline()
	for _, chunk := range internalResponseToStreamChunks(internalResponse) {
		data, err := ra.inAdapter.TransformStream(ctx, chunk)
		if err != nil {
			log.Warnf("failed to transform fallback stream: %v", err)
			return fmt.Errorf("failed to transform fallback stream: %w", err)
		}
		if len(data) == 0 {
			continue
		}
		// 凭据脱敏: 转换出的调用端 SSE chunk 在写下游前逐事件还原。此路径在写首字前
		// 已 commit (wroteMeaningfulDownstream=true), 还原失败只能带内报错, 不能 failover。
		if restored, rerr := ra.restoreClientStreamSse(data); rerr != nil {
			log.Errorf("redact: fallback stream restore failed: %v", rerr)
			return fmt.Errorf("redact: fallback stream restore failed: %w", rerr)
		} else {
			data = restored
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
	// 凭据脱敏: 逐 chunk 还原时若最后一个事件以半截占位符收尾, 还原器会把该通道整段
	// (含被并进去的真实前缀文字) 缓冲, 仅 finish() 才吐。主链路 handleStreamResponse
	// 在终止写出前会 flushRedactRestore; 这条非流转流兜底路径原本漏了收尾 flush, 半截
	// 尾部连同真实内容被静默丢弃。缓冲里的终止事件排在未吐内容之后 (队列按序), 故此处
	// 唯一一次 flush 吐出的尾帧仍严格在终止标记之前。失败按失败回传, 不追加成功结束。
	if tail, ferr := ra.redactFlushClientSse(); ferr != nil {
		log.Errorf("redact: fallback stream tail flush failed: %v", ferr)
		return fmt.Errorf("redact: fallback stream tail flush failed: %w", ferr)
	} else if len(tail) > 0 {
		if _, werr := ra.c.Writer.Write(tail); werr != nil {
			log.Infof("client disconnected during fallback stream tail write: %v", werr)
			return fmt.Errorf("client disconnected during fallback stream tail write: %w", werr)
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

func (ra *relayAttempt) transformStreamChunk(ctx context.Context, data string, outAdapter model.Outbound) ([]byte, *model.InternalLLMResponse, error) {
	internalStream, err := outAdapter.TransformStream(ctx, []byte(data))
	if err != nil {
		log.Warnf("failed to transform stream: %v", err)
		return nil, nil, err
	}
	if internalStream == nil {
		return nil, nil, nil
	}
	ra.captureUpstreamDeclaredModel(internalStream)
	// When model_mapping remapped the request model, restore the client-visible name
	// in the response chunk so the upstream name is never leaked to the client.
	if ra.modelMapped && internalStream.Model != "" {
		internalStream.Model = ra.requestModel
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

// internalStreamHasFinish reports whether an upstream chunk carried a real per-turn
// stop marker (choices[].finish_reason). For the chat wire this is the only terminal
// signal: the SSE-level streamDoneSeen / sawUpstreamCompletion flags only fire on the
// literal [DONE] sentinel or a message_stop / response.completed envelope, so a chat
// stream that ends with a finish_reason chunk and then just closes the socket is a
// *completed* turn, not a truncation. The truncation detector must treat it as such to
// avoid a false in-band failure frame on an otherwise clean EOF.
func internalStreamHasFinish(resp *model.InternalLLMResponse) bool {
	if resp == nil {
		return false
	}
	for _, choice := range resp.Choices {
		if choice.FinishReason != nil && strings.TrimSpace(*choice.FinishReason) != "" {
			return true
		}
	}
	return false
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

// streamEventClass is the one lightweight parse of an SSE payload that the
// stream loops' dispatch checks share. Each check used to decode the same event
// itself just to read its "type", so a single chunk was JSON-parsed three to
// four times per iteration; classifyStreamEvent does it once and every check
// below reads the cached fields.
type streamEventClass struct {
	// eventType is the payload's top-level "type", trimmed. Empty when the
	// payload is not a JSON object or is the [DONE] sentinel.
	eventType string
	// itemType is the payload's nested "item"."type", trimmed. Empty when absent.
	itemType string
	// isDone marks the "[DONE]" sentinel, which is never JSON.
	isDone bool
}

func classifyStreamEvent(data string) streamEventClass {
	trimmed := strings.TrimSpace(data)
	if trimmed == "" {
		return streamEventClass{}
	}
	if strings.HasPrefix(trimmed, "[DONE]") {
		return streamEventClass{isDone: true}
	}
	var envelope struct {
		Type string `json:"type"`
		Item *struct {
			Type string `json:"type"`
		} `json:"item,omitempty"`
	}
	if err := json.Unmarshal([]byte(trimmed), &envelope); err != nil {
		return streamEventClass{}
	}
	class := streamEventClass{eventType: strings.TrimSpace(envelope.Type)}
	if envelope.Item != nil {
		class.itemType = strings.TrimSpace(envelope.Item.Type)
	}
	return class
}

func (ra *relayAttempt) shouldTreatResponsesToolCallDoneAsTerminal(outAdapter model.Outbound, class streamEventClass) bool {
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
	return class.isToolCallDone()
}

// isToolCallDone reports a responses tool-call finalization event.
func (c streamEventClass) isToolCallDone() bool {
	if c.isDone || c.eventType != "response.output_item.done" {
		return false
	}
	switch c.itemType {
	case "tool_call", "function_call", "local_shell_call", "tool_search_call", "custom_tool_call", "mcp_tool_call":
		return true
	default:
		return false
	}
}

// isTerminal reports the upstream stream's terminal boundary: the [DONE]
// sentinel, or a terminal type carried by either the SSE event name or the
// payload itself.
func (c streamEventClass) isTerminal(sseEventType string) bool {
	if c.isDone {
		return true
	}
	switch strings.TrimSpace(sseEventType) {
	case "message_stop", "response.completed":
		return true
	}
	switch c.eventType {
	case "message_stop", "response.completed":
		return true
	default:
		return false
	}
}

// isCompleted reports a REAL terminal completion from the upstream
// (message_stop / response.completed), excluding the [DONE] sentinel.
// [DONE] only marks the SSE channel closing, not a successful completion, so it
// must not be treated as "the model completed" when deciding whether an
// otherwise content-less stream is a legitimate empty completion vs a dropped one.
func (c streamEventClass) isCompleted(sseEventType string) bool {
	if c.isDone {
		return false
	}
	switch strings.TrimSpace(sseEventType) {
	case "message_stop", "response.completed":
		return true
	}
	switch c.eventType {
	case "message_stop", "response.completed":
		return true
	default:
		return false
	}
}

func (c streamEventClass) isKeepalive(sseEventType string) bool {
	if strings.EqualFold(strings.TrimSpace(sseEventType), "ping") {
		return true
	}
	return strings.EqualFold(c.eventType, "ping")
}

// isStreamKeepaliveEvent keeps the raw-payload form for callers that have not
// already classified the event.
func isStreamKeepaliveEvent(eventType string, data string) bool {
	return classifyStreamEvent(data).isKeepalive(eventType)
}

func (ra *relayAttempt) streamKeepaliveData() []byte {
	// Before the first meaningful chunk is committed downstream, keep the connection
	// warm with an SSE comment (":\n\n") for EVERY inbound protocol. A comment is
	// ignored by every SSE client and commits nothing, so a pre-content upstream
	// failure can still fail over. An Anthropic "event: ping" would be a real event
	// that must follow message_start — emitting it before commit would both violate
	// the protocol ordering and count as a committed envelope.
	if ra == nil || !ra.wroteMeaningfulDownstream {
		return []byte(":\n\n")
	}
	if ra.inboundType == inbound.InboundTypeAnthropic {
		return []byte("event: ping\ndata: {\"type\":\"ping\"}\n\n")
	}
	return []byte(":\n\n")
}

// setDownstreamSSEHeadersCtx stages the text/event-stream + anti-buffering response
// headers on the downstream writer. Safe to call more than once: gin just overwrites
// the staged values, and it becomes a no-op once the response head is committed.
func setDownstreamSSEHeadersCtx(c *gin.Context) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
}

func (ra *relayAttempt) setDownstreamSSEHeaders() { setDownstreamSSEHeadersCtx(ra.c) }

// startDownstreamFirstByteKeepalive is the context-only sibling of
// (*relayAttempt).startFirstByteKeepalive for the raw-protocol streaming path
// (codex /responses etc.), which has no relayAttempt. It injects ignorable SSE comment
// heartbeats (":\n\n") to the downstream client while a streaming request waits for its
// first upstream byte, so a slow-first-token upstream does not trip the client's idle
// timeout. It commits the SSE / anti-buffering headers before the first heartbeat so a
// front proxy does not buffer them. The returned stop() halts injection and waits
// (WaitGroup) for the goroutine to exit, so the caller's later writes to c.Writer never
// race it. Downstream-only: it never touches the upstream request, so it cannot affect
// the codex/claude/gemini upstream fingerprint, and it only ever writes ignorable
// comments (never a committed envelope), so a pre-content upstream failure can still fail
// over. delay<=0 or interval<=0 is a no-op.
func startDownstreamFirstByteKeepalive(ctx context.Context, c *gin.Context) func() {
	return startDownstreamKeepaliveWithDelay(ctx, c, currentFirstByteKeepaliveDelay())
}

// startInterventionKeepalive is the hold-loop variant: the intervention hold restarts
// its keepalive every round (stopped before goto runIterator, restarted on re-entry),
// so one round's keepalive lifetime is bounded by the hold backoff cap (~15s) plus the
// attempt time. The 20s first-byte delay would never fire inside a round — the held
// client would sit byte-silent until the rescue budget dies (a codex TUI's ~300s
// stream-idle timeout then races the budget). The dedicated short delay keeps
// "working" heartbeats flowing. Downstream-only; never touches the upstream fingerprint.
func startInterventionKeepalive(ctx context.Context, c *gin.Context) func() {
	return startDownstreamKeepaliveWithDelay(ctx, c, currentInterventionKeepaliveDelay())
}

// startDownstreamKeepaliveWithDelay injects ignorable SSE comment heartbeats (":\n\n")
// downstream after delay, then every relay_stream_keepalive_interval_seconds, until the
// returned stop func is called. delay<=0 or interval<=0 is a no-op.
func startDownstreamKeepaliveWithDelay(ctx context.Context, c *gin.Context, delay time.Duration) func() {
	return startDownstreamKeepaliveWithDelayUnless(ctx, c, delay, nil)
}

// startDownstreamKeepaliveWithDelayUnless is startDownstreamKeepaliveWithDelay with a veto: while
// skip() reports true the loop stays alive but writes nothing, so the response is not committed and
// a failover that ends in failure can still hand back a real status code (see
// relayRequest.suppressPreContentKeepalive). Setting the headers is not a commit — only the first
// body write is, which is why the veto can withhold the response head too.
func startDownstreamKeepaliveWithDelayUnless(ctx context.Context, c *gin.Context, delay time.Duration, skip func() bool) func() {
	if delay <= 0 {
		return func() {}
	}
	interval := currentStreamKeepaliveInterval()
	if interval <= 0 {
		return func() {}
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	stopped := false
	wg.Add(1)
	safe.SafeGo("first-byte-keepalive-raw", func() {
		defer wg.Done()
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		headersSet := false
		for {
			mu.Lock()
			if stopped {
				mu.Unlock()
				return
			}
			if skip != nil && skip() {
				// Vetoed: withhold the write (and with it the header commit) so the response stays
				// uncommitted; keep looping in case the veto lifts.
				mu.Unlock()
			} else {
				if !headersSet {
					// Commit the anti-buffering headers before the first heartbeat byte so a
					// front proxy (nginx/OpenResty) forwards the pre-content heartbeats instead
					// of buffering them. proxySSEWithOptions re-sets the same headers later
					// (idempotent, no-op once committed).
					setDownstreamSSEHeadersCtx(c)
					headersSet = true
				}
				_, werr := c.Writer.Write([]byte(":\n\n"))
				c.Writer.Flush()
				mu.Unlock()
				if werr != nil {
					return
				}
			}
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	})
	var once sync.Once
	return func() {
		once.Do(func() {
			mu.Lock()
			stopped = true
			mu.Unlock()
			close(done)
			wg.Wait()
		})
	}
}

// startFirstByteKeepalive 在等待上游首字节期间，向下游注入心跳防前置反代空闲掐断。
// 返回的 stop 函数会停止注入并等待注入 goroutine 完全退出（保证之后主写入不与它并发）。
// delay<=0 或 interval<=0 时为 no-op。
func (ra *relayAttempt) startFirstByteKeepalive(ctx context.Context) func() {
	delay := currentFirstByteKeepaliveDelay()
	if delay <= 0 {
		return func() {}
	}
	interval := currentStreamKeepaliveInterval()
	if interval <= 0 {
		return func() {}
	}
	// Reset the stop flag so a second call within the same relayAttempt re-arms the
	// heartbeat — a goto retryWithAdapter fallback (Responses→Chat, cursor/encrypted
	// -content recovery, Anthropic stream→non-stream) re-sends upstream and its slow
	// first byte should be covered too. The prior stop() already joined its goroutine
	// via wg.Wait(), so nothing races this reset.
	ra.prewarmMu.Lock()
	ra.prewarmStopped = false
	ra.prewarmMu.Unlock()
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	safe.SafeGo("first-byte-keepalive", func() {
		defer wg.Done()
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		// Commit the SSE / anti-buffering headers before the first heartbeat byte: the
		// first Write flushes the response head, and without X-Accel-Buffering:no a front
		// proxy (nginx/OpenResty) would buffer these pre-content heartbeats so the client
		// still sees nothing. handleStreamResponse re-sets the same headers (idempotent)
		// once the upstream response arrives. Guarded by prewarmMu so it is ordered before
		// the writes and never races the main goroutine (which only runs after stop()).
		ra.prewarmMu.Lock()
		if !ra.prewarmStopped {
			ra.setDownstreamSSEHeaders()
		}
		ra.prewarmMu.Unlock()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			ra.prewarmMu.Lock()
			if ra.prewarmStopped {
				ra.prewarmMu.Unlock()
				return
			}
			if ra.suppressPreContentKeepalive.Load() && !ra.wroteBusinessData {
				// 假成功防线 (见 relayRequest.suppressPreContentKeepalive): 救援期间一律不发预内容
				// 心跳 —— 心跳会提交 200, 让"全渠道失败"变成调用方眼里的"成功但空回答"。
				ra.prewarmMu.Unlock()
			} else {
				_, werr := ra.c.Writer.Write(ra.streamKeepaliveData())
				ra.c.Writer.Flush()
				ra.prewarmMu.Unlock()
				if werr != nil {
					return
				}
			}
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	})
	var once sync.Once
	return func() {
		once.Do(func() {
			ra.prewarmMu.Lock()
			ra.prewarmStopped = true
			ra.prewarmMu.Unlock()
			close(done)
			wg.Wait()
		})
	}
}

// responseIsEventStream reports whether the upstream response carries an SSE
// content-type. Some upstreams (web-chat bridges) ignore "stream": false and reply
// with text/event-stream anyway; handleResponse hands such responses to the
// forced-stream aggregation path so a non-stream client still receives one complete
// JSON response instead of an unmarshal error on the leading 'd' of "data:".
func responseIsEventStream(response *http.Response) bool {
	return strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream")
}

// peekedBody replays a bufio.Reader that was used to Peek a response body.
// bufio.Reader.Peek consumes the underlying stream's error state (its readErr is
// one-shot), so a non-EOF error hit during the peek — e.g. the upstream
// body-size limit error, whose probe read may also swallow the overflow byte —
// would otherwise vanish and the body would end in a clean EOF. peekedBody
// re-injects that error after the replayed bytes are drained.
type peekedBody struct {
	br      *bufio.Reader
	orig    io.Closer
	pending error
}

func (p *peekedBody) Read(b []byte) (int, error) {
	// 缓冲已耗尽且已有暂存错误: 先回放该错误, 绝不再碰底层。Peek 已把 bufio 的
	// readErr 一次性消费掉, 此时 br.Read 会重新读底层——底层若停住会无限延迟
	// 这个已知错误, 若返回别的错误会覆盖首个关键错误(F9)。
	if p.pending != nil && p.br.Buffered() == 0 {
		err := p.pending
		p.pending = nil
		return 0, err
	}
	n, err := p.br.Read(b)
	if n > 0 {
		if err != nil && !errors.Is(err, io.EOF) && p.pending == nil {
			p.pending = err
		}
		return n, nil
	}
	if p.pending != nil && (err == nil || errors.Is(err, io.EOF)) {
		err = p.pending
		p.pending = nil
	}
	return n, err
}

func (p *peekedBody) Close() error { return p.orig.Close() }

// sseFramingPeekWindow is the identification window for the content-type
// fallback: enough bytes to see a physical SSE field line, small enough that
// the probe read stays a bounded prefix of the body.
const sseFramingPeekWindow = 512

// sseFieldLinePresent reports whether any physical line in the peek window
// starts with an SSE field name ("data:"/"event:"). Line-level matching cannot
// misfire on ordinary JSON: in a valid JSON encoding a physical line never
// starts with "data:" or "event:", so string values that merely contain those
// prefixes are not matched.
func sseFieldLinePresent(peek []byte) bool {
	for _, line := range bytes.Split(peek, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if bytes.HasPrefix(line, []byte("data:")) || bytes.HasPrefix(line, []byte("event:")) {
			return true
		}
	}
	return false
}

// responseBodyLooksLikeSSE is the fallback for upstreams that omit (or mislabel)
// the Content-Type header while still sending SSE: it peeks the first bytes of
// the (already unwrapped) body and reports whether the framing is SSE. The peek
// does not consume the body — peeked bytes are replayed via peekedBody, which
// also preserves any error the peek consumed.
//
// 识别读取被请求取消与首 token 超时预算覆盖(F8): Peek 会一直等满窗口或等错误,
// 一个只发了不足 512 字节前奏就停住的上游会把非流式识别无限挂住——那时聚合器
// 的首 token/间隔超时和心跳都还没启动。超时/取消时关闭真实 body 让停在探测读
// 里的协程返回(带缓冲通道保证它退出), 并返回错误, 上层按普通尝试失败处理,
// 保留既有换渠道行为。探测协程受控: 生命周期就是这一次识别, 识别结束(含超时
// 关闭)即结束, 不留后台读取。
//
// 一旦足以判定帧型(见到 data:/event: 行, 或窗口读满/EOF/错误)立即返回, 不无
// 条件等满窗口。返回的探测耗时由调用方计入聚合器的首 token 预算, 不重新赠送
// 完整窗口。单读者所有权: 探测读完成后才把同一个 body 交给聚合器读。
func (ra *relayAttempt) responseBodyLooksLikeSSE(ctx context.Context, response *http.Response) (bool, time.Duration, error) {
	if response == nil || response.Body == nil {
		return false, 0, nil
	}
	br := bufio.NewReaderSize(response.Body, sseFramingPeekWindow)
	orig := response.Body
	pb := &peekedBody{br: br, orig: orig}
	response.Body = pb

	type probeResult struct {
		peek []byte
		err  error
	}
	resCh := make(chan probeResult, 1)
	safe.SafeGo("relay-sse-framing-probe", func() {
		// 增量 Peek: 每多到一个字节就检查一次是否已可判定, 不等满窗口。
		var (
			peek []byte
			err  error
		)
		for want := 1; want <= sseFramingPeekWindow; want++ {
			peek, err = br.Peek(want)
			if err != nil || sseFieldLinePresent(peek) {
				break
			}
		}
		resCh <- probeResult{peek: peek, err: err}
	})

	startedAt := time.Now()
	var timerC <-chan time.Time
	if ra.firstTokenTimeOutSec > 0 {
		timer := time.NewTimer(time.Duration(ra.firstTokenTimeOutSec) * time.Second)
		defer timer.Stop()
		timerC = timer.C
	}

	select {
	case res := <-resCh:
		elapsed := time.Since(startedAt)
		// 探测协程已结束, 此时回填 pending 才不会与探测读并发。
		pb.pending = res.err
		return sseFieldLinePresent(res.peek), elapsed, nil
	case <-timerC:
		// 关闭真实 body: 停在 Peek 里的探测读随之返回, 协程经带缓冲通道退出。
		// 之后无人再读该 body(本次尝试失败), 不存在并发读。
		_ = orig.Close()
		return false, time.Since(startedAt), fmt.Errorf(
			"upstream response framing not decidable within first-token timeout (%ds)", ra.firstTokenTimeOutSec)
	case <-ctx.Done():
		_ = orig.Close()
		return false, time.Since(startedAt), fmt.Errorf(
			"upstream response identification canceled: %w", ctx.Err())
	}
}

// handleResponse 处理非流式响应
func (ra *relayAttempt) handleResponse(ctx context.Context, response *http.Response, outAdapter model.Outbound) error {
	limitUpstreamResponseBody(response)
	// SSE self-heal: some upstreams (web-chat bridges) ignore "stream": false and
	// reply with an SSE body. Detect it by content-type first and hand the untouched
	// response to the forced-stream aggregation path (it unwraps encoding and guards
	// non-2xx JSON bodies itself), so the non-stream client still gets one complete
	// JSON response. Mirrors the stream path's non-SSE content-type guard in the
	// opposite direction.
	if responseIsEventStream(response) {
		return ra.handleStreamResponseAsNonStream(ctx, response, outAdapter, 0)
	}
	// Decompress any upstream Content-Encoding before the outbound parses the body. The
	// claude/anthropic outbound advertises gzip,deflate,br,zstd and the shared transport has
	// DisableCompression=true, so Go does not auto-decompress here (unlike the streaming path,
	// which already calls unwrapResponseEncoding). No-ops on identity/absent/unknown encoding.
	if err := unwrapResponseEncoding(response); err != nil {
		return fmt.Errorf("failed to unwrap upstream response encoding: %w", err)
	}
	// SSE self-heal fallback: a few upstreams omit (or mislabel) the Content-Type
	// header while still sending SSE; line-level framing detection on the peeked
	// body prefix. Align the header with the body's real framing so the aggregation
	// path's content-type guard accepts the handoff. The identification read is
	// bounded by the request context and the first-token budget (F8): a stalled
	// upstream fails this attempt (existing channel-switch behavior) instead of
	// hanging the non-stream client.
	isSSE, identifyElapsed, identifyErr := ra.responseBodyLooksLikeSSE(ctx, response)
	if identifyErr != nil {
		return identifyErr
	}
	if isSSE {
		response.Header.Set("Content-Type", "text/event-stream")
		return ra.handleStreamResponseAsNonStream(ctx, response, outAdapter, identifyElapsed)
	}
	internalResponse, err := outAdapter.TransformResponse(ctx, response)
	if err != nil {
		log.Warnf("failed to transform response: %v", err)
		return fmt.Errorf("failed to transform outbound response: %w", err)
	}
	ra.captureUpstreamDeclaredModel(internalResponse)
	// When model_mapping remapped the request model, restore the client-visible name
	// so the upstream name is never leaked to the client.
	if ra.modelMapped && internalResponse != nil {
		internalResponse.Model = ra.requestModel
	}

	inResponse, err := ra.inAdapter.TransformResponse(ctx, internalResponse)
	if err != nil {
		log.Warnf("failed to transform response: %v", err)
		return fmt.Errorf("failed to transform inbound response: %w", err)
	}
	// 凭据脱敏: 调用端格式 body 在写客户端前还原占位符。失败=显式错误(尚未写客户端,
	// 可 failover), 绝不透传占位符。
	if ra.redactSession != nil && ra.redactApplied {
		restored, rerr := ra.redactSession.RestoreJSONBody(inResponse)
		if rerr != nil {
			return fmt.Errorf("redact: restored inbound response failed: %w", rerr)
		}
		inResponse = restored
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

	// 凭据脱敏 (#6): 先把从 adapter 取回的 internal response 的受支持文字还原为原文，再交给
	// metrics 与存档——这样 metrics 与本次保存的助手历史看到的内容与客户端实际收到的还原
	// 原文一致，不会把占位符存进 transcript（后续独立会话无该 token 映射会形成还原缺口）。
	internalResponse = ra.restoreInternalResponseTexts(internalResponse)
	ra.metrics.SetInternalResponse(internalResponse, ra.internalRequest.Model)
	// 自报型号与 ActualModel 同点提交: 都归属「产出这次内部响应的尝试」。
	// 失败尝试的声明留在尝试级不落请求级, 赢家不自报就是未知(F10)。
	ra.metrics.CommitUpstreamDeclaredModel(ra.upstreamDeclaredModel)
	ra.recordResponsesSessionFromInbound(internalResponse)
}

// captureUpstreamDeclaredModel 记录上游自报的模型名(审计用)。
//
// 必须在 model_mapping 的"客户端可见名还原"之前调用: 那一步会把 internalResponse.Model
// 覆盖成 ra.requestModel, 覆盖之后就再也拿不到上游的真名了。纯观测, 不改任何转发字节。
//
// 写的是尝试级字段(首个非空胜出, 流式分片重复回显同一型号): 请求级 metrics 只在
// collectResponse 由「产出最终响应的这次尝试」提交(见 CommitUpstreamDeclaredModel),
// 失败尝试的声明不会挤占后来成功渠道的日志归属(F10)。
func (ra *relayAttempt) captureUpstreamDeclaredModel(resp *model.InternalLLMResponse) {
	if ra == nil || resp == nil {
		return
	}
	name := strings.TrimSpace(resp.UpstreamDeclaredModel)
	if name == "" {
		// Outbounds that declare through Model (OpenAI/Anthropic) keep working;
		// converters that write the audit-only field (Gemini) leave Model alone,
		// so the client-visible body/SSE and the model_mapping rewrite on Model
		// stay exactly as before.
		name = strings.TrimSpace(resp.Model)
	}
	if name == "" || ra.upstreamDeclaredModel != "" {
		return
	}
	ra.upstreamDeclaredModel = name
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
