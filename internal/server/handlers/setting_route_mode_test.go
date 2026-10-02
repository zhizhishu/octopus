package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bestruirui/octopus/internal/conf"
	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/server/auth"
	"github.com/bestruirui/octopus/internal/server/middleware"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
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

// S05 remainder: missing Authorization (not a garbage token) must refuse and leave
// the stored mode / locked-rule-free settings row untouched.
func TestRouteModeSetRejectsMissingAuthorization(t *testing.T) {
	engine, _ := setupRouteModeHandlerTest(t)
	if err := op.SettingSetString(model.SettingKeyRouteModeOverride, "spread"); err != nil {
		t.Fatal(err)
	}
	seedLockedFillFirstRule(t, "locked-auth-model")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/setting/set", bytes.NewReader(routeModePayload("fill_first")))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing auth status=%d body=%s", rec.Code, rec.Body.String())
	}
	assertHandlerRouteMode(t, "spread", "spread")
	assertLockedFillFirstUnchanged(t, "locked-auth-model")
}

// S05 remainder: a real JWT that was valid for this admin, but is already expired,
// must refuse. A garbage string is not this case.
func TestRouteModeSetRejectsExpiredJWT(t *testing.T) {
	engine, _ := setupRouteModeHandlerTest(t)
	if err := op.SettingSetString(model.SettingKeyRouteModeOverride, "spread"); err != nil {
		t.Fatal(err)
	}
	seedLockedFillFirstRule(t, "locked-expired-model")
	admin, err := op.UserGetByUsername("route-mode-admin")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(-2 * time.Hour)
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, &auth.Claims{
		UserID:   admin.ID,
		Username: admin.Username,
		Role:     admin.Role,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute)),
			Issuer:    conf.APP_NAME,
			Subject:   fmt.Sprintf("%d", admin.ID),
		},
	}).SignedString([]byte(admin.Username + admin.Password))
	if err != nil {
		t.Fatal(err)
	}
	rec := doSettingAuthRequest(engine, http.MethodPost, "/api/v1/setting/set", token, routeModePayload("fill_first"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired jwt status=%d body=%s", rec.Code, rec.Body.String())
	}
	assertHandlerRouteMode(t, "spread", "spread")
	assertLockedFillFirstUnchanged(t, "locked-expired-model")
}

// S06 remainder: a fill_first locked rule stays fill_first when the global default
// is switched via setting/set (import preservation is covered elsewhere).
func TestLockedFillFirstRuleSurvivesGlobalSpreadSwitch(t *testing.T) {
	engine, bearer := setupRouteModeHandlerTest(t)
	seedLockedFillFirstRule(t, "locked-fill-model")
	rec := doSettingAuthRequest(engine, http.MethodPost, "/api/v1/setting/set", bearer, routeModePayload("spread"))
	if rec.Code != http.StatusOK {
		t.Fatalf("set spread: %d %s", rec.Code, rec.Body.String())
	}
	assertHandlerRouteMode(t, "spread", "spread")
	assertLockedFillFirstUnchanged(t, "locked-fill-model")
}

func seedLockedFillFirstRule(t *testing.T, modelName string) {
	t.Helper()
	ctx := t.Context()
	plan, err := op.AccessPlanSelect(0, "svip", ctx)
	if err != nil {
		t.Fatal(err)
	}
	channel := model.Channel{Name: "locked-" + modelName, Enabled: true, Model: modelName}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatal(err)
	}
	rule := model.AccessRouteRule{RouteProfileID: plan.RouteProfileID, RequestModel: modelName, Mode: model.GroupModeFillFirst}
	if err := op.AccessRouteRuleCreate(&rule, ctx); err != nil {
		t.Fatal(err)
	}
	target := model.AccessRouteTarget{RouteRuleID: rule.ID, ChannelID: channel.ID, UpstreamModel: modelName, Priority: 2, Weight: 3, Enabled: true}
	if err := op.AccessRouteTargetCreate(&target, ctx); err != nil {
		t.Fatal(err)
	}
}

func assertLockedFillFirstUnchanged(t *testing.T, modelName string) {
	t.Helper()
	ctx := t.Context()
	plan, err := op.AccessPlanSelect(0, "svip", ctx)
	if err != nil {
		t.Fatal(err)
	}
	group, _, ok, err := op.AccessPlanGroupForModel(plan, modelName, ctx)
	if err != nil || !ok || !group.ModeLocked || group.Mode != model.GroupModeFillFirst {
		t.Fatalf("locked fill_first changed: mode=%d locked=%v ok=%v err=%v", group.Mode, group.ModeLocked, ok, err)
	}
	if len(group.Items) != 1 || group.Items[0].Weight != 3 || group.Items[0].Priority != 2 {
		t.Fatalf("locked items mutated: %+v", group.Items)
	}
}
