package migrate

import (
	"path/filepath"
	"testing"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func newRenumberTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "octopus.db")
	gdb, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	// Close the sqlite handle before t.TempDir()'s cleanup deletes the file — Windows
	// refuses to unlink a file still held open (t.Cleanup is LIFO, so this runs first).
	if sqlDB, dberr := gdb.DB(); dberr == nil {
		t.Cleanup(func() { _ = sqlDB.Close() })
	}
	if err := gdb.AutoMigrate(
		&model.FingerprintProfile{},
		&model.Channel{},
		&model.ChannelKey{},
		&model.StatsChannel{},
	); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return gdb
}

// assertRenumbered asserts the post-migration invariant: the two built-in presets
// sit at ids 1 (Debian) & 2 (Ubuntu), the channel that pinned the Debian preset now
// points at id 1, the channel that pinned Ubuntu now points at id 2, an unpinned
// channel is untouched, and Cloak.Mode is preserved on every remapped channel.
func assertRenumbered(t *testing.T, gdb *gorm.DB, stage string) {
	t.Helper()

	var profiles []model.FingerprintProfile
	if err := gdb.Order("id").Find(&profiles).Error; err != nil {
		t.Fatalf("%s: reload profiles: %v", stage, err)
	}
	if len(profiles) != 2 {
		t.Fatalf("%s: expected 2 profiles, got %d: %+v", stage, len(profiles), profiles)
	}
	byName := make(map[string]model.FingerprintProfile, len(profiles))
	for _, p := range profiles {
		byName[p.Name] = p
	}
	if got := byName[builtinDebianProfileName].ID; got != 1 {
		t.Fatalf("%s: Debian preset id = %d, want 1 (%+v)", stage, got, profiles)
	}
	if got := byName[builtinUbuntuProfileName].ID; got != 2 {
		t.Fatalf("%s: Ubuntu preset id = %d, want 2 (%+v)", stage, got, profiles)
	}

	get := func(name string) model.Channel {
		var ch model.Channel
		if err := gdb.Where("name = ?", name).First(&ch).Error; err != nil {
			t.Fatalf("%s: load channel %q: %v", stage, name, err)
		}
		return ch
	}
	if ch := get("pins-debian"); ch.Cloak.ProfileID != 1 || ch.Cloak.Mode != "auto" {
		t.Fatalf("%s: pins-debian cloak = %+v, want {Mode:auto ProfileID:1}", stage, ch.Cloak)
	}
	if ch := get("pins-ubuntu"); ch.Cloak.ProfileID != 2 || ch.Cloak.Mode != "always" {
		t.Fatalf("%s: pins-ubuntu cloak = %+v, want {Mode:always ProfileID:2}", stage, ch.Cloak)
	}
	if ch := get("no-pin"); ch.Cloak.ProfileID != 0 {
		t.Fatalf("%s: no-pin cloak profile id = %d, want 0", stage, ch.Cloak.ProfileID)
	}
}

// TestMigrateRenumberBuiltinFingerprintProfiles seeds the exact live shape — the two
// built-in presets at ids 2 & 4 (ids 1 & 3 deleted) plus a channel pinning the Debian
// preset via cloak.ProfileID=2 — runs the migration, and asserts the presets are
// packed to 1 & 2 and every channel cloak reference is remapped. It then runs the
// migration a SECOND time to prove idempotency (a clean no-op once ids are 1 & 2).
func TestMigrateRenumberBuiltinFingerprintProfiles(t *testing.T) {
	gdb := newRenumberTestDB(t)

	debian := &model.FingerprintProfile{
		ID:             2,
		Name:           builtinDebianProfileName,
		CodexUserAgent: "codex_cli_rs/0.144.1 (Debian 12.0.0; x86_64) unknown (codex_cli_rs; 0.144.1)",
	}
	ubuntu := &model.FingerprintProfile{
		ID:             4,
		Name:           builtinUbuntuProfileName,
		CodexUserAgent: "codex_cli_rs/0.144.1 (Ubuntu 24.04.1; x86_64) unknown (codex_cli_rs; 0.144.1)",
	}
	if err := gdb.Create(debian).Error; err != nil {
		t.Fatalf("seed debian preset: %v", err)
	}
	if err := gdb.Create(ubuntu).Error; err != nil {
		t.Fatalf("seed ubuntu preset: %v", err)
	}

	channels := []*model.Channel{
		{Name: "pins-debian", Cloak: model.ChannelCloak{Mode: "auto", ProfileID: 2}},
		{Name: "pins-ubuntu", Cloak: model.ChannelCloak{Mode: "always", ProfileID: 4}},
		{Name: "no-pin", Cloak: model.ChannelCloak{Mode: "auto", ProfileID: 0}},
	}
	for _, ch := range channels {
		if err := gdb.Create(ch).Error; err != nil {
			t.Fatalf("seed channel %q: %v", ch.Name, err)
		}
	}

	if err := migrateRenumberBuiltinFingerprintProfiles(gdb); err != nil {
		t.Fatalf("first run: %v", err)
	}
	assertRenumbered(t, gdb, "after first run")

	// Idempotency: a second run must not error and must leave the state identical.
	if err := migrateRenumberBuiltinFingerprintProfiles(gdb); err != nil {
		t.Fatalf("second run (idempotency): %v", err)
	}
	assertRenumbered(t, gdb, "after second run")
}

// TestMigrateRenumberBuiltinFingerprintProfilesNoOpWhenAlreadyPacked proves the
// migration is a clean no-op (no error, no id churn) when the presets already sit at
// 1 & 2 — the fresh-deploy / already-converged case.
func TestMigrateRenumberBuiltinFingerprintProfilesNoOpWhenAlreadyPacked(t *testing.T) {
	gdb := newRenumberTestDB(t)

	if err := gdb.Create(&model.FingerprintProfile{ID: 1, Name: builtinDebianProfileName}).Error; err != nil {
		t.Fatalf("seed debian preset: %v", err)
	}
	if err := gdb.Create(&model.FingerprintProfile{ID: 2, Name: builtinUbuntuProfileName}).Error; err != nil {
		t.Fatalf("seed ubuntu preset: %v", err)
	}
	if err := gdb.Create(&model.Channel{Name: "pins-debian", Cloak: model.ChannelCloak{Mode: "auto", ProfileID: 1}}).Error; err != nil {
		t.Fatalf("seed channel: %v", err)
	}
	if err := gdb.Create(&model.Channel{Name: "pins-ubuntu", Cloak: model.ChannelCloak{Mode: "always", ProfileID: 2}}).Error; err != nil {
		t.Fatalf("seed channel: %v", err)
	}
	if err := gdb.Create(&model.Channel{Name: "no-pin", Cloak: model.ChannelCloak{Mode: "auto", ProfileID: 0}}).Error; err != nil {
		t.Fatalf("seed channel: %v", err)
	}

	if err := migrateRenumberBuiltinFingerprintProfiles(gdb); err != nil {
		t.Fatalf("run on already-packed db: %v", err)
	}
	assertRenumbered(t, gdb, "already-packed")
}
