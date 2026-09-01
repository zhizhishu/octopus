package relay

import (
	"context"
	"testing"
)

func resetRequestStateForTest() {
	stateMu.Lock()
	defer stateMu.Unlock()
	stateRequests = make(map[uint64]*RequestState)
	stateWatchers = make(map[chan *RequestState]struct{})
	finishedQueue = nil
	stateIDSeq.Store(0)
}

func TestRequestStateSnapshotFiltersNormalUsers(t *testing.T) {
	resetRequestStateForTest()
	t.Cleanup(resetRequestStateForTest)

	first := newRequestState("model-a", "responses", 11, 101)
	second := newRequestState("model-b", "messages", 22, 202)

	userSnapshot := GetRequestStateSnapshotForUser(11, false)
	if len(userSnapshot) != 1 || userSnapshot[0].ID != first.ID {
		t.Fatalf("normal user snapshot = %+v, want only request %d", userSnapshot, first.ID)
	}
	adminSnapshot := GetRequestStateSnapshotForUser(0, true)
	if len(adminSnapshot) != 2 {
		t.Fatalf("admin snapshot length = %d, want 2 (second id %d)", len(adminSnapshot), second.ID)
	}
}

func TestRequestStatePublishesRunningRoundImmediately(t *testing.T) {
	resetRequestStateForTest()
	t.Cleanup(resetRequestStateForTest)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates := SubscribeRequestState(ctx)
	defer UnsubscribeRequestState(updates)

	state := newRequestState("model-a", "responses", 11, 101)
	initial := <-updates
	if initial.ID != state.ID || initial.Status != "running" {
		t.Fatalf("initial update = %+v, want running request %d", initial, state.ID)
	}
	state.startRound("channel-1", "mapped-model")
	round := <-updates
	if !round.Sending || round.Round != 1 || round.TargetChannel != "channel-1" {
		t.Fatalf("round update = %+v, want channel-1 sending round 1", round)
	}
}
