package redact

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// testPEM returns a synthetic multi-line private-key block that the G (gitleaks)
// detector matches as ONE span, so the restored raw value contains real newlines.
func testPEM() string {
	return "-----BEGIN PRIVATE KEY-----\n" +
		strings.Repeat("MIIEvQIBADANBgkqhkiG9w0BAQEFBAAKCAQEA", 2) +
		"\n-----END PRIVATE KEY-----"
}

// chatToolArgsBody builds a chat completion body whose tool-call arguments embed
// the given inner JSON text.
func chatToolArgsBody(t *testing.T, inner string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"choices": []any{map[string]any{
			"message": map[string]any{
				"tool_calls": []any{map[string]any{
					"function": map[string]any{"name": "store", "arguments": inner},
				}},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// chatToolArgsFrom unmarshals a chat completion body and returns the first
// tool-call arguments string.
func chatToolArgsFrom(t *testing.T, body []byte) string {
	t.Helper()
	var resp struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					Function struct {
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("outer body not valid JSON: %v", err)
	}
	if len(resp.Choices) == 0 || len(resp.Choices[0].Message.ToolCalls) == 0 {
		t.Fatalf("no tool call in %s", body)
	}
	return resp.Choices[0].Message.ToolCalls[0].Function.Arguments
}

// chatSseArgs concatenates the tool_calls function.arguments deltas from emitted
// chat SSE events — what the client reassembles.
func chatSseArgs(t *testing.T, sse string) string {
	t.Helper()
	var combined strings.Builder
	for _, block := range strings.Split(strings.TrimSpace(sse), "\n\n") {
		line := strings.TrimPrefix(block, "data: ")
		if line == block {
			continue
		}
		var ev struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Function struct {
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("bad event %q: %v", line, err)
		}
		for _, ch := range ev.Choices {
			for _, tc := range ch.Delta.ToolCalls {
				combined.WriteString(tc.Function.Arguments)
			}
		}
	}
	return combined.String()
}

// chatArgsDelta builds a chat SSE delta event carrying one tool_calls arguments
// fragment.
func chatArgsDelta(t *testing.T, args string) string {
	t.Helper()
	return sseEvent(t, map[string]any{
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{
				"tool_calls": []any{map[string]any{
					"index":    0,
					"function": map[string]any{"arguments": args},
				}},
			},
		}},
	})
}

// sseDataDeltas concatenates the top-level string .delta fields from emitted SSE
// events (responses function_call_arguments deltas / custom tool input deltas).
func sseDataDeltas(t *testing.T, sse string) string {
	t.Helper()
	var combined strings.Builder
	for _, block := range strings.Split(strings.TrimSpace(sse), "\n\n") {
		for _, line := range strings.Split(block, "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var ev struct {
				Delta string `json:"delta"`
			}
			if err := json.Unmarshal([]byte(line[6:]), &ev); err != nil {
				t.Fatalf("bad data line %q: %v", line, err)
			}
			combined.WriteString(ev.Delta)
		}
	}
	return combined.String()
}

// assertPemArgs asserts the reassembled arguments text is valid inner JSON whose
// pem leaf round-trips the original multi-line PEM with ESCAPED (not raw) newlines.
func assertPemArgs(t *testing.T, args, pem string) {
	t.Helper()
	if !json.Valid([]byte(args)) {
		t.Fatalf("arguments must be valid inner JSON, got %q", args)
	}
	var inner struct {
		Pem string `json:"pem"`
	}
	if err := json.Unmarshal([]byte(args), &inner); err != nil {
		t.Fatalf("unmarshal arguments: %v", err)
	}
	if inner.Pem != pem {
		t.Fatalf("pem round-trip mismatch:\ngot  %q\nwant %q", inner.Pem, pem)
	}
	if !strings.Contains(args, "\\n") {
		t.Fatalf("arguments should contain escaped newlines, got %q", args)
	}
	if strings.Contains(args, "\n") {
		t.Fatalf("arguments must not contain raw newlines, got %q", args)
	}
}

// redactPEM registers the PEM in the session token map and returns its placeholder.
func redactPEM(t *testing.T, s *Session, protocol string) (token, pem string) {
	t.Helper()
	pem = testPEM()
	var req []byte
	var err error
	switch protocol {
	case "anthropic_messages":
		req, err = json.Marshal(map[string]any{"model": "c", "max_tokens": 64, "messages": []any{map[string]any{"role": "user", "content": pem}}})
	default:
		req, err = json.Marshal(map[string]string{"input": pem})
	}
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.RedactJSONBody(req)
	if err != nil {
		t.Fatalf("redact request: %v", err)
	}
	return extractToken(t, out), pem
}

// TestRestoreJSONBodyNestedToolArgsEscaped: a multi-line secret restored into a
// tool-arguments JSON string must be re-embedded ESCAPED, so the arguments text
// stays valid inner JSON (the pre-fix behavior inserted raw newlines and broke it).
func TestRestoreJSONBodyNestedToolArgsEscaped(t *testing.T) {
	e := newTestEngine(t)
	s, err := e.NewSession("G", "openai_chat", false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	pem := testPEM()
	inner, err := json.Marshal(map[string]string{"pem": pem})
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.RedactJSONBody(chatToolArgsBody(t, string(inner)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "{{Redact:") {
		t.Fatalf("gitleaks private-key rule did not fire: %s", out)
	}
	restored, err := s.RestoreJSONBody(out)
	if err != nil {
		t.Fatal(err)
	}
	assertPemArgs(t, chatToolArgsFrom(t, restored), pem)
}

// TestRedactRestoreRoundTripToolArgsIdentity: a tool-arguments slot whose secret has
// no JSON-special characters must round-trip semantically identical — the parse
// guard must not over-escape ordinary restores.
func TestRedactRestoreRoundTripToolArgsIdentity(t *testing.T) {
	e := newTestEngine(t)
	s, err := e.NewSession("E", "openai_chat", false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	body := chatToolArgsBody(t, `{"email":"a@example.com"}`)
	out, err := s.RedactJSONBody(body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "{{Redact:") {
		t.Fatalf("email detector did not fire: %s", out)
	}
	restored, err := s.RestoreJSONBody(out)
	if err != nil {
		t.Fatal(err)
	}
	var want, got any
	if err := json.Unmarshal(body, &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(restored, &got); err != nil {
		t.Fatalf("restored body not valid JSON: %v", err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("round trip not identity:\nwant %s\ngot  %s", body, restored)
	}
}

// TestSseRestoreToolArgsJsonEscaped: chat tool_calls arguments streamed in one delta
// must restore with escaped re-embedding.
func TestSseRestoreToolArgsJsonEscaped(t *testing.T) {
	e := newTestEngine(t)
	s, _ := e.NewSession("G", "openai_chat", false)
	defer s.Close()
	token, pem := redactPEM(t, s, "openai_chat")
	ev1 := chatArgsDelta(t, `{"pem":"`+token+`"}`)
	r, err := s.NewSseRestorer()
	if err != nil {
		t.Fatal(err)
	}
	got := drainAll(t, r, []string{ev1})
	if strings.Contains(got, "{{Redact:") {
		t.Fatalf("placeholder leaked downstream: %s", got)
	}
	assertPemArgs(t, chatSseArgs(t, got), pem)
}

// TestSseRestoreToolArgsJsonEscapedTokenSplit: the placeholder split across two
// arguments deltas — the first ingest must buffer, and the reassembled arguments
// must stay valid inner JSON.
func TestSseRestoreToolArgsJsonEscapedTokenSplit(t *testing.T) {
	e := newTestEngine(t)
	s, _ := e.NewSession("G", "openai_chat", false)
	defer s.Close()
	token, pem := redactPEM(t, s, "openai_chat")
	cut := 20
	ev1 := chatArgsDelta(t, `{"pem":"`+token[:cut])
	ev2 := chatArgsDelta(t, token[cut:]+`"}`)
	r, err := s.NewSseRestorer()
	if err != nil {
		t.Fatal(err)
	}
	got1, err := r.Ingest(ev1)
	if err != nil {
		t.Fatal(err)
	}
	if got1 != "" {
		t.Fatalf("first ingest should buffer the partial placeholder, got %q", got1)
	}
	got2, err := r.Ingest(ev2)
	if err != nil {
		t.Fatal(err)
	}
	tail, err := r.Finish()
	if err != nil {
		t.Fatal(err)
	}
	assertPemArgs(t, chatSseArgs(t, got1+got2+tail), pem)
}

// TestSseRestoreToolArgsJsonEscapedMidJsonFlush: the placeholder completes while the
// arguments JSON is still open and the JSON closes in a LATER delta. The channel
// flushes mid-JSON — the fragment-escaped restore must still keep the reassembled
// arguments valid (a parse-guarded flush would fall back to raw restore here and
// break the inner JSON).
func TestSseRestoreToolArgsJsonEscapedMidJsonFlush(t *testing.T) {
	e := newTestEngine(t)
	s, _ := e.NewSession("G", "openai_chat", false)
	defer s.Close()
	token, pem := redactPEM(t, s, "openai_chat")
	cut := 20
	ev1 := chatArgsDelta(t, `{"pem":"`+token[:cut])
	ev2 := chatArgsDelta(t, token[cut:])
	ev3 := chatArgsDelta(t, `"}`)
	r, err := s.NewSseRestorer()
	if err != nil {
		t.Fatal(err)
	}
	got := drainAll(t, r, []string{ev1, ev2, ev3})
	assertPemArgs(t, chatSseArgs(t, got), pem)
}

// TestSseRestoreResponsesFunctionCallArgsEscaped: responses
// response.function_call_arguments.delta events must restore escaped.
func TestSseRestoreResponsesFunctionCallArgsEscaped(t *testing.T) {
	e := newTestEngine(t)
	s, _ := e.NewSession("G", "openai_responses", false)
	defer s.Close()
	token, pem := redactPEM(t, s, "openai_responses")
	ev := "event: response.function_call_arguments.delta\ndata: " + mustJSON(t, map[string]any{"type": "response.function_call_arguments.delta", "output_index": 0, "content_index": 0, "item_id": "fc_1", "delta": `{"pem":"` + token + `"}`})
	r, err := s.NewSseRestorer()
	if err != nil {
		t.Fatal(err)
	}
	got := drainAll(t, r, []string{ev})
	assertPemArgs(t, sseDataDeltas(t, got), pem)
}

// TestSseRestoreAnthropicPartialJsonEscaped: anthropic content_block_delta
// input_json_delta.partial_json must restore escaped.
func TestSseRestoreAnthropicPartialJsonEscaped(t *testing.T) {
	e := newTestEngine(t)
	s, _ := e.NewSession("G", "anthropic_messages", false)
	defer s.Close()
	token, pem := redactPEM(t, s, "anthropic_messages")
	ev := "event: content_block_delta\ndata: " + mustJSON(t, map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": `{"pem":"` + token + `"}`}})
	r, err := s.NewSseRestorer()
	if err != nil {
		t.Fatal(err)
	}
	got := drainAll(t, r, []string{ev})
	var combined strings.Builder
	for _, block := range strings.Split(strings.TrimSpace(got), "\n\n") {
		for _, line := range strings.Split(block, "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var ev struct {
				Delta struct {
					PartialJSON string `json:"partial_json"`
				} `json:"delta"`
			}
			if err := json.Unmarshal([]byte(line[6:]), &ev); err != nil {
				t.Fatalf("bad data line %q: %v", line, err)
			}
			combined.WriteString(ev.Delta.PartialJSON)
		}
	}
	assertPemArgs(t, combined.String(), pem)
}

// TestSseRestoreOutputItemDoneArgsEscaped: a complete arguments string inside
// response.output_item.done (restoreCompleteStrings path) must restore escaped.
func TestSseRestoreOutputItemDoneArgsEscaped(t *testing.T) {
	e := newTestEngine(t)
	s, _ := e.NewSession("G", "openai_responses", false)
	defer s.Close()
	token, pem := redactPEM(t, s, "openai_responses")
	ev := "event: response.output_item.done\ndata: " + mustJSON(t, map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"type": "function_call", "id": "fc_1", "name": "store", "arguments": `{"pem":"` + token + `"}`}})
	r, err := s.NewSseRestorer()
	if err != nil {
		t.Fatal(err)
	}
	got := drainAll(t, r, []string{ev})
	var args string
	found := false
	for _, block := range strings.Split(strings.TrimSpace(got), "\n\n") {
		for _, line := range strings.Split(block, "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var ev struct {
				Item struct {
					Arguments string `json:"arguments"`
				} `json:"item"`
			}
			if err := json.Unmarshal([]byte(line[6:]), &ev); err != nil {
				t.Fatalf("bad data line %q: %v", line, err)
			}
			if ev.Item.Arguments != "" {
				args = ev.Item.Arguments
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("no item.arguments in output: %s", got)
	}
	assertPemArgs(t, args, pem)
}

// TestSseRestoreCustomToolInputFreeTextUnchanged: custom tool input is FREE TEXT —
// the restored raw value must be re-embedded byte-identically (real newlines stay
// real newlines; no JSON escaping is added).
func TestSseRestoreCustomToolInputFreeTextUnchanged(t *testing.T) {
	e := newTestEngine(t)
	s, _ := e.NewSession("G", "openai_responses", false)
	defer s.Close()
	token, pem := redactPEM(t, s, "openai_responses")
	ev := "event: response.custom_tool_call_input.delta\ndata: " + mustJSON(t, map[string]any{"type": "response.custom_tool_call_input.delta", "output_index": 0, "content_index": 0, "item_id": "ct_1", "delta": "free text " + token + " tail"})
	r, err := s.NewSseRestorer()
	if err != nil {
		t.Fatal(err)
	}
	got := drainAll(t, r, []string{ev})
	want := "free text " + pem + " tail"
	if combined := sseDataDeltas(t, got); combined != want {
		t.Fatalf("custom tool input must restore byte-identically:\ngot  %q\nwant %q", combined, want)
	}
}
