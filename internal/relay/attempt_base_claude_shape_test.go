package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/transformer/inbound"
	"github.com/zhizhishu/octopus/internal/transformer/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// 这条测的是**调用点**,不是纯函数: 它按尝试循环的真实顺序跑
// captureAttemptBase -> base.restore -> applyTransformOptionsWithInboundSetter,
// 让 ensureClaudeCLIShapeTopLevelKeys / applyClaudeOneMillionRuntimeShape 这两个
// 「就地改写共享 internalRequest」的写入者真的跑起来。只单测 restore() 的话,
// 漏掉的字段会安静地留在默认零值上,红不了。
func TestFailedCloakedAttemptLeavesNoClaudeShapeForTheNextAttempt(t *testing.T) {
	setupRescueDeadlineDB(t)
	if err := op.SettingSetString(dbmodel.SettingKeyRelayClaudeCLIShapeKeys, "true"); err != nil {
		t.Fatalf("enable claude CLI shape switch: %v", err)
	}

	text := "ping"
	stream := true
	internalRequest := &model.InternalLLMRequest{
		Model:        "request-model",
		RawAPIFormat: model.APIFormatAnthropicMessage,
		Stream:       &stream,
		Messages:     []model.Message{{Role: "user", Content: model.MessageContent{Content: &text}}},
	}
	base := captureAttemptBase(internalRequest)

	// 首跳: cloak 打开、且渠道级 1M opt-in 的 Anthropic 渠道。它会把
	// thinking/context_management/output_config/safeguards 补到共享请求上,
	// 并把 AnthropicOneMillionBeta 置 true —— 这些只对「这一跳」成立。
	first := runAttemptStep(t, internalRequest, &base, &dbmodel.Channel{
		Type:               outbound.OutboundTypeAnthropic,
		AnthropicContext1M: true,
	})
	if len(internalRequest.AnthropicThinking) == 0 ||
		len(internalRequest.AnthropicContextManagement) == 0 ||
		len(internalRequest.AnthropicOutputConfig) == 0 ||
		!internalRequest.TransformOptions.AnthropicOneMillionBeta {
		t.Fatalf("首跳没有就地改写共享请求, 这条测试就失去意义: thinking=%s context=%s output=%s 1m=%v",
			internalRequest.AnthropicThinking, internalRequest.AnthropicContextManagement,
			internalRequest.AnthropicOutputConfig, internalRequest.TransformOptions.AnthropicOneMillionBeta)
	}
	_ = first

	// 次跳: cloak=never 的 Anthropic 渠道(明确要求「别冒充 Claude」的国产兼容上游),
	// 没开 AnthropicContext1M。客户端本来也没发这些成员。
	second := runAttemptStep(t, internalRequest, &base, &dbmodel.Channel{
		Type:  outbound.OutboundTypeAnthropic,
		Cloak: dbmodel.ChannelCloak{Mode: "never"},
	})

	if got := string(internalRequest.AnthropicThinking); got != "" {
		t.Errorf("次跳 AnthropicThinking = %s, want empty (首跳补的 thinking 骑到了 cloak=never 的渠道)", got)
	}
	if got := string(internalRequest.AnthropicContextManagement); got != "" {
		t.Errorf("次跳 AnthropicContextManagement = %s, want empty", got)
	}
	if got := string(internalRequest.AnthropicOutputConfig); got != "" {
		t.Errorf("次跳 AnthropicOutputConfig = %s, want empty", got)
	}
	if _, ok := internalRequest.AnthropicExtraTopLevel["safeguards"]; ok {
		t.Errorf("次跳 still carries safeguards=%s from the first attempt", internalRequest.AnthropicExtraTopLevel["safeguards"])
	}
	if internalRequest.TransformOptions.AnthropicOneMillionBeta {
		t.Error("次跳 AnthropicOneMillionBeta = true, want false (次跳渠道没开 1M, 客户端也没要)")
	}
	if model.AnthropicRequestWantsOneMillionBeta(internalRequest) {
		t.Error("次跳仍会发 context-1m beta 头")
	}
	// 反向守卫: SuppressClaudeIdentity 是**每一跳重新赋值**的自愈字段(relay.go 里的
	// `= !shouldApplyChannelCloak(channel.Cloak)`)。谁把它并进 restore 的「残留还原」
	// 一组,这里就会变红——那会让 cloak=never 的渠道重新收到伪造的 Claude 身份。
	if !internalRequest.TransformOptions.SuppressClaudeIdentity {
		t.Error("cloak=never 的次跳 SuppressClaudeIdentity = false: 它就是被上一跳的残留盖掉的")
	}

	// 真正出站的那份 body: 这四样一个都不能有。
	body := string(outboundBody(t, second))
	for _, key := range []string{"thinking", "context_management", "output_config", "safeguards"} {
		if strings.Contains(body, `"`+key+`"`) {
			t.Errorf("次跳出站 body 带着首跳的 %q: %s", key, body)
		}
	}
}

// 反方向: 客户端**自己**发了这些成员时, 首跳的改写不能把它们换掉、更不能还原成 nil。
// 这条卡的是「快照漏来字段」那一半: 只测「恢复成空」的话, 把 captureAttemptBase 里的
// 快照删掉测试仍然绿(空 -> 空, 看不出区别)。
func TestCallerSuppliedClaudeShapeSurvivesAFailedCloakedAttempt(t *testing.T) {
	setupRescueDeadlineDB(t)
	if err := op.SettingSetString(dbmodel.SettingKeyRelayClaudeCLIShapeKeys, "true"); err != nil {
		t.Fatalf("enable claude CLI shape switch: %v", err)
	}

	callerThinking := `{"type":"enabled","budget_tokens":4096}`
	callerContext := `{"edits":[]}`
	callerOutput := `{"effort":"low"}`
	callerSafeguards := `[{"type":"caller_own"}]`
	text := "ping"
	stream := true
	internalRequest := &model.InternalLLMRequest{
		Model:                      "request-model",
		RawAPIFormat:               model.APIFormatAnthropicMessage,
		Stream:                     &stream,
		AnthropicThinking:          json.RawMessage(callerThinking),
		AnthropicContextManagement: json.RawMessage(callerContext),
		AnthropicOutputConfig:      json.RawMessage(callerOutput),
		AnthropicExtraTopLevel:     map[string]json.RawMessage{"safeguards": json.RawMessage(callerSafeguards)},
		Messages:                   []model.Message{{Role: "user", Content: model.MessageContent{Content: &text}}},
		// 客户端用 opus[1m] 之类的别名要了 1M; prepareAnthropicModelCompatibility 在建 base
		// 之前就把它置 true, 所以这是「客户端真实意图」, 快照必须带上它。
		TransformOptions: model.TransformOptions{AnthropicOneMillionBeta: true},
	}
	base := captureAttemptBase(internalRequest)

	// 首跳: cloak 打开 + 渠道 1M opt-in。thinking 等已经在, 补值路径只补空缺所以不动作,
	// 但这个渠道仍然把 1M 标记打到了共享请求上。
	runAttemptStep(t, internalRequest, &base, &dbmodel.Channel{
		Type:               outbound.OutboundTypeAnthropic,
		AnthropicContext1M: true,
	})

	// 次跳: 另一个渠道。客户端发的成员必须逐字还在(不是被首跳改过, 也不是被还原成 nil)。
	runAttemptStep(t, internalRequest, &base, &dbmodel.Channel{
		Type: outbound.OutboundTypeAnthropic,
	})

	if got := string(internalRequest.AnthropicThinking); got != callerThinking {
		t.Errorf("次跳 AnthropicThinking = %s, want the caller's %s", got, callerThinking)
	}
	if got := string(internalRequest.AnthropicContextManagement); got != callerContext {
		t.Errorf("次跳 AnthropicContextManagement = %s, want the caller's %s", got, callerContext)
	}
	if got := string(internalRequest.AnthropicOutputConfig); got != callerOutput {
		t.Errorf("次跳 AnthropicOutputConfig = %s, want the caller's %s", got, callerOutput)
	}
	if got := string(internalRequest.AnthropicExtraTopLevel["safeguards"]); got != callerSafeguards {
		t.Errorf("次跳 safeguards = %s, want the caller's %s", got, callerSafeguards)
	}
	// 客户端自己要的 1M 不能因为「次跳渠道没开 AnthropicContext1M」就被悄悄丢掉。
	if !internalRequest.TransformOptions.AnthropicOneMillionBeta {
		t.Error("次跳 AnthropicOneMillionBeta = false: 客户端要的 1M 被首跳的残留还原逻辑吃掉了")
	}
}

// runAttemptStep 复刻尝试循环里对共享 internalRequest 做的两件事:
// 先 base.restore 回到「客户端真正要什么」, 再跑这一跳自己的改写。
func runAttemptStep(t *testing.T, req *model.InternalLLMRequest, base *attemptBase, channel *dbmodel.Channel) *relayAttempt {
	t.Helper()
	gin.SetMode(gin.TestMode)
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	// 非 CLI 的调用方: 这是 ensureClaudeCLIShapeTopLevelKeys 真正会补成员的前提。
	httpReq.Header.Set("User-Agent", "python-requests/2.31.0")
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httpReq
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			c:               c,
			internalRequest: req,
			inboundType:     inbound.InboundTypeAnthropic,
			requestModel:    "request-model",
		},
		channel: channel,
	}
	base.restore(req, "request-model")
	if err := ra.applyTransformOptionsWithInboundSetter(false); err != nil {
		t.Fatalf("applyTransformOptions: %v", err)
	}
	return ra
}
