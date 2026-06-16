package balancer

import (
	"sync"
	"testing"
	"time"

	"github.com/bestruirui/octopus/internal/model"
)

func TestStickyWithSessionKeySeparatesClientSessions(t *testing.T) {
	globalSession = sync.Map{}

	group := model.Group{
		SessionKeepTime: 300,
		Items: []model.GroupItem{
			{ChannelID: 1, ModelName: "gpt-5.5", Priority: 1, Weight: 1},
			{ChannelID: 2, ModelName: "gpt-5.5", Priority: 2, Weight: 1},
		},
	}

	SetStickyWithSessionKey(9, "gpt-5.5", "session:a", 2, 22)
	SetStickyWithSessionKey(9, "gpt-5.5", "session:b", 1, 11)

	iterA := NewIteratorWithSessionKey(group, 9, "gpt-5.5", "session:a")
	if !iterA.Next() {
		t.Fatalf("expected first candidate for session a")
	}
	if got := iterA.Item().ChannelID; got != 2 {
		t.Fatalf("session a sticky channel = %d, want 2", got)
	}
	if !iterA.IsStickyChannelKey(2, 22) {
		t.Fatalf("session a sticky key was not preserved")
	}

	iterB := NewIteratorWithSessionKey(group, 9, "gpt-5.5", "session:b")
	if !iterB.Next() {
		t.Fatalf("expected first candidate for session b")
	}
	if got := iterB.Item().ChannelID; got != 1 {
		t.Fatalf("session b sticky channel = %d, want 1", got)
	}
	if !iterB.IsStickyChannelKey(1, 11) {
		t.Fatalf("session b sticky key was not preserved")
	}
}

func TestStickyKeyIDForCurrentChannel(t *testing.T) {
	globalSession = sync.Map{}

	group := model.Group{
		SessionKeepTime: 300,
		Items: []model.GroupItem{
			{ChannelID: 1, ModelName: "gpt-5.5", Priority: 1, Weight: 1},
			{ChannelID: 2, ModelName: "gpt-5.5", Priority: 2, Weight: 1},
		},
	}
	SetStickyWithSessionKey(9, "gpt-5.5", "session:sticky-key", 2, 22)

	iter := NewIteratorWithSessionKey(group, 9, "gpt-5.5", "session:sticky-key")
	if !iter.Next() {
		t.Fatalf("expected first candidate")
	}
	if got := iter.StickyKeyIDForCurrentChannel(2); got != 22 {
		t.Fatalf("sticky key id = %d, want 22", got)
	}
	if got := iter.StickyKeyIDForCurrentChannel(1); got != 0 {
		t.Fatalf("non-current sticky key id = %d, want 0", got)
	}
}

func TestClearStickyWithSessionKey(t *testing.T) {
	globalSession = sync.Map{}

	SetStickyWithSessionKey(9, "gpt-5.5", "session:clear", 2, 22)
	ClearStickyWithSessionKey(9, "gpt-5.5", "session:clear")

	if got := GetStickyWithSessionKey(9, "gpt-5.5", "session:clear", time.Minute); got != nil {
		t.Fatalf("expected sticky entry to be cleared, got %#v", got)
	}
}

func TestStickyWithoutSessionKeyKeepsLegacyBehavior(t *testing.T) {
	globalSession = sync.Map{}

	SetSticky(3, "glm-5", 8, 88)
	got := GetStickyWithSessionKey(3, "glm-5", "", time.Minute)
	if got == nil || got.ChannelID != 8 || got.ChannelKeyID != 88 {
		t.Fatalf("legacy sticky = %#v, want channel 8 key 88", got)
	}
}
