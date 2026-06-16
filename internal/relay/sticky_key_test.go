package relay

import (
	"testing"

	dbmodel "github.com/bestruirui/octopus/internal/model"
)

func TestPrioritizeAvailableChannelKeyMovesStickyKeyFirst(t *testing.T) {
	keys := []dbmodel.ChannelKey{
		{ID: 1, ChannelKey: "first"},
		{ID: 22, ChannelKey: "sticky"},
		{ID: 3, ChannelKey: "third"},
	}

	got := prioritizeAvailableChannelKey(keys, 22)
	if len(got) != 3 {
		t.Fatalf("expected 3 keys, got %#v", got)
	}
	if got[0].ID != 22 || got[1].ID != 1 || got[2].ID != 3 {
		t.Fatalf("unexpected key order: %#v", got)
	}
	if keys[0].ID != 1 {
		t.Fatalf("original key order should not be mutated, got %#v", keys)
	}
}
