package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/modeltest"
	"github.com/zhizhishu/octopus/internal/modelverify/behavior"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/server/middleware"
	"github.com/zhizhishu/octopus/internal/server/resp"
	"github.com/zhizhishu/octopus/internal/server/router"
	"github.com/zhizhishu/octopus/internal/utils/log"
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
		).
		AddRoute(
			router.NewRoute("/jobs", http.MethodGet).
				Handle(listModelAuditJobs),
		).
		AddRoute(
			router.NewRoute("/jobs/:id", http.MethodGet).
				Handle(getModelAuditJob),
		)

	router.NewGroupRouter("/api/v1/model-audit").
		Use(middleware.Auth()).
		Use(middleware.AdminOnly()).
		Use(middleware.RequireJSON()).
		AddRoute(
			router.NewRoute("/enqueue", http.MethodPost).
				Handle(enqueueModelAudit),
		).
		AddRoute(
			router.NewRoute("/jobs/:id/cancel", http.MethodPost).
				Handle(cancelModelAuditJob),
		)
}

// modelAuditEnqueueRequest 批量入队请求：一次提交一批模型，后台串行跑。
type modelAuditEnqueueRequest struct {
	ChannelID      int      `json:"channel_id"`
	Models         []string `json:"models"`
	Probes         []string `json:"probes"`
	TimeoutSeconds int      `json:"timeout_seconds"`
	LogID          int64    `json:"log_id"`
}

func enqueueModelAudit(c *gin.Context) {
	var req modelAuditEnqueueRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	jobID, err := op.ModelAuditEnqueue(c.Request.Context(), op.ModelAuditEnqueueInput{
		ChannelID:      req.ChannelID,
		Models:         req.Models,
		Probes:         req.Probes,
		TimeoutSeconds: req.TimeoutSeconds,
		LogID:          req.LogID,
		Trigger:        "manual",
	})
	if err != nil {
		// 已经在队列里不算错误：用户想要的结果（这些模型会被检测）已经成立。
		// 用 409 而不是 400，让前端能把它显示成"不用重复提交"的提示，
		// 而不是一条红色的失败信息。
		if errors.Is(err, op.ErrAuditAlreadyQueued) {
			resp.Error(c, http.StatusConflict, err.Error())
			return
		}
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	resp.Success(c, gin.H{"job_id": jobID})
}

func listModelAuditJobs(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	jobs, err := op.ModelAuditJobList(c.Request.Context(), limit)
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, jobs)
}

func getModelAuditJob(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		resp.Error(c, http.StatusBadRequest, "invalid job id")
		return
	}
	job, results, err := op.ModelAuditJobDetail(c.Request.Context(), id)
	if err != nil {
		resp.Error(c, http.StatusNotFound, "job not found")
		return
	}
	resp.Success(c, gin.H{"job": job, "results": results})
}

func cancelModelAuditJob(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		resp.Error(c, http.StatusBadRequest, "invalid job id")
		return
	}
	if err := op.ModelAuditJobCancel(c.Request.Context(), id); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, gin.H{"job_id": id})
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
	endpoint := op.AuditEndpointForChannel(*channel)

	// 单飞闸门：同一 (渠道, 上游模型) 同时只允许一个手动检测在跑。
	// 放在解析出 upstreamModel 之后，因为锁的粒度就是这一对；放在任何探针发送之前，
	// 因为这条路径上的每个探针都是一次真实计费请求。重复点击/多标签页/直接调接口
	// 都会在这里被挡住，不是在 UI 上假挡一下。
	release, ok := acquireModelAuditSlot(channel.ID, upstreamModel)
	if !ok {
		resp.Error(c, http.StatusConflict,
			"an audit for this channel and model is already running; wait for it to finish instead of starting a second billable run")
		return
	}
	defer release()

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
	results := behavior.Run(ctx, ids, sender, upstreamModel)
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
	// 用“注册表里有没有”判存在，而不是拿 DefaultProbes 当白名单：
	// 指纹探针刻意默认关闭（开销高），但手动勾选时必须能跑。
	return behavior.IsKnownProbe(id)
}
