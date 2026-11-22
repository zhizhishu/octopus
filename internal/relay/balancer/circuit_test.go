package balancer

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/model"
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

func TestAuthFailuresUseShortCooldownCap(t *testing.T) {
	policy := failurePolicyForStatus(http.StatusForbidden, "glm-5.2")
	if policy.reason != "auth_or_permission" {
		t.Fatalf("expected auth_or_permission policy, got %#v", policy)
	}
	if policy.cooldownBaseCeil != 15 || policy.cooldownMaxCeil != 30 {
		t.Fatalf("auth cooldown caps should be base<=15 max<=30, got %#v", policy)
	}
	// Even with high tripCount, auth must not open for minutes.
	if got := getCooldownForPolicy(8, policy); got > 30*time.Second {
		t.Fatalf("auth cooldown at tripCount=8 must stay <=30s, got %s", got)
	}
}

func TestAbsoluteMaxCooldownCapsExtremeSettings(t *testing.T) {
	// Simulate unset/default path: no policy ceil, default max was historically 600s.
	// Absolute safety cap must keep multi-trip backoff under 3 minutes.
	if got := getCooldownForPolicy(20, circuitFailurePolicy{}); got > 180*time.Second {
		t.Fatalf("absolute max cooldown must be <=180s, got %s", got)
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

func TestClosedCircuitCheckDoesNotClaimHalfOpenProbe(t *testing.T) {
	const channelID, keyID = 1013, 1014
	const name, endpoint, capability = "claude-sonnet-4-5", "messages", "stream"
	key := circuitScopedKey(channelID, keyID, name, endpoint, capability)
	t.Cleanup(func() { globalBreaker.Delete(key) })
	if !CircuitClosedScoped(channelID, keyID, name, endpoint, capability) {
		t.Fatal("new scope must start closed")
	}
	globalBreaker.Store(key, &circuitEntry{
		State: StateOpen, TripCount: 1, LastFailureTime: time.Now().Add(-time.Hour),
	})
	if CircuitClosedScoped(channelID, keyID, name, endpoint, capability) {
		t.Fatal("continuation admitted after cooldown but before a successful probe")
	}
	if tripped, _ := IsTrippedScoped(channelID, keyID, name, endpoint, capability); tripped {
		t.Fatal("an ordinary attempt must still be able to claim the half-open probe")
	}
	if CircuitClosedScoped(channelID, keyID, name, endpoint, capability) {
		t.Fatal("continuation admitted while a probe is in flight")
	}
}

func TestStreamResumeOutcomeDoesNotSettleAnOrdinaryHalfOpenProbe(t *testing.T) {
	const channelID, keyID = 1015, 1016
	const name, endpoint, capability = "claude-sonnet-4-5", "messages", "stream"
	key := circuitScopedKey(channelID, keyID, name, endpoint, capability)
	t.Cleanup(func() { globalBreaker.Delete(key) })

	entry := &circuitEntry{State: StateClosed}
	globalBreaker.Store(key, entry)
	RecordContinuationCircuitOutcomeScoped(channelID, keyID, name, endpoint, capability, 500, false)
	if entry.ConsecutiveFailures != 1 {
		t.Fatal("continuation failure was not counted while closed")
	}
	RecordContinuationCircuitOutcomeScoped(channelID, keyID, name, endpoint, capability, 200, true)
	if entry.ConsecutiveFailures != 0 {
		t.Fatal("continuation success was not counted while closed")
	}

	lastFailure := time.Now().Add(-time.Hour)
	entry.State, entry.TripCount, entry.LastFailureTime = StateOpen, 1, lastFailure
	if tripped, _ := IsTrippedScoped(channelID, keyID, name, endpoint, capability); tripped {
		t.Fatal("ordinary request could not claim the probe")
	}
	RecordContinuationCircuitOutcomeScoped(channelID, keyID, name, endpoint, capability, 200, true)
	RecordContinuationCircuitOutcomeScoped(channelID, keyID, name, endpoint, capability, 500, false)
	if entry.State != StateHalfOpen || entry.TripCount != 1 || !entry.LastFailureTime.Equal(lastFailure) {
		t.Fatal("continuation settled or changed the ordinary probe")
	}
	RecordSuccessScoped(channelID, keyID, name, endpoint, capability)
	if entry.State != StateClosed || entry.TripCount != 0 {
		t.Fatal("the ordinary probe could not settle normally")
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

func TestIteratorPrioritizeChannelsStaysWithinHardTier(t *testing.T) {
	// FillFirst's hard tier is the (ChannelPriority, Priority) pair, so three channels
	// with distinct item priorities are three different tiers. A preferred channel can
	// only be reordered inside its own tier and must NOT leapfrog a higher tier — the
	// old global-two-way-split expectation (preferred channel to the very front) is gone.
	iter := NewIterator(model.Group{
		Mode: model.GroupModeFailover,
		Items: []model.GroupItem{
			{ChannelID: 1, ModelName: "chat", Priority: 1},
			{ChannelID: 2, ModelName: "responses", Priority: 2},
			{ChannelID: 3, ModelName: "anthropic", Priority: 3},
		},
	}, 0, "request-model")

	iter.PrioritizeChannels(map[int]bool{2: true})

	if !iter.Next() || iter.Item().ChannelID != 1 {
		t.Fatalf("higher tier must stay first; a lower-tier preferred channel must not rise")
	}
	if !iter.Next() || iter.Item().ChannelID != 2 {
		t.Fatalf("original priority order must be preserved within distinct tiers")
	}
	if !iter.Next() || iter.Item().ChannelID != 3 {
		t.Fatalf("expected the remaining channel last")
	}
}
