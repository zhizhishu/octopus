package redact

import (
	"encoding/json"
	"strings"
	"testing"
)

// sseEvent builds one SSE event block (without the trailing blank line).
func sseEvent(t *testing.T, payload any) string {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return "data: " + string(b)
}

// drainAll feeds events into the restorer and returns everything it emits.
func drainAll(t *testing.T, r *SseRestorer, events []string) string {
	t.Helper()
	var out strings.Builder
	for _, ev := range events {
		got, err := r.Ingest(ev)
		if err != nil {
			t.Fatalf("ingest: %v", err)
		}
		out.WriteString(got)
	}
	tail, err := r.Finish()
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	out.WriteString(tail)
	return out.String()
}

// extractToken pulls the first placeholder from a redacted body.
func extractToken(t *testing.T, body []byte) string {
	t.Helper()
	s := string(body)
	a := strings.Index(s, "{{Redact:")
	if a < 0 {
		t.Fatalf("no token in %s", s)
	}
	b := strings.Index(s[a:], "}}") + a + 2
	return s[a:b]
}

// TestSseRestoreOpenAIChatDeltaSplit ports Cosy's "OpenAI Chat SSE restores a
// placeholder split across logical SSE delta events": the model streams the
// placeholder in two content deltas; the restorer must buffer the first half,
// then emit both events with the secret restored.
func TestSseRestoreOpenAIChatDeltaSplit(t *testing.T) {
	e := newTestEngine(t)
	s, _ := e.NewSession("E", "openai_chat", false)
	defer s.Close()
	out, err := s.RedactJSONBody([]byte(`{"model":"g","stream":true,"messages":[{"role":"user","content":"a@example.com"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	token := extractToken(t, out)
	cut := 17
	ev1 := sseEvent(t, map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "before " + token[:cut]}}}})
	ev2 := sseEvent(t, map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": token[cut:] + " after"}}}})

	r, err := s.NewSseRestorer()
	if err != nil {
		t.Fatal(err)
	}
	got := drainAll(t, r, []string{ev1, ev2})
	if strings.Contains(got, "{{Redact:") {
		t.Fatalf("placeholder leaked downstream: %s", got)
	}
	// concatenate the delta contents from the emitted events
	var combined strings.Builder
	for _, block := range strings.Split(strings.TrimSpace(got), "\n\n") {
		var ev struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		line := strings.TrimPrefix(block, "data: ")
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("bad event %q: %v", line, err)
		}
		if len(ev.Choices) > 0 {
			combined.WriteString(ev.Choices[0].Delta.Content)
		}
	}
	if combined.String() != "before a@example.com after" {
		t.Fatalf("combined = %q", combined.String())
	}
}

// TestSseRestoreResponsesDeltaSplit ports the Responses variant: output_text.delta
// events carrying the placeholder in two pieces.
func TestSseRestoreResponsesDeltaSplit(t *testing.T) {
	e := newTestEngine(t)
	s, _ := e.NewSession("E", "openai_responses", false)
	defer s.Close()
	out, err := s.RedactJSONBody([]byte(`{"model":"g","stream":true,"input":"a@example.com"}`))
	if err != nil {
		t.Fatal(err)
	}
	token := extractToken(t, out)
	cut := 31
	ev1 := "event: response.output_text.delta\ndata: " + mustJSON(t, map[string]any{"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "delta": token[:cut]})
	ev2 := "event: response.output_text.delta\ndata: " + mustJSON(t, map[string]any{"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "delta": token[cut:]})

	r, _ := s.NewSseRestorer()
	got := drainAll(t, r, []string{ev1, ev2})
	var combined strings.Builder
	for _, block := range strings.Split(strings.TrimSpace(got), "\n\n") {
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "data: ") {
				var ev struct {
					Delta string `json:"delta"`
				}
				if err := json.Unmarshal([]byte(line[6:]), &ev); err != nil {
					t.Fatalf("bad data line %q: %v", line, err)
				}
				combined.WriteString(ev.Delta)
			}
		}
	}
	if combined.String() != "a@example.com" {
		t.Fatalf("combined = %q", combined.String())
	}
}

// TestSseRestoreAnthropicDeltaSplit ports the Anthropic variant: content_block_delta
// text_delta events carrying the placeholder in two pieces.
func TestSseRestoreAnthropicDeltaSplit(t *testing.T) {
	e := newTestEngine(t)
	s, _ := e.NewSession("E", "anthropic_messages", false)
	defer s.Close()
	out, err := s.RedactJSONBody([]byte(`{"model":"c","stream":true,"max_tokens":20,"messages":[{"role":"user","content":"a@example.com"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	token := extractToken(t, out)
	cut := 9
	ev1 := "event: content_block_delta\ndata: " + mustJSON(t, map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": token[:cut]}})
	ev2 := "event: content_block_delta\ndata: " + mustJSON(t, map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": token[cut:]}})

	r, _ := s.NewSseRestorer()
	got := drainAll(t, r, []string{ev1, ev2})
	var combined strings.Builder
	for _, block := range strings.Split(strings.TrimSpace(got), "\n\n") {
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "data: ") {
				var ev struct {
					Delta struct {
						Text string `json:"text"`
					} `json:"delta"`
				}
				if err := json.Unmarshal([]byte(line[6:]), &ev); err != nil {
					t.Fatalf("bad data line %q: %v", line, err)
				}
				combined.WriteString(ev.Delta.Text)
			}
		}
	}
	if combined.String() != "a@example.com" {
		t.Fatalf("combined = %q", combined.String())
	}
}

// TestSseRestorerPassthroughNothingRedacted: with zero redactions the restorer is a
// pass-through and events flow through untouched.
func TestSseRestorerPassthroughNothingRedacted(t *testing.T) {
	e := newTestEngine(t)
	s, _ := e.NewSession("E", "openai_chat", false)
	defer s.Close()
	ev := sseEvent(t, map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "hello"}}}})
	r, _ := s.NewSseRestorer()
	got, err := r.Ingest(ev)
	if err != nil {
		t.Fatal(err)
	}
	if got != ev {
		t.Fatalf("passthrough altered event: %q", got)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
