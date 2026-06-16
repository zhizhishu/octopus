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
	router.NewGroupRouter("/api/v1/redeem").
		Use(middleware.Auth()).
		Use(middleware.RequireJSON()).
		AddRoute(
			router.NewRoute("/redeem", http.MethodPost).
				Handle(redeemCode),
		)

	router.NewGroupRouter("/api/v1/redeem").
		Use(middleware.Auth()).
		Use(middleware.AdminOnly()).
		Use(middleware.RequireJSON()).
		AddRoute(
			router.NewRoute("/list", http.MethodGet).
				Handle(listRedeemCode),
		).
		AddRoute(
			router.NewRoute("/generate", http.MethodPost).
				Handle(generateRedeemCode),
		).
		AddRoute(
			router.NewRoute("/update", http.MethodPost).
				Handle(updateRedeemCode),
		).
		AddRoute(
			router.NewRoute("/delete/:id", http.MethodDelete).
				Handle(deleteRedeemCode),
		)
}

func listRedeemCode(c *gin.Context) {
	codes, err := op.RedeemCodeList(c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, model.NewRedeemCodeResponses(codes))
}

func generateRedeemCode(c *gin.Context) {
	var req model.RedeemCodeGenerateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	codes, err := op.RedeemCodeGenerate(req, middleware.CurrentUserID(c), c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	resp.Success(c, model.NewRedeemCodeResponses(codes))
}

func updateRedeemCode(c *gin.Context) {
	var req model.RedeemCodeUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	code, err := op.RedeemCodeUpdate(req, c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	resp.Success(c, model.NewRedeemCodeResponse(code))
}

func deleteRedeemCode(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidParam)
		return
	}
	if err := op.RedeemCodeDelete(id, c.Request.Context()); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	resp.Success(c, nil)
}

func redeemCode(c *gin.Context) {
	var req model.RedeemCodeRedeemRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	user, err := op.RedeemCodeRedeem(req.Code, middleware.CurrentUserID(c), c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	resp.Success(c, model.NewUserResponse(user))
}
