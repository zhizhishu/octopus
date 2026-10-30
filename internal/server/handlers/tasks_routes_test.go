package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/relay"
	"github.com/zhizhishu/octopus/internal/server/middleware"
	"github.com/zhizhishu/octopus/internal/server/router"
)

// 统一任务接口走真实注册的路由 + 真实 APIKeyAuth：
//   - 提交按路径分族：/v1/tasks/video* 与 /v1/video/* 归 video，其余 /v1/tasks/* 归 openai-compatible；
//   - 查询（GET /v1/tasks、GET /v1/tasks/:id）中间件不拦，handler 按 key 的族只给能看的任务类型；
//   - 列表只含这把 key 自己的任务。
func TestUnifiedTaskRoutesFamiliesAndScoping(t *testing.T) {
	gin.SetMode(gin.TestMode)
	if middleware.APIKeyEndpointFamiliesContextKey != relay.APIKeyEndpointFamiliesContextKey {
		t.Fatal("middleware and relay disagree on the endpoint-families context key")
	}
	for path, want := range map[string]model.APIKeyEndpointFamily{
		"/v1/tasks/video":                 model.APIKeyEndpointFamilyVideo,
		"/v1/tasks/video-edits":           model.APIKeyEndpointFamilyVideo,
		"/v1/video/generations":           model.APIKeyEndpointFamilyVideo,
		"/v1/video/generations/abc":       model.APIKeyEndpointFamilyVideo,
		"/v1/tasks/image":                 model.APIKeyEndpointFamilyOpenAICompatible,
		"/v1/tasks/midjourney":            model.APIKeyEndpointFamilyOpenAICompatible,
		"/v1/tasks":                       model.APIKeyEndpointFamilyOpenAICompatible,
		"/v1/tasksx":                      model.APIKeyEndpointFamilyUnclassified,
		"/v1/videos/generations/whatever": model.APIKeyEndpointFamilyVideo,
	} {
		if got := middleware.EndpointFamilyForPath(path); got != want {
			t.Fatalf("%s family = %q, want %q", path, got, want)
		}
	}

	ctx := setupGeminiModelsDB(t)
	user := createGeminiModelsUser(t, ctx)
	videoKey := createVideosFamilyAPIKey(t, ctx, user, "video only", "sk-octopus-tasks-video", []model.APIKeyEndpointFamily{model.APIKeyEndpointFamilyVideo})
	openaiKey := createVideosFamilyAPIKey(t, ctx, user, "openai only", "sk-octopus-tasks-openai", []model.APIKeyEndpointFamily{model.APIKeyEndpointFamilyOpenAICompatible})

	row := model.VideoTask{UpstreamTaskID: "vid-route-1", OwnerTokenID: videoKey.ID, OwnerUserID: user.ID, Status: model.VideoTaskQueued, CreatedAt: 100}
	if err := op.VideoTaskCreate(ctx, &row); err != nil {
		t.Fatalf("video row: %v", err)
	}

	engine := gin.New()
	if err := router.RegisterAll(engine); err != nil {
		t.Fatalf("register routes: %v", err)
	}
	do := func(method, path, key, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)
		return rec
	}

	// 提交：族不对 → 403；族对 → 进 handler（测试库没有渠道，回的是 handler 自己的错误，不是 401/403）。
	if rec := do(http.MethodPost, "/v1/tasks/image", videoKey.APIKey, `{"model":"m","prompt":"p"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("video key image submit: %d", rec.Code)
	}
	if rec := do(http.MethodPost, "/v1/tasks/video", openaiKey.APIKey, `{"model":"m","prompt":"p"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("openai key video submit: %d", rec.Code)
	}
	if rec := do(http.MethodPost, "/v1/video/generations", openaiKey.APIKey, `{"model":"m","prompt":"p"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("openai key /v1/video/generations: %d", rec.Code)
	}
	for _, path := range []string{"/v1/tasks/video-generations", "/v1/video/generations"} {
		rec := do(http.MethodPost, path, videoKey.APIKey, `{"model":"m","prompt":"p"}`)
		if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden || strings.Contains(rec.Body.String(), "404 page not found") {
			t.Fatalf("%s should reach the handler: %d %s", path, rec.Code, rec.Body.String())
		}
	}

	// 查询：两把 key 都过中间件；只有创建它的 video key 看得见。
	rec := do(http.MethodGet, "/v1/tasks/vid-route-1", videoKey.APIKey, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"QUEUED"`) {
		t.Fatalf("owner fetch: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(http.MethodGet, "/v1/tasks/vid-route-1", openaiKey.APIKey, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("other key fetch: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(http.MethodGet, "/v1/video/generations/vid-route-1", videoKey.APIKey, ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"code":"success"`) {
		t.Fatalf("new-api video fetch: %d %s", rec.Code, rec.Body.String())
	}
	list := func(key string) (int, []string) {
		rec := do(http.MethodGet, "/v1/tasks", key, "")
		var out struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		var ids []string
		for _, d := range out.Data {
			ids = append(ids, d.ID)
		}
		return rec.Code, ids
	}
	if code, ids := list(videoKey.APIKey); code != http.StatusOK || len(ids) != 1 || ids[0] != "vid-route-1" {
		t.Fatalf("video key list: %d %v", code, ids)
	}
	if code, ids := list(openaiKey.APIKey); code != http.StatusOK || len(ids) != 0 {
		t.Fatalf("openai key list: %d %v", code, ids)
	}
	if rec := do(http.MethodGet, "/v1/tasks", "sk-octopus-nope", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad key: %d", rec.Code)
	}
}
