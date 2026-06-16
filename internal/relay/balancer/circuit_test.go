package balancer

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/bestruirui/octopus/internal/model"
)

func TestTransientFailuresUseGentlerBreakerPolicy(t *testing.T) {
	globalBreaker = sync.Map{}

	for i := 0; i < 9; i++ {
		RecordFailureWithStatus(4, 1, "claude-opus-4-7[1m]", http.StatusServiceUnavailable)
		if tripped, _ := IsTripped(4, 1, "claude-opus-4-7[1m]"); tripped {
			t.Fatalf("breaker tripped after %d transient failures, want still closed", i+1)
		}
	}

	RecordFailureWithStatus(4, 1, "claude-opus-4-7[1m]", http.StatusServiceUnavailable)
	tripped, remaining := IsTripped(4, 1, "claude-opus-4-7[1m]")
	if !tripped {
		t.Fatalf("breaker did not trip after tenth transient failure")
	}
	if remaining <= 0 || remaining > 30*time.Second {
		t.Fatalf("expected transient cooldown around 30s, got %s", remaining)
	}

	RecordFailureWithStatus(4, 1, "claude-opus-4-7[1m]", http.StatusServiceUnavailable)
	tripped, remaining = IsTripped(4, 1, "claude-opus-4-7[1m]")
	if !tripped {
		t.Fatalf("breaker should stay tripped after another transient failure")
	}
	if remaining <= 0 || remaining > 30*time.Second {
		t.Fatalf("expected transient max cooldown capped around 30s, got %s", remaining)
	}

	policy := failurePolicyForStatus(http.StatusServiceUnavailable, "claude-opus-4-7[1m]")
	if got := getCooldownForPolicy(4, policy); got != 30*time.Second {
		t.Fatalf("expected repeated transient cooldown cap 30s, got %s", got)
	}
}

func TestClaudeOneMillionPolicyAppliesWithoutStatus(t *testing.T) {
	policy := failurePolicyForStatus(0, "claude-opus-4-7[1m]")
	if policy.thresholdFloor != 10 || policy.cooldownBaseCeil != 30 || policy.cooldownMaxCeil != 30 {
		t.Fatalf("unexpected claude 1m policy: %#v", policy)
	}
}

func TestClaudeOneMillionPolicyAppliesFromCapability(t *testing.T) {
	policy := failurePolicyForStatusAndCapability(0, "claude-fable-5", "stream+anthropic_context_1m")
	if policy.thresholdFloor != 10 || policy.cooldownBaseCeil != 30 || policy.cooldownMaxCeil != 30 {
		t.Fatalf("unexpected clean-model claude 1m policy: %#v", policy)
	}
}

func TestScopedCircuitBreakerDoesNotBleedAcrossEndpointCapability(t *testing.T) {
	globalBreaker = sync.Map{}

	for i := 0; i < 10; i++ {
		RecordFailureWithStatusScoped(7, 3, "claude-fable-5", "messages", "stream+anthropic_context_1m", http.StatusServiceUnavailable)
	}
	if tripped, _ := IsTrippedScoped(7, 3, "claude-fable-5", "messages", "stream+anthropic_context_1m"); !tripped {
		t.Fatalf("expected scoped messages/1m breaker to trip")
	}
	if tripped, _ := IsTrippedScoped(7, 3, "claude-fable-5", "responses", "stream"); tripped {
		t.Fatalf("responses stream scope must not inherit messages/1m cooldown")
	}
	if tripped, _ := IsTrippedScoped(7, 3, "claude-fable-5", "messages", "stream"); tripped {
		t.Fatalf("same endpoint with different capability must not inherit 1m cooldown")
	}
}

func TestSnapshotAndResetChannelCircuit(t *testing.T) {
	globalBreaker = sync.Map{}

	for i := 0; i < 10; i++ {
		RecordFailureWithStatus(9, 2, "claude-opus-4-7[1m]", http.StatusServiceUnavailable)
	}
	for i := 0; i < 10; i++ {
		RecordFailureWithStatus(10, 3, "claude-opus-4-7[1m]", http.StatusServiceUnavailable)
	}

	status := SnapshotChannel(9)
	if !status.Tripped {
		t.Fatalf("expected channel 9 to be tripped")
	}
	if status.OpenKeys != 1 {
		t.Fatalf("expected one open breaker entry, got %d", status.OpenKeys)
	}
	if status.RemainingSeconds <= 0 || status.RemainingSeconds > 30 {
		t.Fatalf("expected remaining cooldown around 30s, got %d", status.RemainingSeconds)
	}

	if deleted := ResetChannel(9); deleted != 1 {
		t.Fatalf("expected one deleted breaker entry, got %d", deleted)
	}
	if status := SnapshotChannel(9); status.Tripped || status.OpenKeys != 0 {
		t.Fatalf("expected channel 9 breaker reset, got %#v", status)
	}
	if status := SnapshotChannel(10); !status.Tripped {
		t.Fatalf("resetting channel 9 must not clear channel 10")
	}
}

func TestIteratorPrioritizeChannelsKeepsPreferredFirst(t *testing.T) {
	iter := NewIterator(model.Group{
		Mode: model.GroupModeFailover,
		Items: []model.GroupItem{
			{ChannelID: 1, ModelName: "chat", Priority: 1},
			{ChannelID: 2, ModelName: "responses", Priority: 2},
			{ChannelID: 3, ModelName: "anthropic", Priority: 3},
		},
	}, 0, "request-model")

	iter.PrioritizeChannels(map[int]bool{2: true})

	if !iter.Next() {
		t.Fatalf("expected first candidate")
	}
	if got := iter.Item().ChannelID; got != 2 {
		t.Fatalf("expected preferred channel first, got %d", got)
	}
	if !iter.Next() || iter.Item().ChannelID != 1 {
		t.Fatalf("expected original order inside non-preferred bucket")
	}
	if !iter.Next() || iter.Item().ChannelID != 3 {
		t.Fatalf("expected remaining non-preferred channel")
	}
}
