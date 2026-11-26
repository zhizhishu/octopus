package handlers

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/prompt"
	"github.com/zhizhishu/octopus/internal/server/middleware"
	"github.com/zhizhishu/octopus/internal/server/resp"
	"github.com/zhizhishu/octopus/internal/server/router"
)

func init() {
	router.NewGroupRouter("/api/v1/prompt-rule").
		Use(middleware.Auth()).
		Use(middleware.AdminOnly()).
		Use(middleware.RequireJSON()).
		AddRoute(router.NewRoute("/list", http.MethodGet).Handle(listPromptRules)).
		AddRoute(router.NewRoute("/create", http.MethodPost).Handle(createPromptRule)).
		AddRoute(router.NewRoute("/update", http.MethodPost).Handle(updatePromptRule)).
		AddRoute(router.NewRoute("/delete", http.MethodPost).Handle(deletePromptRule)).
		AddRoute(router.NewRoute("/toggle", http.MethodPost).Handle(togglePromptRule)).
		AddRoute(router.NewRoute("/reorder", http.MethodPost).Handle(reorderPromptRules)).
		AddRoute(router.NewRoute("/preview", http.MethodPost).Handle(previewPromptRules))
}

// promptRuleStatus 把 op 写路径的错误分成 400/500：定位不到/校验失败/重排不匹配
// 是调用方的问题；其余（store、DB）是服务端问题。
func promptRuleStatus(err error) int {
	switch {
	case errors.Is(err, op.ErrPromptRuleNotFound),
		errors.Is(err, op.ErrPromptRuleInvalid),
		errors.Is(err, op.ErrPromptRuleReorder):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

func listPromptRules(c *gin.Context) {
	rules, err := op.PromptRuleList(c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, rules)
}

func createPromptRule(c *gin.Context) {
	var rule model.PromptRule
	if err := c.ShouldBindJSON(&rule); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := op.PromptRuleCreate(&rule, c.Request.Context()); err != nil {
		resp.Error(c, promptRuleStatus(err), err.Error())
		return
	}
	resp.Success(c, rule)
}

func updatePromptRule(c *gin.Context) {
	var rule model.PromptRule
	if err := c.ShouldBindJSON(&rule); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	// 完整覆盖式更新：定位只认 body 里的 id；created_at 保留（body 不可伪造），
	// updated_at 由服务端推进。
	if err := op.PromptRuleUpdate(&rule, c.Request.Context()); err != nil {
		resp.Error(c, promptRuleStatus(err), err.Error())
		return
	}
	resp.Success(c, rule)
}

func deletePromptRule(c *gin.Context) {
	var req struct {
		ID int `json:"id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := op.PromptRuleDelete(req.ID, c.Request.Context()); err != nil {
		resp.Error(c, promptRuleStatus(err), err.Error())
		return
	}
	resp.Success(c, nil)
}

func togglePromptRule(c *gin.Context) {
	var req struct {
		ID      int  `json:"id"`
		Enabled bool `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := op.PromptRuleToggle(req.ID, req.Enabled, c.Request.Context()); err != nil {
		resp.Error(c, promptRuleStatus(err), err.Error())
		return
	}
	resp.Success(c, req)
}

func reorderPromptRules(c *gin.Context) {
	var req struct {
		IDs []int `json:"ids"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := op.PromptRuleReorder(req.IDs, c.Request.Context()); err != nil {
		resp.Error(c, promptRuleStatus(err), err.Error())
		return
	}
	resp.Success(c, req)
}

// previewPromptRule 是提示词页的一次性预览：body 是模拟请求，返回最终文本与来源段。
// 管理员只是把它当一条模拟规则的 is_admin 参与 Evaluate 匹配，不牵涉鉴权判定。
func previewPromptRules(c *gin.Context) {
	var req struct {
		PlanID       int    `json:"plan_id"`
		Model        string `json:"model"`
		ChannelID    int    `json:"channel_id"`
		IsAdmin      bool   `json:"is_admin"`
		ClientSystem string `json:"client_system"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	rules, err := op.PromptRuleList(c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	scope := prompt.Scope{
		PlanID:       req.PlanID,
		RequestModel: req.Model,
		ChannelID:    req.ChannelID,
		IsAdmin:      req.IsAdmin,
	}
	resp.Success(c, prompt.Preview(rules, scope, req.ClientSystem))
}
