package relay

import (
	"testing"

	dbmodel "github.com/bestruirui/octopus/internal/model"
)

func TestRouteModeOverrideFromSetting(t *testing.T) {
	// The mapper accepts four input categories: explicit spread, explicit
	// fill_first, empty, and unknown. Only spread surfaces as GroupModeSpread;
	// every other value (fill_first, empty, unknown) resolves to GroupModeFillFirst
	// so the effective default is always a concrete mode and never 0.
	cases := []struct {
		name string
		raw  string
		want dbmodel.GroupMode
	}{
		{"spread canonical", "spread", dbmodel.GroupModeSpread},
		{"spread mixed-case", "SPREAD", dbmodel.GroupModeSpread},
		{"spread trimmed", " spread ", dbmodel.GroupModeSpread},
		{"fill_first canonical", "fill_first", dbmodel.GroupModeFillFirst},
		{"fill_first mixed-case", "Fill_First", dbmodel.GroupModeFillFirst},
		{"fill_first trimmed", " fill_first ", dbmodel.GroupModeFillFirst},
		{"empty -> fill_first", "", dbmodel.GroupModeFillFirst},
		{"blank -> fill_first", "   ", dbmodel.GroupModeFillFirst},
		{"unknown -> fill_first", "bogus", dbmodel.GroupModeFillFirst},
		{"unknown alias -> fill_first", "round_robin", dbmodel.GroupModeFillFirst},
	}
	for _, c := range cases {
		if got := routeModeOverrideFromSetting(c.raw); got != c.want {
			t.Errorf("%s: routeModeOverrideFromSetting(%q) = %d, want %d", c.name, c.raw, got, c.want)
		}
	}
}

func TestApplyGroupGlobalDefaultsResolved(t *testing.T) {
	t.Run("unlocked groups follow the global mode", func(t *testing.T) {
		// Four input categories (spread / fill_first / empty / unknown) all apply
		// as an override whenever the group is NOT mode-locked.
		cases := []struct {
			name   string
			stored dbmodel.GroupMode
			raw    string
			want   dbmodel.GroupMode
		}{
			{"spread overrides stored fill_first", dbmodel.GroupModeFillFirst, "spread", dbmodel.GroupModeSpread},
			{"spread overrides stored spread (idempotent)", dbmodel.GroupModeSpread, "spread", dbmodel.GroupModeSpread},
			{"fill_first overrides stored spread", dbmodel.GroupModeSpread, "fill_first", dbmodel.GroupModeFillFirst},
			{"empty resolves to fill_first", dbmodel.GroupModeSpread, "", dbmodel.GroupModeFillFirst},
			{"unknown resolves to fill_first", dbmodel.GroupModeSpread, "bogus", dbmodel.GroupModeFillFirst},
		}
		for _, c := range cases {
			g := applyGroupGlobalDefaultsResolved(dbmodel.Group{Mode: c.stored}, c.raw, 0)
			if g.Mode != c.want {
				t.Errorf("%s: mode = %d, want %d", c.name, g.Mode, c.want)
			}
		}
	})

	t.Run("locked groups keep their own mode regardless of the global", func(t *testing.T) {
		// A mode-locked group preserves its explicit per-group choice for every
		// global value (spread / fill_first / empty / unknown).
		cases := []struct {
			name   string
			stored dbmodel.GroupMode
			raw    string
		}{
			{"spread global keeps locked spread", dbmodel.GroupModeSpread, "spread"},
			{"fill_first global keeps locked spread", dbmodel.GroupModeSpread, "fill_first"},
			{"empty global keeps locked spread", dbmodel.GroupModeSpread, ""},
			{"unknown global keeps locked spread", dbmodel.GroupModeSpread, "bogus"},
			{"spread global keeps locked fill_first", dbmodel.GroupModeFillFirst, "spread"},
			{"empty global keeps locked fill_first", dbmodel.GroupModeFillFirst, ""},
		}
		for _, c := range cases {
			g := applyGroupGlobalDefaultsResolved(dbmodel.Group{Mode: c.stored, ModeLocked: true}, c.raw, 0)
			if g.Mode != c.stored {
				t.Errorf("%s: locked mode changed = %d, want %d (unchanged)", c.name, g.Mode, c.stored)
			}
		}
	})

	t.Run("first-token default is only a fallback", func(t *testing.T) {
		// Global first-token default fills in only when the group's own is unset (<=0).
		g := applyGroupGlobalDefaultsResolved(dbmodel.Group{FirstTokenTimeOut: 0}, "", 30)
		if g.FirstTokenTimeOut != 30 {
			t.Fatalf("expected fallback 30, got %d", g.FirstTokenTimeOut)
		}

		// The group's own first-token timeout wins over the global default.
		g = applyGroupGlobalDefaultsResolved(dbmodel.Group{FirstTokenTimeOut: 5}, "", 30)
		if g.FirstTokenTimeOut != 5 {
			t.Fatalf("expected group value 5 to win, got %d", g.FirstTokenTimeOut)
		}

		// Both applied together on an unlocked group: mode overridden and timeout filled.
		g = applyGroupGlobalDefaultsResolved(dbmodel.Group{Mode: dbmodel.GroupModeFillFirst, FirstTokenTimeOut: 0}, "spread", 45)
		if g.Mode != dbmodel.GroupModeSpread || g.FirstTokenTimeOut != 45 {
			t.Fatalf("expected spread+45, got mode=%d ftt=%d", g.Mode, g.FirstTokenTimeOut)
		}

		// On a locked group the global mode never applies, but the first-token
		// fallback still does (its own timeout was unset).
		g = applyGroupGlobalDefaultsResolved(dbmodel.Group{Mode: dbmodel.GroupModeFillFirst, ModeLocked: true, FirstTokenTimeOut: 0}, "spread", 45)
		if g.Mode != dbmodel.GroupModeFillFirst || g.FirstTokenTimeOut != 45 {
			t.Fatalf("expected kept fill_first + fallback 45, got mode=%d ftt=%d", g.Mode, g.FirstTokenTimeOut)
		}
	})
}