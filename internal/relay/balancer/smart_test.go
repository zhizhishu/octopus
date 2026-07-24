package balancer

import (
	"net/http"
	"sync"
	"testing"

	"github.com/bestruirui/octopus/internal/model"
)

func TestIteratorDoesNotForceTrippedStickyKey(t *testing.T) {
	globalSession = sync.Map{}
	globalBreaker = sync.Map{}
	ResetRuntimeTelemetry()

	group := model.Group{
		Mode:            model.GroupModeFailover,
		SessionKeepTime: 300,
		Items: []model.GroupItem{
			{ChannelID: 1, ModelName: "gpt-5.5", Priority: 1, Weight: 1},
			{ChannelID: 2, ModelName: "gpt-5.5", Priority: 2, Weight: 1},
		},
	}
	SetStickyWithSessionKey(9, "gpt-5.5", "session:tripped", 2, 22)
	for i := 0; i < 10; i++ {
		RecordFailureWithStatus(2, 22, "gpt-5.5", http.StatusServiceUnavailable)
	}

	iter := NewIteratorWithSessionKey(group, 9, "gpt-5.5", "session:tripped")
	if !iter.Next() {
		t.Fatalf("expected first candidate")
	}
	if got := iter.Item().ChannelID; got != 1 {
		t.Fatalf("tripped sticky channel forced to front: got %d want 1", got)
	}
	if iter.IsSticky() {
		t.Fatalf("tripped sticky candidate should not be marked sticky")
	}
}

func TestPrioritizeChannelKeysByHealthAvoidsTrippedPreferredKey(t *testing.T) {
	globalBreaker = sync.Map{}
	ResetRuntimeTelemetry()

	keys := []model.ChannelKey{
		{ID: 11, ChannelID: 1, Enabled: true},
		{ID: 12, ChannelID: 1, Enabled: true},
	}
	for i := 0; i < 10; i++ {
		RecordFailureWithStatus(1, 11, "gpt", http.StatusTooManyRequests)
	}

	got := PrioritizeChannelKeysByHealth(keys, 1, "gpt", 11)
	if len(got) != 2 || got[0].ID != 12 {
		t.Fatalf("expected healthy key before tripped preferred key, got %#v", got)
	}
}
