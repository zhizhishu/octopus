package middleware

import (
	"net/http"
	"strings"
	"time"

	"github.com/zhizhishu/octopus/internal/conf"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/server/auth"
	"github.com/zhizhishu/octopus/internal/server/resp"
	"github.com/gin-gonic/gin"
)

func Auth() gin.HandlerFunc {
	return func(c *gin.Context) {
		token := c.GetHeader("Authorization")
		if token == "" {
			resp.Error(c, http.StatusBadRequest, resp.ErrBadRequest)
			c.Abort()
			return
		}
		bearer := strings.TrimPrefix(token, "Bearer ")
		user, ok := auth.VerifyJWTToken(bearer)
		if !ok {
			// Long-lived admin access token fallback for automation/CLI that cannot hold
			// a short-lived login JWT. Only matches a non-empty configured token (empty =
			// disabled, never a backdoor); see auth.VerifyAdminAccessToken.
			if admin, matched := auth.VerifyAdminAccessToken(bearer); matched {
				// Distinguish the credential kind so handlers can refuse to let an
				// automation token export a secret (see setting.getSettingSecret).
				// The settings group guards site secrets with a WHITELIST on the other
				// value ("jwt"), so renaming these markers denies loudly rather than
				// silently opening a hole.
				c.Set("auth_method", "admin_token")
				SetCurrentUser(c, admin)
				c.Next()
				return
			}
			resp.Error(c, http.StatusUnauthorized, resp.ErrUnauthorized)
			c.Abort()
			return
		}
		c.Set("auth_method", "jwt")
		SetCurrentUser(c, user)
		c.Next()
	}
}

func AdminOnly() gin.HandlerFunc {
	return func(c *gin.Context) {
		user, ok := CurrentUser(c)
		if !ok || !user.IsAdmin() {
			resp.Error(c, http.StatusForbidden, "admin permission required")
			c.Abort()
			return
		}
		c.Next()
	}
}

func APIKeyAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		var apiKey string
		var requestType string

		if key := c.Request.Header.Get("x-api-key"); key != "" {
			apiKey = key
			requestType = "anthropic"
		} else if key := c.Request.Header.Get("x-goog-api-key"); key != "" {
			apiKey = key
			requestType = "gemini"
		} else if auth := c.Request.Header.Get("Authorization"); auth != "" {
			apiKey = strings.TrimPrefix(auth, "Bearer ")
			requestType = "openai"
		} else if strings.HasPrefix(c.Request.URL.Path, "/v1beta/") {
			q := c.Request.URL.Query()
			if key := q.Get("key"); key != "" {
				apiKey = key
				requestType = "gemini"
				q.Del("key")
				c.Request.URL.RawQuery = q.Encode()
			}
		}

		if apiKey == "" {
			resp.Error(c, http.StatusUnauthorized, resp.ErrUnauthorized)
			c.Abort()
			return
		}

		if !strings.HasPrefix(apiKey, "sk-"+conf.APP_NAME+"-") {
			resp.Error(c, http.StatusUnauthorized, resp.ErrUnauthorized)
			c.Abort()
			return
		}
		apiKeyObj, err := op.APIKeyGetByAPIKey(apiKey, c.Request.Context())
		if err != nil {
			resp.Error(c, http.StatusUnauthorized, resp.ErrUnauthorized)
			c.Abort()
			return
		}
		if !apiKeyObj.Enabled {
			resp.Error(c, http.StatusUnauthorized, "API key is disabled")
			c.Abort()
			return
		}
		if apiKeyObj.ExpireAt > 0 && apiKeyObj.ExpireAt < time.Now().Unix() {
			resp.Error(c, http.StatusUnauthorized, "API key has expired")
			c.Abort()
			return
		}
		owner, err := op.UserGet(apiKeyObj.UserID)
		if err != nil || !owner.IsActive() {
			resp.Error(c, http.StatusUnauthorized, "API key owner is disabled")
			c.Abort()
			return
		}
		// 准入即预留：UserCanRelay 在锁内扣下一笔在途额度，防住"N 个并发请求
		// 在扣费发生前全部读到余额>0 而一起放行"。返回的 reservation 必须归还，
		// 由 defer 兜住所有返回路径（含 panic）。
		reservation, err := op.UserCanRelay(owner.ID, c.Request.Context())
		if err != nil {
			resp.Error(c, http.StatusPaymentRequired, err.Error())
			c.Abort()
			return
		}
		if reservation > 0 {
			// 注意：defer 挂在 c.Next() 之前注册，因此它在整个下游 handler 链
			// 执行完毕、实际扣费（UserRecordUsage）已经发生之后才跑 —— 正是需要的顺序。
			defer op.UserReleaseQuota(owner.ID, reservation)
		}
		statsAPIKey := op.StatsAPIKeyGet(apiKeyObj.ID)
		if apiKeyObj.MaxCost > 0 && apiKeyObj.MaxCost < statsAPIKey.StatsMetrics.OutputCost+statsAPIKey.StatsMetrics.InputCost {
			resp.Error(c, http.StatusUnauthorized, "API key has reached the max cost")
			c.Abort()
			return
		}
		endpointFamily := apiKeyEndpointFamilyForPath(c.Request.URL.Path)
		if endpointFamily != "" && !apiKeyObj.AllowsEndpointFamily(endpointFamily) {
			resp.ErrorWithCode(c, http.StatusForbidden, "endpoint_not_allowed", "API key is not allowed to access "+string(endpointFamily)+" endpoints")
			c.Abort()
			return
		}
		c.Set("request_type", requestType)
		c.Set("endpoint_family", string(endpointFamily))
		c.Set("supported_models", apiKeyObj.SupportedModels)
		c.Set("api_key_id", apiKeyObj.ID)
		c.Set("user_id", owner.ID)
		c.Set("request_ip", ResolveClientIP(c))
		SetCurrentUser(c, owner)
		c.Next()
	}
}

// EndpointFamilyForPath 把路径映射到 API key 端点族，供路由表契约测试使用。
//
// 为什么需要这个导出：apiKeyEndpointFamilyForPath 是手工维护的清单，加了新 relay
// 端点却忘了在这里登记时，该路径落到 Unclassified，于是**对所有 key 恒 403**；
// 而大多数 handler 测试直接调 handler、绕过本中间件，同样全绿 —— 这个漏登记
// 在测试期没有任何信号，只能等上线后由用户撞上。
//
// 导出它是为了让 internal/server/handlers 里的契约测试能拿到"真实注册的路由表"，
// 逐条问族归属，把漏登记变成一条会红的断言。生产代码请直接用内部函数，别绕这个。
func EndpointFamilyForPath(path string) model.APIKeyEndpointFamily {
	return apiKeyEndpointFamilyForPath(path)
}

func apiKeyEndpointFamilyForPath(path string) model.APIKeyEndpointFamily {
	path = strings.TrimSpace(path)
	if strings.HasPrefix(path, "/v1beta/") {
		return model.APIKeyEndpointFamilyGemini
	}
	if path == "/v1/messages" || path == "/v1/messages/count_tokens" {
		return model.APIKeyEndpointFamilyAnthropic
	}
	// 视频是独立 family：异步生成按次计费，不能跟着 openai-compatible（chat）走。
	if path == "/v1/videos" || strings.HasPrefix(path, "/v1/videos/") {
		return model.APIKeyEndpointFamilyVideo
	}
	if path == "/v1/chat/completions" ||
		path == "/v1/responses" ||
		path == "/v1/embeddings" ||
		path == "/v1/models" ||
		strings.HasPrefix(path, "/v1/models/") ||
		path == "/v1/completions" ||
		path == "/v1/edits" ||
		path == "/v1/responses/compact" ||
		path == "/v1/moderations" ||
		path == "/v1/rerank" ||
		// 决策类端点（systemone）。归到 openai-compatible 而不是新开一个 family：
		// 它和 chat 共用一个上游账号、共用一个 key 额度，管理员配 key 时不会想
		// 「聊天能过、决策不能过」这种分裂；单开 family 还要动
		// AllAPIKeyEndpointFamilies、前端勾选框和一次迁移，收益不抵成本。
		// 🔴 新增任何 /v1/xxx 端点都必须在这里登记，否则落到下面的 Unclassified，
		// 对所有 key 恒 403 —— 而且绕过中间件的 handler 测试照样全绿，查不出来。
		path == "/v1/systemone" ||
		strings.HasPrefix(path, "/v1/images/") ||
		strings.HasPrefix(path, "/v1/audio/") {
		return model.APIKeyEndpointFamilyOpenAICompatible
	}
	if path == "/chat/completions" ||
		path == "/responses" ||
		path == "/responses/compact" ||
		path == "/backend-api/codex/responses" ||
		path == "/backend-api/codex/responses/compact" {
		return model.APIKeyEndpointFamilyOpenAICompatible
	}
	// 未归类路径默认拒绝（fail-closed）：返回一个 Normalize 不认识的 family，
	// 于是 AllowsEndpointFamily 恒为 false，新增 relay 端点忘了归类也不会放行。
	// 唯一例外是 /api/ 下的密钥自助/管理接口，它们不属于任何 relay family。
	if strings.HasPrefix(path, "/api/") {
		return ""
	}
	return model.APIKeyEndpointFamilyUnclassified
}

func SetCurrentUser(c *gin.Context, user model.User) {
	c.Set("user", user)
	c.Set("user_id", user.ID)
	c.Set("user_role", string(user.Role))
}

func CurrentUser(c *gin.Context) (model.User, bool) {
	value, ok := c.Get("user")
	if !ok {
		return model.User{}, false
	}
	user, ok := value.(model.User)
	return user, ok
}

func CurrentUserID(c *gin.Context) int {
	return c.GetInt("user_id")
}

func CurrentUserIsAdmin(c *gin.Context) bool {
	user, ok := CurrentUser(c)
	return ok && user.IsAdmin()
}
