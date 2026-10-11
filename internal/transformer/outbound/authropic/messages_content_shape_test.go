package authropic

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"testing"

	inboundAnthropic "github.com/bestruirui/octopus/internal/transformer/inbound/anthropic"
)

// Golden regression for the "outbound body must keep the caller's shape" rule (Shape 铁律).
//
// Field evidence this pins (same constructed body sent twice: once to a transparent capture port,
// once through the relay):
//
//	[0] user      BLOCKS   [1] asst BLOCKS   [2] user BLOCKS   [3] asst STRING
//	[4] user      BLOCKS   [5] asst BLOCKS(thinking,text)      [6] user BLOCKS
//
// transparent leg: every message came out exactly as it went in.
// relay leg:       [0][1][2][4][6] came out as STRING — a single text block was collapsed into a
// bare string — while [5] (two blocks) stayed an array. That selectivity is what proves it was a
// targeted normalization, not a whole-body re-serialization.
//
// No upstream and no TLS termination port are needed: this drives the same inbound -> internal ->
// outbound transformers the relay uses and compares the emitted `content` shape field by field.
func TestAnthropicRoundTripKeepsContentBlockShape(t *testing.T) {
	const body = `{
	  "model": "claude-sonnet-example",
	  "max_tokens": 64,
	  "messages": [
	    {"role":"user","content":[{"type":"text","text":"BLOCK-USER-0"}]},
	    {"role":"assistant","content":[{"type":"text","text":"BLOCK-ASST-1"}]},
	    {"role":"user","content":[{"type":"text","text":"BLOCK-USER-2"}]},
	    {"role":"assistant","content":"STRING-ASST-3"},
	    {"role":"user","content":[{"type":"text","text":"BLOCK-USER-4"}]},
	    {"role":"assistant","content":[{"type":"thinking","thinking":"THINK-5","signature":"sig-5"},{"type":"text","text":"BLOCK-ASST-5"}]},
	    {"role":"user","content":[{"type":"text","text":"BLOCK-USER-6"}]}
	  ]
	}`

	want := messageContents(t, []byte(body))

	internalReq, err := (&inboundAnthropic.MessagesInbound{}).TransformRequest(context.Background(), []byte(body))
	if err != nil {
		t.Fatalf("inbound TransformRequest error: %v", err)
	}
	raw, err := json.Marshal(convertToAnthropicRequest(internalReq))
	if err != nil {
		t.Fatalf("marshal outbound request error: %v", err)
	}
	got := messageContents(t, raw)

	if len(got) != len(want) {
		t.Fatalf("outbound carries %d messages, caller sent %d", len(got), len(want))
	}
	for i := range want {
		wantKind, gotKind := jsonKind(want[i]), jsonKind(got[i])
		if wantKind != gotKind {
			t.Errorf("messages[%d].content shape changed: caller sent %s, outbound carries %s (caller=%s outbound=%s)",
				i, wantKind, gotKind, compact(t, want[i]), compact(t, got[i]))
			continue
		}
		if !sameJSON(t, want[i], got[i]) {
			t.Errorf("messages[%d].content changed: caller sent %s, outbound carries %s",
				i, compact(t, want[i]), compact(t, got[i]))
		}
	}
}

// TestAnthropicSingleTextBlockIsNotCollapsed is the minimal repro of the reported defect: one
// inbound text block array must reach the upstream as an array, byte for byte.
func TestAnthropicSingleTextBlockIsNotCollapsed(t *testing.T) {
	const body = `{"model":"claude-sonnet-example","max_tokens":16,"messages":[{"role":"user","content":[{"type":"text","text":"SHAPE-PROBE-20261011010700"}]}]}`

	internalReq, err := (&inboundAnthropic.MessagesInbound{}).TransformRequest(context.Background(), []byte(body))
	if err != nil {
		t.Fatalf("inbound TransformRequest error: %v", err)
	}
	raw, err := json.Marshal(convertToAnthropicRequest(internalReq))
	if err != nil {
		t.Fatalf("marshal outbound request error: %v", err)
	}
	got := messageContents(t, raw)
	if len(got) != 1 {
		t.Fatalf("outbound carries %d messages, want 1", len(got))
	}
	if kind := jsonKind(got[0]); kind != "array" {
		t.Fatalf("a single text block was collapsed to %s outbound: %s", kind, compact(t, got[0]))
	}
	want := messageContents(t, []byte(body))[0]
	if !bytes.Equal(compactBytes(t, want), compactBytes(t, got[0])) {
		t.Fatalf("outbound content is not the caller's content: caller=%s outbound=%s",
			compact(t, want), compact(t, got[0]))
	}
}

func messageContents(t *testing.T, raw []byte) []json.RawMessage {
	t.Helper()
	var decoded struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode messages: %v (body=%s)", err, string(raw))
	}
	out := make([]json.RawMessage, 0, len(decoded.Messages))
	for _, m := range decoded.Messages {
		out = append(out, m.Content)
	}
	return out
}

// jsonKind names the JSON shape a caller's content field had, so a shape change reads plainly in
// the failure output ("array" -> "string") instead of as a diff nobody can parse.
func jsonKind(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	switch {
	case len(trimmed) == 0:
		return "absent"
	case trimmed[0] == '[':
		return "array"
	case trimmed[0] == '"':
		return "string"
	case trimmed[0] == '{':
		return "object"
	default:
		return "other"
	}
}

func sameJSON(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		t.Fatalf("decode caller content: %v", err)
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		t.Fatalf("decode outbound content: %v", err)
	}
	return reflect.DeepEqual(av, bv)
}

func compactBytes(t *testing.T, raw json.RawMessage) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatalf("compact %s: %v", string(raw), err)
	}
	return buf.Bytes()
}

func compact(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	return string(compactBytes(t, raw))
}
