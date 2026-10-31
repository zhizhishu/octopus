package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/conf"
	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/server/router"
)

// /apikey/batch、/apikey/usage、/apikey/models 走真实路由（Auth 中间件同生产），
// 重点核对普通用户只能碰自己的 Key。
func TestAPIKeyBatchUsageModelsRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv(strings.ToUpper(conf.APP_NAME)+"_ADMIN_TOKEN", "")
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "apikey_routes.db"), false); err != nil {
		t.Fatalf("init db: %v", err)
	}
	ctx := context.Background()
	t.Cleanup(func() { _ = db.Close() })
	if err := op.InitCache(); err != nil {
		t.Fatalf("init cache: %v", err)
	}
	mkUser := func(name string, role model.UserRole) model.User {
		u, err := op.UserCreate(model.UserCreateRequest{Username: name, Password: "password-" + name, Role: role, Status: model.UserStatusActive}, ctx)
		if err != nil {
			t.Fatalf("create user %s: %v", name, err)
		}
		return u
	}
	admin := mkUser("routes-admin", model.UserRoleAdmin)
	alice := mkUser("alice", model.UserRoleUser)
	bob := mkUser("bob", model.UserRoleUser)
	mkKey := func(owner model.User, name string) model.APIKey {
		k := model.APIKey{UserID: owner.ID, Name: name, APIKey: "sk-octopus-routes-" + name, Enabled: true}
		if err := op.APIKeyCreate(&k, ctx); err != nil {
			t.Fatalf("create key: %v", err)
		}
		return k
	}
	ka := mkKey(alice, "a")
	kb := mkKey(bob, "b")

	engine := gin.New()
	if err := router.RegisterAll(engine); err != nil {
		t.Fatalf("register: %v", err)
	}
	adminJWT := dashJWT(t, admin)
	aliceJWT := dashJWT(t, alice)

	do := func(method, path, jwt string, body any) (int, json.RawMessage) {
		t.Helper()
		var buf bytes.Buffer
		if body != nil {
			_ = json.NewEncoder(&buf).Encode(body)
		}
		req := httptest.NewRequest(method, path, &buf)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+jwt)
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)
		var env struct {
			Data json.RawMessage `json:"data"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &env)
		return rec.Code, env.Data
	}

	code, data := do(http.MethodPost, "/api/v1/apikey/batch", aliceJWT, map[string]any{"ids": []int{ka.ID, kb.ID}, "action": "disable"})
	var res model.APIKeyBatchResult
	if err := json.Unmarshal(data, &res); err != nil || code != http.StatusOK {
		t.Fatalf("batch code=%d data=%s", code, data)
	}
	if len(res.Done) != 1 || res.Done[0] != ka.ID || len(res.Failed) != 1 || res.Failed[0].ID != kb.ID {
		t.Fatalf("batch result %+v", res)
	}
	if k, _ := op.APIKeyGet(kb.ID, ctx); !k.Enabled {
		t.Fatal("alice disabled bob's key")
	}

	if code, _ := do(http.MethodGet, "/api/v1/apikey/usage?id="+itoa(kb.ID)+"&range=7d", aliceJWT, nil); code == http.StatusOK {
		t.Fatal("alice read bob's key usage")
	}
	code, data = do(http.MethodGet, "/api/v1/apikey/usage?id="+itoa(ka.ID)+"&range=7d", aliceJWT, nil)
	var usage model.UserUsageResponse
	if err := json.Unmarshal(data, &usage); err != nil || code != http.StatusOK || usage.APIKeyID != ka.ID || usage.UserID != alice.ID || len(usage.Daily) != 7 {
		t.Fatalf("usage code=%d %+v", code, usage)
	}
	if code, _ := do(http.MethodGet, "/api/v1/apikey/usage?id="+itoa(kb.ID), adminJWT, nil); code != http.StatusOK {
		t.Fatalf("admin usage code=%d", code)
	}

	// /models：普通用户带 user_id 也只看自己的（不报错，按自己算）；管理员可以看别人的。
	if code, _ := do(http.MethodGet, "/api/v1/apikey/models?user_id="+itoa(bob.ID), aliceJWT, nil); code != http.StatusOK {
		t.Fatalf("alice models code=%d", code)
	}
	if code, _ := do(http.MethodGet, "/api/v1/apikey/models?user_id="+itoa(bob.ID), adminJWT, nil); code != http.StatusOK {
		t.Fatalf("admin models code=%d", code)
	}
	if code, _ := do(http.MethodGet, "/api/v1/apikey/models?user_id=99999", adminJWT, nil); code == http.StatusOK {
		t.Fatal("unknown user should fail")
	}

	// 列表响应仍是完整 Key（其他客户端依赖），并多了 last_used_at。
	code, data = do(http.MethodGet, "/api/v1/apikey/list", aliceJWT, nil)
	if code != http.StatusOK || !strings.Contains(string(data), `"api_key":"sk-octopus-routes-a"`) || !strings.Contains(string(data), `"last_used_at":0`) {
		t.Fatalf("list code=%d data=%s", code, data)
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
