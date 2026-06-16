package balancer

import (
	"sync"
	"testing"
	"time"

	"github.com/bestruirui/octopus/internal/model"
)

// Spread (round-robin) must keep equally healthy same-priority channels rotating
// instead of collapsing onto the lowest channel ID like a "pick best" strategy.
func TestSpreadRotatesEquallyHealthyChannels(t *testing.T) {
	roundRobinCounters = sync.Map{}
	ResetRuntimeTelemetry()

	items := []model.GroupItem{
		{ChannelID: 1, ModelName: "m", Priority: 1, Weight: 1},
		{ChannelID: 2, ModelName: "m", Priority: 1, Weight: 1},
	}
	first := (&Spread{}).Candidates(items)[0].ChannelID
	second := (&Spread{}).Candidates(items)[0].ChannelID
	if first == second {
		t.Fatalf("equally healthy channels should rotate, got %d then %d", first, second)
	}
}

// Priority is always a hard boundary; spread only balances inside a bucket.
func TestSpreadKeepsPriorityHardBoundary(t *testing.T) {
	roundRobinCounters = sync.Map{}
	ResetRuntimeTelemetry()

	items := []model.GroupItem{
		{ChannelID: 10, ModelName: "m", Priority: 2, Weight: 1},
		{ChannelID: 1, ModelName: "m", Priority: 1, Weight: 1},
	}
	for i := 0; i < 6; i++ {
		if got := (&Spread{}).Candidates(items)[0].ChannelID; got != 1 {
			t.Fatalf("priority 1 must stay ahead of priority 2, got %d", got)
		}
	}
}

// A busy channel (in-flight + selection load) is demoted within its priority
// bucket so spread sends new turns to the idle peer.
func TestSpreadDeprioritizesBusyChannel(t *testing.T) {
	roundRobinCounters = sync.Map{}
	ResetRuntimeTelemetry()

	items := []model.GroupItem{
		{ChannelID: 1, ModelName: "m", Priority: 1, Weight: 1, RoutingStats: model.RoutingRuntimeStats{InFlight: 3, AvailableKeyCount: 1, HealthyKeyCount: 1}},
		{ChannelID: 2, ModelName: "m", Priority: 1, Weight: 1, RoutingStats: model.RoutingRuntimeStats{AvailableKeyCount: 1, HealthyKeyCount: 1}},
	}
	for i := 0; i < 6; i++ {
		if got := (&Spread{}).Candidates(items)[0].ChannelID; got != 2 {
			t.Fatalf("idle channel should be preferred over busy one, got %d", got)
		}
	}
}

// A channel whose snapshot reports zero available keys cannot serve the request
// (the iterator will skip it for lack of a key), so spread must sink it behind a
// peer that can still serve — even a busy one — instead of wasting the front turn.
func TestSpreadSinksChannelWithNoAvailableKeys(t *testing.T) {
	roundRobinCounters = sync.Map{}
	ResetRuntimeTelemetry()

	items := []model.GroupItem{
		// Channel 1 has no usable key; channel 2 is healthy but carrying in-flight load.
		{ChannelID: 1, ModelName: "m", Priority: 1, Weight: 1, RoutingStats: model.RoutingRuntimeStats{AvailableKeyCount: 0, HealthyKeyCount: 0}},
		{ChannelID: 2, ModelName: "m", Priority: 1, Weight: 1, RoutingStats: model.RoutingRuntimeStats{InFlight: 3, AvailableKeyCount: 1, HealthyKeyCount: 1}},
	}
	for i := 0; i < 6; i++ {
		if got := (&Spread{}).Candidates(items)[0].ChannelID; got != 2 {
			t.Fatalf("servable channel should outrank a channel with no available keys, got %d", got)
		}
	}
}

// Selection-time reservation must show up as channel load so a burst spreads.
func TestMarkRuntimeSelectionCountsAsPendingLoad(t *testing.T) {
	ResetRuntimeTelemetry()

	MarkRuntimeSelection(1, "m")
	MarkRuntimeSelection(1, "m")
	snap := SnapshotRoutingRuntime(1, "m", nil, false)
	if snap.PendingSelections != 2 {
		t.Fatalf("expected 2 pending selections, got %d", snap.PendingSelections)
	}
}

// Starting the real attempt consumes one reservation so load is not double
// counted as both a pending selection and an in-flight request.
func TestBeginRuntimeAttemptConsumesSelection(t *testing.T) {
	ResetRuntimeTelemetry()

	MarkRuntimeSelection(1, "m")
	done := BeginRuntimeAttempt(1, 11, "m")
	defer done()
	snap := SnapshotRoutingRuntime(1, "m", nil, false)
	if snap.PendingSelections != 0 {
		t.Fatalf("attempt should consume the pending selection, got %d", snap.PendingSelections)
	}
	if snap.InFlight != 1 {
		t.Fatalf("expected 1 in-flight after begin, got %d", snap.InFlight)
	}
}

// An upstream Retry-After must win over the fixed per-status cooldown.
func TestRecordRuntimeFailureRespectsRetryAfter(t *testing.T) {
	ResetRuntimeTelemetry()

	// fixed 429 cooldown is 30s; Retry-After of 90s must extend it well beyond.
	RecordRuntimeFailure(1, 11, "m", 429, 0, 90*time.Second)
	snap := SnapshotRoutingRuntime(1, "m", nil, false)
	if snap.CooldownRemainingMs < 60*1000 {
		t.Fatalf("expected Retry-After (90s) to win over fixed 30s, got %dms", snap.CooldownRemainingMs)
	}
}

// Without Retry-After, the fixed per-status cooldown still applies.
func TestRecordRuntimeFailureFallsBackToFixedCooldown(t *testing.T) {
	ResetRuntimeTelemetry()

	RecordRuntimeFailure(1, 11, "m", 429, 0, 0)
	snap := SnapshotRoutingRuntime(1, "m", nil, false)
	if snap.CooldownRemainingMs <= 0 {
		t.Fatalf("expected fixed 429 cooldown to apply, got %dms", snap.CooldownRemainingMs)
	}
}

// A transient upstream-wide 503 must not let a single oversized Retry-After
// freeze the only key long enough to cascade into "no available channel". 429
// stays uncapped (a per-key rate-limit signal); transient 5xx is bounded.
func TestRecordRuntimeFailureBoundsTransientRetryAfter(t *testing.T) {
	ResetRuntimeTelemetry()

	// 503 + a 30-minute Retry-After must be capped at maxTransientRetryAfterCooldown.
	RecordRuntimeFailure(1, 11, "m", 503, 0, 30*time.Minute)
	snap := SnapshotRoutingRuntime(1, "m", nil, false)
	capMs := maxTransientRetryAfterCooldown.Milliseconds()
	if snap.CooldownRemainingMs > capMs {
		t.Fatalf("expected transient 503 Retry-After capped at %dms, got %dms", capMs, snap.CooldownRemainingMs)
	}
	if snap.CooldownRemainingMs <= 0 {
		t.Fatalf("expected a bounded cooldown to still apply, got %dms", snap.CooldownRemainingMs)
	}

	// The same oversized Retry-After on a 429 stays uncapped (per-key signal).
	ResetRuntimeTelemetry()
	RecordRuntimeFailure(2, 22, "m", 429, 0, 30*time.Minute)
	snap429 := SnapshotRoutingRuntime(2, "m", nil, false)
	if snap429.CooldownRemainingMs <= capMs {
		t.Fatalf("expected 429 Retry-After to stay uncapped (>%dms), got %dms", capMs, snap429.CooldownRemainingMs)
	}
}

// Within one health tier, a distinctly slower channel is demoted so spread sends
// new turns to the faster peer (latency-awareness — the axonhub-inspired upgrade).
func TestSpreadPrefersFasterChannelWithinTier(t *testing.T) {
	roundRobinCounters = sync.Map{}
	ResetRuntimeTelemetry()

	items := []model.GroupItem{
		{ChannelID: 1, ModelName: "m", Priority: 1, Weight: 1, RoutingStats: model.RoutingRuntimeStats{AvailableKeyCount: 1, HealthyKeyCount: 1, LatencyEWMAms: 5000}},
		{ChannelID: 2, ModelName: "m", Priority: 1, Weight: 1, RoutingStats: model.RoutingRuntimeStats{AvailableKeyCount: 1, HealthyKeyCount: 1, LatencyEWMAms: 300}},
	}
	for i := 0; i < 6; i++ {
		if got := (&Spread{}).Candidates(items)[0].ChannelID; got != 2 {
			t.Fatalf("distinctly faster channel should lead its tier, got %d", got)
		}
	}
}

// Anti-collapse guard: two channels with clearly different latencies that still
// land in the SAME coarse band (800ms and 1400ms are both in the >750 band) must
// keep rotating instead of the marginally-faster one winning every single turn.
// This proves the quantization — not just sub-750ms ties — is what preserves spread.
func TestSpreadKeepsRotatingSimilarLatency(t *testing.T) {
	roundRobinCounters = sync.Map{}
	ResetRuntimeTelemetry()

	items := []model.GroupItem{
		{ChannelID: 1, ModelName: "m", Priority: 1, Weight: 1, RoutingStats: model.RoutingRuntimeStats{AvailableKeyCount: 1, HealthyKeyCount: 1, LatencyEWMAms: 800}},
		{ChannelID: 2, ModelName: "m", Priority: 1, Weight: 1, RoutingStats: model.RoutingRuntimeStats{AvailableKeyCount: 1, HealthyKeyCount: 1, LatencyEWMAms: 1400}},
	}
	first := (&Spread{}).Candidates(items)[0].ChannelID
	second := (&Spread{}).Candidates(items)[0].ChannelID
	if first == second {
		t.Fatalf("channels in the same latency band should still rotate, got %d then %d", first, second)
	}
}

// A channel with recent consecutive failures (but not yet cooled down) yields to a
// clean peer in the same tier.
func TestSpreadDemotesRecentlyFailingChannel(t *testing.T) {
	roundRobinCounters = sync.Map{}
	ResetRuntimeTelemetry()

	items := []model.GroupItem{
		{ChannelID: 1, ModelName: "m", Priority: 1, Weight: 1, RoutingStats: model.RoutingRuntimeStats{AvailableKeyCount: 1, HealthyKeyCount: 1, ConsecutiveFailures: 3}},
		{ChannelID: 2, ModelName: "m", Priority: 1, Weight: 1, RoutingStats: model.RoutingRuntimeStats{AvailableKeyCount: 1, HealthyKeyCount: 1}},
	}
	for i := 0; i < 6; i++ {
		if got := (&Spread{}).Candidates(items)[0].ChannelID; got != 2 {
			t.Fatalf("recently failing channel should yield to a clean peer, got %d", got)
		}
	}
}

// Among busy (same-tier) channels, the lighter-loaded one leads so a burst keeps
// spreading toward the least-loaded peer.
func TestSpreadPrefersLessLoadedBusyChannel(t *testing.T) {
	roundRobinCounters = sync.Map{}
	ResetRuntimeTelemetry()

	items := []model.GroupItem{
		{ChannelID: 1, ModelName: "m", Priority: 1, Weight: 1, RoutingStats: model.RoutingRuntimeStats{AvailableKeyCount: 1, HealthyKeyCount: 1, InFlight: 6}},
		{ChannelID: 2, ModelName: "m", Priority: 1, Weight: 1, RoutingStats: model.RoutingRuntimeStats{AvailableKeyCount: 1, HealthyKeyCount: 1, InFlight: 1}},
	}
	for i := 0; i < 6; i++ {
		if got := (&Spread{}).Candidates(items)[0].ChannelID; got != 2 {
			t.Fatalf("less-loaded busy channel should lead, got %d", got)
		}
	}
}

// Self-heal: a slow latency sample that has gone stale (no fresh attempt within
// latencyStalenessWindow) must be treated as unobserved, so the demoted channel
// re-enters rotation and gets re-probed instead of being pinned behind a fast peer
// forever. Contrast with TestSpreadPrefersFasterChannelWithinTier, where the same
// 5000ms sample is fresh and DOES pin the channel.
func TestSpreadReprobesStaleSlowChannel(t *testing.T) {
	roundRobinCounters = sync.Map{}
	ResetRuntimeTelemetry()

	items := []model.GroupItem{
		{ChannelID: 1, ModelName: "m", Priority: 1, Weight: 1, RoutingStats: model.RoutingRuntimeStats{AvailableKeyCount: 1, HealthyKeyCount: 1, LatencyEWMAms: 5000, LatencyStale: true}},
		{ChannelID: 2, ModelName: "m", Priority: 1, Weight: 1, RoutingStats: model.RoutingRuntimeStats{AvailableKeyCount: 1, HealthyKeyCount: 1, LatencyEWMAms: 300}},
	}
	first := (&Spread{}).Candidates(items)[0].ChannelID
	second := (&Spread{}).Candidates(items)[0].ChannelID
	if first == second {
		t.Fatalf("a stale slow sample must not pin a channel; expected rotation, got %d then %d", first, second)
	}
}

// Streaming turns rank on first-token latency, not overall latency: channel 1 has
// great overall latency but a terrible first token, channel 2 is the opposite, so a
// streaming request must lead with channel 2.
func TestSpreadPrefersLowerFirstTokenWhenStreaming(t *testing.T) {
	roundRobinCounters = sync.Map{}
	ResetRuntimeTelemetry()

	items := []model.GroupItem{
		{ChannelID: 1, ModelName: "m", Priority: 1, Weight: 1, RoutingStats: model.RoutingRuntimeStats{PreferStream: true, AvailableKeyCount: 1, HealthyKeyCount: 1, LatencyEWMAms: 100, FirstTokenEWMAms: 5000}},
		{ChannelID: 2, ModelName: "m", Priority: 1, Weight: 1, RoutingStats: model.RoutingRuntimeStats{PreferStream: true, AvailableKeyCount: 1, HealthyKeyCount: 1, LatencyEWMAms: 9000, FirstTokenEWMAms: 200}},
	}
	for i := 0; i < 6; i++ {
		if got := (&Spread{}).Candidates(items)[0].ChannelID; got != 2 {
			t.Fatalf("streaming should prefer the lower first-token channel, got %d", got)
		}
	}
}

// Lock the coarse latency band boundaries (exclusive '>'): exactly 750/1500/3000/6000
// stay in the lower band; one millisecond more crosses up.
func TestSpreadLatencyBandBoundaries(t *testing.T) {
	band := func(lat float64) int {
		return spreadRank(model.GroupItem{RoutingStats: model.RoutingRuntimeStats{LatencyEWMAms: lat}})
	}
	cases := []struct {
		lat  float64
		want int
	}{
		{750, 0}, {751, 1},
		{1500, 1}, {1501, 2},
		{3000, 2}, {3001, 3},
		{6000, 3}, {6001, 4},
	}
	for _, c := range cases {
		if got := band(c.lat); got != c.want {
			t.Fatalf("latency %.0fms: want band %d, got %d", c.lat, c.want, got)
		}
	}
}

// A bare 429 / transient 5xx soft-cooldown starts at a few seconds and doubles per
// consecutive failure, capped — so a transient "backend busy" recovers fast while a
// persistently failing key still backs off. Guards the fix for relay upstreams (e.g.
// anyrouter) that 429/503 when overloaded without a Retry-After.
func TestRuntimeCooldownExponentialBackoff(t *testing.T) {
	cases := []struct {
		status int
		consec int64
		want   time.Duration
	}{
		{429, 1, 5 * time.Second},
		{429, 2, 10 * time.Second},
		{429, 3, 20 * time.Second},
		{429, 50, maxRuntimeCooldown}, // escalates but never exceeds the cap
		{503, 1, 3 * time.Second},     // transient 5xx uses an even shorter base
		{503, 2, 6 * time.Second},
		{500, 1, 3 * time.Second},
		{400, 1, 0}, // non-cooldown statuses never cool down
		{200, 5, 0},
	}
	for _, c := range cases {
		if got := runtimeCooldownForStatus(c.status, c.consec); got != c.want {
			t.Fatalf("status %d consec %d: want %v, got %v", c.status, c.consec, c.want, got)
		}
	}
}
