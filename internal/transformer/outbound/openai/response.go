package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/samber/lo"

	"github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/utils/xurl"
)

// ResponseOutbound implements the Outbound interface for OpenAI Responses API.
type ResponseOutbound struct {
	// Stream state tracking
	streamID    string
	streamModel string
	initialized bool

	toolCallIDByOutputIndex          map[int]string
	toolCallNameByOutputIndex        map[int]string
	toolCallNameEmittedByOutputIndex map[int]bool
	toolCallArgsByOutputIndex        map[int]string
	hasToolCallStream                bool
}

func (o *ResponseOutbound) TransformRequest(ctx context.Context, request *model.InternalLLMRequest, baseUrl, key string) (*http.Request, error) {
	if request == nil {
		return nil, fmt.Errorf("request is nil")
	}

	// Convert to Responses API request format
	responsesReq := ConvertToResponsesRequest(request)

	body, err := json.Marshal(responsesReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal responses api request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers
	req.Header.Set("Content-Type", "application/json")
	if responsesReq.Stream != nil && *responsesReq.Stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	// The genuine Codex CLI (Rust/reqwest) sends NO Accept-Encoding header at all, so
	// octopus deliberately sets none here — a byte-for-byte match with real codex. Both
	// upstream transports (stock net/http and the uTLS fhttp2 h2 transport) run with
	// DisableCompression=true, so neither auto-injects "gzip" / "gzip, deflate, br" when
	// the header is absent (that auto-injection would itself be a fingerprint tell). Any
	// upstream Content-Encoding is decompressed by the relay's unwrapResponseEncoding, so
	// omitting the request header never leaves the SSE/JSON reader on compressed bytes.
	req.Header.Set("Authorization", "Bearer "+key)

	targetURL, err := xurl.JoinOpenAIPath(baseUrl, "/v1/responses")
	if err != nil {
		return nil, fmt.Errorf("failed to join openai responses url: %w", err)
	}
	req.URL, err = req.URL.Parse(targetURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse openai responses url: %w", err)
	}
	mergeInboundQuery(req.URL, request.Query)
	req.Method = http.MethodPost

	return req, nil
}

func (o *ResponseOutbound) TransformResponse(ctx context.Context, response *http.Response) (*model.InternalLLMResponse, error) {
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
		var errResp struct {
			Error model.ErrorDetail `json:"error"`
		}
		if err := json.Unmarshal(body, &errResp); err == nil && errResp.Error.Message != "" {
			return nil, &model.ResponseError{
				StatusCode: response.StatusCode,
				Detail:     errResp.Error,
			}
		}
		return nil, fmt.Errorf("HTTP error %d: %s", response.StatusCode, string(body))
	}

	var resp ResponsesResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal responses api response: %w", err)
	}

	// Convert to internal response
	return convertToLLMResponseFromResponses(&resp), nil
}

func (o *ResponseOutbound) TransformStream(ctx context.Context, eventData []byte) (*model.InternalLLMResponse, error) {
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
		o.initialized = true
	}
	o.ensureToolCallState()

	// Parse the streaming event
	var streamEvent ResponsesStreamEvent
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
	case "response.created", "response.in_progress":
		if streamEvent.Response != nil {
			o.streamID = streamEvent.Response.ID
			o.streamModel = streamEvent.Response.Model
			resp.ID = o.streamID
			resp.Model = o.streamModel
		}
		resp.Choices = []model.Choice{
			{
				Index: 0,
				Delta: &model.Message{
					Role: "assistant",
				},
			},
		}

	case "response.output_text.delta":
		resp.Choices = []model.Choice{
			{
				Index: 0,
				Delta: &model.Message{
					Role: "assistant",
					Content: model.MessageContent{
						Content: lo.ToPtr(streamEvent.Delta),
					},
				},
			},
		}

	case "response.function_call_arguments.delta":
		o.hasToolCallStream = true
		callID := o.toolCallIDByOutputIndex[streamEvent.OutputIndex]
		if callID == "" {
			callID = streamEvent.CallID
		}
		if callID == "" && streamEvent.ItemID != nil {
			callID = *streamEvent.ItemID
		}
		name := o.toolCallNameByOutputIndex[streamEvent.OutputIndex]
		if name == "" {
			name = streamEvent.Name
		}
		o.toolCallArgsByOutputIndex[streamEvent.OutputIndex] += streamEvent.Delta

		resp.Choices = []model.Choice{
			{
				Index: 0,
				Delta: &model.Message{
					Role: "assistant",
					ToolCalls: []model.ToolCall{
						{
							Index: streamEvent.OutputIndex,
							ID:    callID,
							Type:  "function",
							Function: model.FunctionCall{
								Name:      o.emitToolCallName(streamEvent.OutputIndex, name),
								Arguments: streamEvent.Delta,
							},
						},
					},
				},
			},
		}

	case "response.function_call_arguments.done":
		// Some upstreams (observed: anyrouter's gpt-5.6 responses proxy, esp. at low
		// reasoning effort) deliver the COMPLETE tool-call arguments in a single
		// function_call_arguments.done event instead of streaming them as
		// function_call_arguments.delta chunks, and leave the following
		// output_item.done's item arguments empty. Without handling this event the
		// arguments were dropped entirely: codex received a function call with empty
		// arguments it could not execute and stalled after its spoken preamble
		// ("I'll check the current directory now." then stop). Emit only the suffix
		// not already streamed so arguments are neither dropped nor duplicated when
		// deltas also arrived.
		if streamEvent.Arguments == "" {
			return nil, nil
		}
		o.hasToolCallStream = true
		callID := o.toolCallIDByOutputIndex[streamEvent.OutputIndex]
		if callID == "" {
			callID = streamEvent.CallID
		}
		if callID == "" && streamEvent.ItemID != nil {
			callID = *streamEvent.ItemID
		}
		previous := o.toolCallArgsByOutputIndex[streamEvent.OutputIndex]
		argsDelta := streamEvent.Arguments
		if strings.HasPrefix(streamEvent.Arguments, previous) {
			argsDelta = streamEvent.Arguments[len(previous):]
		}
		o.toolCallArgsByOutputIndex[streamEvent.OutputIndex] = streamEvent.Arguments
		if argsDelta == "" {
			return nil, nil
		}
		name := o.toolCallNameByOutputIndex[streamEvent.OutputIndex]
		if name == "" {
			name = streamEvent.Name
		}
		resp.Choices = []model.Choice{
			{
				Index: 0,
				Delta: &model.Message{
					Role: "assistant",
					ToolCalls: []model.ToolCall{
						{
							Index: streamEvent.OutputIndex,
							ID:    callID,
							Type:  "function",
							Function: model.FunctionCall{
								Name:      o.emitToolCallName(streamEvent.OutputIndex, name),
								Arguments: argsDelta,
							},
						},
					},
				},
			},
		}

	case "response.output_item.added", "response.output_item.done":
		if streamEvent.Item != nil && isResponsesToolCallItemType(streamEvent.Item.Type) {
			o.hasToolCallStream = true
			callID := streamEvent.Item.CallID
			if callID == "" {
				callID = streamEvent.Item.ID
			}
			if callID != "" {
				o.toolCallIDByOutputIndex[streamEvent.OutputIndex] = callID
			}
			if name := responsesToolCallName(streamEvent.Item); name != "" {
				o.toolCallNameByOutputIndex[streamEvent.OutputIndex] = name
			}
			argsDelta := ""
			if streamEvent.Type == "response.output_item.done" && streamEvent.Item.Arguments != "" {
				previous := o.toolCallArgsByOutputIndex[streamEvent.OutputIndex]
				if strings.HasPrefix(streamEvent.Item.Arguments, previous) {
					argsDelta = streamEvent.Item.Arguments[len(previous):]
				} else {
					argsDelta = streamEvent.Item.Arguments
				}
				o.toolCallArgsByOutputIndex[streamEvent.OutputIndex] = streamEvent.Item.Arguments
			}
			resp.Choices = []model.Choice{
				{
					Index: 0,
					Delta: &model.Message{
						Role: "assistant",
						ToolCalls: []model.ToolCall{
							{
								Index: streamEvent.OutputIndex,
								ID:    callID,
								Type:  "function",
								Function: model.FunctionCall{
									Name:      o.emitToolCallName(streamEvent.OutputIndex, o.toolCallNameByOutputIndex[streamEvent.OutputIndex]),
									Arguments: argsDelta,
								},
							},
						},
					},
				},
			}
		} else {
			return nil, nil
		}

	case "response.reasoning_summary_text.delta":
		resp.Choices = []model.Choice{
			{
				Index: 0,
				Delta: &model.Message{
					Role:             "assistant",
					ReasoningContent: lo.ToPtr(streamEvent.Delta),
				},
			},
		}

	case "response.completed":
		var usage *ResponsesUsage
		if streamEvent.Response != nil {
			if streamEvent.Response.ID != "" {
				o.streamID = streamEvent.Response.ID
				resp.ID = o.streamID
			}
			if streamEvent.Response.Model != "" {
				o.streamModel = streamEvent.Response.Model
				resp.Model = o.streamModel
			}
			var finishReason *string
			if streamEvent.Response.Status != nil {
				switch *streamEvent.Response.Status {
				case "completed":
					if o.hasToolCallStream {
						finishReason = lo.ToPtr("tool_calls")
					} else {
						finishReason = lo.ToPtr("stop")
					}
				case "incomplete":
					finishReason = lo.ToPtr("length")
				case "failed":
					finishReason = lo.ToPtr("error")
				}
			}
			resp.Choices = []model.Choice{
				{
					Index:        0,
					FinishReason: finishReason,
				},
			}
			if streamEvent.Response.Usage != nil {
				usage = streamEvent.Response.Usage
			}
		}
		if usage == nil {
			usage = streamEvent.Usage
		}
		if usage != nil {
			resp.Usage = convertResponsesUsage(usage)
		}

	case "response.incomplete":
		resp.Choices = []model.Choice{
			{
				Index:        0,
				FinishReason: lo.ToPtr("length"),
			},
		}

	case "response.failed", "error":
		resp.Choices = []model.Choice{
			{
				Index:        0,
				FinishReason: lo.ToPtr("error"),
			},
		}

	default:
		// Skip unhandled events
		return nil, nil
	}

	return resp, nil
}

func (o *ResponseOutbound) ensureToolCallState() {
	if o.toolCallIDByOutputIndex == nil {
		o.toolCallIDByOutputIndex = make(map[int]string)
	}
	if o.toolCallNameByOutputIndex == nil {
		o.toolCallNameByOutputIndex = make(map[int]string)
	}
	if o.toolCallNameEmittedByOutputIndex == nil {
		o.toolCallNameEmittedByOutputIndex = make(map[int]bool)
	}
	if o.toolCallArgsByOutputIndex == nil {
		o.toolCallArgsByOutputIndex = make(map[int]string)
	}
}

// ToolCallArgumentsSeen reports whether any function-call arguments have already
// been accumulated for the given output index from response.function_call_arguments.delta
// chunks. The relay uses this to tell a real tool-call terminal (arguments captured)
// from a premature output_item.done marker that arrives with empty arguments before the
// upstream streams them — treating the latter as terminal would cut the stream and hand
// the client a tool call with empty arguments it cannot execute.
func (o *ResponseOutbound) ToolCallArgumentsSeen(outputIndex int) bool {
	if o == nil || o.toolCallArgsByOutputIndex == nil {
		return false
	}
	return strings.TrimSpace(o.toolCallArgsByOutputIndex[outputIndex]) != ""
}

// emitToolCallName returns the tool call name only the first time it is emitted
// for a given output index, and an empty string on every subsequent call.
//
// Upstream Responses streams repeat the full function name on every
// response.function_call_arguments.delta event (and again on output_item.done).
// Forwarding that verbatim makes downstream chat-style aggregators — which do
// `name += delta.Name` — duplicate the name once per streamed chunk, so a long
// argument payload (more chunks) yields more copies. The genuine OpenAI Chat
// Completions streaming shape carries the tool name only on the first tool_call
// delta, arguments incrementally afterwards; we mirror that shape here.
func (o *ResponseOutbound) emitToolCallName(outputIndex int, name string) string {
	if name == "" || o.toolCallNameEmittedByOutputIndex[outputIndex] {
		return ""
	}
	o.toolCallNameEmittedByOutputIndex[outputIndex] = true
	return name
}

// ResponsesRequest represents the OpenAI Responses API request format.
type ResponsesRequest struct {
	Model                string                `json:"model"`
	Instructions         string                `json:"instructions,omitempty"`
	Input                ResponsesInput        `json:"input"`
	Tools                []ResponsesTool       `json:"tools,omitempty"`
	ToolChoice           *ResponsesToolChoice  `json:"tool_choice,omitempty"`
	ParallelToolCalls    *bool                 `json:"parallel_tool_calls,omitempty"`
	Stream               *bool                 `json:"stream,omitempty"`
	Text                 *ResponsesTextOptions `json:"text,omitempty"`
	Store                *bool                 `json:"store,omitempty"`
	ServiceTier          *string               `json:"service_tier,omitempty"`
	User                 *string               `json:"user,omitempty"`
	PromptCacheKey       *string               `json:"prompt_cache_key,omitempty"`
	PromptCacheRetention *string               `json:"prompt_cache_retention,omitempty"`
	PreviousResponseID   *string               `json:"previous_response_id,omitempty"`
	Metadata             map[string]string     `json:"metadata,omitempty"`
	ClientMetadata       json.RawMessage       `json:"client_metadata,omitempty"`
	MaxOutputTokens      *int64                `json:"max_output_tokens,omitempty"`
	Temperature          *float64              `json:"temperature,omitempty"`
	TopP                 *float64              `json:"top_p,omitempty"`
	Reasoning            *ResponsesReasoning   `json:"reasoning,omitempty"`
	Include              []string              `json:"include,omitempty"`
}

type ResponsesInput struct {
	Text  *string
	Items []ResponsesItem
	Raw   json.RawMessage
}

func (i ResponsesInput) MarshalJSON() ([]byte, error) {
	if len(i.Raw) > 0 {
		return i.Raw, nil
	}
	if i.Text != nil {
		return json.Marshal(i.Text)
	}
	return json.Marshal(i.Items)
}

func (i *ResponsesInput) UnmarshalJSON(data []byte) error {
	i.Raw = append(i.Raw[:0], data...)
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		i.Text = &text
		return nil
	}
	var items []ResponsesItem
	if err := json.Unmarshal(data, &items); err == nil {
		i.Items = items
		return nil
	}
	return fmt.Errorf("invalid input format")
}

type ResponsesItem struct {
	ID       string          `json:"id,omitempty"`
	Type     string          `json:"type,omitempty"`
	Role     string          `json:"role,omitempty"`
	Content  *ResponsesInput `json:"content,omitempty"`
	Status   *string         `json:"status,omitempty"`
	Text     *string         `json:"text,omitempty"`
	ImageURL *string         `json:"image_url,omitempty"`
	Detail   *string         `json:"detail,omitempty"`

	// input_file fields (Codex / OpenAI Responses document input)
	FileID   *string `json:"file_id,omitempty"`
	Filename *string `json:"filename,omitempty"`
	FileData *string `json:"file_data,omitempty"`
	FileURL  *string `json:"file_url,omitempty"`

	// input_audio carries audio content for the OpenAI Responses
	// `input_audio` item type: {type:"input_audio", input_audio:{data, format}}.
	InputAudio *ResponsesInputAudio `json:"input_audio,omitempty"`

	// Annotations for output_text content
	Annotations []ResponsesAnnotation `json:"annotations,omitempty"`

	// Function call fields
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Action    string `json:"action,omitempty"`

	// Function call output
	Output *ResponsesInput `json:"output,omitempty"`

	// Image generation fields
	Result       *string `json:"result,omitempty"`
	Background   *string `json:"background,omitempty"`
	OutputFormat *string `json:"output_format,omitempty"`
	Quality      *string `json:"quality,omitempty"`
	Size         *string `json:"size,omitempty"`

	// Reasoning fields
	Summary          []ResponsesReasoningSummary `json:"summary,omitempty"`
	EncryptedContent *string                     `json:"encrypted_content,omitempty"`
}

// ResponsesInputAudio is the nested payload of an OpenAI Responses
// `input_audio` content item. Data is base64-encoded audio and Format is the
// codec (e.g. "wav", "mp3"), mirroring the internal model.Audio fields.
type ResponsesInputAudio struct {
	Data   string `json:"data"`
	Format string `json:"format"`
}

// decodeFlexibleJSONString reads a JSON value that is normally a string but may
// arrive as an object/array/number from non-compliant upstreams. Some second-hand
// relay gateways send function-call `arguments` as a raw JSON object instead of a
// stringified JSON. A JSON string is taken verbatim; any other JSON value is
// re-serialized to its compact string form so downstream string handling keeps
// working instead of failing the whole stream event.
func decodeFlexibleJSONString(raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return "", err
		}
		return s, nil
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, trimmed); err != nil {
		return "", err
	}
	return compact.String(), nil
}

// UnmarshalJSON tolerates non-compliant upstreams that send function-call
// `arguments` as a JSON object/array rather than the OpenAI-spec stringified JSON.
func (r *ResponsesItem) UnmarshalJSON(data []byte) error {
	type responsesItemAlias ResponsesItem
	var aux struct {
		responsesItemAlias
		Arguments json.RawMessage `json:"arguments,omitempty"`
		Action    json.RawMessage `json:"action,omitempty"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*r = ResponsesItem(aux.responsesItemAlias)
	args, err := decodeFlexibleJSONString(aux.Arguments)
	if err != nil {
		return err
	}
	r.Arguments = args
	action, err := decodeFlexibleJSONString(aux.Action)
	if err != nil {
		return err
	}
	r.Action = action
	if r.Arguments == "" && r.Action != "" {
		r.Arguments = r.Action
	}
	return nil
}

func isResponsesToolCallItemType(typ string) bool {
	switch strings.TrimSpace(typ) {
	case "tool_call",
		"function_call",
		"local_shell_call",
		"tool_search_call",
		"custom_tool_call",
		"mcp_tool_call":
		return true
	default:
		return false
	}
}

func responsesToolCallName(item *ResponsesItem) string {
	if item == nil {
		return ""
	}
	if name := strings.TrimSpace(item.Name); name != "" {
		return name
	}
	typ := strings.TrimSpace(item.Type)
	switch typ {
	case "function_call":
		return "function"
	case "local_shell_call":
		return "local_shell"
	case "tool_search_call":
		return "tool_search"
	case "custom_tool_call":
		return "custom_tool"
	case "mcp_tool_call":
		return "mcp_tool"
	case "tool_call":
		return "tool"
	default:
		return strings.TrimSuffix(typ, "_call")
	}
}

type ResponsesReasoningSummary struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type ResponsesAnnotation struct {
	Type       string  `json:"type"`
	StartIndex *int    `json:"start_index,omitempty"`
	EndIndex   *int    `json:"end_index,omitempty"`
	URL        *string `json:"url,omitempty"`
	Title      *string `json:"title,omitempty"`
	FileID     *string `json:"file_id,omitempty"`
	Filename   *string `json:"filename,omitempty"`
}

type ResponsesTool struct {
	Raw               json.RawMessage `json:"-"`
	Type              string          `json:"type,omitempty"`
	Name              string          `json:"name,omitempty"`
	Description       string          `json:"description,omitempty"`
	Parameters        map[string]any  `json:"parameters,omitempty"`
	Strict            *bool           `json:"strict,omitempty"`
	Background        string          `json:"background,omitempty"`
	OutputFormat      string          `json:"output_format,omitempty"`
	Quality           string          `json:"quality,omitempty"`
	Size              string          `json:"size,omitempty"`
	OutputCompression *int64          `json:"output_compression,omitempty"`
	InputFidelity     string          `json:"input_fidelity,omitempty"`
	InputImageMask    map[string]any  `json:"input_image_mask,omitempty"`
	Moderation        string          `json:"moderation,omitempty"`
	PartialImages     *int64          `json:"partial_images,omitempty"`
	Watermark         bool            `json:"watermark,omitempty"`
}

func (t ResponsesTool) MarshalJSON() ([]byte, error) {
	if len(t.Raw) > 0 {
		return t.Raw, nil
	}
	type Alias ResponsesTool
	return json.Marshal(Alias(t))
}

type ResponsesToolChoice struct {
	Raw      json.RawMessage `json:"-"`
	Mode     *string         `json:"mode,omitempty"`
	Type     *string         `json:"type,omitempty"`
	Name     *string         `json:"name,omitempty"`
	Function *struct {
		Name string `json:"name,omitempty"`
	} `json:"function,omitempty"`
}

func (t ResponsesToolChoice) MarshalJSON() ([]byte, error) {
	if len(t.Raw) > 0 {
		return t.Raw, nil
	}
	// If only Mode is set and it's a simple mode like "auto", "none", "required"
	if t.Mode != nil && t.Type == nil && t.Name == nil {
		return json.Marshal(*t.Mode)
	}
	// Otherwise, serialize as an object
	type Alias ResponsesToolChoice
	return json.Marshal(Alias(t))
}

type ResponsesTextOptions struct {
	Raw       json.RawMessage      `json:"-"`
	Format    *ResponsesTextFormat `json:"format,omitempty"`
	Verbosity *string              `json:"verbosity,omitempty"`
}

func (t ResponsesTextOptions) MarshalJSON() ([]byte, error) {
	if len(t.Raw) > 0 {
		return t.Raw, nil
	}
	type Alias ResponsesTextOptions
	return json.Marshal(Alias(t))
}

type ResponsesTextFormat struct {
	Type   string          `json:"type,omitempty"`
	Name   string          `json:"name,omitempty"`
	Schema json.RawMessage `json:"schema,omitempty"`
}

type ResponsesReasoning struct {
	Effort string `json:"effort,omitempty"`
}

// ResponsesResponse represents the OpenAI Responses API response format.
type ResponsesResponse struct {
	Object    string          `json:"object"`
	ID        string          `json:"id"`
	Model     string          `json:"model"`
	CreatedAt int64           `json:"created_at"`
	Output    []ResponsesItem `json:"output"`
	Status    *string         `json:"status,omitempty"`
	Usage     *ResponsesUsage `json:"usage,omitempty"`
	Error     *ResponsesError `json:"error,omitempty"`
}

type ResponsesUsage struct {
	InputTokens       int64 `json:"input_tokens"`
	InputTokenDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
		AudioTokens  int64 `json:"audio_tokens,omitempty"`
	} `json:"input_tokens_details"`
	OutputTokens       int64 `json:"output_tokens"`
	OutputTokenDetails struct {
		ReasoningTokens          int64 `json:"reasoning_tokens"`
		AudioTokens              int64 `json:"audio_tokens,omitempty"`
		AcceptedPredictionTokens int64 `json:"accepted_prediction_tokens,omitempty"`
		RejectedPredictionTokens int64 `json:"rejected_prediction_tokens,omitempty"`
	} `json:"output_tokens_details"`
	TotalTokens              int64 `json:"total_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens,omitempty"`
}

type ResponsesError struct {
	Code    ResponsesErrorCode `json:"code"`
	Message string             `json:"message"`
}

// ResponsesErrorCode accepts both the OpenAI-style string codes and numeric
// status-like codes emitted by some compatible gateways. A strict int here
// makes the whole stream fail before the "response.failed" event can be
// converted into a normal error finish chunk.
type ResponsesErrorCode string

func (c *ResponsesErrorCode) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		*c = ""
		return nil
	}

	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*c = ResponsesErrorCode(s)
		return nil
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var n json.Number
	if err := decoder.Decode(&n); err == nil {
		*c = ResponsesErrorCode(n.String())
		return nil
	}

	*c = ResponsesErrorCode(string(data))
	return nil
}

type ResponsesStreamEvent struct {
	Type           string             `json:"type"`
	SequenceNumber int                `json:"sequence_number"`
	Response       *ResponsesResponse `json:"response,omitempty"`
	Usage          *ResponsesUsage    `json:"usage,omitempty"`
	OutputIndex    int                `json:"output_index"`
	Item           *ResponsesItem     `json:"item,omitempty"`
	ItemID         *string            `json:"item_id,omitempty"`
	ContentIndex   *int               `json:"content_index,omitempty"`
	Delta          string             `json:"delta,omitempty"`
	Text           string             `json:"text,omitempty"`
	Name           string             `json:"name,omitempty"`
	CallID         string             `json:"call_id,omitempty"`
	Arguments      string             `json:"arguments,omitempty"`
	SummaryIndex   *int               `json:"summary_index,omitempty"`
	Code           string             `json:"code,omitempty"`
	Message        string             `json:"message,omitempty"`
}

// UnmarshalJSON tolerates upstreams that send the top-level `arguments`/`delta`
// of a stream event as a JSON object/array instead of a stringified JSON. The
// nested `item` is decoded through ResponsesItem.UnmarshalJSON automatically.
func (e *ResponsesStreamEvent) UnmarshalJSON(data []byte) error {
	type responsesStreamEventAlias ResponsesStreamEvent
	var aux struct {
		responsesStreamEventAlias
		Arguments json.RawMessage `json:"arguments,omitempty"`
		Delta     json.RawMessage `json:"delta,omitempty"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*e = ResponsesStreamEvent(aux.responsesStreamEventAlias)
	args, err := decodeFlexibleJSONString(aux.Arguments)
	if err != nil {
		return err
	}
	e.Arguments = args
	delta, err := decodeFlexibleJSONString(aux.Delta)
	if err != nil {
		return err
	}
	e.Delta = delta
	return nil
}

func (u *ResponsesUsage) UnmarshalJSON(data []byte) error {
	type responsesUsageAlias ResponsesUsage
	type tokenDetails struct {
		CachedTokens *int64 `json:"cached_tokens"`
		AudioTokens  *int64 `json:"audio_tokens"`
	}
	type outputTokenDetails struct {
		ReasoningTokens          *int64 `json:"reasoning_tokens"`
		AudioTokens              *int64 `json:"audio_tokens"`
		AcceptedPredictionTokens *int64 `json:"accepted_prediction_tokens"`
		RejectedPredictionTokens *int64 `json:"rejected_prediction_tokens"`
	}
	var aux struct {
		responsesUsageAlias
		PromptTokens             *int64              `json:"prompt_tokens"`
		CompletionTokens         *int64              `json:"completion_tokens"`
		PromptTokensDetails      *tokenDetails       `json:"prompt_tokens_details"`
		CompletionTokensDetails  *outputTokenDetails `json:"completion_tokens_details"`
		CachedTokens             *int64              `json:"cached_tokens"`
		PromptCacheHitTokens     *int64              `json:"prompt_cache_hit_tokens"`
		CacheCreationInputTokens *int64              `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     *int64              `json:"cache_read_input_tokens"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}

	*u = ResponsesUsage(aux.responsesUsageAlias)
	if u.InputTokens == 0 && aux.PromptTokens != nil {
		u.InputTokens = *aux.PromptTokens
	}
	if u.OutputTokens == 0 && aux.CompletionTokens != nil {
		u.OutputTokens = *aux.CompletionTokens
	}
	if u.TotalTokens == 0 && (u.InputTokens > 0 || u.OutputTokens > 0) {
		u.TotalTokens = u.InputTokens + u.OutputTokens
	}
	if aux.CacheCreationInputTokens != nil && *aux.CacheCreationInputTokens > 0 {
		u.CacheCreationInputTokens = *aux.CacheCreationInputTokens
	}
	if aux.CacheReadInputTokens != nil && *aux.CacheReadInputTokens > 0 {
		u.CacheReadInputTokens = *aux.CacheReadInputTokens
		if u.InputTokenDetails.CachedTokens == 0 {
			u.InputTokenDetails.CachedTokens = *aux.CacheReadInputTokens
		}
	}
	if aux.PromptTokensDetails != nil {
		if u.InputTokenDetails.CachedTokens == 0 && aux.PromptTokensDetails.CachedTokens != nil {
			u.InputTokenDetails.CachedTokens = *aux.PromptTokensDetails.CachedTokens
		}
		if u.InputTokenDetails.AudioTokens == 0 && aux.PromptTokensDetails.AudioTokens != nil {
			u.InputTokenDetails.AudioTokens = *aux.PromptTokensDetails.AudioTokens
		}
	}
	if u.InputTokenDetails.CachedTokens == 0 {
		switch {
		case aux.CachedTokens != nil && *aux.CachedTokens > 0:
			u.InputTokenDetails.CachedTokens = *aux.CachedTokens
		case aux.PromptCacheHitTokens != nil && *aux.PromptCacheHitTokens > 0:
			u.InputTokenDetails.CachedTokens = *aux.PromptCacheHitTokens
		}
	}
	if aux.CompletionTokensDetails != nil {
		if u.OutputTokenDetails.ReasoningTokens == 0 && aux.CompletionTokensDetails.ReasoningTokens != nil {
			u.OutputTokenDetails.ReasoningTokens = *aux.CompletionTokensDetails.ReasoningTokens
		}
		if u.OutputTokenDetails.AudioTokens == 0 && aux.CompletionTokensDetails.AudioTokens != nil {
			u.OutputTokenDetails.AudioTokens = *aux.CompletionTokensDetails.AudioTokens
		}
		if u.OutputTokenDetails.AcceptedPredictionTokens == 0 && aux.CompletionTokensDetails.AcceptedPredictionTokens != nil {
			u.OutputTokenDetails.AcceptedPredictionTokens = *aux.CompletionTokensDetails.AcceptedPredictionTokens
		}
		if u.OutputTokenDetails.RejectedPredictionTokens == 0 && aux.CompletionTokensDetails.RejectedPredictionTokens != nil {
			u.OutputTokenDetails.RejectedPredictionTokens = *aux.CompletionTokensDetails.RejectedPredictionTokens
		}
	}
	return nil
}

// Conversion functions

func ConvertToResponsesRequest(req *model.InternalLLMRequest) *ResponsesRequest {
	result := &ResponsesRequest{
		Model:                req.Model,
		Temperature:          req.Temperature,
		TopP:                 req.TopP,
		Stream:               req.Stream,
		Store:                req.Store,
		ServiceTier:          req.ServiceTier,
		User:                 req.User,
		PromptCacheKey:       req.PromptCacheKey,
		PromptCacheRetention: req.PromptCacheRetention,
		PreviousResponseID:   req.PreviousResponseID,
		Metadata:             req.Metadata,
		ClientMetadata:       cloneRawMessage(req.ClientMetadata),
		MaxOutputTokens:      req.MaxCompletionTokens,
		ParallelToolCalls:    req.ParallelToolCalls,
		Include:              append([]string(nil), req.Include...),
	}

	// Convert instructions from system messages
	result.Instructions = convertInstructionsFromMessages(req.Messages)

	// Convert input from messages
	result.Input = convertInputFromMessages(req.Messages)

	// Convert tools
	if len(req.Tools) > 0 {
		result.Tools = convertToolsToResponses(req.Tools)
	}

	// Convert tool choice
	if req.ToolChoice != nil {
		result.ToolChoice = convertToolChoiceToResponses(req.ToolChoice)
	}

	// Convert text options
	if req.ResponseFormat != nil {
		result.Text = &ResponsesTextOptions{
			Format: &ResponsesTextFormat{
				Type:   req.ResponseFormat.Type,
				Schema: req.ResponseFormat.JSONSchema,
			},
		}
	}

	// Convert reasoning
	if req.ReasoningEffort != "" || req.ReasoningBudget != nil {
		result.Reasoning = &ResponsesReasoning{
			Effort: req.ReasoningEffort,
		}
	}

	applyRawResponsesRequestFields(req, result)

	return result
}

func applyRawResponsesRequestFields(req *model.InternalLLMRequest, result *ResponsesRequest) {
	if req == nil || result == nil || !hasRawResponsesRequestFields(req) {
		return
	}
	if req.ResponsesInstructions != nil {
		result.Instructions = *req.ResponsesInstructions
	}
	if len(req.ResponsesInputRaw) > 0 {
		result.Input = ResponsesInput{Raw: cloneRawMessage(req.ResponsesInputRaw)}
	}
	if len(req.ResponsesToolsRaw) > 0 {
		result.Tools = make([]ResponsesTool, 0, len(req.ResponsesToolsRaw))
		for _, raw := range req.ResponsesToolsRaw {
			if len(raw) == 0 {
				continue
			}
			result.Tools = append(result.Tools, ResponsesTool{Raw: cloneRawMessage(raw)})
		}
	}
	if len(req.ResponsesToolChoiceRaw) > 0 {
		result.ToolChoice = &ResponsesToolChoice{Raw: cloneRawMessage(req.ResponsesToolChoiceRaw)}
	}
	if len(req.ResponsesTextRaw) > 0 {
		result.Text = &ResponsesTextOptions{Raw: cloneRawMessage(req.ResponsesTextRaw)}
	}
}

func hasRawResponsesRequestFields(req *model.InternalLLMRequest) bool {
	if req == nil {
		return false
	}
	return req.RawAPIFormat == model.APIFormatOpenAIResponse ||
		req.ResponsesInstructions != nil ||
		len(req.ResponsesInputRaw) > 0 ||
		len(req.ResponsesToolsRaw) > 0 ||
		len(req.ResponsesToolChoiceRaw) > 0 ||
		len(req.ResponsesTextRaw) > 0
}

func cloneRawMessage(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	out := make([]byte, len(raw))
	copy(out, raw)
	return out
}

func convertInstructionsFromMessages(msgs []model.Message) string {
	var instructions []string
	for _, msg := range msgs {
		if msg.Role != "system" && msg.Role != "developer" {
			continue
		}
		if msg.Content.Content != nil {
			instructions = append(instructions, *msg.Content.Content)
		}
		if len(msg.Content.MultipleContent) > 0 {
			var sb strings.Builder
			for _, p := range msg.Content.MultipleContent {
				if p.Type == "text" && p.Text != nil {
					if sb.Len() > 0 {
						sb.WriteString("\n")
					}
					sb.WriteString(*p.Text)
				}
			}
			if sb.Len() > 0 {
				instructions = append(instructions, sb.String())
			}
		}
	}
	return strings.Join(instructions, "\n")
}

func convertInputFromMessages(msgs []model.Message) ResponsesInput {
	if len(msgs) == 0 {
		return ResponsesInput{}
	}

	var items []ResponsesItem
	for _, msg := range msgs {
		switch msg.Role {
		case "system", "developer":
			continue
		case "user":
			items = append(items, convertUserMessageToResponses(msg))
		case "assistant":
			items = append(items, convertAssistantMessageToResponses(msg)...)
		case "tool":
			items = append(items, convertToolMessageToResponses(msg))
		}
	}

	return ResponsesInput{Items: items}
}

func convertUserMessageToResponses(msg model.Message) ResponsesItem {
	var contentItems []ResponsesItem

	if msg.Content.Content != nil {
		contentItems = append(contentItems, ResponsesItem{
			Type: "input_text",
			Text: msg.Content.Content,
		})
	} else {
		for _, p := range msg.Content.MultipleContent {
			switch p.Type {
			case "text":
				if p.Text != nil {
					contentItems = append(contentItems, ResponsesItem{
						Type: "input_text",
						Text: p.Text,
					})
				}
			case "image_url":
				if p.ImageURL != nil {
					contentItems = append(contentItems, ResponsesItem{
						Type:     "input_image",
						ImageURL: &p.ImageURL.URL,
						Detail:   p.ImageURL.Detail,
					})
				}
			case "file":
				if fileItem := convertFileToInputFile(p); fileItem != nil {
					contentItems = append(contentItems, *fileItem)
				}
			case "input_audio":
				if audioItem := convertAudioToInputAudio(p); audioItem != nil {
					contentItems = append(contentItems, *audioItem)
				}
			}
		}
	}

	return ResponsesItem{
		Type:    "message",
		Role:    msg.Role,
		Content: &ResponsesInput{Items: contentItems},
	}
}

// convertFileToInputFile rebuilds the internal "file" content part into a Codex
// / OpenAI Responses `input_file` item, mirroring the input_image handling. It
// preserves base64 data, remote url, file id and filename. Returns nil when the
// part carries no usable file reference.
func convertFileToInputFile(p model.MessageContentPart) *ResponsesItem {
	if p.File == nil {
		return nil
	}
	file := p.File

	item := &ResponsesItem{Type: "input_file"}
	has := false
	if file.Filename != "" {
		item.Filename = &file.Filename
		has = true
	}
	if file.FileData != "" {
		data := file.FileData
		item.FileData = &data
		has = true
	}
	if file.FileURL != "" {
		url := file.FileURL
		item.FileURL = &url
		has = true
	}
	if file.FileID != "" {
		id := file.FileID
		item.FileID = &id
		has = true
	}
	if !has {
		return nil
	}
	return item
}

// convertAudioToInputAudio rebuilds the internal "input_audio" content part into
// an OpenAI Responses `input_audio` item, mirroring the input_image / input_file
// handling. Internal audio (carried as model.Audio by chat / gemini inbounds)
// holds base64 data and a format; without this the audio is silently dropped on
// the way out to a Responses upstream. Returns nil when no audio data is present.
func convertAudioToInputAudio(p model.MessageContentPart) *ResponsesItem {
	if p.Audio == nil || p.Audio.Data == "" {
		return nil
	}
	return &ResponsesItem{
		Type: "input_audio",
		InputAudio: &ResponsesInputAudio{
			Data:   p.Audio.Data,
			Format: p.Audio.Format,
		},
	}
}

func convertAssistantMessageToResponses(msg model.Message) []ResponsesItem {
	var items []ResponsesItem

	if msg.ReasoningSignature != nil && strings.TrimSpace(*msg.ReasoningSignature) != "" {
		item := ResponsesItem{
			Type:             "reasoning",
			EncryptedContent: msg.ReasoningSignature,
		}
		if text := strings.TrimSpace(msg.GetReasoningContent()); text != "" {
			item.Summary = []ResponsesReasoningSummary{{
				Type: "summary_text",
				Text: text,
			}}
		}
		items = append(items, item)
	} else if text := strings.TrimSpace(msg.GetReasoningContent()); text != "" {
		items = append(items, ResponsesItem{
			Type: "reasoning",
			Summary: []ResponsesReasoningSummary{{
				Type: "summary_text",
				Text: text,
			}},
		})
	}

	// Handle tool calls
	for _, tc := range msg.ToolCalls {
		items = append(items, ResponsesItem{
			Type:      "function_call",
			CallID:    tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}

	// Handle content
	var contentItems []ResponsesItem
	if msg.Content.Content != nil {
		contentItems = append(contentItems, ResponsesItem{
			Type: "output_text",
			Text: msg.Content.Content,
		})
	} else {
		for _, p := range msg.Content.MultipleContent {
			if p.Type == "text" && p.Text != nil {
				contentItems = append(contentItems, ResponsesItem{
					Type: "output_text",
					Text: p.Text,
				})
			}
		}
	}

	if len(contentItems) > 0 {
		items = append(items, ResponsesItem{
			Type:    "message",
			Role:    msg.Role,
			Status:  lo.ToPtr("completed"),
			Content: &ResponsesInput{Items: contentItems},
		})
	}

	return items
}

func convertToolMessageToResponses(msg model.Message) ResponsesItem {
	var output ResponsesInput

	if msg.Content.Content != nil {
		output.Text = msg.Content.Content
	} else if len(msg.Content.MultipleContent) > 0 {
		for _, p := range msg.Content.MultipleContent {
			if p.Type == "text" && p.Text != nil {
				output.Items = append(output.Items, ResponsesItem{
					Type: "input_text",
					Text: p.Text,
				})
			}
		}
	}

	if output.Text == nil && len(output.Items) == 0 {
		output.Text = lo.ToPtr("")
	}

	return ResponsesItem{
		Type:   "function_call_output",
		CallID: lo.FromPtr(msg.ToolCallID),
		Output: &output,
	}
}

func convertToolsToResponses(tools []model.Tool) []ResponsesTool {
	result := make([]ResponsesTool, 0, len(tools))
	for _, tool := range tools {
		switch tool.Type {
		case "function":
			rt := ResponsesTool{
				Type:        "function",
				Name:        tool.Function.Name,
				Description: tool.Function.Description,
				Strict:      tool.Function.Strict,
			}
			if len(tool.Function.Parameters) > 0 {
				var params map[string]any
				if err := json.Unmarshal(tool.Function.Parameters, &params); err == nil {
					rt.Parameters = params
				}
			}
			result = append(result, rt)
		case "image_generation":
			rt := ResponsesTool{
				Type: "image_generation",
			}
			if tool.ImageGeneration != nil {
				rt.Background = tool.ImageGeneration.Background
				rt.InputFidelity = tool.ImageGeneration.InputFidelity
				rt.InputImageMask = tool.ImageGeneration.InputImageMask
				rt.Moderation = tool.ImageGeneration.Moderation
				rt.OutputFormat = tool.ImageGeneration.OutputFormat
				rt.Quality = tool.ImageGeneration.Quality
				rt.Size = tool.ImageGeneration.Size
				rt.OutputCompression = tool.ImageGeneration.OutputCompression
				rt.PartialImages = tool.ImageGeneration.PartialImages
				rt.Watermark = tool.ImageGeneration.Watermark
			}
			result = append(result, rt)
		}
	}
	return result
}

func convertToolChoiceToResponses(tc *model.ToolChoice) *ResponsesToolChoice {
	if tc == nil {
		return nil
	}

	result := &ResponsesToolChoice{}
	if tc.ToolChoice != nil {
		result.Mode = tc.ToolChoice
	} else if tc.NamedToolChoice != nil {
		toolType := strings.ToLower(tc.NamedToolChoice.Type)
		if toolType == "" || toolType == "tool" {
			toolType = "function"
		}
		result.Type = &toolType
		result.Name = &tc.NamedToolChoice.Function.Name
	}
	return result
}

func convertToLLMResponseFromResponses(resp *ResponsesResponse) *model.InternalLLMResponse {
	if resp == nil {
		return &model.InternalLLMResponse{
			Object: "chat.completion",
		}
	}

	result := &model.InternalLLMResponse{
		ID:      resp.ID,
		Object:  "chat.completion",
		Model:   resp.Model,
		Created: resp.CreatedAt,
	}

	var (
		contentParts     []model.MessageContentPart
		textContent      strings.Builder
		reasoningContent strings.Builder
		toolCalls        []model.ToolCall
	)

	for _, outputItem := range resp.Output {
		switch outputItem.Type {
		case "message":
			if outputItem.Content != nil {
				for _, item := range outputItem.Content.Items {
					if item.Type == "output_text" && item.Text != nil {
						textContent.WriteString(*item.Text)
					}
				}
			}
		case "output_text":
			if outputItem.Text != nil {
				textContent.WriteString(*outputItem.Text)
			}
		case "function_call":
			toolCalls = append(toolCalls, model.ToolCall{
				ID:   outputItem.CallID,
				Type: "function",
				Function: model.FunctionCall{
					Name:      outputItem.Name,
					Arguments: outputItem.Arguments,
				},
			})
		case "reasoning":
			for _, summary := range outputItem.Summary {
				reasoningContent.WriteString(summary.Text)
			}
		case "image_generation_call":
			if outputItem.Result != nil && *outputItem.Result != "" {
				outputFormat := "png"
				if outputItem.OutputFormat != nil {
					outputFormat = *outputItem.OutputFormat
				}
				contentParts = append(contentParts, model.MessageContentPart{
					Type: "image_url",
					ImageURL: &model.ImageURL{
						URL: "data:image/" + outputFormat + ";base64," + *outputItem.Result,
					},
				})
			}
		}
	}

	choice := model.Choice{
		Index: 0,
		Message: &model.Message{
			Role:      "assistant",
			ToolCalls: toolCalls,
		},
	}

	// Set reasoning content if present
	if reasoningContent.Len() > 0 {
		choice.Message.ReasoningContent = lo.ToPtr(reasoningContent.String())
	}

	// Set message content
	if textContent.Len() > 0 {
		if len(contentParts) > 0 {
			textPart := model.MessageContentPart{
				Type: "text",
				Text: lo.ToPtr(textContent.String()),
			}
			contentParts = append([]model.MessageContentPart{textPart}, contentParts...)
			choice.Message.Content = model.MessageContent{
				MultipleContent: contentParts,
			}
		} else {
			choice.Message.Content = model.MessageContent{
				Content: lo.ToPtr(textContent.String()),
			}
		}
	} else if len(contentParts) > 0 {
		choice.Message.Content = model.MessageContent{
			MultipleContent: contentParts,
		}
	}

	// Set finish reason based on status
	if len(toolCalls) > 0 {
		choice.FinishReason = lo.ToPtr("tool_calls")
	} else if resp.Status != nil {
		switch *resp.Status {
		case "completed":
			choice.FinishReason = lo.ToPtr("stop")
		case "failed":
			choice.FinishReason = lo.ToPtr("error")
		case "incomplete":
			choice.FinishReason = lo.ToPtr("length")
		}
	}

	result.Choices = []model.Choice{choice}
	result.Usage = convertResponsesUsage(resp.Usage)

	return result
}

func convertResponsesUsage(usage *ResponsesUsage) *model.Usage {
	if usage == nil {
		return nil
	}

	result := &model.Usage{
		PromptTokens:             usage.InputTokens,
		CompletionTokens:         usage.OutputTokens,
		TotalTokens:              usage.TotalTokens,
		CacheCreationInputTokens: usage.CacheCreationInputTokens,
	}

	if usage.InputTokenDetails.CachedTokens > 0 || usage.InputTokenDetails.AudioTokens > 0 {
		result.PromptTokensDetails = &model.PromptTokensDetails{
			CachedTokens: usage.InputTokenDetails.CachedTokens,
			AudioTokens:  usage.InputTokenDetails.AudioTokens,
		}
	}
	if usage.CacheReadInputTokens > 0 || usage.CacheCreationInputTokens > 0 {
		result.SeparateCacheInputTokens = true
		if result.PromptTokensDetails == nil {
			result.PromptTokensDetails = &model.PromptTokensDetails{}
		}
		if result.PromptTokensDetails.CachedTokens == 0 {
			result.PromptTokensDetails.CachedTokens = usage.CacheReadInputTokens
		}
	}

	if usage.OutputTokenDetails.ReasoningTokens > 0 || usage.OutputTokenDetails.AudioTokens > 0 || usage.OutputTokenDetails.AcceptedPredictionTokens > 0 || usage.OutputTokenDetails.RejectedPredictionTokens > 0 {
		result.CompletionTokensDetails = &model.CompletionTokensDetails{
			ReasoningTokens:          usage.OutputTokenDetails.ReasoningTokens,
			AudioTokens:              usage.OutputTokenDetails.AudioTokens,
			AcceptedPredictionTokens: usage.OutputTokenDetails.AcceptedPredictionTokens,
			RejectedPredictionTokens: usage.OutputTokenDetails.RejectedPredictionTokens,
		}
	}

	return result
}
