package handlers

import (
	"context"
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
}

type modelAuditRequest struct {
	ChannelID      int      `json:"channel_id"`
	Model          string   `json:"model"`
	Probes         []string `json:"probes"`
	TimeoutSeconds int      `json:"timeout_seconds"`
}

type modelAuditResponse struct {
	ChannelID     int                 `json:"channel_id"`
	ChannelName   string              `json:"channel_name"`
	UpstreamModel string              `json:"upstream_model"`
	Endpoint      string              `json:"endpoint"`
	DurationMs    int                 `json:"duration_ms"`
	Report        behavior.ViewReport `json:"report"`
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
