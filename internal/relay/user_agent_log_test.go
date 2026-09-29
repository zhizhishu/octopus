package relay

import (
	"strings"
	"testing"
)

// TestTruncateUserAgentForLog locks the client-UA snapshot contract: trimmed,
// rune-safe, capped at 512 with no suffix marker (the frontend parses the raw
// UA for client identification, so the stored value must stay clean), and empty
// stays empty so old rows without the column degrade to "no badge".
func TestTruncateUserAgentForLog(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"whitespace only", "   \t ", ""},
		{"plain ua kept", "claude-cli/2.1.284 (external, sdk-cli)", "claude-cli/2.1.284 (external, sdk-cli)"},
		{"surrounding spaces trimmed", "  codex_cli_rs/0.156.1  ", "codex_cli_rs/0.156.1"},
		{"ascii over cap", strings.Repeat("a", 600), strings.Repeat("a", 512)},
		{"multibyte rune-safe", strings.Repeat("界", 600), strings.Repeat("界", 512)},
		{"exactly at cap", strings.Repeat("b", 512), strings.Repeat("b", 512)},
		{"under cap untouched", strings.Repeat("c", 511), strings.Repeat("c", 511)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateUserAgentForLog(tc.in)
			if got != tc.want {
				t.Fatalf("truncateUserAgentForLog(%q) = %q (len %d), want %q (len %d)",
					tc.in[:min(len(tc.in), 40)], got, len(got), tc.want[:min(len(tc.want), 40)], len(tc.want))
			}
		})
	}
}
