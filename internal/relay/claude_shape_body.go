package relay

import (
	"encoding/json"
	"strings"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/bestruirui/octopus/internal/utils/log"
)

// The three top-level members a genuine Claude CLI puts on every POST /v1/messages, measured
// 22/22 in the archived golden captures (374 messages):
//
//	thinking           22/22
//	context_management 22/22
//	output_config      22/22
//	safeguards         10/22 (45.5%) — deliberately NOT synthesised here
//
// A real CLI always sends them, so a non-CLI caller (pi / DSH / a plain SDK) produced a body
// that was missing keys the upstream expects from a CLI-shaped client. The values below are the
// measured ones, not invented: see _artifacts/golden/_archive/samples/2026-10-10-*-golden-direct-*.
//
// Semantic consequence, on purpose and worth knowing before turning this on: `thinking` genuinely
// enables thinking upstream, and `output_config.effort = "high"` is forwarded as the caller's
// effort. This is the "look like the real CLI" side of the trade-off, taken deliberately; the
// switch below is the way back per deployment.
var (
	claudeCLIShapeThinking          = json.RawMessage(`{"type":"adaptive","display":"omitted"}`)
	claudeCLIShapeContextManagement = json.RawMessage(`{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]}`)
	claudeCLIShapeOutputConfig      = json.RawMessage(`{"effort":"high"}`)
)

// ensureClaudeCLIShapeTopLevelKeys completes the CLI-shaped top-level members on a request whose
// caller is not itself a CLI. Reached from the same place as ensureClaudeMetadataUserID, so the
// relay's Anthropic outbound carries the same body shape a genuine CLI would have sent.
//
// Never touches:
//   - a CLI-shaped caller's body (it already sends these; pass-through stays pass-through),
//   - a value the caller did send (only absent members are filled),
//   - safeguards / tool_choice / temperature / top_p (the CLI sends the first in only 45.5% of
//     captures and the others in 0%, so adding them would move away from the real shape, not
//     toward it),
//   - any non-Anthropic channel or a channel whose cloak is off,
//   - the internal ReasoningEffort/AdaptiveThinking form of "thinking" (see below).
//
// It must also run AFTER prepareClaudePlainClientShape: that path decides whether the caller is a
// plain client by asking whether its body already carries the CLI members, so filling them earlier
// would make a plain client look native and silently drop the fallback agent tool set.
func (ra *relayAttempt) ensureClaudeCLIShapeTopLevelKeys() {
	if ra == nil || ra.internalRequest == nil || ra.channel == nil || ra.c == nil {
		return
	}
	if ra.channel.Type != outbound.OutboundTypeAnthropic {
		return
	}
	if !shouldApplyChannelCloak(ra.channel.Cloak) {
		return
	}
	// A genuine CLI caller owns its own shape; completing it would rewrite the caller's body
	// (filling thinking would switch thinking ON for a client that chose to leave it out). So it is
	// left alone — but a CLI-shaped caller MISSING these members is an anomaly worth a warning: a
	// real CLI sends all three on every request (22/22), so this is either a spoofed User-Agent or
	// a trimmed client, and the operator should know the body is not what the shape suggests.
	if clientIsCLIShaped(ra.c.Request) {
		if missing := missingClaudeCLIShapeKeys(ra.internalRequest); len(missing) > 0 {
			log.Warnf("claude CLI shape: a CLI-shaped caller is missing top-level %v — leaving the body exactly as the caller sent it "+
				"(a genuine CLI sends all three on every request, so this is a spoofed fingerprint or a trimmed client; "+
				"completing them would switch thinking on for a client that chose to leave it out, so it stays the caller's call)",
				missing)
		}
		return
	}
	enabled, err := op.SettingGetBool(dbmodel.SettingKeyRelayClaudeCLIShapeKeys)
	if err != nil || !enabled {
		return
	}

	req := ra.internalRequest
	// thinking / output_config are expressed two ways in this codebase: a raw Anthropic
	// preservation (what a CLI sends) or the internal ReasoningEffort/AdaptiveThinking pair (what a
	// non-CLI caller sets). The outbound derives the same members from the second form, and a
	// preserved raw value wins over it — so filling the raw form while the internal form is set
	// would silently discard the caller's effort. Fill only when neither form is present.
	thinkingUnset := !req.AdaptiveThinking && strings.TrimSpace(req.ReasoningEffort) == ""
	if len(req.AnthropicThinking) == 0 && thinkingUnset {
		req.AnthropicThinking = claudeCLIShapeThinking
	}
	if len(req.AnthropicContextManagement) == 0 {
		req.AnthropicContextManagement = claudeCLIShapeContextManagement
	}
	if len(req.AnthropicOutputConfig) == 0 && thinkingUnset {
		req.AnthropicOutputConfig = claudeCLIShapeOutputConfig
	}

	log.Infof("claude CLI shape: completed top-level thinking/context_management/output_config for a non-CLI caller (thinking enables upstream thinking, output_config effort=high); key %s=false disables",
		dbmodel.SettingKeyRelayClaudeCLIShapeKeys)
}

// missingClaudeCLIShapeKeys lists the CLI-shaped top-level members this request does not carry.
func missingClaudeCLIShapeKeys(req *model.InternalLLMRequest) []string {
	if req == nil {
		return nil
	}
	var missing []string
	if len(req.AnthropicThinking) == 0 {
		missing = append(missing, "thinking")
	}
	if len(req.AnthropicContextManagement) == 0 {
		missing = append(missing, "context_management")
	}
	if len(req.AnthropicOutputConfig) == 0 {
		missing = append(missing, "output_config")
	}
	return missing
}
