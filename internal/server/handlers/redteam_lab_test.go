package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/redteamlab"
	"github.com/zhizhishu/octopus/internal/server/middleware"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

func newRedTeamLabRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	group := r.Group("/api/v1/redteam-lab", middleware.Auth(), middleware.AdminOnly())
	group.GET("/config", redTeamLabGetConfig)
	group.PUT("/config", redTeamLabSaveConfig)
	group.GET("/runs", redTeamLabListRuns)
	group.POST("/run", redTeamLabRun)
	settings := r.Group("/api/v1/setting", middleware.Auth(), middleware.AdminOnly())
	settings.POST("/set", setSetting)
	return r
}
func redTeamRequest(r *gin.Engine, method, target, token string, data any) *httptest.ResponseRecorder {
	var body []byte
	if data != nil {
		body, _ = json.Marshal(data)
	}
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	if data != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res := httptest.NewRecorder()
	r.ServeHTTP(res, req)
	return res
}
func TestRedTeamLabRealAdminGuardAndOneShapedRequest(t *testing.T) {
	ctx := setupSettingAuthTestDB(t)
	admin := seedSettingAuthSiteSecrets(t, ctx)
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPost {
			t.Errorf("upstream request method %q", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer sk-example-upstream-test" {
			t.Errorf("upstream auth missing expected test value")
		}
		payload, _ := io.ReadAll(r.Body)
		if !bytes.Contains(payload, []byte("OCT_BOUNDARY_TEST_001")) || !bytes.Contains(payload, []byte("model-a")) {
			t.Errorf("wrong user prompt or model in outbound payload: %s", payload)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-redteam","object":"chat.completion","model":"model-a","choices":[{"index":0,"message":{"role":"assistant","content":"OCT_BOUNDARY_MARKER"},"finish_reason":"stop"}],"usage":{"prompt_tokens":14,"completion_tokens":5,"total_tokens":19}}`))
	}))
	defer upstream.Close()
	channel := model.Channel{
		Name: "redteam-stub-channel", Type: outbound.OutboundTypeOpenAIChat, Enabled: true,
		Model: "model-a", BaseUrls: []model.BaseUrl{{URL: upstream.URL}},
		Keys: []model.ChannelKey{{Enabled: true, ChannelKey: "sk-example-upstream-test"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatal(err)
	}
	r := newRedTeamLabRouter()
	bearer := jwtBearer(t, admin)
	key := "redteam-test-case"
	cfg := redteamlab.Config{Enabled: true, Cases: []redteamlab.Case{{
		ID: key, Name: "授权检测", Prompt: "OCT_BOUNDARY_TEST_001 please answer the marker.", Expected: "OCT_BOUNDARY_MARKER", MaxTokens: 64,
	}}}
	payload := map[string]any{"channel_id": channel.ID, "model": "model-a", "case_id": key, "confirm_cost": true}

	// The admin automation token cannot turn on/send tests.
	if got := redTeamRequest(r, http.MethodPut, "/api/v1/redteam-lab/config", adminTokenBearer(), cfg); got.Code != http.StatusForbidden {
		t.Fatalf("admin automation token wrote config: %d %s", got.Code, got.Body)
	}
	if got := redTeamRequest(r, http.MethodPost, "/api/v1/redteam-lab/run", adminTokenBearer(), payload); got.Code != http.StatusForbidden {
		t.Fatalf("automation token ran test: %d", got.Code)
	}
	indirect := model.Setting{Key: model.SettingKeyRedTeamLabConfig, Value: `{"enabled":true,"cases":[]}`}
	if got := redTeamRequest(r, http.MethodPost, "/api/v1/setting/set", adminTokenBearer(), indirect); got.Code != http.StatusForbidden {
		t.Fatalf("automation token changed lab via generic setting route: %d %s", got.Code, got.Body)
	}

	// Saving a disabled-by-default configuration must be an explicit action.
	if got := redTeamRequest(r, http.MethodPost, "/api/v1/redteam-lab/run", bearer, payload); got.Code != http.StatusForbidden {
		t.Fatalf("ran before enable: %d %s", got.Code, got.Body)
	}

	if got := redTeamRequest(r, http.MethodPut, "/api/v1/redteam-lab/config", bearer, cfg); got.Code != http.StatusOK {
		t.Fatalf("save: %d %s", got.Code, got.Body)
	}
	if got := redTeamRequest(r, http.MethodGet, "/api/v1/redteam-lab/config", bearer, nil); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), key) {
		t.Fatalf("load saved: %d %s", got.Code, got.Body)
	}

	noConfirm := map[string]any{"channel_id": channel.ID, "model": "model-a", "case_id": key, "confirm_cost": false}
	if got := redTeamRequest(r, http.MethodPost, "/api/v1/redteam-lab/run", bearer, noConfirm); got.Code != http.StatusBadRequest {
		t.Fatalf("run without confirmation: %d", got.Code)
	}
	if count := requests.Load(); count != 0 {
		t.Fatalf("cost without confirmation: %d", count)
	}

	payload["model"] = "model-not-enabled-for-channel"
	if rejected := redTeamRequest(r, http.MethodPost, "/api/v1/redteam-lab/run", bearer, payload); rejected.Code != http.StatusBadRequest {
		t.Fatalf("unconfigured channel model allowed: %d %s", rejected.Code, rejected.Body)
	}
	if requests.Load() != 0 {
		t.Fatal("invalid model caused a paid upstream request")
	}
	payload["model"] = "model-a"

	got := redTeamRequest(r, http.MethodPost, "/api/v1/redteam-lab/run", bearer, payload)
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), "OCT_BOUNDARY_MARKER") || !strings.Contains(got.Body.String(), `"matched"`) {
		t.Fatalf("real channel run failed: %d %s", got.Code, got.Body)
	}
	if count := requests.Load(); count != 1 {
		t.Fatalf("expected exactly one billable send, got %d", count)
	}
	history := redTeamRequest(r, http.MethodGet, "/api/v1/redteam-lab/runs", bearer, nil)
	if history.Code != http.StatusOK || !strings.Contains(history.Body.String(), "OCT_BOUNDARY_MARKER") {
		t.Fatalf("history not saved: %d %s", history.Code, history.Body)
	}
	// Invalid case must be fail closed and must not send.
	payload["case_id"] = "missing"
	if got := redTeamRequest(r, http.MethodPost, "/api/v1/redteam-lab/run", bearer, payload); got.Code != http.StatusBadRequest {
		t.Fatalf("unknown case accepted: %d", got.Code)
	}
	if requests.Load() != 1 {
		t.Fatal("invalid case caused upstream request")
	}
}

func TestRedTeamLabInvalidConfigDoesNotSaveAndIsJWTOnly(t *testing.T) {
	ctx := setupSettingAuthTestDB(t)
	admin := seedSettingAuthSiteSecrets(t, ctx)
	r := newRedTeamLabRouter()
	token := jwtBearer(t, admin)
	bad := redteamlab.Config{Enabled: true, Cases: []redteamlab.Case{{ID: "invalid/path", Name: "bad", Prompt: "short request", MaxTokens: 32}}}
	if got := redTeamRequest(r, http.MethodPut, "/api/v1/redteam-lab/config", token, bad); got.Code != http.StatusBadRequest {
		t.Fatalf("invalid config allowed: %d", got.Code)
	}
	if cfg, err := loadRedTeamLabConfig(); err != nil || cfg.Enabled {
		t.Fatalf("invalid setting leaked into config: %+v %v", cfg, err)
	}
	// A valid, non-admin user must not access records or execute any test.
	user, err := op.UserCreate(model.UserCreateRequest{
		Username: "ordinary-redteam", Password: "password", Role: model.UserRoleUser, Status: model.UserStatusActive,
	}, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	userToken := jwtBearer(t, user)
	for _, tc := range []struct {
		method, path string
		data         any
	}{
		{http.MethodGet, "/api/v1/redteam-lab/config", nil},
		{http.MethodGet, "/api/v1/redteam-lab/runs", nil},
		{http.MethodPost, "/api/v1/redteam-lab/run", map[string]any{"confirm_cost": true}},
	} {
		got := redTeamRequest(r, tc.method, tc.path, userToken, tc.data)
		if got.Code != http.StatusForbidden {
			t.Fatalf("user allowed: %s %s -> %d", tc.method, tc.path, got.Code)
		}
	}
}
