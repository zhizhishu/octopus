package handlers

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/server/auth"
	"github.com/zhizhishu/octopus/internal/server/middleware"
	"github.com/zhizhishu/octopus/internal/server/resp"
	"github.com/zhizhishu/octopus/internal/server/router"
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
		).
		AddRoute(
			router.NewRoute("/balance", http.MethodPost).
				Handle(adjustUserBalance),
		).
		AddRoute(
			router.NewRoute("/balance-log", http.MethodGet).
				Handle(listUserBalanceLog),
		).
		AddRoute(
			router.NewRoute("/usage", http.MethodGet).
				Handle(getUserUsage),
		).
		AddRoute(
			router.NewRoute("/batch", http.MethodPost).
				Handle(batchUser),
		).
		AddRoute(
			router.NewRoute("/access", http.MethodGet).
				Handle(listUserAccess),
		).
		AddRoute(
			router.NewRoute("/access/detail", http.MethodGet).
				Handle(getUserAccessDetail),
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
	user, err := op.UserRegister(userRegister, middleware.ResolveClientIP(c), c.Request.Context())
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

	// Rate-limit IP key: middleware.ResolveClientIP resolves the real client IP via
	// the trusted-proxy set (Cloudflare ranges + OCTOPUS_TRUSTED_PROXIES + the
	// trusted_proxies setting) and CF-Connecting-IP / X-Forwarded-For, so it is
	// accurate behind a trusted proxy and NOT spoofable from an untrusted peer. Do
	// not read the raw CF-Connecting-IP header here — that would bypass the trust
	// check and let an attacker forge the rate-limit key.
	ipKey := middleware.ResolveClientIP(c)

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

// updateUser 是部分更新：请求里没带的字段保持原值（见 model.UserPatchRequest）。
// 旧客户端整条发，结果与以前一样；新的用户页只发改了的字段。
func updateUser(c *gin.Context) {
	var req model.UserPatchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	user, err := op.UserPatch(req, middleware.CurrentUserID(c), c.Request.Context())
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

// updateUserQuota 是旧的「直接改成」接口：把余额和月卡几项设成请求里的值。
// 只写余额 / 月卡这几列（用户名、身份、状态、备注、方案都不碰），余额有变化会记一条流水。
func updateUserQuota(c *gin.Context) {
	var req model.UserQuotaUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	reason := "直接改成（/user/quota）"
	user, err := op.UserPatch(model.UserPatchRequest{
		ID:                 req.ID,
		Balance:            &req.Balance,
		BalanceReason:      &reason,
		MonthlyLimit:       &req.MonthlyLimit,
		MonthlyUsed:        &req.MonthlyUsed,
		MonthlyExpireAt:    &req.MonthlyExpireAt,
		MonthlyResetAt:     &req.MonthlyResetAt,
		DailyLimit:         req.DailyLimit,
		DailyQuota:         req.DailyQuota,
		DailyUsed:          req.DailyUsed,
		MonthlyExpireAtISO: &req.MonthlyExpireAtISO,
		MonthlyResetAtISO:  &req.MonthlyResetAtISO,
		NextResetAtISO:     &req.NextResetAtISO,
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

// adjustUserBalance 加 / 减 / 直接改成一个用户的余额，并记一条流水。
// 加和减在库里是一条 balance = balance ± ? 的 UPDATE，不会盖掉同时发生的扣费。
func adjustUserBalance(c *gin.Context) {
	var req model.UserBalanceAdjustRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	user, entry, err := op.UserBalanceAdjust(req, middleware.CurrentUserID(c), c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	resp.Success(c, model.UserBalanceAdjustResponse{User: model.NewUserResponse(user), Log: entry})
}

func listUserBalanceLog(c *gin.Context) {
	id, err := strconv.Atoi(c.Query("id"))
	if err != nil || id <= 0 {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidParam)
		return
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	logs, err := op.UserBalanceLogList(id, limit, c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	resp.Success(c, logs)
}

// getUserUsage 返回一个用户在 today / 7d / 30d / all 里的用量（总数、每天花费、常用模型）。
func getUserUsage(c *gin.Context) {
	id, err := strconv.Atoi(c.Query("id"))
	if err != nil || id <= 0 {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidParam)
		return
	}
	usage, err := op.UserUsage(id, c.Query("range"), c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	resp.Success(c, usage)
}

// batchUser 对一批用户做同一件事（启用 / 停用 / 加余额 / 改方案 / 删除），逐个报结果。
func batchUser(c *gin.Context) {
	var req model.UserBatchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	result, err := op.UserBatch(req, middleware.CurrentUserID(c), c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	resp.Success(c, result)
}

// listUserAccess 给用户列表：每个有记录的用户最近一次的 IP / 客户端，以及「异常切换」提示。仅管理员。
func listUserAccess(c *gin.Context) {
	summaries, err := op.UserAccessSummaries(c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, summaries)
}

// getUserAccessDetail 给用户抽屉：这个人用过的 IP、客户端（次数 / 最近时间 / 哪把 key）。仅管理员。
func getUserAccessDetail(c *gin.Context) {
	id, err := strconv.Atoi(c.Query("id"))
	if err != nil || id <= 0 {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidParam)
		return
	}
	detail, err := op.UserAccessDetailGet(id, c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	resp.Success(c, detail)
}
