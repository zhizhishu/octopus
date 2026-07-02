package model

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestChannelGetAvailableChannelKeysSkipsRecent429AndSortsByCost(t *testing.T) {
	now := time.Now().Unix()
	channel := Channel{
		Keys: []ChannelKey{
			{ID: 1, Enabled: true, ChannelKey: "recent-429", StatusCode: 429, LastUseTimeStamp: now - 2, TotalCost: 0},
			{ID: 2, Enabled: true, ChannelKey: "higher-cost", TotalCost: 2},
			{ID: 3, Enabled: true, ChannelKey: "lower-cost", TotalCost: 1},
			{ID: 4, Enabled: false, ChannelKey: "disabled", TotalCost: 0},
			{ID: 5, Enabled: true, ChannelKey: "", TotalCost: 0},
			{ID: 6, Enabled: true, ChannelKey: "old-429", StatusCode: 429, LastUseTimeStamp: now - int64(6*time.Minute/time.Second), TotalCost: 3},
		},
	}

	keys := channel.GetAvailableChannelKeys()
	if len(keys) != 3 {
		t.Fatalf("expected 3 available keys, got %d: %#v", len(keys), keys)
	}
	wantIDs := []int{3, 2, 6}
	for i, want := range wantIDs {
		if keys[i].ID != want {
			t.Fatalf("available key %d: got id %d, want %d", i, keys[i].ID, want)
		}
	}

	first := channel.GetChannelKey()
	if first.ID != 3 {
		t.Fatalf("expected lowest cost available key id 3, got %#v", first)
	}
}

func TestChannelGetAvailableChannelKeysQuarantines401(t *testing.T) {
	now := time.Now().Unix()
	channel := Channel{
		Keys: []ChannelKey{
			// Within the auth cooldown → quarantined (dead key, stop burning requests).
			{ID: 1, Enabled: true, ChannelKey: "recent-401", StatusCode: 401, LastUseTimeStamp: now - 60, TotalCost: 0},
			// Past the auth cooldown → self-heals back into rotation for a re-probe.
			{ID: 2, Enabled: true, ChannelKey: "old-401", StatusCode: 401, LastUseTimeStamp: now - int64(16*time.Minute/time.Second), TotalCost: 5},
			{ID: 3, Enabled: true, ChannelKey: "fresh", StatusCode: 0, TotalCost: 9},
		},
	}

	ids := map[int]bool{}
	for _, k := range channel.GetAvailableChannelKeys() {
		ids[k.ID] = true
	}
	if ids[1] {
		t.Fatalf("recent 401 key must be quarantined within the auth cooldown: %v", ids)
	}
	if !ids[2] {
		t.Fatalf("401 key past the auth cooldown must self-heal into rotation: %v", ids)
	}
	if !ids[3] {
		t.Fatalf("fresh key must be available: %v", ids)
	}
}

func TestKeyCooldownWindow401UsesAuthCooldown(t *testing.T) {
	if d, ok := keyCooldownWindow(401); !ok || d != ChannelKeyAuthErrorCooldown {
		t.Fatalf("401 cooldown = (%v, %v), want (%v, true)", d, ok, ChannelKeyAuthErrorCooldown)
	}
	// 403 stays on the per-model circuit-breaker path, not the key-wide cooldown, so a
	// key that 403s on one model is not blacked out for every model.
	if _, ok := keyCooldownWindow(403); ok {
		t.Fatalf("403 must NOT use the key-wide cooldown")
	}
}

func TestChannelGetAvailableChannelKeysCooldownIsStatusAware(t *testing.T) {
	now := time.Now().Unix()
	channel := Channel{
		Keys: []ChannelKey{
			// transient 5xx within the short (3s) window are skipped
			{ID: 1, Enabled: true, ChannelKey: "recent-503", StatusCode: 503, LastUseTimeStamp: now - 1, TotalCost: 0},
			{ID: 2, Enabled: true, ChannelKey: "recent-529", StatusCode: 529, LastUseTimeStamp: now - 1, TotalCost: 0},
			// a transient 5xx older than the 3s window is back (the short window passed)
			{ID: 3, Enabled: true, ChannelKey: "expired-503", StatusCode: 503, LastUseTimeStamp: now - 60, TotalCost: 5},
			// a recent 429 (within the 5s window) is skipped
			{ID: 4, Enabled: true, ChannelKey: "recent-429", StatusCode: 429, LastUseTimeStamp: now - 2, TotalCost: 9},
			{ID: 5, Enabled: true, ChannelKey: "fresh", StatusCode: 0, TotalCost: 1},
		},
	}

	keys := channel.GetAvailableChannelKeys()
	wantIDs := []int{5, 3} // fresh(cost1) then expired-503(cost5); 503/529 and 429 still cooling
	if len(keys) != len(wantIDs) {
		t.Fatalf("expected %d available keys, got %d: %#v", len(wantIDs), len(keys), keys)
	}
	for i, want := range wantIDs {
		if keys[i].ID != want {
			t.Fatalf("available key %d: got id %d, want %d", i, keys[i].ID, want)
		}
	}
}

// A thin (single-key) route must never be blacked out by a transient cooldown:
// if the only enabled key is cooling down, it is still returned so the circuit
// breaker / Retry-After govern recovery instead of "no available channel".
func TestChannelGetAvailableChannelKeysNeverBlacksOutSingleKey(t *testing.T) {
	now := time.Now().Unix()
	channel := Channel{
		Keys: []ChannelKey{
			{ID: 1, Enabled: true, ChannelKey: "only-key", StatusCode: 503, LastUseTimeStamp: now - 1, TotalCost: 0},
		},
	}
	keys := channel.GetAvailableChannelKeys()
	if len(keys) != 1 || keys[0].ID != 1 {
		t.Fatalf("a single cooled key must still be returned, got %#v", keys)
	}
}

// 同 key 优先 (sticky): the same keys must order by ID (ignoring cost) under the
// sticky strategy, but by cost under the default — proving the strategy switch works
// and that the default behaviour is unchanged (backward compatible).
func TestChannelGetAvailableChannelKeysStickyPrefersLowestID(t *testing.T) {
	build := func(strategy KeySelectStrategy) Channel {
		return Channel{
			KeySelectStrategy: strategy,
			Keys: []ChannelKey{
				{ID: 1, Enabled: true, ChannelKey: "primary", TotalCost: 100},
				{ID: 2, Enabled: true, ChannelKey: "cheaper", TotalCost: 1},
				{ID: 3, Enabled: true, ChannelKey: "cheapest", TotalCost: 0},
			},
		}
	}

	sticky := build(KeySelectStrategySticky)
	stickyKeys := sticky.GetAvailableChannelKeys()
	wantSticky := []int{1, 2, 3}
	if len(stickyKeys) != len(wantSticky) {
		t.Fatalf("sticky: expected %d keys, got %d: %#v", len(wantSticky), len(stickyKeys), stickyKeys)
	}
	for i, want := range wantSticky {
		if stickyKeys[i].ID != want {
			t.Fatalf("sticky key %d: got id %d, want %d (must order by id, not cost)", i, stickyKeys[i].ID, want)
		}
	}
	if first := sticky.GetChannelKey(); first.ID != 1 {
		t.Fatalf("sticky: expected lowest-id key 1 first, got %#v", first)
	}

	balanced := build(KeySelectStrategyCostBalanced)
	if first := balanced.GetChannelKey(); first.ID != 3 {
		t.Fatalf("cost-balanced default: expected cheapest key 3 first, got %#v", first)
	}
}

// Under sticky, when the primary (lowest-ID) key is cooling down it must yield to the
// next-lowest healthy key (still by ID, NOT cost), and reclaim priority once it heals.
func TestChannelGetAvailableChannelKeysStickyFallsThroughWhilePrimaryCools(t *testing.T) {
	now := time.Now().Unix()
	channel := Channel{
		KeySelectStrategy: KeySelectStrategySticky,
		Keys: []ChannelKey{
			{ID: 1, Enabled: true, ChannelKey: "primary-429", StatusCode: 429, LastUseTimeStamp: now - 2, TotalCost: 0},
			{ID: 2, Enabled: true, ChannelKey: "backup", StatusCode: 0, TotalCost: 50},
			{ID: 3, Enabled: true, ChannelKey: "backup2", StatusCode: 0, TotalCost: 0},
		},
	}
	keys := channel.GetAvailableChannelKeys()
	wantIDs := []int{2, 3} // primary cooling → next-lowest healthy id first, cost ignored
	if len(keys) != len(wantIDs) {
		t.Fatalf("expected %d available keys while primary cools, got %d: %#v", len(wantIDs), len(keys), keys)
	}
	for i, want := range wantIDs {
		if keys[i].ID != want {
			t.Fatalf("sticky fallthrough key %d: got id %d, want %d", i, keys[i].ID, want)
		}
	}
}

// singleCooledKeyChannel builds a channel whose only enabled key is currently within a
// transient (5xx) cooldown window — i.e. the all-keys-cooling fallback path.
func singleCooledKeyChannel(id int) Channel {
	return Channel{
		ID: id,
		Keys: []ChannelKey{
			{ID: 1, Enabled: true, ChannelKey: "only-key", StatusCode: 503, LastUseTimeStamp: time.Now().Unix() - 1, TotalCost: 0},
		},
	}
}

// R3c: when every key of a (single-key) channel is cooling down, the fallback that
// surfaces the just-failed key must be throttled to at most one probe per
// ChannelKeyProbeInterval — a burst of rapid requests must NOT all get the cooling key
// back and slam it. The first call probes; the immediately-following call gets nothing.
func TestChannelGetAvailableChannelKeysThrottlesCooldownFallbackProbe(t *testing.T) {
	defer restoreProbeInterval(ChannelKeyProbeInterval)
	ChannelKeyProbeInterval = time.Hour // large so the 2nd call is unambiguously throttled
	channel := singleCooledKeyChannel(990001)

	first := channel.GetAvailableChannelKeys()
	if len(first) != 1 || first[0].ID != 1 {
		t.Fatalf("first probe must surface the single cooling key, got %#v", first)
	}
	second := channel.GetAvailableChannelKeys()
	if len(second) != 0 {
		t.Fatalf("a second probe within the interval must be throttled to empty, got %#v", second)
	}
}

// After ChannelKeyProbeInterval elapses the route must re-admit a probe so a recovered
// key self-heals quickly instead of staying dark for the whole cooldown.
func TestChannelGetAvailableChannelKeysReadmitsProbeAfterInterval(t *testing.T) {
	defer restoreProbeInterval(ChannelKeyProbeInterval)
	ChannelKeyProbeInterval = 5 * time.Millisecond
	channel := singleCooledKeyChannel(990002)

	if first := channel.GetAvailableChannelKeys(); len(first) != 1 {
		t.Fatalf("first probe must be admitted, got %#v", first)
	}
	time.Sleep(20 * time.Millisecond) // > interval
	if again := channel.GetAvailableChannelKeys(); len(again) != 1 || again[0].ID != 1 {
		t.Fatalf("probe must be re-admitted after the interval elapses, got %#v", again)
	}
}

// GetChannelKey builds an auth header (model sync/discovery), not a hot-path request, so
// it must bypass the probe throttle entirely: it always returns the cooling key and never
// consumes a probe slot — no regression for that path even after a hot-path probe fired.
func TestChannelGetChannelKeyBypassesCooldownProbeThrottle(t *testing.T) {
	defer restoreProbeInterval(ChannelKeyProbeInterval)
	ChannelKeyProbeInterval = time.Hour
	channel := singleCooledKeyChannel(990003)

	// Consume the single hot-path probe slot for this channel.
	if got := channel.GetAvailableChannelKeys(); len(got) != 1 {
		t.Fatalf("setup: hot-path probe should be admitted once, got %#v", got)
	}
	// GetChannelKey must still return the real cooling key on every call.
	for i := 0; i < 3; i++ {
		if k := channel.GetChannelKey(); k.ID != 1 || k.ChannelKey != "only-key" {
			t.Fatalf("GetChannelKey call %d must bypass the throttle and return the cooling key, got %#v", i, k)
		}
	}
}

// The throttle must be concurrency-safe: under a burst of concurrent requests to a
// single-key channel whose only key is cooling, exactly ONE must be admitted (the rest
// get an empty set). Run under -race to prove the check-and-record is atomic.
func TestChannelGetAvailableChannelKeysConcurrentCooldownProbeAdmitsOne(t *testing.T) {
	defer restoreProbeInterval(ChannelKeyProbeInterval)
	ChannelKeyProbeInterval = time.Hour
	channel := singleCooledKeyChannel(990004)

	const goroutines = 64
	var admitted int64
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			<-start // release all at once to maximise contention
			if len(channel.GetAvailableChannelKeys()) > 0 {
				atomic.AddInt64(&admitted, 1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if admitted != 1 {
		t.Fatalf("exactly one concurrent probe must be admitted, got %d", admitted)
	}
}

// The relay's final-candidate last-resort path must surface a briefly-cooling sole key
// even after the hot-path probe slot is spent, so a single-route client's retry reaches
// the upstream instead of blacking out. GetAvailableChannelKeysLastResort bypasses the
// throttle entirely (like the auth-header path) and always returns the cooling key.
func TestChannelGetAvailableChannelKeysLastResortBypassesThrottle(t *testing.T) {
	defer restoreProbeInterval(ChannelKeyProbeInterval)
	ChannelKeyProbeInterval = time.Hour // any hot-path re-probe within the hour is throttled
	channel := singleCooledKeyChannel(990005)

	// Spend the single hot-path probe slot.
	if got := channel.GetAvailableChannelKeys(); len(got) != 1 {
		t.Fatalf("setup: hot-path probe should be admitted once, got %#v", got)
	}
	// Hot path is now throttled to empty...
	if got := channel.GetAvailableChannelKeys(); len(got) != 0 {
		t.Fatalf("hot path must be throttled to empty after the probe, got %#v", got)
	}
	// ...but the last-resort path still returns the cooling key on every call.
	for i := 0; i < 3; i++ {
		lr := channel.GetAvailableChannelKeysLastResort()
		if len(lr) != 1 || lr[0].ID != 1 {
			t.Fatalf("last-resort call %d must return the sole cooling key, got %#v", i, lr)
		}
	}
}

// restoreProbeInterval resets the package-level knob after a test that overrode it.
func restoreProbeInterval(d time.Duration) { ChannelKeyProbeInterval = d }
