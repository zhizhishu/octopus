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
