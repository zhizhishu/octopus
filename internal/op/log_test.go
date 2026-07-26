package op

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
)

func setupRelayLogTest(t *testing.T) context.Context {
	t.Helper()

	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "octopus.db"), false); err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
	})

	ctx := context.Background()
	if err := settingRefreshCache(ctx); err != nil {
		t.Fatalf("refresh setting cache: %v", err)
	}
	if err := RelayLogClear(ctx, nil); err != nil {
		t.Fatalf("clear logs: %v", err)
	}
	return ctx
}

func TestRelayLogCleanupByStorageKeepsNewestLog(t *testing.T) {
	ctx := setupRelayLogTest(t)

	if err := SettingSetString(model.SettingKeyRelayLogMaxStorageGB, "0.000001"); err != nil {
		t.Fatalf("set storage limit: %v", err)
	}

	logs := []model.RelayLog{
		{ID: 1001, Time: 1001, RequestModelName: "oldest", RequestContent: strings.Repeat("a", 700), ResponseContent: strings.Repeat("b", 700)},
		{ID: 1002, Time: 1002, RequestModelName: "middle", RequestContent: strings.Repeat("c", 700), ResponseContent: strings.Repeat("d", 700)},
		{ID: 1003, Time: 1003, RequestModelName: "newest", RequestContent: strings.Repeat("e", 700), ResponseContent: strings.Repeat("f", 700)},
	}
	if err := db.GetDB().WithContext(ctx).Create(&logs).Error; err != nil {
		t.Fatalf("create logs: %v", err)
	}

	if err := relayLogCleanupByStorage(ctx); err != nil {
		t.Fatalf("cleanup by storage: %v", err)
	}

	var remaining []model.RelayLog
	if err := db.GetDB().WithContext(ctx).Order("id ASC").Find(&remaining).Error; err != nil {
		t.Fatalf("list logs: %v", err)
	}
	if len(remaining) != 1 {
		t.Fatalf("expected one log to remain, got %d", len(remaining))
	}
	if remaining[0].ID != 1003 {
		t.Fatalf("expected newest log to remain, got %d", remaining[0].ID)
	}
}

func TestRelayLogStorageInfoReportsConfiguredLimit(t *testing.T) {
	ctx := setupRelayLogTest(t)

	if err := SettingSetString(model.SettingKeyRelayLogMaxStorageGB, "1.5"); err != nil {
		t.Fatalf("set storage limit: %v", err)
	}

	if err := db.GetDB().WithContext(ctx).Create(&model.RelayLog{
		ID:               2001,
		Time:             2001,
		RequestModelName: "audit-model",
		RequestContent:   `{"messages":[{"role":"user","content":"hello"}]}`,
		ResponseContent:  `{"choices":[{"message":{"content":"hi"}}]}`,
	}).Error; err != nil {
		t.Fatalf("create log: %v", err)
	}

	info, err := RelayLogStorageInfo(ctx, nil)
	if err != nil {
		t.Fatalf("storage info: %v", err)
	}
	if info.StoredBytes <= 0 {
		t.Fatalf("expected stored bytes to be positive")
	}
	if info.MaxGB != 1.5 {
		t.Fatalf("expected max gb 1.5, got %v", info.MaxGB)
	}
	if info.MaxBytes <= 0 {
		t.Fatalf("expected max bytes to be positive")
	}
}

func TestRelayLogAddBackfillsCurrentUnixTime(t *testing.T) {
	ctx := setupRelayLogTest(t)

	if err := SettingSetString(model.SettingKeyRelayLogKeepEnabled, "false"); err != nil {
		t.Fatalf("disable relay log persistence: %v", err)
	}
	before := time.Now().Unix()
	if err := RelayLogAdd(ctx, model.RelayLog{RequestModelName: "time-backfill"}); err != nil {
		t.Fatalf("add relay log: %v", err)
	}
	after := time.Now().Unix()

	logs, err := RelayLogList(ctx, nil, nil, 1, 10, nil)
	if err != nil {
		t.Fatalf("list relay logs: %v", err)
	}
	if len(logs) == 0 {
		t.Fatalf("expected a relay log")
	}
	if logs[0].Time < before || logs[0].Time > after {
		t.Fatalf("expected backfilled current unix time between %d and %d, got %d", before, after, logs[0].Time)
	}
}

func TestRelayLogListScopesAndRedactsUserLogs(t *testing.T) {
	ctx := setupRelayLogTest(t)

	logs := []model.RelayLog{
		{
			ID:                    3001,
			UserID:                1,
			APIKeyID:              8,
			RequestIP:             "203.0.113.9",
			Time:                  3001,
			RequestEndpoint:       "messages",
			RequestPath:           "/v1/messages",
			RequestModelName:      "claude-opus-4-7[1m]",
			RequestAPIKeyName:     "user-key",
			ChannelId:             4,
			ChannelName:           "Claude-Relay-1",
			ActualModelName:       "claude-opus-4-7[1m]",
			InputTokens:           10,
			OutputTokens:          2,
			CacheHitTokens:        3,
			CacheWriteTokens:      4,
			CacheInputTokens:      13,
			CacheHitRate:          0.3,
			Ftut:                  111,
			UseTime:               222,
			Cost:                  0.123,
			RequestContent:        `{"message":"secret request"}`,
			ResponseContent:       `{"message":"secret response"}`,
			Error:                 "upstream error",
			ErrorCode:             "service_busy",
			ErrorStatus:           503,
			ErrorStrategy:         "internal strategy",
			TotalAttempts:         1,
			AccessPlanSlug:        "vip",
			AccessPlanName:        "VIP",
			RouteProfileName:      "private-route",
			BillingProfileName:    "private-billing",
			BillingModel:          "claude-opus-4-7[1m]",
			BaseInputPrice:        1,
			DefaultMultiplier:     2,
			PromptOverrideSources: []string{"global"},
			Attempts: []model.ChannelAttempt{{
				ChannelID:   1,
				ChannelName: "private-channel",
				Status:      "failed",
			}},
		},
		{
			ID:              3002,
			UserID:          2,
			Time:            3002,
			RequestContent:  `{"message":"other request"}`,
			ResponseContent: `{"message":"other response"}`,
		},
	}
	if err := db.GetDB().WithContext(ctx).Create(&logs).Error; err != nil {
		t.Fatalf("create logs: %v", err)
	}

	scope := &model.RelayLogScope{UserID: 1, Redact: true}
	result, err := RelayLogList(ctx, nil, nil, 1, 20, scope)
	if err != nil {
		t.Fatalf("list logs: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected one scoped log, got %d", len(result))
	}
	if result[0].UserID != 1 {
		t.Fatalf("expected scoped user log, got user %d", result[0].UserID)
	}
	if result[0].RequestContent != "" || result[0].ResponseContent != "" {
		t.Fatalf("expected request and response content to be redacted")
	}
	if result[0].Error != "[redacted]" {
		t.Fatalf("expected error to be redacted, got %q", result[0].Error)
	}
	if result[0].Attempts != nil {
		t.Fatalf("expected attempts to be redacted")
	}
	if result[0].RequestIP != "" {
		t.Fatalf("expected request ip to be redacted, got %q", result[0].RequestIP)
	}
	summary := RelayLogUserSummary(result[0])
	if summary.RequestModelName != "claude-opus-4-7[1m]" || summary.InputTokens != 10 || summary.CacheHitTokens != 3 {
		t.Fatalf("expected user summary to preserve safe usage fields: %#v", summary)
	}
	// The channel identity must NOT survive into a user-facing summary (a user must
	// not learn which upstream channel served them); the field is removed entirely.
	if summary.ErrorCode != "service_busy" || summary.ErrorStatus != 503 {
		t.Fatalf("expected user summary to preserve safe error status/code: %#v", summary)
	}
	if result[0].RouteProfileName != "private-route" || len(result[0].PromptOverrideSources) != 1 || result[0].BaseInputPrice != 1 {
		t.Fatalf("admin/internal fields should remain in redacted relay log for admin pipeline compatibility")
	}

	publicSummary := RelayLogUserSummary(logs[0])
	if publicSummary.BillingModel != "claude-opus-4-7[1m]" {
		t.Fatalf("expected public summary to preserve safe audit fields: %#v", publicSummary)
	}
}

func TestRelayLogListSinceReturnsScopedLogsOldestFirst(t *testing.T) {
	ctx := setupRelayLogTest(t)

	logs := []model.RelayLog{
		{ID: 7001, UserID: 1, Time: 7001, RequestModelName: "old"},
		{ID: 7002, UserID: 2, Time: 7002, RequestModelName: "other-user"},
		{ID: 7003, UserID: 1, Time: 7003, RequestModelName: "newer"},
		{ID: 7004, UserID: 1, Time: 7004, RequestModelName: "newest"},
	}
	if err := db.GetDB().WithContext(ctx).Create(&logs).Error; err != nil {
		t.Fatalf("create logs: %v", err)
	}

	result, err := RelayLogListSince(ctx, 7001, 10, &model.RelayLogScope{UserID: 1})
	if err != nil {
		t.Fatalf("list since: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected two scoped replay logs, got %#v", result)
	}
	if result[0].ID != 7003 || result[1].ID != 7004 {
		t.Fatalf("expected replay logs oldest first after since id, got %#v", result)
	}
}

func TestRelayLogListDedupesCacheAndDBOverlap(t *testing.T) {
	ctx := setupRelayLogTest(t)

	duplicate := model.RelayLog{ID: 8001, Time: 8001, RequestModelName: "duplicate"}
	if err := db.GetDB().WithContext(ctx).Create(&duplicate).Error; err != nil {
		t.Fatalf("create db log: %v", err)
	}

	relayLogCacheLock.Lock()
	relayLogCache = append(relayLogCache, duplicate)
	relayLogCacheLock.Unlock()

	result, err := RelayLogList(ctx, nil, nil, 1, 10, nil)
	if err != nil {
		t.Fatalf("list relay logs: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected duplicate cache/db log to appear once, got %#v", result)
	}
	if result[0].ID != duplicate.ID {
		t.Fatalf("expected duplicate log id %d, got %d", duplicate.ID, result[0].ID)
	}
}

func TestRelayLogListPaginatesAfterMergingCacheAndDB(t *testing.T) {
	ctx := setupRelayLogTest(t)

	dbLogs := []model.RelayLog{
		{ID: 9001, Time: 9001, RequestModelName: "oldest"},
		{ID: 9002, Time: 9002, RequestModelName: "older"},
		{ID: 9003, Time: 9003, RequestModelName: "middle"},
		{ID: 9004, Time: 9004, RequestModelName: "newer"},
		{ID: 9005, Time: 9005, RequestModelName: "newest"},
	}
	if err := db.GetDB().WithContext(ctx).Create(&dbLogs).Error; err != nil {
		t.Fatalf("create db logs: %v", err)
	}

	relayLogCacheLock.Lock()
	relayLogCache = append(relayLogCache, dbLogs[3], dbLogs[4])
	relayLogCacheLock.Unlock()

	result, err := RelayLogList(ctx, nil, nil, 2, 2, nil)
	if err != nil {
		t.Fatalf("list relay logs with cache: %v", err)
	}
	if len(result) != 2 || result[0].ID != 9003 || result[1].ID != 9002 {
		t.Fatalf("expected stable second page after cache/db merge, got %#v", result)
	}

	relayLogCacheLock.Lock()
	relayLogCache = relayLogCache[:0]
	relayLogCacheLock.Unlock()

	result, err = RelayLogList(ctx, nil, nil, 2, 2, nil)
	if err != nil {
		t.Fatalf("list relay logs after refresh: %v", err)
	}
	if len(result) != 2 || result[0].ID != 9003 || result[1].ID != 9002 {
		t.Fatalf("expected same second page after cache clears, got %#v", result)
	}
}
