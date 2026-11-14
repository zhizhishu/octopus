package handlers

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/server/middleware"
	"github.com/zhizhishu/octopus/internal/server/resp"
	"github.com/zhizhishu/octopus/internal/server/router"
)

func init() {
	router.NewGroupRouter("/api/v1/user-limits").
		Use(middleware.Auth()).
		AddRoute(router.NewRoute("/current", http.MethodGet).Handle(currentUserLimits))
	router.NewGroupRouter("/api/v1/user-limits").
		Use(middleware.Auth()).Use(middleware.AdminOnly()).
		AddRoute(router.NewRoute("/:id", http.MethodGet).Handle(getUserLimits)).
		AddRoute(router.NewRoute("/:id", http.MethodPut).Handle(saveUserLimits))
}

func currentUserLimits(c *gin.Context) {
	user, ok := middleware.CurrentUser(c)
	if !ok {
		resp.Error(c, http.StatusUnauthorized, resp.ErrUnauthorized)
		return
	}
	writeUserLimits(c, user.ID)
}

func userLimitsID(c *gin.Context) (int, bool) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id <= 0 {
		resp.Error(c, http.StatusBadRequest, "invalid user id")
		return 0, false
	}
	if _, err := op.UserGet(id); err != nil {
		resp.Error(c, http.StatusNotFound, "user not found")
		return 0, false
	}
	return id, true
}

func getUserLimits(c *gin.Context) {
	id, ok := userLimitsID(c)
	if ok {
		writeUserLimits(c, id)
	}
}

func writeUserLimits(c *gin.Context, userID int) {
	state, err := op.UserQuotaStatusGet(c.Request.Context(), userID)
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, "failed to read user limits")
		return
	}
	resp.Success(c, state)
}

func saveUserLimits(c *gin.Context) {
	id, ok := userLimitsID(c)
	if !ok {
		return
	}
	var policy model.UserQuotaPolicy
	if err := c.ShouldBindJSON(&policy); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	policy.UserID = id
	if _, err := op.UserQuotaPolicySave(c.Request.Context(), policy); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, op.ErrUserQuotaConflict) {
			status = http.StatusConflict
		}
		resp.Error(c, status, err.Error())
		return
	}
	writeUserLimits(c, id)
}
