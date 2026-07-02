package balancer

import (
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
)

// sessionKeepTimeDefault returns the global fallback sticky window (seconds) applied
// when a group's own SessionKeepTime is unset (<=0). It reads the cached
// session_keep_time_default setting so an admin can turn sticky on fleet-wide without
// editing every group; 0 (or unset / parse error) means no global default, preserving
// the legacy per-group-only behavior.
//
// Mirrors circuit.go's getThreshold(): a direct cached-setting read with a graceful
// fallback. Kept as a func var so balancer unit tests can override it (like
// DecisionLogHook) without wiring up a settings cache.
var sessionKeepTimeDefault = func() int {
	v, err := op.SettingGetInt(model.SettingKeySessionKeepTimeDefault)
	if err != nil || v <= 0 {
		return 0
	}
	return v
}
