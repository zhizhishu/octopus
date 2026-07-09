package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/utils/xurl"
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
	}

	// Convert developer role to system role for compatibility
	for i := range request.Messages {
		if request.Messages[i].Role == "developer" {
			request.Messages[i].Role = "system"
		}
	}

	if request.Stream != nil && *request.Stream {
		if request.StreamOptions == nil {
			request.StreamOptions = &model.StreamOptions{IncludeUsage: true}
		} else if !request.StreamOptions.IncludeUsage {
			request.StreamOptions.IncludeUsage = true
		}
	}

	applyGLMThinking(request)
	applyDeepSeekResponseFormat(request)
	applyThirdPartyChatParamCompat(request, baseUrl)

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
	if rawJSONPresent(request.Thinking) {
		return
	}

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

// glmWantsThinking reports an explicit request to enable reasoning.
func glmWantsThinking(request *model.InternalLLMRequest) bool {
	if request.AdaptiveThinking {
		return true
	}
	if request.ReasoningBudget != nil && *request.ReasoningBudget > 0 {
		return true
	}
	switch strings.ToLower(request.ReasoningEffort) {
	case "low", "medium", "high":
		return true
	}
	return false
}

// glmDisablesThinking reports an explicit request to turn reasoning off.
// Mirrors the volcengine semantics where "minimal" maps to disabled, and also
// covers the OpenAI "none" effort.
func glmDisablesThinking(request *model.InternalLLMRequest) bool {
	switch strings.ToLower(request.ReasoningEffort) {
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
