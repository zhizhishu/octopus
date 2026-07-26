package middleware

import (
	"net/http"
	"strings"
	"time"

	"github.com/bestruirui/octopus/internal/conf"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/server/auth"
	"github.com/bestruirui/octopus/internal/server/resp"
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
				SetCurrentUser(c, admin)
				c.Next()
				return
			}
			resp.Error(c, http.StatusUnauthorized, resp.ErrUnauthorized)
			c.Abort()
			return
		}
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
		if err := op.UserCanRelay(owner.ID, c.Request.Context()); err != nil {
			resp.Error(c, http.StatusPaymentRequired, err.Error())
			c.Abort()
			return
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

func apiKeyEndpointFamilyForPath(path string) model.APIKeyEndpointFamily {
	path = strings.TrimSpace(path)
	if strings.HasPrefix(path, "/v1beta/") {
		return model.APIKeyEndpointFamilyGemini
	}
	if path == "/v1/messages" || path == "/v1/messages/count_tokens" {
		return model.APIKeyEndpointFamilyAnthropic
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
	return ""
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
