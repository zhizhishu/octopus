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

// Grok 语音两条端点走真实注册的路由 + 真实 APIKeyAuth：
//   - 归 openai-compatible 族（与 /v1/audio/* 同族）；
//   - 只许 anthropic 的 key → 403 endpoint_not_allowed；
//   - 没带 key / 错 key → 401；
//   - openai-compatible key 能进 handler（测试库没有渠道，handler 自己报错，但不是 403/路由 404）。
func TestGrokVoiceRoutesAuthAndFamily(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, path := range []string{"/v1/tts", "/v1/stt"} {
		if got := middleware.EndpointFamilyForPath(path); got != model.APIKeyEndpointFamilyOpenAICompatible {
			t.Fatalf("%s family = %q, want openai-compatible", path, got)
		}
	}

	ctx := setupGeminiModelsDB(t)
	user := createGeminiModelsUser(t, ctx)
	anthropicKey := createVideosFamilyAPIKey(t, ctx, user, "anthropic only", "sk-octopus-grok-voice-anthropic", []model.APIKeyEndpointFamily{
		model.APIKeyEndpointFamilyAnthropic,
	})
	openaiKey := createVideosFamilyAPIKey(t, ctx, user, "openai only", "sk-octopus-grok-voice-openai", []model.APIKeyEndpointFamily{
		model.APIKeyEndpointFamilyOpenAICompatible,
	})

	engine := gin.New()
	if err := router.RegisterAll(engine); err != nil {
		t.Fatalf("register routes: %v", err)
	}

	do := func(path, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"text":"hi","language":"en"}`))
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)
		return rec
	}

	for _, path := range []string{"/v1/tts", "/v1/stt"} {
		if rec := do(path, ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s without key: got %d, want 401", path, rec.Code)
		}
		if rec := do(path, "sk-octopus-not-a-real-key"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s with unknown key: got %d, want 401", path, rec.Code)
		}

		rec := do(path, anthropicKey.APIKey)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s with anthropic-only key: got %d body %.200s, want 403", path, rec.Code, rec.Body.String())
		}
		var body struct {
			ErrorCode string `json:"error_code"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if body.ErrorCode != "endpoint_not_allowed" {
			t.Fatalf("%s with anthropic-only key: error_code %q", path, body.ErrorCode)
		}

		rec = do(path, openaiKey.APIKey)
		if rec.Code == http.StatusForbidden || rec.Code == http.StatusUnauthorized {
			t.Fatalf("%s with openai-compatible key rejected by auth: %d %.200s", path, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "404 page not found") {
			t.Fatalf("%s is not registered", path)
		}
	}
}
