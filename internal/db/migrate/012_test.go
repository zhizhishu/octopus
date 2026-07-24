package migrate

import (
	"testing"

	"github.com/bestruirui/octopus/internal/model"
)

// TestMigrateNormalizeLegacyGroupModes seeds groups across every legacy + canonical
// mode, runs the migration, and asserts the retired random(2)/weighted(4)/smart(5)
// values are rewritten to spread(1) while spread(1) and failover(3) are left
// untouched. A second run proves idempotency (a clean no-op once nothing is legacy).
func TestMigrateNormalizeLegacyGroupModes(t *testing.T) {
	gdb := newRenumberTestDB(t)
	if err := gdb.AutoMigrate(&model.Group{}); err != nil {
		t.Fatalf("automigrate groups: %v", err)
	}

	seed := []*model.Group{
		{Name: "g-roundrobin", Mode: model.GroupModeRoundRobin}, // 1 canonical spread
		{Name: "g-random", Mode: 2},                             // legacy random
		{Name: "g-failover", Mode: model.GroupModeFailover},     // 3 canonical fillfirst
		{Name: "g-weighted", Mode: 4},                           // legacy weighted
		{Name: "g-smart", Mode: 5},                              // legacy smart
	}
	for _, g := range seed {
		if err := gdb.Create(g).Error; err != nil {
			t.Fatalf("seed group %q: %v", g.Name, err)
		}
	}

	want := map[string]model.GroupMode{
		"g-roundrobin": model.GroupModeSpread,    // 1 unchanged
		"g-random":     model.GroupModeSpread,    // 2 -> 1
		"g-failover":   model.GroupModeFillFirst, // 3 unchanged
		"g-weighted":   model.GroupModeSpread,    // 4 -> 1
		"g-smart":      model.GroupModeSpread,    // 5 -> 1
	}
	assertModes := func(stage string) {
		t.Helper()
		for name, wantMode := range want {
			var g model.Group
			if err := gdb.Where("name = ?", name).First(&g).Error; err != nil {
				t.Fatalf("%s: load group %q: %v", stage, name, err)
			}
			if g.Mode != wantMode {
				t.Fatalf("%s: group %q mode = %d, want %d", stage, name, g.Mode, wantMode)
			}
		}
	}

	if err := migrateNormalizeLegacyGroupModes(gdb); err != nil {
		t.Fatalf("first run: %v", err)
	}
	assertModes("after first run")

	if err := migrateNormalizeLegacyGroupModes(gdb); err != nil {
		t.Fatalf("second run (idempotency): %v", err)
	}
	assertModes("after second run")
}
