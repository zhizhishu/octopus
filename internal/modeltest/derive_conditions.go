package modeltest

import (
	"context"
	"net/http"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/modelverify"
	"github.com/zhizhishu/octopus/internal/modelverify/numberdist"
	"github.com/zhizhishu/octopus/internal/relay"
	"github.com/zhizhishu/octopus/internal/transformer/inbound"
	geminiInbound "github.com/zhizhishu/octopus/internal/transformer/inbound/gemini"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// fix-1127 第 3 项（Hark 定的中间路线）：hlwy 报告里的「条件」不再靠逐格观测
// （观测钩子长在已拆除的探针自拼出站上），而是整轮开始时**从渠道配置推算**一次。
//
// 不另写一套形状逻辑（用户：能不造轮子就不造）——推算用的就是 relay 出站实际
// 调用的同一批函数：
//  1. 请求体由 RelaySender.buildRelayRequest 渲染（与真实采样同一个模板）；
//  2. 入站变换用 inbound.Get（relay.go:1421 同一个注册表）；
//  3. 出站变换用 outbound.Get（relay outboundAdapterFor 同一个注册表）；
//  4. 参数覆盖用 relay.ApplyParamOverride（relay.go 出站路径同一个函数）；
//  5. 最终条件用 probe_conditions.go 里同一套提取逻辑从出站请求体读出。
//
// 唯一无法离线复刻的是 relayAttempt 上的流式强制规则（方法是未导出的、挂在
// 运行时 attempt 上）。它的确定性部分只有两条，照 relay.go:2222-2260 的口径
// 复刻在此，并在注释里保持指向；推不出的一律 honest unknown（提取逻辑本来就
// 把读不到的字段留 unknown）。

// DeriveRelayConditions 推算一次 hlwy 采样在指定渠道上的最终出站条件。
// 失败（准入不过关、变换报错）返回该协议的全 unknown 快照——与「没观测到」同义，
// 绝不拿猜的值充数。
func DeriveRelayConditions(channel dbmodel.Channel, upstreamModel, endpoint string) numberdist.Conditions {
	unknown := op_HlwyUnknownConditionsShim(channel)
	req, ok := deriveOutboundRequest(channel, upstreamModel, endpoint)
	if !ok {
		return unknown
	}
	conditions := buildOutboundConditions(probeProtocolOf(channel.Type), req)
	conditions.ConfigHash = HlwyChannelConfigHash(channel)
	return conditions
}

// deriveOutboundRequest 是 DeriveRelayConditions 的推算管线本体（同源整形→出站变换→
// ParamOverride），包内单独拆出是为了让一致性测试拿到推算的最终出站请求体，与假上游
// 截获的真实出站字节逐键对比。失败返回 ok=false——与 DeriveRelayConditions 的
// unknown 同义，绝不拿猜的值充数。
func deriveOutboundRequest(channel dbmodel.Channel, upstreamModel, endpoint string) (*http.Request, bool) {
	sender, err := NewRelaySender(channel, upstreamModel, endpoint)
	if err != nil {
		return nil, false
	}
	spec, err := sender.buildRelayRequest(modelverify.Ask{Prompt: numberdist.Prompt})
	if err != nil {
		return nil, false
	}
	inAdapter := inbound.Get(sender.inboundType)
	outAdapter := outbound.Get(channel.Type)
	if inAdapter == nil || outAdapter == nil {
		return nil, false
	}
	ctx := context.Background()
	if sender.inboundType == inbound.InboundTypeGemini {
		ctx = geminiInbound.WithRequestOptions(ctx, sender.requestModel, false)
	}
	internalReq, err := inAdapter.TransformRequest(ctx, spec.body)
	if err != nil || internalReq == nil {
		return nil, false
	}
	// 流式强制的确定性部分（relay.go:2222-2260）：responses 出站恒强制流式；
	// anthropic 渠道在 cloak 生效时强制流式（嵌入除外，hlwy 不到嵌入）。
	forced := false
	if channel.Type == outbound.OutboundTypeOpenAIResponse {
		forced = true
	}
	if channel.Type == outbound.OutboundTypeAnthropic && shouldApplyChannelCloak(channel.Cloak) {
		forced = true
	}
	stream := forced
	internalReq.Stream = &stream

	// 内部请求级整形（fix-1127 返工第 2 轮第 1 条）：调 relay 唯一的出站整形编排
	// ——生产 attempt 也走它（relay/outbound_shaping.go，applyTransformOptions 直接
	// 委托同一函数），覆盖所有协议形状（Anthropic fallback tools/默认 effort/CLI 顶层
	// 成员、Codex/Responses 指纹与形状、Gemini 等），不存在第二份拷贝。
	// cliShaped=false：hlwy 合成请求 UA 是 octopus-internal-test，永远不是 CLI 形状，
	// 与真实采样一致。
	if err := relay.ApplyChannelOutboundShaping(internalReq, &channel, false, sender.inboundType); err != nil {
		return nil, false
	}

	key := ""
	if keys := channel.GetAvailableChannelKeys(); len(keys) > 0 {
		key = keys[0].ChannelKey
	}
	req, err := outAdapter.TransformRequest(ctx, internalReq, channel.GetBaseUrl(), key)
	if err != nil || req == nil {
		return nil, false
	}
	if err := relay.ApplyParamOverride(req, channel.ParamOverride); err != nil {
		return nil, false
	}
	return req, true
}

// op_HlwyUnknownConditionsShim 是 DeriveRelayConditions 内部兜底：与
// op.HlwyUnknownConditions 同义但不反向依赖 op 包（op 已依赖 modeltest，
// 反向会循环）。字段取值必须保持与 op.HlwyUnknownConditions 逐字一致。
func op_HlwyUnknownConditionsShim(channel dbmodel.Channel) numberdist.Conditions {
	return numberdist.Conditions{
		Protocol:         probeProtocolOf(channel.Type),
		ProtocolID:       numberdist.ProtocolID,
		ParserVersion:    numberdist.ParserVersion,
		TransportProfile: numberdist.TransportProfile,
		TemperatureState: numberdist.StateUnknown,
		TopPState:        numberdist.StateUnknown,
	}
}

// HlwyChannelConfigHash 是被测渠道「决定出站长什么样」的配置快照哈希。
// 进报告的只有这个 hash（不是配置本身——里面有 base url 等不该复制进每份
// 报告的东西）；两轮 hash 不同即配置不同，分布数字不可对照（HlwyComparability
// 的不变式）。输入覆盖：渠道类型、cloak 形状、参数覆盖、模型映射、AnthropicContext1M，
// 以及出站整形编排实际读取的全局形状开关（relay.OutboundShapingSettingsSnapshot：
// Claude 默认 effort、CLI shape keys、auto cache control、codex 默认 effort/fast mode/
// verbosity 等，fix-1127 返工 r2 第 5 条）。ThinkingToContent 只在回客户端方向折叠
// 思考内容，不改变世界出站长什么样，故意不在其中。
func HlwyChannelConfigHash(channel dbmodel.Channel) string {
	snapshot := struct {
		Type               outbound.OutboundType `json:"type"`
		Cloak              dbmodel.ChannelCloak  `json:"cloak"`
		ParamOverride      *string               `json:"param_override,omitempty"`
		ModelMapping       map[string]string     `json:"model_mapping"`
		AnthropicContext1M bool                  `json:"anthropic_context_1m"`
		Settings           map[string]string     `json:"settings"`
	}{
		Type:               channel.Type,
		Cloak:              channel.Cloak,
		ParamOverride:      channel.ParamOverride,
		ModelMapping:       channel.ModelMapping,
		AnthropicContext1M: channel.AnthropicContext1M,
		Settings:           relay.OutboundShapingSettingsSnapshot(),
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
