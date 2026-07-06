package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/conf"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/gin-gonic/gin"
)

func adminTokenRouter() *gin.Engine {
	router := gin.New()
	router.GET("/api/v1/protected", Auth(), func(c *gin.Context) {
		user, _ := CurrentUser(c)
		c.JSON(http.StatusOK, gin.H{"admin": user.IsAdmin(), "uid": user.ID})
	})
	return router
}

func doAdminTokenRequest(bearer string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/protected", nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	adminTokenRouter().ServeHTTP(rec, req)
	return rec
}

func clearAdminTokenEnv(t *testing.T) {
	t.Helper()
	// Keep the env fallback from leaking a value into tests that assume it is unset.
	t.Setenv(strings.ToUpper(conf.APP_NAME)+"_ADMIN_TOKEN", "")
}

// A configured admin token authenticates and resolves to the REAL default admin
// user (genuine ID), so downstream handlers that key on user.ID work normally.
func TestAuthAdminTokenGrantsRealAdmin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupMiddlewareAuthDB(t)
	clearAdminTokenEnv(t)
	admin := createMiddlewareAuthUser(t, ctx)

	const token = "octopus-admin-token-abcdefghij-0123456789" // >= 24 chars
	if err := op.SettingSetString(model.SettingKeyAdminToken, token); err != nil {
		t.Fatalf("set admin token: %v", err)
	}

	rec := doAdminTokenRequest(token)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin token must authenticate, got %d body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Admin bool `json:"admin"`
		UID   int  `json:"uid"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if !body.Admin {
		t.Fatalf("admin token must resolve to an admin user")
	}
	if body.UID != admin.ID {
		t.Fatalf("admin token must resolve to the REAL admin id %d, got %d", admin.ID, body.UID)
	}
}

// The single most important guarantee: an UNSET admin token (empty default) is not a
// backdoor — no bearer, however long, can authenticate through the fallback.
func TestAuthAdminTokenEmptyIsNotBackdoor(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupMiddlewareAuthDB(t)
	clearAdminTokenEnv(t)
	createMiddlewareAuthUser(t, ctx)

	// Admin token setting stays at its empty default; an attacker's guess must fail.
	rec := doAdminTokenRequest("any-long-enough-guess-token-000000")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("empty admin token must NOT authenticate any bearer, got %d", rec.Code)
	}
}

// A configured-but-too-short token is refused, so a stray/weak value never becomes a
// bypass even if an operator fat-fingers it.
func TestAuthAdminTokenShortConfiguredRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupMiddlewareAuthDB(t)
	clearAdminTokenEnv(t)
	createMiddlewareAuthUser(t, ctx)

	if err := op.SettingSetString(model.SettingKeyAdminToken, "short"); err != nil {
		t.Fatalf("set admin token: %v", err)
	}
	if rec := doAdminTokenRequest("short"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("too-short admin token must be refused, got %d", rec.Code)
	}
}

// A wrong bearer against a valid configured token is rejected.
func TestAuthAdminTokenWrongTokenRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupMiddlewareAuthDB(t)
	clearAdminTokenEnv(t)
	createMiddlewareAuthUser(t, ctx)

	if err := op.SettingSetString(model.SettingKeyAdminToken, "octopus-admin-token-abcdefghij-0123456789"); err != nil {
		t.Fatalf("set admin token: %v", err)
	}
	if rec := doAdminTokenRequest("octopus-admin-token-WRONG-value-99999999"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong admin token must be rejected, got %d", rec.Code)
	}
}
