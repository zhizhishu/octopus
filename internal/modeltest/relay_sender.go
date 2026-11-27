package modeltest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/modelverify"
	"github.com/zhizhishu/octopus/internal/relay"
	"github.com/zhizhishu/octopus/internal/transformer/inbound"
	geminiInbound "github.com/zhizhishu/octopus/internal/transformer/inbound/gemini"
	transformermodel "github.com/zhizhishu/octopus/internal/transformer/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// ErrChannelGateBusy 标记一次发送被**本地渠道闸门**（并发位/RPM 名额）挡下——
// 请求一次都没发到上游。这不是上游拒收：渠道只是此刻满，等到名额空出来
// （RPM 整分钟桶滚动/并发释放）再发就是。调用方用 errors.Is 判定，
// 文案本身就是归因子「渠道限流（本地）」（fix-1127 返工，用户拍板）。
var ErrChannelGateBusy = errors.New("渠道限流（本地）")

// IsChannelGateBusy 判定错误是不是本地闸门忙（含 UpstreamStatusError 包装）。
func IsChannelGateBusy(err error) bool {
	return errors.Is(err, ErrChannelGateBusy)
}

// ChannelGateBusyWait 给出闸门忙后的建议等待：RPM 是整分钟桶（见
// balancer/channel_gate.go），等不到下一分钟名额不会重置；并发位通常远早于此
// 释放，但等待上层本就在异步采样/实验里，正确性优先。下一分钟边界 + 一点余量。
func ChannelGateBusyWait() time.Duration {
	now := time.Now()
	next := now.Truncate(time.Minute).Add(time.Minute)
	return time.Until(next) + 200*time.Millisecond
}

// RelaySender 把渠道测试 / 模型检测（含鹈鹕 creative）的题目经**正常 relay 转发
// 链路**发出去（用户 2026-11-26 拍板：学 new-api controller/channel-test.go，
// 不再用 modeltest 自建探针出站，也不需要任何 API Key）。
//
// 与 ProbeSender 的分工：
//   - ProbeSender 仍服务断路器定时探测（探针契约不动，shape 由 testChannelKey
//     那套负责）；红队实验自 fix-1127 起也走 RelaySender。
//   - RelaySender 是渠道测试/模型检测的出站：进程内合成一个 gin 请求（不带任何
//     身份），直接调 relay.HandlerForChannelTest——被测渠道以参数直指定。上游
//     指纹/形状/cloak/闸门/attempts/转发日志全部走生产同一条链路——对上游而言，
//     一次检测请求与一次真实转发在字节层面无法区分；对运营而言，转发日志里能
//     看到这次检测的 attempts、耗时、费用，request_source=model_test。
//
// 测试语义（relay/channel_test_entry.go）：候选只有被测渠道，不跳渠道、无兜底、
// 无救援、不占用户额度、不注入提示词规则。渠道 RPM 照计（fix-1127 用户拍板，与正常
// 请求同一计数点）。渠道停用不拦（测一条
// 停用渠道是合法操作）。没保存（ID<=0）的渠道不许测——准入直接拒，见下。
type RelaySender struct {
	channel      dbmodel.Channel
	requestModel string
	endpoint     string
	inboundType  inbound.InboundType
	// suppressRelayLog：批量采样（hlwy）要求逐请求不写转发日志行（fix-1127
	// 第 3 项：一轮采样只落一条汇总日志，不刷 100–200 条）。
	suppressRelayLog bool
}

// SetSuppressRelayLog 让之后每次 Send 都不落逐请求转发日志行（汇总行由调用方
// 在整轮结束时自落）。单次的渠道测试/检测不要用——那条日志正是运营要的观测。
func (s *RelaySender) SetSuppressRelayLog(v bool) {
	s.suppressRelayLog = v
}

// relayAnthropicMaxTokens 是 Anthropic 入站必须携带的输出预算。
// Anthropic messages 协议 max_tokens 必填；检测题（画 SVG 等）需要完整输出，
// 像正常 claude 客户端一样给 32000。其余协议（OpenAI/Gemini 系）不强夹
// max_tokens——调用方（modelverify.Ask.MaxTokens）显式要预算才带。
const relayAnthropicMaxTokens = 32000

// relayInternalUserAgent 是内部合成请求的观测用 UA（只进 metrics 的客户端识别
// 字段，出站 UA 由 relay 的 header 默认值统一覆盖，不影响上游形状）。
const relayInternalUserAgent = "octopus-internal-test/1.0"

// NewRelaySender 绑定被测渠道与模型，准备经 relay 链路发题。
//
// endpoint 语义与 NewProbeSender 一致（openai_chat / openai_responses /
// anthropic_messages / gemini_generate_content / embeddings；空值按 openai_chat，
// custom 视渠道类型处理）。准入检查也保持同款错误口径（密钥三句话、embedding
// 能力位、base url），调用方（model_audit_worker 等）依赖这些错误区分
// 「确定性失败」与「可重试」。
func NewRelaySender(channel dbmodel.Channel, requestModel, endpoint string) (*RelaySender, error) {
	// 用户 2026-11-26 拍板：没保存的渠道不许测。pin 对 ID<=0 本就只能空转，
	// 这里在准入直接说人话，别让请求落进正常路由撞出莫名其妙的 404。
	if channel.ID <= 0 {
		return nil, errors.New("渠道未保存，请先保存再测试")
	}
	if channel.Type == outbound.OutboundTypeSystemOne {
		return nil, fmt.Errorf("决策模型渠道不走 relay 检测，请用渠道里的「测试」")
	}
	endpointName := strings.TrimSpace(endpoint)
	if endpointName == "" {
		endpointName = defaultProbeEndpoint
	}
	if isEmbeddingTestEndpoint(endpointName) {
		if !channel.SupportsCapability(dbmodel.CapabilityEmbeddings) {
			return nil, fmt.Errorf("渠道未声明 embedding 能力，无法校验嵌入端点")
		}
	} else if !outbound.IsChatChannelType(channel.Type) {
		return nil, fmt.Errorf("渠道类型 %d 不支持 relay 检测", channel.Type)
	}
	if strings.TrimSpace(channel.GetBaseUrl()) == "" {
		return nil, fmt.Errorf("渠道没有配置 base url")
	}
	if strings.TrimSpace(requestModel) == "" {
		return nil, fmt.Errorf("未指定上游模型")
	}
	// 密钥可用性预检：与 NewProbeSender 同一套三句话。relay 链路自己会按生产
	// 口径选 key / 冷却，这里只是为了把「配置缺失 / 全在冷却」在发请求前说清楚，
	// 让 model_audit_worker 的重试语义保持不变。
	keys := channel.GetAvailableChannelKeys()
	if channel.DisableCircuitBreaker {
		keys = channel.GetAllEnabledChannelKeys()
	}
	if len(keys) == 0 {
		if channel.DisableCircuitBreaker {
			return nil, fmt.Errorf("渠道没有启用任何密钥")
		}
		if len(channel.GetAllEnabledChannelKeys()) == 0 {
			return nil, fmt.Errorf("渠道没有配置密钥")
		}
		return nil, fmt.Errorf("%w，恢复后会自动重试", ErrProbeKeysCoolingDown)
	}
	inboundType, err := relayInboundFor(endpointName)
	if err != nil {
		return nil, err
	}
	return &RelaySender{
		channel:      channel,
		requestModel: strings.TrimSpace(requestModel),
		endpoint:     endpointName,
		inboundType:  inboundType,
	}, nil
}

// relayInboundFor 把测试端点名映射成 relay 入站协议与合成请求路径。
func relayInboundFor(endpointName string) (inbound.InboundType, error) {
	normalized, err := normalizeEndpoint(endpointName)
	if err != nil {
		return 0, err
	}
	switch normalized.name {
	case "openai_chat", customTestEndpointName:
		return inbound.InboundTypeOpenAIChat, nil
	case "openai_responses":
		return inbound.InboundTypeOpenAIResponse, nil
	case "anthropic_messages":
		return inbound.InboundTypeAnthropic, nil
	case "gemini_generate_content":
		return inbound.InboundTypeGemini, nil
	case embeddingTestEndpointName:
		return inbound.InboundTypeOpenAIEmbedding, nil
	default:
		// systemone / images 等端点在调用侧已被各自的分支接走，到这里说明
		// 出现了新的端点名：拒绝而不是悄悄按 chat 发。
		return 0, fmt.Errorf("测试端点 %s 不支持经 relay 链路发送", endpointName)
	}
}

// relayRequestSpec 是一次合成请求的形状。
type relayRequestSpec struct {
	method string
	path   string
	body   []byte
}

// buildRelayRequest 按入站协议渲染请求体。stream 一律 false：内部调用方要的是
// 整段回复，relay 出站侧若对该渠道强制流式（Anthropic 场景）会自行聚合成一个
// 完整 JSON 再写给客户端——这正是普通客户端非流式调用的同款行为。
func (s *RelaySender) buildRelayRequest(ask modelverify.Ask) (relayRequestSpec, error) {
	prompt := strings.TrimSpace(ask.Prompt)
	if len(ask.Turns) == 0 && prompt == "" {
		return relayRequestSpec{}, fmt.Errorf("探针题目为空")
	}
	model := s.requestModel
	var spec relayRequestSpec
	var err error
	switch s.inboundType {
	case inbound.InboundTypeOpenAIChat:
		body := map[string]any{"model": model, "stream": false}
		if len(ask.Turns) > 0 {
			// 多轮回放（签名探针）只在 Anthropic 出站渠道上有意义（见 Protocol 的
			// 口径）；OpenAI 入站没有 thinking/signature 的回放通道，退化为拼接
			// 的单轮题目——签名探针不会走到这里（它按 Protocol 过滤渠道）。
			var sb strings.Builder
			for _, turn := range ask.Turns {
				if turn.Content != "" {
					if sb.Len() > 0 {
						sb.WriteString("\n\n")
					}
					sb.WriteString(turn.Content)
				}
			}
			body["messages"] = []any{map[string]any{"role": "user", "content": sb.String()}}
		} else {
			body["messages"] = []any{map[string]any{"role": "user", "content": prompt}}
		}
		if ask.MaxTokens != nil && *ask.MaxTokens > 0 {
			body["max_tokens"] = *ask.MaxTokens
		}
		spec = relayRequestSpec{method: http.MethodPost, path: "/v1/chat/completions"}
		spec.body, err = json.Marshal(body)
		if err != nil {
			return spec, err
		}
	case inbound.InboundTypeOpenAIResponse:
		body := map[string]any{"model": model, "stream": false}
		if len(ask.Turns) > 0 {
			input := make([]any, 0, len(ask.Turns))
			for _, turn := range ask.Turns {
				input = append(input, map[string]any{"role": turn.Role, "content": turn.Content})
			}
			body["input"] = input
		} else {
			body["input"] = prompt
		}
		if ask.MaxTokens != nil && *ask.MaxTokens > 0 {
			body["max_output_tokens"] = *ask.MaxTokens
		}
		spec = relayRequestSpec{method: http.MethodPost, path: "/v1/responses"}
		spec.body, err = json.Marshal(body)
		if err != nil {
			return spec, err
		}
	case inbound.InboundTypeAnthropic:
		body := map[string]any{
			"model":      model,
			"max_tokens": relayAnthropicMaxTokens,
			"stream":     false,
		}
		if ask.Thinking {
			// 真 claude 客户端开思考都带预算；1024 是出站护栏认可的最小值
			// （budget floor），足够签名探针采到 thinking 块。
			body["thinking"] = map[string]any{"type": "enabled", "budget_tokens": 1024}
		}
		messages := make([]any, 0, max(1, len(ask.Turns)))
		if len(ask.Turns) > 0 {
			for _, turn := range ask.Turns {
				messages = append(messages, relayAnthropicTurnMessage(turn))
			}
		} else {
			messages = append(messages, map[string]any{"role": "user", "content": prompt})
		}
		body["messages"] = messages
		spec = relayRequestSpec{method: http.MethodPost, path: "/v1/messages"}
		spec.body, err = json.Marshal(body)
		if err != nil {
			return spec, err
		}
	case inbound.InboundTypeGemini:
		text := prompt
		if len(ask.Turns) > 0 {
			var sb strings.Builder
			for _, turn := range ask.Turns {
				if turn.Content != "" {
					if sb.Len() > 0 {
						sb.WriteString("\n\n")
					}
					sb.WriteString(turn.Content)
				}
			}
			text = sb.String()
		}
		body := map[string]any{
			"contents": []any{map[string]any{
				"role":  "user",
				"parts": []any{map[string]any{"text": text}},
			}},
		}
		if ask.MaxTokens != nil && *ask.MaxTokens > 0 {
			body["generationConfig"] = map[string]any{"maxOutputTokens": *ask.MaxTokens}
		}
		spec = relayRequestSpec{
			method: http.MethodPost,
			path:   "/v1beta/models/" + model + ":generateContent",
		}
		spec.body, err = json.Marshal(body)
		if err != nil {
			return spec, err
		}
	case inbound.InboundTypeOpenAIEmbedding:
		body := map[string]any{"model": model, "input": prompt}
		spec = relayRequestSpec{method: http.MethodPost, path: "/v1/embeddings"}
		spec.body, err = json.Marshal(body)
		if err != nil {
			return spec, err
		}
	default:
		return spec, fmt.Errorf("relay 检测不支持入站类型 %d", s.inboundType)
	}
	return spec, nil
}

// relayAnthropicTurnMessage 把一条回放轮翻成 anthropic 客户端形状的消息：
// assistant 轮的思考（含签名 / redacted）按真实客户端的 content 块序列回放。
func relayAnthropicTurnMessage(turn modelverify.Turn) map[string]any {
	if turn.Role != "assistant" {
		content := turn.Content
		if content == "" {
			content = " "
		}
		return map[string]any{"role": turn.Role, "content": content}
	}
	blocks := make([]any, 0, 2)
	if turn.Thinking != "" || turn.Signature != "" {
		if turn.Redacted {
			data := turn.Signature
			if data == "" {
				data = turn.Thinking
			}
			blocks = append(blocks, map[string]any{"type": "redacted_thinking", "data": data})
		} else {
			blocks = append(blocks, map[string]any{
				"type":      "thinking",
				"thinking":  turn.Thinking,
				"signature": turn.Signature,
			})
		}
	}
	if turn.Content != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": turn.Content})
	}
	if len(blocks) == 0 {
		blocks = append(blocks, map[string]any{"type": "text", "text": " "})
	}
	return map[string]any{"role": "assistant", "content": blocks}
}

// Send 经 relay 链路发一次题，返回 (HTTP 状态码, 最终内部响应, 错误)。
//
// 成功判定：relay 把最终内部响应回写在 gin context 上（relay.InternalPinnedResponse），
// 且合成响应的状态码是 2xx。失败时错误带状态码（modelverify.UpstreamStatusError），
// 便于判定层区分「上游明确拒收」与「没跑成」。
func (s *RelaySender) Send(ctx context.Context, ask modelverify.Ask) (int, *transformermodel.InternalLLMResponse, error) {
	spec, err := s.buildRelayRequest(ask)
	if err != nil {
		return 0, nil, err
	}
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	httpReq := httptest.NewRequest(spec.method, spec.path, bytes.NewReader(spec.body))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", relayInternalUserAgent)
	reqCtx := ctx
	if s.inboundType == inbound.InboundTypeGemini {
		// gemini 入站的 model/stream 来自 URL 动作而非请求体（与 handlers/gemini.go
		// 同一口径），必须在请求上下文里带过去。
		reqCtx = geminiInbound.WithRequestOptions(ctx, s.requestModel, false)
	}
	httpReq = httpReq.WithContext(reqCtx)
	ginCtx.Request = httpReq

	// 身份全零值：没有任何 API Key/用户。统计与 last_used 的 op 层函数对零 id
	// 本来就是空操作；user_role 给 admin 只是让身份语义自洽（测试入口在 relay
	// 里已整段跳过提示词注入，与角色无关）。
	if s.suppressRelayLog {
		relay.MarkSuppressInternalTestRelayLog(ginCtx)
	}
	ginCtx.Set("api_key_id", 0)
	ginCtx.Set("user_id", 0)
	ginCtx.Set("user_role", string(dbmodel.UserRoleAdmin))
	ginCtx.Set("request_ip", "127.0.0.1")
	ginCtx.Set("supported_models", "")

	relay.HandlerForChannelTest(s.inboundType, ginCtx, s.channel)

	status := recorder.Code
	parsed := relay.InternalTestResponse(ginCtx)
	if status >= 400 || parsed == nil {
		body := recorder.Body.Bytes()
		message := relayErrorMessage(body)
		if message == "" {
			message = fmt.Sprintf("relay 转发失败（HTTP %d）", status)
		}
		// 本地渠道闸门终态（relay.go channelRateLimitedCode，一次都没发到上游）
		// 必须与上游 429 分开归因：这是「渠道此刻满」，等名额空了再发即可，
		// 不是上游拒收。判定靠响应体里我们自己的错误码——上游 429 的转发体
		// 不会带这个字面量。
		if status == http.StatusTooManyRequests && bytes.Contains(body, []byte("channel_rate_limited")) {
			return status, nil, &modelverify.UpstreamStatusError{Status: status, Err: ErrChannelGateBusy}
		}
		return status, nil, &modelverify.UpstreamStatusError{Status: status, Err: errors.New(message)}
	}
	return status, parsed, nil
}

// relayErrorMessage 从 relay 写给客户端的错误响应体里取人话错误（OpenAI / Anthropic /
// Gemini 三种错误形状都试一遍），失败回落空串。
func relayErrorMessage(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var generic map[string]json.RawMessage
	if err := json.Unmarshal(body, &generic); err != nil {
		return ""
	}
	readString := func(raw json.RawMessage) string {
		if len(raw) == 0 {
			return ""
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return ""
		}
		return s
	}
	if msg := readString(generic["message"]); msg != "" {
		return msg
	}
	for _, key := range []string{"error", "err"} {
		raw, ok := generic[key]
		if !ok {
			continue
		}
		// {"error": "plain"} 或 {"error": {"message": "..."}}
		if s := readString(raw); s != "" {
			return s
		}
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(raw, &nested); err == nil {
			if s := readString(nested["message"]); s != "" {
				return s
			}
		}
	}
	return ""
}

// Ask 实现 modelverify.Sender：发一条题，返回归一后的响应。
//
// 信封字段从 relay 回写的最终内部响应里取（与 ProbeSender 的口径对齐但来源不同：
// 那边从探针自己的解析取，这边从 relay 的聚合结果取）。HeaderFamilies 拿不到
// （那是出站响应头的观测，只在探针链路里有）——依赖它的探针继续用 ProbeSender
// （断路器/红队路径，不在本次切换范围）。
func (s *RelaySender) Ask(ctx context.Context, ask modelverify.Ask) (modelverify.Response, error) {
	_, parsed, err := s.Send(ctx, ask)
	if err != nil {
		return modelverify.Response{}, err
	}
	resp := ProbeResponseFrom(parsed)
	resp.DeclaredModel = strings.TrimSpace(parsed.UpstreamDeclaredModel)
	if resp.DeclaredModel == "" {
		resp.DeclaredModel = strings.TrimSpace(parsed.Model)
	}
	if s.channel.Type == outbound.OutboundTypeAnthropic {
		resp.ResponseID = strings.TrimSpace(parsed.ID)
		resp.FirstToolCallID = probeFirstNonEmptyToolCallID(parsed)
		resp.SignatureClaudeHash = strings.HasPrefix(resp.Signature, "claude#")
	}
	return resp, nil
}

// Protocol 与 ProbeSender 同口径：渠道出站协议 id，决定 think-effort 的观测单位。
func (s *RelaySender) Protocol() string {
	return probeProtocolOf(s.channel.Type)
}

// InjectedFamilies 与 ProbeSender 同口径：伪装身份注入的家族，供复述系统提示词
// 探针扣除注入文字。relay 链路的 cloak / 协议身份与探针出站同一套配置。
func (s *RelaySender) InjectedFamilies() []string {
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

// RelaySenderRequestModel 暴露发送器绑定的模型名（调试/测试用）。
func (s *RelaySender) RelaySenderRequestModel() string {
	return s.requestModel
}
