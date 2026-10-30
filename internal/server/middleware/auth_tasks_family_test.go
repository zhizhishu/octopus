package middleware

import (
	"net/http"
	"testing"

	"github.com/zhizhishu/octopus/internal/model"
)

// 只有统一任务接口的 GET 查询不按族拦（handler 自己按族过滤任务类型）；
// 提交与其它路径保持路径分族，未登记的路径仍 fail-closed。
func TestAPIKeyEndpointFamilyForRequestUnifiedTasks(t *testing.T) {
	cases := []struct {
		method, path string
		want         model.APIKeyEndpointFamily
	}{
		{http.MethodGet, "/v1/tasks", ""},
		{http.MethodGet, "/v1/tasks/vid_123", ""},
		{http.MethodGet, "/v1/tasks/video-looking-id", ""},
		{http.MethodGet, "/v1/tasks/a/b", model.APIKeyEndpointFamilyOpenAICompatible},
		{http.MethodPost, "/v1/tasks/video", model.APIKeyEndpointFamilyVideo},
		{http.MethodPost, "/v1/tasks/image", model.APIKeyEndpointFamilyOpenAICompatible},
		{http.MethodPost, "/v1/tasks", model.APIKeyEndpointFamilyOpenAICompatible},
		{http.MethodGet, "/v1/video/generations/x", model.APIKeyEndpointFamilyVideo},
		{http.MethodGet, "/v1/chat/completions", model.APIKeyEndpointFamilyOpenAICompatible},
		{http.MethodGet, "/v1/unknown", model.APIKeyEndpointFamilyUnclassified},
	}
	for _, tc := range cases {
		if got := apiKeyEndpointFamilyForRequest(tc.method, tc.path); got != tc.want {
			t.Fatalf("%s %s = %q, want %q", tc.method, tc.path, got, tc.want)
		}
	}
}
