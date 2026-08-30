package intervention

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
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

	ctx := context.Background()
	_, err = Wait(ctx, id1, 20*time.Millisecond)
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
	_, err = Wait(ctxCancel, id2, 1*time.Second)
	if err != ErrCanceled {
		t.Fatalf("expected ErrCanceled, got: %v", err)
	}
	// After wait, id2 must be removed
	if _, ok := Get(id2); ok {
		t.Fatalf("entry should be removed after canceled Wait")
	}
}
