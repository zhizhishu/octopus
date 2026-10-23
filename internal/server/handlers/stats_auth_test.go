package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/server/router"
)

// 这六个统计接口曾经**没有任何鉴权中间件**：server.go 里没有全局鉴权，
// 所以不带任何凭证 GET /api/v1/stats/today 就能拿到全站 token 用量、成本、
// 模型排名、渠道健康度 —— 客户规模和上游成本结构对公网裸奔。
//
// 同文件里 /apikey 挂了 Auth，/user-rank 还挂了 Auth+AdminOnly，唯独这六个漏了。
//
// 这条测试用真实请求验证：未登录必须被拒。它同时守住回归 ——
// 谁把 Use(middleware.Auth()) 删掉，这里立刻红。
func TestStatsRoutesRequireAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()

	// RegisterAll 曾把 registeredRouters 置 nil（"用后即清"），导致同进程里
	// 第二个 engine 拿到 0 条路由、所有路径 404 —— 同包 codex_routes_test.go
	// 也调它，谁先跑谁拿到路由，后跑的测试断言什么都与真实行为无关。
	// 该副作用已修（路由描述表不该被消费掉），这里直接建 engine。
	if err := router.RegisterAll(engine); err != nil {
		t.Fatalf("register routes: %v", err)
	}
	if len(engine.Routes()) == 0 {
		t.Fatal("RegisterAll 之后 engine 里一条路由都没有：路由描述表被消费了，" +
			"下面所有断言都会退化成 404，与鉴权无关")
	}

	paths := []string{
		"/api/v1/stats/today",
		"/api/v1/stats/daily",
		"/api/v1/stats/hourly",
		"/api/v1/stats/total",
		"/api/v1/stats/model-health",
		"/api/v1/stats/model-rank",
	}
	for _, p := range paths {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)

		if rec.Code == http.StatusOK {
			t.Errorf("GET %s 未登录返回 200：该接口没有任何鉴权，公网可读全站用量与成本"+
				"（body 前 200 字节: %.200s）", p, rec.Body.String())
			continue
		}
		// middleware.Auth() 对「没带 Authorization 头」的请求按项目既有约定返回
		// 400 ErrBadRequest（见 middleware/auth.go:19-22），不是 401。
		// 这里只断言「不是 200」，避免把项目的状态码风格当成本次回归的判据。
		if rec.Code != http.StatusBadRequest && rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
			t.Errorf("GET %s 未登录返回 %d，期望被拒（400/401/403）", p, rec.Code)
		}
	}
}
