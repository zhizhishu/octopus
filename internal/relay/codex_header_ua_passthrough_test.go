package relay

import (
	"net/http"
	"strings"
	"testing"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
)

// TestApplyCodexHeaderDefaultsForcesProfilePair locks the codex identity rule: a downstream
// exec/tui/cli_rs user-agent is NO LONGER echoed. Every codex outbound carries the resolved
// fingerprint's pair (Originator + User-Agent) so they always match, at the current version,
// preserving the sub2api #3901 originator↔UA-first-token pairing rule without letting a client
// pick its own identity bytes.
func TestApplyCodexHeaderDefaultsForcesProfilePair(t *testing.T) {
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
	}{
		{"codex_exec downstream is not echoed", codexExecUA, "codex_exec"},
		{"codex-tui downstream is not echoed", codexTuiUA, "codex-tui"},
		{"codex_cli_rs downstream is not echoed", codexRsUA, "codex_cli_rs"},
		{"foreign downstream falls back to profile pair", "python-requests/2.32.0", "python"},
		{"empty downstream falls back to profile pair", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, "https://upstream.example/v1/responses", nil)
			if err != nil {
				t.Fatalf("create request: %v", err)
			}
			applyCodexHeaderDefaultsWithFingerprint(req, nil, fp, tc.inboundUserAgent, tc.inboundOriginator)
			gotUA := req.Header.Get("User-Agent")
			gotOriginator := req.Header.Get("Originator")
			if gotUA != fp.CodexUserAgent() {
				t.Fatalf("User-Agent = %q, want the profile pair %q", gotUA, fp.CodexUserAgent())
			}
			if gotOriginator != fp.CodexOriginator() {
				t.Fatalf("Originator = %q, want the profile pair %q", gotOriginator, fp.CodexOriginator())
			}
			// The pair must be internally consistent: Originator == the UA's leading token.
			slash := strings.Index(gotUA, "/")
			if slash <= 0 || gotOriginator != gotUA[:slash] {
				t.Fatalf("Originator %q must equal the User-Agent's leading token (UA %q)", gotOriginator, gotUA)
			}
			// ...at the current codex version.
			if !strings.Contains(gotUA, dbmodel.DefaultCodexCLIVersion) {
				t.Fatalf("User-Agent %q must carry the current codex version %q", gotUA, dbmodel.DefaultCodexCLIVersion)
			}
		})
	}
}

// TestApplyCodexHeaderDefaultsCustomProfileWins pins that a channel with an explicit custom
// profile keeps THAT profile's pair — the resolved default/global identity never overwrites the
// operator's chosen codex identity, even when a conflicting downstream pair arrives.
func TestApplyCodexHeaderDefaultsCustomProfileWins(t *testing.T) {
	ctx := setupRelayErrorDB(t)
	profile := dbmodel.FingerprintProfile{
		Name:            "custom-codex-identity",
		Seed:            "custom-codex-seed",
		CodexUserAgent:  "codex_cli_rs/0.163.0 (Debian 12.0.0; x86_64) unknown (codex_cli_rs; 0.163.0)",
		CodexOriginator: "codex_cli_rs",
	}
	if err := op.FingerprintProfileCreate(&profile, ctx); err != nil {
		t.Fatalf("create fingerprint profile: %v", err)
	}
	fp := resolveFingerprintForChannel(&dbmodel.Channel{Cloak: dbmodel.ChannelCloak{ProfileID: profile.ID}})

	req, err := http.NewRequest(http.MethodPost, "https://upstream.example/v1/responses", nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	// A conflicting downstream pair must not override the profile.
	applyCodexHeaderDefaultsWithFingerprint(req, nil, fp,
		"codex-tui/0.161.0 (Mac OS X 15.0; arm64) unknown (codex-tui; 0.161.0)", "codex-tui")
	if got := req.Header.Get("User-Agent"); got != profile.CodexUserAgent {
		t.Fatalf("custom profile User-Agent = %q, want %q", got, profile.CodexUserAgent)
	}
	if got := req.Header.Get("Originator"); got != profile.CodexOriginator {
		t.Fatalf("custom profile Originator = %q, want %q", got, profile.CodexOriginator)
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

// TestCodexIdentityPairIgnoresInbound pins the direct pairing rule: codexIdentityPair no longer
// echoes a downstream exec/tui/cli_rs pair — it always returns the resolved fingerprint's pair,
// so UA and Originator can never be split, and an inbound Originator cannot override the rs UA.
func TestCodexIdentityPairIgnoresInbound(t *testing.T) {
	fp := resolveFingerprintForChannel(nil)
	for _, inbound := range []struct{ ua, originator string }{
		{"codex_exec/0.161.0 (Mac OS X 15.0; arm64) unknown (codex_exec; 0.161.0)", "codex_exec"},
		{"codex-tui/0.161.0 (Mac OS X 15.0; arm64) unknown (codex-tui; 0.161.0)", "codex-tui"},
		{"codex_cli_rs/0.161.0 (Ubuntu 24.04.1; x86_64) unknown (codex_cli_rs; 0.161.0)", "codex_cli_rs"},
		{"curl/8.0", "curl"},
		{"", ""},
	} {
		ua, originator := codexIdentityPair(inbound.ua, inbound.originator, fp)
		if ua != fp.CodexUserAgent() || originator != fp.CodexOriginator() {
			t.Fatalf("codexIdentityPair(%q, %q) = (%q, %q), want the profile pair (%q, %q)",
				inbound.ua, inbound.originator, ua, originator, fp.CodexUserAgent(), fp.CodexOriginator())
		}
	}
}
