package middleware

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/conf"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/server/auth"
	"github.com/zhizhishu/octopus/internal/server/resp"
	"github.com/zhizhishu/octopus/internal/telemetry"
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
		} else if key, ok := midjourneyClientKey(c.Request); ok {
			apiKey = key
			requestType = "openai"
		} else if key, ok := realtimeSubprotocolKey(c.Request); ok {
			apiKey = key
			requestType = "openai"
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
		if apiKeyObj.IsRevoked() {
			resp.Error(c, http.StatusUnauthorized, "API key has been revoked")
			c.Abort()
			return
		}
		// 访客 Key 只能调模型转发端点：/api/* 下的一切（包括 /api/v1/apikey/login|stats，
		// 也就是控制台的「用 Key 登录」）一律 403。管理接口本来就只认 JWT / 管理员令牌（Auth()），
		// 这里是把 APIKeyAuth 自己挂着的那几条 /api/ 路由也关上。
		if apiKeyObj.Guest && isAPIManagementPath(c.Request.URL.Path) {
			resp.ErrorWithCode(c, http.StatusForbidden, "guest_key_forbidden", "guest key can only call model endpoints")
			c.Abort()
			return
		}
		owner, err := op.UserGet(apiKeyObj.UserID)
		if err != nil || !owner.IsActive() {
			resp.Error(c, http.StatusUnauthorized, "API key owner is disabled")
			c.Abort()
			return
		}
		if err := op.GuestKeyAuthCheck(apiKeyObj, owner); err != nil {
			resp.Error(c, http.StatusUnauthorized, "guest key is not usable: "+err.Error())
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
		// 长连接（/v1/realtime）会话中途要判断"剩下的还够不够下一份"，而这笔在途预留是它自己占的，
		// 不能算成别人的占用；把金额交给下游，见 relay.grokRealtimeBiller。
		c.Set(QuotaReservationContextKey, reservation)
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
		endpointFamily := apiKeyEndpointFamilyForRequest(c.Request.Method, c.Request.URL.Path)
		if endpointFamily != "" && !apiKeyObj.AllowsEndpointFamily(endpointFamily) {
			resp.ErrorWithCode(c, http.StatusForbidden, "endpoint_not_allowed", "API key is not allowed to access "+string(endpointFamily)+" endpoints")
			c.Abort()
			return
		}
		c.Set("request_type", requestType)
		c.Set("endpoint_family", string(endpointFamily))
		c.Set("supported_models", apiKeyObj.SupportedModels)
		// 统一任务查询（GET /v1/tasks*）跨族：中间件不按族拦，由 handler 按这份清单只给 key 能看的任务类型。
		c.Set(APIKeyEndpointFamiliesContextKey, apiKeyObj.EffectiveEndpointFamilies())
		c.Set("api_key_id", apiKeyObj.ID)
		clientIP := ResolveClientIP(c)
		c.Set("request_ip", clientIP)
		// 用户页「客户端指纹」：只读入站请求（头名顺序 / UA / 非秘密身份头 / 反代转交的 JA3/JA4），
		// 只动内存。不改 c.Request、不读 body，出站请求与它无关。
		// 访客 Key 也记：归属是管理员（user_id = 管理员），按 api_key_id 区分是哪把访客 Key。
		op.UserClientRecord(owner.ID, apiKeyObj.ID, clientIP, telemetry.Capture(c.Request, PeerIsTrustedProxy(c)), 0)
		if apiKeyObj.Guest {
			// 访客 Key 的归属是管理员（计费、日志、IP 记录都按 user_id = 管理员走，api_key_id 区分是哪把访客 Key），
			// 但上下文里的「当前用户」降成非管理员：任何下游用 CurrentUserIsAdmin 判权限的地方都拿不到管理员身份。
			c.Set(model.APIKeyGuestContextKey, true)
			SetCurrentUser(c, guestContextUser(owner))
		} else {
			SetCurrentUser(c, owner)
		}
		c.Set("user_id", owner.ID)
		c.Next()
	}
}

// GuestKeyRole 是访客 Key 请求在上下文里的角色（不是可分配的用户角色，库里没有这个值）。
const GuestKeyRole model.UserRole = "guest_key"

func guestContextUser(owner model.User) model.User {
	u := owner
	u.Role = GuestKeyRole
	u.Password = ""
	return u
}

// IsGuestKeyRequest：这次请求是不是访客 Key。
func IsGuestKeyRequest(c *gin.Context) bool {
	return c.GetBool(model.APIKeyGuestContextKey)
}

// isAPIManagementPath：/api/ 开头的都是控制台 / 管理 / 自助接口，不是模型转发。
func isAPIManagementPath(path string) bool {
	path = strings.TrimSpace(path)
	return path == "/api" || strings.HasPrefix(path, "/api/")
}

// midjourneyClientKey 只在 /mj/ 路径上认 mj-api-secret 头，照 new-api middleware/auth.go:559：
// Authorization 为空、或是 midjourney-proxy 客户端惯用的占位值 "midjourney-proxy" 时，
// 改从 mj-api-secret 取 key。Authorization 带着真 key 时仍以它为准（返回 false，走原分支）。
func midjourneyClientKey(r *http.Request) (string, bool) {
	if r == nil || !strings.HasPrefix(r.URL.Path, "/mj/") {
		return "", false
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(auth) >= 7 && strings.EqualFold(auth[:7], "Bearer ") {
		auth = strings.TrimSpace(auth[7:])
	}
	if auth != "" && auth != "midjourney-proxy" {
		return "", false
	}
	key := strings.TrimSpace(r.Header.Get("mj-api-secret"))
	if len(key) >= 7 && strings.EqualFold(key[:7], "Bearer ") {
		key = strings.TrimSpace(key[7:])
	}
	if key == "" {
		return "", false
	}
	return key, true
}

// realtimeSubprotocolKey 只在 /v1/realtime 上、且没带 Authorization 时，从 WebSocket 子协议里取 key。
// 浏览器的 WebSocket 不能带自定义头，只能把凭证放进 Sec-WebSocket-Protocol：
//   - openai-insecure-api-key.<key>：OpenAI SDK 浏览器写法，new-api middleware/auth.go
//     apiKeyFromWebSocketSubprotocol 同款；
//   - xai-client-secret.<key>：xAI 文档的浏览器写法（上游那边放的是临时 token，到 octopus
//     这里放的是 octopus 的 key）。
//
// Authorization 带着值时以它为准（返回 false），存量鉴权顺序不变。
func realtimeSubprotocolKey(r *http.Request) (string, bool) {
	if r == nil || r.URL.Path != "/v1/realtime" {
		return "", false
	}
	if strings.TrimSpace(r.Header.Get("Authorization")) != "" {
		return "", false
	}
	for _, value := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, part := range strings.Split(value, ",") {
			part = strings.TrimSpace(part)
			for _, prefix := range []string{"openai-insecure-api-key.", "xai-client-secret."} {
				if strings.HasPrefix(part, prefix) {
					if key := strings.TrimSpace(strings.TrimPrefix(part, prefix)); key != "" {
						return key, true
					}
				}
			}
		}
	}
	return "", false
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

// QuotaReservationContextKey 存本次请求在 UserCanRelay 里占的在途预留金额（float64）。
// 与 relay.quotaReservationContextKey 同值（relay 测试里有断言）。
const QuotaReservationContextKey = "quota_reservation"

// APIKeyEndpointFamiliesContextKey 与 relay.APIKeyEndpointFamiliesContextKey 同值（relay 测试里有断言）。
const APIKeyEndpointFamiliesContextKey = "api_key_endpoint_families"

// apiKeyEndpointFamilyForRequest 在路径分类之上只多一条按方法的例外：
// 统一任务接口的**查询**（GET /v1/tasks、GET /v1/tasks/:id）一次覆盖视频 / 图片 / MJ 三类任务，
// 不属于任何单一族；中间件不拦（返回 ""），由 relay.TaskGetHandler / TaskListHandler 按 key 的族
// 过滤可见的任务类型（只开 video 族的 key 只看得到视频任务，依此类推）。提交仍按路径分族。
func apiKeyEndpointFamilyForRequest(method, path string) model.APIKeyEndpointFamily {
	if method == http.MethodGet && isUnifiedTaskQueryPath(strings.TrimSpace(path)) {
		return ""
	}
	return apiKeyEndpointFamilyForPath(path)
}

func isUnifiedTaskQueryPath(path string) bool {
	if path == "/v1/tasks" {
		return true
	}
	rest, ok := strings.CutPrefix(path, "/v1/tasks/")
	return ok && rest != "" && !strings.Contains(rest, "/")
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
	// new-api 的视频路由别名（/v1/video/generations[/:task_id]）与统一任务接口的视频提交
	// （POST /v1/tasks/video[-generations|-edits|-extensions]）同属视频族。
	if strings.HasPrefix(path, "/v1/video/") || strings.HasPrefix(path, "/v1/tasks/video") {
		return model.APIKeyEndpointFamilyVideo
	}
	// 统一任务接口的其余提交（image / midjourney）与 /v1/images/*、/mj/* 同族。
	// 查询的 GET 例外见 apiKeyEndpointFamilyForRequest。
	if path == "/v1/tasks" || strings.HasPrefix(path, "/v1/tasks/") {
		return model.APIKeyEndpointFamilyOpenAICompatible
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
		strings.HasPrefix(path, "/v1/audio/") ||
		// xAI（Grok）语音 REST。与 /v1/audio/* 同族：同是音频、同走 raw 透传、
		// 同一个上游账号。
		path == "/v1/tts" ||
		path == "/v1/stt" ||
		// xAI（Grok）实时语音 WebSocket（handlers/realtime.go）。同是语音、同一个上游账号。
		path == "/v1/realtime" ||
		// Midjourney（midjourney-proxy 协议，见 handlers/midjourney.go）。与 /v1/images/* 同族：
		// 同是出图、同按次计费、同走 OpenAI 兼容渠道；单开 family 要动前端勾选框和迁移。
		strings.HasPrefix(path, "/mj/") {
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
