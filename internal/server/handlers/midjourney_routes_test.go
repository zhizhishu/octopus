package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/server/middleware"
	"github.com/zhizhishu/octopus/internal/server/router"
)

// Midjourney 路由走真实注册的路由表 + 真实 APIKeyAuth：
//   - 15 条路由（new-api router/relay-router.go:228-243 去掉注释掉的 notify）全部注册；
//   - 归 openai-compatible 族；只许 anthropic 的 key → 403 endpoint_not_allowed；
//   - mj-api-secret 头只在 /mj/ 下认（Authorization 为空或是占位值 midjourney-proxy 时），
//     /v1 端点上不认；Authorization 带真 key 时以它为准。
func TestMidjourneyRoutesAuthAndFamily(t *testing.T) {
	gin.SetMode(gin.TestMode)
	routes := []struct{ method, path, concrete string }{
		{http.MethodPost, "/mj/submit/action", ""},
		{http.MethodPost, "/mj/submit/shorten", ""},
		{http.MethodPost, "/mj/submit/modal", ""},
		{http.MethodPost, "/mj/submit/imagine", ""},
		{http.MethodPost, "/mj/submit/change", ""},
		{http.MethodPost, "/mj/submit/simple-change", ""},
		{http.MethodPost, "/mj/submit/describe", ""},
		{http.MethodPost, "/mj/submit/blend", ""},
		{http.MethodPost, "/mj/submit/edits", ""},
		{http.MethodPost, "/mj/submit/video", ""},
		{http.MethodPost, "/mj/submit/upload-discord-images", ""},
		{http.MethodPost, "/mj/insight-face/swap", ""},
		{http.MethodGet, "/mj/task/:id/fetch", "/mj/task/123/fetch"},
		{http.MethodGet, "/mj/task/:id/image-seed", "/mj/task/123/image-seed"},
		{http.MethodPost, "/mj/task/list-by-condition", ""},
	}

	ctx := setupGeminiModelsDB(t)
	user := createGeminiModelsUser(t, ctx)
	anthropicKey := createVideosFamilyAPIKey(t, ctx, user, "anthropic only", "sk-octopus-mj-anthropic", []model.APIKeyEndpointFamily{
		model.APIKeyEndpointFamilyAnthropic,
	})
	openaiKey := createVideosFamilyAPIKey(t, ctx, user, "openai only", "sk-octopus-mj-openai", []model.APIKeyEndpointFamily{
		model.APIKeyEndpointFamilyOpenAICompatible,
	})

	engine := gin.New()
	if err := router.RegisterAll(engine); err != nil {
		t.Fatalf("register routes: %v", err)
	}
	registered := map[string]bool{}
	for _, r := range engine.Routes() {
		registered[r.Method+" "+r.Path] = true
	}

	do := func(method, path string, headers map[string]string) *httptest.ResponseRecorder {
		var body *strings.Reader
		if method == http.MethodPost {
			body = strings.NewReader(`{"prompt":"x","ids":["1"]}`)
		} else {
			body = strings.NewReader("")
		}
		req := httptest.NewRequest(method, path, body)
		req.Header.Set("Content-Type", "application/json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)
		return rec
	}
	passedAuth := func(rec *httptest.ResponseRecorder) bool {
		// 测试库没有渠道，handler 自己会回 4xx/5xx（如 model not found / task_not_found），
		// 但不是鉴权的 401/403，也不是路由表的 404 page not found。
		return rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden &&
			!strings.Contains(rec.Body.String(), "404 page not found")
	}

	for _, r := range routes {
		if !registered[r.method+" "+r.path] {
			t.Fatalf("%s %s is not registered", r.method, r.path)
		}
		if got := middleware.EndpointFamilyForPath(r.path); got != model.APIKeyEndpointFamilyOpenAICompatible {
			t.Fatalf("%s family = %q, want openai-compatible", r.path, got)
		}
		path := r.path
		if r.concrete != "" {
			path = r.concrete
		}

		if rec := do(r.method, path, nil); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s without key: got %d, want 401", path, rec.Code)
		}
		if rec := do(r.method, path, map[string]string{"mj-api-secret": "sk-octopus-not-a-real-key"}); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s with unknown mj-api-secret: got %d, want 401", path, rec.Code)
		}
		rec := do(r.method, path, map[string]string{"mj-api-secret": anthropicKey.APIKey})
		var errBody struct {
			ErrorCode string `json:"error_code"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &errBody)
		if rec.Code != http.StatusForbidden || errBody.ErrorCode != "endpoint_not_allowed" {
			t.Fatalf("%s with anthropic-only key: got %d %.200s, want 403 endpoint_not_allowed", path, rec.Code, rec.Body.String())
		}

		for name, headers := range map[string]map[string]string{
			"mj-api-secret":               {"mj-api-secret": openaiKey.APIKey},
			"Bearer mj-api-secret":        {"mj-api-secret": "Bearer " + openaiKey.APIKey},
			"midjourney-proxy+secret":     {"Authorization": "Bearer midjourney-proxy", "mj-api-secret": openaiKey.APIKey},
			"Authorization wins":          {"Authorization": "Bearer " + openaiKey.APIKey, "mj-api-secret": "garbage"},
			"plain Authorization (no mj)": {"Authorization": "Bearer " + openaiKey.APIKey},
		} {
			if rec := do(r.method, path, headers); !passedAuth(rec) {
				t.Fatalf("%s via %s rejected: %d %.200s", path, name, rec.Code, rec.Body.String())
			}
		}
		// Authorization 带着一个错 key：不能退回 mj-api-secret 放行。
		if rec := do(r.method, path, map[string]string{"Authorization": "Bearer sk-octopus-wrong", "mj-api-secret": openaiKey.APIKey}); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: a wrong Authorization key must not fall back to mj-api-secret, got %d", path, rec.Code)
		}
	}

	// mj-api-secret 只在 /mj/ 下认。
	if rec := do(http.MethodPost, "/v1/chat/completions", map[string]string{"mj-api-secret": openaiKey.APIKey}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("mj-api-secret must not authenticate /v1 endpoints, got %d", rec.Code)
	}
}
