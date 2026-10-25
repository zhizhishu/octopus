package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/db"
	appmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/server/middleware"
	"github.com/zhizhishu/octopus/internal/server/router"
)

// TestSystemoneEndpointIsReachableThroughRealMiddleware 是这条端点**唯一**能证明
// 「客户端真的能用」的测试：它走完整的 gin engine + 真的 APIKeyAuth 中间件，
// 而不是像 relay 层的用例那样直接调 handler。
//
// 【为什么必须有这一条】handler 层和 relay 层的测试都会手动 `c.Set("api_key_id")`
// 把中间件整个跳过。于是「路由注册了、handler 逻辑也对、单测全绿」和「真实请求
// 拿到 403」可以同时成立 —— 这个端点第一版就是这样：middleware 的端点族闸门
// （internal/server/middleware/auth.go 的 apiKeyEndpointFamilyForPath）不认识
// /v1/systemone，把它归成 Unclassified，而 AllowsEndpointFamily 对 Unclassified
// 恒 false，结果是所有 key 一律 403 endpoint_not_allowed。绕过中间件的测试一条都
// 抓不到，因为它们根本没经过那道闸门。
//
// 【断言什么】一个**只开了 openai-compatible 一个族**的 key（这是最典型的配置，
// 也是被闸门误伤的那种）打 /v1/systemone，必须被中间件放行，落到 handler。
// 这里让 handler 返回 204，就能把「中间件放行」和「handler 被调到」分开验证：
//
//	403 → 闸门没归类（bug 复现）
//	404 → 路由没注册
//	204 → 两道关都过了
func TestSystemoneEndpointIsReachableThroughRealMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)

	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "octopus.db"), false); err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() {
		// Windows 上无法 unlink 正在使用的 sqlite 文件，不关会让 TempDir
		// 清理失败、测试因为错误的原因变红。
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
	})
	if err := op.InitCache(); err != nil {
		t.Fatalf("init cache: %v", err)
	}
	ctx := context.Background()

	user, err := op.UserCreate(appmodel.UserCreateRequest{
		Username: "admin",
		Password: "password",
		Role:     appmodel.UserRoleAdmin,
		Status:   appmodel.UserStatusActive,
	}, ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	// 只开 openai-compatible：模拟管理员最常见的配法。如果闸门没归类，
	// 这个 key 会在中间件就被 403 掉，根本到不了 handler。
	key := appmodel.APIKey{
		UserID:  user.ID,
		Name:    "chat-only",
		APIKey:  "sk-" + "octopus" + "-systemone-reach-test",
		Enabled: true,
		EndpointFamilies: []appmodel.APIKeyEndpointFamily{
			appmodel.APIKeyEndpointFamilyOpenAICompatible,
		},
	}
	if err := op.APIKeyCreate(&key, ctx); err != nil {
		t.Fatalf("create api key: %v", err)
	}

	// 真 engine：先装一条 204 的探针路由，再用 RegisterAll 装全量路由。
	// 直接注册探针会盖掉真实 handler，所以这里改用一个独立 engine 只验证
	// 「中间件 + 路由可达」，真实 handler 的行为由 relay 层用例负责。
	engine := gin.New()
	engine.POST("/v1/systemone", middleware.APIKeyAuth(), func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key.APIKey)
	engine.ServeHTTP(rec, req)

	if rec.Code == http.StatusForbidden {
		var body struct {
			ErrorCode string `json:"error_code"`
			Message   string `json:"message"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		t.Fatalf("/v1/systemone was rejected by the endpoint-family gate (%s / %s): "+
			"add it to apiKeyEndpointFamilyForPath in internal/server/middleware/auth.go, "+
			"otherwise every API key gets 403 even though the route exists",
			body.ErrorCode, body.Message)
	}
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected /v1/systemone to pass APIKeyAuth, got %d body %s", rec.Code, rec.Body.String())
	}
}

// TestSystemoneRouteSurvivesFullRegistration 用真的 RegisterAll 建全量路由表，
// 确认 /v1/systemone 没被别的组或通配路由吃掉（例如未来加了 /v1/:something）。
//
// 和上面那条分开：上面验证中间件放行，这条验证路由表里真有一条 POST
// /v1/systemone。两条都在才算「客户端能用」。
func TestSystemoneRouteSurvivesFullRegistration(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	if err := router.RegisterAll(engine); err != nil {
		t.Fatalf("register routes: %v", err)
	}

	var matched *gin.RouteInfo
	for _, r := range engine.Routes() {
		if r.Path == "/v1/systemone" {
			cp := r
			matched = &cp
			break
		}
	}
	if matched == nil {
		t.Fatalf("POST /v1/systemone is missing from the full route table (%d routes)", len(engine.Routes()))
	}
	if matched.Method != http.MethodPost {
		t.Fatalf("/v1/systemone registered as %s, want POST", matched.Method)
	}
}
