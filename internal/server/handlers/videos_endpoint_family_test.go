package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/server/router"
)

// 这条测试走**真实注册的路由**（videos.go 的 init() → router.RegisterAll），
// 不是自己在测试里手写一条 /v1/videos。它守两件事：
//  1. /v1/videos 真的被挂上了 APIKeyAuth 且归类到 video family —— 只许 chat 的
//     key 必须被拒（视频按次计费，放行就是直接花钱）。
//  2. 路由路径与中间件的分类表不会各改各的（改了路径忘了改分类，这里立刻红）。
func TestVideosRoutesBlockChatOnlyKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupGeminiModelsDB(t)
	user := createGeminiModelsUser(t, ctx)

	chatKey := createVideosFamilyAPIKey(t, ctx, user, "chat only", "sk-octopus-videos-chat-only", []model.APIKeyEndpointFamily{
		model.APIKeyEndpointFamilyOpenAICompatible,
	})
	videoKey := createVideosFamilyAPIKey(t, ctx, user, "video only", "sk-octopus-videos-only", []model.APIKeyEndpointFamily{
		model.APIKeyEndpointFamilyVideo,
	})

	engine := gin.New()
	if err := router.RegisterAll(engine); err != nil {
		t.Fatalf("register routes: %v", err)
	}
	if len(engine.Routes()) == 0 {
		t.Fatal("RegisterAll 之后 engine 里一条路由都没有，下面的断言会退化成 404")
	}

	// chat-only key：创建与轮询都必须 403 endpoint_not_allowed（中间件在 handler 之前中断）。
	for _, tc := range []struct {
		method string
		path   string
	}{
		{method: http.MethodPost, path: "/v1/videos"},
		{method: http.MethodGet, path: "/v1/videos/task-abc"},
	} {
		rec := doVideosRequest(engine, tc.method, tc.path, chatKey.APIKey)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s with chat-only key: expected 403, got %d body %.200s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
		var body struct {
			ErrorCode string `json:"error_code"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode body %.200s: %v", rec.Body.String(), err)
		}
		if body.ErrorCode != "endpoint_not_allowed" {
			t.Fatalf("%s %s with chat-only key: error_code got %q want endpoint_not_allowed", tc.method, tc.path, body.ErrorCode)
		}
	}

	// video family key：鉴权必须放行。handler 之后可能因为测试库里没有渠道/方案而
	// 报错，但绝不能是 family 拒绝 —— 否则等于视频端点对所有人都不可用。
	for _, tc := range []struct {
		method string
		path   string
	}{
		{method: http.MethodPost, path: "/v1/videos"},
		{method: http.MethodGet, path: "/v1/videos/task-abc"},
	} {
		rec := doVideosRequest(engine, tc.method, tc.path, videoKey.APIKey)
		body := rec.Body.String()
		if rec.Code == http.StatusForbidden {
			t.Fatalf("%s %s with video key: family 校验把视频 key 也拒了，body %.200s", tc.method, tc.path, body)
		}
		// gin 自己没匹配到路由时才会回这串；handler 内部的 404（model not found /
		// unknown video task）会带 JSON 错误体。用它区分"路由没注册"和"真的进了 handler"。
		if strings.Contains(body, "404 page not found") {
			t.Fatalf("%s %s with video key: 路由没注册上，这条断言没测到鉴权", tc.method, tc.path)
		}
		t.Logf("%s %s with video key: %d body %.120s", tc.method, tc.path, rec.Code, body)
	}
}

func createVideosFamilyAPIKey(t *testing.T, ctx context.Context, user model.User, name string, secret string, families []model.APIKeyEndpointFamily) model.APIKey {
	t.Helper()
	key := model.APIKey{
		UserID:           user.ID,
		Name:             name,
		APIKey:           secret,
		Enabled:          true,
		EndpointFamilies: families,
	}
	if err := op.APIKeyCreate(&key, ctx); err != nil {
		t.Fatalf("create api key %q: %v", name, err)
	}
	return key
}

func doVideosRequest(engine *gin.Engine, method string, path string, apiKey string) *httptest.ResponseRecorder {
	body := strings.NewReader(`{"model":"sora-2"}`)
	req := httptest.NewRequest(method, path, body)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec
}
