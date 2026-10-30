package handlers

import (
	"net/http"

	"github.com/zhizhishu/octopus/internal/relay"
	"github.com/zhizhishu/octopus/internal/server/middleware"
	"github.com/zhizhishu/octopus/internal/server/router"
)

// xAI（Grok）实时语音 WebSocket：GET /v1/realtime?model=grok-voice-latest。
// 路由照 sub2api routes/gateway.go 的 gateway.GET("/realtime") 与 new-api
// router/relay-router.go 的 wsRouter.GET("/realtime")；实现见 relay.GrokRealtimeHandler。
// 鉴权仍是 APIKeyAuth：Authorization 头，或浏览器写法的 WebSocket 子协议
// openai-insecure-api-key.<key> / xai-client-secret.<key>（见 middleware.realtimeSubprotocolKey）。
func init() {
	router.NewGroupRouter("/v1").
		Use(middleware.APIKeyAuth()).
		AddRoute(
			router.NewRoute("/realtime", http.MethodGet).
				Handle(relay.GrokRealtimeHandler),
		)
}
