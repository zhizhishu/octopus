package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

// TestSanitizeResponsesInputItemIDsDropsLegacyItemPrefix pins the outbound
// history-repair rule: input items replayed with an octopus-legacy "item_" id (or
// any id violating the per-type official prefix) lose ONLY the id field, the item
// body survives, and valid ids pass through byte-untouched. This is what stops the
// upstream 400 "Invalid 'input[N].id' ... Expected an ID that begins with 'msg'"
// after a session was poisoned by the old mint prefix.
func TestSanitizeResponsesInputItemIDsDropsLegacyItemPrefix(t *testing.T) {
	raw := json.RawMessage(`[` +
		`{"type":"message","role":"assistant","id":"item_9Sn3PKhw54WN0qtx","content":[{"type":"output_text","text":"hi"}]},` +
		`{"type":"function_call","id":"item_badcall","call_id":"item_badcall","name":"shell_command","arguments":"{}"},` +
		`{"type":"reasoning","id":"item_badreasoning","encrypted_content":"blob","summary":[]},` +
		`{"type":"image_generation_call","id":"item_badimage","status":"completed"},` +
		`{"type":"message","role":"assistant","id":"msg_valid","content":[{"type":"output_text","text":"keep"}]},` +
		`{"type":"function_call","id":"fc_valid","call_id":"call_1","name":"f","arguments":"{}"},` +
		`{"type":"reasoning","id":"rs_valid","encrypted_content":"blob2","summary":[]},` +
		`{"type":"image_generation_call","id":"ig_valid","status":"completed"},` +
		`{"type":"function_call_output","id":"item_unconstrained","call_id":"call_1","output":"ok"},` +
		`{"type":"web_search_call","id":"item_alsofree"}` +
		`]`)

	sanitized, changed := sanitizeResponsesInputItemIDsRaw(raw)
	if !changed {
		t.Fatalf("expected sanitizer to report a change")
	}

	var items []map[string]json.RawMessage
	if err := json.Unmarshal(sanitized, &items); err != nil {
		t.Fatalf("sanitized output is not a valid item array: %v", err)
	}
	if len(items) != 10 {
		t.Fatalf("item count changed: got %d want 10", len(items))
	}

	assertNoID := func(index int) {
		t.Helper()
		if _, exists := items[index]["id"]; exists {
			t.Fatalf("input[%d].id should have been dropped, still present: %s", index, items[index]["id"])
		}
	}
	assertID := func(index int, want string) {
		t.Helper()
		var got string
		if err := json.Unmarshal(items[index]["id"], &got); err != nil || got != want {
			t.Fatalf("input[%d].id = %q (err %v), want %q", index, got, err, want)
		}
	}

	assertNoID(0)
	assertNoID(1)
	assertNoID(2)
	assertNoID(3)
	assertID(4, "msg_valid")
	assertID(5, "fc_valid")
	assertID(6, "rs_valid")
	assertID(7, "ig_valid")
	assertID(8, "item_unconstrained")
	assertID(9, "item_alsofree")

	// The stripped function_call keeps its pairing key: call_id is never touched.
	var callID string
	if err := json.Unmarshal(items[1]["call_id"], &callID); err != nil || callID != "item_badcall" {
		t.Fatalf("input[1].call_id = %q (err %v), want item_badcall untouched", callID, err)
	}
	// The stripped reasoning item keeps its encrypted continuity blob.
	var encrypted string
	if err := json.Unmarshal(items[2]["encrypted_content"], &encrypted); err != nil || encrypted != "blob" {
		t.Fatalf("input[2].encrypted_content = %q (err %v), want blob untouched", encrypted, err)
	}
}

// TestSanitizeResponsesInputItemIDsNoOpKeepsBytes pins the healthy-traffic
// guarantee: when every id already carries its official prefix (what a genuine CLI
// echoes from a genuine backend), the sanitizer must return the ORIGINAL raw bytes
// unchanged — codex shape stays byte-faithful.
func TestSanitizeResponsesInputItemIDsNoOpKeepsBytes(t *testing.T) {
	raw := json.RawMessage(`[{"type":"message","role":"assistant","id":"msg_ok","content":[{"type":"output_text","text":"hi"}]},{"type":"function_call","id":"fc_ok","call_id":"call_2","name":"f","arguments":"{\"a\":1}"}]`)
	sanitized, changed := sanitizeResponsesInputItemIDsRaw(raw)
	if changed {
		t.Fatalf("healthy input reported as changed")
	}
	if string(sanitized) != string(raw) {
		t.Fatalf("healthy input bytes were rewritten:\n got %s\nwant %s", sanitized, raw)
	}
}

func TestSanitizeResponsesInputItemIDsRequiresUnderscorePrefix(t *testing.T) {
	cases := []struct {
		itemType string
		id       string
	}{
		{itemType: "message", id: "msgInvalid"},
		{itemType: "function_call", id: "fcInvalid"},
		{itemType: "custom_tool_call", id: "ctcInvalid"},
		{itemType: "reasoning", id: "rsInvalid"},
		{itemType: "image_generation_call", id: "igInvalid"},
	}
	for _, item := range cases {
		if !ShouldDropResponsesInputItemID(item.itemType, item.id) {
			t.Errorf("ShouldDropResponsesInputItemID(%q, %q) = false, want true", item.itemType, item.id)
		}
	}
}

// TestSanitizeResponsesInputItemIDsIgnoresNonArrayInput pins that the text
// shorthand (`"input": "just a prompt"`) and other non-array shapes are untouched.
func TestSanitizeResponsesInputItemIDsIgnoresNonArrayInput(t *testing.T) {
	raw := json.RawMessage(`"Say OK only"`)
	sanitized, changed := sanitizeResponsesInputItemIDsRaw(raw)
	if changed {
		t.Fatalf("string input reported as changed")
	}
	if string(sanitized) != string(raw) {
		t.Fatalf("string input rewritten: got %s", sanitized)
	}
}

// TestConvertToResponsesRequestSanitizesPoisonedHistory pins the wiring: a request
// whose raw Responses input carries a legacy item_ message id goes out with that id
// removed, while the rest of the payload survives.
func TestConvertToResponsesRequestSanitizesPoisonedHistory(t *testing.T) {
	req := &model.InternalLLMRequest{
		Model:        "gpt-test",
		RawAPIFormat: model.APIFormatOpenAIResponse,
		ResponsesInputRaw: json.RawMessage(`[` +
			`{"type":"message","role":"assistant","id":"item_9Sn3PKhw54WN0qtx","content":[{"type":"output_text","text":"old turn"}]},` +
			`{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]}` +
			`]`),
	}

	result := ConvertToResponsesRequest(req)
	wire, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal responses request: %v", err)
	}
	if strings.Contains(string(wire), "item_9Sn3PKhw54WN0qtx") {
		t.Fatalf("poisoned id still on the wire: %s", wire)
	}
	if !strings.Contains(string(wire), "old turn") || !strings.Contains(string(wire), "next") {
		t.Fatalf("history content lost during sanitize: %s", wire)
	}
}
