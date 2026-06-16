package newapi

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestJobManagerRunsDryRunThenApply(t *testing.T) {
	now := time.Date(2026, 5, 18, 20, 0, 0, 0, time.Local)
	sourcePath := filepath.Join(t.TempDir(), "newapi.db")
	setupNewAPISourceFile(t, sourcePath, now)
	target := setupOctopusTarget(t)
	include := true

	manager := NewJobManager()
	dryRun, err := manager.Start(JobStartRequest{
		SourceType:     "sqlite",
		SourceDSN:      sourcePath,
		IncludeLogs:    &include,
		IncludeAPIKeys: &include,
		BatchSize:      100,
	}, target)
	if err != nil {
		t.Fatalf("start dry-run job: %v", err)
	}
	dryRun = waitJob(t, manager, dryRun.ID)
	if dryRun.Status != JobStatusSucceeded || dryRun.Summary == nil || !dryRun.Summary.DryRun {
		t.Fatalf("unexpected dry-run snapshot: %#v", dryRun)
	}
	if !dryRun.CanApply {
		t.Fatalf("successful dry-run should be applyable")
	}

	if _, err := manager.Start(JobStartRequest{
		SourceType:     "sqlite",
		SourceDSN:      sourcePath,
		Apply:          true,
		ConfirmApply:   true,
		IncludeLogs:    &include,
		IncludeAPIKeys: &include,
		BatchSize:      100,
	}, target); err == nil {
		t.Fatalf("expected apply without dry-run reference to fail")
	}

	apply, err := manager.Start(JobStartRequest{
		SourceType:     "sqlite",
		SourceDSN:      sourcePath,
		Apply:          true,
		ConfirmApply:   true,
		DryRunJobID:    dryRun.ID,
		IncludeLogs:    &include,
		IncludeAPIKeys: &include,
		BatchSize:      100,
	}, target)
	if err != nil {
		t.Fatalf("start apply job: %v", err)
	}
	apply = waitJob(t, manager, apply.ID)
	if apply.Status != JobStatusSucceeded || apply.Summary == nil || apply.Summary.DryRun {
		t.Fatalf("unexpected apply snapshot: %#v", apply)
	}
	if apply.Summary.UsersCreated != 1 || apply.Summary.LogsCreated != 0 || apply.Summary.APIKeysCreated != 0 || apply.Summary.IncludedLogs || apply.Summary.IncludedAPIKeys {
		t.Fatalf("unexpected apply summary: %#v", apply.Summary)
	}
}

func TestJobManagerRejectsMismatchedApplyOptions(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "newapi.db")
	setupNewAPISourceFile(t, sourcePath, time.Now())
	target := setupOctopusTarget(t)
	exclude := false

	manager := NewJobManager()
	dryRun, err := manager.Start(JobStartRequest{
		SourceType: "sqlite",
		SourceDSN:  sourcePath,
	}, target)
	if err != nil {
		t.Fatalf("start dry-run job: %v", err)
	}
	dryRun = waitJob(t, manager, dryRun.ID)
	if dryRun.Status != JobStatusSucceeded {
		t.Fatalf("unexpected dry-run status: %#v", dryRun)
	}

	if _, err := manager.Start(JobStartRequest{
		SourceType:     "sqlite",
		SourceDSN:      sourcePath,
		Apply:          true,
		ConfirmApply:   true,
		DryRunJobID:    dryRun.ID,
		IncludeLogs:    &exclude,
		IncludeAPIKeys: &exclude,
		BatchSize:      200,
	}, target); err == nil {
		t.Fatalf("expected mismatched apply options to fail")
	}
}

func waitJob(t *testing.T, manager *JobManager, id string) JobSnapshot {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		snapshot, ok := manager.Get(id)
		if !ok {
			t.Fatalf("job %s not found", id)
		}
		if snapshot.Status != JobStatusQueued && snapshot.Status != JobStatusRunning {
			return snapshot
		}
		time.Sleep(10 * time.Millisecond)
	}
	snapshot, _ := manager.Get(id)
	t.Fatalf("job %s did not finish: %#v", id, snapshot)
	return JobSnapshot{}
}

func setupNewAPISourceFile(t *testing.T, path string, now time.Time) {
	t.Helper()
	conn, err := gorm.Open(sqlite.Open(path), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open source sqlite: %v", err)
	}
	if err := conn.AutoMigrate(&sourceUser{}, &sourceLog{}, &sourceToken{}, &sourceOption{}); err != nil {
		t.Fatalf("migrate source: %v", err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte("old-password"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Create(&sourceUser{
		ID:        1,
		Username:  "alice",
		Password:  string(hash),
		Status:    1,
		Quota:     1_000_000,
		CreatedAt: now.Add(-24 * time.Hour).Unix(),
	}).Error; err != nil {
		t.Fatalf("seed source user: %v", err)
	}
	if err := conn.Create(&sourceLog{
		ID:               10,
		UserID:           1,
		CreatedAt:        now.Add(-time.Hour).Unix(),
		Type:             newAPILogTypeConsume,
		Content:          "chat completion",
		Username:         "alice",
		TokenName:        "old key",
		ModelName:        "gpt-4o",
		Quota:            250_000,
		PromptTokens:     11,
		CompletionTokens: 22,
		UseTime:          2,
		TokenID:          99,
		IP:               "203.0.113.9",
	}).Error; err != nil {
		t.Fatalf("seed source log: %v", err)
	}
	if err := conn.Create(&sourceToken{
		ID:             99,
		UserID:         1,
		Key:            "legacy-secret",
		Status:         1,
		Name:           "old key",
		CreatedTime:    now.Add(-23 * time.Hour).Unix(),
		RemainQuota:    750_000,
		UnlimitedQuota: false,
		UsedQuota:      250_000,
	}).Error; err != nil {
		t.Fatalf("seed source token: %v", err)
	}
	if err := conn.Create(&sourceOption{Key: "QuotaPerUnit", Value: "500000"}).Error; err != nil {
		t.Fatalf("seed source option: %v", err)
	}
	sqlDB, err := conn.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
}
