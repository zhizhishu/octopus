package authropic

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

	anthropicModel "github.com/bestruirui/octopus/internal/transformer/inbound/anthropic"
	"github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/utils/log"
	"github.com/bestruirui/octopus/internal/utils/mime"
	"github.com/bestruirui/octopus/internal/utils/xurl"
)

type MessageOutbound struct {
	// Stream state tracking
	streamID    string
	streamModel string
	streamUsage *model.Usage
	toolIndex   int
	toolCalls   map[int]*model.ToolCall
	initialized bool
}

func (o *MessageOutbound) TransformRequest(ctx context.Context, request *model.InternalLLMRequest, baseUrl, key string) (*http.Request, error) {
	if request == nil {
		return nil, fmt.Errorf("request is nil")
	}

	// Convert to Anthropic request format
	anthropicReq := convertToAnthropicRequest(request)

	body, err := json.Marshal(anthropicReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal anthropic request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers
	req.Header.Set("Content-Type", "application/json")
	// Claude Code keeps Accept as application/json and uses the JSON `stream`
	// flag to request SSE. Some routers key their Claude device/profile
	// emulation on that client shape, so do not switch Accept for stream=true.
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	applyAnthropicAuthHeaders(req, baseUrl, key)
	if request.Stream != nil && *request.Stream {
		// Advertise the same encoding preference as the genuine claude-cli 2.1.198
		// (gzip, deflate, br, zstd) instead of "identity" — a self-declared claude-cli
		// that refuses compression is a detectable tell. Because this header is set
		// manually, Go's transport will NOT auto-decompress, so the relay unwraps any
		// upstream Content-Encoding before the SSE reader (unwrapResponseEncoding).
		// Anthropic does not compress text/event-stream bodies in practice, so the
		// unwrap is a no-op on the common path.
		req.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	}
	for _, beta := range request.TransformOptions.AnthropicBetas {
		addAnthropicBetaHeader(req.Header, beta)
	}
	if model.AnthropicRequestWantsOneMillionBeta(request) {
		addAnthropicBetaHeader(req.Header, model.AnthropicOneMillionBeta)
	}

	// Parse and set URL
	targetURL, err := xurl.JoinAnthropicPath(baseUrl, "/v1/messages")
	if err != nil {
		return nil, fmt.Errorf("failed to join anthropic messages url: %w", err)
	}
	parsedUrl, err := url.Parse(targetURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse base url: %w", err)
	}

	// Merge inbound query params without dropping provider-specific base URL
	// params such as CPA's beta switches.
	if request.Query != nil {
		q := parsedUrl.Query()
		for key, values := range request.Query {
			q.Del(key)
			for _, value := range values {
				q.Add(key, value)
			}
		}
		parsedUrl.RawQuery = q.Encode()
	}
	req.URL = parsedUrl

	return req, nil
}

func addAnthropicBetaHeader(headers http.Header, beta string) {
	beta = strings.TrimSpace(beta)
	if beta == "" {
		return
	}
	existing := strings.Split(headers.Get("Anthropic-Beta"), ",")
	seen := map[string]bool{}
	values := make([]string, 0, len(existing)+1)
	for _, item := range existing {
		item = strings.TrimSpace(item)
		if item == "" || seen[strings.ToLower(item)] {
			continue
		}
		seen[strings.ToLower(item)] = true
		values = append(values, item)
	}
	if !seen[strings.ToLower(beta)] {
		values = append(values, beta)
	}
	headers.Set("Anthropic-Beta", strings.Join(values, ","))
}

func applyAnthropicAuthHeaders(req *http.Request, baseURL, key string) {
	key = strings.TrimSpace(key)
	if req == nil || key == "" {
		return
	}
	req.Header.Set("X-API-Key", key)
	if shouldSendAnthropicBearerAuth(baseURL) {
		req.Header.Set("Authorization", "Bearer "+key)
	}
}

func shouldSendAnthropicBearerAuth(baseURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return false
	}
	if host == "api.anthropic.com" || strings.HasSuffix(host, ".anthropic.com") {
		return false
	}
	return true
}

func (o *MessageOutbound) TransformResponse(ctx context.Context, response *http.Response) (*model.InternalLLMResponse, error) {
	if response == nil {
		return nil, fmt.Errorf("response is nil")
	}

	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	if len(body) == 0 {
		return nil, fmt.Errorf("response body is empty")
	}

	// Check for error response
	if response.StatusCode >= 400 {
		var errResp anthropicModel.AnthropicError
		if err := json.Unmarshal(body, &errResp); err == nil && errResp.Error.Message != "" {
			return nil, &model.ResponseError{
				StatusCode: response.StatusCode,
				Detail: model.ErrorDetail{
					Message: errResp.Error.Message,
					Type:    errResp.Error.Type,
				},
			}
		}
		return nil, fmt.Errorf("HTTP error %d: %s", response.StatusCode, string(body))
	}

	var anthropicResp anthropicModel.Message
	if err := json.Unmarshal(body, &anthropicResp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal anthropic response: %w", err)
	}

	// Convert to internal response
	return convertToLLMResponse(&anthropicResp), nil
}

func (o *MessageOutbound) TransformStream(ctx context.Context, eventData []byte) (*model.InternalLLMResponse, error) {
	eventData = bytes.TrimSpace(eventData)
	if len(eventData) == 0 {
		return nil, nil
	}

	// Handle [DONE] marker
	if bytes.HasPrefix(eventData, []byte("[DONE]")) {
		return &model.InternalLLMResponse{
			Object: "[DONE]",
		}, nil
	}

	// Initialize state if needed
	if !o.initialized {
		o.toolCalls = make(map[int]*model.ToolCall)
		o.toolIndex = -1
		o.initialized = true
	}

	// Parse the streaming event
	var streamEvent anthropicModel.StreamEvent
	if err := json.Unmarshal(eventData, &streamEvent); err != nil {
		return nil, fmt.Errorf("failed to unmarshal stream event: %w", err)
	}

	resp := &model.InternalLLMResponse{
		ID:      o.streamID,
		Model:   o.streamModel,
		Object:  "chat.completion.chunk",
		Created: 0,
	}

	switch streamEvent.Type {
	case "error":
		if streamEvent.Error != nil {
			return nil, fmt.Errorf("anthropic stream error: %s: %s", streamEvent.Error.Type, streamEvent.Error.Message)
		}
		return nil, fmt.Errorf("anthropic stream error")

	case "message_start":
		if streamEvent.Message != nil {
			o.streamID = streamEvent.Message.ID
			o.streamModel = streamEvent.Message.Model
			resp.ID = o.streamID
			resp.Model = o.streamModel

			if streamEvent.Message.Usage != nil &&
				(streamEvent.Message.Usage.InputTokens > 0 ||
					streamEvent.Message.Usage.OutputTokens > 0 ||
					streamEvent.Message.Usage.CacheReadInputTokens > 0 ||
					streamEvent.Message.Usage.CacheCreationInputTokens > 0) {
				o.streamUsage = convertAnthropicUsage(streamEvent.Message.Usage)
				resp.Usage = o.streamUsage
			}
		}

		resp.Choices = []model.Choice{
			{
				Index: 0,
				Delta: &model.Message{
					Role: "assistant",
				},
			},
		}

	case "content_block_start":
		if streamEvent.ContentBlock != nil {
			switch streamEvent.ContentBlock.Type {
			case "tool_use":
				o.toolIndex++
				toolCall := model.ToolCall{
					Index: o.toolIndex,
					ID:    streamEvent.ContentBlock.ID,
					Type:  "function",
					Function: model.FunctionCall{
						Name:      lo.FromPtr(streamEvent.ContentBlock.Name),
						Arguments: "",
					},
				}
				o.toolCalls[o.toolIndex] = &toolCall

				resp.Choices = []model.Choice{
					{
						Index: 0,
						Delta: &model.Message{
							Role:      "assistant",
							ToolCalls: []model.ToolCall{toolCall},
						},
					},
				}
			case "text", "thinking":
				// These are handled in content_block_delta
				return nil, nil
			default:
				return nil, nil
			}
		}

	case "content_block_delta":
		if streamEvent.Delta != nil && streamEvent.Delta.Type != nil {
			choice := model.Choice{
				Index: 0,
				Delta: &model.Message{
					Role: "assistant",
				},
			}

			switch *streamEvent.Delta.Type {
			case "text_delta":
				if streamEvent.Delta.Text != nil {
					choice.Delta.Content = model.MessageContent{
						Content: streamEvent.Delta.Text,
					}
				}
			case "input_json_delta":
				if streamEvent.Delta.PartialJSON != nil && o.toolIndex >= 0 {
					choice.Delta.ToolCalls = []model.ToolCall{
						{
							Index: o.toolIndex,
							ID:    o.toolCalls[o.toolIndex].ID,
							Type:  "function",
							Function: model.FunctionCall{
								Arguments: *streamEvent.Delta.PartialJSON,
							},
						},
					}
				}
			case "thinking_delta":
				if streamEvent.Delta.Thinking != nil {
					choice.Delta.ReasoningContent = streamEvent.Delta.Thinking
				}
			case "signature_delta":
				if streamEvent.Delta.Signature != nil {
					choice.Delta.ReasoningSignature = streamEvent.Delta.Signature
				}
			default:
				return nil, nil
			}

			resp.Choices = []model.Choice{choice}
		}

	case "message_delta":
		if streamEvent.Usage != nil {
			usage := convertAnthropicUsage(streamEvent.Usage)
			if o.streamUsage != nil {
				usage.PromptTokens = o.streamUsage.PromptTokens
				usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
			}
			o.streamUsage = usage
		}

		if streamEvent.Delta != nil && streamEvent.Delta.StopReason != nil {
			finishReason := convertStopReason(streamEvent.Delta.StopReason)
			resp.Choices = []model.Choice{
				{
					Index:        0,
					FinishReason: finishReason,
				},
			}
		}

	case "message_stop":
		resp.Choices = []model.Choice{}
		if o.streamUsage != nil {
			resp.Usage = o.streamUsage
		}

	case "content_block_stop", "ping":
		return nil, nil

	default:
		return nil, nil
	}

	return resp, nil
}

// convertToAnthropicRequest converts internal LLM request to Anthropic format
func convertToAnthropicRequest(req *model.InternalLLMRequest) *anthropicModel.MessageRequest {
	result := &anthropicModel.MessageRequest{
		Model:       model.NormalizeAnthropicModelAlias(req.Model),
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stream:      req.Stream,
		MaxTokens:   resolveMaxTokens(req),
		System:      convertSystemPrompt(req),
	}

	if req.ServiceTier != nil {
		result.ServiceTier = *req.ServiceTier
	}
	if len(req.AnthropicContextManagement) > 0 {
		result.ContextManagement = append(json.RawMessage(nil), req.AnthropicContextManagement...)
	}
	rawThinkingPreserved := false
	if thinking, ok := decodeAnthropicThinking(req.AnthropicThinking); ok {
		result.Thinking = thinking
		rawThinkingPreserved = true
	}
	if outputConfig, ok := decodeAnthropicOutputConfig(req.AnthropicOutputConfig); ok {
		result.OutputConfig = outputConfig
	}

	if req.Metadata != nil && req.Metadata["user_id"] != "" {
		result.Metadata = &anthropicModel.AnthropicMetadata{UserID: req.Metadata["user_id"]}
	}

	// Convert messages
	result.Messages = convertMessages(req)

	// Convert tools
	if len(req.Tools) > 0 {
		result.Tools = convertTools(req.Tools)
	} else if req.AnthropicToolsPresent {
		result.ForceEmptyTools = true
	}

	// Convert tool choice and parallel tool-use preference. This keeps Claude
	// agent/MCP calls faithful when requests arrive from OpenAI/Gemini formats.
	if toolChoice := convertToolChoice(req.ToolChoice, req.ParallelToolCalls, len(req.Tools) > 0); toolChoice != nil {
		result.ToolChoice = toolChoice
	}

	// Convert stop sequences
	if req.Stop != nil {
		result.StopSequences = convertStopSequences(req.Stop)
	}

	// Convert thinking/reasoning
	if !rawThinkingPreserved && req.ReasoningEffort != "" {
		if req.AdaptiveThinking {
			result.Thinking = &anthropicModel.Thinking{
				Type: anthropicModel.ThinkingTypeAdaptive,
			}
			if result.OutputConfig == nil {
				result.OutputConfig = &anthropicModel.OutputConfig{
					Effort: req.ReasoningEffort,
				}
			}
		} else {
			result.Thinking = &anthropicModel.Thinking{
				Type:         anthropicModel.ThinkingTypeEnabled,
				BudgetTokens: getThinkingBudget(req.ReasoningEffort, req.ReasoningBudget, result.MaxTokens),
			}
		}
	}

	// Cross-protocol structured output: when an OpenAI client requested
	// json_schema structured output and there is no native Anthropic
	// output_config to honor (Claude Code's own output_config always wins and is
	// never overwritten), map the schema onto Anthropic's output_config.format so
	// the structured-output intent survives the OpenAI -> Anthropic hop.
	if result.OutputConfig == nil {
		if format, ok := anthropicOutputFormatFromResponseFormat(req.ResponseFormat); ok {
			result.OutputConfig = &anthropicModel.OutputConfig{Format: format}
		}
	}

	if req.TransformOptions.AnthropicAutoCacheControl {
		applyAutomaticCacheControl(result)
	}

	return result
}

// anthropicOutputFormatFromResponseFormat converts an OpenAI-style json_schema
// response_format into the Anthropic output_config.format shape
// ({"type":"json_schema","name":...,"schema":...}). It returns ok=false for nil,
// non-json_schema, or empty/unparseable schemas so the caller leaves
// output_config untouched. The internal ResponseFormat.JSONSchema arrives in two
// shapes: the OpenAI Chat wrapper {"name","schema","strict"} and the OpenAI
// Responses bare schema object; both are normalized here.
func anthropicOutputFormatFromResponseFormat(rf *model.ResponseFormat) (json.RawMessage, bool) {
	if rf == nil || rf.Type != "json_schema" || !rawJSONPresent(rf.JSONSchema) {
		return nil, false
	}

	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(rf.JSONSchema, &decoded); err != nil || decoded == nil {
		return nil, false
	}

	format := map[string]json.RawMessage{
		"type": json.RawMessage(`"json_schema"`),
	}

	// Chat-style wrapper carries the actual schema under "schema" alongside an
	// optional "name". Responses-style payloads are the bare schema object.
	if schema, ok := decoded["schema"]; ok && rawJSONPresent(schema) {
		format["schema"] = append(json.RawMessage(nil), schema...)
		if name, ok := decoded["name"]; ok && rawJSONPresent(name) {
			format["name"] = append(json.RawMessage(nil), name...)
		}
	} else {
		format["schema"] = append(json.RawMessage(nil), rf.JSONSchema...)
	}

	raw, err := json.Marshal(format)
	if err != nil {
		return nil, false
	}
	return raw, true
}

func decodeAnthropicThinking(raw json.RawMessage) (*anthropicModel.Thinking, bool) {
	if !rawJSONPresent(raw) {
		return nil, false
	}
	var thinking anthropicModel.Thinking
	if err := json.Unmarshal(raw, &thinking); err != nil || strings.TrimSpace(thinking.Type) == "" {
		return nil, false
	}
	return &thinking, true
}

func decodeAnthropicOutputConfig(raw json.RawMessage) (*anthropicModel.OutputConfig, bool) {
	if !rawJSONPresent(raw) {
		return nil, false
	}
	var outputConfig anthropicModel.OutputConfig
	if err := json.Unmarshal(raw, &outputConfig); err != nil {
		return nil, false
	}
	return &outputConfig, true
}

func rawJSONPresent(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return trimmed != "" && trimmed != "null"
}

func convertToolChoice(tc *model.ToolChoice, parallelToolCalls *bool, hasTools bool) *anthropicModel.ToolChoice {
	var result *anthropicModel.ToolChoice
	if tc != nil {
		if tc.ToolChoice != nil {
			switch strings.ToLower(*tc.ToolChoice) {
			case "auto":
				result = &anthropicModel.ToolChoice{Type: "auto"}
			case "none":
				result = &anthropicModel.ToolChoice{Type: "none"}
			case "required", "any":
				result = &anthropicModel.ToolChoice{Type: "any"}
			}
		} else if tc.NamedToolChoice != nil {
			toolType := strings.ToLower(tc.NamedToolChoice.Type)
			name := strings.TrimSpace(tc.NamedToolChoice.Function.Name)
			if name != "" && (toolType == "" || toolType == "function" || toolType == "tool") {
				result = &anthropicModel.ToolChoice{
					Type: "tool",
					Name: &name,
				}
			}
		}
	}

	if parallelToolCalls != nil && (result != nil || hasTools) {
		if result == nil {
			result = &anthropicModel.ToolChoice{Type: "auto"}
		}
		disable := !*parallelToolCalls
		result.DisableParallelToolUse = &disable
	}

	return result
}

func resolveMaxTokens(req *model.InternalLLMRequest) int64 {
	var maxtoken int64 = 1
	switch {
	case req.MaxTokens != nil:
		maxtoken = *req.MaxTokens
	case req.MaxCompletionTokens != nil:
		maxtoken = *req.MaxCompletionTokens
	default:
		maxtoken = 8192
	}
	if maxtoken < 1 {
		maxtoken = 1
	}
	return maxtoken
}

const claudeBillingHeaderPrefix = "x-anthropic-billing-header:"

// Named Claude CLI fingerprint constants observed on a genuine claude-cli request.
// ClaudeCLIVersion is the client version and MUST equal the version in
// model.DefaultClaudeHeaderUserAgent — TestClaudeFingerprintVersionConsistency
// guards against drift (this package cannot import internal/model: that would be an
// import cycle, since internal/model already depends on this package).
const (
	ClaudeCLIVersion       = "2.1.198"
	ClaudeCLIVersionSuffix = "542"
)

// claudeBillingHeaderText returns the Claude CLI billing-header system block.
// Real Claude Code requests carry this as the first system text block; upstream
// relays (e.g. the relay) use it to recognise a genuine Claude CLI client, so
// omitting it gets the request risk-rejected (429/503 before the business layer).
func claudeBillingHeaderText() string {
	return claudeBillingHeaderPrefix +
		" cc_version=" + ClaudeCLIVersion + "." + ClaudeCLIVersionSuffix +
		"; cc_entrypoint=sdk-cli;"
}

// claudeAgentIdentityText is the Claude Code agent-identity system block. Real
// Claude Code sends it as the second system block (after the billing header).
// Upstream relays (e.g. the relay) route by it: a request whose system carries the
// billing header but NOT this genuine agent identity is risk-rejected (429/503)
// before the business layer, so non-CLI clients get it injected to reach the
// serving pool while real CLI requests pass through unchanged.
const claudeAgentIdentityText = "You are a Claude agent, built on Anthropic's Claude Agent SDK."

func systemPromptHasClaudeAgentIdentity(parts []anthropicModel.SystemPromptPart) bool {
	for _, p := range parts {
		if strings.Contains(p.Text, "built on Anthropic's Claude Agent SDK") {
			return true
		}
	}
	return false
}

func convertSystemPrompt(req *model.InternalLLMRequest) *anthropicModel.SystemPrompt {
	var systemMessages []model.Message
	for _, msg := range req.Messages {
		if msg.Role == "system" {
			systemMessages = append(systemMessages, msg)
		}
	}

	parts := make([]anthropicModel.SystemPromptPart, 0, len(systemMessages)+1)
	for _, msg := range systemMessages {
		parts = append(parts, anthropicModel.SystemPromptPart{
			Type:         "text",
			Text:         lo.FromPtr(msg.Content.Content),
			CacheControl: convertCacheControl(msg.CacheControl),
		})
	}

	// Cloak disabled (channel cloak mode "never"): do not synthesize the Claude CLI
	// billing-header / agent-identity blocks. Pass the client's own system blocks
	// through untouched so non-the relay Anthropic-compatible upstreams (e.g. domestic
	// GLM/DeepSeek anthropic endpoints) never receive injected Claude identity. A
	// genuine Claude CLI client's own billing/identity blocks (sent as system messages)
	// still flow through here; only octopus's synthetic injection is skipped.
	if req.TransformOptions.SuppressClaudeIdentity {
		if len(parts) == 0 {
			return nil
		}
		return &anthropicModel.SystemPrompt{MultiplePrompts: parts}
	}

	// Prepend the Claude CLI billing-header system block when the client did not
	// already send one (genuine Claude CLI puts it as the first system block, so
	// real CLI requests pass through unchanged; non-CLI clients get it injected).
	if len(parts) == 0 || !strings.HasPrefix(strings.TrimSpace(parts[0].Text), claudeBillingHeaderPrefix) {
		parts = append([]anthropicModel.SystemPromptPart{{
			Type: "text",
			Text: claudeBillingHeaderText(),
		}}, parts...)
	}

	// Ensure the genuine Claude Code agent-identity block as well. Keep it right after
	// the billing header (the order real Claude Code uses); inject only when missing
	// so authentic CLI requests are untouched.
	if !systemPromptHasClaudeAgentIdentity(parts) {
		identity := anthropicModel.SystemPromptPart{Type: "text", Text: claudeAgentIdentityText}
		if len(parts) > 0 {
			parts = append([]anthropicModel.SystemPromptPart{parts[0], identity}, parts[1:]...)
		} else {
			parts = append(parts, identity)
		}
	}

	return &anthropicModel.SystemPrompt{
		MultiplePrompts: parts,
	}
}

// filterOutResponsesCustomTools removes OpenAI Responses custom (freeform) tool
// calls — and the tool_result messages paired to them — from the history before
// it is encoded for a Claude upstream. Anthropic has no freeform-tool concept:
// tool_use.input must be a JSON object, so a custom tool's freeform payload
// cannot be represented. Rather than send Claude a tool_use with an empty "{}"
// input (which makes the model "forget" the call and can desync tool_use /
// tool_result pairing into a 400), the custom call and its result are dropped
// together. Standard function tool calls are untouched. Mirrors axonhub's
// FilterOutResponseCustomToolMessages.
func filterOutResponsesCustomTools(messages []model.Message) []model.Message {
	// Pass 1: collect every custom tool call id up front. Collecting before we
	// filter means a tool_result that appears BEFORE its custom tool call
	// (out-of-order / dirty history) is still recognized as an orphan and dropped,
	// instead of being encoded for Claude without a matching tool_use (which the
	// API rejects with a 400). A single forward pass could not see the later call.
	// Only non-empty ids are tracked — an id-less custom call cannot be paired to a
	// specific result, so we do not blanket-drop every id-less tool_result (that
	// would delete legitimate ones); this mirrors axonhub's FilterOutResponse-
	// CustomToolMessages, which likewise matches only by concrete id.
	removed := make(map[string]struct{})
	for _, msg := range messages {
		for _, tc := range msg.ToolCalls {
			if tc.Type == model.ToolCallTypeCustom && tc.ID != "" {
				removed[tc.ID] = struct{}{}
			}
		}
	}
	if len(removed) == 0 {
		return messages
	}

	// Pass 2: drop the custom tool calls and their paired tool_results.
	filtered := make([]model.Message, 0, len(messages))
	for _, msg := range messages {
		if msg.Role == "tool" && msg.ToolCallID != nil {
			if _, ok := removed[*msg.ToolCallID]; ok {
				continue
			}
		}

		if len(msg.ToolCalls) == 0 {
			filtered = append(filtered, msg)
			continue
		}

		kept := make([]model.ToolCall, 0, len(msg.ToolCalls))
		for _, tc := range msg.ToolCalls {
			if tc.Type == model.ToolCallTypeCustom {
				continue
			}
			kept = append(kept, tc)
		}

		cloned := msg
		cloned.ToolCalls = kept
		// Drop an assistant turn that carried nothing but the stripped custom
		// tool call, so we don't encode an empty message.
		if isEmptyAfterCustomToolFilter(cloned) {
			continue
		}
		filtered = append(filtered, cloned)
	}

	return filtered
}

func isEmptyAfterCustomToolFilter(msg model.Message) bool {
	if len(msg.ToolCalls) > 0 {
		return false
	}
	if msg.Content.Content != nil && *msg.Content.Content != "" {
		return false
	}
	if len(msg.Content.MultipleContent) > 0 {
		return false
	}
	if msg.Refusal != "" || msg.ToolCallID != nil || msg.FunctionCall != nil ||
		msg.ReasoningContent != nil || msg.Reasoning != nil || msg.ReasoningSignature != nil ||
		msg.Audio != nil || len(msg.Images) > 0 {
		return false
	}
	return true
}

func convertMessages(req *model.InternalLLMRequest) []anthropicModel.MessageParam {
	filteredMessages := filterOutResponsesCustomTools(req.Messages)
	messages := make([]anthropicModel.MessageParam, 0, len(filteredMessages))
	processedIndexes := make(map[int]bool)

	for _, msg := range filteredMessages {
		if msg.Role == "system" {
			continue
		}

		converted := convertSingleMessage(msg, filteredMessages, processedIndexes)
		for _, convertedMsg := range converted {
			// Anthropic API 要求消息角色必须交替出现（user/assistant/user/assistant）。
			// 当 OpenAI 格式的多个连续 tool 消息被各自转换为独立的 user 消息时，
			// 会产生连续的同角色消息，需要合并以避免 "Improperly formed request" 错误。
			if n := len(messages); n > 0 && messages[n-1].Role == convertedMsg.Role {
				last := &messages[n-1]
				last.Content = anthropicModel.MessageContent{
					MultipleContent: append(contentToBlocks(last.Content), contentToBlocks(convertedMsg.Content)...),
				}
			} else {
				messages = append(messages, convertedMsg)
			}
		}
	}

	return messages
}

// contentToBlocks 将 MessageContent 统一展开为 MessageContentBlock 切片。
func contentToBlocks(c anthropicModel.MessageContent) []anthropicModel.MessageContentBlock {
	if len(c.MultipleContent) > 0 {
		// 返回副本，避免后续 append 污染原 slice
		return append([]anthropicModel.MessageContentBlock(nil), c.MultipleContent...)
	}
	if c.Content != nil && *c.Content != "" {
		return []anthropicModel.MessageContentBlock{{Type: "text", Text: c.Content}}
	}
	return nil
}

func convertSingleMessage(msg model.Message, allMessages []model.Message, processedIndexes map[int]bool) []anthropicModel.MessageParam {
	switch msg.Role {
	case "tool":
		return convertToolMessage(msg, allMessages, processedIndexes)
	case "user":
		if msg.MessageIndex != nil && processedIndexes[*msg.MessageIndex] {
			return nil
		}
		return convertUserMessage(msg)
	case "assistant":
		return convertAssistantMessage(msg)
	default:
		return nil
	}
}

func convertToolMessage(msg model.Message, allMessages []model.Message, processedIndexes map[int]bool) []anthropicModel.MessageParam {
	if msg.MessageIndex == nil {
		return []anthropicModel.MessageParam{{
			Role: "user",
			Content: anthropicModel.MessageContent{
				MultipleContent: []anthropicModel.MessageContentBlock{convertToolResultBlock(msg)},
			},
		}}
	}

	if processedIndexes[*msg.MessageIndex] {
		return nil
	}

	var toolMsgs []model.Message
	for _, m := range allMessages {
		if m.Role == "tool" && m.MessageIndex != nil && *m.MessageIndex == *msg.MessageIndex {
			toolMsgs = append(toolMsgs, m)
		}
	}

	if len(toolMsgs) == 0 {
		return nil
	}

	contentBlocks := make([]anthropicModel.MessageContentBlock, 0, len(toolMsgs))
	for _, tm := range toolMsgs {
		contentBlocks = append(contentBlocks, convertToolResultBlock(tm))
	}

	// Merge the associated user message content (if any) into the same Anthropic user message.
	// In Anthropic Messages, tool_result blocks live inside a user message's content array.
	// Our internal format represents tool results as separate "tool" role messages, but the
	// original Anthropic request may also include additional user content alongside tool_result.
	if userMsg := findUserMessageByIndex(allMessages, *msg.MessageIndex); userMsg != nil {
		userContent := buildMessageContent(*userMsg)
		contentBlocks = append(contentBlocks, contentToBlocks(userContent)...)
	}

	processedIndexes[*msg.MessageIndex] = true

	return []anthropicModel.MessageParam{{
		Role:    "user",
		Content: anthropicModel.MessageContent{MultipleContent: contentBlocks},
	}}
}

func findUserMessageByIndex(allMessages []model.Message, messageIndex int) *model.Message {
	for i := range allMessages {
		m := &allMessages[i]
		if m.Role == "user" && m.MessageIndex != nil && *m.MessageIndex == messageIndex {
			return m
		}
	}
	return nil
}

// claudeToolIDSanitizer matches any character not allowed in a Claude
// tool_use.id / tool_result.tool_use_id (Claude requires ^[a-zA-Z0-9_-]+$).
var claudeToolIDSanitizer = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// sanitizeClaudeToolID replaces characters a non-compliant upstream may put in a
// tool id (e.g. '/', '.', ':') with '_', so Claude does not reject the request.
// The mapping is deterministic, so a tool_use.id and its paired
// tool_result.tool_use_id — both derived from the same original id — stay equal
// and the pairing is preserved. Empty ids are left as-is (a synthetic replacement
// would risk mismatching the pair), mirroring CLIProxyAPI's SanitizeClaudeToolID.
func sanitizeClaudeToolID(id string) string {
	if id == "" {
		return ""
	}
	return claudeToolIDSanitizer.ReplaceAllString(id, "_")
}

func sanitizeClaudeToolIDPtr(id *string) *string {
	if id == nil {
		return nil
	}
	s := sanitizeClaudeToolID(*id)
	return &s
}

// toolUseInput returns a valid Claude tool_use.input. Claude requires input to be
// a JSON object, so arguments are used verbatim only when they are a valid JSON
// object; empty, syntactically invalid, or non-object arguments (including a codex
// freeform/custom tool's non-JSON payload) collapse to "{}" instead of being sent
// as-is and rejected — the same guard the reference relays use (CLIProxyAPI checks
// gjson.Valid && IsObject, axonhub SafeJSONRawMessage).
func toolUseInput(arguments string) json.RawMessage {
	if arguments != "" && json.Valid([]byte(arguments)) {
		if trimmed := strings.TrimSpace(arguments); len(trimmed) > 0 && trimmed[0] == '{' {
			return json.RawMessage(arguments)
		}
	}
	return json.RawMessage("{}")
}

func convertToolResultBlock(msg model.Message) anthropicModel.MessageContentBlock {
	block := anthropicModel.MessageContentBlock{
		Type:         "tool_result",
		ToolUseID:    sanitizeClaudeToolIDPtr(msg.ToolCallID),
		CacheControl: convertCacheControl(msg.CacheControl),
		IsError:      msg.ToolCallIsError,
	}

	if msg.Content.Content != nil {
		block.Content = &anthropicModel.MessageContent{
			Content: msg.Content.Content,
		}
	} else if len(msg.Content.MultipleContent) > 0 {
		blocks := make([]anthropicModel.MessageContentBlock, 0, len(msg.Content.MultipleContent))
		for _, part := range msg.Content.MultipleContent {
			switch part.Type {
			case "text":
				text := part.Text
				if text == nil {
					text = lo.ToPtr("")
				}
				blocks = append(blocks, anthropicModel.MessageContentBlock{
					Type: "text",
					Text: text,
				})
			case "image_url":
				if imageBlock := convertImageURLToBlock(part); imageBlock != nil {
					blocks = append(blocks, *imageBlock)
				}
			case "file":
				if docBlock := convertFileToDocumentBlock(part); docBlock != nil {
					blocks = append(blocks, *docBlock)
				}
			case "input_audio":
				// Audio inside a tool_result has no Anthropic equivalent either;
				// degrade to a visible placeholder instead of dropping it.
				if audioBlock := convertAudioToPlaceholderBlock(part); audioBlock != nil {
					blocks = append(blocks, *audioBlock)
				}
			}
		}

		if len(blocks) > 0 {
			block.Content = &anthropicModel.MessageContent{
				MultipleContent: blocks,
			}
		}
	}

	if block.Content == nil {
		block.Content = &anthropicModel.MessageContent{
			Content: lo.ToPtr(""),
		}
	}

	return block
}

func convertUserMessage(msg model.Message) []anthropicModel.MessageParam {
	content := buildMessageContent(msg)
	return []anthropicModel.MessageParam{{Role: "user", Content: content}}
}

func convertAssistantMessage(msg model.Message) []anthropicModel.MessageParam {
	if len(msg.ToolCalls) > 0 {
		return convertAssistantWithToolCalls(msg)
	}

	content := buildMessageContent(msg)
	return []anthropicModel.MessageParam{{Role: "assistant", Content: content}}
}

func convertAssistantWithToolCalls(msg model.Message) []anthropicModel.MessageParam {
	var blocks []anthropicModel.MessageContentBlock

	// Add thinking block only when it carries a signature. Anthropic 400s on a thinking block
	// without a valid signature, so a reasoning block that lost its signature crossing a
	// protocol boundary (codex Responses -> Anthropic multi-turn, where claude's
	// thinking.signature was never surfaced back as reasoning.encrypted_content) must be
	// dropped rather than sent unsigned. A genuine claude->claude turn always carries the
	// signature, so this is a no-op there. Mirrors CLIProxyAPI (drops on an empty signature).
	if msg.ReasoningContent != nil && *msg.ReasoningContent != "" && anthropicThinkingSignaturePresent(msg.ReasoningSignature) {
		blocks = append(blocks, anthropicModel.MessageContentBlock{
			Type:      "thinking",
			Thinking:  msg.ReasoningContent,
			Signature: msg.ReasoningSignature,
		})
	}

	// Add text content if present
	if msg.Content.Content != nil && *msg.Content.Content != "" {
		blocks = append(blocks, anthropicModel.MessageContentBlock{
			Type:         "text",
			Text:         msg.Content.Content,
			CacheControl: convertCacheControl(msg.CacheControl),
		})
	} else if len(msg.Content.MultipleContent) > 0 {
		for _, part := range msg.Content.MultipleContent {
			if part.Type == "text" && part.Text != nil {
				blocks = append(blocks, anthropicModel.MessageContentBlock{
					Type:         "text",
					Text:         part.Text,
					CacheControl: convertCacheControl(part.CacheControl),
				})
			}
		}
	}

	// Add tool calls
	for _, toolCall := range msg.ToolCalls {
		blocks = append(blocks, anthropicModel.MessageContentBlock{
			Type:         "tool_use",
			ID:           sanitizeClaudeToolID(toolCall.ID),
			Name:         &toolCall.Function.Name,
			Input:        toolUseInput(toolCall.Function.Arguments),
			CacheControl: convertCacheControl(toolCall.CacheControl),
		})
	}

	if len(blocks) == 0 {
		return nil
	}

	return []anthropicModel.MessageParam{{
		Role:    "assistant",
		Content: anthropicModel.MessageContent{MultipleContent: blocks},
	}}
}

func buildMessageContent(msg model.Message) anthropicModel.MessageContent {
	// Handle simple string content
	if msg.Content.Content != nil {
		if msg.CacheControl != nil || hasThinkingContent(msg) {
			return buildMultipleContentWithThinking(msg)
		}
		return anthropicModel.MessageContent{Content: msg.Content.Content}
	}

	// Handle multiple content parts
	if len(msg.Content.MultipleContent) > 0 {
		return convertMultiplePartContent(msg)
	}

	return anthropicModel.MessageContent{}
}

func hasThinkingContent(msg model.Message) bool {
	return msg.ReasoningContent != nil && *msg.ReasoningContent != ""
}

// anthropicThinkingSignaturePresent reports whether a captured reasoning signature can be
// replayed as an Anthropic thinking-block signature. Anthropic rejects (400) a thinking block
// whose signature is absent, so an unsigned reasoning block must be dropped rather than
// emitted. Mirrors CLIProxyAPI convertResponsesReasoningToClaudeThinking.
func anthropicThinkingSignaturePresent(sig *string) bool {
	return sig != nil && strings.TrimSpace(*sig) != ""
}

func buildMultipleContentWithThinking(msg model.Message) anthropicModel.MessageContent {
	var blocks []anthropicModel.MessageContentBlock

	// Drop an unsigned thinking block (see anthropicThinkingSignaturePresent). The text block
	// below is always appended, so the assistant message is never left empty.
	if msg.ReasoningContent != nil && *msg.ReasoningContent != "" && anthropicThinkingSignaturePresent(msg.ReasoningSignature) {
		blocks = append(blocks, anthropicModel.MessageContentBlock{
			Type:      "thinking",
			Thinking:  msg.ReasoningContent,
			Signature: msg.ReasoningSignature,
		})
	}

	blocks = append(blocks, anthropicModel.MessageContentBlock{
		Type:         "text",
		Text:         msg.Content.Content,
		CacheControl: convertCacheControl(msg.CacheControl),
	})

	return anthropicModel.MessageContent{MultipleContent: blocks}
}

func convertMultiplePartContent(msg model.Message) anthropicModel.MessageContent {
	blocks := make([]anthropicModel.MessageContentBlock, 0, len(msg.Content.MultipleContent))

	for _, part := range msg.Content.MultipleContent {
		switch part.Type {
		case "text":
			if part.Text != nil {
				blocks = append(blocks, anthropicModel.MessageContentBlock{
					Type:         "text",
					Text:         part.Text,
					CacheControl: convertCacheControl(part.CacheControl),
				})
			}
		case "image_url":
			if part.ImageURL != nil && part.ImageURL.URL != "" {
				block := convertImageURLToBlock(part)
				if block != nil {
					blocks = append(blocks, *block)
				}
			}
		case "file":
			if block := convertFileToDocumentBlock(part); block != nil {
				blocks = append(blocks, *block)
			}
		case "input_audio":
			// Anthropic's Messages API has no audio input block, so audio sent
			// downstream over an Anthropic channel cannot be forwarded faithfully.
			// Rather than silently dropping it (which loses the turn's intent and
			// can desync a multimodal conversation), degrade to a visible text
			// placeholder and log a warning so the loss is observable. This mirrors
			// the sub2api "degrade, don't drop" philosophy.
			if block := convertAudioToPlaceholderBlock(part); block != nil {
				blocks = append(blocks, *block)
			}
		}
	}

	// Add tool calls if present
	for _, toolCall := range msg.ToolCalls {
		blocks = append(blocks, anthropicModel.MessageContentBlock{
			Type:         "tool_use",
			ID:           sanitizeClaudeToolID(toolCall.ID),
			Name:         &toolCall.Function.Name,
			Input:        toolUseInput(toolCall.Function.Arguments),
			CacheControl: convertCacheControl(toolCall.CacheControl),
		})
	}

	if len(blocks) == 0 {
		return anthropicModel.MessageContent{}
	}

	return anthropicModel.MessageContent{MultipleContent: blocks}
}

func convertImageURLToBlock(part model.MessageContentPart) *anthropicModel.MessageContentBlock {
	if part.ImageURL == nil || part.ImageURL.URL == "" {
		return nil
	}

	url := part.ImageURL.URL
	if parsed := xurl.ParseDataURL(url); parsed != nil {
		return &anthropicModel.MessageContentBlock{
			Type: "image",
			Source: &anthropicModel.ImageSource{
				Type:      "base64",
				MediaType: parsed.MediaType,
				Data:      parsed.Data,
			},
			CacheControl: convertCacheControl(part.CacheControl),
		}
	}

	return &anthropicModel.MessageContentBlock{
		Type: "image",
		Source: &anthropicModel.ImageSource{
			Type: "url",
			URL:  part.ImageURL.URL,
		},
		CacheControl: convertCacheControl(part.CacheControl),
	}
}

// convertFileToDocumentBlock rebuilds the internal "file" content part into an
// Anthropic `document` block, mirroring convertImageURLToBlock for images. It
// supports base64 (data URL or raw base64 + media type), remote url, and
// provider file_id sources. Returns nil when there is nothing to rebuild.
func convertFileToDocumentBlock(part model.MessageContentPart) *anthropicModel.MessageContentBlock {
	if part.File == nil {
		return nil
	}
	file := part.File

	var source *anthropicModel.ImageSource
	switch {
	case file.FileData != "":
		if parsed := xurl.ParseDataURL(file.FileData); parsed != nil {
			mediaType := parsed.MediaType
			if mediaType == "" {
				mediaType = file.MediaType
			}
			source = &anthropicModel.ImageSource{
				Type:      "base64",
				MediaType: mediaType,
				Data:      parsed.Data,
			}
		} else {
			source = &anthropicModel.ImageSource{
				Type:      "base64",
				MediaType: file.MediaType,
				Data:      file.FileData,
			}
		}
	case file.FileURL != "":
		// Prefer an explicit MediaType; otherwise infer it (network-free) from the
		// URL extension so the document block is not left with an empty media type.
		mediaType := file.MediaType
		if mediaType == "" {
			mediaType = mime.FromURL(file.FileURL)
		}
		source = &anthropicModel.ImageSource{
			Type:      "url",
			MediaType: mediaType,
			URL:       file.FileURL,
		}
	case file.FileID != "":
		source = &anthropicModel.ImageSource{
			Type:   "file",
			FileID: file.FileID,
		}
	default:
		return nil
	}

	return &anthropicModel.MessageContentBlock{
		Type:         "document",
		Source:       source,
		CacheControl: convertCacheControl(part.CacheControl),
	}
}

// convertAudioToPlaceholderBlock turns an OpenAI-style input_audio part into a
// visible text block, because the Anthropic Messages API has no audio input
// block. The original behavior silently dropped audio (no case in the content
// switch), which loses the conversation turn. Degrading to a labeled placeholder
// keeps the loss observable to the model and downstream, and a warning is logged
// so operators can see audio is being shed over an Anthropic channel. Returns
// nil when there is no audio payload to describe.
func convertAudioToPlaceholderBlock(part model.MessageContentPart) *anthropicModel.MessageContentBlock {
	if part.Audio == nil {
		return nil
	}

	format := strings.TrimSpace(part.Audio.Format)
	if format == "" {
		format = "unknown"
	}
	log.Warnf("anthropic outbound: audio input (format=%s) cannot be sent over an Anthropic channel; replaced with a text placeholder", format)

	placeholder := fmt.Sprintf("[audio input (%s) omitted: the Anthropic Messages API does not accept audio]", format)
	return &anthropicModel.MessageContentBlock{
		Type:         "text",
		Text:         &placeholder,
		CacheControl: convertCacheControl(part.CacheControl),
	}
}

func convertTools(tools []model.Tool) []anthropicModel.Tool {
	result := make([]anthropicModel.Tool, 0, len(tools))
	for _, tool := range tools {
		// Anthropic built-in tools (computer-use, bash, text_editor, ...) were
		// preserved verbatim on the inbound path. Restore the original tool object
		// instead of dropping it, so proprietary fields (display_width_px, etc.)
		// reach the model. cache_control attached on the internal side is merged
		// back in by anthropicModel.Tool.MarshalJSON.
		if tool.Type == model.ToolTypeAnthropicBuiltin && len(tool.RawTool) > 0 {
			result = append(result, anthropicModel.Tool{
				Raw:          tool.RawTool,
				CacheControl: convertCacheControl(tool.CacheControl),
			})
			continue
		}
		if tool.Type != "function" {
			continue
		}
		result = append(result, anthropicModel.Tool{
			Name:         tool.Function.Name,
			Description:  tool.Function.Description,
			InputSchema:  tool.Function.Parameters,
			CacheControl: convertCacheControl(tool.CacheControl),
		})
	}
	return result
}

func convertStopSequences(stop *model.Stop) []string {
	if stop == nil {
		return nil
	}
	if stop.Stop != nil {
		return []string{*stop.Stop}
	}
	if len(stop.MultipleStop) > 0 {
		return stop.MultipleStop
	}
	return nil
}

func convertCacheControl(cc *model.CacheControl) *anthropicModel.CacheControl {
	if cc == nil {
		return nil
	}
	return &anthropicModel.CacheControl{
		Type: cc.Type,
		TTL:  cc.TTL,
	}
}

func getThinkingBudget(effort string, budget *int64, maxTokens int64) *int64 {
	var result int64
	if budget != nil {
		result = *budget
	} else {
		switch effort {
		case anthropicModel.EffortLow:
			result = 1024
		case anthropicModel.EffortMedium:
			result = 8192
		case anthropicModel.EffortHigh:
			result = 32768
		case anthropicModel.EffortMax:
			// "max" maps to a budget above "high".
			result = 64000
		default:
			result = 8192
		}
	}

	// Anthropic requires thinking budget_tokens < max_tokens. Clamp so a high/max
	// effort (or an oversized explicit budget) never produces an upstream 400 when
	// the resolved max_tokens is small (e.g. the 8192 default). maxTokens <= 0 means
	// unknown/unbounded, so leave the budget untouched in that case.
	if maxTokens > 0 && result >= maxTokens {
		result = maxTokens - 1
	}
	return &result
}

// Response conversion functions

func convertToLLMResponse(resp *anthropicModel.Message) *model.InternalLLMResponse {
	if resp == nil {
		return &model.InternalLLMResponse{
			Object: "chat.completion",
		}
	}

	result := &model.InternalLLMResponse{
		ID:      resp.ID,
		Object:  "chat.completion",
		Model:   resp.Model,
		Created: 0,
	}

	var (
		content           model.MessageContent
		thinkingText      *string
		thinkingSignature *string
		toolCalls         []model.ToolCall
		textParts         []string
	)

	for _, block := range resp.Content {
		switch block.Type {
		case "text":
			if block.Text != nil && *block.Text != "" {
				textParts = append(textParts, *block.Text)
				content.MultipleContent = append(content.MultipleContent, model.MessageContentPart{
					Type: "text",
					Text: block.Text,
				})
			}
		case "tool_use":
			if block.ID != "" && block.Name != nil {
				input := "{}"
				if len(block.Input) > 0 {
					input = string(block.Input)
				}
				toolCalls = append(toolCalls, model.ToolCall{
					ID:   block.ID,
					Type: "function",
					Function: model.FunctionCall{
						Name:      *block.Name,
						Arguments: input,
					},
				})
			}
		case "thinking":
			if block.Thinking != nil {
				thinkingText = block.Thinking
			}
			thinkingSignature = block.Signature
		}
	}

	// If we only have text content, use simple string format
	if len(textParts) > 0 && len(content.MultipleContent) == len(textParts) {
		allText := strings.Join(textParts, "")
		content.Content = &allText
		content.MultipleContent = nil
	}

	message := &model.Message{
		Role:               resp.Role,
		Content:            content,
		ToolCalls:          toolCalls,
		ReasoningContent:   thinkingText,
		ReasoningSignature: thinkingSignature,
	}

	choice := model.Choice{
		Index:        0,
		Message:      message,
		FinishReason: convertStopReason(resp.StopReason),
	}

	result.Choices = []model.Choice{choice}
	result.Usage = convertAnthropicUsage(resp.Usage)

	return result
}

func convertStopReason(stopReason *string) *string {
	if stopReason == nil {
		return nil
	}

	switch *stopReason {
	case "end_turn":
		return lo.ToPtr("stop")
	case "max_tokens":
		return lo.ToPtr("length")
	case "stop_sequence", "pause_turn":
		return lo.ToPtr("stop")
	case "tool_use":
		return lo.ToPtr("tool_calls")
	case "refusal":
		return lo.ToPtr("content_filter")
	default:
		return stopReason
	}
}

func convertAnthropicUsage(usage *anthropicModel.Usage) *model.Usage {
	if usage == nil {
		return nil
	}

	result := &model.Usage{
		PromptTokens:             usage.InputTokens,
		CompletionTokens:         usage.OutputTokens,
		TotalTokens:              usage.InputTokens + usage.OutputTokens + usage.CacheReadInputTokens + usage.CacheCreationInputTokens,
		CacheCreationInputTokens: usage.CacheCreationInputTokens,
		AnthropicUsage:           true,
	}

	if usage.CacheReadInputTokens > 0 {
		result.PromptTokensDetails = &model.PromptTokensDetails{
			CachedTokens: usage.CacheReadInputTokens,
		}
	}
	return result
}
