package relay

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/utils/log"
	"github.com/gin-gonic/gin"
)

// 选路在"还没有尝试任何渠道"之前就失败的原因。
const (
	// routeSelectionNoCandidates: 模型有分组，但分组里没有任何可用渠道。
	routeSelectionNoCandidates = "no_candidates"
	// routeSelectionUnconfigured: 模型没能映射到一个可用分组（方案路由未配置、被拒、或模型不存在）。
	routeSelectionUnconfigured = "route_unconfigured"
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
	if httpStatus == 0 {
		httpStatus = http.StatusServiceUnavailable
	}

	requestPath := ""
	if c.Request != nil && c.Request.URL != nil {
		requestPath = c.Request.URL.Path
	}
	if strings.TrimSpace(message) == "" {
		message = "no available channel"
	}

	relayLog := dbmodel.RelayLog{
		UserID:             c.GetInt("user_id"),
		APIKeyID:           c.GetInt("api_key_id"),
		RequestIP:          c.GetString("request_ip"),
		Time:               time.Now().Unix(),
		RequestEndpoint:    endpointNameForInbound(inboundType, requestPath),
		RequestPath:        requestPath,
		RequestModelName:   strings.TrimSpace(req.Model),
		ActualModelName:    strings.TrimSpace(req.Model),
		UseTime:            0,
		TotalAttempts:      0,
		Error:              message,
		ErrorCode:          dbmodel.RelayLogErrorCodeRouteUnresolved,
		ErrorStatus:        httpStatus,
		ErrorStrategy:      fmt.Sprintf("%s;reason=%s;upstream_forwarded=false", dbmodel.RelayLogErrorStrategyRouteSelectionPart, reason),
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

	if logErr := op.RelayLogAdd(ctx, relayLog); logErr != nil {
		log.Warnf("failed to save route selection relay log: %v", logErr)
	}
}
