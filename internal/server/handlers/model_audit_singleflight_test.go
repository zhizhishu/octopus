package handlers

import (
	"fmt"
	"sync"
	"testing"
)

// The audit endpoint is the ONLY place in the product allowed to spend upstream tokens
// per user click, and every probe it sends is a real billable request. These tests pin
// the server-side single-flight gate that makes "repeat clicks must not double-charge"
// true even when the caller is not the UI (second admin, second tab, direct API call).
//
// These tests touch a package-level sync.Map, so every case MUST use its own key and
// release through t.Cleanup: a leaked entry would make an unrelated test fail (or pass
// for the wrong reason) when the whole package runs in one process.

// newAuditTestKey returns a key unique to this test, derived from the test name so
// concurrent or sequential tests can never observe each other's slots and no shared
// mutable counter (which would itself be a data race under -race) is needed.
func newAuditTestKey(t *testing.T) (int, string) {
	t.Helper()
	name := t.Name()
	// A stable hash of the test name keeps ids spread out; the name is unique per test,
	// which is all the isolation we need.
	h := 0
	for _, r := range name {
		h = (h*31 + int(r)) % 100000
	}
	if h < 0 {
		h = -h
	}
	return 1 + h, name
}

func TestModelAuditSlotBlocksSecondConcurrentRun(t *testing.T) {
	ch, model := newAuditTestKey(t)
	release, ok := acquireModelAuditSlot(ch, model)
	if !ok {
		t.Fatalf("first acquire must succeed on a free slot")
	}
	t.Cleanup(release)

	if _, ok2 := acquireModelAuditSlot(ch, model); ok2 {
		t.Fatalf("a second concurrent acquire for the same (channel, model) must be refused: " +
			"otherwise the same audit runs twice and bills twice")
	}
}

func TestModelAuditSlotIsReusableAfterRelease(t *testing.T) {
	ch, model := newAuditTestKey(t)
	release, ok := acquireModelAuditSlot(ch, model)
	if !ok {
		t.Fatalf("first acquire must succeed")
	}
	release()
	release2, ok2 := acquireModelAuditSlot(ch, model)
	if !ok2 {
		t.Fatalf("the slot must be acquirable again after release")
	}
	t.Cleanup(release2)
}

// TestModelAuditSlotIsScopedPerChannelAndModel proves the lock is per (channel, model)
// pair rather than global, so auditing one model does not freeze the whole page.
func TestModelAuditSlotIsScopedPerChannelAndModel(t *testing.T) {
	chA, modelA := newAuditTestKey(t)
	chB := chA + 5000       // same model, different channel
	modelB := modelA + "-b" // same channel, different model

	relA, okA := acquireModelAuditSlot(chA, modelA)
	if !okA {
		t.Fatalf("first acquire must succeed")
	}
	t.Cleanup(relA)

	// Different model, same channel: must NOT be blocked (a global lock would make the
	// detection page single-model for no reason).
	relB, okB := acquireModelAuditSlot(chA, modelB)
	if !okB {
		t.Fatalf("a different model on the same channel must be auditable concurrently")
	}
	t.Cleanup(relB)

	// Same model, different channel: must NOT be blocked either.
	relC, okC := acquireModelAuditSlot(chB, modelA)
	if !okC {
		t.Fatalf("the same model on a different channel must be auditable concurrently")
	}
	t.Cleanup(relC)

	// Same pair: blocked, proving the key is the pair and not just the model.
	if _, ok := acquireModelAuditSlot(chA, modelA); ok {
		t.Fatalf("the same (channel, model) pair must still be blocked")
	}
}

// TestModelAuditSlotKeysDoNotCollide guards the NUL separator: without it,
// channel=1/model="2x" and channel=12/model="x" could build the same key and block
// each other (a false positive that would look like a hung audit).
func TestModelAuditSlotKeysDoNotCollide(t *testing.T) {
	if modelAuditLockKey(1, "2x") == modelAuditLockKey(12, "x") {
		t.Fatalf("lock keys must not collide across channel/model boundaries")
	}
	if modelAuditLockKey(1, "a\x00b") == modelAuditLockKey(1, "a") {
		t.Fatalf("a model name containing the separator must not alias another key")
	}
}

// TestModelAuditSlotReleaseIsIdempotentAndSafe proves the release func is harmless when
// called twice and, critically, that a REFUSED acquire cannot free a slot its owner
// still holds — that would silently reopen the double-billing hole.
func TestModelAuditSlotReleaseIsIdempotentAndSafe(t *testing.T) {
	ch, model := newAuditTestKey(t)

	held, acquired := acquireModelAuditSlot(ch, model)
	if !acquired {
		t.Fatalf("a fresh key must acquire")
	}
	defer held()

	refusedRelease, refusedOK := acquireModelAuditSlot(ch, model)
	if refusedOK {
		t.Fatalf("the second acquire must be refused while the slot is held")
	}
	refusedRelease() // must be a no-op
	refusedRelease() // and must stay safe when called twice

	// The owner's slot must still be occupied.
	if _, got := acquireModelAuditSlot(ch, model); got {
		t.Fatalf("a refused acquire's release must not free the slot held by another caller")
	}
}

// TestModelAuditSlotConcurrentAcquireYieldsExactlyOneWinner exercises the way the bug
// actually happens: many callers at once. At no instant may two hold the slot.
func TestModelAuditSlotConcurrentAcquireYieldsExactlyOneWinner(t *testing.T) {
	ch, model := newAuditTestKey(t)
	const callers = 32

	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	overlap := 0
	currentlyHolding := 0

	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, ok := acquireModelAuditSlot(ch, model)
			if !ok {
				return
			}
			mu.Lock()
			wins++
			currentlyHolding++
			if currentlyHolding > 1 {
				overlap++
			}
			mu.Unlock()

			release()

			mu.Lock()
			currentlyHolding--
			mu.Unlock()
		}()
	}
	wg.Wait()

	if overlap != 0 {
		t.Fatalf("%d overlapping holders observed: the slot must never be held by two callers at once", overlap)
	}
	if wins == 0 {
		t.Fatalf("at least one caller must win; a gate that refuses everyone is broken")
	}
	// And the slot must be free at the end.
	final, ok := acquireModelAuditSlot(ch, model)
	if !ok {
		t.Fatalf("the slot must be free after all callers returned")
	}
	t.Cleanup(final)
}

// TestModelAuditSlotSerializesHolders asserts mutual exclusion directly: while one
// holder is inside, every concurrent acquire must be refused.
func TestModelAuditSlotSerializesHolders(t *testing.T) {
	ch, model := newAuditTestKey(t)

	release, ok := acquireModelAuditSlot(ch, model)
	if !ok {
		t.Fatalf("initial acquire must succeed")
	}

	const callers = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	refused := 0
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r, got := acquireModelAuditSlot(ch, model); got {
				r() // should not happen; counted as a leak below
				mu.Lock()
				refused-- // signal the anomaly
				mu.Unlock()
				return
			}
			mu.Lock()
			refused++
			mu.Unlock()
		}()
	}
	wg.Wait()
	release()

	if refused != callers {
		t.Fatalf("all %d callers should have been refused while the slot was held, got %d", callers, refused)
	}
}

// keep fmt referenced if a future edit trims the file; cheap and avoids churn.
var _ = fmt.Sprintf
