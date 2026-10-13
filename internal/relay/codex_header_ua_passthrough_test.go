package relay

import (
	"net/http"
	"testing"
)

// TestApplyCodexHeaderDefaultsUserAgentPassthrough locks the codex UA passthrough: a
// downstream that presents a genuine Codex CLI user-agent keeps its EXACT Originator+UA pair
// on the outbound (so the upstream sees the bytes a direct connection would have carried),
// while any other downstream (no CLI, or a foreign UA) falls back to the profile's codex_exec
// pair, preserving the sub2api #3901 originator↔UA-first-token pairing rule.
func TestApplyCodexHeaderDefaultsUserAgentPassthrough(t *testing.T) {
	fp := resolveFingerprintForChannel(nil)
	const (
		codexExecUA = "codex_exec/0.161.0 (Mac OS X 15.0; arm64) unknown (codex_exec; 0.161.0)"
		codexTuiUA  = "codex-tui/0.161.0 (Mac OS X 15.0; arm64) unknown (codex-tui; 0.161.0)"
		codexRsUA   = "codex_cli_rs/0.161.0 (Ubuntu 24.04.1; x86_64) unknown (codex_cli_rs; 0.161.0)"
	)

	cases := []struct {
		name              string
		inboundUserAgent  string
		inboundOriginator string
		wantUserAgent     string
		wantOriginator    string
	}{
		{
			name:              "codex_exec prefix is echoed verbatim",
			inboundUserAgent:  codexExecUA,
			inboundOriginator: "codex_exec",
			wantUserAgent:     codexExecUA,
			wantOriginator:    "codex_exec",
		},
		{
			name:              "codex_cli_rs prefix is echoed verbatim",
			inboundUserAgent:  codexRsUA,
			inboundOriginator: "codex_cli_rs",
			wantUserAgent:     codexRsUA,
			wantOriginator:    "codex_cli_rs",
		},
		{
			name:              "codex-tui prefix is echoed verbatim",
			inboundUserAgent:  codexTuiUA,
			inboundOriginator: "codex-tui",
			wantUserAgent:     codexTuiUA,
			wantOriginator:    "codex-tui",
		},
		{
			name:              "codex family with empty originator falls back to profile originator",
			inboundUserAgent:  codexExecUA,
			inboundOriginator: "",
			wantUserAgent:     codexExecUA,
			wantOriginator:    fp.CodexOriginator(),
		},
		{
			name:              "non-codex UA falls back to profile pair",
			inboundUserAgent:  "python-requests/2.32.0",
			inboundOriginator: "python",
			wantUserAgent:     fp.CodexUserAgent(),
			wantOriginator:    fp.CodexOriginator(),
		},
		{
			name:              "empty UA falls back to profile pair",
			inboundUserAgent:  "",
			inboundOriginator: "",
			wantUserAgent:     fp.CodexUserAgent(),
			wantOriginator:    fp.CodexOriginator(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, "https://upstream.example/v1/responses", nil)
			if err != nil {
				t.Fatalf("create request: %v", err)
			}
			applyCodexHeaderDefaultsWithFingerprint(req, nil, fp, tc.inboundUserAgent, tc.inboundOriginator)
			if got := req.Header.Get("User-Agent"); got != tc.wantUserAgent {
				t.Fatalf("User-Agent = %q, want %q", got, tc.wantUserAgent)
			}
			if got := req.Header.Get("Originator"); got != tc.wantOriginator {
				t.Fatalf("Originator = %q, want %q", got, tc.wantOriginator)
			}
		})
	}
}

// TestApplyCodexHeaderDefaultsEmitsNoConnectionHeader locks the removal of the synthesized
// `Connection: Keep-Alive`: a genuine Codex CLI (reqwest/hyper) never emits it, and keep-alive
// is the HTTP/1.1 default, so oct must not add one either.
func TestApplyCodexHeaderDefaultsEmitsNoConnectionHeader(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://upstream.example/v1/responses", nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	applyCodexHeaderDefaultsWithFingerprint(req, nil, resolveFingerprintForChannel(nil), "", "")
	if got := req.Header.Get("Connection"); got != "" {
		t.Fatalf("codex path must not synthesize a Connection header, got %q", got)
	}
}

// TestCodexIdentityPairEchoesGenuinePair is a direct, settings-independent pin of the pairing
// rule the passthrough relies on (guards against a future edit splitting UA and Originator).
func TestCodexIdentityPairEchoesGenuinePair(t *testing.T) {
	fp := resolveFingerprintForChannel(nil)
	const codexUA = "codex_exec/0.161.0 (Mac OS X 15.0; arm64) unknown (codex_exec; 0.161.0)"

	if ua, originator := codexIdentityPair(codexUA, "codex_exec", fp); ua != codexUA || originator != "codex_exec" {
		t.Fatalf("genuine pair must be echoed verbatim, got ua=%q originator=%q", ua, originator)
	}
	if ua, originator := codexIdentityPair("curl/8.0", "curl", fp); ua != fp.CodexUserAgent() || originator != fp.CodexOriginator() {
		t.Fatalf("non-codex UA must fall back to the profile pair, got ua=%q originator=%q", ua, originator)
	}
	if _, originator := codexIdentityPair(codexUA, "", fp); originator != fp.CodexOriginator() {
		t.Fatalf("empty inbound originator must fall back to %q, got %q", fp.CodexOriginator(), originator)
	}
}
