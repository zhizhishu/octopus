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

// TestSseRestoreToolArgsJsonEscapedEmptyFirstDelta: OpenAI's name-bearing first
// tool-call chunk carries arguments:"" — the channel is created on that empty
// fragment, so the JSON-args marking must re-check on later appends instead of
// deciding once on the first fragment (a one-shot test on "" would leave the
// channel unmarked for the whole stream and restore raw, re-breaking the inner
// JSON).
func TestSseRestoreToolArgsJsonEscapedEmptyFirstDelta(t *testing.T) {
	e := newTestEngine(t)
	s, _ := e.NewSession("G", "openai_chat", false)
	defer s.Close()
	token, pem := redactPEM(t, s, "openai_chat")
	cut := 20
	start := sseEvent(t, map[string]any{
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{
				"tool_calls": []any{map[string]any{
					"index": 0,
					"id":    "call_1",
					"type":  "function",
					"function": map[string]any{
						"name":      "store",
						"arguments": "",
					},
				}},
			},
		}},
	})
	ev2 := chatArgsDelta(t, `{"pem":"`+token[:cut])
	ev3 := chatArgsDelta(t, token[cut:]+`"}`)
	r, err := s.NewSseRestorer()
	if err != nil {
		t.Fatal(err)
	}
	got1, err := r.Ingest(start)
	if err != nil {
		t.Fatal(err)
	}
	got2, err := r.Ingest(ev2)
	if err != nil {
		t.Fatal(err)
	}
	if got2 != "" {
		t.Fatalf("second ingest should buffer the partial placeholder, got %q", got2)
	}
	got3, err := r.Ingest(ev3)
	if err != nil {
		t.Fatal(err)
	}
	tail, err := r.Finish()
	if err != nil {
		t.Fatal(err)
	}
	assertPemArgs(t, chatSseArgs(t, got1+got2+got3+tail), pem)
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

// extractAllTokens pulls every placeholder from a redacted body, in order.
func extractAllTokens(t *testing.T, body []byte) []string {
	t.Helper()
	s := string(body)
	var tokens []string
	for {
		a := strings.Index(s, "{{Redact:")
		if a < 0 {
			break
		}
		b := strings.Index(s[a:], "}}") + a + 2
		tokens = append(tokens, s[a:b])
		s = s[b:]
	}
	if len(tokens) == 0 {
		t.Fatalf("no tokens in %s", body)
	}
	return tokens
}

// chatToolArgsByIndex reassembles tool_calls arguments per tool_call index — the
// way a real client reassembles them. Concatenating ALL tool arguments (like
// chatSseArgs) cannot detect two interleaved tools cross-contaminating each other.
func chatToolArgsByIndex(t *testing.T, sse string) map[int]string {
	t.Helper()
	out := map[int]string{}
	for _, block := range strings.Split(strings.TrimSpace(sse), "\n\n") {
		line := strings.TrimPrefix(block, "data: ")
		if line == block {
			continue
		}
		var ev struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Index    int `json:"index"`
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
				out[tc.Index] += tc.Function.Arguments
			}
		}
	}
	return out
}

// chatToolDelta builds one chat SSE delta event carrying an arguments fragment for
// one tool call (the element's array position is always 0, as real chunks do).
func chatToolDelta(t *testing.T, toolIndex int, args string) string {
	t.Helper()
	return sseEvent(t, map[string]any{
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{
				"tool_calls": []any{map[string]any{
					"index":    toolIndex,
					"function": map[string]any{"arguments": args},
				}},
			},
		}},
	})
}

// chatToolStart builds the name-bearing first chunk for one tool (arguments:"").
func chatToolStart(t *testing.T, toolIndex int, id string) string {
	t.Helper()
	return sseEvent(t, map[string]any{
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{
				"tool_calls": []any{map[string]any{
					"index":    toolIndex,
					"id":       id,
					"type":     "function",
					"function": map[string]any{"name": "store", "arguments": ""},
				}},
			},
		}},
	})
}

// TestSseRestoreInterleavedToolCallsKeepOwnArgs: two tool calls interleaved across
// events, each carrying a different multi-line secret split across fragments. Chunks
// hold ONE tool_call element whose array position is always 0, so the restore channel
// must key on the element's own protocol index — array-position keying merges both
// tools into one channel and cross-contaminates their arguments.
func TestSseRestoreInterleavedToolCallsKeepOwnArgs(t *testing.T) {
	e := newTestEngine(t)
	s, _ := e.NewSession("G", "openai_chat", false)
	defer s.Close()
	pemA := testPEM()
	pemB := "-----BEGIN PRIVATE KEY-----\n" +
		strings.Repeat("MIIEvQIBADANBgkqhkiG9w0BAQEFBAAKCAQEA", 3) +
		"\n-----END PRIVATE KEY-----"
	body, err := json.Marshal(map[string]any{"input": []string{pemA, pemB}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.RedactJSONBody(body)
	if err != nil {
		t.Fatalf("redact request: %v", err)
	}
	toks := extractAllTokens(t, out)
	if len(toks) != 2 {
		t.Fatalf("expected two placeholders, got %d in %s", len(toks), out)
	}
	tokA, tokB := toks[0], toks[1]
	cut := 20
	events := []string{
		chatToolStart(t, 0, "call_a"),
		chatToolStart(t, 1, "call_b"),
		chatToolDelta(t, 0, `{"pem":"`+tokA[:cut]),
		chatToolDelta(t, 1, `{"pem":"`+tokB[:cut]),
		chatToolDelta(t, 0, tokA[cut:]+`"}`),
		chatToolDelta(t, 1, tokB[cut:]+`"}`),
	}
	r, err := s.NewSseRestorer()
	if err != nil {
		t.Fatal(err)
	}
	got := drainAll(t, r, events)
	if strings.Contains(got, "{{Redact:") {
		t.Fatalf("placeholder leaked downstream: %s", got)
	}
	byIndex := chatToolArgsByIndex(t, got)
	if len(byIndex) != 2 {
		t.Fatalf("expected two tool calls, got %d: %v", len(byIndex), byIndex)
	}
	assertPemArgs(t, byIndex[0], pemA)
	assertPemArgs(t, byIndex[1], pemB)
}

// TestSseRestoreTwoChoicesToolZeroIsolated: two choices each carrying tool_call
// index 0, plus a whitespace-only fragment and head/mid/tail placeholder splits.
// The choice-scoped channel prefix must keep the choices isolated, and every split
// variant must reassemble to the same final value.
func TestSseRestoreTwoChoicesToolZeroIsolated(t *testing.T) {
	e := newTestEngine(t)
	s, _ := e.NewSession("G", "openai_chat", false)
	defer s.Close()
	token, pem := redactPEM(t, s, "openai_chat")
	cut := 20
	choiceDelta := func(choice int, args string) string {
		return sseEvent(t, map[string]any{
			"choices": []any{map[string]any{
				"index": choice,
				"delta": map[string]any{
					"tool_calls": []any{map[string]any{
						"index":    0,
						"function": map[string]any{"arguments": args},
					}},
				},
			}},
		})
	}
	events := []string{
		choiceDelta(0, ""),
		choiceDelta(1, ""),
		choiceDelta(0, `{"pem":"`+token[:cut]),
		// A whitespace-only fragment of its own: the JSON-args marking must re-check
		// on later appends (leading whitespace precedes the '{' in the next fragment).
		choiceDelta(1, "   "),
		choiceDelta(0, token[cut:]),
		choiceDelta(1, `{"pem":"`+token[:cut]),
		choiceDelta(0, `"}`),
		choiceDelta(1, token[cut:]+`"}`),
	}
	r, err := s.NewSseRestorer()
	if err != nil {
		t.Fatal(err)
	}
	got := drainAll(t, r, events)
	if strings.Contains(got, "{{Redact:") {
		t.Fatalf("placeholder leaked downstream: %s", got)
	}
	byChoice := map[int]string{}
	for _, block := range strings.Split(strings.TrimSpace(got), "\n\n") {
		line := strings.TrimPrefix(block, "data: ")
		if line == block {
			continue
		}
		var ev struct {
			Choices []struct {
				Index int `json:"index"`
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
				byChoice[ch.Index] += tc.Function.Arguments
			}
		}
	}
	if len(byChoice) != 2 {
		t.Fatalf("expected two choices, got %d: %v", len(byChoice), byChoice)
	}
	assertPemArgs(t, byChoice[0], pem)
	assertPemArgs(t, byChoice[1], pem)
}

// TestRestoreToolArgsNonPlaceholderBytesUntouched: the fragment-escaped restore must
// only rewrite the placeholder span. Whatever the redaction side emitted — including
// big-integer literals, duplicate keys, runs of whitespace and pre-existing escapes
// (the redaction side's own nested-JSON re-serialization limits are out of scope
// here) — the RESTORE stage must hand it back byte-for-byte with only the token
// span replaced, so this asserts on the raw string, not a re-parsed object.
func TestRestoreToolArgsNonPlaceholderBytesUntouched(t *testing.T) {
	e := newTestEngine(t)
	s, _ := e.NewSession("G", "openai_chat", false)
	defer s.Close()
	token, pem := redactPEM(t, s, "openai_chat")
	literal := `{"n":123456789012345678901234567890,"s":"a  b\tc","dup":1,"dup":2,"esc":"x\ny","pem":"` + token + `"}`
	body := chatToolArgsBody(t, literal)
	out, err := s.RedactJSONBody(body)
	if err != nil {
		t.Fatalf("redact request: %v", err)
	}
	if !strings.Contains(string(out), "{{Redact:") {
		t.Fatalf("gitleaks private-key rule did not fire: %s", out)
	}
	baseline := chatToolArgsFrom(t, out)
	restored, err := s.RestoreJSONBody(out)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(baseline, token, jsonEscapeForTest(pem), 1)
	if got := chatToolArgsFrom(t, restored); got != want {
		t.Fatalf("restore must only rewrite the placeholder span:\ngot  %q\nwant %q", got, want)
	}
}

// jsonEscapeForTest mirrors the core's jsonStringEscape for the escapable characters
// a test PEM can contain.
func jsonEscapeForTest(s string) string {
	return strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "\n", "\\n", "\r", "\\r", "\t", "\\t").Replace(s)
}

// TestSseRestoreLegacyFunctionCallArgumentsEscaped: legacy chat function_call
// (name+arguments directly on the delta, no tool_calls array) streams its arguments
// as a JSON slot too — the channel must be classified as JSON args so the restored
// multi-line secret is re-embedded escaped.
func TestSseRestoreLegacyFunctionCallArgumentsEscaped(t *testing.T) {
	e := newTestEngine(t)
	s, _ := e.NewSession("G", "openai_chat", false)
	defer s.Close()
	token, pem := redactPEM(t, s, "openai_chat")
	cut := 20
	ev1 := sseEvent(t, map[string]any{
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{
				"function_call": map[string]any{"name": "lookup", "arguments": `{"pem":"` + token[:cut]},
			},
		}},
	})
	ev2 := sseEvent(t, map[string]any{
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{
				"function_call": map[string]any{"arguments": token[cut:] + `"}`},
			},
		}},
	})
	r, err := s.NewSseRestorer()
	if err != nil {
		t.Fatal(err)
	}
	got := drainAll(t, r, []string{ev1, ev2})
	if strings.Contains(got, "{{Redact:") {
		t.Fatalf("placeholder leaked downstream: %s", got)
	}
	var combined strings.Builder
	for _, block := range strings.Split(strings.TrimSpace(got), "\n\n") {
		line := strings.TrimPrefix(block, "data: ")
		if line == block {
			continue
		}
		var ev struct {
			Choices []struct {
				Delta struct {
					FunctionCall struct {
						Arguments string `json:"arguments"`
					} `json:"function_call"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("bad event %q: %v", line, err)
		}
		for _, ch := range ev.Choices {
			combined.WriteString(ch.Delta.FunctionCall.Arguments)
		}
	}
	assertPemArgs(t, combined.String(), pem)
}

// TestSseRestoreCustomToolInputJsonShapedConsistent: a JSON-shaped custom-tool
// input payload. The custom-tool contract is RAW TEXT, so every path — streamed
// deltas, the done event's input, output_item.done's item.input, and the non-stream
// final object — must restore it byte-identically with the raw multi-line value.
// A parse-guarded (fragment-escaped) restore on the done/final paths would disagree
// with the plain-text delta path and double-escape what the client concatenates.
func TestSseRestoreCustomToolInputJsonShapedConsistent(t *testing.T) {
	e := newTestEngine(t)
	s, _ := e.NewSession("G", "openai_responses", false)
	defer s.Close()
	token, pem := redactPEM(t, s, "openai_responses")
	input := `{"note":"` + token + `"}`
	evDelta := "event: response.custom_tool_call_input.delta\ndata: " + mustJSON(t, map[string]any{"type": "response.custom_tool_call_input.delta", "output_index": 0, "content_index": 0, "item_id": "ct_1", "delta": input})
	evDone := "event: response.custom_tool_call_input.done\ndata: " + mustJSON(t, map[string]any{"type": "response.custom_tool_call_input.done", "output_index": 0, "item_id": "ct_1", "input": input})
	evItem := "event: response.output_item.done\ndata: " + mustJSON(t, map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"id": "ct_1", "type": "custom_tool_call", "status": "completed", "call_id": "call_1", "name": "exec", "input": input}})
	r, err := s.NewSseRestorer()
	if err != nil {
		t.Fatal(err)
	}
	got := drainAll(t, r, []string{evDelta, evDone, evItem})
	want := `{"note":"` + pem + `"}`
	if combined := sseDataDeltas(t, got); combined != want {
		t.Fatalf("delta path must restore byte-identically:\ngot  %q\nwant %q", combined, want)
	}
	var doneInput, itemInput string
	for _, block := range strings.Split(strings.TrimSpace(got), "\n\n") {
		for _, line := range strings.Split(block, "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var ev struct {
				Type  string `json:"type"`
				Input string `json:"input"`
				Item  struct {
					Type  string `json:"type"`
					Input string `json:"input"`
				} `json:"item"`
			}
			if err := json.Unmarshal([]byte(line[6:]), &ev); err != nil {
				t.Fatalf("bad data line %q: %v", line, err)
			}
			if ev.Type == "response.custom_tool_call_input.done" {
				doneInput = ev.Input
			}
			if ev.Item.Type == "custom_tool_call" {
				itemInput = ev.Item.Input
			}
		}
	}
	if doneInput != want {
		t.Fatalf("done event input must match the delta path:\ngot  %q\nwant %q", doneInput, want)
	}
	if itemInput != want {
		t.Fatalf("output_item.done input must match the delta path:\ngot  %q\nwant %q", itemInput, want)
	}
	// Non-stream final object: the same payload inside a complete response body.
	finalBody, err := json.Marshal(map[string]any{"output": []any{map[string]any{"type": "custom_tool_call", "id": "ct_1", "call_id": "call_1", "name": "exec", "input": input}}})
	if err != nil {
		t.Fatal(err)
	}
	restored, err := s.RestoreJSONBody(finalBody)
	if err != nil {
		t.Fatal(err)
	}
	var final struct {
		Output []struct {
			Type  string `json:"type"`
			Input string `json:"input"`
		} `json:"output"`
	}
	if err := json.Unmarshal(restored, &final); err != nil {
		t.Fatalf("restored body not valid JSON: %v", err)
	}
	if len(final.Output) != 1 || final.Output[0].Input != want {
		t.Fatalf("non-stream final input must match the delta path:\ngot  %#v\nwant %q", final.Output, want)
	}
}
