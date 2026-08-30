package intervention

import (
	"time"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
)

const (
	defaultTimeoutSeconds = 1800
	// A held request pins one upstream connection slot, so the registry is capped. Past
	// the cap new failures fall straight through to the client rather than queueing up
	// an unbounded number of blocked goroutines.
	maxPending = 64
)

// Enabled reports whether failed requests should be held for operator review instead of
// returning the upstream error. Defaults to off so existing deployments are unaffected.
func Enabled() bool {
	enabled, err := op.SettingGetBool(dbmodel.SettingKeyRelayInterventionEnabled)
	if err != nil {
		return false
	}
	return enabled
}

// Timeout is how long a held request waits for an operator before giving up and letting
// the original error through.
func Timeout() time.Duration {
	seconds, err := op.SettingGetInt(dbmodel.SettingKeyRelayInterventionTimeoutSec)
	if err != nil || seconds <= 0 {
		return defaultTimeoutSeconds * time.Second
	}
	return time.Duration(seconds) * time.Second
}

// HasCapacity reports whether another request may be held.
func HasCapacity() bool {
	registry.RLock()
	defer registry.RUnlock()
	return len(registry.pending) < maxPending
}
