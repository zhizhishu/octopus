package balancer

import (
	"sync"
	"testing"
	"time"

	"github.com/bestruirui/octopus/internal/model"
)

// TestGlobalSessionKeepTimeDefaultEnablesSticky pins that a group with its own
// SessionKeepTime unset (0) still gets sticky routing when the global
// session_keep_time_default is configured (>0), so an admin can enable sticky
// fleet-wide without editing every group.
func TestGlobalSessionKeepTimeDefaultEnablesSticky(t *testing.T) {
	globalSession = sync.Map{}
	prev := sessionKeepTimeDefault
	defer func() { sessionKeepTimeDefault = prev }()
	sessionKeepTimeDefault = func() int { return 300 }

	group := model.Group{
		SessionKeepTime: 0, // group-level unset: must fall back to the global default
		Items: []model.GroupItem{
			{ChannelID: 1, ModelName: "gpt-5.5", Priority: 1, Weight: 1},
			{ChannelID: 2, ModelName: "gpt-5.5", Priority: 2, Weight: 1},
		},
	}
	SetStickyWithSessionKey(9, "gpt-5.5", "session:global", 2, 22)

	iter := NewIteratorWithSessionKey(group, 9, "gpt-5.5", "session:global")
	if !iter.Next() {
		t.Fatalf("expected first candidate")
	}
	if got := iter.Item().ChannelID; got != 2 {
		t.Fatalf("global-default sticky channel = %d, want 2", got)
	}
	if !iter.IsStickyChannelKey(2, 22) {
		t.Fatalf("global-default sticky key was not preserved")
	}
}

// TestZeroSessionKeepTimeDefaultKeepsLegacyBehavior pins backward compatibility:
// with both the group-level SessionKeepTime and the global default at 0, sticky
// stays off and the balancer ignores any recorded sticky entry (channel 1 wins on
// priority, not the recorded channel 2).
func TestZeroSessionKeepTimeDefaultKeepsLegacyBehavior(t *testing.T) {
	globalSession = sync.Map{}
	prev := sessionKeepTimeDefault
	defer func() { sessionKeepTimeDefault = prev }()
	sessionKeepTimeDefault = func() int { return 0 }

	group := model.Group{
		SessionKeepTime: 0,
		Items: []model.GroupItem{
			// channel 1 holds the top ChannelPriority so the non-sticky fallback
			// deterministically leads with it: spread now buckets/rotates by
			// ChannelPriority, not the per-item drag order.
			{ChannelID: 1, ModelName: "gpt-5.5", Priority: 1, Weight: 1, ChannelPriority: 0},
			{ChannelID: 2, ModelName: "gpt-5.5", Priority: 2, Weight: 1, ChannelPriority: 1},
		},
	}
	SetStickyWithSessionKey(9, "gpt-5.5", "session:legacy", 2, 22)

	iter := NewIteratorWithSessionKey(group, 9, "gpt-5.5", "session:legacy")
	if !iter.Next() {
		t.Fatalf("expected first candidate")
	}
	if iter.IsSticky() {
		t.Fatalf("sticky must stay off when both group and global default are 0")
	}
	if got := iter.Item().ChannelID; got != 1 {
		t.Fatalf("legacy non-sticky first candidate = %d, want 1 (priority order, not recorded sticky)", got)
	}
}

// TestGroupSessionKeepTimeOverridesGlobalDefault pins precedence: an explicit
// group-level SessionKeepTime is used as-is and the global default is not consulted.
// A tiny group TTL with an already-expired entry means sticky must NOT reactivate
// even though the (larger) global default would still consider it fresh.
func TestGroupSessionKeepTimeOverridesGlobalDefault(t *testing.T) {
	globalSession = sync.Map{}
	prev := sessionKeepTimeDefault
	defer func() { sessionKeepTimeDefault = prev }()
	// If the global default were (wrongly) consulted, this large window would keep
	// the entry sticky; the group's own 1s window must win and expire it.
	sessionKeepTimeDefault = func() int { return 3600 }

	group := model.Group{
		SessionKeepTime: 1, // explicit group window: takes precedence over the global default
		Items: []model.GroupItem{
			// channel 1 holds the top ChannelPriority so the post-expiry fallback
			// deterministically leads with it (spread buckets by ChannelPriority).
			{ChannelID: 1, ModelName: "gpt-5.5", Priority: 1, Weight: 1, ChannelPriority: 0},
			{ChannelID: 2, ModelName: "gpt-5.5", Priority: 2, Weight: 1, ChannelPriority: 1},
		},
	}
	// Backdate the sticky entry beyond the 1s group window but well within 3600s.
	globalSession.Store(sessionKey(9, "gpt-5.5", "session:override"), &SessionEntry{
		ChannelID:    2,
		ChannelKeyID: 22,
		Timestamp:    time.Now().Add(-5 * time.Second),
	})

	iter := NewIteratorWithSessionKey(group, 9, "gpt-5.5", "session:override")
	if !iter.Next() {
		t.Fatalf("expected first candidate")
	}
	if iter.IsSticky() {
		t.Fatalf("expired group-window entry must not be sticky (global default must not override group TTL)")
	}
	if got := iter.Item().ChannelID; got != 1 {
		t.Fatalf("first candidate = %d, want 1 (priority order after group-window expiry)", got)
	}
}
