package migrate

import (
	"testing"

	"github.com/bestruirui/octopus/internal/model"
	"gorm.io/gorm"
)

// assertFollowGlobalMigrated asserts the post-migration invariant: every channel that
// pinned the follow-global sentinel (ProfileID 0) now points at the Debian preset (id 1)
// with its Cloak.Mode preserved, and a channel already pinned to a concrete preset is
// left untouched.
func assertFollowGlobalMigrated(t *testing.T, gdb *gorm.DB, stage string) {
	t.Helper()
	get := func(name string) model.Channel {
		var ch model.Channel
		if err := gdb.Where("name = ?", name).First(&ch).Error; err != nil {
			t.Fatalf("%s: load channel %q: %v", stage, name, err)
		}
		return ch
	}
	if ch := get("follow-auto"); ch.Cloak.ProfileID != 1 || ch.Cloak.Mode != "auto" {
		t.Fatalf("%s: follow-auto cloak = %+v, want {Mode:auto ProfileID:1}", stage, ch.Cloak)
	}
	if ch := get("follow-always"); ch.Cloak.ProfileID != 1 || ch.Cloak.Mode != "always" {
		t.Fatalf("%s: follow-always cloak = %+v, want {Mode:always ProfileID:1}", stage, ch.Cloak)
	}
	if ch := get("follow-never"); ch.Cloak.ProfileID != 1 || ch.Cloak.Mode != "never" {
		t.Fatalf("%s: follow-never cloak = %+v, want {Mode:never ProfileID:1}", stage, ch.Cloak)
	}
	if ch := get("follow-emptymode"); ch.Cloak.ProfileID != 1 || ch.Cloak.Mode != "" {
		t.Fatalf("%s: follow-emptymode cloak = %+v, want {Mode:\"\" ProfileID:1}", stage, ch.Cloak)
	}
	if ch := get("pinned-ubuntu"); ch.Cloak.ProfileID != 2 || ch.Cloak.Mode != "auto" {
		t.Fatalf("%s: pinned-ubuntu cloak = %+v, want {Mode:auto ProfileID:2}", stage, ch.Cloak)
	}
}

// TestMigrateChannelsFollowGlobalToDebian seeds channels across every cloak mode still on
// the follow-global sentinel (ProfileID 0) plus one already pinned to Ubuntu, runs the
// migration, and asserts every follow-global channel is repointed to Debian (id 1) with
// its mode preserved while the pinned channel is untouched. It then runs a SECOND time to
// prove idempotency (a clean no-op once nothing points at 0).
func TestMigrateChannelsFollowGlobalToDebian(t *testing.T) {
	gdb := newRenumberTestDB(t)

	if err := gdb.Create(&model.FingerprintProfile{ID: 1, Name: builtinDebianProfileName}).Error; err != nil {
		t.Fatalf("seed debian preset: %v", err)
	}
	if err := gdb.Create(&model.FingerprintProfile{ID: 2, Name: builtinUbuntuProfileName}).Error; err != nil {
		t.Fatalf("seed ubuntu preset: %v", err)
	}

	channels := []*model.Channel{
		{Name: "follow-auto", Cloak: model.ChannelCloak{Mode: "auto", ProfileID: 0}},
		{Name: "follow-always", Cloak: model.ChannelCloak{Mode: "always", ProfileID: 0}},
		{Name: "follow-never", Cloak: model.ChannelCloak{Mode: "never", ProfileID: 0}},
		{Name: "follow-emptymode", Cloak: model.ChannelCloak{Mode: "", ProfileID: 0}},
		{Name: "pinned-ubuntu", Cloak: model.ChannelCloak{Mode: "auto", ProfileID: 2}},
	}
	for _, ch := range channels {
		if err := gdb.Create(ch).Error; err != nil {
			t.Fatalf("seed channel %q: %v", ch.Name, err)
		}
	}

	if err := migrateChannelsFollowGlobalToDebian(gdb); err != nil {
		t.Fatalf("first run: %v", err)
	}
	assertFollowGlobalMigrated(t, gdb, "after first run")

	if err := migrateChannelsFollowGlobalToDebian(gdb); err != nil {
		t.Fatalf("second run (idempotency): %v", err)
	}
	assertFollowGlobalMigrated(t, gdb, "after second run")
}

// TestMigrateChannelsFollowGlobalToDebianSkipsWhenPresetAbsent proves the migration is a
// safe no-op (no error, no channel churn) when the Debian preset does not exist — it must
// never repoint a channel onto a missing profile.
func TestMigrateChannelsFollowGlobalToDebianSkipsWhenPresetAbsent(t *testing.T) {
	gdb := newRenumberTestDB(t)

	if err := gdb.Create(&model.FingerprintProfile{ID: 2, Name: builtinUbuntuProfileName}).Error; err != nil {
		t.Fatalf("seed ubuntu preset: %v", err)
	}
	if err := gdb.Create(&model.Channel{Name: "follow-auto", Cloak: model.ChannelCloak{Mode: "auto", ProfileID: 0}}).Error; err != nil {
		t.Fatalf("seed channel: %v", err)
	}

	if err := migrateChannelsFollowGlobalToDebian(gdb); err != nil {
		t.Fatalf("run with absent Debian preset: %v", err)
	}
	var ch model.Channel
	if err := gdb.Where("name = ?", "follow-auto").First(&ch).Error; err != nil {
		t.Fatalf("load channel: %v", err)
	}
	if ch.Cloak.ProfileID != 0 {
		t.Fatalf("channel repointed despite absent Debian preset: profile_id = %d, want 0", ch.Cloak.ProfileID)
	}
}

// TestMigrateChannelsFollowGlobalToDebianLegacyDebianName resolves the Debian preset by its
// LEGACY name ("Linux 真机") — the pre-rename direct-upgrade case, since op.InitCache's
// rename runs after this migration — and still repoints the follow-global channel at it.
func TestMigrateChannelsFollowGlobalToDebianLegacyDebianName(t *testing.T) {
	gdb := newRenumberTestDB(t)

	if err := gdb.Create(&model.FingerprintProfile{ID: 1, Name: "Linux 真机"}).Error; err != nil {
		t.Fatalf("seed legacy debian: %v", err)
	}
	if err := gdb.Create(&model.Channel{Name: "follow-auto", Cloak: model.ChannelCloak{Mode: "auto", ProfileID: 0}}).Error; err != nil {
		t.Fatalf("seed channel: %v", err)
	}

	if err := migrateChannelsFollowGlobalToDebian(gdb); err != nil {
		t.Fatalf("run: %v", err)
	}
	var ch model.Channel
	if err := gdb.Where("name = ?", "follow-auto").First(&ch).Error; err != nil {
		t.Fatalf("load channel: %v", err)
	}
	if ch.Cloak.ProfileID != 1 {
		t.Fatalf("channel profile_id = %d, want 1 (migrated to legacy-named Debian preset)", ch.Cloak.ProfileID)
	}
}
