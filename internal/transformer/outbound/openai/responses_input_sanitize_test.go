package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zhizhishu/octopus/internal/transformer/model"
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

// TestSanitizeRetypesFcCustomToolCallToCtc is the production regression test for
// the 2026-11-28 400: upstream OpenAI rejected input[8].id 'fc_oAGlKW5KNWz41qZL'
// with "Expected an ID that begins with 'ctc'" because octopus used to mint fc_
// ids for custom_tool_call items and the sanitize pass deliberately let them
// through. Now the sanitizer retypes fc_ -> ctc_ preserving the suffix (sub2api
// retypedResponsesToolCallItemID), so the poisoned history is repaired in flight
// and the item id stays stable and unique. call_id - the real pairing key - is
// never touched.
func TestSanitizeRetypesFcCustomToolCallToCtc(t *testing.T) {
	raw := json.RawMessage(`[` +
		`{"type":"custom_tool_call","id":"fc_oAGlKW5KNWz41qZL","call_id":"call_pairing_key","name":"exec","input":"pwd"},` +
		`{"type":"function_call","id":"fc_regularCall99","call_id":"call_other","name":"shell","arguments":"{}"},` +
		`{"type":"custom_tool_call","id":"ctc_alreadyGood","call_id":"call_ok","name":"exec","input":"ls"},` +
		`{"type":"custom_tool_call","id":"item_noKnownPrefix","call_id":"call_drop","name":"exec","input":"ls"}` +
		`]`)

	sanitized, changed := sanitizeResponsesInputItemIDsRaw(raw)
	if !changed {
		t.Fatalf("expected sanitizer to report a change")
	}

	var items []map[string]json.RawMessage
	if err := json.Unmarshal(sanitized, &items); err != nil {
		t.Fatalf("sanitized output is not a valid item array: %v", err)
	}
	if len(items) != 4 {
		t.Fatalf("item count changed: got %d want 4", len(items))
	}

	assertID := func(index int, want string) {
		t.Helper()
		var got string
		if err := json.Unmarshal(items[index]["id"], &got); err != nil || got != want {
			t.Fatalf("input[%d].id = %q (err %v), want %q", index, got, err, want)
		}
	}

	// fc_ custom_tool_call retyped to ctc_ with suffix preserved.
	assertID(0, "ctc_oAGlKW5KNWz41qZL")
	// function_call keeps its fc_ id byte-for-byte (only custom_tool_call tightened).
	assertID(1, "fc_regularCall99")
	// already-valid ctc_ passes through untouched.
	assertID(2, "ctc_alreadyGood")
	// no known tool-call prefix -> no retype possible -> id dropped, item kept.
	if _, exists := items[3]["id"]; exists {
		t.Fatalf("input[3].id should have been dropped, still present: %s", items[3]["id"])
	}

	// Pairing survives: call_id is never touched on any item.
	var callID string
	if err := json.Unmarshal(items[0]["call_id"], &callID); err != nil || callID != "call_pairing_key" {
		t.Fatalf("input[0].call_id = %q (err %v), want call_pairing_key untouched", callID, err)
	}
	// Freeform payload survives the retype.
	var input string
	if err := json.Unmarshal(items[0]["input"], &input); err != nil || input != "pwd" {
		t.Fatalf("input[0].input = %q (err %v), want pwd untouched", input, err)
	}
}

// TestRetypeResponsesToolCallItemID pins the prefix table copied from sub2api
// responses_client_tools.go:296/310.
func TestRetypeResponsesToolCallItemID(t *testing.T) {
	cases := []struct {
		id       string
		itemType string
		want     string
	}{
		{"fc_abc123", "custom_tool_call", "ctc_abc123"},
		{"ctc_abc123", "custom_tool_call", "ctc_abc123"},
		{"fc_abc123", "function_call", "fc_abc123"},
		{"ctc_abc123", "function_call", "fc_abc123"},
		{"item_abc123", "custom_tool_call", "item_abc123"},
		{"fc_abc123", "message", "fc_abc123"},
		{"", "custom_tool_call", ""},
	}
	for _, c := range cases {
		if got := RetypeResponsesToolCallItemID(c.id, c.itemType); got != c.want {
			t.Errorf("RetypeResponsesToolCallItemID(%q, %q) = %q, want %q", c.id, c.itemType, got, c.want)
		}
	}
}

// TestShouldDropFcCustomToolCall pins that fc_ on custom_tool_call is no longer
// accepted as valid (strict upstreams demand ctc_).
func TestShouldDropFcCustomToolCall(t *testing.T) {
	if !ShouldDropResponsesInputItemID("custom_tool_call", "fc_oAGlKW5KNWz41qZL") {
		t.Errorf("fc_ custom_tool_call id must be treated as invalid")
	}
	if ShouldDropResponsesInputItemID("custom_tool_call", "ctc_oAGlKW5KNWz41qZL") {
		t.Errorf("ctc_ custom_tool_call id must be accepted")
	}
}
