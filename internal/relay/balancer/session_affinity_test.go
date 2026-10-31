package balancer

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/model"
)

// resetAffinityState isolates one test: clean sticky store + counter, clean breaker /
// round-robin / telemetry, and settings func vars pinned to "nothing configured".
func resetAffinityState(t *testing.T, affinityOn bool) {
	t.Helper()
	globalSession = sync.Map{}
	sessionEntryCount.Store(0)
	globalBreaker = sync.Map{}
	roundRobinCounters = sync.Map{}
	smoothWeightedStates = sync.Map{}
	ResetRuntimeTelemetry()
	prevKeep, prevAffinity, prevMax := sessionKeepTimeDefault, sessionAffinityEnabled, maxSessionEntries
	sessionKeepTimeDefault = func() int { return 0 }
	sessionAffinityEnabled = func() bool { return affinityOn }
	t.Cleanup(func() {
		sessionKeepTimeDefault, sessionAffinityEnabled, maxSessionEntries = prevKeep, prevAffinity, prevMax
		globalSession = sync.Map{}
		sessionEntryCount.Store(0)
		globalBreaker = sync.Map{}
	})
}

// planRouteGroup mirrors what op.AccessPlanGroupForModel builds for a 轮询 rule:
// SessionKeepTime is never set (0), every target sits in the same tier.
func planRouteGroup(mode model.GroupMode, channelIDs ...int) model.Group {
	items := make([]model.GroupItem, 0, len(channelIDs))
	for _, id := range channelIDs {
		items = append(items, model.GroupItem{ChannelID: id, ModelName: "claude-opus", Priority: 1, Weight: 1, RoutingWeight: 1})
	}
	return model.Group{Name: "claude-opus", Mode: mode, ModeLocked: true, Items: items}
}

func firstChannel(t *testing.T, it *Iterator) int {
	t.Helper()
	if !it.Next() {
		t.Fatalf("expected a candidate")
	}
	return it.Item().ChannelID
}

// Same session, plan group with no configured keep time, 轮询 (mode 6 and mode 1): every
// later turn must land on the channel+key that served the first turn, while a request
// without that session keeps rotating.
func TestSessionAffinityDefaultPinsSameSessionAcrossRotation(t *testing.T) {
	for _, mode := range []model.GroupMode{model.GroupModeSpreadTiered, model.GroupModeSpread, model.GroupModeFillFirst} {
		t.Run(fmt.Sprintf("mode%d", mode), func(t *testing.T) {
			resetAffinityState(t, true)
			group := planRouteGroup(mode, 1, 2, 3)
			const session = "anthropic-session:abc"

			first := NewIteratorWithSession(group, 7, "claude-opus", session, true)
			pinned := firstChannel(t, first)
			if first.IsSticky() {
				t.Fatalf("first turn has no binding yet, must not be sticky")
			}
			SetStickyWithSessionKey(7, "claude-opus", session, pinned, pinned*10)

			for turn := 0; turn < 12; turn++ {
				it := NewIteratorWithSession(group, 7, "claude-opus", session, true)
				if got := firstChannel(t, it); got != pinned {
					t.Fatalf("turn %d went to channel %d, want pinned %d", turn, got, pinned)
				}
				if !it.IsStickyChannelKey(pinned, pinned*10) {
					t.Fatalf("turn %d lost the pinned key", turn)
				}
				SetStickyWithSessionKey(7, "claude-opus", session, pinned, pinned*10) // success refresh
			}
		})
	}
}

// Without a session key nothing changes: rotation still spreads over the peers and a
// legacy apiKey+model entry is ignored because no window is configured.
func TestSessionAffinityNoSessionKeyKeepsLegacyRouting(t *testing.T) {
	resetAffinityState(t, true)
	group := planRouteGroup(model.GroupModeSpread, 1, 2, 3)
	SetStickyWithSessionKey(7, "claude-opus", "", 2, 20)

	seen := map[int]bool{}
	for i := 0; i < 12; i++ {
		it := NewIteratorWithSession(group, 7, "claude-opus", "", true)
		seen[firstChannel(t, it)] = true
		if it.IsSticky() {
			t.Fatalf("request without session key must not become sticky")
		}
	}
	if len(seen) < 2 {
		t.Fatalf("round-robin without session key stopped rotating: %v", seen)
	}
	if got := effectiveStickyTTL(0, ""); got != 0 {
		t.Fatalf("no session key ttl = %v, want 0", got)
	}
}

// stickyEnabled=false (route_sticky.go opts a pure-optimisation source out in 轮询) still
// wins over the built-in window.
func TestSessionAffinityHonoursStickyOptOut(t *testing.T) {
	resetAffinityState(t, true)
	group := planRouteGroup(model.GroupModeSpread, 1, 2, 3)
	SetStickyWithSessionKey(7, "claude-opus", "prompt-cache:x", 3, 30)
	seen := map[int]bool{}
	for i := 0; i < 12; i++ {
		it := NewIteratorWithSession(group, 7, "claude-opus", "prompt-cache:x", false)
		seen[firstChannel(t, it)] = true
		if it.IsSticky() {
			t.Fatalf("stickyEnabled=false must never be sticky")
		}
	}
	if len(seen) < 2 {
		t.Fatalf("opted-out source stopped rotating: %v", seen)
	}
}

// The pinned channel tripped (or cooling down) → normal routing; once another channel
// serves the turn the binding moves there.
func TestSessionAffinityFallsBackWhenPinnedChannelUnhealthy(t *testing.T) {
	resetAffinityState(t, true)
	group := planRouteGroup(model.GroupModeFillFirst, 1, 2)
	const session = "codex-session:s1"
	SetStickyWithSessionKey(7, "claude-opus", session, 1, 10)

	for i := 0; i < 200; i++ {
		if tripped, _ := IsTripped(1, 10, "claude-opus"); tripped {
			break
		}
		RecordFailure(1, 10, "claude-opus")
	}
	if tripped, _ := IsTripped(1, 10, "claude-opus"); !tripped {
		t.Fatalf("test setup: breaker did not trip")
	}

	it := NewIteratorWithSession(group, 7, "claude-opus", session, true)
	if it.IsStickyChannel(1) || it.stickyIdx >= 0 {
		t.Fatalf("tripped pinned channel must not be preferred")
	}
	// Relay clears the binding on a counted failure and re-binds on the next success.
	ClearStickyWithSessionKey(7, "claude-opus", session)
	SetStickyWithSessionKey(7, "claude-opus", session, 2, 20)
	it = NewIteratorWithSession(group, 7, "claude-opus", session, true)
	if got := firstChannel(t, it); got != 2 || !it.IsStickyChannelKey(2, 20) {
		t.Fatalf("after re-stick want channel 2 key 20, got %d sticky=%t", got, it.IsSticky())
	}
}

// A binding to a channel the current plan/route does not allow is ignored: the iterator
// only ever reorders the candidates it was given.
func TestSessionAffinityRespectsPlanAllowedChannels(t *testing.T) {
	resetAffinityState(t, true)
	const session = "anthropic-session:plan"
	SetStickyWithSessionKey(7, "claude-opus", session, 99, 990) // bound under another plan

	group := planRouteGroup(model.GroupModeFillFirst, 1, 2)
	it := NewIteratorWithSession(group, 7, "claude-opus", session, true)
	for it.Next() {
		if it.Item().ChannelID == 99 {
			t.Fatalf("channel outside the plan route was offered")
		}
		if it.IsSticky() {
			t.Fatalf("binding to a non-candidate channel must not mark sticky")
		}
	}
	if it.Len() != 2 {
		t.Fatalf("candidates changed: %d", it.Len())
	}
}

// Explicit configuration still wins over the built-in hour: a 1s group window expires the
// binding even though the default would keep it.
func TestSessionAffinityConfiguredWindowWins(t *testing.T) {
	resetAffinityState(t, true)
	const session = "anthropic-session:ttl"
	globalSession.Store(sessionKey(7, "claude-opus", session), &SessionEntry{ChannelID: 2, ChannelKeyID: 20, Timestamp: time.Now().Add(-5 * time.Second)})

	group := planRouteGroup(model.GroupModeFillFirst, 1, 2)
	group.SessionKeepTime = 1
	if it := NewIteratorWithSession(group, 7, "claude-opus", session, true); it.stickyIdx >= 0 {
		t.Fatalf("expired (group window) binding must not stick")
	}

	sessionKeepTimeDefault = func() int { return 120 }
	if got := effectiveStickyTTL(0, session); got != 120*time.Second {
		t.Fatalf("global default ttl = %v, want 120s", got)
	}
	sessionKeepTimeDefault = func() int { return 0 }
	if got := effectiveStickyTTL(0, session); got != DefaultSessionAffinityTTL {
		t.Fatalf("built-in ttl = %v, want %v", got, DefaultSessionAffinityTTL)
	}
}

// session_affinity_enabled=false restores the old "no window = no sticky".
func TestSessionAffinityDisabledSwitch(t *testing.T) {
	resetAffinityState(t, false)
	const session = "anthropic-session:off"
	SetStickyWithSessionKey(7, "claude-opus", session, 2, 20)
	group := planRouteGroup(model.GroupModeFillFirst, 1, 2)
	it := NewIteratorWithSession(group, 7, "claude-opus", session, true)
	if got := firstChannel(t, it); got != 1 || it.IsSticky() {
		t.Fatalf("disabled affinity must keep priority order, got %d sticky=%t", got, it.IsSticky())
	}
}

func TestSessionStoreStaysBounded(t *testing.T) {
	resetAffinityState(t, true)
	maxSessionEntries = 100
	for i := 0; i < 1000; i++ {
		SetStickyWithSessionKey(7, "m", fmt.Sprintf("s:%d", i), 1, 1)
	}
	n := 0
	globalSession.Range(func(_, _ any) bool { n++; return true })
	if n > 100 || sessionEntryCount.Load() != int64(n) {
		t.Fatalf("store size %d (counter %d), want <= 100 and counter in sync", n, sessionEntryCount.Load())
	}
	// Refreshing an existing session does not grow the counter.
	before := sessionEntryCount.Load()
	globalSession.Range(func(k, _ any) bool {
		globalSession.Store(k, &SessionEntry{ChannelID: 1, Timestamp: time.Now()})
		return false
	})
	SetStickyWithSessionKey(7, "m", "s:999", 1, 1)
	if sessionEntryCount.Load() > before+1 {
		t.Fatalf("refresh grew the counter: %d -> %d", before, sessionEntryCount.Load())
	}
}

func TestSessionStoreConcurrentAccess(t *testing.T) {
	resetAffinityState(t, true)
	maxSessionEntries = 500
	group := planRouteGroup(model.GroupModeSpread, 1, 2, 3)
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				key := fmt.Sprintf("s:%d:%d", w, i%50)
				it := NewIteratorWithSession(group, 7, "m", key, true)
				if it.Next() {
					SetStickyWithSessionKey(7, "m", key, it.Item().ChannelID, 1)
				}
				if i%7 == 0 {
					ClearStickyWithSessionKey(7, "m", key)
				}
			}
		}(w)
	}
	wg.Wait()
	n := int64(0)
	globalSession.Range(func(_, _ any) bool { n++; return true })
	if n != sessionEntryCount.Load() {
		t.Fatalf("counter drifted: map=%d counter=%d", n, sessionEntryCount.Load())
	}
}
