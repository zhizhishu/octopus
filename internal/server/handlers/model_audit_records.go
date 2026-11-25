package handlers

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/server/middleware"
	"github.com/zhizhishu/octopus/internal/server/resp"
	"github.com/zhizhishu/octopus/internal/server/router"
	"gorm.io/gorm"
)

// 检测记录的手动清理 + 今日额度（只读）。全部仅管理员。
//
// 删除只删已经停下的任务：排队中/在跑的、以及已取消但还有一行在收尾的，
// 一律跳过并告诉前端是哪几条（见 op.ModelAuditJobsDelete）。
func init() {
	router.NewGroupRouter("/api/v1/model-audit").
		Use(middleware.Auth()).
		Use(middleware.AdminOnly()).
		AddRoute(
			router.NewRoute("/jobs/:id", http.MethodDelete).
				Handle(deleteModelAuditJob),
		).
		AddRoute(
			router.NewRoute("/budget", http.MethodGet).
				Handle(getModelAuditBudget),
		)

	router.NewGroupRouter("/api/v1/model-audit").
		Use(middleware.Auth()).
		Use(middleware.AdminOnly()).
		Use(middleware.RequireJSON()).
		AddRoute(
			router.NewRoute("/jobs/delete", http.MethodPost).
				Handle(deleteModelAuditJobs),
		).
		AddRoute(
			router.NewRoute("/jobs/clear-finished", http.MethodPost).
				Handle(clearFinishedModelAuditJobs),
		)
}

func deleteModelAuditJob(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		resp.Error(c, http.StatusBadRequest, "invalid job id")
		return
	}
	switch err := op.ModelAuditJobDelete(c.Request.Context(), id); {
	case err == nil:
		resp.Success(c, gin.H{"deleted": 1})
	case errors.Is(err, gorm.ErrRecordNotFound):
		resp.Error(c, http.StatusNotFound, "记录不存在（可能已经删了）")
	case errors.Is(err, op.ErrAuditJobBusy):
		resp.Error(c, http.StatusConflict, err.Error())
	default:
		resp.Error(c, http.StatusInternalServerError, err.Error())
	}
}

type modelAuditDeleteRequest struct {
	IDs []int64 `json:"ids"`
}

func deleteModelAuditJobs(c *gin.Context) {
	var req modelAuditDeleteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	out, err := op.ModelAuditJobsDelete(c.Request.Context(), req.IDs)
	if err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	resp.Success(c, out)
}

func clearFinishedModelAuditJobs(c *gin.Context) {
	out, err := op.ModelAuditJobsClearFinished(c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, out)
}

// getModelAuditBudget 今日检测额度（纯读，不扣）。定时体检与鹈鹕测试从这里扣。
func getModelAuditBudget(c *gin.Context) {
	used, limit := op.ModelAuditBudgetStatus(time.Now())
	remaining := limit - used
	if remaining < 0 {
		remaining = 0
	}
	resp.Success(c, gin.H{"used": used, "limit": limit, "remaining": remaining})
}
