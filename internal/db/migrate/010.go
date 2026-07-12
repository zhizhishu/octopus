package migrate

import (
	"errors"
	"fmt"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/utils/log"
	"gorm.io/gorm"
)

func init() {
	RegisterAfterAutoMigration(Migration{
		Version: 10,
		Up:      migrateChannelsFollowGlobalToDebian,
	})
}

// migrateChannelsFollowGlobalToDebian repoints every channel still pinned to the legacy
// "跟随全局 / follow-global" fingerprint sentinel (ChannelCloak.ProfileID == 0) at the
// built-in "Linux · Debian" preset.
//
// Why: the client-fingerprint UI dropped the "Default (follow global)" dropdown option,
// so a channel now always names a concrete preset (Debian / Ubuntu). Rows created under
// the old default still sit at ProfileID 0 — without this they would render blank in the
// edit dialog and keep silently inheriting the global UA. This converges them onto the
// Debian preset, matching the new create-form default.
//
// Safety / correctness:
//   - Shape-safe: the Debian preset's Claude fingerprint fields (UA / OS / arch / package
//     & runtime version / timeout) are byte-identical to the global Claude defaults, so a
//     Claude channel moves with an UNCHANGED Claude wire fingerprint; the Codex UA differs
//     only by a real OS token (Ubuntu -> Debian, both valid codex_cli_rs UAs); Gemini /
//     chat channels don't consume the profile at all, so the move is inert for them.
//   - Preserves Cloak.Mode: writes the whole cloak JSON column via Select("cloak"),
//     mirroring op.ChannelUpdate, so auto / always / never / "" modes are untouched.
//   - Resolves the Debian preset by NAME (not a hard-coded id) so it stays correct
//     regardless of id-renumber migration ordering; skips if the preset is absent.
//   - Idempotent: once no channel points at 0 the pass changes nothing, so it is safe to
//     run on every startup.
//   - Runs in AfterAutoMigrate, BEFORE op.InitCache loads the channel cache, so the cache
//     loads already-converged references.
func migrateChannelsFollowGlobalToDebian(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}
	if !db.Migrator().HasTable(&model.Channel{}) || !db.Migrator().HasTable(&model.FingerprintProfile{}) {
		return nil
	}

	// Resolve the Debian preset by name — new OR legacy (op.InitCache's rename runs after
	// this migration, so a pre-rename upgrade still has the old name here). If it is absent
	// there is nothing safe to point follow-global channels at, so skip rather than migrate
	// onto a missing profile.
	var debian model.FingerprintProfile
	if err := db.Where("name IN ?", builtinDebianProfileNames).First(&debian).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			log.Warnf("follow-global channel migration: Debian preset (any of %v) absent; skipping", builtinDebianProfileNames)
			return nil
		}
		return fmt.Errorf("failed to load Debian preset: %w", err)
	}
	if debian.ID <= 0 {
		log.Warnf("follow-global channel migration: %q preset has non-positive id %d; skipping", builtinDebianProfileName, debian.ID)
		return nil
	}

	var channels []model.Channel
	if err := db.Find(&channels).Error; err != nil {
		return fmt.Errorf("failed to load channels: %w", err)
	}
	migrated := 0
	for i := range channels {
		ch := &channels[i]
		if ch.Cloak.ProfileID != 0 {
			continue
		}
		ch.Cloak.ProfileID = debian.ID
		// Select("cloak") force-writes only the cloak JSON column and re-runs the JSON
		// serializer, preserving Cloak.Mode (even the empty "" mode).
		if err := db.Model(&model.Channel{}).Where("id = ?", ch.ID).
			Select("cloak").Updates(&model.Channel{Cloak: ch.Cloak}).Error; err != nil {
			return fmt.Errorf("failed to repoint channel %d to Debian preset: %w", ch.ID, err)
		}
		migrated++
	}
	if migrated > 0 {
		log.Infof("follow-global channel migration: repointed %d channel(s) to %q (id %d)", migrated, builtinDebianProfileName, debian.ID)
	}
	return nil
}
