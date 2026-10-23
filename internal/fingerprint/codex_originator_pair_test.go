package fingerprint

import (
	"strings"
	"testing"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
)

// Originator 必须与 User-Agent 同家族、同首 token。
//
// 上游的判据是「Originator == UA 首个 "/" 之前那段」（header_defaults.go 引的
// sub2api #3901）。配对一拆，整条 codex 渠道被判伪造指纹而全量 400/403，
// 而且只有**改过 UA** 的实例中招，默认实例完全正常 —— 排查方向会被带偏,
// 用户只会觉得"这个渠道坏了"，不会想到是自己改过 UA。
//
// 这里的核心不变式：CodexOriginator() 的结果，永远等于 CodexUserAgent() 的首 token。
func TestCodexOriginatorFollowsUserAgent(t *testing.T) {
	cases := []struct {
		name   string
		ua     string
		origin string
	}{
		{"默认家族", "codex_cli_rs/0.163.0 (Windows 11; x86_64) unknown (codex_cli_rs; 0.163.0)", "codex_cli_rs"},
		{"codex_exec 家族", "codex_exec/0.163.0 (Windows 11; x86_64) unknown (codex_exec; 0.163.0)", "codex_exec"},
		{"大写也要归一", "Codex_CLI_RS/1.2.3 (Mac)", "codex_cli_rs"},
		{"无版本后缀", "codex_exec", "codex_exec"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var r Resolved
			r = Resolved{hasProfile: true, profile: dbmodel.FingerprintProfile{CodexUserAgent: tc.ua}}

			got := r.CodexOriginator()
			if got != tc.origin {
				t.Errorf("CodexOriginator() = %q, want %q（UA 是 %q）",
					got, tc.origin, tc.ua)
			}
			// 不变式：永远等于 UA 首 token。这条比逐个 case 更能防回归。
			firstToken, _, _ := strings.Cut(strings.ToLower(r.CodexUserAgent()), "/")
			if got != firstToken {
				t.Errorf("配对被拆开：UA 首 token = %q 但 Originator = %q；"+
					"上游会判伪造指纹并拒绝整条渠道", firstToken, got)
			}
		})
	}
}

// 显式配了 originator 就以它为准（不覆盖用户明确设置）。
func TestCodexOriginatorHonoursExplicitProfileValue(t *testing.T) {
	var r Resolved
	r = Resolved{hasProfile: true, profile: dbmodel.FingerprintProfile{
		CodexUserAgent:  "codex_exec/1.0.0",
		CodexOriginator: "codex_exec",
	}}
	if got := r.CodexOriginator(); got != "codex_exec" {
		t.Fatalf("显式配置被忽略：got %q", got)
	}
}
