package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/relay"
	"github.com/zhizhishu/octopus/internal/server/middleware"
	"github.com/zhizhishu/octopus/internal/server/router"
)

// 视频端点的注册顺序是**契约的一部分**：
//
//  1. `:id/content` 必须先于 `:id` 注册；
//  2. 三条新的 GET 子路径（字面量段）先于裸 `:id`（参数段）。
//
// 上游按子路径分组管理任务（见 relay.videoUpstreamPath），所以这几条不是
// "同一件事的别名"，而是各自不同的上游路径。注册顺序错了，同层级的字面量与
// `:param` 会在路由树里抢同一个位置 —— 轻则某条路径永远匹配不上，重则启动时
// panic。这里不做"能不能跑起来"的猜测，handlers 包里有一条真起 engine 的测试
// （videos_media_kind_route_test.go）盯着这张表。
func init() {
	router.NewGroupRouter("/v1").
		Use(middleware.APIKeyAuth()).
		AddRoute(
			router.NewRoute("/videos", http.MethodPost).
				Handle(createVideo),
		).
		// 取成品：字面量子路径 + /content，全部在 `:id` 之前。
		AddRoute(
			router.NewRoute("/videos/generations/:id/content", http.MethodGet).
				Handle(getVideoContent),
		).
		AddRoute(
			router.NewRoute("/videos/edits/:id/content", http.MethodGet).
				Handle(getVideoContent),
		).
		AddRoute(
			router.NewRoute("/videos/extensions/:id/content", http.MethodGet).
				Handle(getVideoContent),
		).
		// 查询：同样先于裸 `:id`。
		AddRoute(
			router.NewRoute("/videos/generations/:id", http.MethodGet).
				Handle(getVideo),
		).
		AddRoute(
			router.NewRoute("/videos/edits/:id", http.MethodGet).
				Handle(getVideo),
		).
		AddRoute(
			router.NewRoute("/videos/extensions/:id", http.MethodGet).
				Handle(getVideo),
		).
		// 存量契约：兼容别名 /videos 与 /videos/:id，行为一个字都不许变。
		AddRoute(
			router.NewRoute("/videos/:id", http.MethodGet).
				Handle(getVideo),
		).
		// 创建：三条子路径各自带上自己的 media kind。
		AddRoute(
			router.NewRoute("/videos/generations", http.MethodPost).
				Handle(createVideoWithMediaKind(relay.VideoMediaKindGenerations)),
		).
		AddRoute(
			router.NewRoute("/videos/edits", http.MethodPost).
				Handle(createVideoWithMediaKind(relay.VideoMediaKindEdits)),
		).
		AddRoute(
			router.NewRoute("/videos/extensions", http.MethodPost).
				Handle(createVideoWithMediaKind(relay.VideoMediaKindExtensions)),
		)
}

func createVideo(c *gin.Context) {
	relay.VideosHandler(c)
}

// createVideoWithMediaKind 把「客户端打的是哪个子路径」带进 handler。
//
// 为什么要闭包而不是从 c.Request.URL.Path 现猜：路径解析一旦和路由表各写一份，
// 新增 kind 时必漏一处（这个仓库栽过三次：header 顺序、stats 鉴权、视频计费）。
// 路径到 kind 的映射只许留在上面那张路由表里。
func createVideoWithMediaKind(mediaKind string) gin.HandlerFunc {
	return func(c *gin.Context) {
		relay.VideosHandlerKind(c, mediaKind)
	}
}

// getVideo 轮询任务状态。
//
// 刻意不传 media kind：查询走哪条上游子路径只认**创建时记录下来**的 kind
// （内存归属 → video_tasks.media_kind）。路由上的 kind 若覆盖它，客户端换个
// 路径轮询就能把一条 edits 任务送到 generations 下面，换成必然的 404。
func getVideo(c *gin.Context) {
	relay.VideoPollHandler(c)
}

// getVideoContent 取成品（视频二进制），与 getVideo 共用同一套归属校验。
func getVideoContent(c *gin.Context) {
	relay.VideoContentHandler(c)
}
