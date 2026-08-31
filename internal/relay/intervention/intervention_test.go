package intervention

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/bestruirui/octopus/internal/model"
)

func resetRegistryForTest() {
	registry.Lock()
	registry.pending = make(map[string]*Pending)
	registry.Unlock()
}

func TestRegistryCapacityAtomic(t *testing.T) {
	resetRegistryForTest()
	defer resetRegistryForTest()

	registeredIDs := make([]string, 0, maxPending)
	for i := 0; i < maxPending; i++ {
		id, err := Register(&Pending{
			RequestModel: fmt.Sprintf("model-%d", i),
		})
		if err != nil {
			t.Fatalf("expected Register to succeed under capacity, got err: %v", err)
		}
		if id == "" {
			t.Fatalf("expected non-empty ID, got empty")
		}
		registeredIDs = append(registeredIDs, id)
	}

	// 65th registration must fail with ErrCapacity
	extraID, err := Register(&Pending{RequestModel: "overflow"})
	if err != ErrCapacity {
		t.Fatalf("expected ErrCapacity on registry full, got err: %v, id: %s", err, extraID)
	}
	if extraID != "" {
		t.Fatalf("expected empty id on capacity error, got %s", extraID)
	}

	// Release one entry, now registration must succeed again
	Cancel(registeredIDs[0])
	newID, err := Register(&Pending{RequestModel: "back-in-capacity"})
	if err != nil {
		t.Fatalf("expected Register to succeed after freeing a slot, got err: %v", err)
	}
	if newID == "" {
		t.Fatalf("expected non-empty ID after freeing a slot, got empty")
	}
}

func TestRegistryConcurrentCapacity(t *testing.T) {
	resetRegistryForTest()
	defer resetRegistryForTest()

	const totalAttempts = 120
	var wg sync.WaitGroup
	var successCount, capacityErrCount int
	var mu sync.Mutex

	for i := 0; i < totalAttempts; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, err := Register(&Pending{
				RequestModel: fmt.Sprintf("concurrent-%d", idx),
			})
			mu.Lock()
			if err == nil {
				successCount++
			} else if err == ErrCapacity {
				capacityErrCount++
			}
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	if successCount != maxPending {
		t.Fatalf("expected exactly %d successful registrations, got %d", maxPending, successCount)
	}
	if capacityErrCount != (totalAttempts - maxPending) {
		t.Fatalf("expected %d capacity errors, got %d", totalAttempts-maxPending, capacityErrCount)
	}
}

func TestRegistryListSortedByCreatedAt(t *testing.T) {
	resetRegistryForTest()
	defer resetRegistryForTest()

	baseTime := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	p1 := &Pending{ID: "iv-3", CreatedAt: baseTime.Add(3 * time.Second), RequestModel: "m3"}
	p2 := &Pending{ID: "iv-1", CreatedAt: baseTime.Add(1 * time.Second), RequestModel: "m1"}
	p3 := &Pending{ID: "iv-2", CreatedAt: baseTime.Add(2 * time.Second), RequestModel: "m2"}
	p4 := &Pending{ID: "iv-0-b", CreatedAt: baseTime, RequestModel: "m0b"}
	p5 := &Pending{ID: "iv-0-a", CreatedAt: baseTime, RequestModel: "m0a"}

	for _, p := range []*Pending{p1, p2, p3, p4, p5} {
		if _, err := Register(p); err != nil {
			t.Fatalf("register failed: %v", err)
		}
	}

	list := List()
	if len(list) != 5 {
		t.Fatalf("expected 5 items, got %d", len(list))
	}

	wantOrder := []string{"iv-0-a", "iv-0-b", "iv-1", "iv-2", "iv-3"}
	for i, want := range wantOrder {
		if list[i].ID != want {
			t.Errorf("list[%d].ID = %s; want %s", i, list[i].ID, want)
		}
	}
}

func TestRegistryDuplicateResolve(t *testing.T) {
	resetRegistryForTest()
	defer resetRegistryForTest()

	id, err := Register(&Pending{RequestModel: "test-model"})
	if err != nil {
		t.Fatalf("register failed: %v", err)
	}

	r1 := Resolution{Action: ActionRetryChannel, ChannelID: 1, KeyID: 10}
	if err := Resolve(id, r1); err != nil {
		t.Fatalf("first Resolve failed: %v", err)
	}

	r2 := Resolution{Action: ActionAbort}
	if err := Resolve(id, r2); err != ErrNotFound {
		t.Fatalf("expected second Resolve to return ErrNotFound, got: %v", err)
	}
}

func TestRegistryWaitTimeoutAndCancel(t *testing.T) {
	resetRegistryForTest()
	defer resetRegistryForTest()

	// 1. Timeout case
	id1, err := Register(&Pending{RequestModel: "test-timeout"})
	if err != nil {
		t.Fatalf("register failed: %v", err)
	}

	ctxTimeout, cancelTimeout := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelTimeout()
	_, err = Wait(ctxTimeout, id1)
	if err != ErrTimeout {
		t.Fatalf("expected ErrTimeout, got: %v", err)
	}
	// After wait, id1 must be removed
	if _, ok := Get(id1); ok {
		t.Fatalf("entry should be removed after timeout Wait")
	}

	// 2. Cancellation case
	id2, err := Register(&Pending{RequestModel: "test-cancel"})
	if err != nil {
		t.Fatalf("register failed: %v", err)
	}

	ctxCancel, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately
	_, err = Wait(ctxCancel, id2)
	if err != ErrCanceled {
		t.Fatalf("expected ErrCanceled, got: %v", err)
	}
	// After wait, id2 must be removed
	if _, ok := Get(id2); ok {
		t.Fatalf("entry should be removed after canceled Wait")
	}
}

func TestBackoffDuration(t *testing.T) {
	tests := []struct {
		round int
		want  time.Duration
	}{
		{round: 0, want: 1 * time.Second},
		{round: 1, want: 1 * time.Second},
		{round: 2, want: 2 * time.Second},
		{round: 3, want: 4 * time.Second},
		{round: 4, want: 8 * time.Second},
		{round: 5, want: 15 * time.Second},
		{round: 6, want: 15 * time.Second},
		{round: 10, want: 15 * time.Second},
	}

	for _, tt := range tests {
		got := BackoffDuration(tt.round)
		if got != tt.want {
			t.Errorf("BackoffDuration(%d) = %v, want %v", tt.round, got, tt.want)
		}
	}
}

func TestStatusAndAttemptsUpdates(t *testing.T) {
	resetRegistryForTest()
	defer resetRegistryForTest()

	id, err := Register(&Pending{
		RequestModel: "test-model",
		Attempts: []model.ChannelAttempt{
			{ChannelID: 1, Status: model.AttemptFailed},
		},
	})
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	snap, ok := Get(id)
	if !ok || snap.Status != StatusAutoRetrying || snap.RescueRound != 0 {
		t.Fatalf("unexpected initial snapshot: %#v", snap)
	}

	nextTime := time.Now().Add(2 * time.Second)
	if err := UpdateStatus(id, StatusAutoRetrying, 2, &nextTime); err != nil {
		t.Fatalf("UpdateStatus failed: %v", err)
	}

	newAttempts := []model.ChannelAttempt{
		{ChannelID: 1, Status: model.AttemptFailed},
		{ChannelID: 2, Status: model.AttemptFailed},
	}
	if err := UpdateAttempts(id, newAttempts, "429 too many requests"); err != nil {
		t.Fatalf("UpdateAttempts failed: %v", err)
	}

	snap2, ok := Get(id)
	if !ok {
		t.Fatalf("Get failed")
	}
	if snap2.Status != StatusAutoRetrying || snap2.RescueRound != 2 {
		t.Fatalf("status update not reflected: %#v", snap2)
	}
	if len(snap2.Attempts) != 2 || snap2.LastError != "429 too many requests" {
		t.Fatalf("attempts update not reflected: %#v", snap2)
	}
	if snap2.NextRetryAt == nil || !snap2.NextRetryAt.Equal(nextTime) {
		t.Fatalf("next retry time mismatch: %v vs %v", snap2.NextRetryAt, nextTime)
	}
}

func TestWaitRoundPreemption(t *testing.T) {
	resetRegistryForTest()
	defer resetRegistryForTest()

	id, err := Register(&Pending{
		RequestModel: "test-model",
	})
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Preemption test
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = Resolve(id, Resolution{
			Action:    ActionRetryChannel,
			ChannelID: 99,
			KeyID:     88,
		})
	}()

	res, isPreempted, waitErr := WaitRound(ctx, id, 500*time.Millisecond)
	if waitErr != nil {
		t.Fatalf("WaitRound error: %v", waitErr)
	}
	if !isPreempted {
		t.Fatalf("expected preemption by operator")
	}
	if res.ChannelID != 99 || res.KeyID != 88 {
		t.Fatalf("unexpected resolution: %#v", res)
	}
}

func TestWaitRoundTimeoutClosedContext(t *testing.T) {
	resetRegistryForTest()
	defer resetRegistryForTest()

	id, err := Register(&Pending{
		RequestModel: "test-model",
	})
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	time.Sleep(20 * time.Millisecond)

	// First wait receives ErrTimeout
	_, _, waitErr := WaitRound(ctx, id, 500*time.Millisecond)
	if !errors.Is(waitErr, ErrTimeout) {
		t.Fatalf("expected ErrTimeout, got %v", waitErr)
	}

	// Subsequent wait on same context also receives ErrTimeout immediately
	_, waitErr2 := WaitOperator(ctx, id)
	if !errors.Is(waitErr2, ErrTimeout) {
		t.Fatalf("expected ErrTimeout on subsequent wait, got %v", waitErr2)
	}
}

func TestInterventionMultiRoundSamePendingLifecycle(t *testing.T) {
	resetRegistryForTest()
	defer resetRegistryForTest()

	// Simulate coordinator lifecycle:
	// 1. Initial registration
	id, err := Register(&Pending{
		RequestModel: "claude-3-7-sonnet",
		Endpoint:     "/v1/messages",
		Attempts:     []model.ChannelAttempt{{ChannelID: 1, Status: model.AttemptFailed}},
		LastError:    "502 Bad Gateway",
		Status:       StatusAutoRetrying,
		RescueRound:  0,
	})
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	if len(List()) != 1 {
		t.Fatalf("expected exactly 1 item in registry, got %d", len(List()))
	}

	// 2. Multi-round re-entries without creating new pending items
	for round := 1; round <= 3; round++ {
		nextRetry := time.Now().Add(BackoffDuration(round))
		if err := UpdateStatus(id, StatusAutoRetrying, round, &nextRetry); err != nil {
			t.Fatalf("UpdateStatus failed on round %d: %v", round, err)
		}

		newAttempts := []model.ChannelAttempt{
			{ChannelID: 1, Status: model.AttemptFailed},
			{ChannelID: round + 1, Status: model.AttemptFailed},
		}
		if err := UpdateAttempts(id, newAttempts, fmt.Sprintf("503 round %d", round)); err != nil {
			t.Fatalf("UpdateAttempts failed on round %d: %v", round, err)
		}

		snap, ok := Get(id)
		if !ok {
			t.Fatalf("Get failed on round %d", round)
		}
		if snap.RescueRound != round {
			t.Fatalf("expected RescueRound=%d, got %d", round, snap.RescueRound)
		}
		if snap.LastError != fmt.Sprintf("503 round %d", round) {
			t.Fatalf("expected LastError update, got %s", snap.LastError)
		}
	}

	// Ensure still only 1 item in registry across all rounds
	if len(List()) != 1 {
		t.Fatalf("registry bloated: expected 1 item, got %d", len(List()))
	}

	// 3. Resolve targets the active waiter
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	waitDone := make(chan struct{})
	var resolvedRes Resolution
	var waitErr error

	go func() {
		defer close(waitDone)
		resolvedRes, waitErr = WaitOperator(ctx, id)
	}()

	time.Sleep(20 * time.Millisecond)
	err = Resolve(id, Resolution{
		Action:    ActionRetryChannel,
		ChannelID: 42,
		KeyID:     77,
		ModelName: "claude-3-7-sonnet",
	})
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}

	<-waitDone
	if waitErr != nil {
		t.Fatalf("WaitOperator failed: %v", waitErr)
	}
	if resolvedRes.ChannelID != 42 || resolvedRes.KeyID != 77 {
		t.Fatalf("unexpected resolution delivered: %#v", resolvedRes)
	}

	// 4. Cancel cleans up exactly once when request completes
	Cancel(id)
	if len(List()) != 0 {
		t.Fatalf("expected registry to be empty after Cancel, got %d", len(List()))
	}
	if _, ok := Get(id); ok {
		t.Fatalf("expected pending to be removed after Cancel")
	}
}
