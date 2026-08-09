package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

type APIFormat string

const (
	APIFormatOpenAIChatCompletion  APIFormat = "openai/chat_completions"
	APIFormatOpenAIResponse        APIFormat = "openai/responses"
	APIFormatOpenAIImageGeneration APIFormat = "openai/image_generation"
	APIFormatOpenAIEmbedding       APIFormat = "openai/embeddings"
	APIFormatGeminiContents        APIFormat = "gemini/contents"
	APIFormatAnthropicMessage      APIFormat = "anthropic/messages"
	APIFormatAiSDKText             APIFormat = "aisdk/text"
	APIFormatAiSDKDataStream       APIFormat = "aisdk/datastream"
)

// Request is the unified llm request model for AxonHub, to keep compatibility with major app and framework.
// It choose to base on the OpenAI chat completion request, but add some extra fields to support more features.
type InternalLLMRequest struct {
	// Messages is a list of messages to send to the llm model.
	// For chat completion requests, this field is required.
	// For embedding requests, this field should be empty and Input should be used instead.
	Messages []Message `json:"messages,omitempty" validator:"required,min=1"`

	// Embedding API 参数（与 Messages 互斥）
	// EmbeddingInput is the text or texts to get embeddings for.
	// For embedding requests, this field is required.
	// For chat completion requests, this field should be empty.
	EmbeddingInput *EmbeddingInput `json:"embedding_input,omitempty"` // string or string[]
	// EmbeddingDimensions is the number of dimensions for the embedding output.
	// Only supported for certain embedding models.
	EmbeddingDimensions *int64 `json:"embedding_dimensions,omitempty"`
	// EmbeddingEncodingFormat is the format of the embedding output.
	// Can be "float" or "base64". Defaults to "float".
	EmbeddingEncodingFormat *string `json:"embedding_encoding_format,omitempty"`

	// Model is the model ID used to generate the response.
	Model string `json:"model" validator:"required"`

	// Number between -2.0 and 2.0. Positive values penalize new tokens based on
	// their existing frequency in the text so far, decreasing the model's likelihood
	// to repeat the same line verbatim.
	//
	// See [OpenAI's
	// documentation](https://platform.openai.com/docs/api-reference/parameter-details)
	// for more information.
	FrequencyPenalty *float64 `json:"frequency_penalty,omitempty"`

	// Whether to return log probabilities of the output tokens or not. If true,
	// returns the log probabilities of each output token returned in the `content` of
	// `message`.
	Logprobs *bool `json:"logprobs,omitempty"`

	// An upper bound for the number of tokens that can be generated for a completion,
	// including visible output tokens and
	// [reasoning tokens](https://platform.openai.com/docs/guides/reasoning).
	MaxCompletionTokens *int64 `json:"max_completion_tokens,omitempty"`

	// The maximum number of [tokens](/tokenizer) that can be generated in the chat
	// completion. This value can be used to control
	// [costs](https://openai.com/api/pricing/) for text generated via API.
	//
	// This value is now deprecated in favor of `max_completion_tokens`, and is not
	// compatible with
	// [o-series models](https://platform.openai.com/docs/guides/reasoning).
	MaxTokens *int64 `json:"max_tokens,omitempty"`

	// How many chat completion choices / images to generate. Billing is usage-token based
	// (buildBillingSnapshot), so cost scales with the upstream usage returned for all
	// choices/images — no separate n multiplier needed. Forwarded to chat and threaded
	// into the /v1/images/generations bridge payload; mirrors new-api, which carries a
	// general top-level n.
	N *int64 `json:"n,omitempty"`

	// Number between -2.0 and 2.0. Positive values penalize new tokens based on
	// whether they appear in the text so far, increasing the model's likelihood to
	// talk about new topics.
	PresencePenalty *float64 `json:"presence_penalty,omitempty"`

	// This feature is in Beta. If specified, our system will make a best effort to
	// sample deterministically, such that repeated requests with the same `seed` and
	// parameters should return the same result. Determinism is not guaranteed, and you
	// should refer to the `system_fingerprint` response parameter to monitor changes
	// in the backend.
	Seed *int64 `json:"seed,omitempty"`

	// Whether or not to store the output of this chat completion request for use in
	// our [model distillation](https://platform.openai.com/docs/guides/distillation)
	// or [evals](https://platform.openai.com/docs/guides/evals) products.
	//
	// Supports text and image inputs. Note: image inputs over 10MB will be dropped.
	Store *bool `json:"store,omitzero"`

	// What sampling temperature to use, between 0 and 2. Higher values like 0.8 will
	// make the output more random, while lower values like 0.2 will make it more
	// focused and deterministic. We generally recommend altering this or `top_p` but
	// not both.
	Temperature *float64 `json:"temperature,omitempty"`

	// An integer between 0 and 20 specifying the number of most likely tokens to
	// return at each token position, each with an associated log probability.
	// `logprobs` must be set to `true` if this parameter is used.
	TopLogprobs *int64 `json:"top_logprobs,omitzero"`

	// An alternative to sampling with temperature, called nucleus sampling, where the
	// model considers the results of the tokens with top_p probability mass. So 0.1
	// means only the tokens comprising the top 10% probability mass are considered.
	//
	// We generally recommend altering this or `temperature` but not both.
	TopP *float64 `json:"top_p,omitempty"`

	// Used by OpenAI to cache responses for similar requests to optimize your cache
	// hit rates. Replaces the `user` field.
	// [Learn more](https://platform.openai.com/docs/guides/prompt-caching).
	PromptCacheKey *string `json:"prompt_cache_key,omitempty"`

	// The retention policy for the prompt cache. Supported by OpenAI Chat
	// Completions and Responses. Valid values are provider-defined.
	PromptCacheRetention *string `json:"prompt_cache_retention,omitempty"`

	// PreviousResponseID is the OpenAI Responses conversation cursor. It must only
	// be forwarded to Responses upstreams that can own that response id.
	PreviousResponseID *string `json:"previous_response_id,omitempty"`

	// A stable identifier used to help detect users of your application that may be
	// violating OpenAI's usage policies. The IDs should be a string that uniquely
	// identifies each user. We recommend hashing their username or email address, in
	// order to avoid sending us any identifying information.
	// [Learn more](https://platform.openai.com/docs/guides/safety-best-practices#safety-identifiers).
	SafetyIdentifier *string `json:"safety_identifier,omitzero"`

	// This field is being replaced by `safety_identifier` and `prompt_cache_key`. Use
	// `prompt_cache_key` instead to maintain caching optimizations. A stable
	// identifier for your end-users. Used to boost cache hit rates by better bucketing
	// similar requests and to help OpenAI detect and prevent abuse.
	// [Learn more](https://platform.openai.com/docs/guides/safety-best-practices#safety-identifiers).
	User *string `json:"user,omitempty"`

	// Parameters for audio output. Required when audio output is requested with
	// `modalities: ["audio"]`.
	// [Learn more](https://platform.openai.com/docs/guides/audio).
	// TODO
	// Audio ChatCompletionAudioParam `json:"audio,omitzero"`

	// Modify the likelihood of specified tokens appearing in the completion.
	//
	// Accepts a JSON object that maps tokens (specified by their token ID in the
	// tokenizer) to an associated bias value from -100 to 100. Mathematically, the
	// bias is added to the logits generated by the model prior to sampling. The exact
	// effect will vary per model, but values between -1 and 1 should decrease or
	// increase likelihood of selection; values like -100 or 100 should result in a ban
	// or exclusive selection of the relevant token.
	LogitBias map[string]int64 `json:"logit_bias,omitempty"`

	// Set of 16 key-value pairs that can be attached to an object. This can be useful
	// for storing additional information about the object in a structured format, and
	// querying for objects via API or the dashboard.
	//
	// Keys are strings with a maximum length of 64 characters. Values are strings with
	// a maximum length of 512 characters.
	Metadata map[string]string `json:"metadata,omitempty"`

	// Output types that you would like the model to generate. Most models are capable
	// of generating text, which is the default:
	//
	// `["text"]`
	// To generate audio, you can use:
	// `["text", "audio"]`
	// To generate image, you can use:
	// `["text", "image"]`
	// Please note that not all models support audio and image generation.
	// Any of "text", "audio", "image".
	Modalities []string `json:"modalities,omitempty"`

	Audio *struct {
		Format string `json:"format,omitempty"`
		Voice  string `json:"voice,omitempty"`
	} `json:"audio,omitempty"`

	// Controls effort on reasoning for reasoning models. It can be set to "low", "medium", or "high".
	ReasoningEffort string `json:"reasoning_effort,omitempty"`

	// ReasoningSummary carries the client's reasoning.summary preference (e.g. "auto",
	// "concise", "detailed"). A genuine codex CLI always sends reasoning.summary="auto"
	// alongside the effort; that field is what makes a Responses upstream emit
	// response.reasoning_summary_text.delta events *during* a long reasoning turn. Without
	// it the upstream reasons silently and a long max-effort turn streams nothing to the
	// client until the final message (perceived as a hang). Help field: only the Responses
	// outbound projects it back onto reasoning.summary; other outbounds ignore it.
	ReasoningSummary string `json:"-"`

	// Reasoning budget for reasoning models.
	// Help fields， will not be sent to the llm service.
	ReasoningBudget *int64 `json:"-"`

	// AdaptiveThinking indicates the client requested adaptive thinking mode.
	// Help field, will not be sent to the llm service.
	AdaptiveThinking bool `json:"-"`

	// EnableThinking is used by Alibaba Qwen models to enable thinking/reasoning output.
	EnableThinking *bool `json:"enable_thinking,omitempty"`

	// Thinking is used by DeepSeek to control thinking mode.
	// e.g., {"type": "disabled"} or {"type": "enabled", "budget_tokens": 1024}
	Thinking json.RawMessage `json:"thinking,omitempty"`

	// ChatTemplateKwargs is used by Nvidia to pass chat template kwargs.
	// e.g., {"enable_thinking": true}
	ChatTemplateKwargs json.RawMessage `json:"chat_template_kwargs,omitempty"`

	// Specifies the processing type used for serving the request.
	ServiceTier *string `json:"service_tier,omitempty"`

	// Not supported with latest reasoning models `o3` and `o4-mini`.
	//
	// Up to 4 sequences where the API will stop generating further tokens. The
	// returned text will not contain the stop sequence.
	Stop *Stop `json:"stop,omitempty"` // string or []string

	Stream        *bool          `json:"stream,omitempty"`
	StreamOptions *StreamOptions `json:"stream_options,omitempty"`

	// Static predicted output content, such as the content of a text file that is
	// being regenerated.
	// TODO
	// Prediction ChatCompletionPredictionContentParam `json:"prediction,omitempty"`

	// Whether to enable
	// [parallel function calling](https://platform.openai.com/docs/guides/function-calling#configuring-parallel-function-calling)
	// during tool use.
	ParallelToolCalls *bool       `json:"parallel_tool_calls,omitempty"`
	Tools             []Tool      `json:"tools,omitempty"`
	ToolChoice        *ToolChoice `json:"tool_choice,omitempty"`

	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`

	// Help fields， will not be sent to the llm service.

	// ExtraBody is helpful to extend the request for different providers.
	// It will not be sent to the OpenAI server.
	ExtraBody json.RawMessage `json:"extra_body,omitempty"`

	// RawRequest is the raw request from the client.
	RawRequest []byte `json:"-"`

	// RawAPIFormat is the original format of the request.
	// e.g. the request from the chat/completions endpoint is in the openai/chat_completion format.
	RawAPIFormat APIFormat `json:"-"`

	// TransformerMetadata stores transformer-specific metadata for preserving format during transformations.
	// This is a help field and will not be sent to the llm service.
	TransformerMetadata map[string]string `json:"-"`

	// TransformOptions stores transformer-specific options for preserving request format.
	// This is a help field and will not be sent to the llm service.
	TransformOptions TransformOptions `json:"-"`

	// Include specifies additional output data to include in the model response.
	// This is a help field and will not be sent to the llm service.
	// e.g., "file_search_call.results", "message.input_image.image_url", "reasoning.encrypted_content"
	Include []string `json:"-"`

	// ClientMetadata preserves OpenAI Responses client_metadata. Codex clients use
	// it for installation/window/turn identity, so same-protocol Responses relays
	// must not drop it.
	ClientMetadata json.RawMessage `json:"-"`

	// AnthropicContextManagement preserves Claude Code's native
	// context_management object. Claude Code uses it for long conversations and
	// automatic compaction; dropping it makes relayed requests diverge from the
	// real client shape even when the endpoint/model are correct.
	AnthropicContextManagement json.RawMessage `json:"-"`

	// AnthropicThinking preserves the native Anthropic thinking object for
	// same-protocol Claude/Claude Code relays. In particular, Claude Code sends
	// {"type":"disabled"} for its structured title request; treating the missing
	// internal reasoning effort as "please synthesize adaptive thinking" mutates
	// the request shape and can make upstream routers choose the wrong path.
	AnthropicThinking json.RawMessage `json:"-"`

	// AnthropicOutputConfig preserves Claude Code's native output_config object,
	// including structured-output format schemas used by the title request.
	AnthropicOutputConfig json.RawMessage `json:"-"`

	// AnthropicToolsPresent records that the inbound Anthropic request contained
	// a tools field even when it was an empty array. Claude Code includes
	// "tools":[] in no-tool turns, and some routers key client-shape detection on
	// that exact presence rather than only on semantic content.
	AnthropicToolsPresent bool `json:"-"`

	// ResponsesRawRequestFields preserve same-protocol OpenAI Responses request
	// shapes that cannot be represented losslessly by the chat-like internal model.
	ResponsesInstructions  *string           `json:"-"`
	ResponsesInputRaw      json.RawMessage   `json:"-"`
	ResponsesToolsRaw      []json.RawMessage `json:"-"`
	ResponsesToolChoiceRaw json.RawMessage   `json:"-"`
	ResponsesTextRaw       json.RawMessage   `json:"-"`

	// Query stores the original query parameters from the inbound request.
	// This is a help field and will not be sent to the llm service.
	Query url.Values `json:"-"`
}

func (r *InternalLLMRequest) Validate() error {
	if r.Model == "" {
		return errors.New("model is required")
	}

	// 检查是否是 embedding 请求
	isEmbeddingRequest := r.EmbeddingInput != nil
	isChatRequest := len(r.Messages) > 0

	if isEmbeddingRequest && isChatRequest {
		return errors.New("cannot specify both messages and input")
	}

	if !isEmbeddingRequest && !isChatRequest {
		return errors.New("either messages or input is required")
	}

	// 验证 embedding 请求
	if isEmbeddingRequest {
		if r.EmbeddingInput.Single == nil && len(r.EmbeddingInput.Multiple) == 0 && len(r.EmbeddingInput.Raw) == 0 {
			return errors.New("input cannot be empty")
		}
	}

	// 验证 chat 请求
	if isChatRequest && len(r.Messages) == 0 {
		return errors.New("messages are required")
	}

	if isChatRequest {
		r.fillMissingToolCallIDsFromToolMessages()
		// r.fillMissingToolCallIDs()
	}

	return nil
}

func (r *InternalLLMRequest) fillMissingToolCallIDs() {
	usedIDs := make(map[string]struct{})
	for _, msg := range r.Messages {
		for _, tc := range msg.ToolCalls {
			if tc.ID == "" {
				continue
			}
			usedIDs[tc.ID] = struct{}{}
		}
	}

	sequence := 0
	for messageIndex := range r.Messages {
		for toolCallIndex := range r.Messages[messageIndex].ToolCalls {
			toolCall := &r.Messages[messageIndex].ToolCalls[toolCallIndex]
			if toolCall.ID != "" {
				continue
			}

			candidate := fmt.Sprintf("call_octopus_%d_%d", messageIndex, toolCallIndex)
			if _, exists := usedIDs[candidate]; exists {
				for {
					candidate = fmt.Sprintf("call_octopus_%d", sequence)
					sequence++
					if _, conflict := usedIDs[candidate]; !conflict {
						break
					}
				}
			}

			toolCall.ID = candidate
			usedIDs[candidate] = struct{}{}
		}
	}
}

func (r *InternalLLMRequest) fillMissingToolCallIDsFromToolMessages() {
	for msgIndex := 0; msgIndex < len(r.Messages); msgIndex++ {
		msg := &r.Messages[msgIndex]
		if msg.Role != "assistant" || len(msg.ToolCalls) == 0 {
			continue
		}

		candidates := make([]string, 0, len(msg.ToolCalls))
		for nextIndex := msgIndex + 1; nextIndex < len(r.Messages); nextIndex++ {
			nextMsg := r.Messages[nextIndex]
			if nextMsg.Role != "tool" {
				break
			}
			if nextMsg.ToolCallID == nil || *nextMsg.ToolCallID == "" {
				continue
			}
			candidates = append(candidates, *nextMsg.ToolCallID)
		}

		if len(candidates) == 0 {
			continue
		}

		used := make(map[string]struct{})
		for _, toolCall := range msg.ToolCalls {
			if toolCall.ID == "" {
				continue
			}
			used[toolCall.ID] = struct{}{}
		}

		candidateIndex := 0
		for toolCallIndex := range msg.ToolCalls {
			if msg.ToolCalls[toolCallIndex].ID != "" {
				continue
			}

			for candidateIndex < len(candidates) {
				candidate := candidates[candidateIndex]
				candidateIndex++
				if _, exists := used[candidate]; exists {
					continue
				}
				msg.ToolCalls[toolCallIndex].ID = candidate
				used[candidate] = struct{}{}
				break
			}
		}
	}
}

// IsEmbeddingRequest returns true if this is an embedding request.
func (r *InternalLLMRequest) IsEmbeddingRequest() bool {
	return r.EmbeddingInput != nil
}

// IsChatRequest returns true if this is a chat completion request.
func (r *InternalLLMRequest) IsChatRequest() bool {
	return len(r.Messages) > 0
}

func (r *InternalLLMRequest) ClearHelpFields() {
	for i, msg := range r.Messages {
		msg.ClearHelpFields()
		r.Messages[i] = msg
	}

	r.ExtraBody = nil
	r.Include = nil
	r.PreviousResponseID = nil
	r.ClientMetadata = nil
	r.AnthropicContextManagement = nil
	r.AnthropicThinking = nil
	r.AnthropicOutputConfig = nil
	r.AnthropicToolsPresent = false
	r.ResponsesInstructions = nil
	r.ResponsesInputRaw = nil
	r.ResponsesToolsRaw = nil
	r.ResponsesToolChoiceRaw = nil
	r.ResponsesTextRaw = nil
}

func (r *InternalLLMRequest) IsImageGenerationRequest() bool {
	if r == nil {
		return false
	}
	if isImageGenerationModelName(r.Model) {
		return true
	}
	for _, modality := range r.Modalities {
		if strings.EqualFold(strings.TrimSpace(modality), "image") {
			return true
		}
	}
	for _, tool := range r.Tools {
		if tool.ImageGeneration != nil || strings.EqualFold(strings.TrimSpace(tool.Type), "image_generation") {
			return true
		}
	}
	return false
}

func isImageGenerationModelName(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return false
	}
	return strings.Contains(name, "gpt-image") ||
		strings.Contains(name, "gpt-4o-image") ||
		strings.Contains(name, "chatgpt-image") ||
		strings.Contains(name, "image2") ||
		strings.Contains(name, "image-2") ||
		strings.HasPrefix(name, "dall-e") ||
		strings.Contains(name, "/dall-e") ||
		IsImagenModel(name) ||
		strings.HasPrefix(name, "flux-") ||
		strings.Contains(name, "/flux-") ||
		strings.HasPrefix(name, "flux.1-") ||
		strings.Contains(name, "/flux.1-") ||
		strings.Contains(name, "qwen-image") ||
		strings.Contains(name, "seedream") ||
		isGeminiImageGenerationModelName(name) ||
		isGrokImageGenerationModelName(name)
}

func isGeminiImageGenerationModelName(name string) bool {
	return strings.Contains(name, "gemini-") &&
		(strings.Contains(name, "-image") || strings.Contains(name, "image-generation"))
}

func isGrokImageGenerationModelName(name string) bool {
	return strings.Contains(name, "grok-imagine-image") ||
		strings.Contains(name, "grok-2-image")
}

// IsImagenModel reports whether the model name refers to a Google Imagen model
// (served via the Gemini :predict endpoint), as opposed to a Gemini image model
// (served via :generateContent). This is the single source of truth shared by
// the image-generation request detection here and the Gemini images bridge in
// the relay package, so Imagen vs Gemini-image routing stays consistent.
//
// It matches both the canonical hyphenated form (e.g. "imagen-4.0-generate-001",
// "models/imagen-3.0") and hyphen-less variants some routers expose (e.g.
// "imagen", "imagen4"). A leading path segment such as "models/" or a vendor
// prefix like "google/" is tolerated.
func IsImagenModel(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return false
	}
	// Strip a leading path/vendor prefix so "models/imagen-3.0" and
	// "google/imagen-4" are detected the same as a bare "imagen-4".
	if idx := strings.LastIndex(name, "/"); idx >= 0 {
		name = name[idx+1:]
	}
	if strings.Contains(name, "imagen-") {
		return true
	}
	return strings.HasPrefix(name, "imagen")
}

type TransformOptions struct {
	// ArrayInputs specifies whether the original input was an array.
	ArrayInputs *bool `json:"-"`

	// AnthropicAutoCacheControl enables conservative automatic cache_control
	// injection for stable Anthropic prompt prefixes.
	AnthropicAutoCacheControl bool `json:"-"`

	// AnthropicOneMillionBeta records that the client requested a Claude 1M
	// compatibility alias such as opus[1m]. Routing can later replace Model with
	// the provider's real model id, so the beta flag must not depend only on the
	// current Model string.
	AnthropicOneMillionBeta bool `json:"-"`

	// AnthropicBetas preserves client-supplied Anthropic beta names that arrived
	// in request bodies. Some proxy-compatible clients put betas in JSON instead
	// of the Anthropic-Beta header; outbound Anthropic requests merge these into
	// the header instead of forwarding a non-standard body field.
	AnthropicBetas []string `json:"-"`

	// SuppressClaudeIdentity, when true, tells the Anthropic outbound transformer
	// NOT to synthesize the Claude CLI billing-header / agent-identity system
	// blocks. The relay sets this when a channel's cloak mode is "never" — e.g. a
	// domestic GLM/DeepSeek Anthropic-compatible upstream that must not receive
	// injected Claude identity. Default false preserves the cloak behaviour, so
	// auto/always channels (and non-relay callers) keep the genuine-CLI shape that
	// the relay requires. A genuine client's own system blocks always pass through.
	SuppressClaudeIdentity bool `json:"-"`
}

type StreamOptions struct {
	// If set, an additional chunk will be streamed before the data: [DONE] message.
	// The usage field on this chunk shows the token usage statistics for the entire request,
	// and the choices field will always be an empty array.
	// All other chunks will also include a usage field, but with a null value.
	IncludeUsage bool `json:"include_usage,omitempty"`
}

type Stop struct {
	Stop         *string
	MultipleStop []string
}

func (s Stop) MarshalJSON() ([]byte, error) {
	if s.Stop != nil {
		return json.Marshal(s.Stop)
	}

	if len(s.MultipleStop) > 0 {
		return json.Marshal(s.MultipleStop)
	}

	return []byte("[]"), nil
}

func (s *Stop) UnmarshalJSON(data []byte) error {
	var str string

	err := json.Unmarshal(data, &str)
	if err == nil {
		s.Stop = &str
		return nil
	}

	var strs []string

	err = json.Unmarshal(data, &strs)
	if err == nil {
		s.MultipleStop = strs
		return nil
	}

	return errors.New("invalid stop type")
}

// Message represents a message in the conversation.
type Message struct {
	Role string `json:"role,omitempty"`
	// Content of the message.
	// string or []ContentPart, be careful about the omitzero tag, it required.
	// Some framework may depended on the behavior, we should not response the field if not present.
	Content MessageContent `json:"content,omitzero"`
	Name    *string        `json:"name,omitempty"`

	// The refusal message generated by the model.
	Refusal string `json:"refusal,omitempty"`

	// For tool call response.
	// Deprecated OpenAI Chat function_call field. Inbound transformers normalize it
	// into ToolCalls so the rest of the relay can use one tool-call representation.
	FunctionCall *FunctionCall `json:"function_call,omitempty"`

	// The index of the message that the tool call is associated with.
	// Is is a help field, will not be sent to the llm service.
	MessageIndex *int    `json:"-"`
	ToolCallID   *string `json:"tool_call_id,omitempty"`
	// The name of the tool call.
	// Is is a help field, will not be sent to the llm service.
	ToolCallName *string `json:"-"`
	// This field is a help field, will not be sent to the llm service.
	ToolCallIsError *bool      `json:"-"`
	ToolCalls       []ToolCall `json:"tool_calls,omitempty"`

	// Images is used by some providers (e.g., Gemini via OpenAI compat) for image generation responses.
	// Images will be merged into Content.MultipleContent during response processing.
	Images []MessageContentPart `json:"images,omitempty"`

	Audio *struct {
		Data       string `json:"data,omitempty"`
		ExpiresAt  int64  `json:"expires_at,omitempty"`
		ID         string `json:"id,omitempty"`
		Transcript string `json:"transcript,omitempty"`
	} `json:"audio,omitempty"`

	// This property is used for the "reasoning" feature supported by deepseek-reasoner
	// the doc from deepseek:
	// - https://api-docs.deepseek.com/api/create-chat-completion#responses
	ReasoningContent *string `json:"reasoning_content,omitempty"`

	// Reasoning is used by some providers (e.g., OpenRouter, Ollama cloud) as an alternative to ReasoningContent.
	// Both fields serve the same purpose, use GetReasoningContent() to get the value.
	Reasoning *string `json:"reasoning,omitempty"`

	// Help field, will not be sent to the llm service, to adapt the anthropic think signature.
	ReasoningSignature *string `json:"reasoning_signature,omitempty"`

	// CacheControl is used for provider-specific cache control (e.g., Anthropic).
	// This field is not serialized in JSON.
	CacheControl *CacheControl `json:"-"`
}

func (m *Message) ClearHelpFields() {
	m.ReasoningContent = nil
	m.Reasoning = nil
	m.ReasoningSignature = nil
}

// GetReasoningContent returns the reasoning content from either ReasoningContent or Reasoning field.
// Different providers use different field names for the same purpose.
func (m *Message) GetReasoningContent() string {
	if m.ReasoningContent != nil {
		return *m.ReasoningContent
	}
	if m.Reasoning != nil {
		return *m.Reasoning
	}
	return ""
}

// SetReasoningContent sets the reasoning content to the ReasoningContent field.
func (m *Message) SetReasoningContent(s string) {
	m.ReasoningContent = &s
}

type MessageContent struct {
	Content         *string              `json:"content,omitempty"`
	MultipleContent []MessageContentPart `json:"multiple_content,omitempty"`
}

func (c MessageContent) MarshalJSON() ([]byte, error) {
	if len(c.MultipleContent) > 0 {
		if len(c.MultipleContent) == 1 && c.MultipleContent[0].Type == "text" {
			return json.Marshal(c.MultipleContent[0].Text)
		}

		return json.Marshal(c.MultipleContent)
	}

	return json.Marshal(c.Content)
}

func (c *MessageContent) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return nil
	}

	var str string

	err := json.Unmarshal(data, &str)
	if err == nil {
		c.Content = &str
		return nil
	}

	var parts []MessageContentPart

	err = json.Unmarshal(data, &parts)
	if err == nil {
		c.MultipleContent = parts
		return nil
	}

	return errors.New("invalid content type")
}

// MessageContentPart represents different types of content (text, image, etc.)
type MessageContentPart struct {
	// Type is the type of the content part.
	// e.g. "text", "image_url"
	Type string `json:"type"`
	// Text is the text content, required when type is "text"
	Text *string `json:"text,omitempty"`

	// ImageURL is the image URL content, required when type is "image_url"
	ImageURL *ImageURL `json:"image_url,omitempty"`

	// Audio is the audio content, required when type is "input_audio"
	Audio *Audio `json:"input_audio,omitempty"`

	// File is the file content, required when type is "file"
	File *File `json:"file,omitempty"`

	// CacheControl is used for provider-specific cache control (e.g., Anthropic).
	// This field is not serialized in JSON.
	CacheControl *CacheControl `json:"-"`
}

// ImageURL represents an image URL with optional detail level.
type ImageURL struct {
	// URL is the URL of the image.
	URL string `json:"url"`

	// Specifies the detail level of the image. Learn more in the
	// [Vision guide](https://platform.openai.com/docs/guides/vision#low-or-high-fidelity-image-understanding).
	//
	// Any of "auto", "low", "high".
	Detail *string `json:"detail,omitempty"`
}

type Audio struct {
	// The format of the encoded audio data. Currently supports "wav" and "mp3".
	//
	// Any of "wav", "mp3".
	Format string `json:"format"`

	// Base64 encoded audio data.
	Data string `json:"data"`
}

type File struct {
	// The filename of the file.
	Filename string `json:"filename"`
	// The base64 encoded data of the file.
	// For OpenAI Chat this is a data URL (e.g. "data:application/pdf;base64,...").
	// Document carriers (Anthropic document / Codex Responses input_file) also
	// store base64 documents here as a data URL so a single field round-trips
	// across providers.
	FileData string `json:"file_data"`

	// MediaType is the document MIME type (e.g. "application/pdf"). It is set when
	// the source carried an explicit media type that is not already encoded in
	// FileData's data URL. Not part of the OpenAI Chat schema, so it is omitted
	// when empty to avoid changing existing passthrough output.
	MediaType string `json:"media_type,omitempty"`

	// FileURL holds a remote document URL when the source referenced the document
	// by URL instead of inline base64 data.
	FileURL string `json:"file_url,omitempty"`

	// FileID holds a provider-side file identifier (e.g. Anthropic Files API or
	// OpenAI uploaded file id) when the source referenced the document by id.
	FileID string `json:"file_id,omitempty"`
}

// ResponseFormat specifies the format of the response.
type ResponseFormat struct {
	// Any of "json_schema", "json_object", "text".
	Type string `json:"type"`
	// TODO: Schema
	JSONSchema json.RawMessage `json:"json_schema,omitempty"`
}

// Response is the unified response model.
// To reduce the work of converting the response, we use the OpenAI response format.
// And other llm provider should convert the response to this format.
// NOTE: the OpenAI stream and non-stream response reuse same struct.
type InternalLLMResponse struct {
	ID string `json:"id"`

	// A list of chat completion choices. Can be more than one if `n` is greater
	// than 1.
	// For chat completion responses, this field is required.
	// For embedding responses, this field should be empty and EmbeddingData should be used instead.
	Choices []Choice `json:"choices,omitempty"`

	// Embedding API 响应（与 Choices 互斥）
	// EmbeddingData is the list of embedding objects.
	// For embedding responses, this field is required.
	// For chat completion responses, this field should be empty.
	EmbeddingData []EmbeddingObject `json:"embedding_data,omitempty"`

	// Object is the type of the response.
	// e.g. "chat.completion", "chat.completion.chunk", "list"
	Object string `json:"object"`

	// Created is the timestamp of when the response was created.
	Created int64 `json:"created"`

	// Model is the model used to generate the response.
	Model string `json:"model"`

	// An optional field that will only be present when you set stream_options: {"include_usage": true} in your request.
	// When present, it contains a null value except for the last chunk which contains the token usage statistics
	// for the entire request.
	Usage *Usage `json:"usage,omitempty"`

	// This fingerprint represents the backend configuration that the model runs with.
	//
	// Can be used in conjunction with the `seed` request parameter to understand when
	// backend changes have been made that might impact determinism.
	SystemFingerprint string `json:"system_fingerprint,omitempty"`

	// ServiceTier is the service tier of the response.
	// e.g. "free", "standard", "premium"
	ServiceTier string `json:"service_tier,omitempty"`

	// Error is the error information, will present if request to llm service failed with status >= 400.
	Error *ResponseError `json:"error,omitempty"`
}

func (r *InternalLLMResponse) ClearHelpFields() {
	for i, choice := range r.Choices {
		if choice.Message != nil {
			choice.Message.ClearHelpFields()
		}

		if choice.Delta != nil {
			choice.Delta.ClearHelpFields()
		}

		r.Choices[i] = choice
	}
}

// IsEmbeddingResponse returns true if this is an embedding response.
func (r *InternalLLMResponse) IsEmbeddingResponse() bool {
	return len(r.EmbeddingData) > 0
}

// IsChatResponse returns true if this is a chat completion response.
func (r *InternalLLMResponse) IsChatResponse() bool {
	return len(r.Choices) > 0
}

// Choice represents a choice in the response.
// Choice represents a choice in the response.
type Choice struct {
	// Index is the index of the choice in the list of choices.
	Index int `json:"index"`

	// Message is the message content, will present if stream is false
	Message *Message `json:"message,omitempty"`

	// Delta is the stream event content, will present if stream is true
	Delta *Message `json:"delta,omitempty"`

	// FinishReason is the reason the model stopped generating tokens.
	// e.g. "stop", "length", "content_filter", "function_call", "tool_calls"
	FinishReason *string `json:"finish_reason,omitempty"`

	Logprobs *LogprobsContent `json:"logprobs,omitempty"`
}

// LogprobsContent represents logprobs information.
type LogprobsContent struct {
	Content []TokenLogprob `json:"content"`
}

// TokenLogprob represents logprob for a token.
type TokenLogprob struct {
	Token       string       `json:"token"`
	Logprob     float64      `json:"logprob"`
	Bytes       []int        `json:"bytes,omitempty"`
	TopLogprobs []TopLogprob `json:"top_logprobs,omitempty"`
}

// TopLogprob represents top alternative tokens.
type TopLogprob struct {
	Token   string  `json:"token"`
	Logprob float64 `json:"logprob"`
	Bytes   []int   `json:"bytes,omitempty"`
}

type ResponseMeta struct {
	ID    string `json:"id"`
	Usage *Usage `json:"usage"`
}

// Usage Represents the total token usage per request to OpenAI.
// For embedding requests, CompletionTokens is always 0.
type Usage struct {
	PromptTokens            int64                    `json:"prompt_tokens"`
	CompletionTokens        int64                    `json:"completion_tokens"`
	TotalTokens             int64                    `json:"total_tokens"`
	PromptTokensDetails     *PromptTokensDetails     `json:"prompt_tokens_details"`
	CompletionTokensDetails *CompletionTokensDetails `json:"completion_tokens_details"`

	// Output only. A detailed breakdown of the token count for each modality in the prompt.
	PromptModalityTokenDetails []ModalityTokenCount `json:"-"`
	// Output only. A detailed breakdown of the token count for each modality in the candidates.
	CompletionModalityTokenDetails []ModalityTokenCount `json:"-"`
	// Provider-native cache fields kept for metrics and billing.
	AnthropicUsage           bool  `json:"-"`
	CacheCreationInputTokens int64 `json:"-"`
	// CacheCreation5m/1hInputTokens split the cache-creation total by TTL bucket, from
	// Anthropic's nested usage.cache_creation{ephemeral_5m/1h_input_tokens}. Anthropic
	// prices 5m vs 1h cache writes differently, so keeping the split lets logs/billing
	// reflect that instead of lumping both into one figure. Zero when the upstream did
	// not report the breakdown.
	CacheCreation5mInputTokens int64 `json:"-"`
	CacheCreation1hInputTokens int64 `json:"-"`
	// Some OpenAI-compatible proxies expose Anthropic-style cache fields where
	// input_tokens excludes cache read/write tokens.
	SeparateCacheInputTokens bool `json:"-"`
}

func (u *Usage) UnmarshalJSON(data []byte) error {
	type usageAlias Usage
	var aux struct {
		usageAlias
		InputTokens              *int64                   `json:"input_tokens"`
		OutputTokens             *int64                   `json:"output_tokens"`
		InputTokensDetails       *PromptTokensDetails     `json:"input_tokens_details"`
		OutputTokensDetails      *CompletionTokensDetails `json:"output_tokens_details"`
		CachedTokens             *int64                   `json:"cached_tokens"`
		PromptCacheHitTokens     *int64                   `json:"prompt_cache_hit_tokens"`
		CacheCreationInputTokens int64                    `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int64                    `json:"cache_read_input_tokens"`
		CacheCreation            *struct {
			Ephemeral5m int64 `json:"ephemeral_5m_input_tokens"`
			Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
		} `json:"cache_creation"`
	}

	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*u = Usage(aux.usageAlias)

	if u.PromptTokens == 0 && aux.InputTokens != nil {
		u.PromptTokens = *aux.InputTokens
	}
	if u.CompletionTokens == 0 && aux.OutputTokens != nil {
		u.CompletionTokens = *aux.OutputTokens
	}
	if u.TotalTokens == 0 && (u.PromptTokens > 0 || u.CompletionTokens > 0) {
		u.TotalTokens = u.PromptTokens + u.CompletionTokens
	}
	if aux.CacheCreationInputTokens > 0 {
		u.CacheCreationInputTokens = aux.CacheCreationInputTokens
		u.SeparateCacheInputTokens = true
	}
	if aux.CacheCreation != nil {
		u.CacheCreation5mInputTokens = aux.CacheCreation.Ephemeral5m
		u.CacheCreation1hInputTokens = aux.CacheCreation.Ephemeral1h
		// If the upstream only sent the per-TTL split (no flat total), derive the total
		// so downstream metrics/billing stay consistent.
		if u.CacheCreationInputTokens == 0 {
			if total := aux.CacheCreation.Ephemeral5m + aux.CacheCreation.Ephemeral1h; total > 0 {
				u.CacheCreationInputTokens = total
				u.SeparateCacheInputTokens = true
			}
		}
	}
	if aux.CacheReadInputTokens > 0 {
		u.SeparateCacheInputTokens = true
	}
	if u.PromptTokensDetails == nil {
		u.PromptTokensDetails = aux.InputTokensDetails
	} else if aux.InputTokensDetails != nil && u.PromptTokensDetails.CachedTokens == 0 {
		u.PromptTokensDetails.CachedTokens = aux.InputTokensDetails.CachedTokens
	}
	if u.PromptTokensDetails == nil && aux.CacheReadInputTokens > 0 {
		u.PromptTokensDetails = &PromptTokensDetails{}
	}
	if u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens == 0 && aux.CacheReadInputTokens > 0 {
		u.PromptTokensDetails.CachedTokens = aux.CacheReadInputTokens
	}
	if u.PromptTokensDetails == nil && (positiveInt64Ptr(aux.CachedTokens) > 0 || positiveInt64Ptr(aux.PromptCacheHitTokens) > 0) {
		u.PromptTokensDetails = &PromptTokensDetails{}
	}
	if u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens == 0 {
		switch {
		case positiveInt64Ptr(aux.CachedTokens) > 0:
			u.PromptTokensDetails.CachedTokens = *aux.CachedTokens
		case positiveInt64Ptr(aux.PromptCacheHitTokens) > 0:
			u.PromptTokensDetails.CachedTokens = *aux.PromptCacheHitTokens
		}
	}
	if u.CompletionTokensDetails == nil {
		u.CompletionTokensDetails = aux.OutputTokensDetails
	}
	return nil
}

func positiveInt64Ptr(v *int64) int64 {
	if v == nil || *v <= 0 {
		return 0
	}
	return *v
}

func (u *Usage) GetCompletionTokens() *int64 {
	if u == nil {
		return nil
	}

	return &u.CompletionTokens
}

func (u *Usage) GetPromptTokens() *int64 {
	if u == nil {
		return nil
	}

	return &u.PromptTokens
}

// CompletionTokensDetails Breakdown of tokens used in a completion.
type CompletionTokensDetails struct {
	AudioTokens              int64 `json:"audio_tokens"`
	ReasoningTokens          int64 `json:"reasoning_tokens"`
	AcceptedPredictionTokens int64 `json:"accepted_prediction_tokens"`
	RejectedPredictionTokens int64 `json:"rejected_prediction_tokens"`
}

// PromptTokensDetails Breakdown of tokens used in the prompt.
type PromptTokensDetails struct {
	AudioTokens  int64 `json:"audio_tokens"`
	CachedTokens int64 `json:"cached_tokens"`
}

// ResponseError represents an error response.
type ResponseError struct {
	StatusCode int         `json:"-"`
	Detail     ErrorDetail `json:"error"`
}

func (e ResponseError) Error() string {
	sb := strings.Builder{}
	if e.StatusCode != 0 {
		sb.WriteString(fmt.Sprintf("Request failed: %s, ", http.StatusText(e.StatusCode)))
	}

	if e.Detail.Message != "" {
		sb.WriteString("error: ")
		sb.WriteString(e.Detail.Message)
	}

	if e.Detail.Code != "" {
		sb.WriteString(", code: ")
		sb.WriteString(e.Detail.Code)
	}

	if e.Detail.Type != "" {
		sb.WriteString(", type: ")
		sb.WriteString(e.Detail.Type)
	}

	if e.Detail.RequestID != "" {
		sb.WriteString(", request_id: ")
		sb.WriteString(e.Detail.RequestID)
	}

	return sb.String()
}

// ErrorDetail represents error details.
type ErrorDetail struct {
	Code      string `json:"code,omitempty"`
	Message   string `json:"message"`
	Type      string `json:"type"`
	Param     string `json:"param,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

// ModalityTokenCount Represents token counting info for a single modality.
type ModalityTokenCount struct {
	Modality string `json:"modality,omitempty"`
	// Number of tokens.
	TokenCount int64 `json:"token_count,omitempty"`
}

// ToolTypeAnthropicBuiltin marks an internal Tool that carries an Anthropic
// built-in tool (computer-use, bash, text_editor, code_execution, ...) verbatim
// in RawTool. Outbound transformers that don't understand these tools (openai,
// gemini) match on Type=="function" and therefore skip it safely; the Anthropic
// outbound transformer restores RawTool unchanged.
const ToolTypeAnthropicBuiltin = "anthropic_builtin"

// Tool represents a function tool.
type Tool struct {
	// Type is the type of the tool.
	// Any of "function", "image_generation", "anthropic_builtin".
	Type            string           `json:"type"`
	Function        Function         `json:"function"`
	ImageGeneration *ImageGeneration `json:"image_generation,omitempty"`

	// RawTool holds the original provider tool object for tools that cannot be
	// represented by the function shape — currently Anthropic built-in tools, whose
	// proprietary fields (display_width_px/display_number/etc.) must round-trip
	// untouched. Present only when Type == ToolTypeAnthropicBuiltin. Not serialized
	// in the standard llm JSON format.
	RawTool json.RawMessage `json:"-"`

	// CacheControl is used for provider-specific cache control (e.g., Anthropic).
	// This field is not serialized in JSON.
	CacheControl *CacheControl `json:"-"`
}

// CacheControl represents cache control configuration.
// This field is used internally for provider-specific cache control
// and should not be serialized in the standard llm JSON format.
type CacheControl struct {
	Type string `json:"-"`
	TTL  string `json:"-"`
}

type toolJSONMarshaller Tool

func (t Tool) MarshalJSON() ([]byte, error) {
	// TODO: find a better way to save the image generation tool to the request body.
	m := toolJSONMarshaller(t)
	// ImageGeneration is not a valid field for chat completion, so we should remove it from the request.
	m.ImageGeneration = nil

	return json.Marshal(m)
}

// Function represents a function definition.
type Function struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// omitempty so a parameterless tool serializes without a key instead of
	// "parameters":null, which strict OpenAI-compatible upstreams reject. A tool
	// that carries a real schema is unaffected (non-empty RawMessage is kept).
	Parameters json.RawMessage `json:"parameters,omitempty"`
	Strict     *bool           `json:"strict,omitempty"`
}

// FunctionCall represents a function call (deprecated).
type FunctionCall struct {
	// The name of the function to call.
	Name string `json:"name"`

	// The arguments to call the function with, as generated by the model in JSON
	// format. Note that the model does not always generate valid JSON, and may
	// hallucinate parameters not defined by your function schema. Validate the
	// arguments in your code before calling your function.
	Arguments string `json:"arguments"`
}

// ToolCallTypeCustom marks an internal tool call that originated as an OpenAI
// Responses "custom tool call" (a freeform-grammar tool). Unlike a normal
// function call, its Function.Arguments holds the tool's freeform `input` text
// (e.g. code), NOT a JSON arguments object, so downstream Responses transformers
// must re-emit it as a custom_tool_call item (carrying `input`) rather than a
// function_call. Normal function calls keep Type "function", so this marker is
// the single signal that lets the outbound→internal→inbound round-trip preserve
// the custom-tool-call nature.
const ToolCallTypeCustom = "custom"

// ToolCall represents a tool call in the response.
type ToolCall struct {
	ID string `json:"id,omitempty"`

	// The type of the tool. Usually `function`; `custom` (ToolCallTypeCustom)
	// marks an OpenAI Responses custom (freeform) tool call.
	Type string `json:"type,omitempty"`

	Function FunctionCall `json:"function"`

	// Index is the index of the tool call in the list of tool calls.
	// Cannot use omitempty, as an index of 0 would be omitted, which can break consumers.
	Index int `json:"index"`

	// CacheControl is used for provider-specific cache control (e.g., Anthropic).
	CacheControl *CacheControl `json:"-"`
}

type ToolFunction struct {
	Name string `json:"name"`
}

// ToolChoice represents the tool choice parameter for function calling.
//
// Tool choice can be a string or a struct.
type ToolChoice struct {
	ToolChoice      *string          `json:"tool_choice,omitempty"`
	NamedToolChoice *NamedToolChoice `json:"named_tool_choice,omitempty"`
}

type NamedToolChoice struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

func (t ToolChoice) MarshalJSON() ([]byte, error) {
	if t.ToolChoice != nil {
		return json.Marshal(t.ToolChoice)
	}

	return json.Marshal(t.NamedToolChoice)
}

func (t *ToolChoice) UnmarshalJSON(data []byte) error {
	var str string

	err := json.Unmarshal(data, &str)
	if err == nil {
		t.ToolChoice = &str
		return nil
	}

	var named NamedToolChoice

	err = json.Unmarshal(data, &named)
	if err == nil {
		t.NamedToolChoice = &named
		return nil
	}

	return errors.New("invalid tool choice type")
}

// ImageGeneration is a permissive structure to carry image generation tool
// parameters. It mirrors the OpenRouter/OpenAI Responses API fields we care
// about, but is intentionally loose to allow forward-compatibility.
type ImageGeneration struct {
	// One of opaque, transparent.
	Background     string         `json:"background,omitempty"`
	InputFidelity  string         `json:"input_fidelity,omitempty"`
	InputImageMask map[string]any `json:"input_image_mask,omitempty"`
	// One of low, auto.
	Moderation string `json:"moderation,omitempty"`
	// The compression level (0-100%) for the generated images. Default: 100.
	OutputCompression *int64 `json:"output_compression,omitempty"`
	// One of png, webp, or jpeg. Default: png.
	OutputFormat string `json:"output_format,omitempty"`
	// The number of images to generate. Default: 1.
	PartialImages *int64 `json:"partial_images,omitempty"`
	// The quality of the image that will be generated.
	// auto (default value) will automatically select the best quality for the given model.
	// high, medium and low are supported for gpt-image-1.
	// hd and standard are supported for dall-e-3.
	// standard is the only option for dall-e-2.
	Quality string `json:"quality,omitempty"`
	// One of 256x256, 512x512, or 1024x1024. Default: 1024x1024.
	Size string `json:"size,omitempty"`

	// Whether to add a watermark to the generated image. Default: false.
	// It only works for the models support watermark, it will be ignored otherwise.
	Watermark bool `json:"watermark,omitempty"`
}

// EmbeddingInput represents the input for embedding requests.
// It can be a single string or an array of strings.
type EmbeddingInput struct {
	Single   *string
	Multiple []string
	// Raw preserves input shapes beyond the two string forms — the OpenAI spec also
	// allows pre-tokenized input (an int array, or an array of int arrays), which
	// tiktoken-based RAG stacks do send. Rejecting them here used to surface as an
	// HTTP 500 to the client; reference gateways (new-api) simply pass unrecognized
	// input through and let the upstream judge. Kept as the original bytes so the
	// outbound rebuild is byte-faithful.
	Raw json.RawMessage
}

func (i EmbeddingInput) MarshalJSON() ([]byte, error) {
	if i.Single != nil {
		return json.Marshal(i.Single)
	}

	if len(i.Multiple) > 0 {
		return json.Marshal(i.Multiple)
	}

	if len(i.Raw) > 0 {
		return i.Raw, nil
	}

	return []byte("null"), nil
}

func (i *EmbeddingInput) UnmarshalJSON(data []byte) error {
	var str string

	err := json.Unmarshal(data, &str)
	if err == nil {
		i.Single = &str
		return nil
	}

	var strs []string

	err = json.Unmarshal(data, &strs)
	if err == nil {
		i.Multiple = strs
		return nil
	}

	// Token arrays ([1,2,3] / [[1,2],[3,4]]) and any future shape: keep the raw
	// bytes and forward verbatim; the upstream is the authority on validity.
	i.Raw = append(json.RawMessage(nil), data...)
	return nil
}

// EmbeddingObject represents a single embedding object in the response.
type EmbeddingObject struct {
	// The object type, always "embedding".
	Object string `json:"object"`
	// The index of this embedding in the list.
	Index int `json:"index"`
	// The embedding vector.
	Embedding Embedding `json:"embedding"`
}

// Embedding represents an embedding vector.
// It can be a float array or a base64-encoded string.
type Embedding struct {
	FloatArray   []float64
	Base64String *string
}

func (e Embedding) MarshalJSON() ([]byte, error) {
	if e.Base64String != nil {
		return json.Marshal(e.Base64String)
	}

	if len(e.FloatArray) > 0 {
		return json.Marshal(e.FloatArray)
	}

	return []byte("[]"), nil
}

func (e *Embedding) UnmarshalJSON(data []byte) error {
	var str string

	err := json.Unmarshal(data, &str)
	if err == nil {
		e.Base64String = &str
		return nil
	}

	var floats []float64

	err = json.Unmarshal(data, &floats)
	if err == nil {
		e.FloatArray = floats
		return nil
	}

	return errors.New("invalid embedding type")
}
