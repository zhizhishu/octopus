package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	request.ClearHelpFields()

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
