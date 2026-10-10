package anthropic

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// MessageRequest represents the Anthropic Messages API request format.
type MessageRequest struct {
	MaxTokens int64          `json:"max_tokens" validate:"required,gte=1"`
	Messages  []MessageParam `json:"messages"   validate:"required"`
	Model     string         `json:"model,omitempty"      validate:"required"`

	// The version of the Anthropic API to use.
	//
	// It is required for bedrock and vertex.
	AnthropicVersion string `json:"anthropic_version,omitempty"`

	// Amount of randomness injected into the response.
	//
	// Defaults to `1.0`. Ranges from `0.0` to `1.0`. Use `temperature` closer to `0.0`
	// for analytical / multiple choice, and closer to `1.0` for creative and
	// generative tasks.
	//
	// Note that even with `temperature` of `0.0`, the results will not be fully
	// deterministic.
	Temperature *float64 `json:"temperature,omitempty"`

	// Only sample from the top K options for each subsequent token.
	//
	// Used to remove "long tail" low probability responses.
	// [Learn more technical details here](https://towardsdatascience.com/how-to-sample-from-language-models-682bceb97277).
	//
	// Recommended for advanced use cases only. You usually only need to use
	// `temperature`.
	TopK *int64 `json:"top_k,omitempty"`

	// Use nucleus sampling.
	//
	// In nucleus sampling, we compute the cumulative distribution over all the options
	// for each subsequent token in decreasing probability order and cut it off once it
	// reaches a particular probability specified by `top_p`. You should either alter
	// `temperature` or `top_p`, but not both.
	//
	// Recommended for advanced use cases only. You usually only need to use
	// `temperature`.
	TopP *float64 `json:"top_p,omitempty"`

	// An object describing metadata about the request.
	Metadata *AnthropicMetadata `json:"metadata,omitempty"`

	// Determines whether to use priority capacity (if available) or standard capacity
	// for this request.
	//
	// Anthropic offers different levels of service for your API requests. See
	// [service-tiers](https://docs.anthropic.com/en/api/service-tiers) for details.
	//
	// Any of "auto", "standard_only".
	ServiceTier string `json:"service_tier,omitempty"`

	// Custom text sequences that will cause the model to stop generating.
	//
	// Our models will normally stop when they have naturally completed their turn,
	// which will result in a response `stop_reason` of `"end_turn"`.
	//
	// If you want the model to stop generating when it encounters custom strings of
	// text, you can use the `stop_sequences` parameter. If the model encounters one of
	// the custom sequences, the response `stop_reason` value will be `"stop_sequence"`
	// and the response `stop_sequence` value will contain the matched stop sequence.
	StopSequences []string `json:"stop_sequences,omitempty"`

	// System is an optional system prompt.
	System *SystemPrompt `json:"system,omitempty"`

	// Thinking is an optional thinking configuration.
	Thinking *Thinking `json:"thinking,omitempty"`

	// OutputConfig is an optional output configuration for adaptive thinking.
	OutputConfig *OutputConfig `json:"output_config,omitempty"`

	// ContextManagement is Claude Code's native long-conversation/auto-compact
	// control object. Keep it as raw JSON so future client fields survive a
	// same-protocol Anthropic relay without this transformer knowing every shape.
	ContextManagement json.RawMessage `json:"context_management,omitempty"`

	// Betas is accepted by some proxy-compatible clients as a body-level beta
	// list. Official Anthropic expects these as Anthropic-Beta headers, so the
	// inbound transformer lifts them into TransformOptions.
	Betas []string `json:"betas,omitempty"`

	// Tools is an optional array of tools.
	Tools []Tool `json:"tools,omitempty"`
	// ToolChoice is an optional tool choice configuration.
	ToolChoice *ToolChoice `json:"tool_choice,omitempty"`

	// Stream is an optional flag to enable streaming.
	Stream *bool `json:"stream,omitempty"`

	// ForceEmptyTools preserves Claude Code's no-tool shape ("tools":[]). It is
	// intentionally ignored on input and only used by outbound Anthropic
	// marshalling when the semantic tool list is empty but the original field was
	// present.
	ForceEmptyTools bool `json:"-"`

	// Extra holds top-level request keys this struct does not model (for example the
	// client's `safeguards` object). A same-protocol Claude -> Claude relay must hand
	// the provider back the request the CLI actually sent: silently dropping an
	// unrecognised top-level key changes the request shape, and can drop a control the
	// provider honours. Populated by UnmarshalJSON, replayed by MarshalJSON.
	Extra map[string]json.RawMessage `json:"-"`
}

// messageRequestEncodedKeys reports the top-level keys MessageRequest models, plus the
// `tools` key whose presence is tracked separately by ForceEmptyTools rather than by the
// slice itself (an empty tools array is omitted by `omitempty`). Derived from the
// encoded struct rather than a hand-written list so a newly added field can never be
// misclassified as "unknown" and emitted twice.
func messageRequestEncodedKeys(m MessageRequest) map[string]struct{} {
	keys := map[string]struct{}{"tools": {}}
	type alias MessageRequest
	data, err := EncodeJSONNoHTMLEscape(alias(m))
	if err != nil {
		return keys
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return keys
	}
	for key := range obj {
		keys[key] = struct{}{}
	}
	return keys
}

func (m *MessageRequest) UnmarshalJSON(data []byte) error {
	type alias MessageRequest
	var parsed alias
	if err := json.Unmarshal(data, &parsed); err != nil {
		return err
	}
	*m = MessageRequest(parsed)

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		// The typed decode above already accepted the object, so a failure here means the
		// raw view is unavailable, not that the request is malformed. Proceed without the
		// fidelity channel rather than rejecting an otherwise valid request.
		return nil
	}
	known := messageRequestEncodedKeys(*m)
	extra := make(map[string]json.RawMessage)
	for key, value := range raw {
		if _, ok := known[key]; ok {
			continue
		}
		if len(value) > 0 {
			extra[key] = append(json.RawMessage(nil), value...)
		}
	}
	if len(extra) > 0 {
		m.Extra = extra
	}
	return nil
}

// EncodeJSONNoHTMLEscape serialises v the way the CLI's own JSON serialiser does. Go's
// json.Marshal rewrites <, > and & as \u003c / \u003e / \u0026, which is a byte-level
// divergence from the body a genuine Claude CLI produces.
func EncodeJSONNoHTMLEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// jsonMemberStart walks back from the offset where a member's value begins to the
// opening quote of that member's key. The encoded object is compact and the top-level
// keys are plain identifiers, so the key string carries no escapes to step over.
func jsonMemberStart(data []byte, valueStart int64) int {
	i := int(valueStart) - 1
	skipSpace := func() {
		for i >= 0 && (data[i] == ' ' || data[i] == '\n' || data[i] == '\t' || data[i] == '\r') {
			i--
		}
	}
	skipSpace()
	if i >= 0 && data[i] == ':' {
		i--
	}
	skipSpace()
	// i now sits on the key's closing quote. Step inside the key and walk back to its
	// opening quote. Top-level keys are plain identifiers, so there are no escaped
	// quotes to step over.
	if i >= 0 && data[i] == '"' {
		i--
	}
	for i >= 0 && data[i] != '"' {
		i--
	}
	return i
}

// topLevelMembers reports where each top-level member of a compact JSON object begins,
// and the offset of the object's closing brace. Splice points are found by decoding the
// object rather than by searching for a key's text: a request whose prompt happens to
// contain `"stream":` must not be mistaken for the real field.
func topLevelMembers(data []byte) (map[string]int, int, bool) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, 0, false
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, 0, false
	}
	starts := make(map[string]int)
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, 0, false
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, 0, false
		}
		valueStart := dec.InputOffset()
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, 0, false
		}
		starts[key] = jsonMemberStart(data, valueStart)
	}
	closeOffset := bytes.LastIndexByte(data, '}')
	if closeOffset < 0 {
		return nil, 0, false
	}
	return starts, closeOffset, true
}

func (m MessageRequest) MarshalJSON() ([]byte, error) {
	type alias MessageRequest
	data, err := EncodeJSONNoHTMLEscape(alias(m))
	if err != nil {
		return nil, err
	}
	forceEmptyTools := m.ForceEmptyTools && len(m.Tools) == 0
	if !forceEmptyTools && len(m.Extra) == 0 {
		return data, nil
	}

	starts, closeOffset, ok := topLevelMembers(data)
	if !ok {
		// Not a shape we can splice into safely: fall back to the plain encoding rather
		// than emitting a malformed body.
		return data, nil
	}
	nonEmpty := len(starts) > 0

	type splice struct {
		at   int
		text string
	}
	splices := make([]splice, 0, len(m.Extra)+1)

	// An explicit "tools":[] belongs where the struct declares `tools` — immediately
	// before `tool_choice` / `stream` — so the member order keeps matching a genuine
	// CLI request. A tools value already carried in Extra wins: replaying the client's
	// own bytes beats a synthesised empty array.
	if forceEmptyTools {
		if _, carried := m.Extra["tools"]; !carried {
			at := closeOffset
			text := `"tools":[]`
			if nonEmpty {
				text = `,"tools":[]`
			}
			if p, ok := starts["tool_choice"]; ok {
				at, text = p, `"tools":[],`
			} else if p, ok := starts["stream"]; ok {
				at, text = p, `"tools":[],`
			}
			splices = append(splices, splice{at: at, text: text})
		}
	}

	// Unmodelled client keys replay verbatim, after every modelled member. Sorted for a
	// deterministic body so the same request always serialises to the same bytes.
	if len(m.Extra) > 0 {
		keys := make([]string, 0, len(m.Extra))
		for key := range m.Extra {
			if len(m.Extra[key]) == 0 || key == "tools" {
				continue
			}
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			encodedKey, err := EncodeJSONNoHTMLEscape(key)
			if err != nil {
				return nil, err
			}
			prefix := ","
			if !nonEmpty && len(splices) == 0 {
				prefix = ""
			}
			splices = append(splices, splice{
				at:   closeOffset,
				text: prefix + string(encodedKey) + ":" + string(m.Extra[key]),
			})
		}
	}
	if len(splices) == 0 {
		return data, nil
	}

	sort.SliceStable(splices, func(i, j int) bool { return splices[i].at < splices[j].at })
	var out bytes.Buffer
	out.Grow(len(data) + 64)
	cursor := 0
	for _, s := range splices {
		if s.at < cursor || s.at > len(data) {
			return data, nil
		}
		out.Write(data[cursor:s.at])
		out.WriteString(s.text)
		cursor = s.at
	}
	out.Write(data[cursor:])
	return out.Bytes(), nil
}

type AnthropicMetadata struct {
	UserID string `json:"user_id,omitempty"`
}

type SystemPrompt struct {
	Prompt *string `json:"prompt,omitempty"`
	// MultiplePrompts is an optional array of system prompts.
	MultiplePrompts []SystemPromptPart `json:"multiple_prompts,omitempty"`
}

func (s *SystemPrompt) MarshalJSON() ([]byte, error) {
	if s.Prompt != nil {
		return EncodeJSONNoHTMLEscape(s.Prompt)
	}

	if len(s.MultiplePrompts) > 0 {
		return EncodeJSONNoHTMLEscape(s.MultiplePrompts)
	}

	return []byte("null"), nil
}

func (s *SystemPrompt) UnmarshalJSON(data []byte) error {
	var str string

	err := json.Unmarshal(data, &str)
	if err == nil {
		s.Prompt = &str
		return nil
	}

	var parts []SystemPromptPart

	err = json.Unmarshal(data, &parts)
	if err == nil {
		s.MultiplePrompts = parts
		return nil
	}

	return fmt.Errorf("invalid system prompt format")
}

type SystemPromptPart struct {
	// Type must be "text".
	Type         string        `json:"type" validate:"required,oneof=text"`
	Text         string        `json:"text" validate:"required"`
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

// Thinking type constants
const (
	ThinkingTypeEnabled  = "enabled"
	ThinkingTypeDisabled = "disabled"
	ThinkingTypeAdaptive = "adaptive"
)

// Effort level constants for OutputConfig
const (
	EffortMax    = "max"
	EffortHigh   = "high"
	EffortMedium = "medium"
	EffortLow    = "low"
)

type Thinking struct {
	Type string `json:"type"                    validate:"required,oneof=enabled disabled adaptive"`
	// Display preserves the adaptive-thinking display preference a real Claude CLI
	// sends (packet-verified 2.1.281: {"type":"adaptive","display":"omitted"}).
	// Dropping it changes the outbound body shape vs a genuine CLI request.
	Display      string `json:"display,omitempty"       validate:"omitempty,oneof=omitted"`
	BudgetTokens *int64 `json:"budget_tokens,omitempty" validate:"required_if=Type enabled"`
}

type OutputConfig struct {
	Effort string                     `json:"effort,omitempty" validate:"omitempty,oneof=max high medium low"`
	Format json.RawMessage            `json:"format,omitempty"`
	Extra  map[string]json.RawMessage `json:"-"`
}

func (o *OutputConfig) UnmarshalJSON(data []byte) error {
	type known struct {
		Effort string          `json:"effort,omitempty"`
		Format json.RawMessage `json:"format,omitempty"`
	}
	var parsed known
	if err := json.Unmarshal(data, &parsed); err != nil {
		return err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	delete(raw, "effort")
	delete(raw, "format")
	o.Effort = parsed.Effort
	if len(parsed.Format) > 0 && string(parsed.Format) != "null" {
		o.Format = append(json.RawMessage(nil), parsed.Format...)
	}
	if len(raw) > 0 {
		o.Extra = raw
	}
	return nil
}

func (o OutputConfig) MarshalJSON() ([]byte, error) {
	obj := make(map[string]json.RawMessage, len(o.Extra)+2)
	for key, value := range o.Extra {
		if len(value) > 0 {
			obj[key] = append(json.RawMessage(nil), value...)
		}
	}
	if o.Effort != "" {
		data, err := EncodeJSONNoHTMLEscape(o.Effort)
		if err != nil {
			return nil, err
		}
		obj["effort"] = data
	}
	if len(o.Format) > 0 {
		obj["format"] = append(json.RawMessage(nil), o.Format...)
	}
	if len(obj) == 0 {
		return []byte(`{}`), nil
	}
	return EncodeJSONNoHTMLEscape(obj)
}

type ToolChoice struct {
	Type string `json:"type" validate:"required,oneof=auto none tool any"`

	// DisableParallelToolUse is an optional flag to disable parallel tool use.
	DisableParallelToolUse *bool `json:"disable_parallel_tool_use,omitempty"`

	// Name is an optional name of the tool to use, it is required when Type is tool.
	Name *string `json:"name,omitempty" validate:"required_if=Type tool"`
}

// Tool represents a tool definition for Anthropic API.
type Tool struct {
	// Ensure the omitempty, otherwise it will be sent empty string to the API, will cause some providers ignore the tool.
	// For now, we only support function (client tool or custom tool in anthropic) tool, so we can just omit the type.
	// Type is parsed only to detect Anthropic built-in tools (computer-use, bash,
	// text_editor, code_execution, ...). Those carry no standard input_schema and
	// instead use proprietary fields (display_width_px, display_number, ...), so we
	// preserve their original JSON in Raw instead of forcing them into a function.
	Type         string          `json:"type,omitempty"`
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"input_schema"`
	CacheControl *CacheControl   `json:"cache_control,omitempty"`

	// Raw holds the original, untouched tool object as it arrived from the client.
	// It is populated for every tool on unmarshal and is what lets the transformer
	// round-trip built-in tools (and any future proprietary fields) without knowing
	// their full shape. Not serialized directly.
	Raw json.RawMessage `json:"-"`
}

// UnmarshalJSON captures the original tool object in Raw while still decoding the
// standard fields. This keeps proprietary built-in tool fields
// (display_width_px/display_height_px/display_number, etc.) from being silently
// dropped during inbound parsing.
func (t *Tool) UnmarshalJSON(data []byte) error {
	type alias Tool
	var parsed alias
	if err := json.Unmarshal(data, &parsed); err != nil {
		return err
	}
	*t = Tool(parsed)
	t.Raw = append(json.RawMessage(nil), data...)
	return nil
}

// MarshalJSON re-emits the original tool object verbatim when Raw is set (built-in
// tools restored on the outbound path), so proprietary fields survive the
// round-trip. cache_control, if it was attached on the internal side, is merged
// back into the raw object. Standard custom/function tools fall back to the field
// set (with type omitted, preserving prior behavior).
func (t Tool) MarshalJSON() ([]byte, error) {
	if len(t.Raw) > 0 {
		if t.CacheControl == nil {
			return append(json.RawMessage(nil), t.Raw...), nil
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(t.Raw, &obj); err != nil {
			// Not an object we can merge into; emit as-is.
			return append(json.RawMessage(nil), t.Raw...), nil
		}
		cc, err := EncodeJSONNoHTMLEscape(t.CacheControl)
		if err != nil {
			return nil, err
		}
		obj["cache_control"] = cc
		return EncodeJSONNoHTMLEscape(obj)
	}
	type standardTool struct {
		Name         string          `json:"name"`
		Description  string          `json:"description"`
		InputSchema  json.RawMessage `json:"input_schema"`
		CacheControl *CacheControl   `json:"cache_control,omitempty"`
	}
	return EncodeJSONNoHTMLEscape(standardTool{
		Name:         t.Name,
		Description:  t.Description,
		InputSchema:  t.InputSchema,
		CacheControl: t.CacheControl,
	})
}

// IsBuiltin reports whether this tool is an Anthropic built-in tool (computer-use,
// bash, text_editor, code_execution, web_search, ...) rather than a standard
// custom/function tool. Built-in tools declare a non-empty Type that is not
// "custom"/"function"; custom tools either omit type or set it to "custom".
func (t Tool) IsBuiltin() bool {
	switch t.Type {
	case "", "custom", "function":
		return false
	default:
		return true
	}
}

type CacheControl struct {
	Type string `json:"type" validate:"required,oneof=ephemeral"`
	// The time-to-live for the cache control breakpoint.
	//
	// This may be one the following values:
	//
	// 5m: 5 minutes
	// 1h: 1 hour
	// Defaults to 5m.
	TTL string `json:"ttl,omitempty"`
}

// InputSchema represents the JSON schema for tool input.
type InputSchema struct {
	Type       string                 `json:"type"`
	Properties map[string]interface{} `json:"properties,omitempty"`
	Required   []string               `json:"required,omitempty"`
}

// MessageParam represents a message in Anthropic format.
type MessageParam struct {
	Role    string         `json:"role"`
	Content MessageContent `json:"content"`
	// OutputConfig preserves the per-message output_config a real claude CLI
	// 2.1.28x attaches to mid-conversation messages (packet-verified 2026-09-24:
	// the env system message carries {"effort":"medium"}). Dropping it changes
	// the outbound body shape vs a genuine CLI request.
	OutputConfig json.RawMessage `json:"output_config,omitempty"`
}

// MessageContent supports both string and array formats.
type MessageContent struct {
	Content         *string               `json:"content,omitempty"`
	MultipleContent []MessageContentBlock `json:"multiple_content,omitempty"`
}

func (m MessageContent) ExtractTrivalBlocks(cacheControl *CacheControl) []MessageContentBlock {
	var contentBlocks []MessageContentBlock
	if m.Content != nil && *m.Content != "" {
		contentBlocks = append(contentBlocks, MessageContentBlock{
			Type:         "text",
			Text:         m.Content,
			CacheControl: cacheControl,
		})
	} else if len(m.MultipleContent) > 0 {
		for _, part := range m.MultipleContent {
			if part.Type == "text" && part.Text != nil && *part.Text != "" {
				contentBlocks = append(contentBlocks, part)
			}

			if part.Type == "image_url" {
				contentBlocks = append(contentBlocks, part)
			}
		}
	}

	return contentBlocks
}

func (c MessageContent) MarshalJSON() ([]byte, error) {
	if c.Content != nil {
		return EncodeJSONNoHTMLEscape(c.Content)
	}

	return EncodeJSONNoHTMLEscape(c.MultipleContent)
}

func (c *MessageContent) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return fmt.Errorf("content cannot be null")
	}

	var blocks []MessageContentBlock

	err := json.Unmarshal(data, &blocks)
	if err == nil {
		c.MultipleContent = blocks
		return nil
	}

	var str string

	err = json.Unmarshal(data, &str)
	if err == nil {
		c.Content = &str
		return nil
	}

	return fmt.Errorf("invalid content type")
}

// MessageContentBlock represents different types of content blocks.
type MessageContentBlock struct {
	// Any of "text", "image", "thinking", "redacted_thinking", "tool_use", "server_tool_use", "tool_result".
	Type string `json:"type"`

	// Text will be present if type is "text".
	// Use pointer to distinguish between "not set" (nil, omitted) and "set to empty" (non-nil, included).
	Text *string `json:"text,omitempty"`

	// Thinking will be present if type is "thinking".
	// Use pointer to distinguish between "not set" (nil, omitted) and "set to empty" (non-nil, included).
	Thinking *string `json:"thinking,omitempty"`

	// Signature will be present if type is "thinking".
	// Use pointer to distinguish between "not set" (nil, omitted) and "set to empty" (non-nil, included).
	Signature *string `json:"signature,omitempty"`

	// Data will be present if type is "redacted_thinking".
	Data string `json:"data,omitempty"`

	// Image will be present if type is "image".
	Source *ImageSource `json:"source,omitempty"`

	// Tool use request
	// tool_use or server_tool_use
	ID           string          `json:"id,omitempty"`
	Name         *string         `json:"name,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
	CacheControl *CacheControl   `json:"cache_control,omitempty"`

	// Tool result fields
	ToolUseID *string `json:"tool_use_id,omitempty"`
	// The content of the tool result.
	// Type can be "text" or "image".
	Content *MessageContent `json:"content,omitempty"`
	IsError *bool           `json:"is_error,omitempty"`
}

// ImageSource represents image source for Anthropic.
type ImageSource struct {
	// Type is the type of image source.
	// Available values: base64, url
	Type string `json:"type"`
	// MediaType is the media type of image.
	// Available values: image/png, image/jpeg, image/gif, image/webp
	MediaType string `json:"media_type"`

	// Data is the image data.
	// If Type is base64, Data is the base64-encoded image data.
	Data string `json:"data"`

	// URL is the URL of the image.
	// It will be present if Type is url.
	URL string `json:"url,omitempty"`

	// FileID references a provider-side uploaded file. It is present when Type is
	// "file" (used by Anthropic document blocks via the Files API).
	FileID string `json:"file_id,omitempty"`
}

// StreamEvent represents events in Anthropic streaming response.
type StreamEvent struct {
	// Any of "message_start", "message_delta", "message_stop", "content_block_start",
	// "content_block_delta", "content_block_stop".
	Type string `json:"type"`

	// Message will be present if type is "message_start".
	Message *StreamMessage `json:"message,omitempty"`

	// Index will be present if type is "content_block_start" or "content_block_delta".
	Index *int64 `json:"index,omitempty"`

	// ContentBlock will be present if type is "content_block_start".
	ContentBlock *MessageContentBlock `json:"content_block,omitempty"`

	// Delta will be present if type is "message_delta" or "content_block_delta".
	Delta *StreamDelta `json:"delta,omitempty"`

	// Error will be present if type is "error".
	Error *ErrorDetail `json:"error,omitempty"`

	Usage *Usage `json:"usage,omitempty"`
}

// StreamDelta represents delta in streaming response.
type StreamDelta struct {
	// Type is the type of delta.
	// Any of "text_delta", "input_json_delta", "citations_delta", "thinking_delta",
	// "signature_delta".
	Type *string `json:"type,omitempty"`

	// Text will be present if type is "text_delta".
	Text *string `json:"text,omitempty"`

	// PartialJSON will be present if type is "input_json_delta".
	PartialJSON *string `json:"partial_json,omitempty"`

	// Thinking will be present if type is "thinking_delta".
	Thinking *string `json:"thinking,omitempty"`

	// Signature will be present if type is "signature_delta".
	Signature *string `json:"signature,omitempty"`

	// For "message_delta"
	// Any of "end_turn", "max_tokens", "stop_sequence", "tool_use", "pause_turn",
	// "refusal".
	StopReason *string `json:"stop_reason,omitempty"`

	// For "message_delta"
	StopSequence *string `json:"stop_sequence,omitempty"`
}

// StreamMessage represents the message part of a stream event.
type StreamMessage struct {
	ID      string                `json:"id"`
	Type    string                `json:"type"`
	Role    string                `json:"role"`
	Content []MessageContentBlock `json:"content"`
	Model   string                `json:"model"`
	Usage   *Usage                `json:"usage,omitempty"`
}

// Message represents the Anthropic Messages API response format.
type Message struct {
	ID      string                `json:"id"`
	Type    string                `json:"type"`
	Role    string                `json:"role"`
	Content []MessageContentBlock `json:"content"`
	Model   string                `json:"model"`
	// Any of "end_turn", "max_tokens", "stop_sequence", "tool_use", "pause_turn",
	// "refusal".
	StopReason *string `json:"stop_reason,omitempty"`
	// Which custom stop sequence was generated, if any.
	//
	// This value will be a non-null string if one of your custom stop sequences was
	// generated.
	StopSequence *string `json:"stop_sequence,omitempty"`
	Usage        *Usage  `json:"usage,omitempty"`
}

type ErrorDetail struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// AnthropicError follow the https://platform.claude.com/docs/en/api/errors
type AnthropicError struct {
	Type       string      `json:"type,omitempty"`
	StatusCode int         `json:"-"`
	RequestID  string      `json:"request_id"`
	Error      ErrorDetail `json:"error"`
}

// Usage represents usage information in Anthropic format.
type Usage struct {
	// The number of input tokens which were used to bill.
	InputTokens int64 `json:"input_tokens,omitempty"`

	// The number of output tokens which were used.
	OutputTokens int64 `json:"output_tokens,omitempty"`

	// The number of input tokens used to create the cache entry.
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens,omitempty"`

	// The number of input tokens read from the cache.
	CacheReadInputTokens int64 `json:"cache_read_input_tokens,omitempty"`

	// Available options: standard, priority, batch
	ServiceTier string `json:"service_tier,omitempty"`
}
