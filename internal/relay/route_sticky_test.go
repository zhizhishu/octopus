package relay

import (
	"testing"

	dbmodel "github.com/bestruirui/octopus/internal/model"
)

// TestRouteStickyEnabled 锁定「分组模式 + 会话来源」→ 是否 sticky 的分级矩阵。
// 这是修「轮询不轮询(同会话永远粘一个渠道)」的核心闸门：轮询模式下纯优化型会话放开、
// 正确性关键会话保留；填充优先模式全程保留。
func TestRouteStickyEnabled(t *testing.T) {
	cases := []struct {
		name   string
		mode   dbmodel.GroupMode
		source string
		want   bool
	}{
		// 填充优先(FillFirst=Failover)：任何来源都 sticky（cache 命中优先，语义不变）。
		{"fillfirst_promptcache", dbmodel.GroupModeFillFirst, "body:prompt_cache_key", true},
		{"fillfirst_fingerprint", dbmodel.GroupModeFillFirst, "octopus:request_fingerprint", true},
		{"fillfirst_prev", dbmodel.GroupModeFillFirst, "body:previous_response_id", true},
		{"fillfirst_empty", dbmodel.GroupModeFillFirst, "", true},

		// 轮询(RoundRobin=Spread)：纯优化型来源放开(不 sticky)，让请求真正在同优先级渠道轮转。
		{"rr_promptcache", dbmodel.GroupModeRoundRobin, "body:prompt_cache_key", false},
		{"rr_fingerprint", dbmodel.GroupModeRoundRobin, "octopus:request_fingerprint", false},
		{"rr_user", dbmodel.GroupModeRoundRobin, "body:user", false},
		{"rr_safety", dbmodel.GroupModeRoundRobin, "body:safety_identifier", false},
		{"rr_empty", dbmodel.GroupModeRoundRobin, "", false},

		// 轮询：正确性关键来源保留 sticky(换渠道会让 codex/claude 多轮崩)。
		{"rr_prev_response", dbmodel.GroupModeRoundRobin, "body:previous_response_id", true},
		{"rr_header_thread", dbmodel.GroupModeRoundRobin, "header:AH-Thread-Id", true},
		{"rr_header_session", dbmodel.GroupModeRoundRobin, "header:Session_id", true},
		{"rr_meta_session", dbmodel.GroupModeRoundRobin, "metadata:user_id:claude-session", true},
		{"rr_client_meta", dbmodel.GroupModeRoundRobin, "client_metadata:session_id", true},
		{"rr_anthropic_session", dbmodel.GroupModeRoundRobin, "metadata:user_id", true},

		// 其它负载均衡模式(smart/weighted/random)与轮询同语义。
		{"smart_promptcache", dbmodel.GroupModeSmart, "body:prompt_cache_key", false},
		{"smart_prev", dbmodel.GroupModeSmart, "body:previous_response_id", true},
		{"weighted_fingerprint", dbmodel.GroupModeWeighted, "octopus:request_fingerprint", false},
		{"random_prev", dbmodel.GroupModeRandom, "body:previous_response_id", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := routeStickyEnabled(tc.mode, tc.source); got != tc.want {
				t.Fatalf("routeStickyEnabled(%v, %q) = %v, want %v", tc.mode, tc.source, got, tc.want)
			}
		})
	}
}

// TestIsCorrectnessCriticalSessionSource 单独锁定「纯优化型 vs 正确性关键」来源分类，
// 防止以后有人往 route_session_key.go 里加新 Source 忘了归类。
func TestIsCorrectnessCriticalSessionSource(t *testing.T) {
	optimizationOnly := []string{
		"",
		"body:prompt_cache_key",
		"body:user",
		"body:safety_identifier",
		"octopus:request_fingerprint",
	}
	for _, s := range optimizationOnly {
		if isCorrectnessCriticalSessionSource(s) {
			t.Fatalf("source %q should be optimization-only (false), got true", s)
		}
	}

	correctnessCritical := []string{
		"body:previous_response_id",
		"header:AH-Thread-Id",
		"header:AH-Trace-Id",
		"header:Session_id",
		"header:Conversation_id",
		"metadata:thread_id",
		"metadata:user_id",
		"metadata:user_id:claude-session",
		"client_metadata:window_id",
		"client_metadata:session_id",
	}
	for _, s := range correctnessCritical {
		if !isCorrectnessCriticalSessionSource(s) {
			t.Fatalf("source %q should be correctness-critical (true), got false", s)
		}
	}
}
