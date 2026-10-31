package handlers

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/server/auth"
	"github.com/zhizhishu/octopus/internal/server/middleware"
	"github.com/zhizhishu/octopus/internal/server/resp"
	"github.com/zhizhishu/octopus/internal/server/router"
)

// 访客 Key 管理接口：整组 Auth() + AdminOnly()。
//   - Auth() 只认登录 JWT / 管理员令牌，任何 API Key（普通的、访客的）都过不去（401）；
//   - AdminOnly() 挡掉普通用户的登录态（403）。
//
// op 层每个函数再判一次管理员，不靠路由一层。
func init() {
	router.NewGroupRouter("/api/v1/guestkey").
		Use(middleware.Auth()).
		Use(middleware.AdminOnly()).
		Use(middleware.RequireJSON()).
		AddRoute(router.NewRoute("/list", http.MethodGet).Handle(listGuestKeys)).
		AddRoute(router.NewRoute("/models", http.MethodGet).Handle(listGuestKeyModels)).
		AddRoute(router.NewRoute("/create", http.MethodPost).Handle(createGuestKey)).
		AddRoute(router.NewRoute("/update", http.MethodPost).Handle(updateGuestKey)).
		AddRoute(router.NewRoute("/revoke/:id", http.MethodPost).Handle(revokeGuestKey)).
		AddRoute(router.NewRoute("/delete/:id", http.MethodDelete).Handle(deleteGuestKey)).
		AddRoute(router.NewRoute("/usage", http.MethodGet).Handle(getGuestKeyUsage)).
		AddRoute(router.NewRoute("/access", http.MethodGet).Handle(getGuestKeyAccess))
}

func guestKeyActor(c *gin.Context) (model.User, bool) {
	actor, ok := middleware.CurrentUser(c)
	if !ok {
		resp.Error(c, http.StatusUnauthorized, resp.ErrUnauthorized)
		return model.User{}, false
	}
	if !actor.IsAdmin() {
		resp.Error(c, http.StatusForbidden, "admin permission required")
		return model.User{}, false
	}
	return actor, true
}

func guestKeyIDParam(c *gin.Context, raw string) (int, bool) {
	id, err := strconv.Atoi(raw)
	if err != nil || id <= 0 {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidParam)
		return 0, false
	}
	return id, true
}

func listGuestKeys(c *gin.Context) {
	actor, ok := guestKeyActor(c)
	if !ok {
		return
	}
	keys, err := op.GuestKeyList(actor, c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusForbidden, err.Error())
		return
	}
	resp.Success(c, keys)
}

func listGuestKeyModels(c *gin.Context) {
	actor, ok := guestKeyActor(c)
	if !ok {
		return
	}
	models, err := op.GuestKeyModels(actor, c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	resp.Success(c, models)
}

func createGuestKey(c *gin.Context) {
	actor, ok := guestKeyActor(c)
	if !ok {
		return
	}
	var req model.GuestKeyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	key, err := op.GuestKeyCreate(req, auth.GenerateAPIKey(), actor, c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	resp.Success(c, key)
}

func updateGuestKey(c *gin.Context) {
	actor, ok := guestKeyActor(c)
	if !ok {
		return
	}
	var req model.GuestKeyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	key, err := op.GuestKeyUpdate(req, actor, c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	resp.Success(c, key)
}

func revokeGuestKey(c *gin.Context) {
	actor, ok := guestKeyActor(c)
	if !ok {
		return
	}
	id, ok := guestKeyIDParam(c, c.Param("id"))
	if !ok {
		return
	}
	key, err := op.GuestKeyRevoke(id, actor, c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	resp.Success(c, key)
}

func deleteGuestKey(c *gin.Context) {
	actor, ok := guestKeyActor(c)
	if !ok {
		return
	}
	id, ok := guestKeyIDParam(c, c.Param("id"))
	if !ok {
		return
	}
	if err := op.GuestKeyDelete(id, actor, c.Request.Context()); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	resp.Success(c, nil)
}

func getGuestKeyUsage(c *gin.Context) {
	actor, ok := guestKeyActor(c)
	if !ok {
		return
	}
	id, ok := guestKeyIDParam(c, c.Query("id"))
	if !ok {
		return
	}
	usage, err := op.GuestKeyUsage(id, c.Query("range"), actor, c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	resp.Success(c, usage)
}

func getGuestKeyAccess(c *gin.Context) {
	actor, ok := guestKeyActor(c)
	if !ok {
		return
	}
	id, ok := guestKeyIDParam(c, c.Query("id"))
	if !ok {
		return
	}
	access, err := op.GuestKeyAccessGet(id, actor, c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	resp.Success(c, access)
}
