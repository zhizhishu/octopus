package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
)

// 访客 Key 权限审计：全部走真实路由表 + 真实中间件 + 真实 handler（setupUserAdminEnv）。

type guestKeyEnv struct {
	userAdminEnv
	plain      model.User
	plainJWT   string
	plainKey   model.APIKey // 普通用户自己的普通 Key
	guest      model.APIKey // 管理员建的访客 Key，只允许 gpt-allowed
	guestToken string
}

func setupGuestKeyEnv(t *testing.T) guestKeyEnv {
	t.Helper()
	env := setupUserAdminEnv(t)
	plain := env.newUser(t, "plain", 5)
	plainKey := model.APIKey{UserID: plain.ID, Name: "plain-key", APIKey: "sk-octopus-plainkey0000000000", Enabled: true}
	if err := op.APIKeyCreate(&plainKey, env.ctx); err != nil {
		t.Fatal(err)
	}
	seedGuestAvailableModels(t, env, "gpt-allowed", "gpt-other", "m")
	rec, data := env.do(t, http.MethodPost, "/api/v1/guestkey/create", env.token, map[string]any{
		"name": "客人 A", "note": "展会试用", "models": []string{"gpt-allowed"},
		"expire_at": time.Now().Add(24 * time.Hour).Unix(), "max_cost": 2,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("admin create guest key: %d %s", rec.Code, rec.Body.String())
	}
	var guest model.APIKey
	if err := json.Unmarshal(data, &guest); err != nil {
		t.Fatal(err)
	}
	if !guest.Guest || guest.UserID != env.admin.ID || guest.SupportedModels != "gpt-allowed" || !strings.HasPrefix(guest.APIKey, "sk-octopus-") {
		t.Fatalf("bad guest key %#v", guest)
	}
	return guestKeyEnv{userAdminEnv: env, plain: plain, plainJWT: jwtBearer(t, plain), plainKey: plainKey, guest: guest, guestToken: guest.APIKey}
}

// seedGuestAvailableModels 让这些模型出现在「当前可用模型」里：一条启用的渠道（上游是本机假服务，
// 回一个固定的最小 chat 回复，绝不连外网）+ 每个方案各一条路由。访客 Key 只能限定这份列表里的模型。
func seedGuestAvailableModels(t *testing.T, env userAdminEnv, models ...string) {
	t.Helper()
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-seed","object":"chat.completion","created":1,"model":"gpt-allowed","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(fake.Close)
	ch := model.Channel{Name: "guest-seed", Enabled: true, Model: strings.Join(models, ","),
		BaseUrls: []model.BaseUrl{{URL: fake.URL}}, Keys: []model.ChannelKey{{ChannelKey: "sk-fake", Enabled: true}}}
	if err := op.ChannelCreate(&ch, env.ctx); err != nil {
		t.Fatal(err)
	}
	plans, err := op.AccessPlanList(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range plans {
		targets := make([]model.AccessRouteTarget, 0, len(models))
		for _, m := range models {
			targets = append(targets, model.AccessRouteTarget{AccessPlanID: p.ID, RequestModel: m, ChannelID: ch.ID, UpstreamModel: m,
				Priority: 1, Weight: 1, Enabled: true, BillingModelSource: "request_model", Mode: 3, FallbackMode: "none"})
		}
		if _, err := op.AccessPlanUpdateRouteTargets(p.ID, targets, env.ctx); err != nil {
			t.Fatal(err)
		}
	}
	got, err := op.GuestKeyModels(env.admin, env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range models {
		found := false
		for _, g := range got {
			found = found || g == m
		}
		if !found {
			t.Fatalf("seeded model %q not in available list %v", m, got)
		}
	}
}

func (e guestKeyEnv) doHeaders(t *testing.T, method, target string, headers map[string]string, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.engine.ServeHTTP(rec, req)
	return rec
}

var guestKeyManagementRoutes = []struct{ method, target string }{
	{http.MethodGet, "/api/v1/guestkey/list"},
	{http.MethodGet, "/api/v1/guestkey/models"},
	{http.MethodPost, "/api/v1/guestkey/create"},
	{http.MethodPost, "/api/v1/guestkey/update"},
	{http.MethodPost, "/api/v1/guestkey/revoke/%d"},
	{http.MethodDelete, "/api/v1/guestkey/delete/%d"},
	{http.MethodGet, "/api/v1/guestkey/usage?id=%d"},
	{http.MethodGet, "/api/v1/guestkey/access?id=%d"},
}

// 访客 Key / 普通用户的 Key / 普通用户登录态，打访客 Key 管理接口一律 401/403，且什么都没改。
func TestGuestKeyManagementRejectsNonAdmin(t *testing.T) {
	env := setupGuestKeyEnv(t)
	body := map[string]any{"id": env.guest.ID, "name": "pwn", "models": []string{"*"}, "expire_at": time.Now().Add(time.Hour).Unix()}
	for _, r := range guestKeyManagementRoutes {
		target := r.target
		if strings.Contains(target, "%d") {
			target = fmt.Sprintf(target, env.guest.ID)
		}
		for name, tc := range map[string]struct {
			token string
			want  int
		}{
			"guest key":      {env.guestToken, http.StatusUnauthorized},
			"plain user key": {env.plainKey.APIKey, http.StatusUnauthorized},
			"plain user jwt": {env.plainJWT, http.StatusForbidden},
		} {
			rec, _ := env.do(t, r.method, target, tc.token, body)
			if rec.Code != tc.want {
				t.Fatalf("%s %s with %s: want %d, got %d %s", r.method, target, name, tc.want, rec.Code, rec.Body.String())
			}
		}
	}
	got, err := op.APIKeyGet(env.guest.ID, env.ctx)
	if err != nil || got.Name != "客人 A" || got.SupportedModels != "gpt-allowed" || got.IsRevoked() {
		t.Fatalf("refused requests changed the guest key: %#v %v", got, err)
	}
	keys, _ := op.GuestKeyList(env.admin, env.ctx)
	if len(keys) != 1 {
		t.Fatalf("refused create must not add a guest key, have %d", len(keys))
	}
}

// 公开接口（不需要登录）在扫描里跳过：它们对任何凭证都一样。
var guestKeyPublicAPIRoutes = map[string]bool{
	"GET /api/v1/user/registration-options":    true,
	"POST /api/v1/user/login":                  true,
	"POST /api/v1/user/register":               true,
	"POST /api/v1/user/send-verification-code": true,
	"GET /api/v1/log/stream":                   true,
	"GET /api/v1/log/stream-state":             true,
}

// 扫真实路由表里每一条 /api/ 路由：访客 Key 和普通用户的 Key 都只能拿到 401/403。
// 唯一的例外是普通 Key 自己的 /api/v1/apikey/login|stats（APIKeyAuth 组），访客 Key 在那里也是 403。
func TestGuestKeyCannotReachAnyManagementRoute(t *testing.T) {
	env := setupGuestKeyEnv(t)
	apiKeySelfRoutes := map[string]bool{"GET /api/v1/apikey/login": true, "GET /api/v1/apikey/stats": true}
	checked := 0
	for _, route := range env.engine.Routes() {
		if !strings.HasPrefix(route.Path, "/api/") {
			continue
		}
		sig := route.Method + " " + route.Path
		if guestKeyPublicAPIRoutes[sig] {
			continue
		}
		target := strings.NewReplacer(":id", "1", ":task_id", "1", ":name", "x", ":key", "x", "*path", "x").Replace(route.Path)
		for _, cred := range []struct {
			name  string
			token string
		}{{"guest", env.guestToken}, {"plain-key", env.plainKey.APIKey}} {
			rec := env.doHeaders(t, route.Method, target, map[string]string{"Authorization": "Bearer " + cred.token}, `{}`)
			if apiKeySelfRoutes[sig] && cred.name == "plain-key" {
				continue // 普通 Key 看自己的统计是原有功能
			}
			if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
				t.Fatalf("%s with %s key must be 401/403, got %d %s", sig, cred.name, rec.Code, rec.Body.String())
			}
		}
		checked++
	}
	if checked < 50 {
		t.Fatalf("route sweep only saw %d /api routes; router not wired?", checked)
	}
	// 其他传 Key 的方式（x-api-key / x-goog-api-key）也进不去控制台自助接口。
	for _, h := range []string{"x-api-key", "x-goog-api-key"} {
		for _, p := range []string{"/api/v1/apikey/login", "/api/v1/apikey/stats"} {
			rec := env.doHeaders(t, http.MethodGet, p, map[string]string{h: env.guestToken}, "")
			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s via %s must be 403, got %d %s", p, h, rec.Code, rec.Body.String())
			}
		}
	}
}

// 普通用户登录态打管理员接口：403（含新加的访客 Key 管理和几条代表性的管理接口）。
func TestPlainUserSessionRejectedOnAdminEndpoints(t *testing.T) {
	env := setupGuestKeyEnv(t)
	for _, target := range []string{"/api/v1/user/list", "/api/v1/setting/list", "/api/v1/channel/list", "/api/v1/access-plan/list", "/api/v1/guestkey/list"} {
		rec, _ := env.do(t, http.MethodGet, target, env.plainJWT, nil)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s with plain jwt: want 403, got %d %s", target, rec.Code, rec.Body.String())
		}
	}
}

// 普通接口建 / 改 / 列 / 启用都碰不到访客 Key，也造不出访客 Key。
func TestGuestKeyNotReachableThroughPlainAPIKeyRoutes(t *testing.T) {
	env := setupGuestKeyEnv(t)

	// 普通用户在请求体里写 guest=true：建出来的仍是普通 Key、归属仍是自己。
	rec, data := env.do(t, http.MethodPost, "/api/v1/apikey/create", env.plainJWT, map[string]any{"name": "x", "guest": true, "user_id": env.admin.ID, "revoked_at": 0})
	if rec.Code != http.StatusOK {
		t.Fatalf("plain create: %d %s", rec.Code, rec.Body.String())
	}
	var created model.APIKey
	_ = json.Unmarshal(data, &created)
	if created.Guest || created.UserID != env.plain.ID {
		t.Fatalf("plain user escalated a key: %#v", created)
	}
	// 管理员经普通接口也建不出访客 Key（只有 /guestkey/create）。
	_, data = env.do(t, http.MethodPost, "/api/v1/apikey/create", env.token, map[string]any{"name": "y", "guest": true})
	var adminPlain model.APIKey
	_ = json.Unmarshal(data, &adminPlain)
	if adminPlain.Guest {
		t.Fatalf("plain create route produced a guest key")
	}
	// 普通 Key 改成访客 Key：不行。
	rec, _ = env.do(t, http.MethodPost, "/api/v1/apikey/update", env.plainJWT, map[string]any{"id": created.ID, "name": "x", "guest": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("plain update: %d %s", rec.Code, rec.Body.String())
	}
	if k, _ := op.APIKeyGet(created.ID, env.ctx); k.Guest {
		t.Fatal("plain update turned a key into a guest key")
	}

	// 普通用户：列表里看不到、改 / 删 / 启停 / 用量都是「不存在」。
	_, data = env.do(t, http.MethodGet, "/api/v1/apikey/list", env.plainJWT, nil)
	if strings.Contains(string(data), env.guestToken) || strings.Contains(string(data), "客人 A") {
		t.Fatalf("plain user sees the guest key: %s", data)
	}
	for _, c := range []struct {
		method, target string
		body           any
	}{
		{http.MethodPost, "/api/v1/apikey/update", map[string]any{"id": env.guest.ID, "name": "mine", "user_id": env.plain.ID}},
		{http.MethodDelete, fmt.Sprintf("/api/v1/apikey/delete/%d", env.guest.ID), nil},
		{http.MethodGet, fmt.Sprintf("/api/v1/apikey/usage?id=%d", env.guest.ID), nil},
	} {
		rec, _ := env.do(t, c.method, c.target, env.plainJWT, c.body)
		if rec.Code == http.StatusOK {
			t.Fatalf("%s %s by plain user must fail, got 200 %s", c.method, c.target, rec.Body.String())
		}
	}
	_, data = env.do(t, http.MethodPost, "/api/v1/apikey/batch", env.plainJWT, map[string]any{"ids": []int{env.guest.ID}, "action": "disable"})
	var batch model.APIKeyBatchResult
	_ = json.Unmarshal(data, &batch)
	if len(batch.Done) != 0 {
		t.Fatalf("plain user batch touched the guest key: %s", data)
	}

	// 管理员：普通列表里也不出现（在「访客」里），普通 update 改不了访客 Key（防止改归属 / 去掉白名单）。
	_, data = env.do(t, http.MethodGet, "/api/v1/apikey/list", env.token, nil)
	if strings.Contains(string(data), env.guestToken) {
		t.Fatalf("admin plain list must not include guest keys: %s", data)
	}
	rec, _ = env.do(t, http.MethodPost, "/api/v1/apikey/update", env.token, map[string]any{"id": env.guest.ID, "name": "z", "user_id": env.plain.ID, "supported_models": ""})
	if rec.Code == http.StatusOK {
		t.Fatalf("plain update route must refuse guest keys, got 200 %s", rec.Body.String())
	}
	got, _ := op.APIKeyGet(env.guest.ID, env.ctx)
	if got.UserID != env.admin.ID || got.SupportedModels != "gpt-allowed" || !got.Guest {
		t.Fatalf("guest key changed through the plain route: %#v", got)
	}
}

func (e guestKeyEnv) chat(t *testing.T, token, modelName string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, modelName)
	return e.doHeaders(t, http.MethodPost, "/v1/chat/completions", map[string]string{
		"Authorization": "Bearer " + token, "User-Agent": "guest-cli/1.0", "X-Forwarded-For": "203.0.113.9",
	}, body)
}

// 不在白名单的模型被拒（400 model_not_supported），而且这次请求照样记下 IP / UA，管理员按 Key 能看到。
func TestGuestKeyRelayModelAllowListAndAccessLog(t *testing.T) {
	env := setupGuestKeyEnv(t)
	rec := env.chat(t, env.guestToken, "gpt-forbidden")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "model_not_supported") {
		t.Fatalf("non-allowed model: want 400 model_not_supported, got %d %s", rec.Code, rec.Body.String())
	}
	// 白名单里的模型过了白名单这一关（渠道上游是本机假服务，不会有任何外网请求）。
	rec = env.chat(t, env.guestToken, "gpt-allowed")
	if strings.Contains(rec.Body.String(), "model_not_supported") || rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
		t.Fatalf("allowed model must pass auth + allow-list, got %d %s", rec.Code, rec.Body.String())
	}

	rec2, data := env.do(t, http.MethodGet, fmt.Sprintf("/api/v1/guestkey/access?id=%d", env.guest.ID), env.token, nil)
	if rec2.Code != http.StatusOK {
		t.Fatalf("access: %d %s", rec2.Code, rec2.Body.String())
	}
	var access model.GuestKeyAccess
	if err := json.Unmarshal(data, &access); err != nil {
		t.Fatal(err)
	}
	if access.TotalRequests < 1 || access.Last == nil || access.Last.UserAgent != "guest-cli/1.0" || access.Last.IP == "" || access.Last.APIKeyID != env.guest.ID {
		t.Fatalf("guest request not recorded per key: %s", data)
	}
	// 普通 Key 的请求不会算到访客 Key 头上。
	for _, g := range access.IPs {
		for _, k := range g.Keys {
			if k.APIKeyID != env.guest.ID {
				t.Fatalf("other key leaked into guest access: %s", data)
			}
		}
	}
}

// 过期 / 作废 / 停用的访客 Key 一律 401；作废后不能再启用、不能再改。
func TestGuestKeyExpiredRevokedDisabledRefused(t *testing.T) {
	env := setupGuestKeyEnv(t)

	// 停用。
	f := false
	if _, err := op.GuestKeyUpdate(model.GuestKeyRequest{ID: env.guest.ID, Name: "客人 A", Models: []string{"gpt-allowed"}, ExpireAt: env.guest.ExpireAt, MaxCost: 2, Enabled: &f}, env.admin, env.ctx); err != nil {
		t.Fatal(err)
	}
	if rec := env.chat(t, env.guestToken, "gpt-allowed"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("disabled guest key: want 401, got %d %s", rec.Code, rec.Body.String())
	}
	tr := true
	if _, err := op.GuestKeyUpdate(model.GuestKeyRequest{ID: env.guest.ID, Name: "客人 A", Models: []string{"gpt-allowed"}, ExpireAt: env.guest.ExpireAt, MaxCost: 2, Enabled: &tr}, env.admin, env.ctx); err != nil {
		t.Fatal(err)
	}

	// 过期（直接改库 + 刷缓存，模拟时间走过了过期点）。
	expired := model.GuestKeyRequest{Name: "过期", Models: []string{"gpt-allowed"}, ExpireAt: time.Now().Add(time.Hour).Unix()}
	ek, err := op.GuestKeyCreate(expired, "sk-octopus-expiredguest000000", env.admin, env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.GetDB().Model(&model.APIKey{}).Where("id = ?", ek.ID).Update("expire_at", time.Now().Add(-time.Minute).Unix()).Error; err != nil {
		t.Fatal(err)
	}
	if err := op.InitCache(); err != nil {
		t.Fatal(err)
	}
	if rec := env.chat(t, ek.APIKey, "gpt-allowed"); rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "expired") {
		t.Fatalf("expired guest key: want 401 expired, got %d %s", rec.Code, rec.Body.String())
	}

	// 作废。
	rec, _ := env.do(t, http.MethodPost, fmt.Sprintf("/api/v1/guestkey/revoke/%d", env.guest.ID), env.token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body.String())
	}
	if rec := env.chat(t, env.guestToken, "gpt-allowed"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked guest key: want 401, got %d %s", rec.Code, rec.Body.String())
	}
	// 作废后：普通批量启用不行，访客 update 也不行。
	_, data := env.do(t, http.MethodPost, "/api/v1/apikey/batch", env.token, map[string]any{"ids": []int{env.guest.ID}, "action": "enable"})
	var batch model.APIKeyBatchResult
	_ = json.Unmarshal(data, &batch)
	if len(batch.Done) != 0 {
		t.Fatalf("revoked key re-enabled via batch: %s", data)
	}
	rec, _ = env.do(t, http.MethodPost, "/api/v1/guestkey/update", env.token, map[string]any{"id": env.guest.ID, "name": "again", "models": []string{"gpt-allowed"}, "expire_at": env.guest.ExpireAt, "enabled": true})
	if rec.Code == http.StatusOK {
		t.Fatalf("revoked key must not be editable, got 200 %s", rec.Body.String())
	}
	if rec := env.chat(t, env.guestToken, "gpt-allowed"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked guest key still refused after attempts: got %d", rec.Code)
	}
	// 记录还在，管理员还能看用量（日志 / 用量按这把 Key 查）。
	if rec, _ := env.do(t, http.MethodGet, fmt.Sprintf("/api/v1/guestkey/usage?id=%d&range=7d", env.guest.ID), env.token, nil); rec.Code != http.StatusOK {
		t.Fatalf("usage of revoked key: %d %s", rec.Code, rec.Body.String())
	}
}

// 管理员被降级后，他名下的访客 Key 立刻不能用（归属必须一直是管理员）。
func TestGuestKeyOwnerDemotedRefused(t *testing.T) {
	env := setupGuestKeyEnv(t)
	other, err := op.UserCreate(model.UserCreateRequest{Username: "root2", Password: "password", Role: model.UserRoleAdmin}, env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	k, err := op.GuestKeyCreate(model.GuestKeyRequest{Name: "g2", Models: []string{"gpt-allowed"}, ExpireAt: time.Now().Add(time.Hour).Unix()}, "sk-octopus-demotedguest000000", other, env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := op.UserUpdate(model.UserUpdateRequest{ID: other.ID, Username: "root2", Role: model.UserRoleUser, Status: model.UserStatusActive}, env.admin.ID, env.ctx); err != nil {
		t.Fatal(err)
	}
	if rec := env.chat(t, k.APIKey, "gpt-allowed"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("guest key of demoted owner: want 401, got %d %s", rec.Code, rec.Body.String())
	}
}

// 校验：必须有模型、必须有过期时间、不能超过最长期限、不能负上限。
func TestGuestKeyCreateValidation(t *testing.T) {
	env := setupGuestKeyEnv(t)
	now := time.Now()
	for name, body := range map[string]map[string]any{
		"no models":   {"name": "a", "models": []string{}, "expire_at": now.Add(time.Hour).Unix()},
		"blank model": {"name": "a", "models": []string{"  "}, "expire_at": now.Add(time.Hour).Unix()},
		"no expire":   {"name": "a", "models": []string{"m"}},
		"past":        {"name": "a", "models": []string{"m"}, "expire_at": now.Add(-time.Hour).Unix()},
		"too far":     {"name": "a", "models": []string{"m"}, "expire_at": now.AddDate(2, 0, 0).Unix()},
		"neg cost":    {"name": "a", "models": []string{"m"}, "expire_at": now.Add(time.Hour).Unix(), "max_cost": -1},
		"comma model": {"name": "a", "models": []string{"a,b"}, "expire_at": now.Add(time.Hour).Unix()},
	} {
		rec, _ := env.do(t, http.MethodPost, "/api/v1/guestkey/create", env.token, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: want 400, got %d %s", name, rec.Code, rec.Body.String())
		}
	}
}

// 限定的模型必须在当前可用模型里：新建、编辑都校验，不认识的名字给一句看得懂的中文错误，什么都不改。
func TestGuestKeyModelsMustBeAvailable(t *testing.T) {
	env := setupGuestKeyEnv(t)
	exp := time.Now().Add(time.Hour).Unix()
	errMsg := func(rec *httptest.ResponseRecorder) string {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &e)
		return e.Message + " " + rec.Body.String()
	}

	// 新建：一个不存在的模型，整把拒掉，并点名是哪个。
	before, _ := op.GuestKeyList(env.admin, env.ctx)
	rec, _ := env.do(t, http.MethodPost, "/api/v1/guestkey/create", env.token, map[string]any{
		"name": "b", "models": []string{"gpt-allowed", "gpt-ghost"}, "expire_at": exp,
	})
	if rec.Code != http.StatusBadRequest || !strings.Contains(errMsg(rec), "这些模型现在不在可用模型列表里：gpt-ghost") {
		t.Fatalf("unknown model on create: want 400 with Chinese message naming gpt-ghost, got %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(errMsg(rec), "gpt-allowed、") || strings.Contains(errMsg(rec), "：gpt-allowed") {
		t.Fatalf("available model must not be listed as missing: %s", rec.Body.String())
	}
	after, _ := op.GuestKeyList(env.admin, env.ctx)
	if len(after) != len(before) {
		t.Fatalf("rejected create must not add a key: %d -> %d", len(before), len(after))
	}

	// 可用的（大小写不同、带 [1m] 也认，和转发时判白名单同一口径）照常能建。
	rec, _ = env.do(t, http.MethodPost, "/api/v1/guestkey/create", env.token, map[string]any{
		"name": "c", "models": []string{"GPT-Other", "m[1m]"}, "expire_at": exp,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("available models on create: want 200, got %d %s", rec.Code, rec.Body.String())
	}

	// 编辑：换成不存在的模型被拒，原来的白名单不变。
	rec, _ = env.do(t, http.MethodPost, "/api/v1/guestkey/update", env.token, map[string]any{
		"id": env.guest.ID, "name": "客人 A", "models": []string{"gpt-ghost", "gpt-ghost-2"}, "expire_at": env.guest.ExpireAt, "max_cost": 2,
	})
	if rec.Code != http.StatusBadRequest || !strings.Contains(errMsg(rec), "这些模型现在不在可用模型列表里：gpt-ghost、gpt-ghost-2") {
		t.Fatalf("unknown model on update: want 400 naming both, got %d %s", rec.Code, rec.Body.String())
	}
	k, err := op.GuestKeyGet(env.guest.ID, env.admin, env.ctx)
	if err != nil || k.SupportedModels != "gpt-allowed" {
		t.Fatalf("rejected update must not change models: %q %v", k.SupportedModels, err)
	}
	// 编辑成可用的照常通过。
	rec, _ = env.do(t, http.MethodPost, "/api/v1/guestkey/update", env.token, map[string]any{
		"id": env.guest.ID, "name": "客人 A", "models": []string{"gpt-allowed", "gpt-other"}, "expire_at": env.guest.ExpireAt, "max_cost": 2,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("available models on update: want 200, got %d %s", rec.Code, rec.Body.String())
	}
}

// 访客 Key 的请求也进「客户端指纹」：按这把 Key 看（访客详情的「指纹」页）能看到，
// 在管理员的用户抽屉「指纹」页里也单独成一个 Key，并标成访客；普通 Key 的指纹不会混进来。
func TestGuestKeyShowsInClientTelemetryPerKey(t *testing.T) {
	env := setupGuestKeyEnv(t)
	adminKey := model.APIKey{UserID: env.admin.ID, Name: "admin-own", APIKey: "sk-octopus-adminown000000000000", Enabled: true}
	if err := op.APIKeyCreate(&adminKey, env.ctx); err != nil {
		t.Fatal(err)
	}
	if rec := env.chat(t, env.guestToken, "gpt-allowed"); rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
		t.Fatalf("guest chat: %d %s", rec.Code, rec.Body.String())
	}
	if rec := env.doHeaders(t, http.MethodPost, "/v1/chat/completions", map[string]string{
		"Authorization": "Bearer " + adminKey.APIKey, "User-Agent": "admin-tool/2.0",
	}, `{"model":"gpt-allowed","messages":[{"role":"user","content":"hi"}]}`); rec.Code == http.StatusUnauthorized {
		t.Fatalf("admin key chat: %d", rec.Code)
	}

	// 只按访客 Key 看。
	rec, data := env.do(t, http.MethodGet, fmt.Sprintf("/api/v1/user/telemetry?key=%d", env.guest.ID), env.token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("telemetry by key: %d %s", rec.Code, rec.Body.String())
	}
	var byKey model.UserClientDetail
	if err := json.Unmarshal(data, &byKey); err != nil {
		t.Fatal(err)
	}
	if len(byKey.Fingerprints) == 0 {
		t.Fatalf("guest key request not in telemetry: %s", data)
	}
	// 指纹缓冲是进程级的，同包别的测试（各自新库、Key id 会重复）可能留下同 id 的记录，
	// 所以这里按 UA 认：访客的请求必须在，管理员自己那把 Key 的请求不许混进来。
	sawGuest := false
	for _, fp := range byKey.Fingerprints {
		if fp.APIKeyID != env.guest.ID || fp.UserID != env.admin.ID {
			t.Fatalf("foreign fingerprint in guest key view: %+v", fp)
		}
		if fp.UserAgent == "admin-tool/2.0" {
			t.Fatalf("admin's own key request leaked into guest key view: %+v", fp)
		}
		sawGuest = sawGuest || fp.UserAgent == "guest-cli/1.0"
	}
	if !sawGuest {
		t.Fatalf("guest request (guest-cli/1.0) not found in guest key view: %s", data)
	}
	if len(byKey.Keys) != 1 || byKey.Keys[0].APIKeyID != env.guest.ID || !byKey.Keys[0].Guest || byKey.Keys[0].APIKeyName != "客人 A" {
		t.Fatalf("guest key info: %+v", byKey.Keys)
	}

	// 管理员的用户抽屉：访客 Key 和管理员自己的 Key 分开列，访客那个标 guest。
	rec, data = env.do(t, http.MethodGet, fmt.Sprintf("/api/v1/user/telemetry?id=%d", env.admin.ID), env.token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("telemetry by user: %d %s", rec.Code, rec.Body.String())
	}
	var byUser model.UserClientDetail
	if err := json.Unmarshal(data, &byUser); err != nil {
		t.Fatal(err)
	}
	seen := map[int]bool{}
	for _, k := range byUser.Keys {
		seen[k.APIKeyID] = true
		if k.Guest != (k.APIKeyID == env.guest.ID) {
			t.Fatalf("guest flag wrong for key %d: %+v", k.APIKeyID, k)
		}
	}
	if !seen[env.guest.ID] || !seen[adminKey.ID] {
		t.Fatalf("admin drawer must list guest key and own key separately: %+v", byUser.Keys)
	}

	// 访客 Key 自己打不了指纹接口（管理接口一律拒）。
	if rec := env.doHeaders(t, http.MethodGet, fmt.Sprintf("/api/v1/user/telemetry?key=%d", env.guest.ID), map[string]string{"Authorization": "Bearer " + env.guestToken}, ""); rec.Code == http.StatusOK {
		t.Fatalf("guest key must not read telemetry: %d", rec.Code)
	}
}
