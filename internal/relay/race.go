package relay

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/redact"
	"github.com/zhizhishu/octopus/internal/relay/balancer"
	"github.com/zhizhishu/octopus/internal/transformer/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
	"github.com/zhizhishu/octopus/internal/utils/safe"
)

// raceWaveGlobalCap is the maximum contenders launched in one race wave.
const raceWaveGlobalCap = 5

// raceContender is one (channel, key) participant in a race wave. Its internalRequest is
// the flat per-channel template (Model set to the item's name, prompt overrides already
// applied on the main goroutine); each racer clones it so racer-local mutation never
// touches the shared template.
type raceContender struct {
	channel         *dbmodel.Channel
	key             dbmodel.ChannelKey
	internalRequest *model.InternalLLMRequest
	promptSnapshot  promptOverrideSnapshot
	delay           time.Duration
	runtimeModel    string
	capabilityKey   string
	// promptOverridesApplied marks a contender whose template went through the
	// cross-channel prompt pipeline (applyPromptOverrides). The single-channel race path
	// leaves it false so the caller's own prompt snapshot is never clobbered.
	promptOverridesApplied bool
}

// raceWinner carries the real winner facts back to the caller (relay) so telemetry,
// sticky, and metrics can be attributed to the channel that actually won.
type raceWinner struct {
	channel                *dbmodel.Channel
	key                    dbmodel.ChannelKey
	runtimeModel           string
	capabilityKey          string
	promptSnapshot         promptOverrideSnapshot
	promptOverridesApplied bool
}

// racerResult captures the race outcome from a single contender goroutine.
type racerResult struct {
	keyIndex       int
	channel        *dbmodel.Channel
	key            dbmodel.ChannelKey
	response       *http.Response
	outAdapter     model.Outbound
	preparedReq    *model.InternalLLMRequest
	promptSnapshot promptOverrideSnapshot
	runtimeModel   string
	capabilityKey  string
	modelMapped    bool
	statusCode     int
	err            error
	upstreamPaths  []string
	startTime      time.Time
	duration       time.Duration
	canceled       bool // true if active loser canceled due to another winner
	finishAttempt  func()
	racerCancel    context.CancelFunc
	// preparedAttempt is the winner's OWN relayAttempt. The winner branch reuses this exact
	// object (swapping only relayRequest -> parentReq) instead of building a fresh winnerRA,
	// so the total-timeout clock the racer armed keeps pointing at the same object and the
	// shared totalClock pointer is preserved (a fresh object would re-arm and mis-fire).
	preparedAttempt        *relayAttempt
	promptOverridesApplied bool
	// 凭据脱敏: 赢家 racer 的 attempt 级 session + 是否真改了 body。赢家响应由挂在
	// parentReq 上的 winnerRA 处理, session 挂在 racer 自己的 relayAttempt 上 — 不带到
	// winnerRA 的话还原全跳过, 占位符直接漏给客户端 (审计 2026-09-25 抓出的 race 泄漏)。
	redactSession *redact.Session
	redactApplied bool
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

// raceDelay returns the per-key hedge delay for a channel (RaceDelayMs, never negative).
func raceDelay(channel *dbmodel.Channel) time.Duration {
	if channel == nil {
		return 0
	}
	ms := channel.RaceDelayMs
	if ms < 0 {
		ms = 0
	}
	return time.Duration(ms) * time.Millisecond
}

// runtimeModelForChannel resolves the upstream model name this channel would use for the
// given template (channel.ModelMapping applied, else the request's model).
func runtimeModelForChannel(channel *dbmodel.Channel, req *model.InternalLLMRequest) string {
	if channel != nil && len(channel.ModelMapping) > 0 && req != nil {
		if mapped, ok := channel.ModelMapping[req.Model]; ok && mapped != "" {
			return mapped
		}
	}
	if req != nil {
		return req.Model
	}
	return ""
}

// raceContentBudget resolves the pre-content wait budget for awaitRaceContent: the
// group's resolved first-token timeout when > 0, else the shipped default
// (DefaultFirstTokenTimeOutSeconds = 120s). Never unbounded — a header-only/empty 200
// that never becomes real content must lose the race, not win it.
func raceContentBudget(firstTokenTimeout int) time.Duration {
	if firstTokenTimeout > 0 {
		return time.Duration(firstTokenTimeout) * time.Second
	}
	if secs, err := strconv.Atoi(dbmodel.DefaultFirstTokenTimeOutSeconds); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	return 120 * time.Second
}

// copyRelayRequestForRacer builds an isolated racer-local relayRequest: every scalar and
// pointer field is copied explicitly (NEVER `*req`, because relayRequest embeds
// suppressPreContentKeepalive atomic.Bool — a struct copy trips `go vet` copylocks). The
// atomic value is copied with Load/Store so a racer keeps the parent's pre-content
// keepalive decision. The racer also gets its OWN RelayMetrics copy (the struct has no
// lock, so a plain value copy is safe) so concurrent prepare never mutates shared metrics.
func copyRelayRequestForRacer(parent *relayRequest, racerInternalRequest *model.InternalLLMRequest) *relayRequest {
	if parent == nil {
		return &relayRequest{internalRequest: racerInternalRequest}
	}
	racer := &relayRequest{
		c:                           parent.c,
		inboundType:                 parent.inboundType,
		inAdapter:                   parent.inAdapter,
		internalRequest:             racerInternalRequest,
		apiKeyID:                    parent.apiKeyID,
		userID:                      parent.userID,
		requestModel:                parent.requestModel,
		clientSessionKey:            parent.clientSessionKey,
		clientSessionSource:         parent.clientSessionSource,
		stickyEnabled:               parent.stickyEnabled,
		iter:                        parent.iter,
		interventionKeyID:           parent.interventionKeyID,
		requestState:                parent.requestState,
		totalTimeoutSec:             parent.totalTimeoutSec,
		totalBudget:                 parent.totalBudget,
		totalDeadline:               parent.totalDeadline,
		totalClock:                  parent.totalClock,
		wroteBusinessData:           parent.wroteBusinessData,
		wroteNonStreamJSONKeepalive: parent.wroteNonStreamJSONKeepalive,
		redactRequired:              parent.redactRequired,
		rescueClock:                 parent.rescueClock,
	}
	// Atomic field: explicit value copy, never a struct assignment.
	racer.suppressPreContentKeepalive.Store(parent.suppressPreContentKeepalive.Load())
	if parent.metrics != nil {
		metricsCopy := *parent.metrics
		racer.metrics = &metricsCopy
	}
	return racer
}

// prepareRacerAttempt keeps the historical 5-arg signature (template == req.internalRequest)
// so existing callers/tests are unaffected; it delegates to prepareRacerAttemptForTemplate.
func prepareRacerAttempt(
	parentCtx context.Context,
	req *relayRequest,
	channel *dbmodel.Channel,
	key dbmodel.ChannelKey,
	outAdapter model.Outbound,
) (*relayAttempt, *http.Request, []string, error) {
	return prepareRacerAttemptForTemplate(parentCtx, req, channel, key, req.internalRequest, outAdapter)
}

// prepareRacerAttemptForTemplate creates an isolated clone of relayRequest and internalRequest,
// running all pre-flight transformations (mapping, overrides, fingerprints, shape, history bridge).
// template is the per-channel flat request (its own clone is made here so racer-local
// mutation never touches the shared template). Note:
// applyTransformOptionsWithInboundSetter(false) is called to avoid concurrent mutations on shared inAdapter.
func prepareRacerAttemptForTemplate(
	parentCtx context.Context,
	req *relayRequest,
	channel *dbmodel.Channel,
	key dbmodel.ChannelKey,
	template *model.InternalLLMRequest,
	outAdapter model.Outbound,
) (*relayAttempt, *http.Request, []string, error) {
	// Create an isolated internalRequest clone from the per-channel template.
	clonedReq := cloneInternalRequestForRetry(template)

	// Isolated racer-local relayRequest: explicit field copy (never `*req`) so no lock is
	// copied and the parent stays untouched.
	isolatedReq := copyRelayRequestForRacer(req, clonedReq)

	ra := &relayAttempt{
		relayRequest: isolatedReq,
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
	// Capture the current top-level instructions verbatim (a *string) so the post-transform
	// rollback restores the pre-redaction form (nil stays nil; "" sentinel stays non-nil).
	originalResponsesInstructions := ra.internalRequest.ResponsesInstructions
	// Stop is replaced (not mutated) by applyInboundRedaction; capture the pre-redaction
	// pointer so the rollback can put it back. See forward() for the full rationale.
	originalStop := ra.internalRequest.Stop

	ra.prepareResponsesSessionCursor(adapter)
	ra.prepareResponsesEncryptedContent(adapter)

	// 凭据脱敏: 与 forward() 同一排序契约 — 在所有历史并入点之后、出站构建之前, 用当次渠道
	// flags 扫描客户端原样字节, 把脱敏后的 message 字段换回 internalRequest。每个 racer
	// 自己的 attempt 级 session, 失败即 prep 失败 (fail-closed)。
	if err := ra.applyInboundRedaction(); err != nil {
		ra.redactClose()
		return nil, nil, nil, err
	}

	forceNonStreamUpstream := ra.shouldPreferAnthropicNonStreamUpstream()
	forceResponsesStreamUpstream := ra.shouldForceOpenAIResponsesStreamUpstream(adapter) && !forceNonStreamUpstream
	forceAnthropicStreamUpstream := ra.shouldForceAnthropicStreamUpstream() && !forceNonStreamUpstream
	if forceResponsesStreamUpstream || forceAnthropicStreamUpstream {
		stream := true
		ra.internalRequest.Stream = &stream
	} else if forceNonStreamUpstream {
		stream := false
		ra.internalRequest.Stream = &stream
	}

	if ra.chatHistoryRebuilt {
		ra.internalRequest.Messages = normalizeChatToolCallPairing(ra.internalRequest.Messages)
	}

	previousVerbatim := ra.internalRequest.TransformOptions.VerbatimUpstreamURL
	if ra.channel != nil {
		ra.internalRequest.TransformOptions.VerbatimUpstreamURL = ra.channel.Type == outbound.OutboundTypeCustomOpenAIChat
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
	ra.internalRequest.ResponsesInstructions = originalResponsesInstructions
	ra.internalRequest.Stop = originalStop
	ra.internalRequest.TransformOptions.VerbatimUpstreamURL = previousVerbatim
	if err != nil {
		ra.redactClose()
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
		ra.redactClose()
		return nil, nil, nil, err
	}

	// 真实出站请求体快照（与主路径同一份开关与辅助函数；位置同样在
	// ApplyParamOverride 之后）。竞速路径每个 racer 各自快照，最终只有赢家
	// 的 metrics 会落库。
	if upstreamRequestBodyLogEnabled() {
		snapshot, snapErr := snapshotOutboundRequestBody(outboundRequest)
		if snapErr != nil {
			// Body 已被消费掉一部分——与主路径同口径，按本次尝试失败处理，
			// 让竞速换人，而不是把残缺请求体发出去。
			ra.redactClose()
			return nil, nil, nil, snapErr
		}
		if len(snapshot) > 0 {
			ra.metrics.SetUpstreamRequestBody(snapshot)
		}
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

// runRaceContenders races ONE wave of contenders (≤ raceWaveGlobalCap) that may span
// multiple channels. Every contender is launched concurrently with its own outbound
// adapter, metrics copy and per-racer lifetime; the first contender that yields real
// content wins and the rest are canceled. Wave composition / per-channel caps are the
// caller's job (runGroupRace); this core runs exactly one wave and never refills.
func runRaceContenders(
	parentReq *relayRequest,
	contenders []raceContender,
	endpoint string,
	firstTokenTimeout int,
) (attemptResult, *raceWinner) {
	if parentReq == nil || len(contenders) == 0 {
		return attemptResult{Success: false, Err: fmt.Errorf("no race contenders"), StatusCode: http.StatusBadGateway}, nil
	}

	parentCtx := parentReq.c.Request.Context()
	raceCtx, cancelAll := context.WithCancel(parentCtx)
	defer cancelAll()

	resultsChan := make(chan racerResult, len(contenders))
	var cancelsMu sync.Mutex
	cancels := make([]context.CancelFunc, len(contenders))

	var winnerFound atomicBool
	var wg sync.WaitGroup

	for i := range contenders {
		idx := i
		contender := contenders[i]
		if contender.channel == nil {
			continue
		}
		// Snapshot the parent on the MAIN goroutine, BEFORE the racer is launched. A racer
		// that passes the registration check can still be descheduled while the winner's
		// main goroutine writes parentReq.internalRequest / metrics.PromptSnapshot; copying
		// the parent inside the closure would then race those writes. Copying here is
		// single-threaded and cannot.
		racerParent := copyRelayRequestForRacer(parentReq, contender.internalRequest)
		wg.Add(1)

		safe.SafeGo(fmt.Sprintf("channel-racer-%d-%d", contender.channel.ID, contender.key.ID), func() {
			defer wg.Done()

			if contender.delay > 0 {
				timer := time.NewTimer(contender.delay)
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
			finishAttempt := balancer.BeginRuntimeAttempt(contender.channel.ID, contender.key.ID, contender.runtimeModel)

			// Registration window: a winner may have CAS'd and swept the cancels slice BEFORE
			// our entry landed, in which case our cancel is never invoked. Re-check and bail
			// out here (we have not contacted upstream) so a late racer can never run on after
			// the race is decided. Release our reservation; send NO result.
			if winnerFound.get() || raceCtx.Err() != nil {
				finishAttempt()
				racerCancel()
				return
			}

			// Independent outbound adapter per racer — never a shared instance (multi-key
			// race included). outbound.Get is a factory, so this is a fresh instance.
			racerOutbound := outbound.Get(contender.channel.Type)
			if racerOutbound == nil {
				finishAttempt()
				racerCancel()
				resultsChan <- racerResult{
					keyIndex:       idx,
					channel:        contender.channel,
					key:            contender.key,
					statusCode:     http.StatusBadRequest,
					err:            fmt.Errorf("unsupported channel type: %d", contender.channel.Type),
					startTime:      startTime,
					duration:       time.Since(startTime),
					runtimeModel:   contender.runtimeModel,
					capabilityKey:  contender.capabilityKey,
					promptSnapshot: contender.promptSnapshot,
				}
				return
			}

			var (
				resp      *http.Response
				sendErr   error
				isWinner  bool
				cleanedUp bool
				racerRA   *relayAttempt
			)

			// Ensure loser runtime reservation, context cancel, and unread body are cleaned
			// up safely in all exit paths (prep fail, send fail, fast 200 incompatible, probe
			// fail, 2xx loser, non-2xx). The winner's redaction session is carried through
			// racerResult into winnerRA and closed by the race branch; losers close their own
			// attempt-level session here. (Per-attempt sessions make racers naturally
			// isolated — there is no shared parent session for a loser to close.)
			defer func() {
				if !isWinner {
					if racerRA != nil {
						racerRA.redactClose()
					}
					if !cleanedUp {
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
				}
			}()

			ra, outboundReq, upstreamPaths, prepErr := prepareRacerAttemptForTemplate(
				racerCtx,
				racerParent,
				contender.channel,
				contender.key,
				contender.internalRequest,
				racerOutbound,
			)
			racerRA = ra
			if prepErr != nil {
				cleanedUp = true
				finishAttempt()
				racerCancel()
				resultsChan <- racerResult{
					keyIndex:       idx,
					channel:        contender.channel,
					key:            contender.key,
					statusCode:     http.StatusBadRequest,
					err:            prepErr,
					upstreamPaths:  upstreamPaths,
					startTime:      startTime,
					duration:       time.Since(startTime),
					runtimeModel:   contender.runtimeModel,
					capabilityKey:  contender.capabilityKey,
					promptSnapshot: contender.promptSnapshot,
				}
				return
			}

			// Bound this racer's upstream lifetime by the request's absolute ceiling (a no-op
			// when no budget is armed). cancelAll only cancels raceCtx, and the winner's
			// racerCancel is released after response handling, so the winner body stays readable.
			if outboundReq != nil {
				outboundReq = outboundReq.WithContext(ra.boundUpstreamLifetime(racerCtx))
			}
			resp, sendErr = ra.sendRequest(outboundReq)
			duration := time.Since(startTime)

			if sendErr != nil {
				// A real network/send error must be classified BEFORE we cancel our own
				// context, otherwise every failure looks like a "raced out" loser.
				if resp != nil && resp.Body != nil {
					_ = resp.Body.Close()
				}
				cleanedUp = true
				isCanceled := racerCtx.Err() != nil || raceCtx.Err() != nil
				finishAttempt()
				racerCancel()
				resultsChan <- racerResult{
					keyIndex:       idx,
					channel:        contender.channel,
					key:            contender.key,
					statusCode:     0,
					err:            sendErr,
					upstreamPaths:  upstreamPaths,
					startTime:      startTime,
					duration:       duration,
					canceled:       isCanceled,
					runtimeModel:   contender.runtimeModel,
					capabilityKey:  contender.capabilityKey,
					promptSnapshot: contender.promptSnapshot,
				}
				return
			}

			// Check response status
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				forceNonStream := ra.shouldPreferAnthropicNonStreamUpstream()
				forceStream := (ra.shouldForceOpenAIResponsesStreamUpstream(ra.outAdapter) || ra.shouldForceAnthropicStreamUpstream()) && !forceNonStream
				// A non-stream upstream returns JSON; never reject it as a "non-SSE for stream" 200.
				if !forceNonStream && isRacerFastIncompatible200(ra.internalRequest, forceStream, resp) {
					body, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
					_ = resp.Body.Close()
					cleanedUp = true
					finishAttempt()
					racerCancel()
					resultsChan <- racerResult{
						keyIndex:       idx,
						channel:        contender.channel,
						key:            contender.key,
						statusCode:     resp.StatusCode,
						err:            fmt.Errorf("upstream returned non-SSE content-type %q for stream request: %s", resp.Header.Get("Content-Type"), string(body)),
						upstreamPaths:  upstreamPaths,
						startTime:      startTime,
						duration:       duration,
						runtimeModel:   contender.runtimeModel,
						capabilityKey:  contender.capabilityKey,
						promptSnapshot: contender.promptSnapshot,
					}
					return
				}

				// A 2xx response only becomes a WINNER once real business content shows up.
				// Header-only / empty-body 200s (heartbeats, openers, usage-only) must lose:
				// awaitRaceContent replays the body through a FRESH probe adapter and reports
				// whether real content arrived within the budget. The formal outbound adapter
				// is a separate instance so probing never perturbs it.
				wantsStream := !forceNonStream && (internalRequestPrefersStream(ra.internalRequest) || forceStream)
				probeAdapter := outbound.Get(contender.channel.Type)
				probeErr := awaitRaceContent(racerCtx, resp, probeAdapter, wantsStream, raceContentBudget(firstTokenTimeout))
				if probeErr != nil {
					// Probe failure == this contender loses; close resources like any failure.
					// Classify cancellation (a real winner canceled us) BEFORE self-cancel.
					if resp.Body != nil {
						_ = resp.Body.Close()
					}
					isCanceled := racerCtx.Err() != nil || raceCtx.Err() != nil
					cleanedUp = true
					finishAttempt()
					racerCancel()
					resultsChan <- racerResult{
						keyIndex:       idx,
						channel:        contender.channel,
						key:            contender.key,
						statusCode:     resp.StatusCode,
						err:            probeErr,
						upstreamPaths:  upstreamPaths,
						startTime:      startTime,
						duration:       duration,
						canceled:       isCanceled,
						runtimeModel:   contender.runtimeModel,
						capabilityKey:  contender.capabilityKey,
						promptSnapshot: contender.promptSnapshot,
					}
					return
				}

				// First real-content 2xx winner claims victory!
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
						keyIndex:               idx,
						channel:                contender.channel,
						key:                    contender.key,
						response:               resp,
						outAdapter:             ra.outAdapter,
						preparedReq:            ra.internalRequest,
						promptSnapshot:         contender.promptSnapshot,
						runtimeModel:           contender.runtimeModel,
						capabilityKey:          contender.capabilityKey,
						modelMapped:            ra.modelMapped,
						statusCode:             resp.StatusCode,
						upstreamPaths:          upstreamPaths,
						startTime:              startTime,
						duration:               duration,
						finishAttempt:          finishAttempt,
						racerCancel:            racerCancel,
						preparedAttempt:        ra,
						promptOverridesApplied: contender.promptOverridesApplied,
						redactSession:          ra.redactSession,
						redactApplied:          ra.redactApplied,
					}
					return
				}

				// Another racer already won, close this body and record as raced out
				_ = resp.Body.Close()
				cleanedUp = true
				finishAttempt()
				racerCancel()
				resultsChan <- racerResult{
					keyIndex:       idx,
					channel:        contender.channel,
					key:            contender.key,
					statusCode:     resp.StatusCode,
					err:            context.Canceled,
					upstreamPaths:  upstreamPaths,
					startTime:      startTime,
					duration:       duration,
					canceled:       true,
					runtimeModel:   contender.runtimeModel,
					capabilityKey:  contender.capabilityKey,
					promptSnapshot: contender.promptSnapshot,
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
				keyIndex:       idx,
				channel:        contender.channel,
				key:            contender.key,
				statusCode:     resp.StatusCode,
				err:            upErr,
				upstreamPaths:  upstreamPaths,
				startTime:      startTime,
				duration:       duration,
				runtimeModel:   contender.runtimeModel,
				capabilityKey:  contender.capabilityKey,
				promptSnapshot: contender.promptSnapshot,
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

	// Process results as they arrive. If a winner arrives, immediately begin response
	// handling rather than blocking indefinitely on slow or non-responsive losers.
	for res := range resultsChan {
		if res.response != nil && winner == nil {
			w := res
			winner = &w
			break
		}
		loserResults = append(loserResults, res)
	}

	if winner != nil {
		// Winner found! Losers have already been canceled.
		// Transfer prepared request + winner prompt snapshot ownership to the parent
		// relayRequest. A cross-channel (group) win always overrides — even a source-less
		// snapshot — while the single-channel race keeps the caller's own snapshot untouched.
		parentReq.internalRequest = winner.preparedReq
		if parentReq.metrics != nil && winner.promptOverridesApplied {
			parentReq.metrics.PromptSnapshot = winner.promptSnapshot
		}
		// 出站请求体快照必须显式回写：每个 racer 的 metrics 是 parent.metrics 的
		// **副本**（见本文件 metricsCopy := *parent.metrics），赢家分支把
		// winnerRA.relayRequest 换成 parentReq 之后，副本里那份快照就再也回不来，
		// 落库恒为空。PromptSnapshot 上面这条回写是同一个道理。
		if parentReq.metrics != nil && winner.preparedAttempt != nil && winner.preparedAttempt.metrics != nil {
			// 无条件覆盖：赢家副本对本次响应才是权威。带条件会让父 metrics 挂着
			// 上一次串行尝试的 body（错误归因，比空更糟）。
			parentReq.metrics.upstreamRequestBody = winner.preparedAttempt.metrics.upstreamRequestBody
			parentReq.metrics.upstreamImageHashes = winner.preparedAttempt.metrics.upstreamImageHashes
		}

		// Reuse the racer's OWN relayAttempt instead of building a fresh winnerRA: the
		// total-timeout clock the racer armed (boundUpstreamLifetime) captured this exact
		// object, so swapping only relayRequest -> parentReq keeps that timer and the shared
		// totalClock pointer valid. A brand-new object would re-arm against fresh state and
		// could mis-fire mid-stream. Prepare-time fields (outAdapter/channel/usedKey/redact
		// session) already live on this object.
		winnerRA := winner.preparedAttempt
		if winnerRA == nil {
			winnerRA = &relayAttempt{
				outAdapter: winner.outAdapter,
				channel:    winner.channel,
				usedKey:    winner.key,
			}
		}
		winnerRA.relayRequest = parentReq
		winnerRA.internalRequest = winner.preparedReq
		winnerRA.firstTokenTimeOutSec = firstTokenTimeout
		winnerRA.modelMapped = winner.modelMapped
		winnerRA.redactSession = winner.redactSession
		winnerRA.redactApplied = winner.redactApplied
		// winnerRA 的 attempt 级 session 由本 race 分支回收 (两个 return 路径都覆盖)。
		defer winnerRA.redactClose()

		// ThinkingToContent is a main-thread, winner-only decision — never preset from the
		// first racer/channel.
		if setter, ok := parentReq.inAdapter.(interface{ SetThinkingToContent(bool) }); ok && winner.channel != nil {
			setter.SetThinkingToContent(winner.channel.ThinkingToContent)
		}

		span := parentReq.iter.StartAttempt(winner.channel.ID, winner.key.ID, winner.channel.Name)
		span.SetModelName(winner.runtimeModel)
		span.SetRouteScope(endpoint, winner.capabilityKey)
		recordAttemptProxy(span, winner.channel)
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

		// Drain remaining loser results until resultsChan closes, so late loser telemetry
		// (raced_out) is never dropped. Termination: after the winner's CAS every racer has
		// been canceled, so each racer goroutine reaches its result-send or no-result exit,
		// and the drain goroutine (wg.Wait -> close) guarantees channel closure.
		for res := range resultsChan {
			if res.response != nil {
				_ = res.response.Body.Close()
			}
			if res.finishAttempt != nil {
				res.finishAttempt()
			}
			if res.racerCancel != nil {
				res.racerCancel()
			}
			loserResults = append(loserResults, res)
		}

		// Record loser spans sequentially on main goroutine
		recordLoserResults(parentReq, endpoint, loserResults, &bestFailStatus)

		if handleErr == nil {
			winnerRA.collectResponse()
			winnerRA.metrics.ChannelKeyRemark = winnerRA.usedKey.Remark
			costDelta := winnerRA.metrics.Stats.InputCost + winnerRA.metrics.Stats.OutputCost
			op.ChannelKeyRecordUse(winnerRA.usedKey, winner.statusCode, usedAt, costDelta)

			span.End(dbmodel.AttemptSuccess, winner.statusCode, "")
			balancer.RecordRuntimeSuccess(winner.channel.ID, winnerRA.usedKey.ID, winner.runtimeModel, balancer.AttemptRuntimeMetrics{
				Duration:     span.Duration(),
				FirstToken:   firstTokenDurationSince(winnerRA.metrics.FirstTokenTime, span.StartedAt()),
				OutputTokens: winnerRA.metrics.Stats.OutputToken,
				Stream:       internalRequestPrefersStream(winnerRA.internalRequest),
			})
			op.StatsChannelUpdate(winner.channel.ID, dbmodel.StatsMetrics{
				WaitTime:       span.Duration().Milliseconds(),
				RequestSuccess: 1,
			})
			balancer.RecordSuccessScoped(winner.channel.ID, winnerRA.usedKey.ID, winner.runtimeModel, endpoint, winner.capabilityKey)
			if winnerRA.stickyEnabled {
				balancer.SetStickyWithSessionKey(winnerRA.apiKeyID, winnerRA.requestModel, winnerRA.clientSessionKey, winner.channel.ID, winnerRA.usedKey.ID)
			}
			winnerRA.metrics.ParamOverride = paramOverrideValue(winner.channel.ParamOverride)
			return attemptResult{Success: true, StatusCode: winner.statusCode}, &raceWinner{
				channel:                winner.channel,
				key:                    winner.key,
				runtimeModel:           winner.runtimeModel,
				capabilityKey:          winner.capabilityKey,
				promptSnapshot:         winner.promptSnapshot,
				promptOverridesApplied: winner.promptOverridesApplied,
			}
		}

		// Handling failed after winning response header
		recStatus := attemptStatusCode(winner.statusCode, handleErr)
		op.ChannelKeyRecordUse(winnerRA.usedKey, recStatus, usedAt, 0)
		span.End(dbmodel.AttemptFailed, recStatus, auditErrorMessage(handleErr))

		breakerCounted := shouldRecordBreakerFailure(recStatus, handleErr)
		if breakerCounted && !winner.channel.DisableCircuitBreaker {
			retryAfter, _ := retryAfterFromError(handleErr)
			balancer.RecordRuntimeFailure(winner.channel.ID, winnerRA.usedKey.ID, winner.runtimeModel, recStatus, span.Duration(), retryAfter)
			balancer.RecordFailureWithStatusScoped(winner.channel.ID, winnerRA.usedKey.ID, winner.runtimeModel, endpoint, winner.capabilityKey, recStatus)
		}
		op.StatsChannelUpdate(winner.channel.ID, dbmodel.StatsMetrics{
			WaitTime:      span.Duration().Milliseconds(),
			RequestFailed: 1,
		})
		winnerRA.metrics.ParamOverride = paramOverrideValue(winner.channel.ParamOverride)

		committed := winnerRA.wroteMeaningfulDownstream
		if committed {
			writeCommittedStreamFailure(winnerRA, handleErr)
			winnerRA.collectResponse()
		}

		retryAfter, _ := retryAfterFromError(handleErr)
		return attemptResult{
			Success:    false,
			Written:    committed,
			Err:        fmt.Errorf("channel %s failed: %w", winner.channel.Name, handleErr),
			StatusCode: recStatus,
			Retryable:  !committed && isRetryableUpstreamStreamError(handleErr),
			Fatal:      isDeterministicClientRejection(handleErr),
			RetryAfter: retryAfter,
		}, nil
	}

	// No winner found from initial receive, wait for resultsChan to close and collect all losers
	for res := range resultsChan {
		loserResults = append(loserResults, res)
	}

	lastErr = recordLoserResults(parentReq, endpoint, loserResults, &bestFailStatus)

	// All raced contenders failed
	if lastErr == nil {
		lastErr = fmt.Errorf("%d raced contenders failed", len(contenders))
	}
	finalStatus := http.StatusBadGateway
	if bestFailStatus > 0 {
		finalStatus = bestFailStatus
	}
	return attemptResult{
		Success:    false,
		Written:    false,
		Err:        fmt.Errorf("race failed: %w", lastErr),
		StatusCode: finalStatus,
		Retryable:  false,
		Fatal:      isDeterministicClientRejection(lastErr),
		RetryAfter: maxLoserRetryAfter(loserResults),
	}, nil
}

// runChannelRace executes concurrent/hedged requests across up to N available keys of a
// single channel. Original interface preserved: it races only the first clamped batch of
// keys and returns the untouched remainder for the caller to try next.
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

	delay := raceDelay(channel)
	template := parentReq.internalRequest
	runtimeModel := runtimeModelForChannel(channel, template)

	contenders := make([]raceContender, 0, len(racerKeys))
	for i, key := range racerKeys {
		contenders = append(contenders, raceContender{
			channel:         channel,
			key:             key,
			internalRequest: template,
			delay:           time.Duration(i) * delay,
			runtimeModel:    runtimeModel,
			capabilityKey:   capabilityKey,
		})
	}

	result, _ := runRaceContenders(parentReq, contenders, requestEndpoint, firstTokenTimeoutSec)
	return result, remainingKeys
}

// nextRaceWave peels the next wave (≤ raceWaveGlobalCap) off an interleaved contender
// queue, honouring each channel's configured per-channel cap (clampRaceConcurrency).
// Because the queue is round-robin interleaved, taking the prefix yields one key per
// channel first, then fills the remaining global slots. The untouched tail is returned
// for the next wave — this is a pure splitter, not a scheduler.
func nextRaceWave(queue []raceContender) (wave []raceContender, rest []raceContender) {
	perChannel := make(map[int]int)
	for _, c := range queue {
		if c.channel == nil {
			continue
		}
		if len(wave) >= raceWaveGlobalCap {
			rest = append(rest, c)
			continue
		}
		if perChannel[c.channel.ID] >= clampRaceConcurrency(c.channel.RaceKeyConcurrency) {
			rest = append(rest, c)
			continue
		}
		wave = append(wave, c)
		perChannel[c.channel.ID]++
	}
	return wave, rest
}

// applyWaveDelays (re)computes each contender's hedge delay for the wave it is about to
// run: the j-th contender of a channel in this wave starts after j*RaceDelayMs.
func applyWaveDelays(wave []raceContender) {
	seen := make(map[int]int)
	for i := range wave {
		if wave[i].channel == nil {
			continue
		}
		chID := wave[i].channel.ID
		wave[i].delay = time.Duration(seen[chID]) * raceDelay(wave[i].channel)
		seen[chID]++
	}
}

// runGroupRace runs waves of contenders in sequence, aggregating failures, until success,
// a committed (Written) response, a fatal client rejection, or exhaustion. On total
// failure it returns the real last error with the maximum observed Retry-After.
func runGroupRace(
	parentReq *relayRequest,
	contenders []raceContender,
	endpoint string,
	firstTokenTimeout int,
) (attemptResult, *raceWinner) {
	queue := contenders
	var last attemptResult
	var maxRetryAfter time.Duration
	ranAny := false

	for len(queue) > 0 {
		// Never launch another wave once the caller's context is gone: the client
		// disconnected (or the request was canceled), so a fresh upstream request would be
		// pure waste. Stop before spending the next wave.
		if raceParentContextDone(parentReq) {
			if !ranAny {
				return attemptResult{Success: false, Err: raceParentContextErr(parentReq), StatusCode: http.StatusBadGateway}, nil
			}
			break
		}
		wave, rest := nextRaceWave(queue)
		if len(wave) == 0 {
			break
		}
		applyWaveDelays(wave)

		result, winner := runRaceContenders(parentReq, wave, endpoint, firstTokenTimeout)
		ranAny = true
		if result.RetryAfter > maxRetryAfter {
			maxRetryAfter = result.RetryAfter
		}
		if result.Success || result.Written || result.Fatal {
			return result, winner
		}
		last = result
		queue = rest
	}

	if !ranAny {
		return attemptResult{Success: false, Err: fmt.Errorf("no race contenders"), StatusCode: http.StatusBadGateway}, nil
	}
	last.RetryAfter = maxRetryAfter
	return last, nil
}

// raceParentContextDone reports whether the caller's HTTP request context is already
// gone (client disconnect / cancellation). Nil-safe for tests.
func raceParentContextDone(req *relayRequest) bool {
	return raceParentContextErr(req) != nil
}

// raceParentContextErr returns the caller request context's error (nil when alive).
func raceParentContextErr(req *relayRequest) error {
	if req == nil || req.c == nil || req.c.Request == nil {
		return nil
	}
	return req.c.Request.Context().Err()
}

// maxLoserRetryAfter returns the largest Retry-After among real (non-cancel) loser errors.
func maxLoserRetryAfter(results []racerResult) time.Duration {
	var max time.Duration
	for _, res := range results {
		if res.err == nil || res.canceled {
			continue
		}
		if d, ok := retryAfterFromError(res.err); ok && d > max {
			max = d
		}
	}
	return max
}

func recordLoserResults(
	parentReq *relayRequest,
	endpoint string,
	results []racerResult,
	bestFailStatus *int,
) error {
	var lastErr error
	for _, res := range results {
		if res.channel == nil {
			continue
		}
		span := parentReq.iter.StartAttempt(res.channel.ID, res.key.ID, res.channel.Name)
		span.SetModelName(res.runtimeModel)
		span.SetRouteScope(endpoint, res.capabilityKey)
		recordAttemptProxy(span, res.channel)
		if len(res.upstreamPaths) > 0 {
			span.SetUpstreamPath(strings.Join(res.upstreamPaths, " -> "))
		}

		usedAt := time.Now().Unix()

		// A canceled loser (another racer already won) is NOT a real upstream failure: no
		// key use, no breaker, no stats — only the raced_out audit span, which is also
		// excluded from total_attempts (see retryAttemptCount: a race loss is not a retry).
		if res.canceled {
			span.End(dbmodel.AttemptFailed, res.statusCode, dbmodel.AttemptMsgRacedOut)
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
		if breakerCounted && !res.channel.DisableCircuitBreaker {
			retryAfter, _ := retryAfterFromError(res.err)
			balancer.RecordRuntimeFailure(res.channel.ID, res.key.ID, res.runtimeModel, recStatus, res.duration, retryAfter)
			balancer.RecordFailureWithStatusScoped(res.channel.ID, res.key.ID, res.runtimeModel, endpoint, res.capabilityKey, recStatus)
		}
		op.StatsChannelUpdate(res.channel.ID, dbmodel.StatsMetrics{
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
		return ra.handleStreamResponseAsNonStream(ctx, response, outAdapter, 0)
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
