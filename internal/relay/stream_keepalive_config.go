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

func defaultStreamKeepaliveInterval() time.Duration {
	return envStreamSecondsDuration("RELAY_STREAM_KEEPALIVE_INTERVAL_SECONDS", defaultStreamKeepaliveIntervalSeconds)
}

func defaultStreamDataIntervalTimeout() time.Duration {
	return envStreamSecondsDuration("RELAY_STREAM_DATA_INTERVAL_TIMEOUT_SECONDS", defaultStreamDataIntervalTimeoutSeconds)
}

func currentFirstByteKeepaliveDelay() time.Duration {
	seconds, err := op.SettingGetInt(dbmodel.SettingKeyFirstByteKeepaliveDelaySeconds)
	if err == nil {
		return streamSecondsDuration(seconds)
	}
	return defaultFirstByteKeepaliveDelay()
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
