package model

import "testing"

func TestCodexOriginatorFromUserAgent(t *testing.T) {
	cases := []struct {
		name string
		ua   string
		want string
	}{
		{"codex_exec", "codex_exec/0.161.0 (Mac OS X 15.0; arm64) unknown (codex_exec; 0.161.0)", "codex_exec"},
		{"codex_cli_rs", "codex_cli_rs/0.163.0 (Ubuntu 24.04.1; x86_64) unknown (codex_cli_rs; 0.163.0)", CodexOriginatorRustCLI},
		{"mixed case exec", "Codex_EXEC/9.9.9", "codex_exec"},
		{"mixed case cli", "Codex_CLI_RS/1.2.3 (Mac)", CodexOriginatorRustCLI},
		{"bare exec token", "codex_exec", "codex_exec"},
		{"codex-tui rewritten to rust cli", "codex-tui/0.161.0 (Mac OS X 15.0; arm64) unknown (codex-tui; 0.161.0)", CodexOriginatorRustCLI},
		{"mixed case tui", "Codex-TUI/1.0", CodexOriginatorRustCLI},
		{"bare tui token", "codex-tui", CodexOriginatorRustCLI},
		{"non-codex", "curl/8.0.0", ""},
		{"non-codex browser", "Mozilla/5.0 (X11; Linux x86_64) Chrome/120.0.0.0", ""},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CodexOriginatorFromUserAgent(tc.ua); got != tc.want {
				t.Fatalf("CodexOriginatorFromUserAgent(%q) = %q, want %q", tc.ua, got, tc.want)
			}
		})
	}
}

func TestDeprecatedTUIUserAgentDetection(t *testing.T) {
	if !CodexUserAgentIsDeprecatedTUI("  Codex-TUI/0.1 ") {
		t.Fatal("mixed-case codex-tui must be detected")
	}
	if CodexUserAgentIsDeprecatedTUI("codex_cli_rs/0.163.0") || CodexUserAgentIsDeprecatedTUI("curl/8") {
		t.Fatal("non-tui UA must not be treated as the deprecated family")
	}
}
