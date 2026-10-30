package relay

import (
	"encoding/json"
	"strings"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/transformer/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
	"github.com/zhizhishu/octopus/internal/utils/log"
	"github.com/samber/lo"
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
// thinking / output_config are NOT a fixed value: they follow the "默认思考强度" setting
// (claude_cli_reasoning_effort) through applyClaudeDefaultEffort, so the switch never forces
// effort=high behind the operator's back. When that setting is 不补 / the client chose its own
// thinking / a model guard skipped it, these two members are left as they are.
//
// context_management (clear_thinking_20251015, keep=all) is only legal while thinking is
// enabled or adaptive; it is marked as injected so the Anthropic outbound drops it otherwise
// (CLIProxyAPI injectClaudeCodeContextManagement).
var (
	claudeCLIShapeThinking          = json.RawMessage(`{"type":"adaptive","display":"omitted"}`)
	claudeCLIShapeContextManagement = json.RawMessage(`{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]}`)
)

func claudeCLIShapeOutputConfig(effort string) json.RawMessage {
	return json.RawMessage(`{"effort":"` + effort + `"}`)
}

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
//   - a thinking / effort choice the caller made (only the default-effort fill is mirrored).
//
// It must also run AFTER prepareClaudePlainClientShape: that path decides whether the caller is a
// plain client by asking whether its body already carries the CLI members, so filling them earlier
// would make a plain client look native and silently drop the fallback agent tool set.
func (ra *relayAttempt) ensureClaudeCLIShapeTopLevelKeys() {
	if ra == nil || ra.internalRequest == nil {
		return
	}
	// Attempt-scoped: a previous hop's injection must not make this hop drop a client value.
	ra.internalRequest.TransformOptions.ClaudeContextManagementInjected = false
	if ra.channel == nil || ra.c == nil {
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
	// thinking / output_config: only when applyClaudeDefaultEffort filled an effort on this
	// attempt (client sent nothing, setting is not 不补, model guard passed). Writing the raw
	// CLI form instead of the internal ReasoningEffort/AdaptiveThinking pair keeps the golden
	// byte shape ({"type":"adaptive","display":"omitted"}); the effort is the same one.
	thinkingFilled := false
	if effort := ra.claudeDefaultEffort; effort != "" && len(req.AnthropicThinking) == 0 && len(req.AnthropicOutputConfig) == 0 {
		req.AnthropicThinking = claudeCLIShapeThinking
		req.AnthropicOutputConfig = claudeCLIShapeOutputConfig(effort)
		thinkingFilled = true
	}
	if len(req.AnthropicContextManagement) == 0 {
		req.AnthropicContextManagement = claudeCLIShapeContextManagement
		req.TransformOptions.ClaudeContextManagementInjected = true
	}

	// safeguards: 值取自黄金样本, 只把 platform 换成我们对外宣称的平台; 其余字段(调用方的
	// cwd/home/permission_mode/git_state 等)是样本默认值, 不是调用方的真实状态 —— 如实记在日志里。
	safeguardsDefaulted := false
	if req.AnthropicExtraTopLevel == nil {
		req.AnthropicExtraTopLevel = make(map[string]json.RawMessage, 1)
	}
	if _, ok := req.AnthropicExtraTopLevel["safeguards"]; !ok {
		req.AnthropicExtraTopLevel["safeguards"] = claudeCLIShapeSafeguards()
		safeguardsDefaulted = true
	}

	log.Infof("claude CLI shape: completed top-level %scontext_management%s for a non-CLI caller (effort=%s from %s; context_management is dropped if thinking ends up off); key %s=false disables",
		lo.Ternary(thinkingFilled, "thinking/output_config/", ""), lo.Ternary(safeguardsDefaulted, "/safeguards", ""),
		lo.Ternary(thinkingFilled, ra.claudeDefaultEffort, "unchanged"), dbmodel.SettingKeyClaudeCLIReasoningEffort, dbmodel.SettingKeyRelayClaudeCLIShapeKeys)
	if safeguardsDefaulted {
		log.Warnf("claude CLI shape: the filled safeguards is NOT the caller's real state — platform is derived from our advertised fingerprint, "+
			"while permission_mode/live_cwd/home_dir/git_state/rules keep the golden sample's defaults (measured: a genuine CLI sends safeguards on "+
			"12 of 22 captured requests, so it is conditional, not universal)")
	}
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
	if _, ok := req.AnthropicExtraTopLevel["safeguards"]; !ok {
		missing = append(missing, "safeguards")
	}
	return missing
}

// claudeCLIShapeSafeguardsTemplate 是"非 CLI 调用方"出站时补的 safeguards 值, 逐字取自
// 2026-10-10 的黄金抓包(真 CLI 直连 22 份里 12 份带这个成员 = 54.5%; 是隔次/按条件出现, 不是每轮都有
// —— 大领导要求补齐, 故补; 口径可回退: 关掉开关即不补)。
//
// ⚠️ 语义: classifier_context 描述的是**调用方自己的机器与 CLI 配置**(live_cwd / home_dir /
// permission_mode / git_state / rules ...), oct 拿不到, 所以保留黄金样本默认值。也就是说补出来的
// 这一段等于替调用方宣称"permission_mode=auto, cwd=/, home=/root, 非 git 仓库"。唯一派生的是
// platform: 取我们对外宣称的指纹平台(与 X-Stainless-OS 同源), 免得出现"头里说 Windows、
// safeguards 里说 linux"这种自相矛盾的组合。哪些字段是默认值见补值时打的日志。
const claudeCLIShapeSafeguardsTemplate = `[{"type":"dangerous_tool_use","classifier_context":{"v":1,"permission_mode":"auto","platform":"__PLATFORM__","live_cwd":"/","home_dir":"/root","rule_roots":{"userSettings":"/root/.claude","projectSettings":"/","localSettings":"/","flagSettings":"/","policySettings":"/","cliArg":"/","command":"/","session":"/","toolsNarrowing":"/","mcpServerPolicy":"/","hostCredential":"/"},"trusted_directories":{"primary":{"path":"/","resolved":["/"]},"additional":[],"network":[],"block_reads_outside_working_directories":false},"rules":{"allow":[],"deny":[],"ask":[]},"auto_mode":{"allow":[],"soft_deny":[],"hard_deny":[],"environment":[]},"artifact_consent_holdback":false,"case_insensitive_paths":false,"restricted":false,"is_remote_mode":false,"classify_all_shell":false,"user_identity":null,"git_state":{"cwd":"/","root":null,"branch":null,"default_branch":null,"status":null,"visibility":{"origin":null,"push_remote":null,"remotes":[],"visibility_cache":[]},"error":"not_a_repo"}}}]`

// claudeCLIShapeSafeguards 返回补给出站的 safeguards: platform 派生, 其余为样本默认值。
func claudeCLIShapeSafeguards() json.RawMessage {
	platform := settingString(dbmodel.SettingKeyClaudeHeaderOS, dbmodel.DefaultClaudeHeaderOS)
	if platform == "" {
		platform = dbmodel.DefaultClaudeHeaderOS
	}
	return json.RawMessage(strings.ReplaceAll(claudeCLIShapeSafeguardsTemplate, "__PLATFORM__", strings.ToLower(platform)))
}
