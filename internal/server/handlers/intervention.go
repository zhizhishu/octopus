package handlers

import (
	"errors"
	"net/http"
	"strings"

	"github.com/bestruirui/octopus/internal/relay/intervention"
	"github.com/bestruirui/octopus/internal/server/middleware"
	"github.com/bestruirui/octopus/internal/server/resp"
	"github.com/bestruirui/octopus/internal/server/router"
	"github.com/gin-gonic/gin"
)

func init() {
	router.NewGroupRouter("/api/v1/intervention").
		Use(middleware.Auth()).
		Use(middleware.AdminOnly()).
		AddRoute(
			router.NewRoute("/list", http.MethodGet).
				Handle(listInterventions),
		).
		AddRoute(
			router.NewRoute("/:id", http.MethodGet).
				Handle(getIntervention),
		).
		AddRoute(
			router.NewRoute("/:id/retry", http.MethodPost).
				Handle(retryIntervention),
		).
		AddRoute(
			router.NewRoute("/:id/abort", http.MethodPost).
				Handle(abortIntervention),
		)
}

// listInterventions returns every request currently held for operator review.
func listInterventions(c *gin.Context) {
	resp.Success(c, intervention.List())
}

func getIntervention(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	snapshot, ok := intervention.Get(id)
	if !ok {
		resp.Error(c, http.StatusNotFound, "intervention not found")
		return
	}
	resp.Success(c, snapshot)
}

type interventionRetryRequest struct {
	ChannelID int    `json:"channel_id"`
	KeyID     int    `json:"key_id"`
	ModelName string `json:"model_name"`
}

// retryIntervention releases a held request against an operator-chosen channel. The
// relay goroutine is still blocked on its wait, so it picks the decision up and runs
// one more attempt before responding to the client.
func retryIntervention(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	var request interventionRetryRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	if request.ChannelID <= 0 {
		resp.Error(c, http.StatusBadRequest, "channel_id is required")
		return
	}

	err := intervention.Resolve(id, intervention.Resolution{
		Action:    intervention.ActionRetryChannel,
		ChannelID: request.ChannelID,
		KeyID:     request.KeyID,
		ModelName: strings.TrimSpace(request.ModelName),
	})
	if err != nil {
		writeInterventionResolveError(c, err)
		return
	}
	resp.Success(c, nil)
}

// abortIntervention gives up on a held request so the original upstream error reaches
// the client after all.
func abortIntervention(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	if err := intervention.Resolve(id, intervention.Resolution{Action: intervention.ActionAbort}); err != nil {
		writeInterventionResolveError(c, err)
		return
	}
	resp.Success(c, nil)
}

func writeInterventionResolveError(c *gin.Context, err error) {
	if errors.Is(err, intervention.ErrNotFound) {
		// Also covers a request that timed out or whose client disconnected while the
		// operator was deciding: the entry is gone either way.
		resp.Error(c, http.StatusNotFound, "intervention not found or already resolved")
		return
	}
	resp.Error(c, http.StatusInternalServerError, err.Error())
}
