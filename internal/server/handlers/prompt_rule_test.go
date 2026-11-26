package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/server/auth"
	"github.com/zhizhishu/octopus/internal/server/router"
)

// 端到端走 HTTP + 真中间件链：鉴权（401/403）、JSON 闸门、预览只读无网络。

func promptRuleRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	if err := router.RegisterAll(engine); err != nil {
		engine.POST("/__register_failed", func(c *gin.Context) {})
	}
	return engine
}

func promptRuleJWT(t *testing.T, role model.UserRole, id int) string {
	t.Helper()
	// VerifyJWTToken 用 op.UserGet(id) 从内存缓存回表验用户，签名 secret
	// （username+password）必须与 op 层那行一致。直接写 DB 行不进 userCache、
	// 也不哈希密码，鉴权中间件一直是 401 —— 照同目录已通过的 handler 测试
	// （stats_breakdown_test.go 的 dashFixture、redteam_lab_v2_test.go）：
	// InitCache 重载缓存后走 op.UserCreate 造登录态；username 带 role+id 只是
	// 为了幂等取或种（username 有唯一索引），行 ID 归 DB 自增，不回头改库。
	username := fmt.Sprintf("prompt-user-%d-%s", id, role)
	user, err := op.UserGetByUsername(username)
	if err != nil {
		user, err = op.UserCreate(model.UserCreateRequest{
			Username: username,
			Password: fmt.Sprintf("pw-%s-%d", role, id),
			Role:     role,
			Status:   model.UserStatusActive,
		}, context.Background())
		if err != nil {
			t.Fatalf("seed user %s: %v", username, err)
		}
	}
	token, _, err := auth.GenerateJWTToken(user, 15)
	if err != nil {
		t.Fatalf("mint jwt: %v", err)
	}
	return token
}

func promptRuleDo(t *testing.T, engine *gin.Engine, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" && method != http.MethodGet {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec
}

func promptRuleAdminDB(t *testing.T) {
	t.Helper()
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "octopus.db"), false); err != nil {
		t.Fatalf("init db: %v", err)
	}
	// 每个测试都是全新 sqlite：必须重载 op 层缓存，否则 userCache 里还是
	// 上一个测试的用户（或空），JWT 回表验不到（照 intervention_test.go 的
	// setupInterventionTestDB 同款顺序：InitDB → Close 兜底 → InitCache）。
	if err := op.InitCache(); err != nil {
		t.Fatalf("init cache: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
	})
}

// TestPromptRuleRoutesRegistered：/list 必须 GET，其余必须 POST，一条就是一条。
func TestPromptRuleRoutesRegistered(t *testing.T) {
	engine := promptRuleRouter()
	if engine == nil {
		t.Fatal("router registration failed")
	}
	want := map[string]string{
		"/api/v1/prompt-rule/list":    http.MethodGet,
		"/api/v1/prompt-rule/create":  http.MethodPost,
		"/api/v1/prompt-rule/update":  http.MethodPost,
		"/api/v1/prompt-rule/delete":  http.MethodPost,
		"/api/v1/prompt-rule/toggle":  http.MethodPost,
		"/api/v1/prompt-rule/reorder": http.MethodPost,
		"/api/v1/prompt-rule/preview": http.MethodPost,
	}
	for path, method := range want {
		found := 0
		for _, route := range engine.Routes() {
			if route.Path == path {
				found++
				if route.Method != method {
					t.Errorf("%s registered as %s, want %s", path, route.Method, method)
				}
			}
		}
		if found != 1 {
			t.Fatalf("%s registered %d times, want exactly 1", path, found)
		}
	}
}

// TestPromptRuleAuthGates：无认证 401，普通用户 403，GET 不强制 body。
func TestPromptRuleAuthGates(t *testing.T) {
	// admin 用户后续要过 op 层访问规则表，先落 sqlite 避免无库 500 干扰信号。
	promptRuleAdminDB(t)
	engine := promptRuleRouter()

	if rec := promptRuleDo(t, engine, http.MethodGet, "/api/v1/prompt-rule/list", "", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing token on GET: want 400, got %d %s", rec.Code, rec.Body.String())
	}
	if rec := promptRuleDo(t, engine, http.MethodPost, "/api/v1/prompt-rule/create", "not-a-token", "{}"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad token: want 401, got %d %s", rec.Code, rec.Body.String())
	}
	if rec := promptRuleDo(t, engine, http.MethodGet, "/api/v1/prompt-rule/list", promptRuleJWT(t, model.UserRoleUser, 8), ""); rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin: want 403, got %d %s", rec.Code, rec.Body.String())
	}
	if rec := promptRuleDo(t, engine, http.MethodGet, "/api/v1/prompt-rule/list", promptRuleJWT(t, model.UserRoleAdmin, 7), ""); rec.Code != http.StatusOK {
		t.Fatalf("admin GET list: want 200, got %d %s", rec.Code, rec.Body.String())
	}
	if rec := promptRuleDo(t, engine, http.MethodPost, "/api/v1/prompt-rule/toggle", promptRuleJWT(t, model.UserRoleAdmin, 7), "{\"id\":1,\"enabled\":true}"); rec.Code == http.StatusBadRequest && strings.Contains(rec.Body.String(), "JSON") {
		t.Fatalf("admin POST with JSON body rejected: %d %s", rec.Code, rec.Body.String())
	}
}

// TestPromptRulePreviewSourceOrder无网络：预览只读规则表，段顺序为
// client + before（评估序反排）+ after（评估序一致），命中原因与注入一致。
func TestPromptRulePreviewSourceOrder(t *testing.T) {
	promptRuleAdminDB(t)
	engine := promptRuleRouter()

	rules := []model.PromptRule{
		{
			Name: "outer-before", Enabled: true, Text: "outer", Position: model.PromptRulePositionBefore,
			PlanMode: model.PromptRulePlanModeAll, ChannelMode: model.PromptRuleChannelModeAll,
			ModelMode: model.PromptRuleModelModeAll, Audience: model.PromptAudienceAll,
		},
		{
			Name: "inner-before", Enabled: true, Text: "inner", Position: model.PromptRulePositionBefore,
			PlanMode: model.PromptRulePlanModeAll, ChannelMode: model.PromptRuleChannelModeAll,
			ModelMode: model.PromptRuleModelModeAll, Audience: model.PromptAudienceAll,
		},
		{
			Name: "tail-after", Enabled: true, Text: "tail", Position: model.PromptRulePositionAfter,
			PlanMode: model.PromptRulePlanModeAll, ChannelMode: model.PromptRuleChannelModeAll,
			ModelMode: model.PromptRuleModelModeAll, Audience: model.PromptAudienceAll,
		},
	}
	for i := range rules {
		body, _ := json.Marshal(rules[i])
		if rec := promptRuleDo(t, engine, http.MethodPost, "/api/v1/prompt-rule/create", promptRuleJWT(t, model.UserRoleAdmin, 7), string(body)); rec.Code != http.StatusOK {
			t.Fatalf("create %s: %d %s", rules[i].Name, rec.Code, rec.Body.String())
		}
	}

	body := `{"plan_id":0,"model":"gpt-x","channel_id":0,"is_admin":true,"client_system":"- client -"}`
	rec := promptRuleDo(t, engine, http.MethodPost, "/api/v1/prompt-rule/preview", promptRuleJWT(t, model.UserRoleAdmin, 7), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data struct {
			Matches []struct {
				Rule    model.PromptRule `json:"rule"`
				Matched bool             `json:"matched"`
				Reason  string           `json:"reason"`
			} `json:"matches"`
			Segments []struct {
				RuleID   int    `json:"rule_id"`
				Name     string `json:"name"`
				Position string `json:"position"`
			} `json:"segments"`
			Text string `json:"text"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode preview: %v", err)
	}
	if out.Data.Text != "inner\nouter\n- client -\ntail" {
		t.Fatalf("final text %q, want inner/outer/client/tail", out.Data.Text)
	}
	if len(out.Data.Matches) != 3 {
		t.Fatalf("want 3 matches, got %d", len(out.Data.Matches))
	}
	for _, m := range out.Data.Matches {
		if !m.Matched || m.Reason != "matched" {
			t.Fatalf("rule %s should match: %+v", m.Rule.Name, m)
		}
	}
	if len(out.Data.Segments) != 4 || out.Data.Segments[0].Name != "inner-before" ||
		out.Data.Segments[1].Name != "outer-before" || out.Data.Segments[2].Name != "client" ||
		out.Data.Segments[3].Name != "tail-after" {
		t.Fatalf("segment order wrong: %+v", out.Data.Segments)
	}
}

// TestPromptRuleInvalidScopeRejected：非法作用域/枚举在 handler 层回 400，
// opaque body 里的 nonsense audience 也一样。
func TestPromptRuleInvalidScopeRejected(t *testing.T) {
	promptRuleAdminDB(t)
	engine := promptRuleRouter()
	token := promptRuleJWT(t, model.UserRoleAdmin, 7)

	for name, body := range map[string]string{
		"bad position":    `{"name":"x","text":"t","position":"append_system","plan_mode":"all","channel_mode":"all","model_mode":"all","audience":"all"}`,
		"bad plan mode":   `{"name":"x","text":"t","position":"before","plan_mode":"fallback","channel_mode":"all","model_mode":"all","audience":"all"}`,
		"bad channel":     `{"name":"x","text":"t","position":"before","plan_mode":"all","channel_mode":"wrong","model_mode":"all","audience":"all"}`,
		"bad audience":    `{"name":"x","text":"t","position":"before","plan_mode":"all","channel_mode":"all","model_mode":"all","audience":"everyone"}`,
		"empty selected":  `{"name":"x","text":"t","position":"before","plan_mode":"selected","channel_mode":"all","model_mode":"selected","audience":"all","models":["m*"]}`,
		"empty plan sel":  `{"name":"x","text":"t","position":"before","plan_mode":"selected","channel_mode":"all","model_mode":"all","audience":"all"}`,
		"bad negative id": `{"name":"x","text":"t","position":"before","plan_mode":"selected","plan_ids":[-2],"channel_mode":"all","model_mode":"all","audience":"all"}`,
		"dup wildcard":    `{"name":"x","text":"t","position":"before","plan_mode":"all","channel_mode":"all","model_mode":"selected","models":["m*","M*"],"audience":"all"}`,
	} {
		if rec := promptRuleDo(t, engine, http.MethodPost, "/api/v1/prompt-rule/create", token, body); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: want 400, got %d %s", name, rec.Code, rec.Body.String())
		}
	}
}
