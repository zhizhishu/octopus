package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/server/router"
)

// 这张表是**契约**：10 条路由、方法、路径都要逐字对上。
//
// 【为什么把它写成测试而不是注释】gin 的路由树里，同层级的字面量段与参数段会抢位置：
// `/images/batches/models` 若注册在 `/images/batches/:id` 之后，`models` 会被当成
// 一个批号，models 列表永远 404。这种错误在 review 时看不出来（两个 AddRoute 长得一模一样），
// 只有真起一个 engine 才暴露。所以这里真起 engine。
func TestBatchImageRoutesAreRegisteredExactly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	if err := router.RegisterAll(engine); err != nil {
		t.Fatalf("register routes: %v", err)
	}

	want := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/v1/images/batches"},
		{http.MethodGet, "/v1/images/batches"},
		{http.MethodGet, "/v1/images/batches/models"},
		{http.MethodGet, "/v1/images/batches/:id"},
		{http.MethodGet, "/v1/images/batches/:id/items"},
		{http.MethodGet, "/v1/images/batches/:id/items/:custom_id/content"},
		{http.MethodGet, "/v1/images/batches/:id/download"},
		{http.MethodPost, "/v1/images/batches/:id/cancel"},
		{http.MethodDelete, "/v1/images/batches/:id"},
		{http.MethodDelete, "/v1/images/batches/:id/outputs"},
	}

	have := make(map[string]bool, len(engine.Routes()))
	for _, route := range engine.Routes() {
		have[route.Method+" "+route.Path] = true
	}

	for _, expected := range want {
		key := expected.method + " " + expected.path
		if !have[key] {
			t.Errorf("route %s is not registered (have %d routes total)", key, len(engine.Routes()))
		}
	}

	// 反向：不许有多余的批处理路由。多一条往往意味着有人加了别名却没想清楚语义。
	registered := 0
	for _, route := range engine.Routes() {
		if strings.HasPrefix(route.Path, "/v1/images/batches/") {
			registered++
		}
		if route.Path == "/v1/images/batches" {
			registered++
		}
	}
	if registered != len(want) {
		t.Errorf("registered %d batch image routes, want exactly %d", registered, len(want))
	}
}

// models 必须真的**匹配到 models handler**，而不是被 `:id` 吃掉。
//
// 【怎么区分两者】两者都会返回非 404，所以看错误体：
// 无认证时两条都会被 APIKeyAuth 拦成 401，分不出来。所以这里看的是**路由能匹配**
// 这件事本身——用一个不存在的批号请求 `/images/batches/models` 与 `/images/batches/xxx`，
// 两者若都被 `:id` 吃掉，gin 的 c.Param("id") 会分别是 "models" 和 "xxx"。
func TestBatchImageModelsIsNotSwallowedByIDParam(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()

	var seenParam string
	var matchedPath string
	engine.GET("/v1/images/batches/models", func(c *gin.Context) {
		seenParam = c.Param("id")
		matchedPath = c.FullPath()
		c.Status(http.StatusOK)
	})
	engine.GET("/v1/images/batches/:id", func(c *gin.Context) {
		seenParam = c.Param("id")
		matchedPath = c.FullPath()
		c.Status(http.StatusOK)
	})

	request, _ := http.NewRequest(http.MethodGet, "/v1/images/batches/models", nil)
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)

	if matchedPath != "/v1/images/batches/models" {
		t.Fatalf("GET /v1/images/batches/models matched %q (id param = %q), want the models route",
			matchedPath, seenParam)
	}
	if seenParam != "" {
		t.Fatalf("models route was matched with id=%q, want no id param", seenParam)
	}
}
