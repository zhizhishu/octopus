package balancer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/zhizhishu/octopus/internal/utils/log"
)

// Breaker and hold state lives in memory; without a checkpoint a restart forgets which
// key/model was open and immediately re-sends traffic into a known-bad upstream. The
// checkpoint is a small JSON file written atomically on a timer and on shutdown and read
// once at boot. Only state that still matters is kept: open/half-open breakers and holds
// that have not expired. Soft runtime telemetry is deliberately not persisted (it is
// seconds-scale and rebuilt by traffic).

const circuitCheckpointVersion = 1

type checkpointPolicy struct {
	ThresholdFloor   int64  `json:"threshold_floor,omitempty"`
	CooldownBaseCeil int    `json:"cooldown_base_ceil,omitempty"`
	CooldownMaxCeil  int    `json:"cooldown_max_ceil,omitempty"`
	Reason           string `json:"reason,omitempty"`
}

type checkpointCircuit struct {
	Key                 string           `json:"key"`
	ConsecutiveFailures int64            `json:"consecutive_failures"`
	LastFailureUnixMs   int64            `json:"last_failure_unix_ms"`
	TripCount           int              `json:"trip_count"`
	Policy              checkpointPolicy `json:"policy"`
}

type checkpointHold struct {
	Key         string `json:"key"`
	UntilUnixMs int64  `json:"until_unix_ms"`
	Rule        string `json:"rule,omitempty"`
	// Endpoint/Capability 是触发该 hold 的端点作用域（熔断探测用的元数据）。
	// 可选键：旧文件没有它，恢复出来就是老口径（未知作用域，不探）。
	Endpoint   string `json:"endpoint,omitempty"`
	Capability string `json:"capability,omitempty"`
}

type circuitCheckpoint struct {
	Version       int                 `json:"version"`
	SavedAtUnixMs int64               `json:"saved_at_unix_ms"`
	Circuits      []checkpointCircuit `json:"circuits"`
	Holds         []checkpointHold    `json:"holds"`
	// Probe 是熔断探测调度的持久状态（当日计数、连续未恢复计数、每渠道历史环）。
	// 它是可选键：旧文件没有它，读出来就是 nil；新文件里这个键对旧读取器只是未知键。
	Probe *checkpointProbe `json:"probe,omitempty"`
}

func captureCheckpoint(now time.Time) circuitCheckpoint {
	cp := circuitCheckpoint{Version: circuitCheckpointVersion, SavedAtUnixMs: now.UnixMilli()}
	globalBreaker.Range(func(key, value any) bool {
		keyString, ok := key.(string)
		if !ok {
			return true
		}
		entry, ok := value.(*circuitEntry)
		if !ok {
			return true
		}
		entry.mu.Lock()
		if entry.State != StateClosed {
			cp.Circuits = append(cp.Circuits, checkpointCircuit{
				Key:                 keyString,
				ConsecutiveFailures: entry.ConsecutiveFailures,
				LastFailureUnixMs:   entry.LastFailureTime.UnixMilli(),
				TripCount:           entry.TripCount,
				Policy: checkpointPolicy{
					ThresholdFloor:   entry.Policy.thresholdFloor,
					CooldownBaseCeil: entry.Policy.cooldownBaseCeil,
					CooldownMaxCeil:  entry.Policy.cooldownMaxCeil,
					Reason:           entry.Policy.reason,
				},
			})
		}
		entry.mu.Unlock()
		return true
	})
	globalHolds.Range(func(key, value any) bool {
		entry := value.(holdEntry)
		if entry.until.After(now) {
			cp.Holds = append(cp.Holds, checkpointHold{
				Key:         key.(string),
				UntilUnixMs: entry.until.UnixMilli(),
				Rule:        entry.rule,
				Endpoint:    entry.endpoint,
				Capability:  entry.capability,
			})
		}
		return true
	})
	sort.Slice(cp.Circuits, func(i, j int) bool { return cp.Circuits[i].Key < cp.Circuits[j].Key })
	sort.Slice(cp.Holds, func(i, j int) bool { return cp.Holds[i].Key < cp.Holds[j].Key })
	cp.Probe = captureProbeCheckpoint()
	return cp
}

// restoreCheckpoint loads the snapshot into memory. A breaker comes back Open with its
// original failure time, so the ordinary cooldown check decides whether it is still held
// or already due for a half-open probe; a half-open probe that was in flight at shutdown
// can never report, so restoring it as HalfOpen would block the key forever.
func restoreCheckpoint(cp circuitCheckpoint, now time.Time) (circuits, holds int) {
	restoreProbeCheckpoint(cp.Probe)
	for _, item := range cp.Circuits {
		if _, ok := parseCircuitKey(item.Key); !ok {
			continue
		}
		policy := circuitFailurePolicy{
			thresholdFloor:   item.Policy.ThresholdFloor,
			cooldownBaseCeil: item.Policy.CooldownBaseCeil,
			cooldownMaxCeil:  item.Policy.CooldownMaxCeil,
			reason:           item.Policy.Reason,
		}
		lastFailure := time.UnixMilli(item.LastFailureUnixMs)
		if now.Sub(lastFailure) >= getCooldownForPolicy(item.TripCount, policy) {
			// Already past its cooldown: a first trip has nothing left to protect. A repeat
			// offender is kept so its trip count (and so its longer backoff) survives; the
			// next routing check turns it half-open and lets one probe through.
			if item.TripCount <= 1 {
				continue
			}
		}
		entry := &circuitEntry{
			State:               StateOpen,
			ConsecutiveFailures: item.ConsecutiveFailures,
			LastFailureTime:     lastFailure,
			TripCount:           item.TripCount,
			Policy:              policy,
			writeSeq:            nextCircuitRevision(),
		}
		if _, loaded := globalBreaker.LoadOrStore(item.Key, entry); !loaded {
			circuits++
		}
	}
	for _, item := range cp.Holds {
		until := time.UnixMilli(item.UntilUnixMs)
		if !until.After(now) {
			continue
		}
		if _, ok := parseCircuitKey(item.Key); !ok {
			continue
		}
		if _, loaded := globalHolds.LoadOrStore(item.Key, holdEntry{
			until:      until,
			rule:       item.Rule,
			endpoint:   item.Endpoint,
			capability: item.Capability,
			writeSeq:   nextCircuitRevision(),
		}); !loaded {
			holds++
		}
	}
	return circuits, holds
}

var checkpointMu sync.Mutex
var lastCheckpointBody []byte

// SaveCheckpoint writes the current state to path atomically. It skips the write when
// nothing changed since the last save (the timestamp is excluded from that comparison).
func SaveCheckpoint(path string) error {
	checkpointMu.Lock()
	defer checkpointMu.Unlock()
	cp := captureCheckpoint(time.Now())
	stable := cp
	stable.SavedAtUnixMs = 0
	body, err := json.Marshal(stable)
	if err != nil {
		return err
	}
	if bytes.Equal(body, lastCheckpointBody) {
		return nil
	}
	data, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	lastCheckpointBody = body
	return nil
}

// LoadCheckpoint restores state saved by SaveCheckpoint. A missing file is not an error.
func LoadCheckpoint(path string) (circuits, holds int, err error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	var cp circuitCheckpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return 0, 0, fmt.Errorf("decode circuit checkpoint: %w", err)
	}
	if cp.Version != circuitCheckpointVersion {
		return 0, 0, fmt.Errorf("circuit checkpoint version %d not supported", cp.Version)
	}
	circuits, holds = restoreCheckpoint(cp, time.Now())
	return circuits, holds, nil
}

// StartCheckpointLoop restores path once, then saves every interval until stop is closed.
// The returned save function is for the shutdown hook.
func StartCheckpointLoop(path string, interval time.Duration, stop <-chan struct{}) func() error {
	if circuits, holds, err := LoadCheckpoint(path); err != nil {
		log.Warnf("circuit checkpoint not restored from %s: %v", path, err)
	} else if circuits > 0 || holds > 0 {
		log.Infof("circuit checkpoint restored from %s: %d breakers, %d holds", path, circuits, holds)
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if err := SaveCheckpoint(path); err != nil {
					log.Warnf("circuit checkpoint save failed: %v", err)
				}
			}
		}
	}()
	return func() error { return SaveCheckpoint(path) }
}
