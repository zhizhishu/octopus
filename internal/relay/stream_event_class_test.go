package relay

import (
	"encoding/json"
	"strings"
	"testing"
)

// The legacy* helpers below are verbatim copies of the per-check parsers the SSE
// loops used before they shared one classifyStreamEvent call per event (each
// check decoded the same payload again just to read its "type"). They exist only
// so this test can prove the shared classification answers identically on every
// payload shape a stream can carry: the refactor must not flip a single dispatch
// decision, otherwise the bytes forwarded downstream could change.

func legacyResponsesStreamEventType(data string) string {
	data = strings.TrimSpace(data)
	if data == "" || strings.HasPrefix(data, "[DONE]") {
		return ""
	}
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(data), &envelope); err != nil {
		return ""
	}
	return strings.TrimSpace(envelope.Type)
}

func legacyResponsesToolCallDoneEvent(data string) bool {
	data = strings.TrimSpace(data)
	if data == "" || strings.HasPrefix(data, "[DONE]") {
		return false
	}
	var envelope struct {
		Type string `json:"type"`
		Item *struct {
			Type string `json:"type"`
		} `json:"item,omitempty"`
	}
	if err := json.Unmarshal([]byte(data), &envelope); err != nil {
		return false
	}
	if strings.TrimSpace(envelope.Type) != "response.output_item.done" || envelope.Item == nil {
		return false
	}
	switch strings.TrimSpace(envelope.Item.Type) {
	case "tool_call", "function_call", "local_shell_call", "tool_search_call", "custom_tool_call", "mcp_tool_call":
		return true
	default:
		return false
	}
}

func legacyUpstreamStreamTerminalEvent(eventType string, data string) bool {
	if strings.HasPrefix(strings.TrimSpace(data), "[DONE]") {
		return true
	}
	switch strings.TrimSpace(eventType) {
	case "message_stop", "response.completed":
		return true
	}
	switch legacyResponsesStreamEventType(data) {
	case "message_stop", "response.completed":
		return true
	default:
		return false
	}
}

func legacyUpstreamStreamCompletedEvent(eventType string, data string) bool {
	if strings.HasPrefix(strings.TrimSpace(data), "[DONE]") {
		return false
	}
	switch strings.TrimSpace(eventType) {
	case "message_stop", "response.completed":
		return true
	}
	switch legacyResponsesStreamEventType(data) {
	case "message_stop", "response.completed":
		return true
	default:
		return false
	}
}

func legacyIsStreamKeepaliveEvent(eventType string, data string) bool {
	if strings.EqualFold(strings.TrimSpace(eventType), "ping") {
		return true
	}
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(data), &envelope); err != nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(envelope.Type), "ping")
}

func TestClassifyStreamEventMatchesLegacyPerCheckParsers(t *testing.T) {
	payloads := []string{
		"",
		"   ",
		"[DONE]",
		"  [DONE]  ",
		"[DONE] trailing",
		"not json at all",
		"{broken json",
		"[1,2,3]",
		`"just a string"`,
		`{"type":123}`,
		`{"type":"message_start","message":{"id":"msg_1"}}`,
		`{"type":"message_stop"}`,
		`  {"type":" message_stop "}  `,
		`{"type":"response.completed","response":{"status":"completed"}}`,
		`{"type":"response.created"}`,
		`{"type":"response.in_progress"}`,
		`{"type":"ping"}`,
		`{"type":" PING "}`,
		`{"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`,
		`{"type":"response.output_item.done","item":{"type":"function_call"}}`,
		`{"type":"response.output_item.done","item":{"type":"custom_tool_call"}}`,
		`{"type":"response.output_item.done","item":{"type":"mcp_tool_call"}}`,
		`{"type":"response.output_item.done","item":{"type":"tool_search_call"}}`,
		`{"type":"response.output_item.done","item":{"type":"local_shell_call"}}`,
		`{"type":"response.output_item.done","item":{"type":"tool_call"}}`,
		`{"type":"response.output_item.done","item":{"type":" function_call "}}`,
		`{"type":"response.output_item.done","item":{"type":"message"}}`,
		`{"type":"response.output_item.done","item":{}}`,
		`{"type":"response.output_item.done","item":null}`,
		`{"type":"response.output_item.done"}`,
		`{"type":"response.output_item.added","item":{"type":"function_call"}}`,
		`{"item":{"type":"function_call"}}`,
		`{"type":"response.function_call_arguments.delta","delta":"{\"a\":1}"}`,
	}
	sseEventTypes := []string{"", "message_stop", "response.completed", "ping", " Ping ", "message_start", "content_block_delta"}

	for _, data := range payloads {
		class := classifyStreamEvent(data)

		if got, want := class.eventType, legacyResponsesStreamEventType(data); got != want {
			t.Fatalf("eventType mismatch for %q: got %q, legacy %q", data, got, want)
		}
		if got, want := class.isDone, strings.HasPrefix(strings.TrimSpace(data), "[DONE]"); got != want {
			t.Fatalf("isDone mismatch for %q: got %v, legacy %v", data, got, want)
		}
		if got, want := class.isToolCallDone(), legacyResponsesToolCallDoneEvent(data); got != want {
			t.Fatalf("isToolCallDone mismatch for %q: got %v, legacy %v", data, got, want)
		}

		for _, eventType := range sseEventTypes {
			if got, want := class.isTerminal(eventType), legacyUpstreamStreamTerminalEvent(eventType, data); got != want {
				t.Fatalf("isTerminal mismatch for event %q payload %q: got %v, legacy %v", eventType, data, got, want)
			}
			if got, want := class.isCompleted(eventType), legacyUpstreamStreamCompletedEvent(eventType, data); got != want {
				t.Fatalf("isCompleted mismatch for event %q payload %q: got %v, legacy %v", eventType, data, got, want)
			}
			if got, want := class.isKeepalive(eventType), legacyIsStreamKeepaliveEvent(eventType, data); got != want {
				t.Fatalf("isKeepalive mismatch for event %q payload %q: got %v, legacy %v", eventType, data, got, want)
			}
			if got, want := isStreamKeepaliveEvent(eventType, data), legacyIsStreamKeepaliveEvent(eventType, data); got != want {
				t.Fatalf("isStreamKeepaliveEvent mismatch for event %q payload %q: got %v, legacy %v", eventType, data, got, want)
			}
		}
	}
}
