package handlers

// Backup/export responses carry the site's full configuration — plaintext admin token,
// SMTP password, channel keys, and (for the log export) full request/response bodies. A
// browser or intermediate proxy that caches one of them hands the whole site away, so each
// of those responses must be marked Cache-Control: no-store.

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/gin-gonic/gin"
)

// TestExportDBIsNotCacheable pins the whole-site backup endpoint.
func TestExportDBIsNotCacheable(t *testing.T) {
	setupSettingAuthTestDB(t)
	admin := seedSettingAuthSiteSecrets(t, t.Context())

	rec := doSettingAuthRequest(newSettingGroupRouter(), http.MethodGet, "/api/v1/setting/export", jwtBearer(t, admin), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin export must succeed, got %d body %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("backup response must be uncacheable, got Cache-Control=%q", got)
	}
}

// TestExportLogIsNotCacheable pins BOTH log export shapes: the default JSON array and the
// opt-in NDJSON/JSONL stream (separate header blocks in the handler).
func TestExportLogIsNotCacheable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "log_export_cache_test.db"), false); err != nil {
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
	if err := db.GetDB().Create(&model.RelayLog{
		ID:               9101,
		Time:             9101,
		RequestEndpoint:  "chat",
		RequestPath:      "/v1/chat/completions",
		RequestModelName: "gpt-5.5",
		ActualModelName:  "gpt-5.5",
		RequestContent:   `{"messages":[{"content":"secret prompt"}]}`,
	}).Error; err != nil {
		t.Fatalf("create relay log: %v", err)
	}

	for _, target := range []string{
		"/api/v1/log/export",
		"/api/v1/log/export?format=ndjson",
		"/api/v1/log/export?format=jsonl",
	} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodGet, target, nil)
		exportLog(c)
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("%s: log export must be uncacheable, got Cache-Control=%q", target, got)
		}
	}
}
