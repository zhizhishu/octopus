package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/modeltest"
	"github.com/bestruirui/octopus/internal/modelverify/behavior"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/server/middleware"
	"github.com/bestruirui/octopus/internal/server/resp"
	"github.com/bestruirui/octopus/internal/server/router"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/bestruirui/octopus/internal/utils/log"
	"github.com/gin-gonic/gin"
)

const (
	auditDefaultTimeoutSeconds = 180
	auditMaxTimeoutSeconds     = 300
)

func init() {
	router.NewGroupRouter("/api/v1/model-audit").
		Use(middleware.Auth()).
		Use(middleware.AdminOnly()).
		Use(middleware.RequireJSON()).
		AddRoute(
			router.NewRoute("/run", http.MethodPost).
				Handle(runModelAudit),
		)

	router.NewGroupRouter("/api/v1/model-audit").
		Use(middleware.Auth()).
		Use(middleware.AdminOnly()).
		AddRoute(
			router.NewRoute("/log-anomalies", http.MethodGet).
				Handle(getLogAnomalies),
		).
		AddRoute(
			router.NewRoute("/scheduled", http.MethodGet).
				Handle(getScheduledAudit),
		)
}

type modelAuditRequest struct {
	ChannelID      int      `json:"channel_id"`
	Model          string   `json:"model"`
	Probes         []string `json:"probes"`
	TimeoutSeconds int      `json:"timeout_seconds"`
	// LogID 可选: 带值时把这次手动审计的结论先到先得地写回那条日志行(trigger=manual),
	// 用于从日志详情页对单条日志发起检测的场景。0/缺省 = 只跑检测不落库。
	LogID int64 `json:"log_id"`
}

type modelAuditResponse struct {
	ChannelID     int                 `json:"channel_id"`
	ChannelName   string              `json:"channel_name"`
	UpstreamModel string              `json:"upstream_model"`
	Endpoint      string              `json:"endpoint"`
	DurationMs    int                 `json:"duration_ms"`
	Report        behavior.ViewReport `json:"report"`
}

func getLogAnomalies(c *gin.Context) {
	report, err := op.LogAnomalyScanGet(c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, report)
}

func getScheduledAudit(c *gin.Context) {
	resp.Success(c, op.ScheduledAuditGet())
}

func runModelAudit(c *gin.Context) {
	var req modelAuditRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	if req.ChannelID <= 0 {
		resp.Error(c, http.StatusBadRequest, "channel_id is required")
		return
	}
	modelName := strings.TrimSpace(req.Model)
	if modelName == "" {
		resp.Error(c, http.StatusBadRequest, "model is required")
		return
	}

	ids, err := parseAuditProbes(req.Probes)
	if err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}

	channel, err := op.ChannelGet(req.ChannelID, c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusNotFound, "channel not found")
		return
	}

	upstreamModel := modelName
	if mapped, ok := channel.ModelMapping[upstreamModel]; ok && mapped != "" {
		upstreamModel = mapped
	}
	endpoint := auditEndpointFor(*channel)

	sender, err := modeltest.NewProbeSender(*channel, upstreamModel, endpoint)
	if err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}

	timeout := req.TimeoutSeconds
	if timeout <= 0 {
		timeout = auditDefaultTimeoutSeconds
	}
	if timeout > auditMaxTimeoutSeconds {
		timeout = auditMaxTimeoutSeconds
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), time.Duration(timeout)*time.Second)
	defer cancel()

	started := time.Now()
	results := behavior.Run(ctx, ids, sender)
	report := behavior.ToView(behavior.BuildReport(results, modelName))

	// 结论落库: 仅当请求显式带 log_id 时才写回那行(trigger=manual, 先到先得)。
	if req.LogID > 0 {
		if reportJSON, err := json.Marshal(report); err == nil {
			if err := op.RelayLogMarkAudited(c.Request.Context(), req.LogID, string(report.Verdict), report.Score, len(report.Findings), "manual", string(reportJSON)); err != nil {
				log.Warnf("manual model audit persist for log %d failed: %v", req.LogID, err)
			}
		}
	}

	resp.Success(c, modelAuditResponse{
		ChannelID:     channel.ID,
		ChannelName:   channel.Name,
		UpstreamModel: upstreamModel,
		Endpoint:      endpoint,
		DurationMs:    int(time.Since(started).Milliseconds()),
		Report:        report,
	})
}

func parseAuditProbes(raw []string) ([]behavior.ProbeID, error) {
	if len(raw) == 0 {
		return behavior.DefaultProbes(), nil
	}
	out := make([]behavior.ProbeID, 0, len(raw))
	seen := map[behavior.ProbeID]struct{}{}
	for _, item := range raw {
		id := behavior.ProbeID(strings.TrimSpace(item))
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		if !knownAuditProbe(id) {
			return nil, fmt.Errorf("unknown probe %q", id)
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("probes is empty")
	}
	return out, nil
}

func knownAuditProbe(id behavior.ProbeID) bool {
	for _, item := range behavior.DefaultProbes() {
		if item == id {
			return true
		}
	}
	return false
}

func auditEndpointFor(ch model.Channel) string {
	switch ch.Type {
	case outbound.OutboundTypeAnthropic:
		return "anthropic_messages"
	case outbound.OutboundTypeOpenAIResponse:
		return "openai_responses"
	case outbound.OutboundTypeGemini:
		return "gemini_generate_content"
	default:
		return "openai_chat"
	}
}
