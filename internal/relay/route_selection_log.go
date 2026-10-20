package relay

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/transformer/inbound"
	transformerModel "github.com/zhizhishu/octopus/internal/transformer/model"
	"github.com/zhizhishu/octopus/internal/utils/log"
	"github.com/gin-gonic/gin"
)

// 选路在"还没有尝试任何渠道"之前就失败的原因。
const (
	// routeSelectionNoCandidates: 模型有分组，但分组里没有任何可用渠道。
	routeSelectionNoCandidates = "no_candidates"
	// routeSelectionUnconfigured: 模型没能映射到一个可用分组（方案路由未配置、被拒、或模型不存在）。
	routeSelectionUnconfigured = "route_unconfigured"
	// 入站请求在"还没进选路"之前就被本地拒绝的原因（见 savePreRoutingFailureRelayLog）。
	//
	// preRoutingRequestUnparsable: 请求体读不出来/解析不了（超过体积上限，或 JSON 坏了）。
	preRoutingRequestUnparsable = "request_unparsable"
	// preRoutingModelNotSupported: 请求的模型不在该 API Key 的 supported_models 白名单里。
	preRoutingModelNotSupported = "model_not_supported"
)

// saveRouteSelectionFailureRelayLog 记录"请求在选到任何渠道之前就失败"的审计日志。
//
// 这些路径过去只写 HTTP 响应、不落任何日志：管理员在日志页看不到一点痕迹，客户端也只
// 拿到一句 "no available channel"，无法区分"等一会儿会自愈"和"配置错了得人去改"。
// 这类失败没有任何上游请求，所以审计日志是运维唯一的线索。
//
// 复用既有的 local_route_selection 词汇，因此不会进模型健康统计
// （op.relayLogExcludedFromModelTelemetry 已按该 strategy 排除），也不计费、不带上游身份。
func saveRouteSelectionFailureRelayLog(ctx context.Context, c *gin.Context, inboundType inbound.InboundType, req *transformerModel.InternalLLMRequest, reason string, httpStatus int, message string) {
	if c == nil || req == nil {
		return
	}
	if strings.TrimSpace(message) == "" {
		message = "no available channel"
	}

	relayLog := buildPreUpstreamRelayLog(ctx, c, inboundType, req, dbmodel.RelayLogErrorCodeRouteUnresolved, preUpstreamErrorStrategy(dbmodel.RelayLogErrorStrategyRouteSelectionPart, reason), httpStatus, message)
	if logErr := op.RelayLogAdd(ctx, relayLog); logErr != nil {
		log.Warnf("failed to save route selection relay log: %v", logErr)
	}
}

// savePreRoutingFailureRelayLog 记录"请求在进入选路之前就被本地拒绝"的审计日志。
//
// 覆盖两个此前只写 HTTP 响应、不落任何日志的窗口：
//   - 请求体读取/解析失败（含 128MiB 上限触发的 413）——连模型名都拿不到；
//   - 请求模型不在该 API Key 的 supported_models 白名单里（400 model_not_supported）。
//
// 二者都没有任何上游请求，客户端只拿到一句拒绝，管理员在日志页同样看不到痕迹。它属于
// 同一类"上游未被触达"的失败，所以与 saveRouteSelectionFailureRelayLog 共用一条落库
// 路径（不是第二套日志系统），并复用 local_validation 词汇：op.relayLogExcludedFromModelTelemetry
// 已按该前缀把这类行挡在模型健康统计之外，也不计费、不带上游身份。
//
// 本函数只落库，不碰 c.Writer：客户端可见的响应字节与本函数无关（字节不变）。
func savePreRoutingFailureRelayLog(ctx context.Context, c *gin.Context, inboundType inbound.InboundType, req *transformerModel.InternalLLMRequest, reason, errorCode string, httpStatus int, message string) {
	if c == nil {
		return
	}

	relayLog := buildPreUpstreamRelayLog(ctx, c, inboundType, req, errorCode, preUpstreamErrorStrategy(dbmodel.RelayLogErrorStrategyLocalValidationPart, reason), httpStatus, message)
	if logErr := op.RelayLogAdd(ctx, relayLog); logErr != nil {
		log.Warnf("failed to save pre-routing relay log: %v", logErr)
	}
}

// preUpstreamErrorStrategy 拼出"上游未被触达"行的 error_strategy：家族前缀 + reason + 内部标记。
// reason 是前端/运维唯一需要读的字段（见 web humanize.strategyReason），其余标记供统计侧排除。
func preUpstreamErrorStrategy(strategyPart, reason string) string {
	return fmt.Sprintf("%s;reason=%s;upstream_forwarded=false", strategyPart, reason)
}

// buildPreUpstreamRelayLog 组装一条"上游从未被触达"的审计日志行（选路前置失败与选路失败共用）。
//
// req 允许为 nil——请求体解析失败时连模型名都不可得，此时 RequestModelName/ActualModelName
// 留空，error_strategy 里的 reason 就是唯一线索。httpStatus 传 0 表示按 503 计（与选路失败一致）。
func buildPreUpstreamRelayLog(ctx context.Context, c *gin.Context, inboundType inbound.InboundType, req *transformerModel.InternalLLMRequest, errorCode, errorStrategy string, httpStatus int, message string) dbmodel.RelayLog {
	if httpStatus == 0 {
		httpStatus = http.StatusServiceUnavailable
	}

	requestPath := ""
	if c.Request != nil && c.Request.URL != nil {
		requestPath = c.Request.URL.Path
	}
	requestModel := ""
	if req != nil {
		requestModel = strings.TrimSpace(req.Model)
	}

	relayLog := dbmodel.RelayLog{
		UserID:             c.GetInt("user_id"),
		APIKeyID:           c.GetInt("api_key_id"),
		RequestIP:          c.GetString("request_ip"),
		Time:               time.Now().Unix(),
		RequestEndpoint:    endpointNameForInbound(inboundType, requestPath),
		RequestPath:        requestPath,
		RequestModelName:   requestModel,
		ActualModelName:    requestModel,
		UseTime:            0,
		TotalAttempts:      0,
		Error:              message,
		ErrorCode:          errorCode,
		ErrorStatus:        httpStatus,
		ErrorStrategy:      errorStrategy,
		UsageSource:        dbmodel.RelayLogUsageSourceNoUsage,
		UsageMissingReason: dbmodel.RelayLogUsageMissingReasonNoInternalResponse,
	}
	if c.Request != nil {
		relayLog.RequestUserAgent = truncateUserAgentForLog(c.Request.UserAgent())
	}
	if apiKey, getErr := op.APIKeyGet(relayLog.APIKeyID, ctx); getErr == nil {
		relayLog.RequestAPIKeyName = apiKey.Name
	}
	if user, getErr := op.UserGet(relayLog.UserID); getErr == nil {
		relayLog.UserName = user.Username
	}
	return relayLog
}
