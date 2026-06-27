package balancer

import (
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/bestruirui/octopus/internal/model"
)

const runtimeEWMAAlpha = 0.35

// selectionReservationWindow bounds how long a balancer selection counts toward a
// channel's effective load before it self-expires. It only needs to cover the gap
// between picking a candidate and BeginRuntimeAttempt taking over the real
// in-flight count, so concurrent bursts spread out instead of stampeding one idle
// channel. Short enough that a skipped/aborted pick never leaks reservation.
const selectionReservationWindow = 3 * time.Second

// latencyStalenessWindow bounds how long the spread balancer trusts a recorded
// latency/first-token EWMA. A channel only gets a fresh latency sample when it is
// actually attempted, so a peer demoted by one slow sample would never be
// re-sampled (octopus has no inactivity decay, unlike axonhub) and would stay
// pinned behind a fast channel forever. After this window the spread strategy
// treats the stale sample as unobserved, so the channel re-enters rotation and
// re-probes — the self-heal that keeps "fastest single channel never wins forever"
// actually true.
const latencyStalenessWindow = 90 * time.Second

// maxTransientRetryAfterCooldown bounds how long an upstream Retry-After may back
// off a key for transient upstream-wide failures (502/503/504/520). A 429 is a
// per-key rate-limit signal and keeps the provider's full Retry-After, but a
// transient 5xx is usually the whole upstream wobbling (e.g. an intermittent 1M
// backend); honouring a single large Retry-After there would freeze the only key
// long enough to cascade into "no available channel". A modest ceiling keeps the
// backoff useful without blacking out every key on one transient failure.
const maxTransientRetryAfterCooldown = 60 * time.Second

var globalRuntimeTelemetry sync.Map // key: channelID:keyID:modelName -> *runtimeEntry, keyID=0 is channel aggregate

type runtimeEntry struct {
	mu sync.Mutex

	latencyEWMAms     float64
	firstTokenEWMAms  float64
	throughputEWMA    float64
	latencySamples    int64
	firstTokenSamples int64
	throughputSamples int64
	latencySampleAt   time.Time

	inFlight            int64
	attempts            int64
	requestSuccess      int64
	requestFailed       int64
	consecutiveFailures int64
	lastFailure         time.Time
	cooldownUntil       time.Time

	// selection-time reservation (channel-level only): counts candidates the
	// balancer just picked but that have not yet entered BeginRuntimeAttempt.
	selectionCount   int64
	selectionResetAt time.Time

	// reqWindow is a 60-bucket-per-second sliding-window counter of upstream
	// attempts started on this channel/model, used to derive a recent
	// requests-per-minute figure for the RPM soft-demote in spreadTier. reqBucketSec
	// records which epoch-second each slot currently represents so a stale slot (from
	// a previous minute) is treated as empty instead of double-counted. Channel-level
	// only: BeginRuntimeAttempt records into the keyID=0 aggregate entry, so the
	// figure is per (channel, model) — matching how MaxConcurrent/InFlight are scoped.
	reqWindow    [60]int32
	reqBucketSec [60]int64
}

// recordRequestLocked counts one upstream attempt into the trailing-60s window.
// Caller must hold e.mu.
func (e *runtimeEntry) recordRequestLocked(now time.Time) {
	sec := now.Unix()
	idx := sec % 60
	if e.reqBucketSec[idx] != sec {
		e.reqBucketSec[idx] = sec
		e.reqWindow[idx] = 0
	}
	e.reqWindow[idx]++
}

// recentRequestCountLocked sums attempts started in the trailing 60 seconds.
// Caller must hold e.mu.
func (e *runtimeEntry) recentRequestCountLocked(now time.Time) int64 {
	cutoff := now.Unix() - 59
	var total int64
	for i := 0; i < 60; i++ {
		if e.reqBucketSec[i] >= cutoff {
			total += int64(e.reqWindow[i])
		}
	}
	return total
}

// AttemptRuntimeMetrics describes a completed upstream attempt for adaptive
// smart routing. Durations are per-attempt rather than whole-request metrics so
// fallback retries do not poison the successful channel's latency sample.
type AttemptRuntimeMetrics struct {
	Duration     time.Duration
	FirstToken   time.Duration
	OutputTokens int64
	Stream       bool
}

func runtimeKey(channelID, keyID int, modelName string) string {
	return fmt.Sprintf("%d:%d:%s", channelID, keyID, modelName)
}

func getOrCreateRuntimeEntry(channelID, keyID int, modelName string) *runtimeEntry {
	key := runtimeKey(channelID, keyID, modelName)
	entry := &runtimeEntry{}
	actual, _ := globalRuntimeTelemetry.LoadOrStore(key, entry)
	return actual.(*runtimeEntry)
}

func loadRuntimeEntry(channelID, keyID int, modelName string) (*runtimeEntry, bool) {
	v, ok := globalRuntimeTelemetry.Load(runtimeKey(channelID, keyID, modelName))
	if !ok {
		return nil, false
	}
	entry, ok := v.(*runtimeEntry)
	return entry, ok
}

func runtimeEntriesForAttempt(channelID, keyID int, modelName string) []*runtimeEntry {
	channelEntry := getOrCreateRuntimeEntry(channelID, 0, modelName)
	if keyID <= 0 {
		return []*runtimeEntry{channelEntry}
	}
	return []*runtimeEntry{
		channelEntry,
		getOrCreateRuntimeEntry(channelID, keyID, modelName),
	}
}

// ResetRuntimeTelemetry clears in-memory adaptive routing data. Production code
// normally never calls this; tests use it to avoid cross-case routing bleed.
func ResetRuntimeTelemetry() {
	globalRuntimeTelemetry = sync.Map{}
}

// MarkRuntimeSelection records that the balancer just picked this channel/model
// as a candidate, before the real attempt starts. This is a selection-time
// reservation: a burst of concurrent requests sees the bumped count via the
// channel snapshot and spreads to other channels instead of all stampeding the
// same idle one. The reservation self-expires after selectionReservationWindow
// and is consumed by BeginRuntimeAttempt, so a skipped pick never leaks it.
func MarkRuntimeSelection(channelID int, modelName string) {
	entry := getOrCreateRuntimeEntry(channelID, 0, modelName)
	entry.mu.Lock()
	now := time.Now()
	if !entry.selectionResetAt.IsZero() && now.After(entry.selectionResetAt) {
		entry.selectionCount = 0
	}
	entry.selectionCount++
	entry.selectionResetAt = now.Add(selectionReservationWindow)
	entry.mu.Unlock()
}

// BeginRuntimeAttempt increments in-flight/load counters for both the selected
// channel/model aggregate and the concrete key. The returned closure must be
// called once when the attempt finishes.
func BeginRuntimeAttempt(channelID, keyID int, modelName string) func() {
	entries := runtimeEntriesForAttempt(channelID, keyID, modelName)
	now := time.Now()
	for i, entry := range entries {
		entry.mu.Lock()
		entry.inFlight++
		entry.attempts++
		// entries[0] is the channel-level aggregate; the real attempt now owns this
		// load, so consume one pending selection to avoid double-counting it as both
		// a selection and an in-flight request. Record the per-minute window only on
		// the channel-level aggregate so RecentRequestCount is per (channel, model).
		if i == 0 {
			if entry.selectionCount > 0 {
				entry.selectionCount--
			}
			entry.recordRequestLocked(now)
		}
		entry.mu.Unlock()
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			for _, entry := range entries {
				entry.mu.Lock()
				if entry.inFlight > 0 {
					entry.inFlight--
				}
				entry.mu.Unlock()
			}
		})
	}
}

// RecordRuntimeSuccess updates recent latency/throughput health for the
// successful channel/key and clears temporary error cooldowns.
func RecordRuntimeSuccess(channelID, keyID int, modelName string, metrics AttemptRuntimeMetrics) {
	for _, entry := range runtimeEntriesForAttempt(channelID, keyID, modelName) {
		entry.recordSuccess(metrics)
	}
}

func (e *runtimeEntry) recordSuccess(metrics AttemptRuntimeMetrics) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.requestSuccess++
	e.consecutiveFailures = 0
	e.cooldownUntil = time.Time{}
	e.observeLatencyLocked(metrics.Duration)
	if metrics.FirstToken > 0 {
		e.firstTokenEWMAms = updateEWMA(e.firstTokenEWMAms, float64(metrics.FirstToken.Milliseconds()), e.firstTokenSamples)
		e.firstTokenSamples++
	}
	if tps := throughputTokensPerSecond(metrics); tps > 0 {
		e.throughputEWMA = updateEWMA(e.throughputEWMA, tps, e.throughputSamples)
		e.throughputSamples++
	}
}

// RecordRuntimeFailure updates recent failure health and applies a short
// soft-cooldown before the full circuit breaker trips. This prevents repeated
// hammering of rate-limited/overloaded keys while still preserving fallback if
// every candidate is unhealthy. When the upstream provided a Retry-After value
// (retryAfter > 0) it takes precedence over the fixed per-status cooldown so the
// channel/key is backed off for exactly as long as the provider asked.
func RecordRuntimeFailure(channelID, keyID int, modelName string, statusCode int, duration time.Duration, retryAfter time.Duration) {
	for _, entry := range runtimeEntriesForAttempt(channelID, keyID, modelName) {
		entry.recordFailure(statusCode, duration, retryAfter)
	}
}

func (e *runtimeEntry) recordFailure(statusCode int, duration time.Duration, retryAfter time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.requestFailed++
	e.consecutiveFailures++
	e.lastFailure = time.Now()
	e.observeLatencyLocked(duration)
	cooldown := runtimeCooldownForStatus(statusCode, e.consecutiveFailures)
	if retryAfter > 0 {
		// Upstream told us exactly how long to wait; trust it over the guess,
		// but bound it for transient upstream-wide failures so one large
		// Retry-After cannot freeze the only key into "no available channel".
		cooldown = boundedRetryAfterCooldown(statusCode, retryAfter)
	}
	if cooldown > 0 {
		until := time.Now().Add(cooldown)
		if until.After(e.cooldownUntil) {
			e.cooldownUntil = until
		}
	}
}

func (e *runtimeEntry) observeLatencyLocked(duration time.Duration) {
	if duration <= 0 {
		return
	}
	e.latencyEWMAms = updateEWMA(e.latencyEWMAms, float64(duration.Milliseconds()), e.latencySamples)
	e.latencySamples++
	e.latencySampleAt = time.Now()
}

func updateEWMA(current, sample float64, samples int64) float64 {
	if sample <= 0 {
		return current
	}
	if samples <= 0 || current <= 0 {
		return sample
	}
	return current*(1-runtimeEWMAAlpha) + sample*runtimeEWMAAlpha
}

func throughputTokensPerSecond(metrics AttemptRuntimeMetrics) float64 {
	if metrics.OutputTokens <= 0 || metrics.Duration <= 0 {
		return 0
	}
	generateDuration := metrics.Duration
	if metrics.FirstToken > 0 && metrics.Duration > metrics.FirstToken {
		generateDuration = metrics.Duration - metrics.FirstToken
	}
	seconds := generateDuration.Seconds()
	if seconds <= 0 {
		return 0
	}
	return float64(metrics.OutputTokens) / seconds
}

// maxRuntimeCooldown caps the exponential soft-cooldown so a persistently failing
// key is still re-tried within a couple of minutes.
const maxRuntimeCooldown = 2 * time.Minute

// runtimeCooldownForStatus returns how long to soft-cool a key/channel after a
// failure. A bare 429 / transient 5xx (no Retry-After) from a relay-style upstream
// is usually a short "backend busy" blip, so it starts at a few seconds and only
// escalates with consecutiveFailures (exponential backoff, capped) — instead of a
// flat long bench that stalls recovery and makes "just retry" never succeed. A
// genuine provider rate-limit ships a Retry-After, which the caller honours over
// this value.
func runtimeCooldownForStatus(statusCode int, consecutiveFailures int64) time.Duration {
	var base time.Duration
	switch statusCode {
	case http.StatusTooManyRequests:
		base = 5 * time.Second
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, 520, 529:
		base = 3 * time.Second
	default:
		if statusCode < 500 {
			return 0
		}
		base = 3 * time.Second
	}

	shift := consecutiveFailures - 1
	if shift < 0 {
		shift = 0
	}
	if shift > 16 {
		shift = 16
	}
	cooldown := base << uint(shift)
	if cooldown <= 0 || cooldown > maxRuntimeCooldown {
		return maxRuntimeCooldown
	}
	return cooldown
}

// boundedRetryAfterCooldown honours an upstream Retry-After but caps it for
// transient upstream-wide statuses (502/503/504/520) so a single large value
// cannot black out the only key. 429 rate-limit backoffs are per-key and keep
// the provider's full value.
func boundedRetryAfterCooldown(statusCode int, retryAfter time.Duration) time.Duration {
	if retryAfter > maxTransientRetryAfterCooldown && isTransientUpstreamStatus(statusCode) {
		return maxTransientRetryAfterCooldown
	}
	return retryAfter
}

func isTransientUpstreamStatus(statusCode int) bool {
	switch statusCode {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, 520, 529:
		return true
	default:
		return false
	}
}

func (e *runtimeEntry) snapshot() model.RoutingRuntimeStats {
	if e == nil {
		return model.RoutingRuntimeStats{}
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	cooldownRemaining := int64(0)
	if !e.cooldownUntil.IsZero() {
		if remaining := time.Until(e.cooldownUntil); remaining > 0 {
			cooldownRemaining = remaining.Milliseconds()
		}
	}
	pendingSelections := int64(0)
	if e.selectionCount > 0 && !e.selectionResetAt.IsZero() && time.Now().Before(e.selectionResetAt) {
		pendingSelections = e.selectionCount
	}
	// A latency sample older than latencyStalenessWindow is no longer trusted by the
	// spread balancer (see the constant): it lets a demoted channel re-enter rotation
	// and re-probe instead of being pinned behind a fast peer forever.
	latencyStale := e.latencyEWMAms > 0 && !e.latencySampleAt.IsZero() && time.Since(e.latencySampleAt) > latencyStalenessWindow
	return model.RoutingRuntimeStats{
		HasRuntime:          e.latencySamples > 0 || e.firstTokenSamples > 0 || e.throughputSamples > 0 || e.requestSuccess > 0 || e.requestFailed > 0 || e.inFlight > 0 || e.attempts > 0 || pendingSelections > 0,
		RecentRequestCount:  e.recentRequestCountLocked(time.Now()),
		LatencyEWMAms:       e.latencyEWMAms,
		FirstTokenEWMAms:    e.firstTokenEWMAms,
		ThroughputEWMA:      e.throughputEWMA,
		LatencyStale:        latencyStale,
		InFlight:            e.inFlight,
		PendingSelections:   pendingSelections,
		Attempts:            e.attempts,
		RequestSuccess:      e.requestSuccess,
		RequestFailed:       e.requestFailed,
		ConsecutiveFailures: e.consecutiveFailures,
		LastFailureUnix: func() int64 {
			if e.lastFailure.IsZero() {
				return 0
			}
			return e.lastFailure.Unix()
		}(),
		CooldownRemainingMs: cooldownRemaining,
	}
}

// SnapshotRoutingRuntime returns a channel/model snapshot enriched with key
// circuit/cooldown state. Keys may be nil when the caller has not loaded the
// channel yet; in that case the channel-level circuit snapshot is still used.
func SnapshotRoutingRuntime(channelID int, modelName string, keys []model.ChannelKey, preferStream bool) model.RoutingRuntimeStats {
	snapshot := model.RoutingRuntimeStats{PreferStream: preferStream}
	if entry, ok := loadRuntimeEntry(channelID, 0, modelName); ok {
		snapshot = entry.snapshot()
		snapshot.PreferStream = preferStream
	}

	available := 0
	healthy := 0
	circuitOpen := 0
	keyCooldown := 0
	channelCooldownRemaining := snapshot.CooldownRemainingMs
	maxKeyCooldownRemaining := int64(0)
	var maxCircuitRemaining int64
	for _, key := range keys {
		if !key.Enabled {
			continue
		}
		available++
		tripped, remaining := IsTripped(channelID, key.ID, modelName)
		if tripped {
			circuitOpen++
			if ms := remaining.Milliseconds(); ms > maxCircuitRemaining {
				maxCircuitRemaining = ms
			}
			continue
		}
		keySnapshot := SnapshotKeyRuntime(channelID, key.ID, modelName)
		if keySnapshot.CooldownRemainingMs > 0 {
			keyCooldown++
			if keySnapshot.CooldownRemainingMs > maxKeyCooldownRemaining {
				maxKeyCooldownRemaining = keySnapshot.CooldownRemainingMs
			}
			continue
		}
		healthy++
	}
	snapshot.AvailableKeyCount = available
	snapshot.HealthyKeyCount = healthy
	snapshot.CircuitOpenKeys = circuitOpen
	snapshot.CircuitRemainingMs = maxCircuitRemaining
	snapshot.KeyCooldownOpenCount = keyCooldown
	if circuitOpen > 0 {
		snapshot.CircuitTripped = true
	}
	if available > 0 {
		if healthy > 0 {
			snapshot.CooldownRemainingMs = 0
		} else {
			snapshot.CooldownRemainingMs = channelCooldownRemaining
			if maxKeyCooldownRemaining > snapshot.CooldownRemainingMs {
				snapshot.CooldownRemainingMs = maxKeyCooldownRemaining
			}
		}
	}

	if available == 0 {
		channelCircuit := SnapshotChannel(channelID)
		if channelCircuit.Tripped {
			snapshot.CircuitTripped = true
			snapshot.CircuitOpenKeys = channelCircuit.OpenKeys
			snapshot.CircuitRemainingMs = int64(channelCircuit.RemainingSeconds) * int64(time.Second/time.Millisecond)
		}
	}
	return snapshot
}

func SnapshotKeyRuntime(channelID, keyID int, modelName string) model.RoutingRuntimeStats {
	if entry, ok := loadRuntimeEntry(channelID, keyID, modelName); ok {
		return entry.snapshot()
	}
	return model.RoutingRuntimeStats{}
}

// PrioritizeChannelKeysByHealth keeps healthy keys ahead of keys that are
// currently circuit-open or in a soft runtime cooldown. A preferred sticky or
// previous-response key still wins when healthy, but it will not be hammered
// while tripped.
func PrioritizeChannelKeysByHealth(keys []model.ChannelKey, channelID int, modelName string, preferredID int) []model.ChannelKey {
	if len(keys) < 2 {
		return keys
	}
	type rankedKey struct {
		key      model.ChannelKey
		index    int
		rank     int
		inFlight int64
		latency  float64
	}
	ranked := make([]rankedKey, 0, len(keys))
	for i, key := range keys {
		snap := SnapshotKeyRuntime(channelID, key.ID, modelName)
		tripped, _ := IsTripped(channelID, key.ID, modelName)
		rank := 1
		switch {
		case tripped:
			rank = 4
		case snap.CooldownRemainingMs > 0:
			rank = 3
		case preferredID > 0 && key.ID == preferredID:
			rank = 0
		default:
			rank = 1
		}
		ranked = append(ranked, rankedKey{
			key:      key,
			index:    i,
			rank:     rank,
			inFlight: snap.InFlight,
			latency:  snap.LatencyEWMAms,
		})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		left := ranked[i]
		right := ranked[j]
		if left.rank != right.rank {
			return left.rank < right.rank
		}
		if left.inFlight != right.inFlight {
			return left.inFlight < right.inFlight
		}
		if left.latency > 0 && right.latency > 0 && left.latency != right.latency {
			return left.latency < right.latency
		}
		return left.index < right.index
	})

	out := make([]model.ChannelKey, len(keys))
	for i := range ranked {
		out[i] = ranked[i].key
	}
	return out
}
