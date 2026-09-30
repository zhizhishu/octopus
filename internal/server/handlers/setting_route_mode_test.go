package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/server/middleware"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func setupRouteModeHandlerTest(t *testing.T) (*gin.Engine, string) {
	t.Helper()
	ctx := setupSettingAuthTestDB(t)
	admin, err := op.UserCreate(model.UserCreateRequest{
		Username: "route-mode-admin", Password: "test-password", Role: model.UserRoleAdmin, Status: model.UserStatusActive,
	}, ctx)
	if err != nil {
		t.Fatal(err)
	}
	engine := newSettingGroupRouter()
	// Reuse the production authentication chain for both ordinary setting endpoints.
	grp := engine.Group("/api/v1/setting", middleware.Auth(), middleware.AdminOnly())
	grp.GET("/list", getSettingList)
	grp.POST("/set", setSetting)
	return engine, jwtBearer(t, admin)
}

func assertHandlerRouteMode(t *testing.T, wantDB, wantCache string) {
	t.Helper()
	var stored model.Setting
	if err := db.GetDB().First(&stored, "key = ?", model.SettingKeyRouteModeOverride).Error; err != nil {
		t.Fatal(err)
	}
	cached, err := op.SettingGetString(model.SettingKeyRouteModeOverride)
	if err != nil || cached != wantCache || stored.Value != wantDB {
		t.Fatalf("route mode DB=%q cache=%q err=%v; want DB=%q cache=%q", stored.Value, cached, err, wantDB, wantCache)
	}
}

func routeModePayload(value string) []byte {
	body, _ := json.Marshal(model.Setting{Key: model.SettingKeyRouteModeOverride, Value: value})
	return body
}

func TestRouteModeSettingResponseIsCanonicalAndRejectsInvalid(t *testing.T) {
	engine, bearer := setupRouteModeHandlerTest(t)
	for _, tc := range []struct{ raw, want string }{
		{" SPREAD ", "spread"}, {"", "fill_first"}, {" Fill_First ", "fill_first"},
	} {
		rec := doSettingAuthRequest(engine, http.MethodPost, "/api/v1/setting/set", bearer, routeModePayload(tc.raw))
		if rec.Code != http.StatusOK {
			t.Fatalf("set %q: status=%d body=%s", tc.raw, rec.Code, rec.Body.String())
		}
		var response struct {
			Data model.Setting `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.Data.Value != tc.want {
			t.Fatalf("set response=%q, want %q", response.Data.Value, tc.want)
		}
		assertHandlerRouteMode(t, tc.want, tc.want)
	}
	for _, raw := range []string{"smart", "round_robin"} {
		rec := doSettingAuthRequest(engine, http.MethodPost, "/api/v1/setting/set", bearer, routeModePayload(raw))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid mode %q: status=%d", raw, rec.Code)
		}
		assertHandlerRouteMode(t, "fill_first", "fill_first")
	}
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		path := "/api/v1/setting/list"
		var body []byte
		if method == http.MethodPost {
			path, body = "/api/v1/setting/set", routeModePayload("spread")
		}
		rec := doSettingAuthRequest(engine, method, path, "expired-test-token", body)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("invalid session %s: status=%d", method, rec.Code)
		}
		assertHandlerRouteMode(t, "fill_first", "fill_first")
	}
}

func TestRouteModeSettingListDoesNotRepairDatabase(t *testing.T) {
	engine, bearer := setupRouteModeHandlerTest(t)
	if err := op.SettingSetString(model.SettingKeyRouteModeOverride, "spread"); err != nil {
		t.Fatal(err)
	}
	// Simulate external legacy data after startup: GET must neither migrate nor pretend
	// this has reached the runtime cache. Only startup/import refresh owns that boundary.
	if err := db.GetDB().Model(&model.Setting{Key: model.SettingKeyRouteModeOverride}).Update("value", "legacy-invalid").Error; err != nil {
		t.Fatal(err)
	}
	writes := 0
	countWrite := func(tx *gorm.DB) { writes++ }
	const callback = "test:count_setting_list_writes"
	for _, register := range []func() error{
		func() error {
			return db.GetDB().Callback().Create().Before("gorm:create").Register(callback, countWrite)
		},
		func() error {
			return db.GetDB().Callback().Update().Before("gorm:update").Register(callback, countWrite)
		},
		func() error {
			return db.GetDB().Callback().Delete().Before("gorm:delete").Register(callback, countWrite)
		},
	} {
		if err := register(); err != nil {
			t.Fatal(err)
		}
	}
	defer db.GetDB().Callback().Create().Remove(callback)
	defer db.GetDB().Callback().Update().Remove(callback)
	defer db.GetDB().Callback().Delete().Remove(callback)
	for i := 0; i < 2; i++ {
		rec := doSettingAuthRequest(engine, http.MethodGet, "/api/v1/setting/list", bearer, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
		}
		assertHandlerRouteMode(t, "legacy-invalid", "spread")
	}
	if writes != 0 {
		t.Fatalf("GET issued %d writes", writes)
	}
}

func TestRouteModeImportNormalizesAndPreservesLockedRule(t *testing.T) {
	engine, bearer := setupRouteModeHandlerTest(t)
	ctx := t.Context()
	plan, err := op.AccessPlanSelect(0, "svip", ctx)
	if err != nil {
		t.Fatal(err)
	}
	channel := model.Channel{Name: "locked-route-channel", Enabled: true, Model: "locked-model"}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatal(err)
	}
	rule := model.AccessRouteRule{RouteProfileID: plan.RouteProfileID, RequestModel: "locked-model", Mode: model.GroupModeSpread}
	if err := op.AccessRouteRuleCreate(&rule, ctx); err != nil {
		t.Fatal(err)
	}
	target := model.AccessRouteTarget{RouteRuleID: rule.ID, ChannelID: channel.ID, UpstreamModel: "locked-model", Priority: 1, Weight: 1, Enabled: true}
	if err := op.AccessRouteTargetCreate(&target, ctx); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ raw, want string }{
		{"", "fill_first"}, {"unknown-legacy", "fill_first"}, {" Fill_First ", "fill_first"}, {" SPREAD ", "spread"},
	} {
		payload, _ := json.Marshal(model.DBDump{Version: 2, Settings: []model.Setting{{Key: model.SettingKeyRouteModeOverride, Value: tc.raw}}})
		rec := doSettingAuthRequest(engine, http.MethodPost, "/api/v1/setting/import", bearer, payload)
		if rec.Code != http.StatusOK {
			t.Fatalf("import %q: %d %s", tc.raw, rec.Code, rec.Body.String())
		}
		assertHandlerRouteMode(t, tc.want, tc.want)
		refreshedPlan, err := op.AccessPlanSelect(0, "svip", ctx)
		if err != nil {
			t.Fatal(err)
		}
		group, _, ok, err := op.AccessPlanGroupForModel(refreshedPlan, "locked-model", ctx)
		if err != nil || !ok || !group.ModeLocked || group.Mode != model.GroupModeSpread {
			t.Fatalf("locked rule changed: mode=%d locked=%v ok=%v err=%v", group.Mode, group.ModeLocked, ok, err)
		}
		dump, err := op.DBExportAll(ctx, false, false)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, setting := range dump.Settings {
			if setting.Key == model.SettingKeyRouteModeOverride {
				found = true
				if setting.Value != tc.want {
					t.Fatalf("export=%q, want %q", setting.Value, tc.want)
				}
			}
		}
		if !found {
			t.Fatal("export omitted route mode")
		}
	}
	// Incremental import without this key must preserve the target's explicit choice.
	rec := doSettingAuthRequest(engine, http.MethodPost, "/api/v1/setting/import", bearer, []byte(`{"version":2}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("missing-key import: %d %s", rec.Code, rec.Body.String())
	}
	assertHandlerRouteMode(t, "spread", "spread")
}

func TestRouteModeWriteFailuresDoNotPublishCacheOrClaimImportSuccess(t *testing.T) {
	engine, bearer := setupRouteModeHandlerTest(t)
	if err := op.SettingSetString(model.SettingKeyRouteModeOverride, "spread"); err != nil {
		t.Fatal(err)
	}
	const callback = "test:reject_route_mode_update"
	if err := db.GetDB().Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "settings" {
			tx.AddError(errors.New("injected setting update failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	defer db.GetDB().Callback().Update().Remove(callback)

	rec := doSettingAuthRequest(engine, http.MethodPost, "/api/v1/setting/set", bearer, routeModePayload("fill_first"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("failed set status=%d", rec.Code)
	}
	assertHandlerRouteMode(t, "spread", "spread")
	// Import uses INSERT ... ON CONFLICT; its raw merge commits before refresh UPDATE fails.
	rec = doSettingAuthRequest(engine, http.MethodPost, "/api/v1/setting/import", bearer, []byte(`{"version":2,"settings":[{"key":"route_mode_override","value":""}]}`))
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "import was committed but cache refresh failed") {
		t.Fatalf("failed refresh must report committed import: %d %s", rec.Code, rec.Body.String())
	}
	assertHandlerRouteMode(t, "", "spread")
}
