package relay

import (
	"net/http"
	"strings"
	"testing"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
)

// TestApplyCodexHeaderDefaultsUserAgentPassthrough locks the codex UA passthrough: a
// downstream that presents a genuine Codex CLI user-agent keeps its EXACT UA on the outbound
// (so the upstream sees the bytes a direct connection would have carried) and its Originator
// is derived from that UA's leading token, so the pair can never disagree. The interactive-TUI
// family is the one exception: it is rewritten to the profile pair because an upstream pairs
// the token against the Originator and that identity must never be published. Any other
// downstream (no CLI, or a foreign UA) falls back to the profile's pair, preserving the
// sub2api #3901 originator↔UA-first-token pairing rule.
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
			name:              "codex-tui is rewritten to the profile pair, never echoed",
			inboundUserAgent:  codexTuiUA,
			inboundOriginator: "codex-tui",
			wantUserAgent:     fp.CodexUserAgent(),
			wantOriginator:    fp.CodexOriginator(),
		},
		{
			name:              "codex family with empty originator derives it from the UA token",
			inboundUserAgent:  codexExecUA,
			inboundOriginator: "",
			wantUserAgent:     codexExecUA,
			wantOriginator:    "codex_exec",
		},
		{
			name:              "rs UA with a leaked codex_exec originator is repaired, not sent",
			inboundUserAgent:  codexRsUA,
			inboundOriginator: "codex_exec",
			wantUserAgent:     codexRsUA,
			wantOriginator:    "codex_cli_rs",
		},
		{
			name:              "rs UA with no originator yields the pairing originator",
			inboundUserAgent:  codexRsUA,
			inboundOriginator: "",
			wantUserAgent:     codexRsUA,
			wantOriginator:    "codex_cli_rs",
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
			applyCodexHeaderDefaultsWithFingerprint(req, nil, fp, tc.inboundUserAgent)
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
	applyCodexHeaderDefaultsWithFingerprint(req, nil, resolveFingerprintForChannel(nil), "")
	if got := req.Header.Get("Connection"); got != "" {
		t.Fatalf("codex path must not synthesize a Connection header, got %q", got)
	}
}

// TestCodexIdentityPairKeepsUserAgentAndOriginatorPaired is the settings-independent pin of the
// rule the passthrough relies on: whatever Originator is emitted must be the leading token of
// the emitted User-Agent, so the two headers can never be split. It also forbids publishing the
// interactive-TUI identity at all.
func TestCodexIdentityPairKeepsUserAgentAndOriginatorPaired(t *testing.T) {
	fp := resolveFingerprintForChannel(nil)
	const codexUA = "codex_exec/0.161.0 (Mac OS X 15.0; arm64) unknown (codex_exec; 0.161.0)"

	if ua, originator := codexIdentityPair(codexUA, fp); ua != codexUA || originator != "codex_exec" {
		t.Fatalf("genuine pair must be echoed verbatim, got ua=%q originator=%q", ua, originator)
	}
	if ua, originator := codexIdentityPair("curl/8.0", fp); ua != fp.CodexUserAgent() || originator != fp.CodexOriginator() {
		t.Fatalf("non-codex UA must fall back to the profile pair, got ua=%q originator=%q", ua, originator)
	}

	// The invariant that matters most: for every codex-family inbound, the emitted Originator
	// is exactly the emitted UA's leading token, so an upstream pairing the two always agrees.
	for _, ua := range []string{
		"codex_exec/0.161.0 (Mac OS X 15.0; arm64) unknown (codex_exec; 0.161.0)",
		"codex_cli_rs/0.163.0 (Ubuntu 24.04.1; x86_64) unknown (codex_cli_rs; 0.163.0)",
		"codex_cli_rs/0.156.1 (Ubuntu 24.04.1; x86_64) unknown (codex_cli_rs; 0.156.1)",
	} {
		gotUA, gotOriginator := codexIdentityPair(ua, fp)
		if gotUA != ua {
			t.Fatalf("codex UA must be echoed verbatim, got %q want %q", gotUA, ua)
		}
		if want := codexOriginatorFromUserAgent(gotUA); gotOriginator != want {
			t.Fatalf("UA %q paired with originator %q, want %q (never split)", gotUA, gotOriginator, want)
		}
	}

	// The interactive-TUI identity must never reach an upstream, in either header.
	tuiUA := "codex-tui/0.161.0 (Mac OS X 15.0; arm64) unknown (codex-tui; 0.161.0)"
	gotUA, gotOriginator := codexIdentityPair(tuiUA, fp)
	if gotUA == tuiUA || gotOriginator == "codex-tui" {
		t.Fatalf("codex-tui identity must be rewritten, got ua=%q originator=%q", gotUA, gotOriginator)
	}
	if strings.Contains(strings.ToLower(gotUA), "codex-tui") {
		t.Fatalf("emitted UA still carries the tui identity: %q", gotUA)
	}
}

// TestInboundCodexDetectionCoversEveryFamily locks inbound detection to the whole codex
// family. It previously compared the Originator against a single pinned constant, so any
// client announcing itself with another family stopped being recognised the moment the
// default identity moved — a silent regression, not a visible failure.
func TestInboundCodexDetectionCoversEveryFamily(t *testing.T) {
	for _, originator := range []string{"codex_exec", "codex_cli_rs", "codex-tui"} {
		if !isCodexFamilyOriginator(originator) {
			t.Fatalf("%q must be recognised as a codex originator", originator)
		}
	}
	for _, originator := range []string{"", "curl", "python-requests", "claude-cli"} {
		if isCodexFamilyOriginator(originator) {
			t.Fatalf("%q must not be recognised as a codex originator", originator)
		}
	}

	// The family judgement must be whitespace/case tolerant: headers arrive as sent.
	if !isCodexFamilyOriginator("  Codex_CLI_RS  ") {
		t.Fatal("originator matching must trim and ignore case")
	}
}

// TestCodexOriginatorFromUserAgentMatchesModel pins the relay wrapper to the
// shared derivation profile save and fingerprint resolution also use. codex-tui
// becomes the Rust CLI originator; a non-codex UA derives nothing.
func TestCodexOriginatorFromUserAgentMatchesModel(t *testing.T) {
	cases := []string{
		"codex_exec/0.161.0 (Mac OS X 15.0; arm64) unknown (codex_exec; 0.161.0)",
		"codex_cli_rs/0.163.0 (Ubuntu 24.04.1; x86_64) unknown (codex_cli_rs; 0.163.0)",
		"Codex_CLI_RS/1.2.3 (Mac)",
		"codex-tui/0.161.0 (Mac OS X 15.0; arm64) unknown (codex-tui; 0.161.0)",
		"Codex-TUI/1.0",
		"curl/8.0.0",
		"",
	}
	for _, ua := range cases {
		got := codexOriginatorFromUserAgent(ua)
		want := dbmodel.CodexOriginatorFromUserAgent(ua)
		if got != want {
			t.Fatalf("codexOriginatorFromUserAgent(%q) = %q, model = %q", ua, got, want)
		}
	}
	if got := codexOriginatorFromUserAgent("codex-tui/0.161.0"); got != dbmodel.CodexOriginatorRustCLI {
		t.Fatalf("codex-tui originator = %q, want Rust CLI %q", got, dbmodel.CodexOriginatorRustCLI)
	}
	if got := codexOriginatorFromUserAgent("curl/8.0.0"); got != "" {
		t.Fatalf("non-codex UA must not derive an Originator, got %q", got)
	}
}
