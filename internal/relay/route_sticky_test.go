package relay

import (
	"testing"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
)

// TestRouteStickyEnabled 锁定「分组模式 + 会话来源」→ 是否 sticky 的分级矩阵。
// 这是修「轮询不轮询(同会话永远粘一个渠道)」的核心闸门：轮询模式下纯优化型会话放开、
// 正确性关键会话保留；填充优先模式全程保留。
// 本用例不初始化 DB/设置缓存，跑的正是「读不到 route_sticky_cache_first → 回落默认关」
// 那条路径，因此它同时是「缓存优先开关缺席时行为不变」的回归。
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

// TestRouteStickyCacheFirstDefaultsOff 坐实出厂默认：DefaultSettings 播种进 DB + 设置缓存后，
// route_sticky_cache_first 必须是 false——「新开关默认关、老部署升级后选路行为一个字节不变」
// 全靠这一条。
func TestRouteStickyCacheFirstDefaultsOff(t *testing.T) {
	setupRelayErrorDB(t)

	enabled, err := op.SettingGetBool(dbmodel.SettingKeyRouteStickyCacheFirst)
	if err != nil {
		t.Fatalf("read %s from seeded defaults: %v", dbmodel.SettingKeyRouteStickyCacheFirst, err)
	}
	if enabled {
		t.Fatalf("%s must default to false (分摊优先), got true", dbmodel.SettingKeyRouteStickyCacheFirst)
	}
	if routeStickyCacheFirstEnabled() {
		t.Fatalf("routeStickyCacheFirstEnabled() must be false with the shipped default")
	}
}

// TestRouteStickyEnabledCacheFirstSwitch 表驱动覆盖「分组模式 × 会话来源 × 缓存优先开关」。
// off 那半张表是现行为回归（必须与开关引入前逐位一致）；on 那半张表锁新语义：轮询等模式下
// **非空**的纯优化型来源改为保留 sticky 换 prompt-cache 命中，而空来源与 FillFirst 不变。
// legacyMode 用已退役的 random(2)——GetBalancer 把未知模式折叠成 Spread，选路闸门也必须
// 一视同仁（只有 FillFirst 才是那条"全程 sticky"的分支）。
func TestRouteStickyEnabledCacheFirstSwitch(t *testing.T) {
	setupRelayErrorDB(t)
	// 先于 setupRelayErrorDB 的 db.Close 清理执行(Cleanup 后进先出)，把全局设置缓存复位，
	// 免得开关的 true 泄漏给同包后续用例。
	t.Cleanup(func() {
		if err := op.SettingSetString(dbmodel.SettingKeyRouteStickyCacheFirst, "false"); err != nil {
			t.Fatalf("restore %s to false: %v", dbmodel.SettingKeyRouteStickyCacheFirst, err)
		}
	})

	const legacyMode = dbmodel.GroupMode(2) // 已退役的 random，折叠成 Spread

	cases := []struct {
		name       string
		mode       dbmodel.GroupMode
		source     string
		cacheFirst bool
		want       bool
	}{
		// ---- 开关关(默认)：与开关引入前完全一致 ----
		{"off_rr_promptcache", dbmodel.GroupModeRoundRobin, "body:prompt_cache_key", false, false},
		{"off_rr_fingerprint", dbmodel.GroupModeRoundRobin, "octopus:request_fingerprint", false, false},
		{"off_rr_user", dbmodel.GroupModeRoundRobin, "body:user", false, false},
		{"off_rr_safety", dbmodel.GroupModeRoundRobin, "body:safety_identifier", false, false},
		{"off_rr_empty", dbmodel.GroupModeRoundRobin, "", false, false},
		{"off_rr_prev_response", dbmodel.GroupModeRoundRobin, "body:previous_response_id", false, true},
		{"off_rr_header_thread", dbmodel.GroupModeRoundRobin, "header:AH-Thread-Id", false, true},
		{"off_rr_meta_session", dbmodel.GroupModeRoundRobin, "metadata:user_id:claude-session", false, true},
		{"off_legacy_promptcache", legacyMode, "body:prompt_cache_key", false, false},
		{"off_legacy_prev_response", legacyMode, "body:previous_response_id", false, true},
		{"off_fillfirst_promptcache", dbmodel.GroupModeFillFirst, "body:prompt_cache_key", false, true},
		{"off_fillfirst_empty", dbmodel.GroupModeFillFirst, "", false, true},

		// ---- 开关开：轮询类模式下非空的纯优化型来源改为保留 sticky ----
		{"on_rr_promptcache", dbmodel.GroupModeRoundRobin, "body:prompt_cache_key", true, true},
		{"on_rr_fingerprint", dbmodel.GroupModeRoundRobin, "octopus:request_fingerprint", true, true},
		{"on_rr_user", dbmodel.GroupModeRoundRobin, "body:user", true, true},
		{"on_rr_safety", dbmodel.GroupModeRoundRobin, "body:safety_identifier", true, true},
		// 空来源=拿不到任何会话键，粘性桶会被互不相干的请求共用，开关开也不粘。
		{"on_rr_empty", dbmodel.GroupModeRoundRobin, "", true, false},
		{"on_rr_prev_response", dbmodel.GroupModeRoundRobin, "body:previous_response_id", true, true},
		{"on_rr_header_thread", dbmodel.GroupModeRoundRobin, "header:AH-Thread-Id", true, true},
		{"on_rr_meta_session", dbmodel.GroupModeRoundRobin, "metadata:user_id:claude-session", true, true},
		{"on_legacy_promptcache", legacyMode, "body:prompt_cache_key", true, true},
		{"on_legacy_empty", legacyMode, "", true, false},
		// FillFirst 两档都全程 sticky，开关碰不到它。
		{"on_fillfirst_promptcache", dbmodel.GroupModeFillFirst, "body:prompt_cache_key", true, true},
		{"on_fillfirst_empty", dbmodel.GroupModeFillFirst, "", true, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			value := "false"
			if tc.cacheFirst {
				value = "true"
			}
			if err := op.SettingSetString(dbmodel.SettingKeyRouteStickyCacheFirst, value); err != nil {
				t.Fatalf("set %s=%s: %v", dbmodel.SettingKeyRouteStickyCacheFirst, value, err)
			}
			if got := routeStickyEnabled(tc.mode, tc.source); got != tc.want {
				t.Fatalf("routeStickyEnabled(%v, %q) with cache_first=%v = %v, want %v",
					tc.mode, tc.source, tc.cacheFirst, got, tc.want)
			}
		})
	}
}

// TestRouteStickyCacheFirstSettingValidates 锁住新键的取值面：只收 true/false，
// 免得管理员写个 "1"/"on" 静默存进去后 SettingGetBool 解析失败、开关看着开实际关。
func TestRouteStickyCacheFirstSettingValidates(t *testing.T) {
	for _, value := range []string{"true", "false"} {
		s := dbmodel.Setting{Key: dbmodel.SettingKeyRouteStickyCacheFirst, Value: value}
		if err := s.Validate(); err != nil {
			t.Fatalf("value %q should be accepted, got %v", value, err)
		}
	}
	for _, value := range []string{"", "1", "on", "TRUE", "yes"} {
		s := dbmodel.Setting{Key: dbmodel.SettingKeyRouteStickyCacheFirst, Value: value}
		if err := s.Validate(); err == nil {
			t.Fatalf("value %q should be rejected, got nil error", value)
		}
	}
}
