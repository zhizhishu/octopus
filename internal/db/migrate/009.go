package migrate

import (
	"fmt"
	"sort"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/utils/log"
	"gorm.io/gorm"
)

func init() {
	RegisterAfterAutoMigration(Migration{
		Version: 9,
		Up:      migrateRenumberBuiltinFingerprintProfiles,
	})
}

const (
	builtinDebianProfileName = "Linux · Debian"
	builtinUbuntuProfileName = "Linux · Ubuntu"
)

// migrateRenumberBuiltinFingerprintProfiles renumbers the two built-in Linux
// fingerprint presets to the low, gap-free primary keys 1 ("Linux · Debian") and 2
// ("Linux · Ubuntu").
//
// Why: earlier lifecycle churn (an auto-seeded redundant "默认(Windows)" row that a
// later build drops, plus a preset backfill) left live deployments with the two
// presets sitting at ids 2 and 4 (ids 1 & 3 were deleted). The IDs are cosmetic to
// the relay path — a channel pins a profile by ChannelCloak.ProfileID, not by row
// order — but the 2/4 gap is confusing in the UI and API. This packs them to 1/2.
//
// Safety / correctness:
//   - There is NO hard foreign key onto fingerprint_profiles from any other table's
//     real column. A channel that pins a profile stores the id inside its Cloak JSON
//     blob (serializer:json), NOT a FK column — so sqlite happily accepts an in-place
//     `UPDATE fingerprint_profiles SET id=?`. Those JSON references are remapped below.
//   - PK reassignment is done in two phases (park each moved row at a guaranteed-free
//     high id, then drop it onto its final id) so it can never transiently collide with
//     an occupied primary key — this is robust even against a hypothetical 1<->2 swap.
//   - Channel cloak references are remapped from the ORIGINAL id via a single snapshot
//     map, so a re-run (when nothing points at an old id anymore) is a no-op and it can
//     never double-remap.
//   - Idempotent: once the presets already sit at 1/2 the remap is empty and this is a
//     clean no-op, safe to run on every startup.
//   - Only sqlite (the live dialect) is renumbered; pg/mysql are skipped with a warning
//     rather than erroring, so a non-sqlite deployment still starts.
//   - Runs in AfterAutoMigrate, BEFORE op.InitCache loads the channel / profile caches,
//     so the caches always load already-consistent ids (no stale in-memory references).
func migrateRenumberBuiltinFingerprintProfiles(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}
	// Nothing to renumber until the table exists.
	if !db.Migrator().HasTable(&model.FingerprintProfile{}) {
		return nil
	}
	// An in-place primary-key renumber is only needed/safe on the live sqlite dialect.
	// Skip (don't error) elsewhere so a pg/mysql deployment still boots.
	if db.Dialector == nil || db.Dialector.Name() != "sqlite" {
		if db.Dialector != nil {
			log.Warnf("renumber built-in fingerprint profiles: skipping on dialect %q", db.Dialector.Name())
		}
		return nil
	}

	// Desired final primary keys for the two built-in presets.
	targets := []struct {
		name string
		id   int
	}{
		{builtinDebianProfileName, 1},
		{builtinUbuntuProfileName, 2},
	}

	var profiles []model.FingerprintProfile
	if err := db.Find(&profiles).Error; err != nil {
		return fmt.Errorf("failed to load fingerprint profiles: %w", err)
	}
	byName := make(map[string]model.FingerprintProfile, len(profiles))
	byID := make(map[int]model.FingerprintProfile, len(profiles))
	maxID := 0
	for _, p := range profiles {
		byName[p.Name] = p
		byID[p.ID] = p
		if p.ID > maxID {
			maxID = p.ID
		}
	}

	// Build the old->new remap for the built-ins that exist and are misplaced.
	remap := make(map[int]int, 2)
	for _, t := range targets {
		p, ok := byName[t.name]
		if !ok {
			// Preset absent (e.g. a fresh deploy that seeds directly at the right id
			// later, or an operator deleted it) — nothing to move.
			continue
		}
		if p.ID == t.id {
			// Already at its final id.
			continue
		}
		// Never clobber an UNRELATED operator profile that already occupies the target
		// id. The only rows allowed to sit on a target id are the two built-ins (both of
		// which are themselves being moved onto 1/2). Anything else -> skip the whole
		// migration rather than overwrite operator data or crash startup.
		if occ, occupied := byID[t.id]; occupied {
			if occ.Name != builtinDebianProfileName && occ.Name != builtinUbuntuProfileName {
				log.Warnf("renumber built-in fingerprint profiles: target id %d already used by %q; skipping to avoid clobbering", t.id, occ.Name)
				return nil
			}
		}
		remap[p.ID] = t.id
	}

	if len(remap) == 0 {
		// Presets already at 1/2 (or absent) — nothing to do.
		return nil
	}

	// Deterministic order for stable, reproducible SQL.
	oldIDs := make([]int, 0, len(remap))
	for oldID := range remap {
		oldIDs = append(oldIDs, oldID)
	}
	sort.Ints(oldIDs)

	// Phase 1: park every moved row at a guaranteed-free high id.
	parkBase := maxID + 1000
	parkOf := make(map[int]int, len(remap))
	for i, oldID := range oldIDs {
		park := parkBase + i + 1
		parkOf[oldID] = park
		if err := db.Exec("UPDATE fingerprint_profiles SET id = ? WHERE id = ?", park, oldID).Error; err != nil {
			return fmt.Errorf("failed to park fingerprint profile id %d: %w", oldID, err)
		}
	}
	// Phase 2: drop each parked row onto its final id.
	for _, oldID := range oldIDs {
		if err := db.Exec("UPDATE fingerprint_profiles SET id = ? WHERE id = ?", remap[oldID], parkOf[oldID]).Error; err != nil {
			return fmt.Errorf("failed to assign fingerprint profile final id %d: %w", remap[oldID], err)
		}
	}

	// Remap channel cloak.ProfileID references. Iterate channels once and compute the
	// new ProfileID from the ORIGINAL value via the snapshot remap, so it can't double
	// remap: a re-run finds no channel pointing at an old id and changes nothing.
	if db.Migrator().HasTable(&model.Channel{}) {
		var channels []model.Channel
		if err := db.Find(&channels).Error; err != nil {
			return fmt.Errorf("failed to load channels for cloak remap: %w", err)
		}
		for i := range channels {
			ch := &channels[i]
			newID, ok := remap[ch.Cloak.ProfileID]
			if !ok {
				continue
			}
			ch.Cloak.ProfileID = newID
			// Mirror op.ChannelUpdate: Select("cloak").Updates(...) writes only the cloak
			// column and re-runs the JSON serializer, preserving Cloak.Mode.
			if err := db.Model(&model.Channel{}).Where("id = ?", ch.ID).
				Select("cloak").Updates(&model.Channel{Cloak: ch.Cloak}).Error; err != nil {
				return fmt.Errorf("failed to remap channel %d cloak profile id: %w", ch.ID, err)
			}
		}
	}

	// Reset the sqlite AUTOINCREMENT high-water mark so the next insert continues right
	// after the new max id (=2) instead of a stale pre-renumber value. Guarded: the
	// sqlite_sequence table only exists when some table was created with AUTOINCREMENT,
	// and the UPDATE is a no-op if fingerprint_profiles has no sequence row.
	var seqTableCount int64
	if err := db.Raw("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'sqlite_sequence'").Scan(&seqTableCount).Error; err != nil {
		return fmt.Errorf("failed to probe sqlite_sequence: %w", err)
	}
	if seqTableCount > 0 {
		if err := db.Exec(
			"UPDATE sqlite_sequence SET seq = (SELECT COALESCE(MAX(id), 0) FROM fingerprint_profiles) WHERE name = 'fingerprint_profiles'",
		).Error; err != nil {
			return fmt.Errorf("failed to reset fingerprint_profiles sequence: %w", err)
		}
	}

	return nil
}
