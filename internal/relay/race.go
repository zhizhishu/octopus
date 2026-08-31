package relay

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/relay/balancer"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	"github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/utils/safe"
)

// racerResult captures the race outcome from a single key worker goroutine.
type racerResult struct {
	keyIndex      int
	key           dbmodel.ChannelKey
	response      *http.Response
	outAdapter    model.Outbound
	preparedReq   *model.InternalLLMRequest
	modelMapped   bool
	statusCode    int
	err           error
	upstreamPaths []string
	startTime     time.Time
	duration      time.Duration
	canceled      bool // true if active loser canceled due to another winner
	finishAttempt func()
	racerCancel   context.CancelFunc
}

// canChannelRace reports whether the channel and request qualify for multi-key racing.
func canChannelRace(req *relayRequest, channel *dbmodel.Channel, availableKeys []dbmodel.ChannelKey) bool {
	if req == nil || channel == nil {
		return false
	}
	if !channel.RaceMode {
		return false
	}
	if req.interventionKeyID > 0 {
		return false
	}
	if len(availableKeys) < 2 {
		return false
	}
	if req.internalRequest == nil {
		return false
	}
	if req.internalRequest.IsEmbeddingRequest() {
		return false
	}
	if req.internalRequest.IsImageGenerationRequest() {
		return false
	}
	if isVideoRequest(req) {
		return false
	}
	return true
}

// isVideoRequest checks if the request is for video generation or manipulation.
func isVideoRequest(req *relayRequest) bool {
	if req == nil || req.c == nil || req.c.Request == nil {
		return false
	}
	path := strings.ToLower(req.c.Request.URL.Path)
	return strings.Contains(path, "/videos")
}

// clampRaceConcurrency clamps channel.RaceKeyConcurrency into [2, 5].
func clampRaceConcurrency(n int) int {
	if n < 2 {
		return 2
	}
	if n > 5 {
		return 5
	}
	return n
}

// prepareRacerAttempt creates an isolated clone of relayRequest and internalRequest,
// running all pre-flight transformations (mapping, overrides, fingerprints, shape, history bridge).
// Note: applyTransformOptionsWithInboundSetter(false) is called to avoid concurrent mutations on shared inAdapter.
func prepareRacerAttempt(
	parentCtx context.Context,
	req *relayRequest,
	channel *dbmodel.Channel,
	key dbmodel.ChannelKey,
	outAdapter model.Outbound,
) (*relayAttempt, *http.Request, []string, error) {
	// Create an isolated internalRequest clone from parent's prepared template
	clonedReq := cloneInternalRequestForRetry(req.internalRequest)

	// Shallow-copy relayRequest so racer-local internalRequest or other mutations
	// never touch the shared parent relayRequest.
	isolatedReq := *req
	isolatedReq.internalRequest = clonedReq

	ra := &relayAttempt{
		relayRequest: &isolatedReq,
		outAdapter:   outAdapter,
		channel:      channel,
		usedKey:      key,
	}
	ra.internalRequest = clonedReq
	ra.responsesDowngradedToChat = false

	if err := ra.applyTransformOptionsWithInboundSetter(false); err != nil {
		return nil, nil, nil, err
	}

	adapter := ra.outAdapter
	upstreamPaths := make([]string, 0, 2)

	originalStream := ra.internalRequest.Stream
	originalPreviousResponseID := ra.internalRequest.PreviousResponseID
	originalMessages := append([]model.Message(nil), ra.internalRequest.Messages...)
	originalResponsesInputRaw := cloneRawJSONMessage(ra.internalRequest.ResponsesInputRaw)

	ra.prepareResponsesSessionCursor(adapter)
	ra.prepareResponsesEncryptedContent(adapter)

	forceResponsesStreamUpstream := ra.shouldForceOpenAIResponsesStreamUpstream(adapter)
	forceAnthropicStreamUpstream := ra.shouldForceAnthropicStreamUpstream()
	if forceResponsesStreamUpstream || forceAnthropicStreamUpstream {
		stream := true
		ra.internalRequest.Stream = &stream
	}

	if ra.chatHistoryRebuilt {
		ra.internalRequest.Messages = normalizeChatToolCallPairing(ra.internalRequest.Messages)
	}

	outboundRequest, err := adapter.TransformRequest(
		parentCtx,
		ra.internalRequest,
		ra.outboundBaseURL(),
		ra.usedKey.ChannelKey,
	)
	ra.internalRequest.Stream = originalStream
	ra.internalRequest.PreviousResponseID = originalPreviousResponseID
	ra.internalRequest.Messages = originalMessages
	ra.internalRequest.ResponsesInputRaw = originalResponsesInputRaw
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to create request: %w", err)
	}
	if outboundRequest != nil && outboundRequest.URL != nil {
		upstreamPath := outboundRequest.URL.EscapedPath()
		if upstreamPath == "" {
			upstreamPath = outboundRequest.URL.Path
		}
		if upstreamPath != "" {
			upstreamPaths = append(upstreamPaths, upstreamPath)
		}
	}

	if err := ApplyParamOverride(outboundRequest, ra.channel.ParamOverride); err != nil {
		return nil, nil, nil, err
	}

	ra.copyHeaders(outboundRequest)
	return ra, outboundRequest, upstreamPaths, nil
}

// isRacerFastIncompatible200 checks if an upstream 200 response has a header
// that is obviously incompatible with what was requested (e.g. non-SSE for stream).
func isRacerFastIncompatible200(req *model.InternalLLMRequest, forceStream bool, resp *http.Response) bool {
	if resp == nil || resp.StatusCode != http.StatusOK {
		return false
	}
	wantsStream := (req != nil && req.Stream != nil && *req.Stream) || forceStream
	if wantsStream {
		ct := resp.Header.Get("Content-Type")
		if ct != "" && !strings.Contains(strings.ToLower(ct), "text/event-stream") {
			return true
		}
	}
	return false
}

// runChannelRace executes concurrent/hedged requests across up to N available keys.
func runChannelRace(
	parentReq *relayRequest,
	channel *dbmodel.Channel,
	keys []dbmodel.ChannelKey,
	outAdapter model.Outbound,
	requestEndpoint string,
	capabilityKey string,
	firstTokenTimeoutSec int,
) (attemptResult, []dbmodel.ChannelKey) {
	concurrency := clampRaceConcurrency(channel.RaceKeyConcurrency)
	if concurrency > len(keys) {
		concurrency = len(keys)
	}
	racerKeys := keys[:concurrency]
	remainingKeys := keys[concurrency:]

	delayMs := channel.RaceDelayMs
	if delayMs < 0 {
		delayMs = 0
	}
	delay := time.Duration(delayMs) * time.Millisecond

	parentCtx := parentReq.c.Request.Context()
	raceCtx, cancelAll := context.WithCancel(parentCtx)
	defer cancelAll()

	resultsChan := make(chan racerResult, concurrency)
	var cancelsMu sync.Mutex
	cancels := make([]context.CancelFunc, concurrency)

	var winnerFound atomicBool
	var wg sync.WaitGroup
	runtimeModel := ""
	if channel != nil && len(channel.ModelMapping) > 0 && parentReq.internalRequest != nil {
		if mapped, ok := channel.ModelMapping[parentReq.internalRequest.Model]; ok && mapped != "" {
			runtimeModel = mapped
		}
	}
	if runtimeModel == "" && parentReq.internalRequest != nil {
		runtimeModel = parentReq.internalRequest.Model
	}

	for i, key := range racerKeys {
		idx := i
		racerKey := key
		wg.Add(1)

		safe.SafeGo(fmt.Sprintf("channel-racer-%d-%d", channel.ID, racerKey.ID), func() {
			defer wg.Done()

			if idx > 0 && delay > 0 {
				timer := time.NewTimer(time.Duration(idx) * delay)
				select {
				case <-timer.C:
				case <-raceCtx.Done():
					timer.Stop()
					return
				}
			}

			if winnerFound.get() || raceCtx.Err() != nil {
				return
			}

			// Each racer uses a direct child of parentCtx (not raceCtx), so cancelling
			// raceCtx or other losers never cancels the winner's outbound request or body.
			racerCtx, racerCancel := context.WithCancel(parentCtx)
			cancelsMu.Lock()
			cancels[idx] = racerCancel
			cancelsMu.Unlock()

			startTime := time.Now()
			finishAttempt := balancer.BeginRuntimeAttempt(channel.ID, racerKey.ID, runtimeModel)

			var (
				resp      *http.Response
				sendErr   error
				isWinner  bool
				cleanedUp bool
			)

			// Ensure loser runtime reservation, context cancel, and unread body are cleaned up
			// safely in all exit paths (prep fail, send fail, fast 200 incompatible, 2xx loser, non-2xx).
			defer func() {
				if !isWinner && !cleanedUp {
					if resp != nil && resp.Body != nil {
						_ = resp.Body.Close()
					}
					if finishAttempt != nil {
						finishAttempt()
					}
					if racerCancel != nil {
						racerCancel()
					}
				}
			}()

			ra, outboundReq, upstreamPaths, prepErr := prepareRacerAttempt(
				racerCtx,
				parentReq,
				channel,
				racerKey,
				outAdapter,
			)
			if prepErr != nil {
				cleanedUp = true
				finishAttempt()
				racerCancel()
				resultsChan <- racerResult{
					keyIndex:      idx,
					key:           racerKey,
					statusCode:    http.StatusBadRequest,
					err:           prepErr,
					upstreamPaths: upstreamPaths,
					startTime:     startTime,
					duration:      time.Since(startTime),
				}
				return
			}

			resp, sendErr = ra.sendRequest(outboundReq)
			duration := time.Since(startTime)

			if sendErr != nil {
				cleanedUp = true
				finishAttempt()
				racerCancel()
				isCanceled := racerCtx.Err() != nil || raceCtx.Err() != nil
				resultsChan <- racerResult{
					keyIndex:      idx,
					key:           racerKey,
					statusCode:    0,
					err:           sendErr,
					upstreamPaths: upstreamPaths,
					startTime:     startTime,
					duration:      duration,
					canceled:      isCanceled,
				}
				return
			}

			// Check response status
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				forceStream := ra.shouldForceOpenAIResponsesStreamUpstream(ra.outAdapter) || ra.shouldForceAnthropicStreamUpstream()
				if isRacerFastIncompatible200(ra.internalRequest, forceStream, resp) {
					body, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
					_ = resp.Body.Close()
					cleanedUp = true
					finishAttempt()
					racerCancel()
					resultsChan <- racerResult{
						keyIndex:      idx,
						key:           racerKey,
						statusCode:    resp.StatusCode,
						err:           fmt.Errorf("upstream returned non-SSE content-type %q for stream request: %s", resp.Header.Get("Content-Type"), string(body)),
						upstreamPaths: upstreamPaths,
						startTime:     startTime,
						duration:      duration,
					}
					return
				}

				// First 2xx winner claims victory!
				if winnerFound.compareAndSwap(false, true) {
					isWinner = true
					// Cancel hedge delay timer
					cancelAll()

					// Cancel all other racers (losers) immediately, leaving winner untouched
					cancelsMu.Lock()
					for kIdx, cFunc := range cancels {
						if kIdx != idx && cFunc != nil {
							cFunc()
						}
					}
					cancelsMu.Unlock()

					// Winner reservation ownership and racerCancel are passed to main goroutine
					resultsChan <- racerResult{
						keyIndex:      idx,
						key:           racerKey,
						response:      resp,
						outAdapter:    ra.outAdapter,
						preparedReq:   ra.internalRequest,
						modelMapped:   ra.modelMapped,
						statusCode:    resp.StatusCode,
						upstreamPaths: upstreamPaths,
						startTime:     startTime,
						duration:      duration,
						finishAttempt: finishAttempt,
						racerCancel:   racerCancel,
					}
					return
				}

				// Another racer already won, close this body and record as raced out
				_ = resp.Body.Close()
				cleanedUp = true
				finishAttempt()
				racerCancel()
				resultsChan <- racerResult{
					keyIndex:      idx,
					key:           racerKey,
					statusCode:    resp.StatusCode,
					err:           context.Canceled,
					upstreamPaths: upstreamPaths,
					startTime:     startTime,
					duration:      duration,
					canceled:      true,
				}
				return
			}

			// Non-2xx response: read body up to limit
			body, _ := io.ReadAll(io.LimitReader(resp.Body, upstreamErrorBodyLimit))
			_ = resp.Body.Close()
			cleanedUp = true
			finishAttempt()
			racerCancel()
			upErr := newUpstreamError(resp.StatusCode, body)
			if d, ok := retryAfterFromHeader(resp.Header); ok {
				upErr.retryAfter = d
				upErr.hasRetryAfter = true
			}
			resultsChan <- racerResult{
				keyIndex:      idx,
				key:           racerKey,
				statusCode:    resp.StatusCode,
				err:           upErr,
				upstreamPaths: upstreamPaths,
				startTime:     startTime,
				duration:      duration,
			}
		})
	}

	// Drain goroutine to ensure channel close after wg
	go func() {
		wg.Wait()
		close(resultsChan)
	}()

	var winner *racerResult
	var loserResults []racerResult
	var lastErr error
	var bestFailStatus int

	// Process results as they arrive. If a winner arrives, immediately begin response handling
	// rather than blocking indefinitely on slow or non-responsive losers.
	for res := range resultsChan {
		if res.response != nil && winner == nil {
			w := res
			winner = &w
			break
		} else {
			loserResults = append(loserResults, res)
		}
	}

	if winner != nil {
		// Winner found! Losers have already been canceled.
		// Explicitly transfer prepared request ownership to parent relayRequest
		// before building winnerRA, avoiding relying on promoted struct field side effects.
		parentReq.internalRequest = winner.preparedReq
		winnerRA := &relayAttempt{
			relayRequest:         parentReq,
			outAdapter:           winner.outAdapter,
			channel:              channel,
			usedKey:              winner.key,
			firstTokenTimeOutSec: firstTokenTimeoutSec,
			modelMapped:          winner.modelMapped,
		}
		winnerRA.internalRequest = winner.preparedReq

		span := parentReq.iter.StartAttempt(channel.ID, winner.key.ID, channel.Name)
		span.SetRouteScope(requestEndpoint, capabilityKey)
		recordAttemptProxy(span, channel)
		if len(winner.upstreamPaths) > 0 {
			span.SetUpstreamPath(strings.Join(winner.upstreamPaths, " -> "))
		}

		// Handle the response using existing handler immediately so stream has lowest latency
		handleErr := winnerRA.executeWinnerResponse(winner.response, winner.outAdapter)
		if winner.finishAttempt != nil {
			winner.finishAttempt()
		}
		if winner.racerCancel != nil {
			winner.racerCancel()
		}

		usedAt := time.Now().Unix()

		// Drain remaining loser results with a bounded timeout (250ms) to record loser telemetry
		drainLosers(resultsChan, &loserResults, 250*time.Millisecond)

		// Record loser spans sequentially on main goroutine
		recordLoserResults(parentReq, channel, requestEndpoint, capabilityKey, runtimeModel, loserResults, &bestFailStatus)

		if handleErr == nil {
			winnerRA.collectResponse()
			winnerRA.metrics.ChannelKeyRemark = winnerRA.usedKey.Remark
			costDelta := winnerRA.metrics.Stats.InputCost + winnerRA.metrics.Stats.OutputCost
			op.ChannelKeyRecordUse(winnerRA.usedKey, winner.statusCode, usedAt, costDelta)

			span.End(dbmodel.AttemptSuccess, winner.statusCode, "")
			balancer.RecordRuntimeSuccess(channel.ID, winnerRA.usedKey.ID, runtimeModel, balancer.AttemptRuntimeMetrics{
				Duration:     span.Duration(),
				FirstToken:   firstTokenDurationSince(winnerRA.metrics.FirstTokenTime, span.StartedAt()),
				OutputTokens: winnerRA.metrics.Stats.OutputToken,
				Stream:       internalRequestPrefersStream(winnerRA.internalRequest),
			})
			op.StatsChannelUpdate(channel.ID, dbmodel.StatsMetrics{
				WaitTime:       span.Duration().Milliseconds(),
				RequestSuccess: 1,
			})
			balancer.RecordSuccessScoped(channel.ID, winnerRA.usedKey.ID, runtimeModel, requestEndpoint, capabilityKey)
			if winnerRA.stickyEnabled {
				balancer.SetStickyWithSessionKey(winnerRA.apiKeyID, winnerRA.requestModel, winnerRA.clientSessionKey, channel.ID, winnerRA.usedKey.ID)
			}
			winnerRA.metrics.ParamOverride = paramOverrideValue(channel.ParamOverride)
			return attemptResult{Success: true, StatusCode: winner.statusCode}, remainingKeys
		}

		// Handling failed after winning response header
		recStatus := attemptStatusCode(winner.statusCode, handleErr)
		op.ChannelKeyRecordUse(winnerRA.usedKey, recStatus, usedAt, 0)
		span.End(dbmodel.AttemptFailed, recStatus, auditErrorMessage(handleErr))

		breakerCounted := shouldRecordBreakerFailure(recStatus, handleErr)
		if breakerCounted && !channel.DisableCircuitBreaker {
			retryAfter, _ := retryAfterFromError(handleErr)
			balancer.RecordRuntimeFailure(channel.ID, winnerRA.usedKey.ID, runtimeModel, recStatus, span.Duration(), retryAfter)
			balancer.RecordFailureWithStatusScoped(channel.ID, winnerRA.usedKey.ID, runtimeModel, requestEndpoint, capabilityKey, recStatus)
		}
		op.StatsChannelUpdate(channel.ID, dbmodel.StatsMetrics{
			WaitTime:      span.Duration().Milliseconds(),
			RequestFailed: 1,
		})
		winnerRA.metrics.ParamOverride = paramOverrideValue(channel.ParamOverride)

		committed := winnerRA.wroteMeaningfulDownstream
		if committed {
			switch winnerRA.inboundType {
			case inbound.InboundTypeOpenAIResponse:
				if message := responsesStreamFailureMessage(handleErr); message != "" {
					writeResponsesFailedSSE(winnerRA.c, winnerRA.requestModel, "upstream_error", message)
				}
			case inbound.InboundTypeAnthropic:
				if message := anthropicStreamFailureMessage(handleErr); message != "" {
					writeAnthropicErrorSSE(winnerRA.c, "api_error", message)
				}
			}
			winnerRA.collectResponse()
		}

		return attemptResult{
			Success:    false,
			Written:    committed,
			Err:        fmt.Errorf("channel %s failed: %w", channel.Name, handleErr),
			StatusCode: recStatus,
			Retryable:  !committed && isRetryableUpstreamStreamError(handleErr),
			Fatal:      isContextWindowError(handleErr),
		}, remainingKeys
	}

	// No winner found from initial receive, wait for resultsChan to close and collect all losers
	for res := range resultsChan {
		loserResults = append(loserResults, res)
	}

	lastErr = recordLoserResults(parentReq, channel, requestEndpoint, capabilityKey, runtimeModel, loserResults, &bestFailStatus)

	// All raced keys failed
	if lastErr == nil {
		lastErr = fmt.Errorf("channel %s all %d raced keys failed", channel.Name, len(racerKeys))
	}
	finalStatus := http.StatusBadGateway
	if bestFailStatus > 0 {
		finalStatus = bestFailStatus
	}
	return attemptResult{
		Success:    false,
		Written:    false,
		Err:        fmt.Errorf("channel %s failed: %w", channel.Name, lastErr),
		StatusCode: finalStatus,
		Retryable:  false,
		Fatal:      isContextWindowError(lastErr),
	}, remainingKeys
}

func drainLosers(resultsChan <-chan racerResult, loserResults *[]racerResult, timeout time.Duration) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case res, ok := <-resultsChan:
			if !ok {
				return
			}
			if res.response != nil {
				_ = res.response.Body.Close()
			}
			if res.finishAttempt != nil {
				res.finishAttempt()
			}
			if res.racerCancel != nil {
				res.racerCancel()
			}
			*loserResults = append(*loserResults, res)
		case <-timer.C:
			return
		}
	}
}

func recordLoserResults(
	parentReq *relayRequest,
	channel *dbmodel.Channel,
	requestEndpoint string,
	capabilityKey string,
	runtimeModel string,
	results []racerResult,
	bestFailStatus *int,
) error {
	var lastErr error
	for _, res := range results {
		span := parentReq.iter.StartAttempt(channel.ID, res.key.ID, channel.Name)
		span.SetRouteScope(requestEndpoint, capabilityKey)
		recordAttemptProxy(span, channel)
		if len(res.upstreamPaths) > 0 {
			span.SetUpstreamPath(strings.Join(res.upstreamPaths, " -> "))
		}

		usedAt := time.Now().Unix()

		if res.canceled {
			span.End(dbmodel.AttemptFailed, res.statusCode, "raced_out")
			continue
		}

		// Real failure for loser/failed racer
		recStatus := attemptStatusCode(res.statusCode, res.err)
		if bestFailStatus != nil {
			// Prioritize 429 over other 4xx/5xx for retry/next-key decisions
			if recStatus == http.StatusTooManyRequests {
				*bestFailStatus = http.StatusTooManyRequests
			} else if *bestFailStatus != http.StatusTooManyRequests && recStatus > 0 {
				*bestFailStatus = recStatus
			}
		}
		op.ChannelKeyRecordUse(res.key, recStatus, usedAt, 0)
		span.End(dbmodel.AttemptFailed, recStatus, auditErrorMessage(res.err))

		breakerCounted := shouldRecordBreakerFailure(recStatus, res.err)
		if breakerCounted && !channel.DisableCircuitBreaker {
			retryAfter, _ := retryAfterFromError(res.err)
			balancer.RecordRuntimeFailure(channel.ID, res.key.ID, runtimeModel, recStatus, res.duration, retryAfter)
			balancer.RecordFailureWithStatusScoped(channel.ID, res.key.ID, runtimeModel, requestEndpoint, capabilityKey, recStatus)
		}
		op.StatsChannelUpdate(channel.ID, dbmodel.StatsMetrics{
			WaitTime:      res.duration.Milliseconds(),
			RequestFailed: 1,
		})
		if res.err != nil {
			lastErr = res.err
		}
	}
	return lastErr
}

func (ra *relayAttempt) executeWinnerResponse(response *http.Response, outAdapter model.Outbound) error {
	defer response.Body.Close()
	ctx := ra.c.Request.Context()

	forceNonStreamUpstream := ra.shouldPreferAnthropicNonStreamUpstream()
	forceResponsesStreamUpstream := ra.shouldForceOpenAIResponsesStreamUpstream(outAdapter) && !forceNonStreamUpstream
	forceAnthropicStreamUpstream := ra.shouldForceAnthropicStreamUpstream() && !forceNonStreamUpstream

	if forceNonStreamUpstream {
		return ra.handleNonStreamResponseAsStream(ctx, response, outAdapter)
	}
	originalStream := ra.internalRequest.Stream
	if (forceResponsesStreamUpstream || forceAnthropicStreamUpstream) && (originalStream == nil || !*originalStream) {
		return ra.handleStreamResponseAsNonStream(ctx, response, outAdapter)
	}
	if forceResponsesStreamUpstream || forceAnthropicStreamUpstream || (ra.internalRequest.Stream != nil && *ra.internalRequest.Stream) {
		return ra.handleStreamResponse(ctx, response, outAdapter)
	}
	return ra.handleResponse(ctx, response, outAdapter)
}

type atomicBool struct {
	val int32
}

func (b *atomicBool) get() bool {
	return atomic.LoadInt32(&b.val) != 0
}

func (b *atomicBool) compareAndSwap(oldVal, newVal bool) bool {
	var o, n int32
	if oldVal {
		o = 1
	}
	if newVal {
		n = 1
	}
	return atomic.CompareAndSwapInt32(&b.val, o, n)
}
