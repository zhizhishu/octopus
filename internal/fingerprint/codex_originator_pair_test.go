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

// 显式 Originator 与 UA 首 token 一致时原样保留。
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

// 显式 Originator 与即将发出的 UA 首 token 不一致时，改成那个 token，不把错配发出去。
// 保存路径会先改库；这里是解析时的第二道，存量错配也不会上线。
func TestCodexOriginatorCorrectsMismatchedProfilePair(t *testing.T) {
	cases := []struct {
		name      string
		ua        string
		origin    string
		wantToken string
	}{
		{"exec UA with cli originator", "codex_exec/1.0.0", "codex_cli_rs", "codex_exec"},
		{"cli UA with exec originator", "codex_cli_rs/0.163.0 (Ubuntu)", "codex_exec", "codex_cli_rs"},
		{"case differs from the lowercase token", "Codex_CLI_RS/1.2.3", "codex_exec", "codex_cli_rs"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := Resolved{hasProfile: true, profile: dbmodel.FingerprintProfile{
				CodexUserAgent:  tc.ua,
				CodexOriginator: tc.origin,
			}}
			if got := r.CodexOriginator(); got != tc.wantToken {
				t.Fatalf("CodexOriginator() = %q, want %q (mismatched profile originator must follow the UA)", got, tc.wantToken)
			}
			first, _, _ := strings.Cut(strings.ToLower(r.CodexUserAgent()), "/")
			if got := r.CodexOriginator(); got != first {
				t.Fatalf("corrected Originator %q still disagrees with UA token %q", got, first)
			}
		})
	}
}

// 推导必须和 model.CodexOriginatorFromUserAgent（op 保存、relay 出站用的同一份）一致。
// codex-tui 不取自己的首 token，改成 Rust CLI 的 codex_cli_rs，并且发出去的 UA 也换成那一对。
// 非 codex UA 的推导是空：显式 Originator 原样保留。
func TestCodexOriginatorDerivationMatchesModel(t *testing.T) {
	cases := []struct {
		ua   string
		want string
	}{
		{"codex_exec/0.161.0 (Mac OS X 15.0; arm64)", "codex_exec"},
		{"codex_cli_rs/0.163.0 (Ubuntu 24.04.1; x86_64)", dbmodel.CodexOriginatorRustCLI},
		{"Codex_CLI_RS/1.2.3 (Mac)", dbmodel.CodexOriginatorRustCLI},
		{"codex-tui/0.161.0 (Mac OS X 15.0; arm64) unknown (codex-tui; 0.161.0)", dbmodel.CodexOriginatorRustCLI},
		{"Codex-TUI/1.0", dbmodel.CodexOriginatorRustCLI},
		{"curl/8.0.0", ""},
		{"", ""},
	}
	for _, tc := range cases {
		t.Run(tc.ua, func(t *testing.T) {
			if got := codexOriginatorFromUserAgent(tc.ua); got != tc.want || got != dbmodel.CodexOriginatorFromUserAgent(tc.ua) {
				t.Fatalf("derivation %q = %q, model = %q, want %q", tc.ua, got, dbmodel.CodexOriginatorFromUserAgent(tc.ua), tc.want)
			}
		})
	}
}

func TestCodexOriginatorRewritesDeprecatedTUIToRustCLI(t *testing.T) {
	r := Resolved{hasProfile: true, profile: dbmodel.FingerprintProfile{
		CodexUserAgent:  "codex-tui/0.161.0 (Mac OS X 15.0; arm64) unknown (codex-tui; 0.161.0)",
		CodexOriginator: "codex-tui",
	}}
	if got := r.CodexUserAgent(); got != dbmodel.DefaultCodexHeaderUserAgent {
		t.Fatalf("emitted UA = %q, want the Rust CLI default", got)
	}
	if got := r.CodexOriginator(); got != dbmodel.CodexOriginatorRustCLI {
		t.Fatalf("CodexOriginator() = %q, want %q", got, dbmodel.CodexOriginatorRustCLI)
	}
	first, _, _ := strings.Cut(strings.ToLower(r.CodexUserAgent()), "/")
	if r.CodexOriginator() != first {
		t.Fatalf("rewritten pair split: originator %q ua token %q", r.CodexOriginator(), first)
	}
}

func TestCodexOriginatorLeavesNonCodexExplicitValue(t *testing.T) {
	r := Resolved{hasProfile: true, profile: dbmodel.FingerprintProfile{
		CodexUserAgent:  "curl/8.0.0",
		CodexOriginator: "keep-me",
	}}
	if got := r.CodexOriginator(); got != "keep-me" {
		t.Fatalf("non-codex UA must not rewrite Originator, got %q", got)
	}
}
