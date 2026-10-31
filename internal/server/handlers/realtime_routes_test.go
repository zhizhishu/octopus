package handlers

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/server/middleware"
	"github.com/zhizhishu/octopus/internal/server/router"
)

// /v1/realtime 走真实注册的路由 + 真实 APIKeyAuth：
//   - 归 openai-compatible 族（与 /v1/tts、/v1/stt 同族）；
//   - 浏览器把 key 放在子协议里（openai-insecure-api-key.<key> / xai-client-secret.<key>）也能过鉴权；
//   - 没 key / 错 key → 401，anthropic-only key → 403；
//   - 测试库里没有渠道，过了鉴权后由 handler 自己回路由错误（不是 401/403，也不是 gin 的路由 404）。
func TestRealtimeRouteAuthAndFamily(t *testing.T) {
	gin.SetMode(gin.TestMode)
	if got := middleware.EndpointFamilyForPath("/v1/realtime"); got != model.APIKeyEndpointFamilyOpenAICompatible {
		t.Fatalf("/v1/realtime family = %q, want openai-compatible", got)
	}

	ctx := setupGeminiModelsDB(t)
	user := createGeminiModelsUser(t, ctx)
	anthropicKey := createVideosFamilyAPIKey(t, ctx, user, "anthropic only", "sk-octopus-realtime-anthropic", []model.APIKeyEndpointFamily{
		model.APIKeyEndpointFamilyAnthropic,
	})
	openaiKey := createVideosFamilyAPIKey(t, ctx, user, "openai only", "sk-octopus-realtime-openai", []model.APIKeyEndpointFamily{
		model.APIKeyEndpointFamilyOpenAICompatible,
	})

	engine := gin.New()
	if err := router.RegisterAll(engine); err != nil {
		t.Fatalf("register routes: %v", err)
	}
	srv := httptest.NewServer(engine)
	t.Cleanup(srv.Close)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/realtime?model=grok-voice-latest"

	lastBody := ""
	dial := func(header http.Header, subprotocols ...string) int {
		t.Helper()
		d := websocket.Dialer{Subprotocols: subprotocols}
		conn, resp, err := d.Dial(wsURL, header)
		if err == nil {
			_ = conn.Close()
			t.Fatalf("no channel configured, handshake must not succeed")
		}
		if resp == nil {
			t.Fatalf("no HTTP response: %v", err)
		}
		b, _ := io.ReadAll(resp.Body)
		lastBody = string(b)
		return resp.StatusCode
	}
	bearer := func(k string) http.Header { return http.Header{"Authorization": {"Bearer " + k}} }

	if got := dial(nil); got != http.StatusUnauthorized {
		t.Fatalf("no key: %d, want 401", got)
	}
	if got := dial(nil, "realtime", "openai-insecure-api-key.sk-octopus-nope"); got != http.StatusUnauthorized {
		t.Fatalf("unknown subprotocol key: %d, want 401", got)
	}
	if got := dial(bearer(anthropicKey.APIKey)); got != http.StatusForbidden {
		t.Fatalf("anthropic-only key: %d, want 403", got)
	}
	for name, try := range map[string]func() int{
		"authorization header":     func() int { return dial(bearer(openaiKey.APIKey)) },
		"openai insecure protocol": func() int { return dial(nil, "realtime", "openai-insecure-api-key."+openaiKey.APIKey) },
		"xai client secret":        func() int { return dial(nil, "xai-client-secret."+openaiKey.APIKey) },
	} {
		got := try()
		if got == http.StatusUnauthorized || got == http.StatusForbidden || strings.Contains(lastBody, "404 page not found") {
			t.Fatalf("%s: openai-compatible key should pass auth and reach the handler, got %d %s", name, got, lastBody)
		}
		if !strings.Contains(lastBody, "model not found") && !strings.Contains(lastBody, "channel") {
			t.Fatalf("%s: expected the handler's routing error (no channel for the model), got %d %s", name, got, lastBody)
		}
	}

	// 普通 GET（没有 Upgrade）过了鉴权也只能拿到 426。
	req := httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)
	req.Header.Set("Authorization", "Bearer "+openaiKey.APIKey)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusUpgradeRequired {
		t.Fatalf("plain GET: %d, want 426", rec.Code)
	}
}

// 用完断开的准入一侧：走真实 APIKeyAuth，余额为 0 的普通用户在升级前就被 402 挡下，
// 与其它端点同一道闸（op.UserCanRelay），不会先连上再断。
func TestRealtimeRouteRejectsZeroBalanceUser(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupGeminiModelsDB(t)
	_ = createGeminiModelsUser(t, ctx) // 第一个用户是管理员
	broke, err := op.UserCreate(model.UserCreateRequest{
		Username: "realtime-broke",
		Password: "password-123",
		Role:     model.UserRoleUser,
		Status:   model.UserStatusActive,
		Balance:  0,
	}, ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	key := createVideosFamilyAPIKey(t, ctx, broke, "broke", "sk-octopus-realtime-broke", []model.APIKeyEndpointFamily{
		model.APIKeyEndpointFamilyOpenAICompatible,
	})
	engine := gin.New()
	if err := router.RegisterAll(engine); err != nil {
		t.Fatalf("register routes: %v", err)
	}
	srv := httptest.NewServer(engine)
	t.Cleanup(srv.Close)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/realtime?model=grok-voice-latest"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Authorization": {"Bearer " + key.APIKey}})
	if err == nil {
		_ = conn.Close()
		t.Fatalf("zero balance must not get a websocket")
	}
	if resp == nil || resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("want 402, got resp=%v err=%v", resp, err)
	}
}
