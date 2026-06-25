package handlers

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/server/auth"
	"github.com/bestruirui/octopus/internal/server/middleware"
	"github.com/bestruirui/octopus/internal/server/resp"
	"github.com/bestruirui/octopus/internal/server/router"
	"github.com/gin-gonic/gin"
)

func init() {
	router.NewGroupRouter("/api/v1/user").
		AddRoute(
			router.NewRoute("/registration-options", http.MethodGet).
				Handle(registrationOptions),
		)

	router.NewGroupRouter("/api/v1/user").
		Use(middleware.RequireJSON()).
		AddRoute(
			router.NewRoute("/login", http.MethodPost).
				Handle(login),
		).
		AddRoute(
			router.NewRoute("/register", http.MethodPost).
				Handle(register),
		).
		AddRoute(
			router.NewRoute("/send-verification-code", http.MethodPost).
				Handle(sendVerificationCode),
		)

	router.NewGroupRouter("/api/v1/user").
		Use(middleware.Auth()).
		Use(middleware.RequireJSON()).
		AddRoute(
			router.NewRoute("/change-password", http.MethodPost).
				Handle(changePassword),
		).
		AddRoute(
			router.NewRoute("/change-username", http.MethodPost).
				Handle(changeUsername),
		).
		AddRoute(
			router.NewRoute("/status", http.MethodGet).
				Handle(status),
		).
		AddRoute(
			router.NewRoute("/check-in/status", http.MethodGet).
				Handle(checkInStatus),
		).
		AddRoute(
			router.NewRoute("/check-in", http.MethodPost).
				Handle(checkIn),
		)

	router.NewGroupRouter("/api/v1/user").
		Use(middleware.Auth()).
		Use(middleware.AdminOnly()).
		Use(middleware.RequireJSON()).
		AddRoute(
			router.NewRoute("/list", http.MethodGet).
				Handle(listUser),
		).
		AddRoute(
			router.NewRoute("/create", http.MethodPost).
				Handle(createUser),
		).
		AddRoute(
			router.NewRoute("/update", http.MethodPost).
				Handle(updateUser),
		).
		AddRoute(
			router.NewRoute("/reset-password", http.MethodPost).
				Handle(resetUserPassword),
		).
		AddRoute(
			router.NewRoute("/quota", http.MethodPost).
				Handle(updateUserQuota),
		).
		AddRoute(
			router.NewRoute("/delete/:id", http.MethodDelete).
				Handle(deleteUser),
		)
}

func login(c *gin.Context) {
	var userLogin model.UserLogin
	if err := c.ShouldBindJSON(&userLogin); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	user, err := op.UserVerify(userLogin.Username, userLogin.Password)
	if err != nil {
		resp.Error(c, http.StatusUnauthorized, resp.ErrUnauthorized)
		return
	}
	token, expire, err := auth.GenerateJWTToken(user, userLogin.Expire)
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, resp.ErrInternalServer)
		return
	}
	resp.Success(c, model.UserLoginResponse{Token: token, ExpireAt: expire, User: model.NewUserResponse(user)})
}

func registrationOptions(c *gin.Context) {
	resp.Success(c, op.UserRegistrationOptions())
}

func register(c *gin.Context) {
	var userRegister model.UserRegister
	if err := c.ShouldBindJSON(&userRegister); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	user, err := op.UserRegister(userRegister, c.ClientIP(), c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	token, expire, err := auth.GenerateJWTToken(user, userRegister.Expire)
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, resp.ErrInternalServer)
		return
	}
	resp.Success(c, model.UserLoginResponse{Token: token, ExpireAt: expire, User: model.NewUserResponse(user)})
}

func sendVerificationCode(c *gin.Context) {
	var req struct {
		Email string `json:"email"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}

	// Rate-limit IP key: c.ClientIP() resolves the real client IP via the
	// trusted-proxy config set in server startup (Cloudflare ranges +
	// RemoteIPHeaders CF-Connecting-IP / X-Forwarded-For), so it is accurate
	// behind Cloudflare and NOT spoofable from a non-Cloudflare source. Do not
	// read the raw CF-Connecting-IP header here — that would bypass the trust
	// check and let an attacker forge the rate-limit key.
	ipKey := c.ClientIP()

	// This route has no Auth middleware, so verify any presented bearer token
	// the same way Auth() does (cryptographic VerifyJWTToken) and only treat the
	// caller as admin when the token verifies AND the user is an admin. No token,
	// an invalid token, or a non-admin user all yield isAdmin = false.
	isAdmin := false
	if authz := c.GetHeader("Authorization"); strings.HasPrefix(authz, "Bearer ") {
		if user, ok := auth.VerifyJWTToken(strings.TrimPrefix(authz, "Bearer ")); ok && user.IsAdmin() {
			isAdmin = true
		}
	}

	if err := op.SendEmailVerificationCode(req.Email, ipKey, isAdmin); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	resp.Success(c, "verification code sent")
}

func changePassword(c *gin.Context) {
	var user model.UserChangePassword
	if err := c.ShouldBindJSON(&user); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	if err := op.UserChangePassword(middleware.CurrentUserID(c), user.OldPassword, user.NewPassword); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	resp.Success(c, "password changed successfully")
}

func changeUsername(c *gin.Context) {
	var user model.UserChangeUsername
	if err := c.ShouldBindJSON(&user); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	if err := op.UserChangeUsername(middleware.CurrentUserID(c), user.NewUsername); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	resp.Success(c, "username changed successfully")
}

func status(c *gin.Context) {
	user, ok := middleware.CurrentUser(c)
	if !ok {
		resp.Error(c, http.StatusUnauthorized, resp.ErrUnauthorized)
		return
	}
	resp.Success(c, model.NewUserResponse(user))
}

func checkInStatus(c *gin.Context) {
	status, err := op.UserCheckInStatus(middleware.CurrentUserID(c), c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	resp.Success(c, status)
}

func checkIn(c *gin.Context) {
	result, err := op.UserCheckIn(middleware.CurrentUserID(c), c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	resp.Success(c, result)
}

func listUser(c *gin.Context) {
	users, err := op.UserList(c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, model.NewUserResponses(users))
}

func createUser(c *gin.Context) {
	var req model.UserCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	user, err := op.UserCreate(req, c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	resp.Success(c, model.NewUserResponse(user))
}

func updateUser(c *gin.Context) {
	var req model.UserUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	user, err := op.UserUpdate(req, middleware.CurrentUserID(c), c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	resp.Success(c, model.NewUserResponse(user))
}

func resetUserPassword(c *gin.Context) {
	var req model.UserResetPasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	if err := op.UserResetPassword(req.ID, req.Password, c.Request.Context()); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	resp.Success(c, nil)
}

func updateUserQuota(c *gin.Context) {
	var req model.UserQuotaUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	current, err := op.UserGet(req.ID)
	if err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	user, err := op.UserUpdate(model.UserUpdateRequest{
		ID:                 req.ID,
		Username:           current.Username,
		Role:               current.Role,
		Status:             current.Status,
		Balance:            req.Balance,
		MonthlyLimit:       req.MonthlyLimit,
		MonthlyUsed:        req.MonthlyUsed,
		MonthlyExpireAt:    req.MonthlyExpireAt,
		MonthlyResetAt:     req.MonthlyResetAt,
		DailyLimit:         req.DailyLimit,
		DailyQuota:         req.DailyQuota,
		DailyUsed:          req.DailyUsed,
		MonthlyExpireAtISO: req.MonthlyExpireAtISO,
		MonthlyResetAtISO:  req.MonthlyResetAtISO,
		NextResetAtISO:     req.NextResetAtISO,
		Note:               current.Note,
	}, middleware.CurrentUserID(c), c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	resp.Success(c, model.NewUserResponse(user))
}

func deleteUser(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidParam)
		return
	}
	if err := op.UserDelete(id, middleware.CurrentUserID(c), c.Request.Context()); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	resp.Success(c, nil)
}
