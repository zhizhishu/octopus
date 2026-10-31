package balancer

import (
	"strings"
	"time"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
)

// DefaultSessionAffinityTTL is the built-in sticky window for a request that carries a
// real client session key (Claude Code metadata.user_id session, Codex session_id /
// prompt_cache_key, previous_response_id, the managed prompt-prefix fingerprint ...)
// when neither the group nor the fleet-wide session_keep_time_default configures one.
//
// Why it exists: access-plan routes synthesize their group on the fly with
// SessionKeepTime=0 and session_keep_time_default ships as 0, so in practice the
// session sticky layer was switched off for every plan-routed request. With 轮询
// (mode 1/6) each turn of one conversation then rotated to another channel / account
// and the upstream prompt cache built on the previous one was never read again.
// Upstream prompt caches live for 5 min – 1 h, so one hour (refreshed on every
// successful turn) keeps a conversation on the account that holds its cache, the same
// window sub2api uses for its session -> account binding.
//
// Requests WITHOUT a client session key keep the legacy behaviour exactly: the
// apiKey+model bucket is shared by unrelated requests, so it never gets this default.
const DefaultSessionAffinityTTL = time.Hour

// sessionKeepTimeDefault returns the global fallback sticky window (seconds) applied
// when a group's own SessionKeepTime is unset (<=0). It reads the cached
// session_keep_time_default setting so an admin can turn sticky on fleet-wide without
// editing every group; 0 (or unset / parse error) means no global default, preserving
// the legacy per-group-only behavior.
//
// Mirrors circuit.go's getThreshold(): a direct cached-setting read with a graceful
// fallback. Kept as a func var so balancer unit tests can override it (like
// DecisionLogHook) without wiring up a settings cache.
var sessionKeepTimeDefault = func() int {
	v, err := op.SettingGetInt(model.SettingKeySessionKeepTimeDefault)
	if err != nil || v <= 0 {
		return 0
	}
	return v
}

// sessionAffinityEnabled reads the session_affinity_enabled escape hatch (default
// true). false restores the old "no window configured = no sticky" behaviour for
// session-keyed requests. A missing / unparsable value counts as enabled. Func var so
// unit tests can flip it without a settings cache.
var sessionAffinityEnabled = func() bool {
	v, err := op.SettingGetBool(model.SettingKeySessionAffinityEnabled)
	if err != nil {
		return true
	}
	return v
}

// effectiveStickyTTL resolves the sticky window for one routing decision:
//  1. the group's own SessionKeepTime when set (>0);
//  2. else the fleet-wide session_keep_time_default when set (>0);
//  3. else, only for a request that carries a client session key, the built-in
//     DefaultSessionAffinityTTL (unless session_affinity_enabled=false);
//  4. else 0 = sticky off (legacy).
func effectiveStickyTTL(groupKeepTime int, clientSessionKey string) time.Duration {
	if groupKeepTime > 0 {
		return time.Duration(groupKeepTime) * time.Second
	}
	if v := sessionKeepTimeDefault(); v > 0 {
		return time.Duration(v) * time.Second
	}
	if strings.TrimSpace(clientSessionKey) != "" && sessionAffinityEnabled() {
		return DefaultSessionAffinityTTL
	}
	return 0
}
