package handlers

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/bestruirui/octopus/internal/relay"
	"github.com/bestruirui/octopus/internal/server/middleware"
	"github.com/bestruirui/octopus/internal/server/resp"
	"github.com/bestruirui/octopus/internal/server/router"
	"github.com/gin-gonic/gin"
)

func init() {
	router.NewGroupRouter("/api/v1/log/running").
		Use(middleware.Auth()).
		Use(middleware.AdminOnly()).
		AddRoute(
			router.NewRoute("/:id/abort", http.MethodPost).
				Handle(abortRunningRequest),
		).
		AddRoute(
			router.NewRoute("/:id/rescue", http.MethodPost).
				Handle(rescueRunningRequest),
		)
}

type abortRunningRequestBody struct {
	StartedAt time.Time `json:"started_at"`
}

func abortRunningRequest(c *gin.Context) {
	controlRunningRequest(c, relay.CancelRunningRequest)
}

func rescueRunningRequest(c *gin.Context) {
	controlRunningRequest(c, relay.RescueRunningRequest)
}

func controlRunningRequest(c *gin.Context, action func(uint64, time.Time) error) {
	idParam := c.Param("id")
	id, err := strconv.ParseUint(idParam, 10, 64)
	if err != nil || id == 0 {
		resp.Error(c, http.StatusBadRequest, "invalid request id")
		return
	}

	var req abortRunningRequestBody
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	if req.StartedAt.IsZero() {
		resp.Error(c, http.StatusBadRequest, "started_at is required")
		return
	}

	err = action(id, req.StartedAt)
	if err != nil {
		if errors.Is(err, relay.ErrRunningRequestNotFound) {
			resp.Error(c, http.StatusNotFound, "running request not found")
			return
		}
		if errors.Is(err, relay.ErrRunningRequestConflict) {
			resp.Error(c, http.StatusConflict, "request control unavailable: state changed or response already started")
			return
		}
		resp.Error(c, http.StatusInternalServerError, "failed to abort running request")
		return
	}

	resp.Success(c, nil)
}
