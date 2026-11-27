package modeltest

import (
	"context"
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
	sender, err := NewRelaySender(channel, upstreamModel, endpoint)
	if err != nil {
		return unknown
	}
	spec, err := sender.buildRelayRequest(modelverify.Ask{Prompt: numberdist.Prompt})
	if err != nil {
		return unknown
	}
	inAdapter := inbound.Get(sender.inboundType)
	outAdapter := outbound.Get(channel.Type)
	if inAdapter == nil || outAdapter == nil {
		return unknown
	}
	ctx := context.Background()
	if sender.inboundType == inbound.InboundTypeGemini {
		ctx = geminiInbound.WithRequestOptions(ctx, sender.requestModel, false)
	}
	internalReq, err := inAdapter.TransformRequest(ctx, spec.body)
	if err != nil || internalReq == nil {
		return unknown
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

	key := ""
	if keys := channel.GetAvailableChannelKeys(); len(keys) > 0 {
		key = keys[0].ChannelKey
	}
	req, err := outAdapter.TransformRequest(ctx, internalReq, channel.GetBaseUrl(), key)
	if err != nil || req == nil {
		return unknown
	}
	if err := relay.ApplyParamOverride(req, channel.ParamOverride); err != nil {
		return unknown
	}
	conditions := buildOutboundConditions(probeProtocolOf(channel.Type), req)
	conditions.ConfigHash = HlwyChannelConfigHash(channel)
	return conditions
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
// 的不变式）。输入覆盖：渠道类型、cloak 形状、参数覆盖、模型映射、AnthropicContext1M、
// ThinkingToContent——都是出站形状的确定性来源。
func HlwyChannelConfigHash(channel dbmodel.Channel) string {
	snapshot := struct {
		Type               outbound.OutboundType `json:"type"`
		Cloak              dbmodel.ChannelCloak  `json:"cloak"`
		ParamOverride      *string               `json:"param_override,omitempty"`
		ModelMapping       map[string]string     `json:"model_mapping"`
		AnthropicContext1M bool                  `json:"anthropic_context_1m"`
		ThinkingToContent  bool                  `json:"thinking_to_content"`
	}{
		Type:               channel.Type,
		Cloak:              channel.Cloak,
		ParamOverride:      channel.ParamOverride,
		ModelMapping:       channel.ModelMapping,
		AnthropicContext1M: channel.AnthropicContext1M,
		ThinkingToContent:  channel.ThinkingToContent,
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
