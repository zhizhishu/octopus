package handlers

import (
	"net/http"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/migration/newapi"
	"github.com/bestruirui/octopus/internal/server/middleware"
	"github.com/bestruirui/octopus/internal/server/resp"
	"github.com/bestruirui/octopus/internal/server/router"
	"github.com/gin-gonic/gin"
)

var newAPIMigrationJobs = newapi.NewJobManager()

func init() {
	router.NewGroupRouter("/api/v1/migration/newapi").
		Use(middleware.Auth()).
		Use(middleware.AdminOnly()).
		Use(middleware.RequireJSON()).
		AddRoute(
			router.NewRoute("/jobs", http.MethodPost).
				Handle(startNewAPIMigrationJob),
		).
		AddRoute(
			router.NewRoute("/jobs", http.MethodGet).
				Handle(listNewAPIMigrationJobs),
		).
		AddRoute(
			router.NewRoute("/jobs/:id", http.MethodGet).
				Handle(getNewAPIMigrationJob),
		).
		AddRoute(
			router.NewRoute("/jobs/:id/cancel", http.MethodPost).
				Handle(cancelNewAPIMigrationJob),
		)
}

func startNewAPIMigrationJob(c *gin.Context) {
	var request newapi.JobStartRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	job, err := newAPIMigrationJobs.Start(request, db.GetDB())
	if err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	resp.Success(c, job)
}

func listNewAPIMigrationJobs(c *gin.Context) {
	resp.Success(c, newAPIMigrationJobs.List())
}

func getNewAPIMigrationJob(c *gin.Context) {
	job, ok := newAPIMigrationJobs.Get(c.Param("id"))
	if !ok {
		resp.Error(c, http.StatusNotFound, "migration job not found")
		return
	}
	resp.Success(c, job)
}

func cancelNewAPIMigrationJob(c *gin.Context) {
	job, err := newAPIMigrationJobs.Cancel(c.Param("id"))
	if err != nil {
		resp.Error(c, http.StatusNotFound, err.Error())
		return
	}
	resp.Success(c, job)
}
