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

// TestGeminiAllowedModelsReturnsAllChannelTypes guards the inbound Gemini
// /v1beta/models list against regressing back to filtering by endpoint family.
//
// OpenAI /v1/models (apiKeyAllowedModels in model.go) and Anthropic inbound
// lists deliberately do NOT filter by channel type (see
// TestModelListShowsModelsAcrossChannelTypes and the "Deliberately NOT
// filtered" comment in both model.go and gemini.go). Octopus transforms
// cross-protocol, so a Gemini inbound key must see models served by chat,
// anthropic and custom channels alike — not only Gemini-channel models.
//
// The prior `endpoint_family` filter called FilterModelNamesForEndpointFamily,
// which (for APIKeyEndpointFamilyGemini) only keeps models whose backing
// channel is OutboundTypeGemini. That hid every non-Gemini-channel model from
// Gemini-native clients (e.g. Cherry Studio's Gemini mode), breaking
// cross-protocol routing on the discovery path even though the request path
// supported it.
//
// This test seeds one model exclusively on each of three channel types
// (gemini / chat / anthropic), then asserts GET /v1beta/models surfaces ALL
// three for a Gemini endpoint-family key. If the endpoint_family filter is
// re-introduced, the chat/anthropic-only models drop out and the test fails.
// (the test was added during the P0-1 + endpoint_family filter removal fix).
func TestGeminiAllowedModelsReturnsAllChannelTypes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupGeminiModelsDB(t)
	user := createGeminiModelsUser(t, ctx)
	apiKey := createGeminiModelsAPIKey(t, ctx, user)

	// Each model lives EXCLUSIVELY on one channel type, so the prior
	// endpoint_family=gemini filter would drop chat-only and anthropic-only
	// models (no Gemini-channel item to satisfy it).
	type channelSeed struct {
		modelName string
		name      string
		obType    outbound.OutboundType
	}
	seeds := []channelSeed{
		{modelName: "model-on-gemini", name: "gemini-channel", obType: outbound.OutboundTypeGemini},
		{modelName: "model-on-chat", name: "chat-channel", obType: outbound.OutboundTypeOpenAIChat},
		{modelName: "model-on-anthropic", name: "anthropic-channel", obType: outbound.OutboundTypeAnthropic},
	}

	for _, s := range seeds {
		channel := model.Channel{Name: s.name, Enabled: true, Type: s.obType}
		if err := op.ChannelCreate(&channel, ctx); err != nil {
			t.Fatalf("create channel %s: %v", s.name, err)
		}
		// Group name == model name so GroupListModelForAPIKeyPlan surfaces the
		// model name (it dedups on group name). One item, one channel — the
		// model is exclusively on this channel type.
		group := model.Group{
			Name: s.modelName,
			Mode: model.GroupModeFailover,
			Items: []model.GroupItem{
				{ChannelID: channel.ID, ModelName: s.modelName},
			},
		}
		if err := op.GroupCreate(&group, ctx); err != nil {
			t.Fatalf("create group %s: %v", s.modelName, err)
		}
	}

	// List via GET /v1beta/models with the Gemini endpoint family set (mirrors
	// middleware.APIKeyAuth tagging a Gemini-native key).
	listIDs := func(endpointFamily string) []string {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/v1beta/models", nil)
		c.Set("api_key_id", apiKey.ID)
		if endpointFamily != "" {
			c.Set("endpoint_family", endpointFamily)
		}
		geminiListModels(c)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status %d body=%s", endpointFamily, w.Code, w.Body.String())
		}
		var out geminiModelListResponse
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s: decode %v body=%s", endpointFamily, err, w.Body.String())
		}
		ids := make([]string, 0, len(out.Models))
		for _, m := range out.Models {
			ids = append(ids, m.Name)
		}
		return ids
	}

	// A Gemini-family key must see models from EVERY channel type — not only
	// the Gemini-channel one. The prior filter would have returned only
	// "model-on-gemini" for endpoint_family=gemini.
	for _, ef := range []string{string(model.APIKeyEndpointFamilyGemini), ""} {
		ids := listIDs(ef)
		for _, want := range []string{"model-on-gemini", "model-on-chat", "model-on-anthropic"} {
			if !slices.Contains(ids, want) {
				t.Fatalf("endpoint_family %q must list %q across all channel types, got %v", ef, want, ids)
			}
		}
	}
}

func setupGeminiModelsDB(t *testing.T) context.Context {
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

func createGeminiModelsUser(t *testing.T, ctx context.Context) model.User {
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

func createGeminiModelsAPIKey(t *testing.T, ctx context.Context, user model.User) model.APIKey {
	t.Helper()
	key := model.APIKey{
		UserID:  user.ID,
		Name:    "gemini models test",
		APIKey:  "sk-octopus-gemini-models-test",
		Enabled: true,
	}
	if err := op.APIKeyCreate(&key, ctx); err != nil {
		t.Fatalf("create api key: %v", err)
	}
	return key
}
