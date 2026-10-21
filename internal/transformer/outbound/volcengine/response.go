package volcengine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/zhizhishu/octopus/internal/transformer/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound/openai"
)

var supportedReasoningEffortModel = map[string]bool{
	"doubao-seed-1-8-251228":      true,
	"doubao-seed-1-6-lite-251015": true,
	"doubao-seed-1-6-251015":      true,
}

type ResponseOutbound struct {
	inner openai.ResponseOutbound
}

func (o *ResponseOutbound) TransformRequest(ctx context.Context, request *model.InternalLLMRequest, baseUrl, key string) (*http.Request, error) {
	if request == nil {
		return nil, fmt.Errorf("request is nil")
	}

	// Convert to Responses API request format
	openaiReq := openai.ConvertToResponsesRequest(request)
	openaiReq.Metadata = nil // volcengine not supported
	if _, ok := supportedReasoningEffortModel[request.Model]; !ok {
		openaiReq.Reasoning = nil
	}
	responsesReq := ResponsesRequest{
		ResponsesRequest: openaiReq,
		Input:            convertToResponsesInput(openaiReq.Input),
	}
	// Map the effort ladder onto volcengine's binary thinking switch.
	//
	// xhigh/max must be enabled: before this they fell into the empty default
	// branch, Thinking stayed at its zero value, omitzero then dropped the whole
	// object — so the *strongest* effort levels sent no thinking parameter at
	// all while "low" did. That inversion is worse than a wrong budget: the
	// upstream simply never learns the client asked for deep reasoning.
	//
	// "none"/"" stay untouched (no explicit intent), and "minimal" maps to
	// disabled, matching the OpenAI semantics where minimal is the off-ramp.
	switch strings.ToLower(strings.TrimSpace(request.ReasoningEffort)) {
	case "minimal", "none":
		responsesReq.Thinking.Type = ThinkingTypeDisabled
	case "low", "medium", "high", "xhigh", "max":
		responsesReq.Thinking.Type = ThinkingTypeEnabled
	default:
	}

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
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)

	// Parse and set URL
	parsedUrl, err := url.Parse(strings.TrimSuffix(baseUrl, "/"))
	if err != nil {
		return nil, fmt.Errorf("failed to parse base url: %w", err)
	}
	parsedUrl.Path = parsedUrl.Path + "/responses"
	req.URL = parsedUrl
	req.Method = http.MethodPost

	return req, nil

}
func (o *ResponseOutbound) TransformResponse(ctx context.Context, response *http.Response) (*model.InternalLLMResponse, error) {
	return o.inner.TransformResponse(ctx, response)
}

func (o *ResponseOutbound) TransformStream(ctx context.Context, eventData []byte) (*model.InternalLLMResponse, error) {
	return o.inner.TransformStream(ctx, eventData)
}

type ResponsesRequest struct {
	*openai.ResponsesRequest
	Input    ResponsesInput `json:"input"`
	Thinking Thinking       `json:"thinking,omitzero"`
}

// MarshalJSON merges volcengine's own members (thinking) into the encoded
// inner request.
//
// This method MUST exist. The embedded *openai.ResponsesRequest has its own
// MarshalJSON, and Go promotes that method onto this outer type — so without an
// explicit override, json.Marshal(ResponsesRequest) called the INNER marshaller
// and silently dropped every field declared out here. thinking was therefore
// never sent to the upstream at all, for any effort level: low, high and max
// were all equally broken, which is why the effort switch alone could not fix it.
//
// The inner request already carries an "input" member, so the outer Input field
// happened to survive via that same key; thinking has no counterpart inside and
// vanished completely.
func (r ResponsesRequest) MarshalJSON() ([]byte, error) {
	inner, err := json.Marshal(r.ResponsesRequest)
	if err != nil {
		return nil, err
	}
	// Reuse the inner encoding verbatim so key order and the no-HTML-escape
	// behaviour stay identical to every other Responses outbound; only splice in
	// the extra member.
	members := map[string]json.RawMessage{}
	if len(inner) > 2 { // not just "{}"
		if err := json.Unmarshal(inner, &members); err != nil {
			return nil, err
		}
	}
	// Keep the converted input, which volcengine reshapes for its own API.
	if encodedInput, err := json.Marshal(r.Input); err == nil && string(encodedInput) != "null" {
		members["input"] = encodedInput
	}
	// thinking is zero-value when no explicit effort was requested; keep it out
	// of the body in that case rather than sending an empty object.
	if r.Thinking.Type != "" {
		encodedThinking, err := json.Marshal(r.Thinking)
		if err != nil {
			return nil, err
		}
		members["thinking"] = encodedThinking
	}
	return json.Marshal(members)
}

type ThinkingType string

const (
	ThinkingTypeAuto     ThinkingType = "auto"
	ThinkingTypeDisabled ThinkingType = "disabled"
	ThinkingTypeEnabled  ThinkingType = "enabled"
)

type Thinking struct {
	Type ThinkingType `json:"type"`
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
	openai.ResponsesItem
	Partial bool `json:"partial,omitempty"`
}

func convertToResponsesInput(input openai.ResponsesInput) ResponsesInput {
	result := ResponsesInput{}
	// When the source input is an OpenAI Responses raw passthrough, copy it so
	// MarshalJSON emits it verbatim instead of dropping everything to "[]".
	if len(input.Raw) > 0 && input.Text == nil && len(input.Items) == 0 {
		result.Raw = cloneRawMessage(input.Raw)
		return result
	}
	if input.Text != nil {
		result.Text = input.Text
		return result
	}

	for _, item := range input.Items {
		result.Items = append(result.Items, ResponsesItem{ResponsesItem: item})
	}
	// If the role of the last message is the assistant, needs set partial.
	// Guard against empty input (e.g. a system-only request whose system
	// message is lifted into Instructions, leaving Items empty) to avoid an
	// out-of-range panic on result.Items[-1].
	if len(result.Items) > 0 {
		idx := len(result.Items) - 1
		if result.Items[idx].Role == "assistant" {
			result.Items[idx].Partial = true
		}
	}
	return result
}

func cloneRawMessage(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	out := make([]byte, len(raw))
	copy(out, raw)
	return out
}
