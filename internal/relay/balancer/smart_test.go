package balancer

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/bestruirui/octopus/internal/model"
)

func TestSmartCandidatesPrefersLowerChannelPriority(t *testing.T) {
	items := []model.GroupItem{
		{
			ChannelID:       1,
			ModelName:       "gpt",
			Priority:        1,
			Weight:          1,
			ChannelPriority: 10,
			ChannelStats:    model.StatsChannel{StatsMetrics: model.StatsMetrics{RequestSuccess: 10}},
		},
		{
			ChannelID:       2,
			ModelName:       "gpt",
			Priority:        1,
			Weight:          1,
			ChannelPriority: 0,
			ChannelStats:    model.StatsChannel{StatsMetrics: model.StatsMetrics{RequestSuccess: 10}},
		},
	}

	candidates := (&Smart{}).Candidates(items)
	if len(candidates) != 2 || candidates[0].ChannelID != 2 {
		t.Fatalf("expected channel 2 first, got %#v", candidates)
	}
}

func TestSmartCandidatesKeepsPriorityBucketsAheadOfStats(t *testing.T) {
	items := []model.GroupItem{
		{
			ChannelID: 1,
			ModelName: "gpt",
			Priority:  2,
			Weight:    100,
			ChannelStats: model.StatsChannel{StatsMetrics: model.StatsMetrics{
				RequestSuccess: 1000,
				RequestFailed:  0,
				WaitTime:       1000 * 50,
			}},
		},
		{
			ChannelID: 2,
			ModelName: "gpt",
			Priority:  1,
			Weight:    1,
			ChannelStats: model.StatsChannel{StatsMetrics: model.StatsMetrics{
				RequestSuccess: 1,
				RequestFailed:  1,
				WaitTime:       2 * 5000,
			}},
		},
	}

	candidates := (&Smart{}).Candidates(items)
	if len(candidates) != 2 || candidates[0].ChannelID != 2 {
		t.Fatalf("expected lower priority bucket before healthier high-priority channel, got %#v", candidates)
	}
}

func TestSmartCandidatesPenalizesFailuresAndLatency(t *testing.T) {
	items := []model.GroupItem{
		{
			ChannelID: 2,
			ModelName: "gpt",
			Priority:  1,
			Weight:    20,
			ChannelStats: model.StatsChannel{StatsMetrics: model.StatsMetrics{
				RequestSuccess: 80,
				RequestFailed:  80,
				WaitTime:       160 * 5000,
			}},
		},
		{
			ChannelID: 1,
			ModelName: "gpt",
			Priority:  1,
			Weight:    1,
			ChannelStats: model.StatsChannel{StatsMetrics: model.StatsMetrics{
				RequestSuccess: 80,
				RequestFailed:  0,
				WaitTime:       80 * 200,
			}},
		},
	}

	candidates := (&Smart{}).Candidates(items)
	if len(candidates) != 2 || candidates[0].ChannelID != 1 {
		t.Fatalf("expected healthy channel first, got %#v", candidates)
	}
}

func TestSmartCandidatesUsesWeightInsideSameHealthyBucket(t *testing.T) {
	items := []model.GroupItem{
		{
			ChannelID: 1,
			ModelName: "gpt",
			Priority:  1,
			Weight:    1,
			ChannelStats: model.StatsChannel{StatsMetrics: model.StatsMetrics{
				RequestSuccess: 10,
				RequestFailed:  0,
				WaitTime:       10 * 200,
			}},
		},
		{
			ChannelID: 2,
			ModelName: "gpt",
			Priority:  1,
			Weight:    20,
			ChannelStats: model.StatsChannel{StatsMetrics: model.StatsMetrics{
				RequestSuccess: 10,
				RequestFailed:  0,
				WaitTime:       10 * 200,
			}},
		},
	}

	candidates := (&Smart{}).Candidates(items)
	if len(candidates) != 2 || candidates[0].ChannelID != 2 {
		t.Fatalf("expected higher weight inside the same healthy bucket, got %#v", candidates)
	}
}

func TestSmartCandidatesPrefersObservedHealthyStatsOverNoStats(t *testing.T) {
	items := []model.GroupItem{
		{
			ChannelID: 2,
			ModelName: "gpt",
			Priority:  1,
			Weight:    1,
		},
		{
			ChannelID: 1,
			ModelName: "gpt",
			Priority:  1,
			Weight:    1,
			ChannelStats: model.StatsChannel{StatsMetrics: model.StatsMetrics{
				RequestSuccess: 10,
				RequestFailed:  0,
				WaitTime:       10 * 200,
			}},
		},
	}

	candidates := (&Smart{}).Candidates(items)
	if len(candidates) != 2 || candidates[0].ChannelID != 1 {
		t.Fatalf("expected observed healthy channel before no-stats channel, got %#v", candidates)
	}
}

func TestSmartCandidatesPrefersFastStreamRuntime(t *testing.T) {
	ResetRuntimeTelemetry()

	RecordRuntimeSuccess(1, 11, "gpt", AttemptRuntimeMetrics{
		Duration:     5 * time.Second,
		FirstToken:   2500 * time.Millisecond,
		OutputTokens: 20,
		Stream:       true,
	})
	RecordRuntimeSuccess(2, 22, "gpt", AttemptRuntimeMetrics{
		Duration:     900 * time.Millisecond,
		FirstToken:   120 * time.Millisecond,
		OutputTokens: 120,
		Stream:       true,
	})

	items := []model.GroupItem{
		{
			ChannelID:    1,
			ModelName:    "gpt",
			Priority:     1,
			Weight:       1,
			RoutingStats: SnapshotRoutingRuntime(1, "gpt", []model.ChannelKey{{ID: 11, ChannelID: 1, Enabled: true}}, true),
		},
		{
			ChannelID:    2,
			ModelName:    "gpt",
			Priority:     1,
			Weight:       1,
			RoutingStats: SnapshotRoutingRuntime(2, "gpt", []model.ChannelKey{{ID: 22, ChannelID: 2, Enabled: true}}, true),
		},
	}

	candidates := (&Smart{}).Candidates(items)
	if len(candidates) != 2 || candidates[0].ChannelID != 2 {
		t.Fatalf("expected faster TTFT/TPS channel first, got %#v", candidates)
	}
}

func TestSmartCandidatesPenalizesUnavailableCircuitBucket(t *testing.T) {
	items := []model.GroupItem{
		{
			ChannelID: 1,
			ModelName: "gpt",
			Priority:  1,
			Weight:    100,
			RoutingStats: model.RoutingRuntimeStats{
				AvailableKeyCount: 1,
				HealthyKeyCount:   0,
				CircuitTripped:    true,
				CircuitOpenKeys:   1,
			},
		},
		{
			ChannelID: 2,
			ModelName: "gpt",
			Priority:  1,
			Weight:    1,
			RoutingStats: model.RoutingRuntimeStats{
				AvailableKeyCount: 1,
				HealthyKeyCount:   1,
			},
		},
	}

	candidates := (&Smart{}).Candidates(items)
	if len(candidates) != 2 || candidates[0].ChannelID != 2 {
		t.Fatalf("expected healthy channel before circuit-open channel, got %#v", candidates)
	}
}

func TestSmartCandidatesPenalizesInFlightToSpreadConcurrentTurns(t *testing.T) {
	items := []model.GroupItem{
		{
			ChannelID: 1,
			ModelName: "gpt",
			Priority:  1,
			Weight:    1,
			RoutingStats: model.RoutingRuntimeStats{
				HasRuntime:        true,
				LatencyEWMAms:     200,
				RequestSuccess:    10,
				AvailableKeyCount: 1,
				HealthyKeyCount:   1,
				InFlight:          4,
			},
		},
		{
			ChannelID: 2,
			ModelName: "gpt",
			Priority:  1,
			Weight:    1,
			RoutingStats: model.RoutingRuntimeStats{
				HasRuntime:        true,
				LatencyEWMAms:     220,
				RequestSuccess:    10,
				AvailableKeyCount: 1,
				HealthyKeyCount:   1,
			},
		},
	}

	candidates := (&Smart{}).Candidates(items)
	if len(candidates) != 2 || candidates[0].ChannelID != 2 {
		t.Fatalf("expected idle channel before busy channel, got %#v", candidates)
	}
}

func TestSmartCandidatesUsesRuntimeLoadRatioForWeightFairness(t *testing.T) {
	items := []model.GroupItem{
		{
			ChannelID: 1,
			ModelName: "gpt",
			Priority:  1,
			Weight:    10,
			RoutingStats: model.RoutingRuntimeStats{
				HasRuntime:        true,
				LatencyEWMAms:     200,
				RequestSuccess:    100,
				Attempts:          100,
				AvailableKeyCount: 1,
				HealthyKeyCount:   1,
			},
		},
		{
			ChannelID: 2,
			ModelName: "gpt",
			Priority:  1,
			Weight:    1,
			RoutingStats: model.RoutingRuntimeStats{
				HasRuntime:        true,
				LatencyEWMAms:     210,
				RequestSuccess:    1,
				AvailableKeyCount: 1,
				HealthyKeyCount:   1,
			},
		},
	}

	candidates := (&Smart{}).Candidates(items)
	if len(candidates) != 2 || candidates[0].ChannelID != 2 {
		t.Fatalf("expected under-used lower-weight channel to get a turn, got %#v", candidates)
	}
}

func TestSmartCandidatesExploresUnobservedOverVerySlowRuntime(t *testing.T) {
	items := []model.GroupItem{
		{
			ChannelID: 1,
			ModelName: "gpt",
			Priority:  1,
			Weight:    1,
			RoutingStats: model.RoutingRuntimeStats{
				HasRuntime:        true,
				LatencyEWMAms:     5000,
				FirstTokenEWMAms:  2500,
				RequestSuccess:    1,
				Attempts:          1,
				AvailableKeyCount: 1,
				HealthyKeyCount:   1,
				PreferStream:      true,
			},
		},
		{
			ChannelID: 2,
			ModelName: "gpt",
			Priority:  1,
			Weight:    1,
		},
	}

	candidates := (&Smart{}).Candidates(items)
	if len(candidates) != 2 || candidates[0].ChannelID != 2 {
		t.Fatalf("expected unobserved channel to be explored before very slow channel, got %#v", candidates)
	}
}

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
