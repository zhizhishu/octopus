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

	"github.com/bestruirui/octopus/internal/conf"
	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/server/auth"
	"github.com/bestruirui/octopus/internal/server/middleware"
	"github.com/gin-gonic/gin"
)

// All values below are obviously-fake test fixtures for the isolated temp DB; no real
// credential, domain or upstream name is involved.
const (
	settingAuthAdminToken   = "octopus-admin-token-abcdefghij-0123456789"
	settingAuthChannelKey   = "sk-upstream-test-channel-key-0123456789"
	settingAuthSMTPPassword = "smtp-test-password-0123456789"
)

// The three endpoints that must only ever answer a real admin LOGIN: the secret read and
// the two backup endpoints. They share one refusal helper in setting.go.
var settingAuthGuardedTargets = []struct {
	name   string
	method string
	target string
	body   []byte
}{
	{name: "secret", method: http.MethodGet, target: "/api/v1/setting/secret?key=admin_access_token"},
	{name: "export", method: http.MethodGet, target: "/api/v1/setting/export?include_logs=true&include_stats=true"},
	{
		name:   "import",
		method: http.MethodPost,
		target: "/api/v1/setting/import",
		// A payload that WOULD overwrite the site's admin token if it were ingested.
		body: []byte(`{"version":2,"settings":[{"key":"admin_access_token","value":"attacker-supplied-token-0123456789"}]}`),
	},
}

func setupSettingAuthTestDB(t *testing.T) context.Context {
	t.Helper()
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "setting_auth_test.db"), false); err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
	})
	if err := op.InitCache(); err != nil {
		t.Fatalf("init cache: %v", err)
	}
	return context.Background()
}

// seedSettingAuthSiteSecrets plants the plaintext secrets a real site would hold, so the
// tests can assert those exact strings never leave the server on a refused request.
func seedSettingAuthSiteSecrets(t *testing.T, ctx context.Context) model.User {
	t.Helper()
	t.Setenv(strings.ToUpper(conf.APP_NAME)+"_ADMIN_TOKEN", "")

	admin, err := op.UserCreate(model.UserCreateRequest{
		Username: "admin",
		Password: "password",
		Role:     model.UserRoleAdmin,
		Status:   model.UserStatusActive,
	}, ctx)
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	for key, value := range map[model.SettingKey]string{
		model.SettingKeyAdminToken:        settingAuthAdminToken,
		model.SettingKeyEmailSMTPPassword: settingAuthSMTPPassword,
	} {
		if err := op.SettingSetString(key, value); err != nil {
			t.Fatalf("set %s: %v", key, err)
		}
	}

	channel := model.Channel{
		Name:    "upstream-test",
		Enabled: true,
		Keys:    []model.ChannelKey{{Enabled: true, ChannelKey: settingAuthChannelKey}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	return admin
}

// newSettingGroupRouter mounts the SAME Auth + AdminOnly chain the production
// /api/v1/setting group uses (see init in setting.go) over the real handlers, so no test
// here bypasses the auth path.
func newSettingGroupRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	grp := engine.Group("/api/v1/setting", middleware.Auth(), middleware.AdminOnly())
	grp.GET("/secret", getSettingSecret)
	grp.GET("/export", exportDB)
	grp.POST("/import", importDB)
	return engine
}

// newMarkerlessSettingRouter mounts ONLY AdminOnly in front of the real handlers and
// stamps auth_method directly. middleware.Auth always writes either "jwt" or
// "admin_token", so the missing / empty / unknown marker branch cannot be reached through
// it; this router is the only way to prove the fail-closed half of the guard.
func newMarkerlessSettingRouter(t *testing.T, stampMarker bool, authMethod string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		admin, err := op.UserDefaultAdmin(c.Request.Context())
		if err != nil {
			t.Fatalf("default admin: %v", err)
		}
		if stampMarker {
			c.Set("auth_method", authMethod)
		}
		middleware.SetCurrentUser(c, admin)
		c.Next()
	}, middleware.AdminOnly())

	grp := engine.Group("/api/v1/setting")
	grp.GET("/secret", getSettingSecret)
	grp.GET("/export", exportDB)
	grp.POST("/import", importDB)
	return engine
}

func doSettingAuthRequest(engine *gin.Engine, method string, target string, bearer string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+bearer)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec
}

func adminTokenBearer() string { return settingAuthAdminToken }

func jwtBearer(t *testing.T, admin model.User) string {
	t.Helper()
	token, _, err := auth.GenerateJWTToken(admin, 0)
	if err != nil {
		t.Fatalf("generate jwt: %v", err)
	}
	return token
}

// assertNoPlantedSecretInBody is the core leak assertion: a refused response must not
// carry any of the site's plaintext credentials.
func assertNoPlantedSecretInBody(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	body := rec.Body.String()
	for _, secret := range []string{settingAuthAdminToken, settingAuthChannelKey, settingAuthSMTPPassword} {
		if strings.Contains(body, secret) {
			t.Fatalf("refused response leaked a plaintext secret %q: %s", secret, body)
		}
	}
	if strings.Contains(body, "sk-") {
		t.Fatalf("refused response looks like it carries an upstream key: %s", body)
	}
}

func assertRefusalBody(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status: got %d want %d, body=%s", rec.Code, wantStatus, rec.Body.String())
	}
	var body struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode refusal body %q: %v", rec.Body.String(), err)
	}
	if body.Message != errAdminLoginRequired {
		t.Fatalf("refusal wording must match the shared one: got %q want %q", body.Message, errAdminLoginRequired)
	}
}

// A script holding the long-lived admin access token must not be able to read the
// plaintext of the very credential it is using.
func TestSettingSecretRejectsAdminTokenSource(t *testing.T) {
	ctx := setupSettingAuthTestDB(t)
	seedSettingAuthSiteSecrets(t, ctx)

	rec := doSettingAuthRequest(newSettingGroupRouter(), http.MethodGet,
		"/api/v1/setting/secret?key=admin_access_token", adminTokenBearer(), nil)

	assertRefusalBody(t, rec, http.StatusForbidden)
	assertNoPlantedSecretInBody(t, rec)
}

// ...and it must not be able to download the whole-site backup either: that package
// carries the admin token, the SMTP password and every channel key in plaintext.
func TestExportDBRejectsAdminTokenSource(t *testing.T) {
	ctx := setupSettingAuthTestDB(t)
	seedSettingAuthSiteSecrets(t, ctx)

	rec := doSettingAuthRequest(newSettingGroupRouter(), http.MethodGet,
		"/api/v1/setting/export?include_logs=true&include_stats=true", adminTokenBearer(), nil)

	assertRefusalBody(t, rec, http.StatusForbidden)
	assertNoPlantedSecretInBody(t, rec)
	if got := rec.Header().Get("Content-Disposition"); got != "" {
		t.Fatalf("refused export must not offer a download, got Content-Disposition=%q", got)
	}
}

// ...nor be able to push an import, and the refused payload must not have been ingested:
// a malicious dump would otherwise rewrite the admin token through the same route.
func TestImportDBRejectsAdminTokenSource(t *testing.T) {
	ctx := setupSettingAuthTestDB(t)
	seedSettingAuthSiteSecrets(t, ctx)

	payload := []byte(`{"version":2,"settings":[{"key":"admin_access_token","value":"attacker-supplied-token-0123456789"}]}`)
	rec := doSettingAuthRequest(newSettingGroupRouter(), http.MethodPost,
		"/api/v1/setting/import", adminTokenBearer(), payload)

	assertRefusalBody(t, rec, http.StatusForbidden)
	assertNoPlantedSecretInBody(t, rec)

	stored, err := op.SettingGetString(model.SettingKeyAdminToken)
	if err != nil {
		t.Fatalf("read admin token: %v", err)
	}
	if stored != settingAuthAdminToken {
		t.Fatalf("refused import must not change the site token, got %q", stored)
	}
}

// Fail-closed: a request WITHOUT the marker, with an EMPTY marker, or with an unknown
// marker is refused on every guarded endpoint — the guard is a whitelist, not the old
// `!= "admin_token"`-style blacklist that let a missing field straight through.
func TestSecretExportImportFailClosedWithoutAdminLoginMarker(t *testing.T) {
	ctx := setupSettingAuthTestDB(t)
	seedSettingAuthSiteSecrets(t, ctx)

	cases := []struct {
		name        string
		stampMarker bool
		authMethod  string
	}{
		{name: "missing marker", stampMarker: false},
		{name: "empty marker", stampMarker: true, authMethod: ""},
		{name: "unknown future marker", stampMarker: true, authMethod: "sso"},
		{name: "case-variant marker", stampMarker: true, authMethod: "JWT"},
	}

	for _, tc := range cases {
		for _, target := range settingAuthGuardedTargets {
			t.Run(tc.name+" / "+target.name, func(t *testing.T) {
				engine := newMarkerlessSettingRouter(t, tc.stampMarker, tc.authMethod)
				rec := doSettingAuthRequest(engine, target.method, target.target, "any-bearer-value-0123456789", target.body)

				assertRefusalBody(t, rec, http.StatusForbidden)
				assertNoPlantedSecretInBody(t, rec)
			})
		}
	}
}

// The guard must not break the operator: a real admin login keeps the secret read and the
// full backup exactly as before (no new default redaction in the dump).
func TestSettingSecretAndExportAllowAdminLoginJWT(t *testing.T) {
	ctx := setupSettingAuthTestDB(t)
	admin := seedSettingAuthSiteSecrets(t, ctx)
	bearer := jwtBearer(t, admin)
	engine := newSettingGroupRouter()

	rec := doSettingAuthRequest(engine, http.MethodGet, "/api/v1/setting/secret?key=admin_access_token", bearer, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin login must read the secret, got %d body %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("secret response must stay uncacheable, got Cache-Control=%q", got)
	}
	var secretBody struct {
		Data struct {
			Key    string `json:"key"`
			Value  string `json:"value"`
			Source string `json:"source"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &secretBody); err != nil {
		t.Fatalf("decode secret body %q: %v", rec.Body.String(), err)
	}
	if secretBody.Data.Value != settingAuthAdminToken || secretBody.Data.Source != "setting" {
		t.Fatalf("admin login must get the plaintext from settings, got value=%q source=%q",
			secretBody.Data.Value, secretBody.Data.Source)
	}

	rec = doSettingAuthRequest(engine, http.MethodGet, "/api/v1/setting/export", bearer, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin login must still export, got %d body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), settingAuthAdminToken) {
		t.Fatalf("logged-in admin backup must stay a faithful (unredacted) migration package")
	}
}

// The same jwt path must still be able to import, so the guard is not a one-way door.
func TestImportDBAllowsAdminLoginJWT(t *testing.T) {
	ctx := setupSettingAuthTestDB(t)
	admin := seedSettingAuthSiteSecrets(t, ctx)

	payload := []byte(`{"version":2,"settings":[{"key":"email_smtp_from","value":"ops@upstream.example"}]}`)
	rec := doSettingAuthRequest(newSettingGroupRouter(), http.MethodPost,
		"/api/v1/setting/import", jwtBearer(t, admin), payload)

	if rec.Code != http.StatusOK {
		t.Fatalf("admin login must still import, got %d body %s", rec.Code, rec.Body.String())
	}
}
