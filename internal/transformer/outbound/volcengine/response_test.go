package volcengine

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/zhizhishu/octopus/internal/transformer/model"
)

// TestTransformRequestSystemOnlyNoPanic ensures that a request whose only
// message is a system message (which is lifted into Instructions, leaving the
// Responses input items empty) does not panic on the tail assistant/partial
// check in convertToResponsesInput.
func TestTransformRequestSystemOnlyNoPanic(t *testing.T) {
	o := &ResponseOutbound{}
	req := &model.InternalLLMRequest{
		Model: "doubao-seed-1-6-251015",
		Messages: []model.Message{
			{
				Role: "system",
				Content: model.MessageContent{
					Content: strPtr("you are a helpful assistant"),
				},
			},
		},
	}

	httpReq, err := o.TransformRequest(context.Background(), req, "https://example.com/api/v3", "test-key")
	if err != nil {
		t.Fatalf("TransformRequest returned error: %v", err)
	}

	body := readBody(t, httpReq)

	var got struct {
		Instructions string          `json:"instructions"`
		Input        json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("failed to unmarshal request body: %v\nbody: %s", err, body)
	}

	if got.Instructions != "you are a helpful assistant" {
		t.Errorf("instructions = %q, want %q", got.Instructions, "you are a helpful assistant")
	}
	// The system message is lifted into Instructions, so the Responses input has
	// no items. The point of this test is that we reach here at all (no
	// out-of-range panic); an empty nil-slice input marshals to "null" / "[]".
	if in := strings.TrimSpace(string(got.Input)); in != "null" && in != "[]" {
		t.Errorf("input = %s, want null or []", got.Input)
	}
}

// TestTransformRequestResponsesRawInputPreserved ensures that an OpenAI
// Responses raw passthrough input is not dropped when converted to the
// volcengine outbound request. Before the fix the volcengine ResponsesInput
// had no Raw field, so the raw input collapsed to "[]".
func TestTransformRequestResponsesRawInputPreserved(t *testing.T) {
	rawInput := json.RawMessage(`[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello volcengine"}]}]`)

	o := &ResponseOutbound{}
	req := &model.InternalLLMRequest{
		Model:             "doubao-seed-1-6-251015",
		RawAPIFormat:      model.APIFormatOpenAIResponse,
		ResponsesInputRaw: rawInput,
	}

	httpReq, err := o.TransformRequest(context.Background(), req, "https://example.com/api/v3", "test-key")
	if err != nil {
		t.Fatalf("TransformRequest returned error: %v", err)
	}

	body := readBody(t, httpReq)

	var got struct {
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("failed to unmarshal request body: %v\nbody: %s", err, body)
	}

	if !jsonEqual(t, got.Input, rawInput) {
		t.Errorf("input was not preserved verbatim.\n got: %s\nwant: %s", got.Input, rawInput)
	}
}

func readBody(t *testing.T, req *http.Request) []byte {
	t.Helper()
	if req.Body == nil {
		t.Fatalf("request body is nil")
	}
	b, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("failed to read request body: %v", err)
	}
	return b
}

func strPtr(s string) *string { return &s }

func jsonEqual(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		t.Fatalf("failed to unmarshal %s: %v", a, err)
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		t.Fatalf("failed to unmarshal %s: %v", b, err)
	}
	am, err := json.Marshal(av)
	if err != nil {
		t.Fatalf("failed to remarshal: %v", err)
	}
	bm, err := json.Marshal(bv)
	if err != nil {
		t.Fatalf("failed to remarshal: %v", err)
	}
	return string(am) == string(bm)
}

// xhigh/max used to fall into an empty default branch, leaving Thinking at its
// zero value so omitzero dropped the whole object. The strongest effort levels
// then sent no thinking parameter at all while "low" did — an inversion where
// asking for more got you less.
func TestVolcengineThinkingEnabledForStrongEfforts(t *testing.T) {
	for _, effort := range []string{"low", "medium", "high", "xhigh", "max"} {
		typ := volcengineThinkingTypeForTest(t, effort)
		if typ != string(ThinkingTypeEnabled) {
			t.Fatalf("effort %q produced thinking.type=%q, want %q", effort, typ, string(ThinkingTypeEnabled))
		}
	}

	// minimal stays the explicit off-ramp.
	if typ := volcengineThinkingTypeForTest(t, "minimal"); typ != string(ThinkingTypeDisabled) {
		t.Fatalf("minimal produced thinking.type=%q, want %q", typ, string(ThinkingTypeDisabled))
	}

	// An unset effort must not be treated as an explicit request either way.
	if typ := volcengineThinkingTypeForTest(t, ""); typ != "" {
		t.Fatalf("empty effort produced thinking.type=%q, want empty", typ)
	}
}

// volcengineThinkingTypeForTest drives a real request transform and reports the
// resulting thinking.type, so the assertion covers the actual wire body rather
// than a copy of the switch.
func volcengineThinkingTypeForTest(t *testing.T, effort string) string {
	t.Helper()
	out := &ResponseOutbound{}
	req, err := out.TransformRequest(context.Background(), &model.InternalLLMRequest{
		Model:           "doubao-seed-1-6-251015",
		ReasoningEffort: effort,
	}, "https://upstream.example", "key")
	if err != nil {
		t.Fatalf("transform failed for effort %q: %v", effort, err)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var parsed struct {
		Thinking struct {
			Type string `json:"type"`
		} `json:"thinking"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	return parsed.Thinking.Type
}
