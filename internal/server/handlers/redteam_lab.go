package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/modeltest"
	"github.com/zhizhishu/octopus/internal/modelverify"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/redteamlab"
	"github.com/zhizhishu/octopus/internal/server/middleware"
	"github.com/zhizhishu/octopus/internal/server/resp"
	"github.com/zhizhishu/octopus/internal/server/router"
)

// A deliberate, administrator-only, *manual* test lane.
// One explicit case per click, no auto-generation, retries or production prompt edits.
var redTeamLabBusy atomic.Bool
var redTeamModelName = regexp.MustCompile(`^[A-Za-z0-9_./:+@-]{1,160}$`)

func init() {
	router.NewGroupRouter("/api/v1/redteam-lab").
		Use(middleware.Auth()).
		Use(middleware.AdminOnly()).
		AddRoute(router.NewRoute("/config", http.MethodGet).Handle(redTeamLabGetConfig)).
		AddRoute(router.NewRoute("/config", http.MethodPut).Use(middleware.RequireJSON()).Handle(redTeamLabSaveConfig)).
		AddRoute(router.NewRoute("/runs", http.MethodGet).Handle(redTeamLabListRuns)).
		AddRoute(router.NewRoute("/run", http.MethodPost).Use(middleware.RequireJSON()).Handle(redTeamLabRun))
}

func loadRedTeamLabConfig() (redteamlab.Config, error) {
	raw, err := op.SettingGetString(model.SettingKeyRedTeamLabConfig)
	if err != nil {
		// Old databases do not contain the new default until the first settings
		// initialization. No setting must ever imply auto-run.
		return redteamlab.Config{Enabled: false, Cases: []redteamlab.Case{}}, nil
	}
	return redteamlab.Parse(raw)
}
func redTeamLabGetConfig(c *gin.Context) {
	if !requireAdminLogin(c) {
		return
	}
	cfg, err := loadRedTeamLabConfig()
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, "保存的实验配置无法读取")
		return
	}
	resp.Success(c, cfg)
}
func redTeamLabSaveConfig(c *gin.Context) {
	if !requireAdminLogin(c) {
		return
	}
	var cfg redteamlab.Config
	if err := c.ShouldBindJSON(&cfg); err != nil {
		resp.Error(c, http.StatusBadRequest, "实验配置 JSON 不合法")
		return
	}
	if err := cfg.Validate(); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		resp.Error(c, http.StatusBadRequest, "实验配置无法编码")
		return
	}
	if err := op.SettingSetString(model.SettingKeyRedTeamLabConfig, string(raw)); err != nil {
		resp.Error(c, http.StatusInternalServerError, "无法保存实验配置")
		return
	}
	resp.Success(c, cfg)
}
func redTeamLabListRuns(c *gin.Context) {
	if !requireAdminLogin(c) {
		return
	}
	var history []model.RedTeamLabRun
	if err := db.GetDB().WithContext(c.Request.Context()).
		Order("id DESC").Limit(25).Find(&history).Error; err != nil {
		resp.Error(c, http.StatusInternalServerError, "读取实验历史失败")
		return
	}
	if history == nil {
		history = []model.RedTeamLabRun{}
	}
	resp.Success(c, history)
}

type redTeamLabRunRequest struct {
	ChannelID int    `json:"channel_id"`
	Model     string `json:"model"`
	CaseID    string `json:"case_id"`
	Confirm   bool   `json:"confirm_cost"`
}

type redTeamLabSender func(context.Context, model.Channel, string, string, modelverify.Ask) (modelverify.Response, error)

var redTeamLabSend redTeamLabSender = func(ctx context.Context, channel model.Channel, upstreamModel, endpoint string, ask modelverify.Ask) (modelverify.Response, error) {
	sender, err := modeltest.NewProbeSender(channel, upstreamModel, endpoint)
	if err != nil {
		return modelverify.Response{}, err
	}
	return sender.Ask(ctx, ask)
}

func redTeamLabRun(c *gin.Context) {
	if !requireAdminLogin(c) {
		return
	}
	var req redTeamLabRunRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, "执行参数格式错误")
		return
	}
	req.Model = strings.TrimSpace(req.Model)
	if !req.Confirm || req.ChannelID < 1 || !redTeamModelName.MatchString(req.Model) {
		resp.Error(c, http.StatusBadRequest, "请选择有效渠道、模型，并明确确认一次付费测试")
		return
	}
	cfg, err := loadRedTeamLabConfig()
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, "实验配置读取失败")
		return
	}
	if !cfg.Enabled {
		resp.Error(c, http.StatusForbidden, "实验开关尚未启用；先保存启用状态")
		return
	}
	selected, ok := redteamlab.FindCase(cfg, req.CaseID)
	if !ok {
		resp.Error(c, http.StatusBadRequest, "请先保存并选择一个有效案例")
		return
	}
	if !redTeamLabBusy.CompareAndSwap(false, true) {
		resp.Error(c, http.StatusConflict, "已有实验正在运行；为防止重复计费，同一时间只允许一条")
		return
	}
	defer redTeamLabBusy.Store(false)

	channel, err := op.ChannelGet(req.ChannelID, c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusNotFound, "渠道不存在")
		return
	}
	if !channel.Enabled {
		resp.Error(c, http.StatusBadRequest, "渠道已停用，不能执行测试")
		return
	}
	if !redTeamChannelOffersModel(*channel, req.Model) {
		resp.Error(c, http.StatusBadRequest, "目标模型不在该渠道已配置的模型列表中")
		return
	}
	upstreamModel := req.Model
	if mapped, exists := channel.ModelMapping[upstreamModel]; exists && strings.TrimSpace(mapped) != "" {
		upstreamModel = mapped
	}
	// The normal channel probe resolves upstream protocol/CLI fingerprint. Never
	// change the channel or construct our own HTTP client for an experiment.
	endpoint := op.AuditEndpointForChannel(*channel)
	now := time.Now()
	run := model.RedTeamLabRun{
		CreatedAt:    now,
		OperatorID:   middleware.CurrentUserID(c),
		ChannelID:    channel.ID,
		ChannelName:  channel.Name,
		RequestModel: req.Model,
		CaseID:       selected.ID,
		CaseName:     selected.Name,
		PromptText:   selected.Prompt,
		ExpectedText: selected.Expected,
		Status:       "running",
		Verdict:      "unscored",
	}
	if err := db.GetDB().WithContext(c.Request.Context()).Create(&run).Error; err != nil {
		resp.Error(c, http.StatusInternalServerError, "无法创建测试记录，未发送请求")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 90*time.Second)
	defer cancel()
	maxTokens := selected.MaxTokens
	outcome, sendErr := redTeamLabSend(ctx, *channel, upstreamModel, endpoint, modelverify.Ask{
		Prompt:    selected.Prompt,
		MaxTokens: &maxTokens,
	})
	run.DurationMs = time.Since(now).Milliseconds()
	if sendErr != nil {
		run.Status = "failed"
		// Never persist proxy URLs, upstream raw errors or credentials here.
		run.ErrorMessage = "上游未完成测试；请检查渠道、密钥冷却和网关日志"
	} else {
		run.Status = "done"
		run.ResponseText, run.ResponseClipped = redteamlab.ClipResponse(outcome.Content)
		run.Verdict = redteamlab.Score(selected.Expected, outcome.Content)
		run.InputTokens = outcome.Usage.Input
		if run.InputTokens == 0 {
			run.InputTokens = outcome.Usage.Prompt
		}
		run.OutputTokens = outcome.Usage.Completion
	}
	// A client disconnect cancels its request, but the attempt must remain auditable.
	saveCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if err := db.GetDB().WithContext(saveCtx).Save(&run).Error; err != nil {
		resp.Error(c, http.StatusInternalServerError, "请求已执行，但保存实验记录失败；不要立即重试")
		return
	}
	if sendErr != nil {
		resp.Error(c, http.StatusBadGateway, fmt.Sprintf("实验编号 %d：%s", run.ID, run.ErrorMessage))
		return
	}
	resp.Success(c, run)
}

func redTeamChannelOffersModel(channel model.Channel, name string) bool {
	for _, allowed := range strings.Split(channel.Model, ",") {
		if strings.TrimSpace(allowed) == name {
			return true
		}
	}
	for _, allowed := range channel.SelectedModels {
		if strings.TrimSpace(allowed) == name {
			return true
		}
	}
	return false
}
