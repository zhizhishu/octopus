package modeltest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/modelverify"
	transformermodel "github.com/zhizhishu/octopus/internal/transformer/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// defaultProbeEndpoint 未指定端点时使用的入站协议。与渠道检测的默认一致。
const defaultProbeEndpoint = "openai_chat"

// ErrProbeKeysCoolingDown 渠道配了密钥，但此刻**全都处于冷却/熔断**。
//
// 必须和"没配密钥"分开：前者等一会儿自己会好，是**可重试的暂时状态**；
// 后者不修配置永远跑不了。以前两者都是同一句"渠道没有可用的密钥"，
// 于是冷却期里连点几次得到的都是"没跑成"，看着像渠道坏了。
var ErrProbeKeysCoolingDown = errors.New("渠道密钥全部在冷却中")

// ProbeSender 把模型校验探针的题目经**渠道检测的既有出站链路**发出。
//
// 它复用 testChannelKey 的全部 shape 处理——codex/claude 指纹、cloak 门控、
// wire headers、param override、流式强制——因此对上游而言，一次探针请求与一次
// 真实渠道检测请求在字节层面无法区分。探针只做两件事：替换题目、读取响应。
//
// 这正是移植 Token-Verifier 时刻意保留的边界：它的 adapter/transport 层用标准
// net/http 自拼请求，一旦照搬就会丢弃这里模拟的客户端身份。所以那两层不搬，
// 出站一律走这条链路。
type ProbeSender struct {
	channel       dbmodel.Channel
	key           dbmodel.ChannelKey
	upstreamModel string
	endpoint      string
	stream        *bool
	adapter       transformermodel.Outbound
}

// NewProbeSender 绑定一个渠道与密钥，准备重复发题。
//
// endpoint 与渠道检测同语义（openai_chat / openai_responses / anthropic_messages /
// gemini_generate_content），空值按 openai_chat 处理。密钥选取沿用 tryChannel 的
// 策略：关闭熔断的渠道取全部启用密钥，否则只取可用（未冷却）密钥。
//
// 能承接嵌入的渠道也在准入范围：出站走 EmbeddingOutbound，请求体只有 model+input，
// 不牵涉任何 chat/claude/codex 形状（见 testChannelKey 的 embeddingProbe 分支）。
// 判据用能力位（SupportsCapability）：空 Capabilities 的存量渠道退回按渠道类型判定，
// 行为与改动前完全一致；勾了 embeddings 的聊天渠道（如火山）也能进来。
func NewProbeSender(channel dbmodel.Channel, upstreamModel, endpoint string) (*ProbeSender, error) {
	// 准入按"本次要测什么"判，不能是 OR：一个普通 Chat 渠道测**嵌入**端点时，
	// relay 侧并不会调度它（能力位没有 embeddings），这里若放行就会测出一条
	// 生产走不通的"可用"，那是假绿灯。
	if channel.Type == outbound.OutboundTypeSystemOne {
		return nil, fmt.Errorf("决策模型渠道不走模型校验探针，请用渠道里的「测试」")
	}
	if isEmbeddingTestEndpoint(endpoint) {
		if !channel.SupportsCapability(dbmodel.CapabilityEmbeddings) {
			return nil, fmt.Errorf("渠道未声明 embedding 能力，无法校验嵌入端点")
		}
	} else if !outbound.IsChatChannelType(channel.Type) {
		return nil, fmt.Errorf("渠道类型 %d 不支持探针校验", channel.Type)
	}
	if strings.TrimSpace(channel.GetBaseUrl()) == "" {
		return nil, fmt.Errorf("渠道没有配置 base url")
	}
	if strings.TrimSpace(upstreamModel) == "" {
		return nil, fmt.Errorf("未指定上游模型")
	}
	keys := channel.GetAvailableChannelKeys()
	if channel.DisableCircuitBreaker {
		keys = channel.GetAllEnabledChannelKeys()
	}
	if len(keys) == 0 {
		// 三种"没有密钥"要说成三句话，因为对用户的意思是三件事：
		//   关熔断的渠道为空 → 一条启用的密钥都没有，去配；
		//   有启用密钥但全在冷却 → 等它自己恢复（可重试）；
		//   连启用密钥都没有 → 根本没配（确定性失败）。
		if channel.DisableCircuitBreaker {
			return nil, fmt.Errorf("渠道没有启用任何密钥")
		}
		if len(channel.GetAllEnabledChannelKeys()) == 0 {
			return nil, fmt.Errorf("渠道没有配置密钥")
		}
		return nil, fmt.Errorf("%w，恢复后会自动重试", ErrProbeKeysCoolingDown)
	}
	adapter := modelTestOutboundAdapter(channel.Type, endpoint)
	if adapter == nil {
		return nil, fmt.Errorf("渠道类型 %d 没有对应的出站适配器", channel.Type)
	}
	if strings.TrimSpace(endpoint) == "" {
		endpoint = defaultProbeEndpoint
	}
	return &ProbeSender{
		channel:       channel,
		key:           keys[0],
		upstreamModel: upstreamModel,
		endpoint:      endpoint,
		adapter:       adapter,
	}, nil
}

// Ask 实现 modelverify.Sender：发一条题，返回归一后的响应。
func (s *ProbeSender) Ask(ctx context.Context, ask modelverify.Ask) (modelverify.Response, error) {
	prompt := strings.TrimSpace(ask.Prompt)
	// 多轮形态（签名回放）不带 Prompt，题目全在轮次里。
	if len(ask.Turns) == 0 && prompt == "" {
		return modelverify.Response{}, fmt.Errorf("探针题目为空")
	}
	r := &modelRunner{}
	r.request = dbmodel.ModelTestRequest{
		Endpoint: s.endpoint,
		Prompt:   prompt,
		Stream:   s.stream,
	}
	// 把探针要求的输出预算透传给出站链路。是否真的采用由 testChannelKey
	// 决定——Anthropic 渠道会忽略它以保持 claude-cli 形状。
	if ask.MaxTokens != nil && *ask.MaxTokens > 0 {
		r.probeMaxTokens = *ask.MaxTokens
	}
	// 多轮形态：把探针的轮次翻成出站消息。留空则走原有单轮 Prompt 路径。
	if len(ask.Turns) > 0 {
		r.probeMessages = probeMessagesFrom(ask.Turns)
	}
	if ask.Thinking {
		r.probeThinking = true
	}
	status, parsed, err := r.testChannelKey(ctx, s.adapter, &s.channel, s.key, s.upstreamModel)
	if err != nil {
		// 带状态码的类型化错误：判定层据此区分「上游明确拒收」（4xx，对签名
		// 回放探针是判定级观测）与「网络/5xx 没跑成」（不算证据）。
		return modelverify.Response{}, &modelverify.UpstreamStatusError{Status: status, Err: err}
	}
	resp := ProbeResponseFrom(parsed)
	// ---- 信封 ----
	//
	// 全部是收到回复之后才读得到的东西，不改任何出站字节（见 modelverify.Response
	// 里那一节注释）。它们只给来源规则表用（behavior/provenance.go）。
	//
	// 自报模型名优先取审计专用字段（Gemini 只写那里），空则回落 Model——
	// 与 relay 侧 captureUpstreamDeclaredModel 同一口径。
	// 流式回复的合成消息不带模型名，走旁路；非流式就在 parsed 上。
	resp.DeclaredModel = strings.TrimSpace(r.lastStreamEnvelope.DeclaredModel)
	if resp.DeclaredModel == "" {
		resp.DeclaredModel = strings.TrimSpace(parsed.UpstreamDeclaredModel)
	}
	if resp.DeclaredModel == "" {
		resp.DeclaredModel = strings.TrimSpace(parsed.Model)
	}
	// 编号与签名形态只在 Anthropic 出站路径上取：只有那条路径上的 id / tool id
	// 是上游原文（见 transformer/outbound/authropic 的 TransformResponse /
	// TransformStream）。其它出站适配器可能自己重写编号，拿它们当云端线索
	// 就是凭空的证据。
	if s.channel.Type == outbound.OutboundTypeAnthropic {
		// 流式回复的编号走旁路（runner 的 lastStreamEnvelope，合成消息里没有）；
		// 非流式回复的编号就在 parsed 上。
		resp.ResponseID = strings.TrimSpace(r.lastStreamEnvelope.ResponseID)
		if resp.ResponseID == "" {
			resp.ResponseID = strings.TrimSpace(parsed.ID)
		}
		resp.FirstToolCallID = strings.TrimSpace(r.lastStreamEnvelope.FirstToolCallID)
		if resp.FirstToolCallID == "" {
			resp.FirstToolCallID = probeFirstNonEmptyToolCallID(parsed)
		}
		// 只看形态：签名原文（redacted 解码之前那一串）是否以 claude# 开头。
		// ProbeResponseFrom 已经将 redacted / 别家密文分流掉了，剩下的
		// resp.Signature 就是上游原文。
		resp.SignatureClaudeHash = strings.HasPrefix(resp.Signature, "claude#")
	}
	// 响应头家族：runner 在收到 2xx 回复时抄好的（只记类别、不记原值）。
	resp.HeaderFamilies = r.lastHeaderFamilies
	return resp, nil
}

// probeFirstNonEmptyToolCallID 取响应里第一个非空 tool call id（Message 与 Delta 都看）。
//
// 与 relay 侧 firstNonEmptyToolCallID 同口径（那里未导出，且不在可 import 的公共面，
// 所以本地实现一份；两边要一致时以 relay 侧为准）。
func probeFirstNonEmptyToolCallID(resp *transformermodel.InternalLLMResponse) string {
	if resp == nil {
		return ""
	}
	for i := range resp.Choices {
		for _, msg := range []*transformermodel.Message{resp.Choices[i].Message, resp.Choices[i].Delta} {
			if msg == nil {
				continue
			}
			for _, toolCall := range msg.ToolCalls {
				if id := strings.TrimSpace(toolCall.ID); id != "" {
					return id
				}
			}
		}
	}
	return ""
}

// probeMessagesFrom 把探针的轮次翻成内部消息形态。
//
// assistant 轮的 Signature 是 harvest 阶段从上游拿到的密文，回放阶段原样还
// 回去；Thinking 是同块采集的思考正文——出站护栏要求普通签名必须带非空
// 正文才构造 thinking 块（防上游 400），只回签名不回正文等于没回放。
// redacted 形态不需要正文（redacted_thinking 块只带 data）。
func probeMessagesFrom(turns []modelverify.Turn) []transformermodel.Message {
	out := make([]transformermodel.Message, 0, len(turns))
	for _, t := range turns {
		msg := transformermodel.Message{Role: t.Role}
		if t.Content != "" {
			content := t.Content
			msg.Content = transformermodel.MessageContent{Content: &content}
		}
		if t.Thinking != "" {
			thinking := t.Thinking
			msg.ReasoningContent = &thinking
		}
		if t.Signature != "" {
			sig := t.Signature
			if t.Redacted {
				sig = transformermodel.EncodeRedactedThinkingSignature(sig)
			}
			msg.ReasoningSignature = &sig
		}
		out = append(out, msg)
	}
	return out
}

// Protocol 返回该渠道实际使用的出站协议 id，用于决定 think-effort 的观测单位。
//
// 取渠道类型而非入站端点：Anthropic 渠道的出站**永远**是 /v1/messages，
// 与测试页选了什么入站端点无关（见 testChannelKey 的注释）。用入站端点会让
// 单位判定错位，而同一 cell 内两侧单位必须一致，否则秩检验失效。
func (s *ProbeSender) Protocol() string {
	return probeProtocolOf(s.channel.Type)
}

// InjectedFamilies 报告本次出站会注入的「别家身份」所属家族。
//
// 用途只有一处：复述系统提示词探针要扣掉 oct 自己注入的文字（见
// behavior/system_prompt_probe.go）。伪装生效时，出站会带上 Claude CLI / Codex
// 的身份提示（system 段的身份声明、指令），模型复述系统提示词时会把它们原样
// 带出来——不扣掉的话，一个正常的伪装渠道会被自己的伪装判成“别家平台痕迹”。
//
// 哪些算“注入”：
//   - 渠道类型本身就是那家协议（Anthropic → anthropic，Responses → openai），
//     这条与是否伪装无关，协议身份是出站形状的一部分；
//   - 伪装形状（Cloak.Shape）：claude → anthropic，codex → openai。
//
// 装扮开关关掉（cloak=never）时一律空：那时既没有 CLI 身份头，也没有
// 身份注入，不该扣任何东西。
//
// 注：model-test 路径不会把渠道 / 接入方案的 system prompt 覆盖（SystemPromptOverride）
// 加进探针请求——那些层只在 relay 正向转发里应用（internal/relay/prompt_override.go:39
// 的 applyPromptOverrides，调用点只有 relay/relay.go:539 与 relay/race_group.go:129）。
// 所以这里不需为它额外并入家族。
func (s *ProbeSender) InjectedFamilies() []string {
	if !shouldApplyChannelCloak(s.channel.Cloak) {
		return nil
	}
	families := make([]string, 0, 2)
	add := func(family string) {
		for _, existing := range families {
			if existing == family {
				return
			}
		}
		families = append(families, family)
	}
	switch s.channel.Type {
	case outbound.OutboundTypeAnthropic:
		add("anthropic")
	case outbound.OutboundTypeOpenAIResponse:
		add("openai")
	}
	switch strings.ToLower(strings.TrimSpace(s.channel.Cloak.Shape)) {
	case "claude":
		add("anthropic")
	case "codex":
		add("openai")
	}
	if len(families) == 0 {
		return nil
	}
	return families
}

func probeProtocolOf(t outbound.OutboundType) string {
	switch t {
	case outbound.OutboundTypeAnthropic:
		return "anthropic-messages"
	case outbound.OutboundTypeOpenAIResponse:
		return "openai-responses"
	case outbound.OutboundTypeGemini:
		return "gemini-generate-content"
	default:
		return "openai-chat"
	}
}

// ProbeResponseFrom 把内部响应归一为探针可消费的观测输入。
//
// 流式与非流式的差异在这里抹平：流式响应落在 Delta、非流式落在 Message，
// 两者结构相同，取到哪个用哪个。
func ProbeResponseFrom(resp *transformermodel.InternalLLMResponse) modelverify.Response {
	out := modelverify.Response{}
	if resp == nil {
		return out
	}
	if len(resp.Choices) > 0 {
		choice := resp.Choices[0]
		msg := choice.Message
		if msg == nil {
			msg = choice.Delta
		}
		if msg != nil {
			out.Content = messageText(msg)
			if msg.ReasoningContent != nil {
				out.ReasoningContent = *msg.ReasoningContent
			}
			// 签名分流。Message.ReasoningSignature 是所有协议共用的一条通道，
			// 四种来源混在里面（见 transformer/model/reasoning_signature.go）：
			//   Anthropic thinking        -> 裸 base64，无标签  ← 我们要的
			//   Anthropic redacted        -> "redacted_thinking:" 前缀
			//   Gemini thoughtSignature   -> "\x00rs-gemini:" 前缀
			//   OpenAI encrypted_content  -> "\x00rs-openai-enc:" 前缀
			// 后两者是别家密文，绝不能被当成 Claude 签名去判真假——
			// 那会把一个正常的 Gemini 渠道误判成"Claude 替身"。
			if msg.ReasoningSignature != nil {
				switch {
				case transformermodel.IsRedactedThinkingSignature(msg.ReasoningSignature):
					data, _ := transformermodel.DecodeRedactedThinkingSignature(*msg.ReasoningSignature)
					out.RedactedSignature = data
				case transformermodel.HasProviderReasoningTag(msg.ReasoningSignature):
					// 其它 provider 的密文，与 Claude 签名无关，丢弃。
				default:
					out.Signature = *msg.ReasoningSignature
				}
			}
			for _, tc := range msg.ToolCalls {
				out.ToolCalls = append(out.ToolCalls, modelverify.ToolCall{
					Name: tc.Function.Name,
					Args: json.RawMessage(tc.Function.Arguments),
				})
			}
		}
		if choice.FinishReason != nil {
			out.FinishReason = *choice.FinishReason
		}
	}
	if resp.Usage != nil {
		out.Usage = modelverify.Usage{
			Prompt:     int(resp.Usage.PromptTokens),
			Completion: int(resp.Usage.CompletionTokens),
		}
		// 在场标记：上游没报思考用量字段时 Reasoning 保持 0，但 Reported=false，
		// 评分层据此区分「明确为 0」与「没有数据」，不把缺失当异常扣分。
		if resp.Usage.CompletionTokensDetails != nil {
			out.Usage.Reasoning = int(resp.Usage.CompletionTokensDetails.ReasoningTokens)
			out.Usage.ReasoningReported = true
		}
		if resp.Usage.PromptTokensDetails != nil {
			out.Usage.Cached = int(resp.Usage.PromptTokensDetails.CachedTokens)
		}
		out.Usage.Input = probeBilledInputTokens(resp.Usage)
	}
	return out
}

// probeBilledInputTokens 上游计费的全部输入 token（含缓存读写）。
//
// Anthropic 口径（以及把 Anthropic 式缓存字段透出的 OpenAI 兼容代理）的
// input_tokens 不含缓存读写，要加回；OpenAI 口径的 prompt_tokens 已含缓存命中，
// 再加就重复。判据沿用 transformer 层已有的两个语义标记，不在这里另起一套。
func probeBilledInputTokens(u *transformermodel.Usage) int {
	if u == nil {
		return 0
	}
	total := u.PromptTokens
	if u.AnthropicUsage || u.SeparateCacheInputTokens {
		if u.PromptTokensDetails != nil {
			total += u.PromptTokensDetails.CachedTokens
		}
		total += u.CacheCreationInputTokens
	}
	return int(total)
}
