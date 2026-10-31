package handlers

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/server/auth"
	"github.com/zhizhishu/octopus/internal/server/middleware"
	"github.com/zhizhishu/octopus/internal/server/resp"
	"github.com/zhizhishu/octopus/internal/server/router"
)

func init() {
	router.NewGroupRouter("/api/v1/apikey").
		Use(middleware.Auth()).
		Use(middleware.RequireJSON()).
		AddRoute(
			router.NewRoute("/create", http.MethodPost).
				Handle(createAPIKey),
		).
		AddRoute(
			router.NewRoute("/list", http.MethodGet).
				Handle(listAPIKey),
		).
		AddRoute(
			router.NewRoute("/update", http.MethodPost).
				Handle(updateAPIKey),
		).
		AddRoute(
			router.NewRoute("/delete/:id", http.MethodDelete).
				Handle(deleteAPIKey),
		).
		AddRoute(
			router.NewRoute("/batch", http.MethodPost).
				Handle(batchAPIKey),
		).
		AddRoute(
			router.NewRoute("/usage", http.MethodGet).
				Handle(getAPIKeyUsage),
		).
		AddRoute(
			router.NewRoute("/models", http.MethodGet).
				Handle(listAPIKeyModels),
		)
	router.NewGroupRouter("/api/v1/apikey").
		Use(middleware.APIKeyAuth()).
		AddRoute(
			router.NewRoute("/stats", http.MethodGet).
				Handle(getStatsAPIKeyById),
		).
		AddRoute(
			router.NewRoute("/login", http.MethodGet).
				Handle(loginAPIKey),
		)
}

func createAPIKey(c *gin.Context) {
	var req model.APIKey
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	actor, ok := middleware.CurrentUser(c)
	if !ok {
		resp.Error(c, http.StatusUnauthorized, resp.ErrUnauthorized)
		return
	}
	if !actor.IsAdmin() || req.UserID <= 0 {
		req.UserID = actor.ID
	}
	req.APIKey = auth.GenerateAPIKey()
	if err := op.APIKeyCreate(&req, c.Request.Context()); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	if actor.IsAdmin() && (len(req.AccessPlanIDs) > 0 || req.DefaultAccessPlanID > 0) {
		if err := op.APIKeyAccessPlanSet(req.ID, req.AccessPlanIDs, req.DefaultAccessPlanID, c.Request.Context()); err != nil {
			resp.Error(c, http.StatusInternalServerError, err.Error())
			return
		}
	}
	created, err := op.APIKeyGet(req.ID, c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, created)
}

func listAPIKey(c *gin.Context) {
	actor, ok := middleware.CurrentUser(c)
	if !ok {
		resp.Error(c, http.StatusUnauthorized, resp.ErrUnauthorized)
		return
	}
	apiKeys, err := op.APIKeyList(c.Request.Context(), actor)
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, apiKeys)
}

func updateAPIKey(c *gin.Context) {
	var req model.APIKey
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	actor, ok := middleware.CurrentUser(c)
	if !ok {
		resp.Error(c, http.StatusUnauthorized, resp.ErrUnauthorized)
		return
	}
	if err := op.APIKeyUpdate(&req, actor, c.Request.Context()); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	if actor.IsAdmin() && (len(req.AccessPlanIDs) > 0 || req.DefaultAccessPlanID > 0) {
		if err := op.APIKeyAccessPlanSet(req.ID, req.AccessPlanIDs, req.DefaultAccessPlanID, c.Request.Context()); err != nil {
			resp.Error(c, http.StatusInternalServerError, err.Error())
			return
		}
	}
	updated, err := op.APIKeyGetScoped(req.ID, actor, c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, updated)
}

func deleteAPIKey(c *gin.Context) {
	id := c.Param("id")
	idNum, err := strconv.Atoi(id)
	if err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidParam)
		return
	}
	actor, ok := middleware.CurrentUser(c)
	if !ok {
		resp.Error(c, http.StatusUnauthorized, resp.ErrUnauthorized)
		return
	}
	if err := op.APIKeyDelete(idNum, actor, c.Request.Context()); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, nil)
}

func getStatsAPIKeyById(c *gin.Context) {
	id := c.GetInt("api_key_id")
	stats := model.NewStatsAPIKeyUsage(op.StatsAPIKeyGet(id))
	info, err := op.APIKeyGet(id, c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	models, err := op.GroupListModelForAPIKey(id, c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	var modelsString string
	if info.SupportedModels == "" {
		modelsString = strings.Join(models, ", ")
	} else {
		supportedModels := lo.Map(strings.Split(info.SupportedModels, ","), func(s string, _ int) string {
			return strings.TrimSpace(s)
		})
		models = lo.Filter(models, func(m string, _ int) bool {
			return lo.Contains(supportedModels, m)
		})
		modelsString = strings.Join(models, ", ")
	}
	info.SupportedModels = modelsString
	resp.Success(c, map[string]any{
		"stats": stats,
		"info":  info,
	})
}

func loginAPIKey(c *gin.Context) {
	resp.Success(c, nil)
}

// batchAPIKey 对一批 Key 做同一件事（启用 / 停用 / 删除），逐个报结果。形状同 /user/batch。
// 普通用户只能动自己的 Key：别人的 id 会出现在 failed 里（"API key not found"）。
func batchAPIKey(c *gin.Context) {
	var req model.APIKeyBatchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	actor, ok := middleware.CurrentUser(c)
	if !ok {
		resp.Error(c, http.StatusUnauthorized, resp.ErrUnauthorized)
		return
	}
	result, err := op.APIKeyBatch(req, actor, c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	resp.Success(c, result)
}

// getAPIKeyUsage 返回一把 Key 在 today / 7d / 30d / all 里的用量（形状同 /user/usage）。
func getAPIKeyUsage(c *gin.Context) {
	id, err := strconv.Atoi(c.Query("id"))
	if err != nil || id <= 0 {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidParam)
		return
	}
	actor, ok := middleware.CurrentUser(c)
	if !ok {
		resp.Error(c, http.StatusUnauthorized, resp.ErrUnauthorized)
		return
	}
	usage, err := op.APIKeyUsage(id, c.Query("range"), actor, c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	resp.Success(c, usage)
}

// listAPIKeyModels 列出某个用户的 Key 能限定的模型。普通用户只看自己的；管理员可带 user_id 看别人的。
func listAPIKeyModels(c *gin.Context) {
	actor, ok := middleware.CurrentUser(c)
	if !ok {
		resp.Error(c, http.StatusUnauthorized, resp.ErrUnauthorized)
		return
	}
	userID := actor.ID
	if raw := c.Query("user_id"); raw != "" && actor.IsAdmin() {
		id, err := strconv.Atoi(raw)
		if err != nil || id <= 0 {
			resp.Error(c, http.StatusBadRequest, resp.ErrInvalidParam)
			return
		}
		userID = id
	}
	models, err := op.APIKeyModelsForUser(userID, c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	resp.Success(c, models)
}
