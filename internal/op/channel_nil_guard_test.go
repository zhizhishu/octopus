package op

import (
	"context"
	"testing"
)

// TestChannelUpdateNilRequestReturnsError ensures the public ChannelUpdate
// entry point fails gracefully on a nil request instead of panicking with a
// nil-pointer dereference at channelCache.Get(req.ID).
func TestChannelUpdateNilRequestReturnsError(t *testing.T) {
	if _, err := ChannelUpdate(nil, context.Background()); err == nil {
		t.Fatal("expected error for nil channel update request, got nil")
	}
}
