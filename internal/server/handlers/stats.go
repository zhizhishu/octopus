package handlers

import (
	"net/http"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/server/middleware"
	"github.com/zhizhishu/octopus/internal/server/resp"
	"github.com/zhizhishu/octopus/internal/server/router"
	"github.com/gin-gonic/gin"
)

func init() {
	// 🔴 这一组必须挂 Auth()。
	//
	// 同文件下面的 /apikey 挂了 Auth，/user-rank、/cache-diagnostics 更挂了
	// Auth+AdminOnly，唯独前六个（今天/按天/按小时/总计/模型健康/模型排行）
	// 什么中间件都没有 —— server.go 里没有任何全局鉴权，所以**不带任何凭证**
	// 就能 GET 到全站 token 用量、成本、模型排名和渠道健康度：客户规模、
	// 上游成本结构、哪家渠道在挂，全都对公网裸奔。
	//
	// 为什么不是 AdminOnly：普通用户的首页（USER_ROUTE_IDS 含 'home'）就是靠
	// 这几个接口画图的，锁成管理员会让他们的首页全空。这里只要求"是登录用户"，
	// 数据范围由各 handler 按角色收窄（管理员看全站，其他人看自己）。
	router.NewGroupRouter("/api/v1/stats").
		Use(middleware.Auth()).
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

// statsScopeIsAdmin 判断当前请求是否该看全站数据。
//
// 这些聚合缓存（statsTodayCache / statsHourlyCache / statsTotalCache / 模型排行 /
// 模型健康）都是**全站口径**、没有按用户拆分的版本。非管理员拿到它们等于看到
// 别人的用量和成本，所以非管理员一律返回零值骨架：前端仍能渲染页面结构，
// 但不会泄露任何跨用户数字。
//
// 为什么不在数据层加按用户聚合：那需要给每个统计缓存再建一份 per-user 维度、
// 并改动所有写入路径（statsHourlyCache 等是全局累加的），属于独立工程。
// 这里先把"公网可读"这个洞堵上，per-user 统计留作后续。
func statsScopeIsAdmin(c *gin.Context) bool {
	actor, ok := middleware.CurrentUser(c)
	if !ok {
		return false
	}
	return actor.IsAdmin()
}

func getStatsToday(c *gin.Context) {
	if !statsScopeIsAdmin(c) {
		resp.Success(c, model.StatsDaily{})
		return
	}
	resp.Success(c, op.StatsTodayGet())
}

func getStatsDaily(c *gin.Context) {
	if !statsScopeIsAdmin(c) {
		resp.Success(c, []model.StatsDaily{})
		return
	}
	statsDaily, err := op.StatsGetDaily(c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, statsDaily)
}

func getStatsHourly(c *gin.Context) {
	if !statsScopeIsAdmin(c) {
		resp.Success(c, []model.StatsHourly{})
		return
	}
	resp.Success(c, op.StatsHourlyGet())
}

func getStatsTotal(c *gin.Context) {
	if !statsScopeIsAdmin(c) {
		resp.Success(c, model.StatsTotal{})
		return
	}
	resp.Success(c, op.StatsTotalGet())
}

func getModelHealth(c *gin.Context) {
	if !statsScopeIsAdmin(c) {
		// 渠道健康度是运维视角的系统指标：哪个渠道在挂、错误率多少。
		// 对普通用户暴露它没有用处，只会泄露上游结构。
		resp.Success(c, model.ModelHealthResponse{})
		return
	}
	health, err := op.ModelHourlyHealth(c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, health)
}

func getStatsModelRank(c *gin.Context) {
	if !statsScopeIsAdmin(c) {
		resp.Success(c, []model.ModelRankItem{})
		return
	}
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
