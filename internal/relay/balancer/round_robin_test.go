package balancer

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bestruirui/octopus/internal/model"
)

func TestRoundRobinCountersAreScopedByCandidateSet(t *testing.T) {
	roundRobinCounters = sync.Map{}

	left := []model.GroupItem{
		{ChannelID: 1, ModelName: "left", Priority: 1},
		{ChannelID: 2, ModelName: "left", Priority: 1},
	}
	right := []model.GroupItem{
		{ChannelID: 10, ModelName: "right", Priority: 1},
		{ChannelID: 20, ModelName: "right", Priority: 1},
	}
	rr := &RoundRobin{}

	if got := rr.Candidates(left)[0].ChannelID; got != 1 {
		t.Fatalf("first left candidate = %d, want 1", got)
	}
	if got := rr.Candidates(right)[0].ChannelID; got != 10 {
		t.Fatalf("first right candidate = %d, want 10; round robin counters must not bleed across groups", got)
	}
	if got := rr.Candidates(left)[0].ChannelID; got != 2 {
		t.Fatalf("second left candidate = %d, want 2", got)
	}
	if got := rr.Candidates(right)[0].ChannelID; got != 20 {
		t.Fatalf("second right candidate = %d, want 20", got)
	}
}

func TestRoundRobinConcurrentDistribution(t *testing.T) {
	roundRobinCounters = sync.Map{}

	items := []model.GroupItem{
		{ChannelID: 1, ModelName: "concurrent", Priority: 1},
		{ChannelID: 2, ModelName: "concurrent", Priority: 1},
		{ChannelID: 3, ModelName: "concurrent", Priority: 1},
	}
	rr := &RoundRobin{}

	var counts [4]int64
	var wg sync.WaitGroup
	for i := 0; i < 300; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			candidates := rr.Candidates(items)
			if len(candidates) != len(items) {
				t.Errorf("candidate count = %d, want %d", len(candidates), len(items))
				return
			}
			first := candidates[0].ChannelID
			if first < 1 || first > 3 {
				t.Errorf("unexpected first channel %d", first)
				return
			}
			atomic.AddInt64(&counts[first], 1)
		}()
	}
	wg.Wait()

	for channelID := 1; channelID <= 3; channelID++ {
		if got := atomic.LoadInt64(&counts[channelID]); got != 100 {
			t.Fatalf("channel %d selected %d times, want 100; counts=%#v", channelID, got, counts)
		}
	}
}

func TestRoundRobinKeepsPriorityBucketsBeforeRotating(t *testing.T) {
	roundRobinCounters = sync.Map{}

	items := []model.GroupItem{
		{ChannelID: 10, ModelName: "fallback-a", Priority: 2},
		{ChannelID: 1, ModelName: "primary-a", Priority: 1},
		{ChannelID: 2, ModelName: "primary-b", Priority: 1},
		{ChannelID: 20, ModelName: "fallback-b", Priority: 2},
	}
	rr := &RoundRobin{}

	first := rr.Candidates(items)
	if len(first) != 4 {
		t.Fatalf("candidate count = %d, want 4", len(first))
	}
	if first[0].ChannelID != 1 || first[1].ChannelID != 2 {
		t.Fatalf("highest priority bucket should be first, got %#v", first)
	}
	if first[2].ChannelID != 10 || first[3].ChannelID != 20 {
		t.Fatalf("fallback bucket should remain after primary bucket, got %#v", first)
	}

	second := rr.Candidates(items)
	if second[0].ChannelID != 2 || second[1].ChannelID != 1 {
		t.Fatalf("same priority bucket should round-robin, got %#v", second)
	}
	if second[2].ChannelID != 20 || second[3].ChannelID != 10 {
		t.Fatalf("fallback bucket should rotate only inside its own priority, got %#v", second)
	}
}

func TestWeightedKeepsHigherPriorityBucketAheadOfWeight(t *testing.T) {
	weightedCounters = sync.Map{}

	items := []model.GroupItem{
		{ChannelID: 1, ModelName: "primary", Priority: 1, Weight: 1},
		{ChannelID: 2, ModelName: "fallback-heavy", Priority: 2, Weight: 1000},
	}
	weighted := &Weighted{}

	for i := 0; i < 20; i++ {
		candidates := weighted.Candidates(items)
		if len(candidates) != 2 {
			t.Fatalf("candidate count = %d, want 2", len(candidates))
		}
		if candidates[0].ChannelID != 1 {
			t.Fatalf("higher priority bucket must stay first even against heavy fallback weight, got %#v", candidates)
		}
	}
}

func TestWeightedCandidatesPreferHigherWeightOverManySelections(t *testing.T) {
	weightedCounters = sync.Map{}

	items := []model.GroupItem{
		{ChannelID: 1, ModelName: "weighted", Weight: 1},
		{ChannelID: 2, ModelName: "weighted", Weight: 100},
	}
	weighted := &Weighted{}

	var low, high int
	for i := 0; i < 1000; i++ {
		candidates := weighted.Candidates(items)
		if len(candidates) != 2 {
			t.Fatalf("candidate count = %d, want 2", len(candidates))
		}
		switch candidates[0].ChannelID {
		case 1:
			low++
		case 2:
			high++
		default:
			t.Fatalf("unexpected first channel %d", candidates[0].ChannelID)
		}
	}
	if high <= low*20 {
		t.Fatalf("higher weighted channel was not preferred enough: high=%d low=%d", high, low)
	}
}
