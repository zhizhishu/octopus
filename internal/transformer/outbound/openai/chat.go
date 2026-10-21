package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/samber/lo"
	"github.com/zhizhishu/octopus/internal/transformer/model"
	"github.com/zhizhishu/octopus/internal/utils/xurl"
)

type ChatOutbound struct{}

func (o *ChatOutbound) TransformRequest(ctx context.Context, request *model.InternalLLMRequest, baseUrl, key string) (*http.Request, error) {
	return transformChatRequest(ctx, request, baseUrl, key, false)
}

type CustomChatOutbound struct{}

func (o *CustomChatOutbound) TransformRequest(ctx context.Context, request *model.InternalLLMRequest, baseUrl, key string) (*http.Request, error) {
	return transformChatRequest(ctx, request, baseUrl, key, true)
}

func transformChatRequest(ctx context.Context, request *model.InternalLLMRequest, baseUrl, key string, customEndpoint bool) (*http.Request, error) {
	originalToolStream := request.ToolStream
	originalThinking := request.Thinking
	originalReasoningEffort := request.ReasoningEffort
	originalMaxTokens := request.MaxTokens
	originalMaxCompletionTokens := request.MaxCompletionTokens
	defer func() {
		// The relay reuses one request across channel/key attempts. Keep this
		// provider-specific projection scoped to the body built for this attempt.
		request.ToolStream = originalToolStream
		request.Thinking = originalThinking
		request.ReasoningEffort = originalReasoningEffort
		request.MaxTokens = originalMaxTokens
		request.MaxCompletionTokens = originalMaxCompletionTokens
	}()

	// ClearHelpFields strips internal-only helper fields, but it also wipes each message's
	// reasoning_content/signature — which DeepSeek V4 REQUIRES on the tool-call assistant
	// message for multi-turn (inbound/openai/response.go injects it there). Preserve message
	// reasoning across the clear; only the chat request body is affected (other outbounds and
	// the response path keep the unchanged ClearHelpFields behaviour).
	savedReasoning := make([]*string, len(request.Messages))
	savedReasoningSig := make([]*string, len(request.Messages))
	for i := range request.Messages {
		savedReasoning[i] = request.Messages[i].ReasoningContent
		savedReasoningSig[i] = request.Messages[i].ReasoningSignature
	}
	request.ClearHelpFields()
	for i := range request.Messages {
		if request.Messages[i].ReasoningContent == nil {
			request.Messages[i].ReasoningContent = savedReasoning[i]
		}
		if request.Messages[i].ReasoningSignature == nil {
			request.Messages[i].ReasoningSignature = savedReasoningSig[i]
		}
		// A provider-tagged signature (Anthropic redacted, Gemini thoughtSignature,
		// OpenAI encrypted_content) is a foreign opaque blob that must never be
		// serialized as a chat "reasoning_signature". Drop it; a bare untagged
		// signature (DeepSeek V4 / Anthropic thinking) is preserved as before.
		//
		// Bug B "gemini cross-protocol history bridge" alignment with reference projects
		// (CLIProxyAPI openai_claude_request.go:367-377 / new-api oai_chat/to_gemini_chat_req.go /
		// sub2api chatcompletions_to_responses.go:108-118): none of the three unconditionally
		// serializes a cross-protocol reasoning block — each gates it on signature
		// compatibility or folds it to plain text. When the signature carries a foreign
		// provider tag, the ReasoningContent next to it was produced by a different upstream
		// (gemini thought, anthropic thinking, openai encrypted reasoning) and is opaque to
		// the current chat upstream. Emitting it as a "reasoning_content" field confuses
		// strict OpenAI-compatible providers (vercel/z.ai/GLM 400 on unknown field, or the
		// model mis-attributes it as live reasoning). DeepSeek V4 keeps its own
		// reasoning_content because DeepSeek signs it with a bare (untagged) signature, so
		// the HasProviderReasoningTag gate leaves it untouched. The gate is computed ONCE on
		// the live signature so a single test decides both fields atomically.
		if model.HasProviderReasoningTag(request.Messages[i].ReasoningSignature) {
			request.Messages[i].ReasoningSignature = nil
			request.Messages[i].ReasoningContent = nil
		}

		// Ensure assistant messages with tool calls have a safe empty string content
		// rather than being omitted entirely if reasoning was stripped, satisfying
		// third-party chat proxies (antigravity/gemini/qwen).
		if strings.EqualFold(strings.TrimSpace(request.Messages[i].Role), "assistant") &&
			len(request.Messages[i].ToolCalls) > 0 &&
			request.Messages[i].Content.Content == nil &&
			len(request.Messages[i].Content.MultipleContent) == 0 {
			request.Messages[i].Content.Content = lo.ToPtr("")
		}
	}

	// o-series / gpt-5 chat models need a different body shape than the rest of
	// the OpenAI Chat Completions surface (max_completion_tokens, no temperature,
	// and system->developer for most of them). Apply that rewrite first so the
	// generic developer->system demotion below does NOT run on these models.
	if isOpenAIReasoningChatModel(request.Model) && isOpenAIOfficialChatBase(baseUrl) {
		// Only the genuine OpenAI chat base is known to require the o/gpt-5 body shape
		// (max_completion_tokens, no temperature, first system->developer). A third-party
		// OpenAI-compatible provider that happens to serve a gpt-5-/o-named model may still
		// expect max_tokens/temperature/system, so route it through the generic demotion
		// path below to avoid a regression. (oct serves genuine OpenAI over /responses, so
		// this chat path is a safety net for the rare official-chat case.)
		applyOpenAIReasoningChatCompat(request)
	} else {
		// Convert developer role to system role for compatibility with
		// non-reasoning OpenAI-compatible chat upstreams that reject "developer".
		for i := range request.Messages {
			if request.Messages[i].Role == "developer" {
				request.Messages[i].Role = "system"
			}
		}
	}

	// Force stream_options.include_usage on every streaming OpenAI-compatible chat
	// request. Providers such as NVIDIA NIM (vLLM), DeepSeek, GLM and Qwen only
	// attach the aggregate usage (cached tokens included) to a trailing chunk when
	// include_usage is requested; without it the stream ends usage-less and the
	// request is logged local_estimate/upstream_usage_missing with cache_hit 0.
	// new-api and sub2api force the same switch for the same reason. ponytail: an
	// upstream that 400s on stream_options simply loses this attempt and is skipped
	// by channel failover; if one ever bites, add a per-channel opt-out rather than
	// a host allowlist.
	if request.Stream != nil && *request.Stream {
		if request.StreamOptions == nil {
			request.StreamOptions = &model.StreamOptions{IncludeUsage: true}
		} else if !request.StreamOptions.IncludeUsage {
			request.StreamOptions.IncludeUsage = true
		}
	}

	applyGLMThinking(request)
	applyGLMToolStreaming(request, baseUrl)
	applyDeepSeekResponseFormat(request)
	applyThirdPartyChatParamCompat(request, baseUrl)
	applyGLMChatTokenFieldCompat(request)

	body, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)

	var targetURL string
	if customEndpoint {
		targetURL, err = xurl.JoinCustomOpenAIChatPath(baseUrl, "/v1/chat/completions")
	} else {
		targetURL, err = xurl.JoinOpenAIPath(baseUrl, "/v1/chat/completions")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to join openai chat url: %w", err)
	}
	req.URL, err = req.URL.Parse(targetURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse openai chat url: %w", err)
	}
	mergeInboundQuery(req.URL, request.Query)
	req.Method = http.MethodPost
	return req, nil
}

// glmThinkingEnabled and glmThinkingDisabled are the canonical thinking
// payloads GLM (glm-4.5/glm-4.6 ...) and z.ai (zai) models expect on the
// OpenAI Chat Completions endpoint. GLM does not understand the OpenAI
// reasoning_effort field, so the client's reasoning intent must be projected
// onto this thinking object instead.
var (
	glmThinkingEnabled  = json.RawMessage(`{"type":"enabled"}`)
	glmThinkingDisabled = json.RawMessage(`{"type":"disabled"}`)
)

// isDeepSeekModel reports whether the model name targets a DeepSeek model.
// DeepSeek's chat API only accepts response_format type "json_object";
// sending "json_schema" causes a 400 error.
func isDeepSeekModel(model string) bool {
	return strings.Contains(strings.ToLower(model), "deepseek")
}

// isOpenAIReasoningChatModel reports whether the model name targets an OpenAI
// o-series or gpt-5 family chat model that rejects max_tokens / temperature
// (and, for most of them, expects the first system message as role=developer).
// Matching is prefix-based on the bare model id so date suffixes and provider
// prefixes still classify correctly (e.g. "o3-2025-04-16", "openai/gpt-5-mini").
func isOpenAIReasoningChatModel(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower == "" {
		return false
	}
	// Strip a common "org/" provider prefix so "openai/o3-mini" still matches.
	if i := strings.LastIndex(lower, "/"); i >= 0 && i+1 < len(lower) {
		lower = lower[i+1:]
	}
	// The gpt-5-chat family ("gpt-5-chat", "gpt-5-chat-latest", "gpt-5.1-chat-latest")
	// are non-reasoning chat models that DO accept temperature / max_tokens, so they
	// must not be classified as reasoning models. Exclude any "-chat" model.
	if strings.Contains(lower, "-chat") {
		return false
	}
	switch {
	case strings.HasPrefix(lower, "o1"),
		strings.HasPrefix(lower, "o3"),
		strings.HasPrefix(lower, "o4"),
		strings.HasPrefix(lower, "gpt-5"):
		return true
	default:
		return false
	}
}

// openAIReasoningUsesDeveloperRole reports whether the reasoning chat model
// expects the first system message promoted to role=developer. Early o1-mini
// and o1-preview still use system; later o-series and gpt-5 use developer.
func openAIReasoningUsesDeveloperRole(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	if i := strings.LastIndex(lower, "/"); i >= 0 && i+1 < len(lower) {
		lower = lower[i+1:]
	}
	// o1-mini / o1-preview (and dated variants) keep the system role.
	if strings.HasPrefix(lower, "o1-mini") || strings.HasPrefix(lower, "o1-preview") {
		return false
	}
	return isOpenAIReasoningChatModel(name)
}

// applyOpenAIReasoningChatCompat rewrites a chat/completions body for o-series
// and gpt-5 models: max_tokens -> max_completion_tokens, drop temperature, and
// (when supported) promote the first system message to developer. Body-only;
// does not touch request headers or fingerprint.
func applyOpenAIReasoningChatCompat(request *model.InternalLLMRequest) {
	if request == nil {
		return
	}
	// max_tokens is rejected by o/gpt-5 chat models; migrate it onto
	// max_completion_tokens when the latter is unset so the client's limit is
	// preserved. If both are set, prefer the modern field and drop max_tokens.
	if request.MaxTokens != nil {
		if request.MaxCompletionTokens == nil {
			request.MaxCompletionTokens = request.MaxTokens
		}
		request.MaxTokens = nil
	}
	// Temperature is unsupported on these models and must not be forwarded.
	request.Temperature = nil

	if !openAIReasoningUsesDeveloperRole(request.Model) {
		return
	}
	// Promote only the first system message to developer (the canonical
	// instruction slot). Leave subsequent system messages untouched so mixed
	// multi-system histories stay stable.
	for i := range request.Messages {
		if request.Messages[i].Role == "system" {
			request.Messages[i].Role = "developer"
			break
		}
	}
}

// applyDeepSeekResponseFormat downgrades a json_schema response_format to
// json_object for DeepSeek models, which do not support the json_schema type.
// All other models and all other response_format types are left untouched.
func applyDeepSeekResponseFormat(request *model.InternalLLMRequest) {
	if request == nil || !isDeepSeekModel(request.Model) {
		return
	}
	if request.ResponseFormat != nil && request.ResponseFormat.Type == "json_schema" {
		request.ResponseFormat = &model.ResponseFormat{Type: "json_object"}
	}
}

// isOpenAIOfficialChatBase reports whether the chat upstream is the genuine
// OpenAI API. Unlike third-party OpenAI-compatible upstreams, api.openai.com
// does accept the newer Chat / Responses residue params (prompt_cache_key,
// safety_identifier, ...), so they are kept only for the official endpoint.
func isOpenAIOfficialChatBase(baseUrl string) bool {
	// Match on the parsed host, not a raw substring: a third-party base like
	// https://api.openai.com.proxy.example/v1 or https://gw/api.openai.com/v1 must NOT
	// be treated as official (which would skip residue stripping and 400 the upstream).
	if parsed, err := url.Parse(baseUrl); err == nil {
		if host := strings.ToLower(parsed.Hostname()); host != "" {
			return host == "api.openai.com" || strings.HasSuffix(host, ".api.openai.com")
		}
	}
	return strings.Contains(strings.ToLower(baseUrl), "api.openai.com")
}

// applyThirdPartyChatParamCompat makes a chat/completions request safe for a
// third-party OpenAI-compatible upstream. A Responses-shaped client (e.g.
// Cursor) carries OpenAI-only params (prompt_cache_key / prompt_cache_retention
// / safety_identifier / store) and sometimes a malformed tool_choice; every
// third-party upstream we forward to over chat/completions (DeepSeek, GLM, Qwen,
// MiniMax, Kimi, Grok, ...) 400s on them ("Unsupported parameter(s): ..." or
// "did not match any variant of untagged enum ChatCompletionToolChoiceOption").
// We strip the residue params for every non-official chat upstream (OpenAI's own
// models are served over the Responses endpoint, not this path) and always
// normalise a malformed tool_choice. Keyed on the upstream, not a per-model
// allowlist, so a newly-added provider is covered automatically.
var deepseekRSeriesRegex = regexp.MustCompile(`(?i)deepseek-r\d+`)

func isDeepSeekReasonerModel(model string) bool {
	lower := strings.ToLower(strings.TrimSpace(model))
	if lower == "" {
		return false
	}
	if strings.Contains(lower, "reasoner") {
		return true
	}
	if deepseekRSeriesRegex.MatchString(lower) {
		return true
	}
	if strings.Contains(lower, "thinking") && strings.Contains(lower, "deepseek") {
		return true
	}
	return strings.Contains(lower, "deepseek-reasoner") ||
		strings.Contains(lower, "deepseek-r1") ||
		strings.Contains(lower, "deepseek-v4-reasoner") ||
		(strings.Contains(lower, "deepseek-v4") && strings.Contains(lower, "thinking"))
}

func applyThirdPartyChatParamCompat(request *model.InternalLLMRequest, baseUrl string) {
	if request == nil {
		return
	}
	// A malformed tool_choice is useless to any upstream — always normalise it.
	sanitizeToolChoiceForStrictUpstream(request)
	// The residue params are OpenAI-only; keep them for genuine api.openai.com,
	// strip them for every third-party OpenAI-compatible chat upstream.
	if isOpenAIOfficialChatBase(baseUrl) {
		return
	}
	request.Store = nil
	request.PromptCacheKey = nil
	request.PromptCacheRetention = nil
	request.SafetyIdentifier = nil
	// service_tier is OpenAI Responses residue; vercel AI Gateway / z.ai chat 400
	// on unknown top-level fields (live log id 1787047851119 carried service_tier).
	request.ServiceTier = nil
	// 思考强度：按上游模型族分别投影，而不是一刀切清空。
	//
	// 这里原先是 `request.ReasoningEffort = ""` 一行到底，理由是"严格第三方
	// 上游（DeepSeek serde）遇到不认识的参数会 400"。防 400 是真的，但代价
	// 是所有国产模型**永远收不到强度**——DeepSeek / Qwen / Kimi / MiniMax /
	// 百川 / 混元 / 阶跃全走这条 chat 路径，客户端要 max，上游看到的是一个
	// 没有任何强度语义的裸请求。而且 ReasoningBudget / AdaptiveThinking 都是
	// json:"-"，序列化时根本不出去，连兜底通道都没有。
	//
	// 正确做法是参考项目的共识（new-api / CLIProxyAPI 都是这个路数）：
	// 已知厂商走显式转换；已知厂商但模型不支持就静默剥离；完全未知的不猜。
	// 落到这里就是按模型族分三档，见 applyReasoningProjection。
	applyReasoningProjection(request)

	// Reasoning models (o1, o3, o4, gpt-5, deepseek-reasoner/r1) reject sampling parameters.
	if isOpenAIReasoningChatModel(request.Model) || isDeepSeekReasonerModel(request.Model) {
		request.Temperature = nil
		request.TopP = nil
		request.PresencePenalty = nil
		request.FrequencyPenalty = nil
	}

	// Clean up consecutive same-role messages for strict OpenAI-compatible upstreams (DeepSeek/Qwen).
	if len(request.Messages) > 1 {
		request.Messages = mergeConsecutiveSameRoleChatMessages(request.Messages)
	}
}

func mergeConsecutiveSameRoleChatMessages(messages []model.Message) []model.Message {
	if len(messages) <= 1 {
		return messages
	}
	merged := make([]model.Message, 0, len(messages))
	for _, m := range messages {
		if len(merged) == 0 {
			merged = append(merged, m)
			continue
		}
		prev := &merged[len(merged)-1]
		if strings.EqualFold(strings.TrimSpace(prev.Role), "assistant") &&
			strings.EqualFold(strings.TrimSpace(m.Role), "assistant") {
			// Merge consecutive assistant messages (e.g. one has reasoning, one has text/tool_calls)
			if m.ReasoningContent != nil && *m.ReasoningContent != "" {
				if prev.ReasoningContent == nil || *prev.ReasoningContent == "" {
					prev.ReasoningContent = m.ReasoningContent
				} else {
					combined := *prev.ReasoningContent + "\n" + *m.ReasoningContent
					prev.ReasoningContent = &combined
				}
			}
			if m.Content.Content != nil && *m.Content.Content != "" {
				if prev.Content.Content == nil || *prev.Content.Content == "" {
					prev.Content.Content = m.Content.Content
				} else {
					combined := *prev.Content.Content + "\n" + *m.Content.Content
					prev.Content.Content = &combined
				}
			}
			if len(m.ToolCalls) > 0 {
				prev.ToolCalls = append(prev.ToolCalls, m.ToolCalls...)
			}
			continue
		}
		if strings.EqualFold(strings.TrimSpace(prev.Role), "user") &&
			strings.EqualFold(strings.TrimSpace(m.Role), "user") {
			// Merge consecutive user messages
			if m.Content.Content != nil && *m.Content.Content != "" {
				if prev.Content.Content == nil || *prev.Content.Content == "" {
					prev.Content.Content = m.Content.Content
				} else {
					combined := *prev.Content.Content + "\n" + *m.Content.Content
					prev.Content.Content = &combined
				}
			}
			if len(m.Content.MultipleContent) > 0 {
				prev.Content.MultipleContent = append(prev.Content.MultipleContent, m.Content.MultipleContent...)
			}
			continue
		}
		merged = append(merged, m)
	}
	return merged
}

// sanitizeToolChoiceForStrictUpstream drops a tool_choice value that would
// marshal to something a strict OpenAI-compatible upstream (DeepSeek) rejects:
// an empty ToolChoice (marshals to null), a named choice whose type is not
// "function", or a named function whose name is empty. A dropped tool_choice
// lets the upstream fall back to its default (auto), which is the safe behaviour.
func sanitizeToolChoiceForStrictUpstream(request *model.InternalLLMRequest) {
	tc := request.ToolChoice
	if tc == nil {
		return
	}
	// Valid form 1: a string mode ("none" / "auto" / "required"). Only these three
	// are legal variants for a strict upstream. An empty or non-standard string
	// (notably "" synthesized from a client's `tool_choice: null`) must be dropped,
	// not forwarded verbatim — deepseek's serde rejects `"tool_choice": ""` with 400.
	if tc.ToolChoice != nil {
		switch normalized := strings.ToLower(*tc.ToolChoice); normalized {
		case "none", "auto", "required":
			// Normalise to the exact lowercase enum value — a strict upstream (deepseek)
			// can 400 on "AUTO"/"Required" even though the mode is legal.
			*tc.ToolChoice = normalized
			return
		}
		request.ToolChoice = nil
		return
	}
	// Valid form 2: a named function choice with a non-empty function name.
	if tc.NamedToolChoice != nil &&
		tc.NamedToolChoice.Type == "function" &&
		tc.NamedToolChoice.Function.Name != "" {
		return
	}
	// Anything else (empty object -> null, non-function type, empty name) is
	// invalid for the strict upstream; drop it so the request still goes through.
	request.ToolChoice = nil
}

// applyReasoningProjection 把内部的思考意图投影成该模型族能接受的出站字段。
//
// 三家参考项目的共识（均已在源码核实）：
//   - 已知厂商走显式转换（new-api 把 DeepSeek 当一等公民处理）
//   - 已知厂商但模型不支持 → 静默剥离，不发出去让上游 400（CLIProxyAPI）
//   - 完全未知 → 不猜、不改形（sub2api ThinkingProtocolUnknown）
//
// 没有一家选择"发出去让上游 400"。所以默认分支必须是**剥离**而不是透传。
//
// 各家接受的字段（均在参考项目源码里核实过）：
//
//	DeepSeek  chat 路径同时收 reasoning_effort 与 thinking:{"type":...}
//	          （new-api relay/channel/deepseek/adaptor.go:132-136 两个都发），
//	          所以我们也两个都发：thinking 管开关、reasoning_effort 管档位。
//	Qwen      收顶层 enable_thinking(布尔) + thinking_budget(整数)，
//	          注意是顶层，不在 chat_template_kwargs 里。
//	GLM       thinking:{"type":...} 由 applyGLMThinking 单独处理，这里只清
//	          掉 reasoning_effort 双字段（zhipu 两个都接受，但 GLM 的 thinking
//	          是二值的，留着 reasoning_effort 只会让严格上游 400）。
//	Kimi      收 thinking:{"type":"enabled","effort":<level>}。
//	其余      （MiniMax / 百川 / 混元 / 阶跃 / 自建中转）参考项目里没有任何
//	          实现 —— 属于"没处理"，默认剥离最安全。
func applyReasoningProjection(request *model.InternalLLMRequest) {
	if request == nil {
		return
	}
	effort := model.NormalizeEffortKey(request.ReasoningEffort)
	budget := int64(0)
	if request.ReasoningBudget != nil && *request.ReasoningBudget > 0 {
		budget = *request.ReasoningBudget
	}
	wantOff := effort == "none" || effort == "minimal"

	if isGLMModel(request.Model) {
		request.ReasoningEffort = ""
		request.ThinkingBudget = nil
		return
	}

	switch {
	case isDeepSeekModel(request.Model):
		// 老一代"自带思考"的模型（deepseek-reasoner / r1 / r2 / v4-thinking）
		// 不接受强度参数，也无所谓开关——它们永远思考。清空是正确行为，
		// 不是缺陷。参考项目同样只对 deepseek-v4-* / deepseek-flash 做转换
		// （new-api relaykit/relayconvert/reasoning/suffix.go:283-285 明确
		// 把 base 限定在这两族），别的一律不碰。
		if isDeepSeekReasonerModel(request.Model) {
			request.ReasoningEffort = ""
			request.ThinkingBudget = nil
			return
		}
		// thinking_budget 只属于 Qwen；发给 DeepSeek 是陌生参数，必 400
		// （new-api relaykit/dto/openai_request.go:139-141 按模型族剥离）。
		request.ThinkingBudget = nil
		// 只有确认支持思考开关的型号才投影，其余（如普通 deepseek-chat）
		// 按未知上游处理 —— 见 applyReasoningProjection 顶部注释。
		if !isDeepSeekThinkingCapable(request.Model) {
			request.ReasoningEffort = ""
			request.ThinkingBudget = nil
			return
		}
		// 两个都发：thinking 管开关，reasoning_effort 管档位。
		if wantOff {
			request.Thinking = json.RawMessage(`{"type":"disabled"}`)
			request.ReasoningEffort = "none"
			return
		}
		if effort != "" || budget > 0 {
			request.Thinking = json.RawMessage(`{"type":"enabled"}`)
			// 🔴 出站发**归一后的原始档位**，不是换算后的档位。
			// new-api 的 deepSeekRunEffort 只写进日志/计费口径
			// （adaptor.go:142 info.SetReasoningEffort），从不落到 wire 上。
			// 把换算值发上线会和计费口径打架。
			request.ReasoningEffort = effort
			return
		}
		request.ReasoningEffort = ""

	case isQwenModel(request.Model):
		// Qwen 用布尔开关 + 独立预算，不认 reasoning_effort。
		enabled := !wantOff && (effort != "" || budget > 0)
		request.EnableThinking = &enabled
		if enabled {
			b := budget
			if b == 0 {
				b = model.EffortBudgetFor(effort)
			}
			request.ThinkingBudget = &b
		}
		request.ReasoningEffort = ""

	case isKimiModel(request.Model):
		// thinking_budget 只属于 Qwen，Kimi 收到是陌生参数。
		request.ThinkingBudget = nil
		// Kimi 的 thinking 自带 effort 成员，不用顶层 reasoning_effort。
		if wantOff {
			request.Thinking = json.RawMessage(`{"type":"disabled"}`)
		} else if effort != "" {
			if payload, err := json.Marshal(map[string]string{
				"type":   "enabled",
				"effort": effort,
			}); err == nil {
				request.Thinking = payload
			}
		}
		request.ReasoningEffort = ""

	default:
		// 未知上游：不猜。剥离所有思考字段，避免陌生参数触发 400——
		// 这正是当初那行无条件清空想解决的问题，现在只对真正未知的生效。
		request.ReasoningEffort = ""
		request.EnableThinking = nil
		request.ThinkingBudget = nil
	}
}

// isDeepSeekThinkingCapable reports whether the model exposes DeepSeek's
// thinking switch + reasoning_effort.
//
// 只认 deepseek-v4-* 与 deepseek-flash：这是参考项目实际做转换的两族
// （new-api relaykit/relayconvert/reasoning/suffix.go:283-285 把 base 明确
// 限定在此）。普通 deepseek-chat 是非推理模型，给它发 thinking 有 400 风险；
// deepseek-reasoner / r1 这类自带思考，由 isDeepSeekReasonerModel 单独处理。
func isDeepSeekThinkingCapable(model string) bool {
	lower := strings.ToLower(strings.TrimSpace(model))
	if idx := strings.LastIndex(lower, "/"); idx >= 0 {
		lower = lower[idx+1:]
	}
	return strings.HasPrefix(lower, "deepseek-v4") || strings.HasPrefix(lower, "deepseek-flash")
}

// isQwenModel reports whether the model name targets an Alibaba Qwen /
// DashScope model. Matched on the bare model id so provider prefixes and date
// suffixes still classify correctly.
func isQwenModel(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	if idx := strings.LastIndex(lower, "/"); idx >= 0 {
		lower = lower[idx+1:]
	}
	return strings.HasPrefix(lower, "qwen") || strings.HasPrefix(lower, "qwq")
}

// isKimiModel reports whether the model name targets a Moonshot / Kimi model.
func isKimiModel(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	if idx := strings.LastIndex(lower, "/"); idx >= 0 {
		lower = lower[idx+1:]
	}
	return strings.HasPrefix(lower, "kimi") || strings.HasPrefix(lower, "moonshot")
}

// isGLMModel reports whether the model name targets a GLM / z.ai model that
// uses the thinking:{type} switch instead of reasoning_effort.
func isGLMModel(model string) bool {
	lower := strings.ToLower(model)
	return strings.Contains(lower, "glm") || strings.Contains(lower, "zai")
}

// applyGLMThinking maps the internal reasoning intent
// (ReasoningEffort / ReasoningBudget / AdaptiveThinking) onto the GLM-specific
// thinking:{type:"enabled"|"disabled"} field.
//
// It is strictly gated on the model name so non-GLM chat providers
// (openai/deepseek/...) keep their existing behaviour and are never injected
// with a thinking field (deepseek-reasoner reasons automatically and has no
// such parameter). When the client already supplied a thinking payload
// (request.Thinking is non-empty, e.g. DeepSeek-style direct passthrough) the
// original value is respected and never overwritten.
func applyGLMThinking(request *model.InternalLLMRequest) {
	if request == nil || !isGLMModel(request.Model) {
		return
	}

	// Respect a client-provided thinking payload (direct passthrough).
	if !rawJSONPresent(request.Thinking) {
		switch {
		case glmWantsThinking(request):
			request.Thinking = append(json.RawMessage(nil), glmThinkingEnabled...)
		case glmDisablesThinking(request):
			request.Thinking = append(json.RawMessage(nil), glmThinkingDisabled...)
		default:
			// No explicit reasoning intent: leave the request untouched instead of
			// force-injecting a thinking field on ordinary GLM requests.
		}
	}

	// GLM consumes thinking:{type}, not OpenAI reasoning_effort. Always drop
	// ReasoningEffort for GLM models so a strict OpenAI-compatible upstream
	// (vercel AI Gateway / z.ai chat) does not 400 on an unknown dual field.
	// Cursor Anthropic (/v1/messages) and Responses clients both land here with
	// ReasoningEffort set from thinking/reasoning blocks.
	request.ReasoningEffort = ""
}

// applyGLMToolStreaming supplies the native GLM tool-streaming default only to
// known first-party hosts: a model name alone does not prove gateway support.
// Explicit client values are preserved for supported models.
func applyGLMToolStreaming(request *model.InternalLLMRequest, baseUrl string) {
	if request == nil {
		return
	}
	if !supportsGLMToolStreaming(request.Model) {
		// tool_stream is a GLM extension. Do not leak a client-supplied value to
		// unrelated OpenAI-compatible providers that may reject unknown fields.
		request.ToolStream = nil
		return
	}
	if request.ToolStream != nil {
		return
	}
	if request.Stream == nil || !*request.Stream || len(request.Tools) == 0 {
		return
	}
	if !isGLMToolStreamingHost(baseUrl) {
		return
	}

	toolStreamingEnabled := true
	request.ToolStream = &toolStreamingEnabled
}

// Match the parsed hostname, never a substring in the path or userinfo.
func isGLMToolStreamingHost(baseUrl string) bool {
	parsed, err := url.Parse(baseUrl)
	if err != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "api.z.ai" || host == "open.bigmodel.cn"
}

func supportsGLMToolStreaming(modelName string) bool {
	normalizedName := strings.ToLower(strings.TrimSpace(modelName))
	if separatorIndex := strings.LastIndex(normalizedName, "/"); separatorIndex >= 0 {
		normalizedName = normalizedName[separatorIndex+1:]
	}
	return modelMatchesVersionFamily(normalizedName, "glm-4.6", "-") ||
		modelMatchesVersionFamily(normalizedName, "glm-4.7", "-") ||
		modelMatchesVersionFamily(normalizedName, "glm-5", ".-")
}

func modelMatchesVersionFamily(modelName, familyName, allowedSuffixDelimiters string) bool {
	if modelName == familyName {
		return true
	}
	if !strings.HasPrefix(modelName, familyName) || len(modelName) == len(familyName) {
		return false
	}
	return strings.ContainsRune(allowedSuffixDelimiters, rune(modelName[len(familyName)]))
}

// applyGLMChatTokenFieldCompat remaps OpenAI-only max_completion_tokens onto
// max_tokens for GLM chat upstreams. Cursor Responses clients populate
// MaxCompletionTokens; vercel/z.ai chat rejects the modern field name.
func applyGLMChatTokenFieldCompat(request *model.InternalLLMRequest) {
	if request == nil || !isGLMModel(request.Model) {
		return
	}
	if request.MaxCompletionTokens != nil {
		if request.MaxTokens == nil {
			request.MaxTokens = request.MaxCompletionTokens
		}
		request.MaxCompletionTokens = nil
	}
}

// glmWantsThinking reports an explicit request to enable reasoning.
func glmWantsThinking(request *model.InternalLLMRequest) bool {
	if request.AdaptiveThinking {
		return true
	}
	if request.ReasoningBudget != nil && *request.ReasoningBudget > 0 {
		return true
	}
	// 用共享表的"已知档位"判断，而不是手写 case 列表。
	//
	// 手写 case 配 strings.ToLower 会漏掉空白：客户端发 " max " 时
	// Lower 仍是 " max "，比不中任何 case → 静默变成"不开思考"。
	// 实测确认 "max"/"MAX"/"Max" 都开思考，唯独 " max " / "MAX " 不开。
	// 同一个值走 anthropic 通道（EffortBudgetFor 会 TrimSpace）却是正常的，
	// 于是同一条客户端请求换个通道强度就变了。
	return model.IsKnownEffort(request.ReasoningEffort)
}

// glmDisablesThinking reports an explicit request to turn reasoning off.
// Mirrors the volcengine semantics where "minimal" maps to disabled, and also
// covers the OpenAI "none" effort.
func glmDisablesThinking(request *model.InternalLLMRequest) bool {
	switch strings.TrimSpace(strings.ToLower(request.ReasoningEffort)) {
	case "minimal", "none":
		return true
	}
	return false
}

// rawJSONPresent reports whether a json.RawMessage carries meaningful content
// (non-empty and not the literal null).
func rawJSONPresent(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return trimmed != "" && trimmed != "null"
}

func (o *CustomChatOutbound) TransformResponse(ctx context.Context, response *http.Response) (*model.InternalLLMResponse, error) {
	return (&ChatOutbound{}).TransformResponse(ctx, response)
}

func (o *CustomChatOutbound) TransformStream(ctx context.Context, eventData []byte) (*model.InternalLLMResponse, error) {
	return (&ChatOutbound{}).TransformStream(ctx, eventData)
}

func (o *ChatOutbound) TransformResponse(ctx context.Context, response *http.Response) (*model.InternalLLMResponse, error) {
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	if len(body) == 0 {
		return nil, fmt.Errorf("response body is empty")
	}

	var resp model.InternalLLMResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}
	patchOpenAICompatibleCacheTokens(&resp, body)
	return &resp, nil
}

func (o *ChatOutbound) TransformStream(ctx context.Context, eventData []byte) (*model.InternalLLMResponse, error) {
	eventData = bytes.TrimSpace(eventData)
	if len(eventData) == 0 {
		return nil, nil
	}

	if bytes.HasPrefix(eventData, []byte("[DONE]")) {
		return &model.InternalLLMResponse{
			Object: "[DONE]",
		}, nil
	}

	var errCheck struct {
		Error *model.ErrorDetail `json:"error"`
	}
	if err := json.Unmarshal(eventData, &errCheck); err == nil && errCheck.Error != nil {
		return nil, &model.ResponseError{
			Detail: *errCheck.Error,
		}
	}

	var resp model.InternalLLMResponse
	if err := json.Unmarshal(eventData, &resp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal stream chunk: %w", err)
	}
	patchOpenAICompatibleCacheTokens(&resp, eventData)
	return &resp, nil
}

func patchOpenAICompatibleCacheTokens(resp *model.InternalLLMResponse, body []byte) {
	if resp == nil || len(body) == 0 {
		return
	}
	if resp.Usage != nil && resp.Usage.PromptTokensDetails != nil && resp.Usage.PromptTokensDetails.CachedTokens > 0 {
		return
	}

	cachedTokens, ok := extractOpenAICompatibleCachedTokens(body)
	if !ok || cachedTokens <= 0 {
		return
	}

	if resp.Usage == nil {
		resp.Usage = &model.Usage{}
	}
	if resp.Usage.PromptTokensDetails == nil {
		resp.Usage.PromptTokensDetails = &model.PromptTokensDetails{}
	}
	if resp.Usage.PromptTokensDetails.CachedTokens == 0 {
		resp.Usage.PromptTokensDetails.CachedTokens = cachedTokens
	}
	if resp.Usage.PromptTokens < cachedTokens {
		resp.Usage.PromptTokens = cachedTokens
	}
	if total := resp.Usage.PromptTokens + resp.Usage.CompletionTokens; resp.Usage.TotalTokens < total {
		resp.Usage.TotalTokens = total
	}
}

func extractOpenAICompatibleCachedTokens(body []byte) (int64, bool) {
	type tokenDetails struct {
		CachedTokens *int64 `json:"cached_tokens"`
	}
	type usageAliases struct {
		PromptTokensDetails *tokenDetails `json:"prompt_tokens_details"`
		InputTokensDetails  *tokenDetails `json:"input_tokens_details"`
		CachedTokens        *int64        `json:"cached_tokens"`
		PromptCacheHit      *int64        `json:"prompt_cache_hit_tokens"`
	}
	var payload struct {
		Usage   usageAliases `json:"usage"`
		Choices []struct {
			Usage usageAliases `json:"usage"`
		} `json:"choices"`
		Timings struct {
			CacheN *int64 `json:"cache_n"`
		} `json:"timings"`
	}
	firstPositive := func(usage usageAliases) (int64, bool) {
		switch {
		case usage.PromptTokensDetails != nil && usage.PromptTokensDetails.CachedTokens != nil && *usage.PromptTokensDetails.CachedTokens > 0:
			return *usage.PromptTokensDetails.CachedTokens, true
		case usage.InputTokensDetails != nil && usage.InputTokensDetails.CachedTokens != nil && *usage.InputTokensDetails.CachedTokens > 0:
			return *usage.InputTokensDetails.CachedTokens, true
		case usage.CachedTokens != nil && *usage.CachedTokens > 0:
			return *usage.CachedTokens, true
		case usage.PromptCacheHit != nil && *usage.PromptCacheHit > 0:
			return *usage.PromptCacheHit, true
		default:
			return 0, false
		}
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return 0, false
	}
	if v, ok := firstPositive(payload.Usage); ok {
		return v, true
	}
	for _, choice := range payload.Choices {
		if v, ok := firstPositive(choice.Usage); ok {
			return v, true
		}
	}
	if payload.Timings.CacheN != nil && *payload.Timings.CacheN > 0 {
		return *payload.Timings.CacheN, true
	}
	return 0, false
}
