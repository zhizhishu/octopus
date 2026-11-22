package handlers

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/server/middleware"
	"github.com/zhizhishu/octopus/internal/server/resp"
	"github.com/zhizhishu/octopus/internal/server/router"
)

func init() {
	router.NewGroupRouter("/api/v1/mcp").Use(middleware.Auth()).AddRoute(
		router.NewRoute("/stats", http.MethodGet).Handle(getMCPStats),
	)
}

func getMCPStats(c *gin.Context) {
	days, err := strconv.Atoi(c.DefaultQuery("days", "7"))
	if err != nil || days < 1 || days > 90 {
		resp.Error(c, http.StatusBadRequest, "days must be between 1 and 90")
		return
	}
	user, _ := middleware.CurrentUser(c)
	stats, err := op.MCPStatsGet(c.Request.Context(), user, days)
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, "failed to read MCP statistics")
		return
	}
	resp.Success(c, stats)
}
