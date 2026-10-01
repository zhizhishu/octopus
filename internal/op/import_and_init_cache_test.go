package op

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"gorm.io/gorm"
)

// Tests for the combined import entry point. ImportAndInitCache does not exist on the
// pre-fix revision, so this file only compiles against the fixed code; the red proof
// for the underlying lost-update defect lives in setting_write_lock_test.go, which
// uses only the long-standing public API and therefore runs on both revisions.

func routeModeDump(value string) *model.DBDump {
	return &model.DBDump{
		Version:  2,
		Settings: []model.Setting{{Key: model.SettingKeyRouteModeOverride, Value: value}},
	}
}

// R-import — the import must hold the settings write lock from before its settings
// upsert until the settings snapshot is published, so a save acknowledged during that
// window cannot be overwritten by the import's repair write.
func TestImportHoldsSettingsLockUntilSnapshotPublished(t *testing.T) {
	ctx := setupSettingTest(t)
	seedRouteMode(t, ctx, "spread", "spread")
	seedDefaultAdmin(t, ctx)

	// The imported value needs a repair write, which is where the import parks.
	gate := parkSettingsUpdate(t, "update")
	importDone := make(chan error, 1)
	go func() {
		_, err := ImportAndInitCache(ctx, routeModeDump(""))
		importDone <- err
	}()
	gate.waitForPark(t, "import settings refresh")

	saveDone := make(chan error, 1)
	go func() { saveDone <- SettingSetString(model.SettingKeyRouteModeOverride, "spread") }()
	select {
	case err := <-saveDone:
		gate.releaseOnce()
		<-importDone
		t.Fatalf("save completed while the import held the settings write lock (err=%v)", err)
	case <-time.After(blockedWait):
	}
	gate.releaseOnce()
	if err := <-importDone; err != nil {
		t.Fatalf("import: %v", err)
	}
	if err := <-saveDone; err != nil {
		t.Fatalf("save: %v", err)
	}
	assertRouteModeConsistent(t, "spread", "spread")
}

// R06 — an import that was committed but whose refresh then failed must be reported
// as exactly that, and must not pretend the import did not happen.
func TestImportCommittedButRefreshFailedIsReported(t *testing.T) {
	ctx := setupSettingTest(t)
	seedRouteMode(t, ctx, "spread", "spread")
	seedDefaultAdmin(t, ctx)

	rejectSettingsWrite(t, "injected setting write failure")
	_, err := ImportAndInitCache(ctx, routeModeDump(""))
	if err == nil {
		t.Fatal("import must fail while the settings repair write is rejected")
	}
	var committed *ImportCacheRefreshError
	if !errors.As(err, &committed) {
		t.Fatalf("error must identify a committed import, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "import was committed but cache refresh failed") {
		t.Fatalf("unexpected message: %v", err)
	}
	// The import really did commit (INSERT ... ON CONFLICT), so the DB holds the
	// imported value while the cache still holds the pre-import snapshot.
	assertRouteModeConsistent(t, "", "spread")
}

// R05 — an import whose transaction fails must roll back, must not be reported as
// committed, and must release the settings write lock.
func TestFailedImportRollsBackAndReleasesLock(t *testing.T) {
	ctx := setupSettingTest(t)
	seedRouteMode(t, ctx, "fill_first", "fill_first")
	seedDefaultAdmin(t, ctx)

	const name = "test:reject_settings_insert"
	if err := db.GetDB().Callback().Create().Before("gorm:create").Register(name, func(tx *gorm.DB) {
		if tx.Statement != nil && tx.Statement.Table == "settings" {
			tx.AddError(errors.New("injected import insert failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	defer db.GetDB().Callback().Create().Remove(name)

	_, err := ImportAndInitCache(ctx, routeModeDump("spread"))
	if err == nil {
		t.Fatal("import must fail when its settings upsert is rejected")
	}
	var committed *ImportCacheRefreshError
	if errors.As(err, &committed) {
		t.Fatalf("a rolled-back import must not be reported as committed: %v", err)
	}
	assertRouteModeConsistent(t, "fill_first", "fill_first")

	_ = db.GetDB().Callback().Create().Remove(name)
	done := make(chan error, 1)
	go func() { done <- SettingSetString(model.SettingKeyRouteModeOverride, "spread") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("save after failed import: %v", err)
		}
	case <-time.After(parkWait):
		t.Fatal("settings write lock was not released after a failed import")
	}
	assertRouteModeConsistent(t, "spread", "spread")
}

// R03 — concurrent imports serialize instead of deadlocking or leaving the database
// and the cache disagreeing.
func TestConcurrentImportsStayConsistent(t *testing.T) {
	ctx := setupSettingTest(t)
	if err := settingRefreshCache(ctx); err != nil {
		t.Fatal(err)
	}
	seedDefaultAdmin(t, ctx)

	values := []string{"spread", "fill_first", "", " SPREAD ", " Fill_First "}
	errs := make([]error, len(values))
	// Release every goroutine at once so the imports genuinely overlap; two concurrent
	// imports used to interleave the fingerprint-preset seeding and fail with a UNIQUE
	// constraint, reported to the admin as a spurious cache-refresh failure.
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, value := range values {
		wg.Add(1)
		go func(i int, value string) {
			defer wg.Done()
			<-start
			_, errs[i] = ImportAndInitCache(ctx, routeModeDump(value))
		}(i, value)
	}
	close(start)
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(30 * time.Second):
		t.Fatal("concurrent imports deadlocked")
	}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("import %q: %v", values[i], err)
		}
	}

	var stored model.Setting
	if err := db.GetDB().First(&stored, "key = ?", model.SettingKeyRouteModeOverride).Error; err != nil {
		t.Fatal(err)
	}
	cached, err := SettingGetString(model.SettingKeyRouteModeOverride)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Value != cached {
		t.Fatalf("imports left DB=%q and cache=%q disagreeing", stored.Value, cached)
	}
	if stored.Value != "spread" && stored.Value != "fill_first" {
		t.Fatalf("imports settled on a non-canonical value %q", stored.Value)
	}
}

// A successful import publishes the imported-and-normalized value to both stores.
func TestSuccessfulImportPublishesNormalizedValue(t *testing.T) {
	ctx := setupSettingTest(t)
	if err := settingRefreshCache(ctx); err != nil {
		t.Fatal(err)
	}
	seedDefaultAdmin(t, ctx)
	for _, tc := range []struct{ in, want string }{
		{"", "fill_first"}, {"unknown-legacy", "fill_first"}, {" SPREAD ", "spread"}, {"fill_first", "fill_first"},
	} {
		if _, err := ImportAndInitCache(ctx, routeModeDump(tc.in)); err != nil {
			t.Fatalf("import %q: %v", tc.in, err)
		}
		assertRouteModeConsistent(t, tc.want, tc.want)
	}
	// An import that does not carry the key keeps the target's explicit choice.
	if _, err := ImportAndInitCache(ctx, &model.DBDump{Version: 2}); err != nil {
		t.Fatal(err)
	}
	assertRouteModeConsistent(t, "fill_first", "fill_first")
}

// The combined entry point must still be usable from a plain background context, e.g.
// a caller that passes a cancelled context must fail (and release the lock) rather
// than hang.
func TestImportWithCancelledContextFails(t *testing.T) {
	ctx := setupSettingTest(t)
	if err := settingRefreshCache(ctx); err != nil {
		t.Fatal(err)
	}
	seedDefaultAdmin(t, ctx)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()

	done := make(chan error, 1)
	go func() {
		_, err := ImportAndInitCache(cancelled, routeModeDump("spread"))
		done <- err
	}()
	select {
	case <-done:
	case <-time.After(parkWait * 3):
		t.Fatal("import with a cancelled context hung")
	}

	saveDone := make(chan error, 1)
	go func() { saveDone <- SettingSetString(model.SettingKeyRouteModeOverride, "spread") }()
	select {
	case err := <-saveDone:
		if err != nil {
			t.Fatalf("save after cancelled import: %v", err)
		}
	case <-time.After(parkWait):
		t.Fatal("settings write lock was not released after a cancelled import")
	}
}
