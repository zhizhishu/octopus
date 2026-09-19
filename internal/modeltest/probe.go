package modeltest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/modelverify"
	transformermodel "github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

// defaultProbeEndpoint 未指定端点时使用的入站协议。与渠道检测的默认一致。
const defaultProbeEndpoint = "openai_chat"

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
func NewProbeSender(channel dbmodel.Channel, upstreamModel, endpoint string) (*ProbeSender, error) {
	if !outbound.IsChatChannelType(channel.Type) {
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
		return nil, fmt.Errorf("渠道没有可用的密钥")
	}
	adapter := outbound.Get(channel.Type)
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
		return modelverify.Response{}, fmt.Errorf("上游返回 %d: %w", status, err)
	}
	return ProbeResponseFrom(parsed), nil
}

// probeMessagesFrom 把探针的轮次翻成内部消息形态。
//
// 只有 assistant 轮会带签名：那是 harvest 阶段从上游拿到的密文，回放阶段原样还
// 回去。thinking 文本留空是刻意的——这是上游的既定回放形态，模型解封的是密文
// 本身，不需要我们提供明文，我们也提供不了。
func probeMessagesFrom(turns []modelverify.Turn) []transformermodel.Message {
	out := make([]transformermodel.Message, 0, len(turns))
	for _, t := range turns {
		msg := transformermodel.Message{Role: t.Role}
		if t.Content != "" {
			content := t.Content
			msg.Content = transformermodel.MessageContent{Content: &content}
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
		if resp.Usage.CompletionTokensDetails != nil {
			out.Usage.Reasoning = int(resp.Usage.CompletionTokensDetails.ReasoningTokens)
		}
		if resp.Usage.PromptTokensDetails != nil {
			out.Usage.Cached = int(resp.Usage.PromptTokensDetails.CachedTokens)
		}
	}
	return out
}
