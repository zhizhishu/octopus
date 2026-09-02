package relay

import (
	"strings"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
)

// routeModeOverrideFromSetting maps the route_mode_override setting value to an
// effective GroupMode override. "spread" → Spread; everything else (fill_first,
// empty, unknown) → FillFirst. Never returns 0 (always overrides).
func routeModeOverrideFromSetting(raw string) dbmodel.GroupMode {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "spread":
		return dbmodel.GroupModeSpread
	default:
		return dbmodel.GroupModeFillFirst
	}
}

// applyGroupGlobalDefaultsResolved is the pure core of applyGroupGlobalDefaults:
// it takes the already-read setting values so it can be unit-tested without a
// settings cache.
//
//   - modeOverrideRaw, when spread, forces Spread; any other value (fill_first,
//     empty, unknown) forces FillFirst. Applied to every group that is NOT mode-locked.
//     Locked groups carry an admin's explicit per-group choice (group editor /
//     access-plan canvas), so the global value acts as a DEFAULT for everyone else rather
//     than a steamroller: "unless the canvas changed it, the global setting wins".
//     Since routeModeOverrideFromSetting never returns 0, an empty value still resolves
//     to the FillFirst default.
//   - firstTokenDefault is a fallback applied ONLY when the group's own
//     FirstTokenTimeOut is unset (<=0), mirroring session_keep_time_default. 0 or
//     negative means no global default, preserving per-group-only behavior.
func applyGroupGlobalDefaultsResolved(group dbmodel.Group, modeOverrideRaw string, firstTokenDefault int) dbmodel.Group {
	if !group.ModeLocked {
		group.Mode = routeModeOverrideFromSetting(modeOverrideRaw)
	}
	if group.FirstTokenTimeOut <= 0 && firstTokenDefault > 0 {
		group.FirstTokenTimeOut = firstTokenDefault
	}
	return group
}

// applyGroupGlobalDefaults resolves fleet-wide routing config onto a group copy
// before any routing decision is made. It reads the cached route_mode_override and
// first_token_time_out_default settings (a graceful fallback to the group's own
// values on missing/parse error) and delegates to applyGroupGlobalDefaultsResolved.
//
// It must run before enrichGroupForSmartRouting's fill-first short-circuit so the
// effective (possibly overridden) mode decides whether load-aware stats get
// hydrated, and so every routing path (relay / raw-protocol / images, primary +
// fallback — all funnel through enrichGroupForSmartRouting) sees the same effective
// mode and first-token timeout.
func applyGroupGlobalDefaults(group dbmodel.Group) dbmodel.Group {
	modeOverrideRaw, _ := op.SettingGetString(dbmodel.SettingKeyRouteModeOverride)
	firstTokenDefault, _ := op.SettingGetInt(dbmodel.SettingKeyFirstTokenTimeOutDefault)
	return applyGroupGlobalDefaultsResolved(group, modeOverrideRaw, firstTokenDefault)
}
