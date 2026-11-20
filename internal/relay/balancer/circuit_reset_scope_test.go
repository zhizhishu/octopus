package balancer

import (
	"net/http"
	"sync"
	"testing"
	"time"
)

func tripForTest(channelID, keyID int, modelName string) {
	for range 10 {
		RecordFailureWithStatus(channelID, keyID, modelName, http.StatusServiceUnavailable)
	}
}

func TestParseCircuitKeyKeepsColonModelsAndScope(t *testing.T) {
	cases := map[string]parsedCircuitKey{
		circuitKey(3, 7, "llama3:8b"): {channelID: 3, keyID: 7, model: "llama3:8b"},
		circuitScopedKey(3, 7, "llama3:8b", "responses", "image"): {
			channelID: 3, keyID: 7, model: "llama3:8b", endpoint: "responses", capability: "image",
		},
	}
	for key, want := range cases {
		got, ok := parseCircuitKey(key)
		if !ok || got != want {
			t.Fatalf("parseCircuitKey(%q) = %+v, %v; want %+v", key, got, ok, want)
		}
	}
	if _, ok := parseCircuitKey("bad"); ok {
		t.Fatal("malformed key must not parse")
	}
}

func TestResetScopedClearsOnlyTheNamedKeyAndModel(t *testing.T) {
	globalBreaker = sync.Map{}
	ResetRuntimeTelemetry()
	tripForTest(31, 1, "model-a")
	tripForTest(31, 1, "model-b")
	tripForTest(31, 2, "model-a")

	if got := len(ListChannelCircuits(31)); got != 3 {
		t.Fatalf("open entries = %d, want 3", got)
	}

	if n := ResetScoped(31, 1, "model-a"); n != 1 {
		t.Fatalf("ResetScoped(key 1, model-a) cleared %d, want 1", n)
	}
	if tripped, _ := IsTripped(31, 1, "model-a"); tripped {
		t.Fatal("key 1 / model-a must be reset")
	}
	if tripped, _ := IsTripped(31, 1, "model-b"); !tripped {
		t.Fatal("key 1 / model-b must stay open")
	}
	if tripped, _ := IsTripped(31, 2, "model-a"); !tripped {
		t.Fatal("key 2 / model-a must stay open")
	}

	if n := ResetScoped(31, 0, "model-b"); n != 1 {
		t.Fatalf("ResetScoped(model-b on all keys) cleared %d, want 1", n)
	}
	views := ListChannelCircuits(31)
	if len(views) != 1 || views[0].KeyID != 2 || views[0].Model != "model-a" || views[0].State != "open" {
		t.Fatalf("remaining entries = %+v, want only key 2 / model-a open", views)
	}
	if views[0].RemainingSeconds <= 0 {
		t.Fatalf("open entry must report remaining cooldown, got %+v", views[0])
	}
	if n := ResetChannel(31); n != 1 {
		t.Fatalf("ResetChannel cleared %d, want 1", n)
	}
	if got := len(ListChannelCircuits(31)); got != 0 {
		t.Fatalf("entries after channel reset = %d, want 0", got)
	}
}

func TestResetScopedLiftsSoftRuntimeCooldown(t *testing.T) {
	globalBreaker = sync.Map{}
	ResetRuntimeTelemetry()
	RecordRuntimeFailure(41, 3, "model-a", http.StatusTooManyRequests, time.Millisecond, time.Minute)
	RecordRuntimeFailure(41, 4, "model-a", http.StatusTooManyRequests, time.Millisecond, time.Minute)

	cooling := func(keyID int) bool {
		entry, ok := loadRuntimeEntry(41, keyID, "model-a")
		if !ok {
			return false
		}
		entry.mu.Lock()
		defer entry.mu.Unlock()
		return time.Now().Before(entry.cooldownUntil)
	}
	if !cooling(3) || !cooling(4) || !cooling(0) {
		t.Fatal("precondition: keys 3, 4 and the channel aggregate must be cooling")
	}

	ResetScoped(41, 3, "model-a")
	if cooling(3) {
		t.Fatal("key 3 soft cooldown must be lifted")
	}
	if !cooling(4) {
		t.Fatal("key 4 soft cooldown must be untouched by a key-3 reset")
	}
	if !cooling(0) {
		t.Fatal("a single-key reset must not clear the channel aggregate")
	}

	ResetScoped(41, 0, "model-a")
	if cooling(4) || cooling(0) {
		t.Fatal("a model-wide reset must lift every key and the aggregate")
	}
}
