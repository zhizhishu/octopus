package relay

import (
	"strings"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/transformer/model"
	"github.com/zhizhishu/octopus/internal/utils/log"
)

// ClaudeDefaultEffortSetting returns the configured Claude default thinking effort
// ("默认思考强度"), normalised to none/low/medium/high/xhigh/max. Legacy stored values are read
// the way migration 023 rewrites them (auto -> high, off/false/disabled -> none), so a node that
// has not run the migration yet behaves exactly like one that has. Unknown values fall back to
// the default, high.
func ClaudeDefaultEffortSetting() string {
	return normalizeClaudeDefaultEffort(settingString(dbmodel.SettingKeyClaudeCLIReasoningEffort, dbmodel.ClaudeDefaultEffortDefault))
}

func normalizeClaudeDefaultEffort(raw string) string {
	switch v := strings.ToLower(strings.TrimSpace(raw)); v {
	case "none", "off", "false", "disabled":
		return model.ClaudeDefaultEffortNone
	case "low", "medium", "high", "xhigh", "max":
		return v
	default: // "", "auto", anything unknown
		return dbmodel.ClaudeDefaultEffortDefault
	}
}

// applyClaudeDefaultEffort fills the configured default effort on a non-Claude-Code request
// bound for an Anthropic channel, only when the client expressed no thinking choice. Called
// from prepareClaudePlainClientShape, so a real Claude Code request (system markers, its own
// output_config, or a claude-cli User-Agent / claude-code beta) never reaches it.
//
// Body only: this changes thinking / output_config in the JSON body, never a header, the TLS
// fingerprint or the identity — see AGENTS.md "Shape 铁律".
func (ra *relayAttempt) applyClaudeDefaultEffort() {
	if ra == nil || ra.internalRequest == nil {
		return
	}
	level := ClaudeDefaultEffortSetting()
	filled, skip := model.ApplyClaudeDefaultEffort(ra.internalRequest, level)
	if filled == "" {
		if skip != model.ClaudeEffortSkipSettingNone && skip != model.ClaudeEffortSkipClientOwned {
			log.Debugf("claude default effort: not filled for model %q (%s)", ra.internalRequest.Model, skip)
		}
		return
	}
	ra.claudeDefaultEffort = filled
	if filled != level {
		log.Debugf("claude default effort: model %q does not take %s, filled %s instead", ra.internalRequest.Model, level, filled)
	}
}
