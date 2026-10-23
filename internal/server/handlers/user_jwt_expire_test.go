package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/server/middleware"
	"github.com/gin-gonic/gin"
)

// maxLoginJWTTTL is the ceiling a login/register response may ever hand out. Kept as a
// literal here on purpose: the test must fail if the production constant is widened
// silently.
const maxLoginJWTTTL = 30 * 24 * time.Hour

func setupUserJWTExpireTestDB(t *testing.T) context.Context {
	t.Helper()
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "user_jwt_expire_test.db"), false); err != nil {
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
	ctx := context.Background()
	if _, err := op.UserCreate(model.UserCreateRequest{
		Username: "admin",
		Password: "password",
		Role:     model.UserRoleAdmin,
		Status:   model.UserStatusActive,
	}, ctx); err != nil {
		t.Fatalf("create user: %v", err)
	}
	return ctx
}

// newLoginRouter mounts the real login handler behind the same middleware chain the
// production /api/v1/user/login group uses, plus an Auth()-guarded probe so a minted
// token is exercised through the real verification path.
func newLoginRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	grp := engine.Group("/api/v1/user", middleware.RequireJSON())
	grp.POST("/login", login)
	engine.GET("/probe", middleware.Auth(), func(c *gin.Context) { c.Status(http.StatusNoContent) })
	return engine
}

func postLogin(t *testing.T, engine *gin.Engine, body string) (model.UserLoginResponse, *httptest.ResponseRecorder) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/login", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return model.UserLoginResponse{}, rec
	}
	var envelope struct {
		Data model.UserLoginResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode login body %q: %v", rec.Body.String(), err)
	}
	return envelope.Data, rec
}

func probeAuthorized(engine *gin.Engine, token string) int {
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec.Code
}

// A caller-supplied "expire" must never mint a token that outlives the 30-day ceiling.
// Pre-fix, expire=999999999 produced a token valid for ~1900 years.
func TestLoginCapsRequestedTokenTTL(t *testing.T) {
	setupUserJWTExpireTestDB(t)
	engine := newLoginRouter()

	for _, expire := range []string{"999999999", "43201", "1000000000000"} {
		login, rec := postLogin(t, engine, `{"username":"admin","password":"password","expire":`+expire+`}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("expire=%s: login failed with %d body %s", expire, rec.Code, rec.Body.String())
		}
		expireAt, err := time.Parse(time.RFC3339, login.ExpireAt)
		if err != nil {
			t.Fatalf("expire=%s: parse expire_at %q: %v", expire, login.ExpireAt, err)
		}
		if ttl := time.Until(expireAt); ttl > maxLoginJWTTTL {
			t.Fatalf("expire=%s: issued token lives %s, want <= %s", expire, ttl, maxLoginJWTTTL)
		}
		if code := probeAuthorized(engine, login.Token); code != http.StatusNoContent {
			t.Fatalf("expire=%s: capped token rejected by Auth (%d)", expire, code)
		}
	}
}

// A negative "expire" other than -1 must not produce an exp-less token (which never
// expires) nor a nil-pointer panic on the expire_at field.
func TestLoginNegativeExpireStillExpires(t *testing.T) {
	setupUserJWTExpireTestDB(t)
	engine := newLoginRouter()

	login, rec := postLogin(t, engine, `{"username":"admin","password":"password","expire":-5}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expire=-5: login failed with %d body %s", rec.Code, rec.Body.String())
	}
	expireAt, err := time.Parse(time.RFC3339, login.ExpireAt)
	if err != nil {
		t.Fatalf("parse expire_at %q: %v", login.ExpireAt, err)
	}
	if ttl := time.Until(expireAt); ttl <= 0 || ttl > maxLoginJWTTTL {
		t.Fatalf("expire=-5: issued token lives %s, want (0, %s]", ttl, maxLoginJWTTTL)
	}
	if code := probeAuthorized(engine, login.Token); code != http.StatusNoContent {
		t.Fatalf("expire=-5: token rejected by Auth (%d)", code)
	}
}

// The ordinary login must keep working: no over-tight cap, and the token still passes
// the real Auth() middleware (anti-regression).
func TestLoginDefaultExpireStillWorks(t *testing.T) {
	setupUserJWTExpireTestDB(t)
	engine := newLoginRouter()

	for _, body := range []string{
		`{"username":"admin","password":"password"}`,
		`{"username":"admin","password":"password","expire":0}`,
		`{"username":"admin","password":"password","expire":-1}`,
		`{"username":"admin","password":"password","expire":60}`,
	} {
		login, rec := postLogin(t, engine, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: login failed with %d body %s", body, rec.Code, rec.Body.String())
		}
		expireAt, err := time.Parse(time.RFC3339, login.ExpireAt)
		if err != nil {
			t.Fatalf("%s: parse expire_at %q: %v", body, login.ExpireAt, err)
		}
		if ttl := time.Until(expireAt); ttl <= 0 || ttl > maxLoginJWTTTL {
			t.Fatalf("%s: issued token lives %s, want (0, %s]", body, ttl, maxLoginJWTTTL)
		}
		if code := probeAuthorized(engine, login.Token); code != http.StatusNoContent {
			t.Fatalf("%s: valid token rejected by Auth (%d)", body, code)
		}
	}
}
