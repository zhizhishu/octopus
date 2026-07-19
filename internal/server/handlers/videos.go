package handlers

import (
	"net/http"

	"github.com/bestruirui/octopus/internal/relay"
	"github.com/bestruirui/octopus/internal/server/middleware"
	"github.com/bestruirui/octopus/internal/server/router"
	"github.com/gin-gonic/gin"
)

func init() {
	router.NewGroupRouter("/v1").
		Use(middleware.APIKeyAuth()).
		AddRoute(
			router.NewRoute("/videos", http.MethodPost).
				Handle(createVideo),
		).
		AddRoute(
			router.NewRoute("/videos/:id", http.MethodGet).
				Handle(getVideo),
		)
}

func createVideo(c *gin.Context) {
	relay.VideosHandler(c)
}

func getVideo(c *gin.Context) {
	relay.VideoPollHandler(c)
}
