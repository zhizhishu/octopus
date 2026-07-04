package relay

import (
	"testing"

	dbmodel "github.com/bestruirui/octopus/internal/model"
)

func TestRouteModeOverrideFromSetting(t *testing.T) {
	cases := []struct {
		raw  string
		want dbmodel.GroupMode
	}{
		{"", 0},
		{"   ", 0},
		{"spread", dbmodel.GroupModeSpread},
		{"SPREAD", dbmodel.GroupModeSpread},
		{" fill_first ", dbmodel.GroupModeFillFirst},
		{"Fill_First", dbmodel.GroupModeFillFirst},
		{"bogus", 0},
		{"round_robin", 0}, // only the canonical setting values map; guard against silent aliases
	}
	for _, c := range cases {
		if got := routeModeOverrideFromSetting(c.raw); got != c.want {
			t.Errorf("routeModeOverrideFromSetting(%q) = %d, want %d", c.raw, got, c.want)
		}
	}
}

func TestApplyGroupGlobalDefaultsResolved(t *testing.T) {
	// No override, no default: group untouched (legacy per-group-only behavior).
	g := applyGroupGlobalDefaultsResolved(dbmodel.Group{Mode: dbmodel.GroupModeFillFirst, FirstTokenTimeOut: 0}, "", 0)
	if g.Mode != dbmodel.GroupModeFillFirst || g.FirstTokenTimeOut != 0 {
		t.Fatalf("expected unchanged, got mode=%d ftt=%d", g.Mode, g.FirstTokenTimeOut)
	}

	// Mode override forces spread even though the group is stored as fill-first.
	g = applyGroupGlobalDefaultsResolved(dbmodel.Group{Mode: dbmodel.GroupModeFillFirst}, "spread", 0)
	if g.Mode != dbmodel.GroupModeSpread {
		t.Fatalf("expected spread override, got %d", g.Mode)
	}

	// Mode override forces fill-first even though the group is stored as spread.
	g = applyGroupGlobalDefaultsResolved(dbmodel.Group{Mode: dbmodel.GroupModeSpread}, "fill_first", 0)
	if g.Mode != dbmodel.GroupModeFillFirst {
		t.Fatalf("expected fill_first override, got %d", g.Mode)
	}

	// First-token global default fills in only when the group's own is unset (<=0).
	g = applyGroupGlobalDefaultsResolved(dbmodel.Group{FirstTokenTimeOut: 0}, "", 30)
	if g.FirstTokenTimeOut != 30 {
		t.Fatalf("expected fallback 30, got %d", g.FirstTokenTimeOut)
	}

	// The group's own first-token timeout wins over the global default.
	g = applyGroupGlobalDefaultsResolved(dbmodel.Group{FirstTokenTimeOut: 5}, "", 30)
	if g.FirstTokenTimeOut != 5 {
		t.Fatalf("expected group value 5 to win, got %d", g.FirstTokenTimeOut)
	}

	// Both applied together, and the group's stored mode is overridden.
	g = applyGroupGlobalDefaultsResolved(dbmodel.Group{Mode: dbmodel.GroupModeFillFirst, FirstTokenTimeOut: 0}, "spread", 45)
	if g.Mode != dbmodel.GroupModeSpread || g.FirstTokenTimeOut != 45 {
		t.Fatalf("expected spread+45, got mode=%d ftt=%d", g.Mode, g.FirstTokenTimeOut)
	}
}
