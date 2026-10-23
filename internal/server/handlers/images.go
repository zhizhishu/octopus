package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/relay"
	"github.com/zhizhishu/octopus/internal/server/middleware"
	"github.com/zhizhishu/octopus/internal/server/router"
)

// 图片端点的注册顺序是**契约的一部分**（契约：/generations/async 必须在
// /generations 之前）：
//
//	`/v1/images/generations` 是纯字面量路由，理论上与 `/generations/async` 不冲突，
//	但"理论上"不作数 —— gin 的同层字面量段/参数段抢位置是启动时 panic 或某条路径
//	永远匹配不上的经典来源（videos 那边就栽过，见 videos.go 的同段注释）。
//	所以这里把更长的字面量路径排在前面，并由 handlers 包里真起 engine 的
//	images_async_route_test.go 盯着这张表。
func init() {
	router.NewGroupRouter("/v1/images").
		Use(middleware.APIKeyAuth()).
		// 异步：字面量更长的先注册。
		AddRoute(
			router.NewRoute("/generations/async", http.MethodPost).
				Handle(asyncSubmit(model.ImageTaskEndpointGenerations)),
		).
		AddRoute(
			router.NewRoute("/edits/async", http.MethodPost).
				Handle(asyncSubmit(model.ImageTaskEndpointEdits)),
		).
		// 异步任务查询（参数段，与上面几条不同层级）。
		AddRoute(
			router.NewRoute("/tasks/:task_id", http.MethodGet).
				Handle(asyncGet),
		).
		AddRoute(
			router.NewRoute("/generations", http.MethodPost).
				Handle(generations),
		).
		AddRoute(
			router.NewRoute("/edits", http.MethodPost).
				Handle(edits),
		).
		AddRoute(
			router.NewRoute("/variations", http.MethodPost).
				Handle(variations),
		)
}

func generations(c *gin.Context) {
	relay.ImagesHandler("/images/generations", c)
}

func edits(c *gin.Context) {
	relay.ImagesHandler("/images/edits", c)
}

func variations(c *gin.Context) {
	relay.ImagesHandler("/images/variations", c)
}

// asyncSubmit 把"客户端打的是哪条子路径"用闭包带进 handler，**不**从 URL 现猜。
// 理由同 videos.go 的 createVideoWithMediaKind：路径到 endpoint 的映射只许留在
// 上面那张路由表里，新增一条就必漏一处（这个仓库栽过三次）。
//
// 传进来的是 relay.ImageTaskEndpoint* 常量（generations / edits），与任务行上
// Endpoint 列的取值同一套：后端执行器靠它回到同一条上游子路径重放。
func asyncSubmit(endpoint string) gin.HandlerFunc {
	return func(c *gin.Context) {
		relay.ImagesAsyncSubmit(endpoint, c)
	}
}

// asyncGet 查询异步任务状态。
//
// 刻意不传 endpoint：查询只认任务行上记录的 endpoint（见 relay.ImagesAsyncGet），
// 路由传进来的值若覆盖它，客户端换个路径就能把一条 edits 任务指到 generations 下面。
func asyncGet(c *gin.Context) { relay.ImagesAsyncGet(c) }
