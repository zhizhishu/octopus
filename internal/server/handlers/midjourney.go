package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/relay"
	"github.com/zhizhishu/octopus/internal/server/middleware"
	"github.com/zhizhishu/octopus/internal/server/router"
)

// Midjourney（midjourney-proxy 协议）路由，逐条对照 new-api router/relay-router.go:228-243。
//
// 没有照搬的三条，原因写在这里，免得以后有人当成漏了：
//   - GET /mj/image/:id：new-api 的「隐藏上游图片地址」代理，挂在鉴权之前（谁拿到 id
//     都能取图），而且只有开了 MjForwardUrlEnabled 才会被回包引用。octopus 的 fetch
//     原样透传上游回包，不会生成指向它的地址，单独挂一条无鉴权取图口子只是空壳。
//   - POST /mj/notify：new-api 自己也注释掉了（relay-router.go:238）。
//   - /:mode/mj/*（relay-router.go:182 的 relay/fast|relax|turbo 前缀）：只是把同一组
//     路由再挂一遍、按前缀改上游速度模式，依赖 new-api 的渠道级模式开关，octopus 没有对应设置。
func init() {
	submit := func(mode string) gin.HandlerFunc {
		return func(c *gin.Context) { relay.MidjourneySubmitHandler(c, mode) }
	}
	router.NewGroupRouter("/mj").
		Use(middleware.APIKeyAuth()).
		AddRoute(router.NewRoute("/submit/action", http.MethodPost).Handle(submit(relay.MidjourneyModeAction))).
		AddRoute(router.NewRoute("/submit/shorten", http.MethodPost).Handle(submit(relay.MidjourneyModeShorten))).
		AddRoute(router.NewRoute("/submit/modal", http.MethodPost).Handle(submit(relay.MidjourneyModeModal))).
		AddRoute(router.NewRoute("/submit/imagine", http.MethodPost).Handle(submit(relay.MidjourneyModeImagine))).
		AddRoute(router.NewRoute("/submit/change", http.MethodPost).Handle(submit(relay.MidjourneyModeChange))).
		AddRoute(router.NewRoute("/submit/simple-change", http.MethodPost).Handle(submit(relay.MidjourneyModeSimpleChange))).
		AddRoute(router.NewRoute("/submit/describe", http.MethodPost).Handle(submit(relay.MidjourneyModeDescribe))).
		AddRoute(router.NewRoute("/submit/blend", http.MethodPost).Handle(submit(relay.MidjourneyModeBlend))).
		AddRoute(router.NewRoute("/submit/edits", http.MethodPost).Handle(submit(relay.MidjourneyModeEdits))).
		AddRoute(router.NewRoute("/submit/video", http.MethodPost).Handle(submit(relay.MidjourneyModeVideo))).
		AddRoute(router.NewRoute("/submit/upload-discord-images", http.MethodPost).Handle(submit(relay.MidjourneyModeUpload))).
		AddRoute(router.NewRoute("/insight-face/swap", http.MethodPost).Handle(submit(relay.MidjourneyModeSwapFace))).
		AddRoute(router.NewRoute("/task/:id/fetch", http.MethodGet).Handle(func(c *gin.Context) { relay.MidjourneyTaskFetchHandler(c, "fetch") })).
		AddRoute(router.NewRoute("/task/:id/image-seed", http.MethodGet).Handle(func(c *gin.Context) { relay.MidjourneyTaskFetchHandler(c, "image-seed") })).
		AddRoute(router.NewRoute("/task/list-by-condition", http.MethodPost).Handle(relay.MidjourneyTaskListByConditionHandler))
}
