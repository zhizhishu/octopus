package relay

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bestruirui/octopus/internal/conf"
	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
)

const (
	defaultStreamKeepaliveIntervalSeconds   = 15
	defaultStreamDataIntervalTimeoutSeconds = 900
)

func currentStreamKeepaliveInterval() time.Duration {
	seconds, err := op.SettingGetInt(dbmodel.SettingKeyRelayStreamKeepaliveSec)
	if err == nil {
		return streamSecondsDuration(seconds)
	}
	return defaultStreamKeepaliveInterval()
}

func currentStreamDataIntervalTimeout() time.Duration {
	seconds, err := op.SettingGetInt(dbmodel.SettingKeyRelayStreamDataTimeoutSec)
	if err == nil {
		return streamSecondsDuration(seconds)
	}
	return defaultStreamDataIntervalTimeout()
}

// currentUpstreamHeaderTimeout bounds how long ONE attempt may wait for the upstream
// to send response headers. 0 (the default) keeps the old behaviour: wait forever.
//
// This is the one gap the first-content guard cannot cover. That guard starts a timer
// only once headers have arrived, so an upstream that accepts the request and never
// answers headers used to be bounded by nothing at all — measured on an isolated
// instance: the attempt sat for the full 60s client window with the guard armed at
// 10s, because the guard had not started. Bounding the header wait turns that into an
// ordinary attempt failure, so failover and automatic rescue continue as before.
func currentUpstreamHeaderTimeout() time.Duration {
	seconds, err := op.SettingGetInt(dbmodel.SettingKeyUpstreamHeaderTimeoutSec)
	if err != nil {
		return clampUpstreamHeaderTimeout(defaultUpstreamHeaderTimeout())
	}
	return clampUpstreamHeaderTimeout(streamSecondsDuration(seconds))
}

// clampUpstreamHeaderTimeout keeps the budget inside the range the setting accepts.
// Defensive: a value written straight into the database (bypassing Validate) would
// otherwise be multiplied into a time.Duration and could overflow negative, which the
// caller reads as "disabled" — the guard would silently disappear.
func clampUpstreamHeaderTimeout(budget time.Duration) time.Duration {
	max := time.Duration(dbmodel.MaxUpstreamHeaderTimeoutSeconds) * time.Second
	if budget > max {
		return max
	}
	return budget
}

func defaultStreamKeepaliveInterval() time.Duration {
	return envStreamSecondsDuration("RELAY_STREAM_KEEPALIVE_INTERVAL_SECONDS", defaultStreamKeepaliveIntervalSeconds)
}

func defaultStreamDataIntervalTimeout() time.Duration {
	return envStreamSecondsDuration("RELAY_STREAM_DATA_INTERVAL_TIMEOUT_SECONDS", defaultStreamDataIntervalTimeoutSeconds)
}

func defaultUpstreamHeaderTimeout() time.Duration {
	// Default 0 = disabled: unchanged behaviour until an operator opts in, because a
	// too-short budget would cut slow-but-healthy channels. Override with
	// OCTOPUS_UPSTREAM_HEADER_TIMEOUT_SECONDS or the runtime setting.
	return envStreamSecondsDuration("UPSTREAM_HEADER_TIMEOUT_SECONDS", 0)
}

func currentFirstByteKeepaliveDelay() time.Duration {
	seconds, err := op.SettingGetInt(dbmodel.SettingKeyFirstByteKeepaliveDelaySeconds)
	if err == nil {
		return streamSecondsDuration(seconds)
	}
	return defaultFirstByteKeepaliveDelay()
}

// currentInterventionKeepaliveDelay is the hold-loop sibling of the first-byte delay.
// The intervention hold restarts its downstream keepalive every round, so this delay
// must be short enough to fire inside one round (hold backoff caps at ~15s); the 20s
// first-byte default would never fire and the held client would sit byte-silent.
func currentInterventionKeepaliveDelay() time.Duration {
	seconds, err := op.SettingGetInt(dbmodel.SettingKeyInterventionKeepaliveDelaySeconds)
	if err == nil {
		return streamSecondsDuration(seconds)
	}
	return defaultInterventionKeepaliveDelay()
}

func defaultFirstByteKeepaliveDelay() time.Duration {
	// Default ON at 20s. Only upstreams slower than this to their first byte get
	// pre-content SSE comment heartbeats (":\n\n"), which keeps a downstream client
	// (e.g. Cursor's ~60s idle timeout) connected through a slow-first-token upstream
	// instead of the client aborting at 60s and masking the real result. Fast upstreams
	// (<20s to first byte) are completely unaffected — the heartbeat goroutine never
	// fires. Downstream-only and ignorable: it never touches the codex/claude/gemini
	// upstream fingerprint. Override via OCTOPUS_RELAY_FIRST_BYTE_KEEPALIVE_DELAY_SECONDS
	// or the runtime setting (set to 0 to disable).
	return envStreamSecondsDuration("RELAY_FIRST_BYTE_KEEPALIVE_DELAY_SECONDS", 20)
}

func defaultInterventionKeepaliveDelay() time.Duration {
	// Default ON at 2s: short enough to fire inside one hold round (backoff caps at
	// ~15s), so a held client sees "working" heartbeats instead of byte-silence until
	// the rescue budget dies. Override via OCTOPUS_RELAY_INTERVENTION_KEEPALIVE_DELAY_SECONDS
	// or the runtime setting (set to 0 to disable).
	return envStreamSecondsDuration("RELAY_INTERVENTION_KEEPALIVE_DELAY_SECONDS", 2)
}

func envStreamSecondsDuration(suffix string, fallbackSeconds int) time.Duration {
	raw := strings.TrimSpace(os.Getenv(strings.ToUpper(conf.APP_NAME) + "_" + suffix))
	if raw == "" {
		return streamSecondsDuration(fallbackSeconds)
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil {
		return streamSecondsDuration(fallbackSeconds)
	}
	return streamSecondsDuration(seconds)
}

func streamSecondsDuration(seconds int) time.Duration {
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}
