package model

import "strings"

// CodexOriginatorRustCLI is the Originator paired with the Rust Codex CLI
// (codex_cli_rs). The deprecated interactive-TUI family is rewritten to this
// token: that identity is never published, and an upstream pairs Originator
// with the User-Agent's leading token (sub2api #3901).
const CodexOriginatorRustCLI = "codex_cli_rs"

const deprecatedCodexTUIToken = "codex-tui"

// CodexOriginatorFromUserAgent is the single derivation of the Originator that
// pairs with a Codex User-Agent. op (on profile save), fingerprint (when
// resolving a channel), and relay (when echoing a downstream CLI) must all use
// this rather than cutting the UA locally.
//
//   - codex_exec and codex_cli_rs, any case, with or without a "/version" suffix:
//     the lowercased leading token.
//   - codex-tui, the deprecated interactive family: the Rust CLI originator.
//     Callers that store or emit the UA must also substitute the Rust CLI
//     User-Agent (DefaultCodexHeaderUserAgent); this function only names the
//     Originator half of that pair.
//   - any other UA, including a non-codex client: "" so a save leaves an
//     explicit Originator untouched.
func CodexOriginatorFromUserAgent(ua string) string {
	token := codexUserAgentToken(ua)
	switch token {
	case "codex_exec", CodexOriginatorRustCLI:
		return token
	case deprecatedCodexTUIToken:
		return CodexOriginatorRustCLI
	default:
		return ""
	}
}

// CodexUserAgentIsDeprecatedTUI reports whether ua is the interactive-TUI
// family. That family is rewritten to the Rust CLI pair wherever a profile is
// saved or resolved.
func CodexUserAgentIsDeprecatedTUI(ua string) bool {
	return codexUserAgentToken(ua) == deprecatedCodexTUIToken
}

func codexUserAgentToken(ua string) string {
	lower := strings.ToLower(strings.TrimSpace(ua))
	if lower == "" {
		return ""
	}
	token, _, _ := strings.Cut(lower, "/")
	return strings.TrimSpace(token)
}
