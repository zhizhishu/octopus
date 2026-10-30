package handlers

import (
	"net/http"

	"github.com/zhizhishu/octopus/internal/relay"
	"github.com/zhizhishu/octopus/internal/server/middleware"
	"github.com/zhizhishu/octopus/internal/server/router"
)

// 统一异步任务接口，路由照 new-api 最新版 router/task-router.go 与 router/video-router.go：
//
//	POST /v1/tasks/:key               提交（:key = video[-generations|-edits|-extensions] | image[-edits] | midjourney?mode=）
//	GET  /v1/tasks/:key               查一条（:key = 任务 id），可选 ?kind=
//	GET  /v1/tasks                    列表（octopus 扩展：new-api 只有控制台 /api/task/self）
//	POST /v1/video/generations        new-api 视频提交别名 = POST /v1/tasks/video
//	GET  /v1/video/generations/:task_id new-api 视频查询（{"code":"success","data":…}）
//
// 提交委托给原生处理器，计费、日志、路由、重试都在那条路径上；见 relay/tasks.go。
func init() {
	router.NewGroupRouter("/v1").
		Use(middleware.APIKeyAuth()).
		AddRoute(router.NewRoute("/tasks", http.MethodGet).Handle(relay.TaskListHandler)).
		AddRoute(router.NewRoute("/tasks/:key", http.MethodGet).Handle(relay.TaskGetHandler)).
		AddRoute(router.NewRoute("/tasks/:key", http.MethodPost).Handle(relay.TaskSubmitHandler)).
		AddRoute(router.NewRoute("/video/generations", http.MethodPost).Handle(relay.VideoGenerationSubmitHandler)).
		AddRoute(router.NewRoute("/video/generations/:task_id", http.MethodGet).Handle(relay.VideoGenerationGetHandler))
}
