package relay

import (
	"net/http"
	"net/http/httptest"

	"github.com/gin-gonic/gin"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/transformer/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// ApplyChannelOutboundShaping 把 relay 真实出站的「内部请求级」整形按生产同一顺序、
// 同一批实现应用到 req 上（fix-1127 返工第 2 条：hlwy 条件推算与 relay 同源）。
//
// 覆盖：SuppressClaudeIdentity / ClaudeNonCLICaller / ThinkingToContent 三个
// TransformOptions 旗标、AnthropicContext1M、fallback tools 注入、默认 effort、
// CLI 顶层成员补齐（thinking/output_config/context_management/safeguards）。
// 不覆盖：ParamOverride（作用在出站 HTTP 请求体上，调用方在 TransformRequest 之后
// 自行 ApplyParamOverride——与生产顺序一致）；会话/历史 bridging（一次性检测请求
// 没有 previous_response_id，那些步骤对它本就是空操作）；提示词注入（测试入口
// 本来就不注入）。
//
// 实现说明：relay 的生产 attempt 路径在 applyTransformOptionsWithInboundSetter 里
// 逐个调用这里用的同一批方法（prepareClaudePlainClientShape /
// ensureClaudeCLIShapeTopLevelKeys，顺序相同）；本函数只是用**同一个实现**在
// 进程内合成上下文上重放一遍——没有任何一份逻辑存在两份拷贝。
func ApplyChannelOutboundShaping(req *model.InternalLLMRequest, channel *dbmodel.Channel, cliShaped bool) {
	if req == nil || channel == nil {
		return
	}
	// 与 applyTransformOptionsWithInboundSetter 顶部同一组旗标、同一取值来源。
	req.TransformOptions.SuppressClaudeIdentity = !shouldApplyChannelCloak(channel.Cloak)
	req.TransformOptions.ClaudeNonCLICaller = !cliShaped
	req.TransformOptions.ThinkingToContent = channel.ThinkingToContent
	if channel.Type != outbound.OutboundTypeAnthropic {
		return
	}
	if channel.AnthropicContext1M {
		req.TransformOptions.AnthropicOneMillionBeta = true
	}
	// 整形方法读 relayAttempt 的 channel/internalRequest/c；c 的唯一用途是判定
	// 调用方是不是 CLI 形状（UA / anthropic-beta）。合成一个只有这个信号的
	// gin 上下文，方法实现与生产逐字相同。
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	hr := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	if cliShaped {
		hr.Header.Set("User-Agent", "claude-cli/2.0.0 (external, cli)")
	}
	c.Request = hr
	ra := &relayAttempt{
		relayRequest: &relayRequest{c: c, internalRequest: req},
		channel:      channel,
	}
	// 顺序与生产一致：先 fallback tools/默认 effort（靠"体内是否已有 CLI 成员"
	// 判普通客户端），后补 CLI 顶层成员——颠倒会把普通客户端的回退工具顶掉。
	ra.prepareClaudePlainClientShape()
	ra.ensureClaudeCLIShapeTopLevelKeys()
}
