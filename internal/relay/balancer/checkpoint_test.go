package balancer

import (
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestCheckpointRoundTripRestoresOpenBreakersAndHolds(t *testing.T) {
	resetHoldState()
	lastCheckpointBody = nil
	tripForTest(61, 1, "m-open")
	ApplyHold(61, 2, "", "key", 10*time.Minute, "quota")
	ApplyHold(61, 3, "m-x", "model", -time.Second, "never") // ignored: non-positive

	path := filepath.Join(t.TempDir(), "circuit_state.json")
	if err := SaveCheckpoint(path); err != nil {
		t.Fatalf("save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("checkpoint missing: %v", err)
	}
	firstMod := info.ModTime()

	// Unchanged state must not rewrite the file.
	time.Sleep(10 * time.Millisecond)
	if err := SaveCheckpoint(path); err != nil {
		t.Fatalf("second save: %v", err)
	}
	if info, _ := os.Stat(path); !info.ModTime().Equal(firstMod) {
		t.Fatal("an unchanged checkpoint must not be rewritten")
	}

	// Simulate a restart.
	globalBreaker = sync.Map{}
	globalHolds = sync.Map{}
	circuits, holds, err := LoadCheckpoint(path)
	if err != nil || circuits != 1 || holds != 1 {
		t.Fatalf("load = %d circuits, %d holds, %v; want 1, 1, nil", circuits, holds, err)
	}
	if tripped, remaining := IsTripped(61, 1, "m-open"); !tripped || remaining <= 0 {
		t.Fatalf("restored breaker must still be open, got %v %v", tripped, remaining)
	}
	if tripped, _ := IsTripped(61, 2, "any-model"); !tripped {
		t.Fatal("restored key hold must still cover the key")
	}
}

func TestCheckpointDropsExpiredAndRestoresHalfOpenAsProbeable(t *testing.T) {
	resetHoldState()
	now := time.Now()
	cp := circuitCheckpoint{
		Version: circuitCheckpointVersion,
		Circuits: []checkpointCircuit{
			// First trip, long past any cooldown: dropped.
			{Key: circuitKey(62, 1, "old"), TripCount: 1, LastFailureUnixMs: now.Add(-time.Hour).UnixMilli()},
			// Repeat offender past cooldown: kept, and immediately probeable (half-open).
			{Key: circuitKey(62, 2, "repeat"), TripCount: 3, LastFailureUnixMs: now.Add(-time.Hour).UnixMilli()},
		},
		Holds: []checkpointHold{
			{Key: holdKey(62, 3, "gone"), UntilUnixMs: now.Add(-time.Second).UnixMilli(), Rule: "expired"},
		},
	}
	circuits, holds := restoreCheckpoint(cp, now)
	if circuits != 1 || holds != 0 {
		t.Fatalf("restored %d circuits, %d holds; want 1, 0", circuits, holds)
	}
	if tripped, _ := IsTripped(62, 2, "repeat"); tripped {
		t.Fatal("a past-cooldown breaker must let the first probe through after restart")
	}
	// The probe is now in flight: a second caller is held until it reports.
	if tripped, _ := IsTripped(62, 2, "repeat"); !tripped {
		t.Fatal("while the probe is in flight the entry is half-open")
	}
	RecordFailureWithStatus(62, 2, "repeat", http.StatusServiceUnavailable)
	if tripped, remaining := IsTripped(62, 2, "repeat"); !tripped || remaining <= 0 {
		t.Fatal("a failed probe re-opens with the kept backoff")
	}
}

func TestLoadCheckpointMissingFileIsNotAnError(t *testing.T) {
	if c, h, err := LoadCheckpoint(filepath.Join(t.TempDir(), "none.json")); err != nil || c != 0 || h != 0 {
		t.Fatalf("missing file = %d %d %v", c, h, err)
	}
}
