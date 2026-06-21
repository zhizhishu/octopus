package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"testing"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/gin-gonic/gin"
)

// TestModelRouteDispatch wires the same route shapes used in init() with stub
// handlers to confirm the static list route GET /v1/models and the param route
// GET /v1/models/:id coexist and dispatch independently (the new single-model
// route must not shadow or regress the list route).
func TestModelRouteDispatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	grp := engine.Group("/v1")
	grp.GET("/models", func(c *gin.Context) { c.String(http.StatusOK, "list") })
	grp.GET("/models/:id", func(c *gin.Context) { c.String(http.StatusOK, "get:%s", c.Param("id")) })

	cases := []struct {
		path string
		want string
	}{
		{"/v1/models", "list"},
		{"/v1/models/gpt-test", "get:gpt-test"},
		{"/v1/models/claude-3-5-sonnet", "get:claude-3-5-sonnet"},
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		engine.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status %d body=%s", tc.path, w.Code, w.Body.String())
		}
		if w.Body.String() != tc.want {
			t.Fatalf("%s: got %q want %q", tc.path, w.Body.String(), tc.want)
		}
	}
}

// TestGetModelReturnsObjectForSupportedModel seeds an enabled channel + group so
// the API key's allowed model set contains "gpt-test", then verifies the single
// model endpoint returns the OpenAI-format object for that id.
func TestGetModelReturnsObjectForSupportedModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupModelGetDB(t)
	user := createModelGetUser(t, ctx)
	apiKey := createModelGetAPIKey(t, ctx, user)
	seedModel(t, ctx, "gpt-test")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models/gpt-test", nil)
	c.Set("api_key_id", apiKey.ID)
	c.Params = gin.Params{{Key: "id", Value: "gpt-test"}}

	getModel(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200, body=%s", w.Code, w.Body.String())
	}
	var out model.OpenAIModel
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v body=%s", err, w.Body.String())
	}
	if out.ID != "gpt-test" {
		t.Fatalf("id: got %q want gpt-test", out.ID)
	}
	if out.Object != "model" {
		t.Fatalf("object: got %q want model", out.Object)
	}
	if out.OwnedBy != "octopus" {
		t.Fatalf("owned_by: got %q want octopus", out.OwnedBy)
	}
}

// TestGetModelNotFoundForUnsupportedModel verifies a model id not in the key's
// allowed set returns 404.
func TestGetModelNotFoundForUnsupportedModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupModelGetDB(t)
	user := createModelGetUser(t, ctx)
	apiKey := createModelGetAPIKey(t, ctx, user)
	seedModel(t, ctx, "gpt-test")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models/does-not-exist", nil)
	c.Set("api_key_id", apiKey.ID)
	c.Params = gin.Params{{Key: "id", Value: "does-not-exist"}}

	getModel(c)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404, body=%s", w.Code, w.Body.String())
	}
}

// TestGetModelListStillReturnsSeededModel guards against a regression of the
// list endpoint after adding the single-model route: the seeded model must
// still appear in GET /v1/models.
func TestGetModelListStillReturnsSeededModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupModelGetDB(t)
	user := createModelGetUser(t, ctx)
	apiKey := createModelGetAPIKey(t, ctx, user)
	seedModel(t, ctx, "gpt-test")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	c.Set("api_key_id", apiKey.ID)

	getModelList(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200, body=%s", w.Code, w.Body.String())
	}
	var out struct {
		Data []model.OpenAIModel `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v body=%s", err, w.Body.String())
	}
	found := false
	for _, m := range out.Data {
		if m.ID == "gpt-test" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected seeded model gpt-test in list, got %s", w.Body.String())
	}
}

// TestModelListFiltersByClientProtocol guards the fix for the regression where
// GET /v1/models was path-classified as openai-compatible, so the endpoint-family
// filter hid every model served only by an Anthropic channel (e.g. Claude) from
// x-api-key (request_type "anthropic") clients like CherryStudio. The list must
// follow the client's protocol: an Anthropic client sees the Anthropic-channel
// model; an OpenAI client does not.
func TestModelListFiltersByClientProtocol(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupModelGetDB(t)
	user := createModelGetUser(t, ctx)
	apiKey := createModelGetAPIKey(t, ctx, user)

	channel := model.Channel{Name: "anthropic-ch", Enabled: true, Type: outbound.OutboundTypeAnthropic}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	group := model.Group{
		Name: "claude-opus-4-8",
		Mode: model.GroupModeFailover,
		Items: []model.GroupItem{
			{ChannelID: channel.ID, ModelName: "claude-opus-4-8"},
		},
	}
	if err := op.GroupCreate(&group, ctx); err != nil {
		t.Fatalf("create group: %v", err)
	}

	listIDs := func(requestType string) []string {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		c.Set("api_key_id", apiKey.ID)
		if requestType != "" {
			c.Set("request_type", requestType)
		}
		getModelList(c)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status %d body=%s", requestType, w.Code, w.Body.String())
		}
		var out struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s: decode %v body=%s", requestType, err, w.Body.String())
		}
		ids := make([]string, 0, len(out.Data))
		for _, m := range out.Data {
			ids = append(ids, m.ID)
		}
		return ids
	}
	if anth := listIDs("anthropic"); !slices.Contains(anth, "claude-opus-4-8") {
		t.Fatalf("anthropic client must see claude-opus-4-8, got %v", anth)
	}
	if oai := listIDs("openai"); slices.Contains(oai, "claude-opus-4-8") {
		t.Fatalf("openai client must NOT see anthropic-only claude-opus-4-8, got %v", oai)
	}
}

func setupModelGetDB(t *testing.T) context.Context {
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

func createModelGetUser(t *testing.T, ctx context.Context) model.User {
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

func createModelGetAPIKey(t *testing.T, ctx context.Context, user model.User) model.APIKey {
	t.Helper()
	key := model.APIKey{
		UserID:  user.ID,
		Name:    "model get",
		APIKey:  "sk-octopus-model-get-test",
		Enabled: true,
	}
	if err := op.APIKeyCreate(&key, ctx); err != nil {
		t.Fatalf("create api key: %v", err)
	}
	return key
}

// seedModel makes modelName appear in the API key's allowed model set by
// creating an enabled channel and a group (named after the model) with one item
// referencing that channel. This mirrors how GET /v1/models derives its list.
func seedModel(t *testing.T, ctx context.Context, modelName string) {
	t.Helper()
	channel := model.Channel{
		Name:    "test-channel",
		Enabled: true,
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	group := model.Group{
		Name: modelName,
		Mode: model.GroupModeFailover,
		Items: []model.GroupItem{
			{ChannelID: channel.ID, ModelName: modelName},
		},
	}
	if err := op.GroupCreate(&group, ctx); err != nil {
		t.Fatalf("create group: %v", err)
	}
}
