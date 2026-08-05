package relay

import (
	"os"
	"strconv"
	"strings"
	"sync"

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

// clientTimeoutHeaders are SDK/client-side advisory timeout headers. When
// Octopus is the relay, forwarding them upstream can make long Claude/MCP or
// Responses tool turns fail early even though the downstream stream is healthy.
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

func shouldForwardClientHeader(key string) bool {
	lower := strings.ToLower(strings.TrimSpace(key))
	if lower == "" {
		return false
	}
	if hopByHopHeaders[lower] {
		return false
	}
	if clientTimeoutHeaders[lower] {
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

	// responsesDowngradedToChat is set when the responses->chat compatibility fallback
	// swapped the outbound to chat/completions. It lets bridgeResponsesHistoryForChat run on
	// that downgraded wire (which keeps no server-side response state) so a previous_response_id
	// turn is rebuilt-or-loudly-rejected instead of forwarded context-stripped under a 200.
	responsesDowngradedToChat bool
}

// attemptResult 封装单次尝试的结果
type attemptResult struct {
	Success    bool  // 是否成功
	Written    bool  // 流式响应是否已开始写入（不可重试）
	Err        error // 失败时的错误
	StatusCode int   // upstream status for retry decisions
	Retryable  bool  // 上游空流等瞬态失败且未写入下游，可安全重试
	Fatal      bool  // 上下文超长等确定性错误：换任何渠道/key 都会同样失败，停止遍历
}
