package grouplimit

import "testing"

func TestAcquireConcurrencyCap(t *testing.T) {
	ResetForTest()
	defer ResetForTest()

	const gid = 1
	r1, ok, _ := Acquire(gid, 2, 0)
	if !ok {
		t.Fatal("first acquire should pass under cap 2")
	}
	r2, ok, _ := Acquire(gid, 2, 0)
	if !ok {
		t.Fatal("second acquire should pass under cap 2")
	}
	if _, ok, reason := Acquire(gid, 2, 0); ok {
		t.Fatal("third acquire should be rejected at cap 2")
	} else if reason == "" {
		t.Fatal("rejection should carry a human-readable reason")
	}

	r1() // free one slot
	r3, ok, _ := Acquire(gid, 2, 0)
	if !ok {
		t.Fatal("acquire should pass after a release frees a slot")
	}
	r2()
	r3()
}

func TestAcquireRPMCap(t *testing.T) {
	ResetForTest()
	defer ResetForTest()

	const gid = 2
	for i := 0; i < 3; i++ {
		rel, ok, _ := Acquire(gid, 0, 3)
		if !ok {
			t.Fatalf("rpm acquire %d should pass under limit 3", i+1)
		}
		rel() // releasing must NOT refund the RPM window
	}
	if _, ok, _ := Acquire(gid, 0, 3); ok {
		t.Fatal("4th request within the same minute should be rejected by rpm cap 3")
	}
}

func TestAcquireNoCapsAlwaysPasses(t *testing.T) {
	ResetForTest()
	defer ResetForTest()

	rel, ok, _ := Acquire(99, 0, 0)
	if !ok {
		t.Fatal("a group with no caps configured should always be admitted")
	}
	rel()
}

func TestRejectedRequestDoesNotConsumeRPM(t *testing.T) {
	ResetForTest()
	defer ResetForTest()

	const gid = 3
	// Concurrency=1, RPM=5. Hold the single concurrency slot so the next request is
	// rejected on concurrency (RPM still has budget) and must not burn an RPM slot.
	r1, ok, _ := Acquire(gid, 1, 5)
	if !ok {
		t.Fatal("first acquire should pass")
	}
	if _, ok, _ := Acquire(gid, 1, 5); ok {
		t.Fatal("second acquire should be rejected on the concurrency cap")
	}
	r1()

	// Only the one admitted request counted toward RPM=5, so four more must pass.
	for i := 0; i < 4; i++ {
		rel, ok, _ := Acquire(gid, 1, 5)
		if !ok {
			t.Fatalf("admit %d should pass; a concurrency-rejected request must not consume RPM", i+1)
		}
		rel()
	}
}
