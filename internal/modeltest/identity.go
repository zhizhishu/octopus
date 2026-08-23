package modeltest

import dbmodel "github.com/bestruirui/octopus/internal/model"

// EndpointIdentity is the resolved upstream identity a channel test presents for
// one endpoint family. It mirrors exactly what the test/forward path sends on the
// wire, so the UI can show "this is the shape/UA the probe uses" without guessing.
type EndpointIdentity struct {
	Shape     string `json:"shape"` // "codex" | "claude" | "generic"
	UserAgent string `json:"user_agent"`
	Detail    string `json:"detail,omitempty"` // extra header hint (originator / os·arch·ver)
}

// ChannelTestIdentity is the per-endpoint identity a channel test will present,
// resolved from the channel's fingerprint profile (ProfileID 0 => global default).
// Read-only view: it never mutates state and applies the SAME fallback chain the
// relay/test forward path uses, upholding the test==real-traffic invariant.
type ChannelTestIdentity struct {
	ProfileID   int              `json:"profile_id"`
	ProfileName string           `json:"profile_name,omitempty"`
	Codex       EndpointIdentity `json:"codex"`   // openai_responses
	Claude      EndpointIdentity `json:"claude"`  // anthropic_messages
	Generic     EndpointIdentity `json:"generic"` // openai_chat / gemini
}

// ResolveChannelTestIdentity returns the identity a model/channel test presents
// for the given channel, per endpoint family. It resolves through the channel's
// ProfileID exactly like resolveFingerprint does for the actual probe, so the
// displayed identity always matches what the test (and thus real traffic) sends.
func ResolveChannelTestIdentity(channel *dbmodel.Channel) ChannelTestIdentity {
	fp := resolveFingerprint(channel)

	generic := fp.GenericUA()
	if generic == "" {
		generic = dbmodel.DefaultGenericUA
	}

	id := ChannelTestIdentity{
		Codex: EndpointIdentity{
			Shape:     "codex",
			UserAgent: fp.CodexUserAgent(),
			Detail:    "originator " + fp.CodexOriginator(),
		},
		Claude: EndpointIdentity{
			Shape:     "claude",
			UserAgent: fp.ClaudeUserAgent(),
			Detail:    fp.ClaudeOS() + "·" + fp.ClaudeArch() + "·" + fp.ClaudePackageVersion(),
		},
		Generic: EndpointIdentity{
			Shape:     "generic",
			UserAgent: generic,
		},
	}
	if fp.HasProfile() {
		id.ProfileID = fp.ProfileID()
		id.ProfileName = fp.ProfileName()
	}
	return id
}
