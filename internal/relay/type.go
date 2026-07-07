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
	"authorization":       true,
	"x-api-key":           true,
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
	if strings.HasPrefix(lower, "x-stainless-") {
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
