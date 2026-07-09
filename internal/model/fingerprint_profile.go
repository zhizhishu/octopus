package model

// FingerprintProfile is one selectable upstream device identity. A channel's
// ChannelCloak.ProfileID picks one of these so two channels (e.g. two upstream
// keys behind different egress IPs) can present DISTINCT devices instead of the
// single per-instance fingerprint shared by all traffic.
//
// Compatibility contract: ProfileID 0 on a channel means "global default" and is
// NOT a row here — it resolves to the per-instance seed + global header settings,
// byte-for-byte the pre-profile behaviour. Only ProfileID > 0 reads a row.
//
// Every header/UA string field is OPTIONAL: an empty value falls back to the
// existing global setting default (the same fallback chain the relay already
// uses), so a profile only needs to spell out the fields it wants to differ on.
// Seed is the per-profile root from which the Claude device_id / Codex
// installation id are derived; leaving it empty makes the op layer generate and
// persist a random one on first save so two profiles get unrelated device ids.
type FingerprintProfile struct {
	ID   int    `json:"id" gorm:"primaryKey"`
	Name string `json:"name" gorm:"unique;not null"`
	// Seed derives device_id / installation id. Empty -> generated+persisted once.
	Seed string `json:"seed"`

	// Claude header set (empty field => fall back to the global setting default).
	ClaudeUserAgent      string `json:"claude_user_agent"`
	ClaudePackageVersion string `json:"claude_package_version"`
	ClaudeRuntimeVersion string `json:"claude_runtime_version"`
	ClaudeOS             string `json:"claude_os"`
	ClaudeArch           string `json:"claude_arch"`
	ClaudeTimeout        string `json:"claude_timeout"`
	// ClaudeStabilize is tri-state: nil => fall back to the global setting,
	// otherwise the explicit profile choice for the X-Stainless-OS/Arch pair.
	ClaudeStabilize *bool `json:"claude_stabilize"`

	// Codex header set (empty field => fall back to the global setting default).
	CodexUserAgent    string `json:"codex_user_agent"`
	CodexOriginator   string `json:"codex_originator"`
	CodexBetaFeatures string `json:"codex_beta_features"`

	// GenericUA is the unified User-Agent applied to NON claude/codex channels
	// (Gemini/Volcengine/plain OpenAI-chat). Empty now falls back to DefaultGenericUA
	// (a stable Linux desktop identity) instead of leaking Go's "Go-http-client/1.1";
	// set it to pin a different identity per profile.
	GenericUA string `json:"generic_ua"`
}

// DefaultGenericUA is the built-in fallback User-Agent for non claude/codex channels
// (Gemini / Volcengine / plain OpenAI-chat) when no profile pins a GenericUA. Without
// it Go's transport emits "Go-http-client/1.1", which flags the caller as a bot/proxy.
// A stable, ordinary Chrome-on-Linux (Ubuntu/Debian, X11 x86_64) desktop UA blends in
// instead. It is downstream-of-nothing: these channels are NOT claude/codex, so this
// never touches the codex/claude CLI fingerprint.
const DefaultGenericUA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// FingerprintProfileUpdateRequest carries a partial update — only non-nil fields
// are applied, mirroring ChannelUpdateRequest's pointer-field convention.
type FingerprintProfileUpdateRequest struct {
	ID                   int     `json:"id" binding:"required"`
	Name                 *string `json:"name,omitempty"`
	Seed                 *string `json:"seed,omitempty"`
	ClaudeUserAgent      *string `json:"claude_user_agent,omitempty"`
	ClaudePackageVersion *string `json:"claude_package_version,omitempty"`
	ClaudeRuntimeVersion *string `json:"claude_runtime_version,omitempty"`
	ClaudeOS             *string `json:"claude_os,omitempty"`
	ClaudeArch           *string `json:"claude_arch,omitempty"`
	ClaudeTimeout        *string `json:"claude_timeout,omitempty"`
	ClaudeStabilize      *bool   `json:"claude_stabilize,omitempty"`
	CodexUserAgent       *string `json:"codex_user_agent,omitempty"`
	CodexOriginator      *string `json:"codex_originator,omitempty"`
	CodexBetaFeatures    *string `json:"codex_beta_features,omitempty"`
	GenericUA            *string `json:"generic_ua,omitempty"`
}
