package handlers

import (
	"net/http"
	"strconv"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/server/middleware"
	"github.com/bestruirui/octopus/internal/server/resp"
	"github.com/bestruirui/octopus/internal/server/router"
	"github.com/gin-gonic/gin"
)

func init() {
	router.NewGroupRouter("/api/v1/fingerprint-profile").
		Use(middleware.Auth()).
		Use(middleware.AdminOnly()).
		Use(middleware.RequireJSON()).
		AddRoute(
			router.NewRoute("/list", http.MethodGet).
				Handle(listFingerprintProfile),
		).
		AddRoute(
			router.NewRoute("/create", http.MethodPost).
				Handle(createFingerprintProfile),
		).
		AddRoute(
			router.NewRoute("/update", http.MethodPost).
				Handle(updateFingerprintProfile),
		).
		AddRoute(
			router.NewRoute("/delete/:id", http.MethodDelete).
				Handle(deleteFingerprintProfile),
		)
}

func listFingerprintProfile(c *gin.Context) {
	profiles, err := op.FingerprintProfileList(c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, profiles)
}

func createFingerprintProfile(c *gin.Context) {
	var profile model.FingerprintProfile
	if err := c.ShouldBindJSON(&profile); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	if err := op.FingerprintProfileCreate(&profile, c.Request.Context()); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, profile)
}

func updateFingerprintProfile(c *gin.Context) {
	var req model.FingerprintProfileUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	profile, err := op.FingerprintProfileUpdate(&req, c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, profile)
}

func deleteFingerprintProfile(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidParam)
		return
	}
	if err := op.FingerprintProfileDelete(id, c.Request.Context()); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, nil)
}
