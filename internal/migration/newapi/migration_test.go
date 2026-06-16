package newapi

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/glebarez/sqlite"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestRunFiltersActiveUsersAndMigratesUsageSummary(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 5, 18, 0, 40, 0, 0, time.Local)
	source := setupNewAPISource(t, now)
	target := setupOctopusTarget(t)

	dryRun, err := Run(ctx, Config{
		SourceDB:       source,
		TargetDB:       target,
		IncludeLogs:    true,
		IncludeAPIKeys: true,
		Apply:          false,
		Now:            now,
	})
	if err != nil {
		t.Fatalf("dry-run migration: %v", err)
	}
	if !dryRun.DryRun {
		t.Fatalf("expected dry-run summary")
	}
	if dryRun.SourceUsers != 2 || dryRun.ActiveUsers != 1 || dryRun.InactiveUsersSkipped != 1 {
		t.Fatalf("unexpected dry-run user summary: %#v", dryRun)
	}
	if dryRun.UsersCreated != 1 || dryRun.APIKeysConsidered != 0 || dryRun.LogsConsidered != 0 || dryRun.IncludedAPIKeys || dryRun.IncludedLogs {
		t.Fatalf("unexpected dry-run import counts: %#v", dryRun)
	}
	var usersAfterDryRun int64
	if err := target.Model(&model.User{}).Count(&usersAfterDryRun).Error; err != nil {
		t.Fatal(err)
	}
	if usersAfterDryRun != 1 {
		t.Fatalf("dry-run modified target users, got %d", usersAfterDryRun)
	}

	applied, err := Run(ctx, Config{
		SourceDB:       source,
		TargetDB:       target,
		IncludeLogs:    true,
		IncludeAPIKeys: true,
		Apply:          true,
		Now:            now,
	})
	if err != nil {
		t.Fatalf("apply migration: %v", err)
	}
	if applied.DryRun || applied.UsersCreated != 1 || applied.APIKeysCreated != 0 || applied.LogsCreated != 0 || applied.StatsUpdated {
		t.Fatalf("unexpected apply summary: %#v", applied)
	}

	var alice model.User
	if err := target.First(&alice, "username = ?", "alice").Error; err != nil {
		t.Fatalf("migrated user missing: %v", err)
	}
	if alice.Role != model.UserRoleUser || alice.Status != model.UserStatusActive {
		t.Fatalf("unexpected migrated user role/status: %#v", alice)
	}
	if alice.Balance != 2 {
		t.Fatalf("expected balance 2, got %.6f", alice.Balance)
	}
	if alice.LastRelayIP != "203.0.113.9" || alice.LastRelayAt == 0 {
		t.Fatalf("last relay audit was not preserved: %#v", alice)
	}
	if !strings.Contains(alice.Note, "active_usage_logs=1") ||
		!strings.Contains(alice.Note, "active_prompt_tokens=11") ||
		!strings.Contains(alice.Note, "active_completion_tokens=22") ||
		!strings.Contains(alice.Note, "migration_policy=summary_only_no_api_keys_no_detail_logs") {
		t.Fatalf("usage summary was not preserved in user note: %s", alice.Note)
	}
	if err := alice.ComparePassword("old-password"); err != nil {
		t.Fatalf("bcrypt password hash was not preserved: %v", err)
	}

	var keyCount int64
	if err := target.Model(&model.APIKey{}).Where("user_id = ?", alice.ID).Count(&keyCount).Error; err != nil {
		t.Fatalf("count api keys: %v", err)
	}
	if keyCount != 0 {
		t.Fatalf("summary-only migration must not import api keys, got %d", keyCount)
	}

	var logCount int64
	if err := target.Model(&model.RelayLog{}).Count(&logCount).Error; err != nil {
		t.Fatalf("count relay logs: %v", err)
	}
	if logCount != 0 {
		t.Fatalf("summary-only migration must not import detailed relay logs, got %d", logCount)
	}

	var totalStats int64
	if err := target.Model(&model.StatsTotal{}).Count(&totalStats).Error; err != nil {
		t.Fatalf("count total stats: %v", err)
	}
	if totalStats != 0 {
		t.Fatalf("summary-only migration must not backfill realtime stats, got %d", totalStats)
	}
}

func TestRunSkipsConflictingUserByDefault(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 5, 18, 0, 45, 0, 0, time.Local)
	source := setupNewAPISource(t, now)
	target := setupOctopusTarget(t)
	conflict := model.User{Username: "alice", Password: "admin", Role: model.UserRoleUser, Status: model.UserStatusActive}
	if err := conflict.HashPassword(); err != nil {
		t.Fatal(err)
	}
	if err := target.Create(&conflict).Error; err != nil {
		t.Fatal(err)
	}

	summary, err := Run(ctx, Config{
		SourceDB:       source,
		TargetDB:       target,
		Apply:          true,
		IncludeLogs:    true,
		IncludeAPIKeys: true,
		Now:            now,
	})
	if err != nil {
		t.Fatalf("apply migration with conflict: %v", err)
	}
	if summary.UsersCreated != 0 || summary.UsersSkippedConflict != 1 || summary.LogsCreated != 0 || summary.APIKeysCreated != 0 {
		t.Fatalf("unexpected conflict summary: %#v", summary)
	}
}

func setupNewAPISource(t *testing.T, now time.Time) *gorm.DB {
	t.Helper()
	conn := openTestDB(t, "newapi.db")
	if err := conn.AutoMigrate(&sourceUser{}, &sourceLog{}, &sourceToken{}, &sourceOption{}); err != nil {
		t.Fatalf("migrate source: %v", err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte("old-password"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatal(err)
	}
	users := []sourceUser{
		{
			ID:           1,
			Username:     "alice",
			Password:     string(hash),
			DisplayName:  "Alice",
			Role:         100,
			Status:       1,
			Email:        "alice@example.test",
			Quota:        1_000_000,
			UsedQuota:    250_000,
			RequestCount: 1,
			Group:        "vip",
			CreatedAt:    now.Add(-48 * time.Hour).Unix(),
		},
		{
			ID:        2,
			Username:  "bot-spam",
			Password:  string(hash),
			Status:    1,
			Quota:     1_000_000,
			CreatedAt: now.Add(-24 * time.Hour).Unix(),
		},
	}
	if err := conn.Create(&users).Error; err != nil {
		t.Fatalf("seed source users: %v", err)
	}
	if err := conn.Create(&sourceOption{Key: "QuotaPerUnit", Value: "500000"}).Error; err != nil {
		t.Fatalf("seed source option: %v", err)
	}
	token := sourceToken{
		ID:             99,
		UserID:         1,
		Key:            "legacy-secret",
		Status:         1,
		Name:           "old key",
		CreatedTime:    now.Add(-47 * time.Hour).Unix(),
		AccessedTime:   now.Add(-1 * time.Hour).Unix(),
		ExpiredTime:    -1,
		RemainQuota:    750_000,
		UnlimitedQuota: false,
		UsedQuota:      250_000,
	}
	if err := conn.Create(&token).Error; err != nil {
		t.Fatalf("seed source token: %v", err)
	}
	logs := []sourceLog{
		{
			ID:               10,
			UserID:           1,
			CreatedAt:        now.Add(-2 * time.Hour).Unix(),
			Type:             newAPILogTypeConsume,
			Content:          "chat completion",
			Username:         "alice",
			TokenName:        "old key",
			ModelName:        "gpt-4o",
			Quota:            250_000,
			PromptTokens:     11,
			CompletionTokens: 22,
			UseTime:          2,
			ChannelID:        7,
			TokenID:          99,
			Group:            "vip",
			IP:               "203.0.113.9",
		},
		{
			ID:        11,
			UserID:    1,
			CreatedAt: now.Add(-1 * time.Hour).Unix(),
			Type:      newAPILogTypeError,
			Content:   "upstream error",
			Username:  "alice",
			TokenName: "old key",
			ModelName: "gpt-4o",
			TokenID:   99,
			Group:     "vip",
			IP:        "203.0.113.9",
		},
	}
	if err := conn.Create(&logs).Error; err != nil {
		t.Fatalf("seed source logs: %v", err)
	}
	return conn
}

func setupOctopusTarget(t *testing.T) *gorm.DB {
	t.Helper()
	conn := openTestDB(t, "octopus.db")
	if err := conn.AutoMigrate(
		&model.User{},
		&model.APIKey{},
		&model.RelayLog{},
		&model.StatsTotal{},
		&model.StatsDaily{},
		&model.StatsHourly{},
		&model.StatsAPIKey{},
	); err != nil {
		t.Fatalf("migrate target: %v", err)
	}
	admin := model.User{Username: "admin", Password: "admin", Role: model.UserRoleAdmin, Status: model.UserStatusActive}
	if err := admin.HashPassword(); err != nil {
		t.Fatal(err)
	}
	if err := conn.Create(&admin).Error; err != nil {
		t.Fatalf("seed target admin: %v", err)
	}
	return conn
}

func openTestDB(t *testing.T, name string) *gorm.DB {
	t.Helper()
	conn, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), name)), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() {
		sqlDB, err := conn.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	})
	return conn
}
