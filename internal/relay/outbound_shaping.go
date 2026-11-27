package relay

import (
	"net/http"
	"net/http/httptest"

	"github.com/gin-gonic/gin"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/transformer/inbound"
	"github.com/zhizhishu/octopus/internal/transformer/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// applyChannelOutboundShaping 是全仓唯一的「内部请求级」出站整形编排
// （fix-1127 返工第 2 轮第 1 条）。生产 attempt 经 applyTransformOptions /
// applyTransformOptionsWithInboundSetter 调到这里，hlwy 条件推算经导出的
// ApplyChannelOutboundShaping 也调到这里——同一份实现，不存在第二份拷贝，
// 因此推算覆盖 Anthropic / Codex-Responses / Gemini 等所有协议形状
// （含 prepareCodexRequestFingerprint/prepareCodexRequestShape、fallback tools
// 注入、system 注入、默认 effort、CLI 顶层成员、历史桥接的空操作分支）。
//
// 覆盖范围 = 本函数体全部；不覆盖：ParamOverride（作用在出站 HTTP 请求体上，
// 两边都在出站变换后各自调 relay.ApplyParamOverride——顺序同源）。
func (ra *relayAttempt) applyChannelOutboundShaping(updateInboundSetter bool) error {
	ra.applyModelMapping()
	ra.internalRequest.TransformOptions.AnthropicAutoCacheControl = false
	// Channel cloak mode "never" disables Claude identity simulation end to end:
	// header defaults are already gated by shouldApplyChannelCloak; this flag carries
	// the same decision into the Anthropic outbound transformer so it skips the
	// synthetic billing-header / agent-identity system blocks too.
	ra.internalRequest.TransformOptions.SuppressClaudeIdentity = !shouldApplyChannelCloak(ra.channel.Cloak)
	// Attempt-scoped: only a caller positively known not to be a Claude Code CLI lets the
	// Anthropic outbound rewrite a native thinking object the target model rejects
	// (enabled+budget_tokens on an adaptive-only model). A CLI-shaped caller stays byte-identical.
	ra.internalRequest.TransformOptions.ClaudeNonCLICaller = ra.c != nil && ra.c.Request != nil && !clientIsCLIShaped(ra.c.Request)

	// Channel-level opt-in: fold empty-content reasoning into content on the chat
	// inbound wire (new-api thinking_to_content). Default false = no behaviour change.
	ra.internalRequest.TransformOptions.ThinkingToContent = ra.channel.ThinkingToContent
	// TransformRequest runs before channel selection, so the inbound adapter may
	// have captured ThinkingToContent=false. Push the live channel flag now if requested.
	if updateInboundSetter {
		if setter, ok := ra.inAdapter.(interface{ SetThinkingToContent(bool) }); ok {
			setter.SetThinkingToContent(ra.channel.ThinkingToContent)
		}
	}

	if openAIPromptCacheKeyChannel(ra.channel.Type) {
		if enabled, err := op.SettingGetBool(dbmodel.SettingKeyOpenAIAutoPromptCacheKey); err == nil {
			convRoot := ""
			if ra.internalRequest != nil && ra.internalRequest.PreviousResponseID != nil {
				convRoot = responsesConversationRootForRequest(ra.context(), *ra.internalRequest.PreviousResponseID, ra.apiKeyID, ra.userID)
			}
			applyOpenAIAutoPromptCacheKeyWithSession(ra.internalRequest, ra.channel.Type, ra.userID, ra.apiKeyID, ra.channel.Cloak.ProfileID, ra.requestModel, convRoot, enabled)
		}
	}
	ra.prepareCodexRequestFingerprint()
	ra.prepareCodexRequestShape()
	ra.ensureClaudeMetadataUserID()

	// Plain responses clients targeting a CHAT channel rely on previous_response_id,
	// which chat/completions does not support and no chat upstream persists. Rebuild
	// the prior turn's history from the local transcript store; if it can't be honored
	// bridgeResponsesHistoryForChat returns a deterministic invalid_request error so
	// the caller fails loudly instead of forwarding a context-stripped turn.
	if openAIChatOutboundChannel(ra.channel.Type) {
		ra.restoreCodexToolsForStatelessOutbound()
		if err := ra.bridgeResponsesHistoryForChat(); err != nil {
			return err
		}
	}

	if ra.channel.Type != outbound.OutboundTypeAnthropic {
		return nil
	}

	// Plain responses clients (e.g. Cursor) targeting a Claude channel rely on
	// previous_response_id, which Anthropic does not support. Replay the prior
	// turn's history into messages so multi-turn conversations continue.
	// Restore the codex client's real tools from the session BEFORE the history
	// bridge clears previous_response_id (the session is keyed by it); otherwise a
	// codex continuation reaches the stateless Anthropic upstream with no tools and
	// the mapped Claude model stalls (narrates instead of calling tools).
	ra.restoreCodexToolsForStatelessOutbound()
	ra.bridgeResponsesHistoryForAnthropic()

	if ra.channel.AnthropicContext1M {
		ra.internalRequest.TransformOptions.AnthropicOneMillionBeta = true
	}
	ra.prepareClaudePlainClientShape()
	// 补齐真 CLI 必发的三个顶层成员; 必须在 prepareClaudePlainClientShape 之后 —— 那条路径靠
	// "体内是否已有 CLI 成员" 判断调用方是不是普通客户端, 提前填值会把普通客户端的回退工具顶掉。
	ra.ensureClaudeCLIShapeTopLevelKeys()

	enabled, err := op.SettingGetBool(dbmodel.SettingKeyAnthropicAutoCacheControl)
	if err != nil {
		return nil
	}
	ra.internalRequest.TransformOptions.AnthropicAutoCacheControl = enabled
	return nil
}


// ApplyChannelOutboundShaping 是条件推算（hlwy derive）进入唯一出站整形编排的
// 入口：用合成 relayAttempt 调 applyChannelOutboundShaping——生产 attempt 走的
// 正是同一个函数（经 applyTransformOptions 委托），整形逻辑只有一份。
//
// cliShaped 表示调用方是否是 CLI 形状（决定 UA / anthropic-beta 那一组判定）；
// hlwy/检测的合成请求 UA 是 octopus-internal-test，永远传 false。inboundType
// 是请求进入 octopus 的协议族（影响 chat 渠道上 responses 入站的 codex 指纹判定）。
func ApplyChannelOutboundShaping(req *model.InternalLLMRequest, channel *dbmodel.Channel, cliShaped bool, inboundType inbound.InboundType) error {
	if req == nil || channel == nil {
		return nil
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	hr := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	if cliShaped {
		hr.Header.Set("User-Agent", "claude-cli/2.0.0 (external, cli)")
	}
	c.Request = hr
	ra := &relayAttempt{
		relayRequest: &relayRequest{c: c, internalRequest: req, inboundType: inboundType},
		channel:      channel,
	}
	return ra.applyChannelOutboundShaping(false)
}
