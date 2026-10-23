package middleware

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/model"
)

// /v1/videos 曾经不在 apiKeyEndpointFamilyForPath 的分类表里：返回 "" 后中间件
// 的 `if endpointFamily != ""` 直接把校验跳过去，于是"只许 chat"的 key 也能
// 生成视频（异步按次计费，等于用别人的钱）。
// 下面这些用例都走真实中间件链路（gin 路由 + APIKeyAuth），不测纯函数。
func newVideoEndpointFamilyAuthRouter(t *testing.T) *gin.Engine {
	t.Helper()
	router := gin.New()
	handler := func(c *gin.Context) {
		// 下游 handler 拿到的 family 必须是 video，否则按 family 计费/筛选的逻辑会跑偏。
		if got := c.GetString("endpoint_family"); got != string(model.APIKeyEndpointFamilyVideo) {
			t.Errorf("endpoint_family: got %q want %q", got, model.APIKeyEndpointFamilyVideo)
		}
		c.Status(http.StatusNoContent)
	}
	router.POST("/v1/videos", APIKeyAuth(), handler)
	router.GET("/v1/videos/:id", APIKeyAuth(), handler)
	return router
}

func TestAPIKeyAuthVideosBlocksChatOnlyKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupMiddlewareAuthDB(t)
	user := createMiddlewareAuthUser(t, ctx)
	chatKey := createMiddlewareAuthAPIKey(t, ctx, user, "chat only", "sk-octopus-video-chat-only-test", []model.APIKeyEndpointFamily{
		model.APIKeyEndpointFamilyOpenAICompatible,
	})
	router := newVideoEndpointFamilyAuthRouter(t)

	for _, tc := range []struct {
		method string
		target string
	}{
		{method: http.MethodPost, target: "/v1/videos"},
		{method: http.MethodGet, target: "/v1/videos/task-abc"},
	} {
		rec := performAuthRequest(router, tc.method, tc.target, "Authorization", "Bearer "+chatKey.APIKey)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s: chat-only key must be forbidden, got %d body %s", tc.method, tc.target, rec.Code, rec.Body.String())
		}
		assertResponseErrorCode(t, rec, "endpoint_not_allowed")
	}
}

func TestAPIKeyAuthVideosAllowsVideoFamilyAndLegacyKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupMiddlewareAuthDB(t)
	user := createMiddlewareAuthUser(t, ctx)
	videoKey := createMiddlewareAuthAPIKey(t, ctx, user, "video only", "sk-octopus-video-only-test", []model.APIKeyEndpointFamily{
		model.APIKeyEndpointFamilyVideo,
	})
	legacyKey := createMiddlewareAuthAPIKey(t, ctx, user, "legacy", "sk-octopus-video-legacy-test", nil)
	router := newVideoEndpointFamilyAuthRouter(t)

	for _, key := range []model.APIKey{videoKey, legacyKey} {
		for _, tc := range []struct {
			method string
			target string
		}{
			{method: http.MethodPost, target: "/v1/videos"},
			{method: http.MethodGet, target: "/v1/videos/task-abc"},
		} {
			rec := performAuthRequest(router, tc.method, tc.target, "Authorization", "Bearer "+key.APIKey)
			if rec.Code != http.StatusNoContent {
				t.Fatalf("%s %s with key %q: expected pass, got %d body %s", tc.method, tc.target, key.Name, rec.Code, rec.Body.String())
			}
		}
	}
}

func TestAPIKeyEndpointFamilyForVideosIsVideo(t *testing.T) {
	for _, path := range []string{"/v1/videos", "/v1/videos/task-abc"} {
		if got := apiKeyEndpointFamilyForPath(path); got != model.APIKeyEndpointFamilyVideo {
			t.Fatalf("path %s endpoint family: got %q want %q", path, got, model.APIKeyEndpointFamilyVideo)
		}
	}
	// chat 仍是 openai-compatible，不能被视频改动带跑。
	if got := apiKeyEndpointFamilyForPath("/v1/chat/completions"); got != model.APIKeyEndpointFamilyOpenAICompatible {
		t.Fatalf("chat endpoint family: got %q want %q", got, model.APIKeyEndpointFamilyOpenAICompatible)
	}
}

// 未归类路径必须 fail-closed：新增 relay 端点忘了归类时不能自动放行。
// /api/ 下的密钥自助接口不属于任何 relay family，保持放行。
func TestAPIKeyAuthUnclassifiedPathFailsClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupMiddlewareAuthDB(t)
	user := createMiddlewareAuthUser(t, ctx)
	chatKey := createMiddlewareAuthAPIKey(t, ctx, user, "chat only", "sk-octopus-unclassified-test", []model.APIKeyEndpointFamily{
		model.APIKeyEndpointFamilyOpenAICompatible,
	})
	legacyKey := createMiddlewareAuthAPIKey(t, ctx, user, "legacy", "sk-octopus-unclassified-legacy-test", nil)

	router := gin.New()
	handler := func(c *gin.Context) { c.Status(http.StatusNoContent) }
	router.POST("/v1/not-yet-classified", APIKeyAuth(), handler)
	router.GET("/api/v1/apikey/stats", APIKeyAuth(), handler)

	for _, key := range []model.APIKey{chatKey, legacyKey} {
		rec := performAuthRequest(router, http.MethodPost, "/v1/not-yet-classified", "Authorization", "Bearer "+key.APIKey)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("unclassified relay path with key %q: expected forbidden, got %d body %s", key.Name, rec.Code, rec.Body.String())
		}
		// 管理/自助接口不受 family 约束。
		rec = performAuthRequest(router, http.MethodGet, "/api/v1/apikey/stats", "Authorization", "Bearer "+key.APIKey)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("/api/ management path with key %q: expected pass, got %d body %s", key.Name, rec.Code, rec.Body.String())
		}
	}
}
