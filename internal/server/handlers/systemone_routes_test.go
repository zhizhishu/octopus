package handlers

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/server/router"
)

// TestSystemoneRouteIsRegistered 钉住决策类端点 /v1/systemone 的注册事实。
//
// 【为什么要真起 engine】这条路由挂在 `/v1` 组下，和 `/v1/chat/completions`、
// `/v1/rerank`、`/v1/messages` 这些同层字面量段共处一棵路由树。漏注册、
// 被别组同名路由覆盖、或误注册成 GET，在代码 review 里都看不出来 ——
// 只有 engine 真的把路由表建出来才能发现。客户端上线后拿到 404 才发现就太晚了。
//
// 反向也要查：不允许出现第二个 /v1/systemone 变体（比如有人加了 /v1/systemone/
// 或 /v1/systemone/decide 这种别名却没确定语义），一条就是一条。
func TestSystemoneRouteIsRegistered(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	if err := router.RegisterAll(engine); err != nil {
		t.Fatalf("register routes: %v", err)
	}

	const wantMethod = http.MethodPost
	const wantPath = "/v1/systemone"

	found := 0
	for _, route := range engine.Routes() {
		if route.Path == wantPath {
			found++
			if route.Method != wantMethod {
				t.Errorf("route %s registered as %s, want %s", wantPath, route.Method, wantMethod)
			}
		}
	}
	if found == 0 {
		t.Fatalf("route %s %s is not registered (have %d routes total)", wantMethod, wantPath, len(engine.Routes()))
	}
	if found > 1 {
		t.Errorf("route %s is registered %d times, want exactly 1", wantPath, found)
	}

	// 反向：不许有 /v1/systemone 的其它变体。
	for _, route := range engine.Routes() {
		if route.Path == wantPath {
			continue
		}
		if len(route.Path) > len(wantPath) && route.Path[:len(wantPath)+1] == wantPath+"/" {
			t.Errorf("unexpected systemone sub-route registered: %s %s (decide the semantics first)", route.Method, route.Path)
		}
	}
}
