package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/gin-gonic/gin"
)

func TestAPIKeyAuthAcceptsGeminiQueryKeyAndStripsIt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupMiddlewareAuthDB(t)

	user := createMiddlewareAuthUser(t, ctx)
	key := createMiddlewareAuthAPIKey(t, ctx, user, "gemini", "sk-octopus-gemini-test", []model.APIKeyEndpointFamily{
		model.APIKeyEndpointFamilyGemini,
	})

	router := gin.New()
	router.POST("/v1beta/models/*modelAction", APIKeyAuth(), func(c *gin.Context) {
		if got := c.GetString("request_type"); got != "gemini" {
			t.Fatalf("request_type: got %q want gemini", got)
		}
		if got := c.GetString("endpoint_family"); got != string(model.APIKeyEndpointFamilyGemini) {
			t.Fatalf("endpoint_family: got %q want gemini", got)
		}
		if got := c.Query("key"); got != "" {
			t.Fatalf("expected query key to be stripped, got %q", got)
		}
		if got := c.Query("alt"); got != "sse" {
			t.Fatalf("expected other query params to remain, got alt=%q", got)
		}
		c.Status(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-pro:generateContent?key="+key.APIKey+"&alt=sse", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected auth success, got %d body %s", rec.Code, rec.Body.String())
	}
}

func TestAPIKeyAuthLegacyKeyAllowsAllEndpointFamilies(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupMiddlewareAuthDB(t)
	user := createMiddlewareAuthUser(t, ctx)
	key := createMiddlewareAuthAPIKey(t, ctx, user, "legacy", "sk-octopus-legacy-test", nil)
	router := newEndpointFamilyAuthRouter()

	tests := []struct {
		name   string
		method string
		target string
		header string
		value  string
	}{
		{
			name:   "openai compatible",
			method: http.MethodPost,
			target: "/v1/chat/completions",
			header: "Authorization",
			value:  "Bearer " + key.APIKey,
		},
		{
			name:   "anthropic",
			method: http.MethodPost,
			target: "/v1/messages",
			header: "x-api-key",
			value:  key.APIKey,
		},
		{
			name:   "gemini",
			method: http.MethodPost,
			target: "/v1beta/models/gemini-pro:streamGenerateContent",
			header: "x-goog-api-key",
			value:  key.APIKey,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := performAuthRequest(router, tt.method, tt.target, tt.header, tt.value)
			if rec.Code != http.StatusNoContent {
				t.Fatalf("expected auth success, got %d body %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestAPIKeyAuthScopedKeyAllowsEnabledFamilyAndBlocksDisabledFamily(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupMiddlewareAuthDB(t)
	user := createMiddlewareAuthUser(t, ctx)
	key := createMiddlewareAuthAPIKey(t, ctx, user, "openai only", "sk-octopus-openai-only-test", []model.APIKeyEndpointFamily{
		model.APIKeyEndpointFamilyOpenAICompatible,
	})
	router := newEndpointFamilyAuthRouter()

	rec := performAuthRequest(router, http.MethodPost, "/v1/chat/completions", "Authorization", "Bearer "+key.APIKey)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected openai-compatible endpoint to pass, got %d body %s", rec.Code, rec.Body.String())
	}

	for _, target := range []string{
		"/responses",
		"/responses/compact",
		"/backend-api/codex/responses",
		"/backend-api/codex/responses/compact",
	} {
		rec = performAuthRequest(router, http.MethodPost, target, "Authorization", "Bearer "+key.APIKey)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("expected openai-compatible alias %s to pass, got %d body %s", target, rec.Code, rec.Body.String())
		}
	}

	rec = performAuthRequest(router, http.MethodPost, "/v1/messages", "x-api-key", key.APIKey)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected anthropic endpoint to be forbidden, got %d body %s", rec.Code, rec.Body.String())
	}
	assertResponseErrorCode(t, rec, "endpoint_not_allowed")

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-pro:generateContent?key="+key.APIKey, nil)
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected gemini endpoint to be forbidden, got %d body %s", rec.Code, rec.Body.String())
	}
	assertResponseErrorCode(t, rec, "endpoint_not_allowed")
}

func TestAPIKeyEndpointFamilyForCodexResponsesAliases(t *testing.T) {
	for _, path := range []string{
		"/responses",
		"/responses/compact",
		"/backend-api/codex/responses",
		"/backend-api/codex/responses/compact",
	} {
		if got := apiKeyEndpointFamilyForPath(path); got != model.APIKeyEndpointFamilyOpenAICompatible {
			t.Fatalf("path %s endpoint family: got %q want %q", path, got, model.APIKeyEndpointFamilyOpenAICompatible)
		}
	}
}

func TestAPIKeyEndpointFamiliesCreateUpdateListGetPreserveField(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupMiddlewareAuthDB(t)
	user := createMiddlewareAuthUser(t, ctx)
	key := createMiddlewareAuthAPIKey(t, ctx, user, "anthropic only", "sk-octopus-anthropic-only-test", []model.APIKeyEndpointFamily{
		model.APIKeyEndpointFamilyAnthropic,
	})

	var stored model.APIKey
	if err := db.GetDB().WithContext(ctx).First(&stored, key.ID).Error; err != nil {
		t.Fatalf("load stored api key: %v", err)
	}
	assertEndpointFamilies(t, stored.EndpointFamilies, []model.APIKeyEndpointFamily{model.APIKeyEndpointFamilyAnthropic})

	got, err := op.APIKeyGet(key.ID, ctx)
	if err != nil {
		t.Fatalf("get api key: %v", err)
	}
	assertEndpointFamilies(t, got.EndpointFamilies, []model.APIKeyEndpointFamily{model.APIKeyEndpointFamilyAnthropic})

	list, err := op.APIKeyList(ctx, user)
	if err != nil {
		t.Fatalf("list api keys: %v", err)
	}
	listed := findAPIKeyByID(t, list, key.ID)
	assertEndpointFamilies(t, listed.EndpointFamilies, []model.APIKeyEndpointFamily{model.APIKeyEndpointFamilyAnthropic})

	got.Name = "anthropic only renamed"
	got.EndpointFamilies = nil
	if err := op.APIKeyUpdate(&got, user, ctx); err != nil {
		t.Fatalf("update api key without endpoint families: %v", err)
	}
	preserved, err := op.APIKeyGet(key.ID, ctx)
	if err != nil {
		t.Fatalf("get preserved api key: %v", err)
	}
	assertEndpointFamilies(t, preserved.EndpointFamilies, []model.APIKeyEndpointFamily{model.APIKeyEndpointFamilyAnthropic})

	preserved.Name = "legacy all"
	preserved.EndpointFamilies = []model.APIKeyEndpointFamily{}
	if err := op.APIKeyUpdate(&preserved, user, ctx); err != nil {
		t.Fatalf("clear endpoint families: %v", err)
	}
	legacy, err := op.APIKeyGet(key.ID, ctx)
	if err != nil {
		t.Fatalf("get legacy api key: %v", err)
	}
	if len(legacy.EndpointFamilies) != 0 {
		t.Fatalf("expected empty endpoint families to be stored as legacy all-allowed, got %#v", legacy.EndpointFamilies)
	}
	for _, family := range model.AllAPIKeyEndpointFamilies() {
		if !legacy.AllowsEndpointFamily(family) {
			t.Fatalf("expected legacy key to allow %s", family)
		}
	}
}

func setupMiddlewareAuthDB(t *testing.T) context.Context {
	t.Helper()
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "octopus.db"), false); err != nil {
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

func createMiddlewareAuthUser(t *testing.T, ctx context.Context) model.User {
	t.Helper()
	user, err := op.UserCreate(model.UserCreateRequest{
		Username: "admin",
		Password: "password",
		Role:     model.UserRoleAdmin,
		Status:   model.UserStatusActive,
	}, ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return user
}

func createMiddlewareAuthAPIKey(t *testing.T, ctx context.Context, user model.User, name string, secret string, families []model.APIKeyEndpointFamily) model.APIKey {
	t.Helper()
	key := model.APIKey{
		UserID:           user.ID,
		Name:             name,
		APIKey:           secret,
		Enabled:          true,
		EndpointFamilies: families,
	}
	if err := op.APIKeyCreate(&key, ctx); err != nil {
		t.Fatalf("create api key: %v", err)
	}
	return key
}

func findAPIKeyByID(t *testing.T, keys []model.APIKey, id int) model.APIKey {
	t.Helper()
	for _, key := range keys {
		if key.ID == id {
			return key
		}
	}
	t.Fatalf("api key %d not found in list", id)
	return model.APIKey{}
}

func assertEndpointFamilies(t *testing.T, got []model.APIKeyEndpointFamily, want []model.APIKeyEndpointFamily) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("endpoint families length: got %#v want %#v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("endpoint families: got %#v want %#v", got, want)
		}
	}
}

func newEndpointFamilyAuthRouter() *gin.Engine {
	router := gin.New()
	handler := func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	}
	router.POST("/v1/chat/completions", APIKeyAuth(), handler)
	router.GET("/v1/models", APIKeyAuth(), handler)
	router.POST("/responses", APIKeyAuth(), handler)
	router.POST("/responses/compact", APIKeyAuth(), handler)
	router.POST("/backend-api/codex/responses", APIKeyAuth(), handler)
	router.POST("/backend-api/codex/responses/compact", APIKeyAuth(), handler)
	router.POST("/v1/messages", APIKeyAuth(), handler)
	router.POST("/v1beta/models/*modelAction", APIKeyAuth(), handler)
	return router
}

func performAuthRequest(router *gin.Engine, method string, target string, header string, value string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	if header != "" {
		req.Header.Set(header, value)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func assertResponseErrorCode(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	var body struct {
		ErrorCode string `json:"error_code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response body %q: %v", rec.Body.String(), err)
	}
	if body.ErrorCode != want {
		t.Fatalf("error_code: got %q want %q in body %s", body.ErrorCode, want, rec.Body.String())
	}
}
