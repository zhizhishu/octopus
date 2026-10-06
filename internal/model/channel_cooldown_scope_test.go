package model

import (
	"testing"
	"time"
)

// The key cooldown is deliberately KEY-scoped, not model-scoped: the cooling state
// lives on the ChannelKey (StatusCode + LastUseTimeStamp) and key selection carries
// no model dimension, so a rate-limit seen while serving model A also skips that key
// for model B on the same key for the duration of the short window below.
//
// That is intentional, not an oversight. These windows are only a few seconds — just
// long enough for the caller's own retry to land right after them, like a direct
// upstream connection — while genuine per-model isolation is provided by the
// per-model circuit breaker (keyed channelID:keyID:modelName in internal/relay/balancer).
// This test pins BOTH halves of that split so nobody "fixes" one side into the other.
func TestKeyCooldownScopeIsKeyWideAndDeliberatelyShort(t *testing.T) {
	// Scope mapping: credential failure is the only long, explicitly key-wide case;
	// rate-limit and shared-upstream-transient failures get short windows.
	cases := []struct {
		status    int
		want      time.Duration
		wantApply bool
		why       string
	}{
		{401, 15 * time.Minute, true, "credential failure affects every model on the key"},
		{429, 5 * time.Second, true, "rate limit: short shared window"},
		{503, 3 * time.Second, true, "shared upstream transient"},
		{502, 3 * time.Second, true, "shared upstream transient"},
		{404, 0, false, "not a cooldown-worthy status"},
		{200, 0, false, "healthy"},
	}
	for _, tc := range cases {
		got, ok := keyCooldownWindow(tc.status)
		if ok != tc.wantApply || got != tc.want {
			t.Fatalf("keyCooldownWindow(%d) = (%s, %v), want (%s, %v): %s",
				tc.status, got, ok, tc.want, tc.wantApply, tc.why)
		}
	}

	// Short-window guard against the old 60s bench: a transient rate limit must clear
	// within seconds so a CLI's native retry succeeds instead of seeing a wall of
	// "no available channel" that outlives its own retry budget.
	if window, _ := keyCooldownWindow(429); window > 10*time.Second {
		t.Fatalf("429 cooldown %s is long enough to outlive a client retry budget", window)
	}
	if window, _ := keyCooldownWindow(503); window > 10*time.Second {
		t.Fatalf("503 cooldown %s is long enough to outlive a client retry budget", window)
	}

	// The cross-model consequence, stated as an assertion: the same ChannelKey carries
	// the cooldown regardless of which model the request targets, because selection has
	// no model dimension to filter on. Unique channel ID: the cooldown-probe throttle is
	// package-level state keyed by channel ID.
	now := time.Now().Unix()
	channel := Channel{
		ID: 987654,
		Keys: []ChannelKey{
			{ID: 1, Enabled: true, ChannelKey: "just-429", StatusCode: 429, LastUseTimeStamp: now - 1},
		},
	}
	// Hot path: a cooling key is not handed out freely — one probe per interval, so a
	// concurrent burst cannot all slam the key that just said "slow down".
	if keys := channel.GetAvailableChannelKeys(); len(keys) != 1 || keys[0].ID != 1 {
		t.Fatalf("expected a single cooling-probe key on the first hot-path call, got %#v", keys)
	}
	if keys := channel.GetAvailableChannelKeys(); len(keys) != 0 {
		t.Fatalf("cooldown probe must be throttled for immediate repeated calls, got %#v", keys)
	}
	// ...but a route with no peer to spill onto still never blacks out: the last-resort
	// path surfaces the cooling key so the request reaches upstream instead of failing
	// locally as "no available channel".
	if keys := channel.GetAvailableChannelKeysLastResort(); len(keys) != 1 || keys[0].ID != 1 {
		t.Fatalf("single-key route must still reach upstream while cooling, got %#v", keys)
	}
}
