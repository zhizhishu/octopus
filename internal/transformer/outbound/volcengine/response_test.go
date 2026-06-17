package volcengine

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
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
