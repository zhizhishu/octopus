package balancer

import (
	"net/http"
	"sync"
	"testing"
)

// A model-level failure must not cool down a sibling model sharing the same channel
// and key. The breaker key is channelID:keyID:modelName, so the per-model breaker is
// what separates "this model is rate-limited / unhealthy" from "this key is bad" —
// the companion test in internal/model pins the key-wide short window.
func TestScopedCircuitBreakerDoesNotBleedAcrossModels(t *testing.T) {
	globalBreaker = sync.Map{}

	// Enough failures to trip model A's breaker (same record count the endpoint-scope
	// test uses to trip a scope deterministically).
	for range 10 {
		RecordFailureWithStatus(21, 5, "model-a", http.StatusServiceUnavailable)
	}
	if tripped, _ := IsTrippedScoped(21, 5, "model-a", "", ""); !tripped {
		t.Fatal("expected model-a breaker to trip on its own channel/key")
	}

	if tripped, _ := IsTrippedScoped(21, 5, "model-b", "", ""); tripped {
		t.Fatal("model-b must not inherit model-a's breaker on the same channel and key")
	}
	// A sibling model on a different key of the same channel is likewise unaffected.
	if tripped, _ := IsTrippedScoped(21, 6, "model-a", "", ""); tripped {
		t.Fatal("model-a on a sibling key must not inherit the other key's breaker")
	}

	// Resetting model A's circuit must not disturb the sibling model's state.
	ResetChannel(21)
	if tripped, _ := IsTrippedScoped(21, 5, "model-a", "", ""); tripped {
		t.Fatal("expected model-a breaker reset")
	}
	if tripped, _ := IsTrippedScoped(21, 5, "model-b", "", ""); tripped {
		t.Fatal("model-b must stay untripped after resetting model-a")
	}
}
