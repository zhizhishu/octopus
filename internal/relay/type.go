package relay

import (
	"github.com/bestruirui/octopus/internal/redact"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bestruirui/octopus/internal/conf"
	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/relay/balancer"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	"github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/gin-gonic/gin"
)

// maxSSEEventSize 定义 SSE 事件的最大大小。
// 对于图像生成模型（如 gemini-3-pro-image-preview），返回的 base64 编码图像数据
// 可能非常大（高分辨率图像可能超过 10MB），因此需要设置足够大的缓冲区。
// 默认 32MB，可通过环境变量 OCTOPUS_RELAY_MAX_SSE_EVENT_SIZE 覆盖。
var maxSSEEventSize = 32 * 1024 * 1024

func init() {
	if raw := strings.TrimSpace(os.Getenv(strings.ToUpper(conf.APP_NAME) + "_RELAY_MAX_SSE_EVENT_SIZE")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			maxSSEEventSize = v
		}
	}
}

// hopByHopHeaders 定义不应转发的 HTTP 头
var hopByHopHeaders = map[string]bool{
	"authorization": true,
	"x-api-key":     true,
	// x-goog-api-key is the Gemini-native downstream auth header (the client's
	// Octopus key). Like authorization / x-api-key it must never be forwarded
	// upstream: the Gemini outbound adapter authenticates with the channel key via
	// the ?key= query param, so a leaked x-goog-api-key both exposes the client's
	// Octopus key to the upstream AND overrides the channel key on providers that
	// prefer the header — which returned 401 octopus_upstream_auth_failed.
	"x-goog-api-key":      true,
	"x-octopus-plan":      true,
	"x-octopus-group":     true,
	"connection":          true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"te":                  true,
	"trailer":             true,
	"transfer-encoding":   true,
	"upgrade":             true,
	"content-length":      true,
	"host":                true,
	"user-agent":          true,
	"accept-encoding":     true,
	"x-forwarded-for":     true,
	"x-forwarded-host":    true,
	"x-forwarded-proto":   true,
	"x-forwarded-port":    true,
	"x-real-ip":           true,
	"forwarded":           true,
	"cf-connecting-ip":    true,
	"true-client-ip":      true,
	"x-client-ip":         true,
	"x-cluster-client-ip": true,
}

// clientTimeoutHeaders are SDK/client-side advisory timeout headers.
//
// 2026-10 reversal of the original policy (this USED to be a drop list): a caller that
// declares its own SDK timeout now has that declaration forwarded verbatim. Three
// real-machine probes settled it — the client sent `x-stainless-timeout: 111`, the direct
// leg reached the upstream as `111`, and the leg through Octopus arrived as `600`, i.e. the
// value was not merely rewritten, it was dropped and then refilled from the fingerprint
// profile. Two problems with that:
//
//  1. Shape: the header MEANS "how long the caller is willing to wait". A genuine CLI
//     sends its own number; answering with a fixed 600 is a deviation from the captured
//     client, and it is the deviation the acceptance runs kept hitting.
//  2. Behaviour: telling the upstream "this caller will wait 600s" plausibly invites
//     longer thinking and slower dribbling upstream — which is exactly the two shapes
//     measured (154s verbose turns, and half-streams still generating past six minutes).
//
// The original rationale was "forwarding these can make long Claude/MCP or Responses tool
// turns fail early even though the downstream stream is healthy". That concern belongs to
// OUR side of the relay: our own rescue clocks (first-content 120s, stream-silence 300s,
// absolute request ceiling) decide when we stop waiting and retry, so we do not need to
// edit the caller's declaration to protect ourselves. A caller that sends nothing is
// unchanged: the fingerprint value still fills in (setHeaderIfMissing).
var clientTimeoutHeaders = map[string]bool{
	"x-stainless-timeout":         true,
	"x-stainless-read-timeout":    true,
	"x-stainless-connect-timeout": true,
	"x-request-timeout":           true,
	"request-timeout":             true,
	"grpc-timeout":                true,
}

// clientTraceHeaders are consumed by Octopus for route/session stickiness.
// Keep them internal so client tracing metadata does not leak into upstream
// fingerprinting or proxy-chain policy decisions.
var clientTraceHeaders = map[string]bool{
	"ah-thread-id":        true,
	"ah-trace-id":         true,
	"x-amp-thread-id":     true,
	"x-client-request-id": true,
	"session_id":          true,
	"session-id":          true,
	"x-session-id":        true,
	"conversation_id":     true,
	"conversation-id":     true,
	"x-conversation-id":   true,
	"trace-id":            true,
	"x-trace-id":          true,
}

// clientIdentityHeaders carry the DOWNSTREAM client's self-identity — its app
// name, the referring app/site, the browser origin. Octopus does not synthesize
// these; forwarding them upstream leaks who the real client is. On the
// claude/codex paths, where the outbound is a synthesized CLI fingerprint, a
// stray X-Title / HTTP-Referer / Origin also contradicts that shape (a genuine
// CLI never sends them), so this doubles as fingerprint hygiene, not only a
// privacy strip. Filtered on EVERY path (both copyHeaders and
// copyHeadersToUpstream run shouldForwardClientHeader). An operator that a
// specific upstream genuinely needs one for can re-add it via the channel
// CustomHeader (applied after this filter). Octopus still READS these off the
// INBOUND request for Cursor-probe detection (client_validation /
// cursor_openai_probe read c.Request.Header directly), which is unaffected.
var clientIdentityHeaders = map[string]bool{
	"x-title":       true,
	"http-referer":  true,
	"referer":       true,
	"origin":        true,
	"x-client-name": true,
	"x-client-app":  true,
}

// shouldForwardClientHeaderForCaller is shouldForwardClientHeader with one scoped
// exception: a CLI-shaped caller's own SDK timeout declarations are forwarded verbatim,
// because that is the one wire shape Octopus imitates (2026-10 probes: the caller sent
// `x-stainless-timeout: 111`, the direct leg reached the upstream as 111, the leg through
// Octopus arrived as 600 — the value was dropped and refilled from the fingerprint).
// Telling an upstream "this caller will wait 600s" when the caller said 111 is both a shape
// deviation and a plausible invitation to think longer and dribble slower.
//
// Everyone else is unchanged: a plain/non-CLI caller still has these headers stripped, so
// nothing about non-CLI traffic gains a caller-declared timeout upstream.
func shouldForwardClientHeaderForCaller(key string, cliShapedCaller bool) bool {
	if cliShapedCaller {
		if lower := strings.ToLower(strings.TrimSpace(key)); clientTimeoutHeaders[lower] {
			return true
		}
	}
	return shouldForwardClientHeader(key)
}

// clientIsCLIShaped reports whether the downstream caller presents itself as the Claude
// Code CLI. Two signals, both already used elsewhere for CLI-shape decisions: the
// claude-code Anthropic-Beta flag, and the claude-cli/<version> User-Agent that the
// captured clients send (the 2026-10 probes sent exactly that UA plus x-api-key).
func clientIsCLIShaped(req *http.Request) bool {
	if req == nil {
		return false
	}
	if strings.HasPrefix(strings.TrimSpace(req.Header.Get("User-Agent")), "claude-cli/") {
		return true
	}
	return strings.Contains(req.Header.Get("Anthropic-Beta"), "claude-code")
}

func shouldForwardClientHeader(key string) bool {
	lower := strings.ToLower(strings.TrimSpace(key))
	if lower == "" {
		return false
	}
	if hopByHopHeaders[lower] {
		return false
	}
	if clientTimeoutHeaders[lower] {
		// Dropped here on purpose: the caller's own SDK timeout is forwarded only for a
		// CLI-shaped caller — see shouldForwardClientHeaderForCaller, which is what the relay
		// copy paths use. A plain non-CLI caller keeps having it stripped, because forwarding
		// its declaration upstream ("I will wait 1s") tells the upstream to give up on a turn
		// we intend to stream for minutes. An existing test pins exactly that
		// (TestAnthropicOneMillionPlainClientGetsClaudeCompatibleStreamShape).
		return false
	}
	if clientTraceHeaders[lower] {
		return false
	}
	if clientIdentityHeaders[lower] {
		return false
	}
	if strings.HasPrefix(lower, "x-stainless-") {
		return false
	}
	// Browser client-hint / fetch-metadata headers (sec-ch-ua, sec-ch-ua-platform,
	// sec-fetch-mode, ...). A genuine CLI/SDK (claude-cli, codex_cli_rs) never emits these;
	// a browser-origin downstream (an immersive-translation extension, a web tool, Cursor's
	// fetch) does. Forwarding them upstream both leaks the real client's browser environment
	// AND dresses a synthesized-CLI request with browser-only headers the shape imitation
	// never intends — so strip them on every path, same as the other client-identity families.
	if strings.HasPrefix(lower, "sec-ch-") || strings.HasPrefix(lower, "sec-fetch-") {
		return false
	}
	return true
}

type relayRequest struct {
	c                   *gin.Context
	inboundType         inbound.InboundType
	inAdapter           model.Inbound
	internalRequest     *model.InternalLLMRequest
	metrics             *RelayMetrics
	apiKeyID            int
	userID              int
	requestModel        string
	clientSessionKey    string
	clientSessionSource string
	stickyEnabled       bool
	iter                *balancer.Iterator
	interventionKeyID   int
	requestState        *RequestState

	// totalTimeoutSec is the resolved whole-request ceiling in seconds (0 = disabled):
	// the group's own TotalTimeOut, else the fleet-wide
	// relay_request_total_timeout_seconds. See request_total_timeout.go.
	totalTimeoutSec int

	// totalBudget is the window actually armed (the resolved seconds, or the test
	// override). Kept beside the deadline so messages name the real budget.
	totalBudget time.Duration

	// totalDeadline is the absolute instant that ceiling passes, computed ONCE when the
	// request is constructed. Nothing may move it: each attempt only ever gets what is
	// left of it.
	totalDeadline time.Time

	// totalClock carries the ceiling's fire bookkeeping across attempts; the racer
	// shallow copy shares the pointer rather than the state.
	totalClock *relayTotalClock

	// wroteBusinessData flips true once real business data (text/tool_call/reasoning/usage
	// content payload) has been written/committed downstream. When wroteBusinessData is true,
	// the request cannot be held for manual intervention (or failover) because partial
	// business response has already been delivered to the client.
	wroteBusinessData bool

	// wroteNonStreamJSONKeepalive flips true once handleStreamResponseAsNonStream has
	// flushed a blank-line keepalive ("\n", valid JSON insignificant whitespace) to a
	// non-stream client while waiting for a slow/reasoning upstream. Like the SSE comment
	// heartbeats it commits the response head (Written()==true) but NOT
	// wroteMeaningfulDownstream, so failover stays possible. It lives on relayRequest
	// (not relayAttempt) so it persists across channel attempts: the all-channels-failed
	// path uses it to deliver a JSON error body instead of splicing an SSE error into a
	// Content-Type: application/json stream.
	wroteNonStreamJSONKeepalive bool

	// suppressPreContentKeepalive stops the PRE-CONTENT heartbeats (the hold keepalive and the
	// first-byte keepalive) once the relay knows this request is failing over. Those heartbeats
	// write ":" comments before any content, which commits HTTP 200 on the downstream connection;
	// after that no later attempt can return a real status code, so an exhausted rescue could only
	// deliver a 200 carrying an in-band error frame — a fake success the caller reads as "the model
	// answered nothing". Staying uncommitted until either real content or a terminal decision is
	// what lets the exhaustion path hand back the upstream's own 429/5xx.
	// It is only ever set while nothing meaningful has been delivered, and content delivery
	// (wroteMeaningfulDownstream) makes it irrelevant: the 200 is committed by real content then.
	suppressPreContentKeepalive atomic.Bool

	// redactRequired pins that this request has been redacted on at least one attempt
	// (protection sticky). Once true, every later attempt — even on a channel that did
	// not opt in — must still redact (with the global default flags if the channel has
	// none) so a protected request is never silently swapped to an unprotected channel.
	redactRequired bool

	// rescueClock owns the releasable automatic-recovery deadline state (see
	// rescue_window.go): the rescue timer, its absolute deadline and the released
	// flag, all guarded by the clock's own mutex. It is allocated by armRescueDeadline
	// when the rescue hold is first built and stays shared by every racer's isolated
	// relayRequest copy (pointer copy — racers never drive the rescue clock, they only
	// ever read through the nil-safe accessors). A pointer keeps sync.Mutex out of the
	// relayRequest struct itself so the racer shallow copy cannot copy a lock.
	rescueClock *relayRescueClock
}

// relayAttempt 尝试级上下文
type relayAttempt struct {
	*relayRequest // 嵌入请求级上下文

	outAdapter           model.Outbound
	channel              *dbmodel.Channel
	usedKey              dbmodel.ChannelKey
	firstTokenTimeOutSec int

	// modelMapped is set to true by applyModelMapping when channel.ModelMapping
	// translated internalRequest.Model to an upstream name. Response transformers
	// use this flag to restore the original client-visible name (ra.requestModel)
	// in the model field returned to the client.
	modelMapped bool

	// upstreamDeclaredModel is THIS attempt's own observation of the upstream's
	// self-declared response model (captured before the model_mapping rewrite,
	// first non-empty wins within the attempt — stream chunks repeat the same
	// model). It is deliberately attempt-scoped: the request-level metrics field
	// is committed only by the attempt that actually produced the final response
	// (collectResponse), so a failed attempt's declaration can never be attributed
	// to the channel that later succeeded (F10).
	upstreamDeclaredModel string

	// upstreamResponded records that THIS attempt got an HTTP response back from
	// the upstream. Receiving a response does NOT mean the request was executed:
	// a deterministic 4xx rejection (e.g. 400/422) is an explicit refusal where
	// nothing ran upstream. The audit message built from this flag states only the
	// "an HTTP response arrived" fact and derives the executed/rejected wording
	// from the response status (see attemptAuditMessage). The retry gate keys off
	// wroteMeaningfulDownstream (a fact about the CLIENT), so without this flag the
	// relay cannot tell "never reached upstream" from "reached upstream". It is
	// recorded for the audit log only and deliberately does not change the retry
	// decision.
	upstreamResponded bool

	// prewarmMu/prewarmStopped guard the first-byte keepalive goroutine so the
	// injected heartbeat writes and the main response writes never race.
	prewarmMu      sync.Mutex
	prewarmStopped bool

	// wroteMeaningfulDownstream flips true the moment the first chunk carrying real
	// content (text / reasoning / tool_calls / images / completion usage) is flushed
	// to the client. Until then the stream opener (message_start / response.created /
	// a bare role delta) is buffered, not written, and only SSE comment heartbeats go
	// out — none of which commit a message envelope. That lets a pre-content upstream
	// failure (opened stream then died before any content) still fail over to another
	// channel instead of stranding the client on a committed-but-empty 200 stream.
	// The whole-stream failover gate keys off THIS flag, not ra.c.Writer.Written(),
	// because comment heartbeats make Written() true without committing any content.
	wroteMeaningfulDownstream bool

	// chatHistoryRebuilt marks that bridgeResponsesHistoryForChat rebuilt the prior turn's
	// history into internalRequest.Messages this attempt. The STORED transcript is left
	// un-normalized (every announced tool_call kept, so a later turn can still pair a
	// still-pending parallel call); the chat tool-call pairing invariant is enforced only on
	// the wire copy at send time in forward(). Reset at the start of every bridge run.
	chatHistoryRebuilt bool

	// chatHistoryRebuiltPreviousResponseID preserves the previous_response_id the chat
	// history bridge cleared (the chat wire must never carry it). recordResponsesSessionFromInbound
	// uses it so a rebuilt chat turn's re-recorded session inherits the prior turn's
	// conversation-root (prompt-cache anchor) instead of minting a fresh root every turn.
	chatHistoryRebuiltPreviousResponseID *string

	// historyBridged marks that ANY history bridge (chat rebuild, plain-responses
	// Codex history, chat-sourced responses cursor graft) merged a PRIOR turn's text
	// into internalRequest.Messages THIS attempt. Those merged bytes are invisible to
	// the client-bytes scan (applyInboundRedaction scans RawRequest = the CLIENT's
	// current turn), so the redaction FAST PATH must not treat a zero client-scan
	// count as "clean" while historyBridged is set. Set at the bridge call sites and
	// never cleared: the flag is attempt-scoped and monotonic — a stale-true only
	// forces an extra (harmless) text pass. chatHistoryRebuilt keeps its own
	// (narrower) responsibilities.
	historyBridged bool

	// responsesDowngradedToChat is set when the responses->chat compatibility fallback
	// swapped the outbound to chat/completions. It lets bridgeResponsesHistoryForChat run on
	// that downgraded wire (which keeps no server-side response state) so a previous_response_id
	// turn is rebuilt-or-loudly-rejected instead of forwarded context-stripped under a 200.
	responsesDowngradedToChat bool

	// Credential-redaction per-attempt state. redactSession is THIS attempt's own
	// redaction session (created lazily by applyInboundRedaction with this channel's
	// flags); each attempt owns its placeholder mapping so failover/race never shares a
	// mapping across channels. redactApplied flips true once inbound text was actually
	// rewritten (placeholders exist client-side), which gates response restoration —
	// clean traffic never pays restore cost. redactFailed marks fail-closed rejection.
	// redactSse is the stream restorer (created on the main goroutine's first restored
	// event); redactStreamBroken degrades stream restoration to an explicit error.
	redactSession      *redact.Session
	redactApplied      bool
	redactFailed       bool
	redactSse          *redact.SseRestorer
	redactStreamBroken bool
}

// attemptResult 封装单次尝试的结果
type attemptResult struct {
	Success    bool  // 是否成功
	Written    bool  // 流式响应是否已开始写入（不可重试）
	Err        error // 失败时的错误
	StatusCode int   // upstream status for retry decisions
	Retryable  bool  // 上游空流等瞬态失败且未写入下游，可安全重试
	Fatal      bool  // 上下文超长等确定性错误：换任何渠道/key 都会同样失败，停止遍历
	// RetryAfter 是上游给的 Retry-After 提示（限流/过载时要求多久后再来），
	// 缺省为 0 表示上游没给。转发层用它给下游定节奏，重试轮次用它避免自己打自己。
	RetryAfter time.Duration
}
