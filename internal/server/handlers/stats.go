package handlers

import (
	"net/http"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/server/middleware"
	"github.com/bestruirui/octopus/internal/server/resp"
	"github.com/bestruirui/octopus/internal/server/router"
	"github.com/gin-gonic/gin"
)

func init() {
	router.NewGroupRouter("/api/v1/stats").
		AddRoute(
			router.NewRoute("/today", http.MethodGet).
				Handle(getStatsToday),
		).
		AddRoute(
			router.NewRoute("/daily", http.MethodGet).
				Handle(getStatsDaily),
		).
		AddRoute(
			router.NewRoute("/hourly", http.MethodGet).
				Handle(getStatsHourly),
		).
		AddRoute(
			router.NewRoute("/total", http.MethodGet).
				Handle(getStatsTotal),
		).
		AddRoute(
			router.NewRoute("/model-health", http.MethodGet).
				Handle(getModelHealth),
		).
		AddRoute(
			router.NewRoute("/model-rank", http.MethodGet).
				Handle(getStatsModelRank),
		)

	router.NewGroupRouter("/api/v1/stats").
		Use(middleware.Auth()).
		AddRoute(
			router.NewRoute("/apikey", http.MethodGet).
				Handle(getStatsAPIKey),
		)

	router.NewGroupRouter("/api/v1/stats").
		Use(middleware.Auth()).
		Use(middleware.AdminOnly()).
		AddRoute(
			router.NewRoute("/user-rank", http.MethodGet).
				Handle(getStatsUserRank),
		).
		AddRoute(
			router.NewRoute("/cache-diagnostics", http.MethodGet).
				Handle(getCacheDiagnostics),
		)
}

func getStatsToday(c *gin.Context) {
	resp.Success(c, op.StatsTodayGet())
}

func getStatsDaily(c *gin.Context) {
	statsDaily, err := op.StatsGetDaily(c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, statsDaily)
}

func getStatsHourly(c *gin.Context) {
	resp.Success(c, op.StatsHourlyGet())
}

func getStatsTotal(c *gin.Context) {
	resp.Success(c, op.StatsTotalGet())
}

func getModelHealth(c *gin.Context) {
	health, err := op.ModelHourlyHealth(c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, health)
}

func getStatsModelRank(c *gin.Context) {
	rank, err := op.ModelRequestRank(c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, rank)
}

func getStatsAPIKey(c *gin.Context) {
	actor, ok := middleware.CurrentUser(c)
	if !ok {
		resp.Error(c, http.StatusUnauthorized, resp.ErrUnauthorized)
		return
	}
	if actor.IsAdmin() {
		resp.Success(c, model.NewStatsAPIKeyUsageList(op.StatsAPIKeyList()))
		return
	}
	keys, err := op.APIKeyList(c.Request.Context(), actor)
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	stats := make([]model.StatsAPIKeyUsage, 0, len(keys))
	for _, key := range keys {
		stats = append(stats, model.NewStatsAPIKeyUsage(op.StatsAPIKeyGet(key.ID)))
	}
	resp.Success(c, stats)
}

func getStatsUserRank(c *gin.Context) {
	rank, err := op.UserUsageRank(c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, rank)
}

func getCacheDiagnostics(c *gin.Context) {
	diagnostics, err := op.CacheDiagnosticsGet(c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, diagnostics)
}
