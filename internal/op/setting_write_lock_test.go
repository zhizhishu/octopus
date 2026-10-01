package op

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"gorm.io/gorm"
)

// These tests pin the settings write/snapshot-publish contract using only the
// long-standing public API (DBImportIncremental / settingRefreshCache /
// SettingSetString), so the exact same file compiles and runs against BOTH the
// pre-fix revision (where it must fail) and the fixed revision (where it must pass).
//
// Interleavings are forced with GORM callbacks, never with sleeps racing for a
// window: the "bad" outcome is reported when a goroutine completes inside a critical
// section it should not have been able to enter, not when a timer expires.

const (
	settingsTable = "settings"
	parkWait      = 10 * time.Second
	// blockedWait bounds how long we wait for a save that MUST NOT complete while a
	// refresh is parked. It is only an upper bound on a wait for an event that the
	// fixed code makes impossible; it is never the pass condition (the assertion
	// below is the pass condition).
	blockedWait = 500 * time.Millisecond
)

// settingsWriteGate parks the first settings UPDATE (mode "update") or the first
// settings SELECT (mode "query") and releases every later one immediately.
type settingsWriteGate struct {
	reached chan struct{}
	release chan struct{}
	once    sync.Once
	name    string
}

func parkSettingsUpdate(t *testing.T, mode string) *settingsWriteGate {
	t.Helper()
	gate := &settingsWriteGate{
		reached: make(chan struct{}),
		release: make(chan struct{}),
		name:    "test:park_settings_" + mode,
	}
	park := func(tx *gorm.DB) {
		if tx.Statement == nil || tx.Statement.Table != settingsTable {
			return
		}
		gate.once.Do(func() { close(gate.reached) })
		<-gate.release
	}
	var err error
	switch mode {
	case "update":
		err = db.GetDB().Callback().Update().Before("gorm:update").Register(gate.name, park)
	case "query":
		err = db.GetDB().Callback().Query().After("gorm:query").Register(gate.name, park)
	default:
		t.Fatalf("unknown gate mode %q", mode)
	}
	if err != nil {
		t.Fatalf("register gate: %v", err)
	}
	t.Cleanup(func() {
		switch mode {
		case "update":
			_ = db.GetDB().Callback().Update().Remove(gate.name)
		case "query":
			_ = db.GetDB().Callback().Query().Remove(gate.name)
		}
		gate.releaseOnce()
	})
	return gate
}

func (g *settingsWriteGate) releaseOnce() {
	select {
	case <-g.release:
	default:
		close(g.release)
	}
}

func (g *settingsWriteGate) waitForPark(t *testing.T, what string) {
	t.Helper()
	select {
	case <-g.reached:
	case <-time.After(parkWait):
		t.Fatalf("%s never reached the parked write", what)
	}
}

func rejectSettingsWrite(t *testing.T, message string) {
	t.Helper()
	const name = "test:reject_settings_write"
	if err := db.GetDB().Callback().Update().Before("gorm:update").Register(name, func(tx *gorm.DB) {
		if tx.Statement != nil && tx.Statement.Table == settingsTable {
			tx.AddError(errors.New(message))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.GetDB().Callback().Update().Remove(name) })
}

func assertRouteModeConsistent(t *testing.T, wantDB, wantCache string) {
	t.Helper()
	var stored model.Setting
	if err := db.GetDB().First(&stored, "key = ?", model.SettingKeyRouteModeOverride).Error; err != nil {
		t.Fatalf("load persisted route mode: %v", err)
	}
	cached, err := SettingGetString(model.SettingKeyRouteModeOverride)
	if err != nil {
		t.Fatalf("read cached route mode: %v", err)
	}
	if stored.Value != wantDB || cached != wantCache {
		t.Fatalf("route mode DB=%q cache=%q, want DB=%q cache=%q", stored.Value, cached, wantDB, wantCache)
	}
}

// seedDefaultAdmin provides the admin row DBImportIncremental's ownership backfill
// requires before it will accept a dump.
func seedDefaultAdmin(t *testing.T, ctx context.Context) {
	t.Helper()
	if _, err := UserCreate(model.UserCreateRequest{
		Username: "settings-lock-admin",
		Password: "test-password",
		Role:     model.UserRoleAdmin,
		Status:   model.UserStatusActive,
	}, ctx); err != nil {
		t.Fatalf("seed default admin: %v", err)
	}
}

func seedRouteMode(t *testing.T, ctx context.Context, dbValue, cacheValue string) {
	t.Helper()
	if err := settingRefreshCache(ctx); err != nil {
		t.Fatalf("initial refresh: %v", err)
	}
	if err := SettingSetString(model.SettingKeyRouteModeOverride, cacheValue); err != nil {
		t.Fatalf("seed route mode: %v", err)
	}
	if dbValue != cacheValue {
		if err := db.GetDB().Model(&model.Setting{Key: model.SettingKeyRouteModeOverride}).Update("Value", dbValue).Error; err != nil {
			t.Fatalf("diverge db from cache: %v", err)
		}
	}
}

// R01 — a settings save must not be able to run inside the settings refresh
// critical section, and the value it saved must survive that refresh.
//
// Pre-fix this fails: the save short circuits on cache equality, the refresh then
// writes its pre-save snapshot both to the DB and to the cache, and the admin's
// acknowledged save is gone.
func TestSettingSaveCannotEnterSettingsRefreshCriticalSection(t *testing.T) {
	ctx := setupSettingTest(t)
	// DB holds an imported legacy empty value while the cache still holds the
	// previous live value — the state a committed-then-failed import leaves behind.
	seedRouteMode(t, ctx, "", "spread")

	gate := parkSettingsUpdate(t, "update")
	refreshDone := make(chan error, 1)
	go func() { refreshDone <- settingRefreshCache(ctx) }()
	gate.waitForPark(t, "settings refresh")

	// Exactly the case a cache-equality short circuit used to answer with a fake
	// success: saving the value the cache already holds.
	saveDone := make(chan error, 1)
	go func() { saveDone <- SettingSetString(model.SettingKeyRouteModeOverride, "spread") }()

	select {
	case err := <-saveDone:
		gate.releaseOnce()
		<-refreshDone
		t.Fatalf("save completed while the settings refresh held its critical section (err=%v); a stale refresh can overwrite it", err)
	case <-time.After(blockedWait):
	}
	gate.releaseOnce()
	if err := <-refreshDone; err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if err := <-saveDone; err != nil {
		t.Fatalf("save: %v", err)
	}

	assertRouteModeConsistent(t, "spread", "spread")
	settings, err := SettingList(ctx)
	if err != nil {
		t.Fatalf("list settings: %v", err)
	}
	for _, setting := range settings {
		if setting.Key == model.SettingKeyRouteModeOverride && setting.Value != "spread" {
			t.Fatalf("SettingList reports %q, want spread", setting.Value)
		}
	}
}

// R04 — even when the refresh has nothing to repair (a valid stored value), it must
// not publish a snapshot that was read before a concurrent save landed.
func TestSettingsSnapshotPublishDoesNotRevertConcurrentSave(t *testing.T) {
	ctx := setupSettingTest(t)
	seedRouteMode(t, ctx, "fill_first", "fill_first")

	gate := parkSettingsUpdate(t, "query")
	refreshDone := make(chan error, 1)
	go func() { refreshDone <- settingRefreshCache(ctx) }()
	gate.waitForPark(t, "settings refresh snapshot read")

	saveDone := make(chan error, 1)
	go func() { saveDone <- SettingSetString(model.SettingKeyRouteModeOverride, "spread") }()

	select {
	case err := <-saveDone:
		gate.releaseOnce()
		<-refreshDone
		t.Fatalf("save completed while the refresh was holding its snapshot (err=%v); the publish would revert the cache", err)
	case <-time.After(blockedWait):
	}
	gate.releaseOnce()
	if err := <-refreshDone; err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if err := <-saveDone; err != nil {
		t.Fatalf("save: %v", err)
	}

	assertRouteModeConsistent(t, "spread", "spread")
}

// R10 — the protection is not special-cased to route_mode_override: any setting
// saved during a full cache refresh must survive the refresh's ReplaceAll.
func TestUnrelatedSettingSurvivesFullCacheRefresh(t *testing.T) {
	ctx := setupSettingTest(t)
	if err := settingRefreshCache(ctx); err != nil {
		t.Fatal(err)
	}
	const key = model.SettingKeyCORSAllowOrigins
	if err := SettingSetString(key, "https://before.example"); err != nil {
		t.Fatal(err)
	}

	gate := parkSettingsUpdate(t, "query")
	refreshDone := make(chan error, 1)
	go func() { refreshDone <- settingRefreshCache(ctx) }()
	gate.waitForPark(t, "settings refresh snapshot read")

	saveDone := make(chan error, 1)
	go func() { saveDone <- SettingSetString(key, "https://after.example") }()
	select {
	case err := <-saveDone:
		gate.releaseOnce()
		<-refreshDone
		t.Fatalf("save completed inside the refresh critical section (err=%v)", err)
	case <-time.After(blockedWait):
	}
	gate.releaseOnce()
	if err := <-refreshDone; err != nil {
		t.Fatal(err)
	}
	if err := <-saveDone; err != nil {
		t.Fatal(err)
	}

	var stored model.Setting
	if err := db.GetDB().First(&stored, "key = ?", key).Error; err != nil {
		t.Fatal(err)
	}
	cached, err := SettingGetString(key)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Value != "https://after.example" || cached != "https://after.example" {
		t.Fatalf("setting reverted: DB=%q cache=%q", stored.Value, cached)
	}

	// The same guarantee for the integer setter.
	const intKey = model.SettingKeyStatsSaveInterval
	before, err := SettingGetString(intKey)
	if err != nil {
		t.Fatal(err)
	}
	interval, err := strconv.Atoi(before)
	if err != nil {
		t.Fatalf("stats interval default is not an integer: %q", before)
	}
	if err := SettingSetInt(intKey, interval+1); err != nil {
		t.Fatal(err)
	}
	var storedInt model.Setting
	if err := db.GetDB().First(&storedInt, "key = ?", intKey).Error; err != nil {
		t.Fatal(err)
	}
	cachedInt, err := SettingGetString(intKey)
	if err != nil {
		t.Fatal(err)
	}
	wantInt := strconv.Itoa(interval + 1)
	if storedInt.Value != wantInt || cachedInt != wantInt {
		t.Fatalf("int setting DB=%q cache=%q, want %q", storedInt.Value, cachedInt, wantInt)
	}
}

// R08 — a save whose value already matches the cache must still go through the DB,
// so a failed DB write is reported instead of being masked by cache equality.
func TestSameValueSaveStillReportsDatabaseFailure(t *testing.T) {
	ctx := setupSettingTest(t)
	seedRouteMode(t, ctx, "spread", "spread")
	rejectSettingsWrite(t, "injected setting write failure")

	if err := SettingSetString(model.SettingKeyRouteModeOverride, "spread"); err == nil {
		t.Fatal("saving the cached value must still verify the database write")
	}
	assertRouteModeConsistent(t, "spread", "spread")
}

// R07 — after an import is committed but its refresh failed, a save of the value the
// stale cache still holds must repair the database instead of reporting success.
//
// Pre-fix this fails: the cache-equality short circuit claims success while the DB
// keeps the imported value, so the admin's acknowledged save never reaches the DB.
func TestSaveAfterFailedImportRepairsDatabase(t *testing.T) {
	ctx := setupSettingTest(t)
	seedRouteMode(t, ctx, "spread", "spread")
	seedDefaultAdmin(t, ctx)

	rejectSettingsWrite(t, "injected setting write failure")
	dump := &model.DBDump{
		Version:  2,
		Settings: []model.Setting{{Key: model.SettingKeyRouteModeOverride, Value: ""}},
	}
	if _, err := DBImportIncremental(ctx, dump); err != nil {
		t.Fatalf("import (INSERT ... ON CONFLICT) should still commit: %v", err)
	}
	if err := settingRefreshCache(ctx); err == nil {
		t.Fatal("refresh must fail while the database write is rejected")
	}
	// The failed refresh leaves the DB holding the imported legacy value and the
	// cache holding the previous live value.
	assertRouteModeConsistent(t, "", "spread")

	// Clear the injected failure: the admin retries the save they were told failed.
	_ = db.GetDB().Callback().Update().Remove("test:reject_settings_write")
	if err := SettingSetString(model.SettingKeyRouteModeOverride, "spread"); err != nil {
		t.Fatalf("save after a failed import: %v", err)
	}
	assertRouteModeConsistent(t, "spread", "spread")
}

// R09 — repeated same-value saves stay idempotent, and a genuinely missing row is
// still reported instead of being mistaken for a no-op.
func TestRepeatedSameValueSaveIsIdempotent(t *testing.T) {
	ctx := setupSettingTest(t)
	seedRouteMode(t, ctx, "spread", "spread")

	for i := 0; i < 3; i++ {
		if err := SettingSetString(model.SettingKeyRouteModeOverride, "spread"); err != nil {
			t.Fatalf("repeat %d: %v", i, err)
		}
		assertRouteModeConsistent(t, "spread", "spread")
	}

	// Delete the row behind the cache's back: the setter must notice.
	if err := db.GetDB().Where("key = ?", model.SettingKeyRouteModeOverride).Delete(&model.Setting{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := SettingSetString(model.SettingKeyRouteModeOverride, "spread"); err == nil {
		t.Fatal("saving a key whose row no longer exists must fail, not silently succeed")
	}
}

// R11 — a refresh that fails must release the write lock, so the instance keeps
// accepting settings saves instead of hanging forever.
func TestFailedRefreshReleasesSettingsWriteLock(t *testing.T) {
	ctx := setupSettingTest(t)
	// DB holds a legacy value the refresh must repair, so its write is the failure
	// point and the failure is deterministic.
	seedRouteMode(t, ctx, "", "spread")

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	// A cancelled context may fail at the read or pass through to the repair write;
	// either way it must return rather than hang.
	if err := settingRefreshCache(cancelled); err == nil {
		assertRouteModeConsistent(t, "fill_first", "fill_first")
		if err := db.GetDB().Model(&model.Setting{Key: model.SettingKeyRouteModeOverride}).Update("Value", "").Error; err != nil {
			t.Fatal(err)
		}
	}

	rejectSettingsWrite(t, "injected setting write failure")
	if err := settingRefreshCache(ctx); err == nil {
		t.Fatal("refresh must fail while its repair write is rejected")
	}
	_ = db.GetDB().Callback().Update().Remove("test:reject_settings_write")

	done := make(chan error, 1)
	go func() { done <- SettingSetString(model.SettingKeyRouteModeOverride, "fill_first") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("save after failed refresh: %v", err)
		}
	case <-time.After(parkWait):
		t.Fatal("settings write lock was not released after a failed refresh")
	}
	assertRouteModeConsistent(t, "fill_first", "fill_first")
}
