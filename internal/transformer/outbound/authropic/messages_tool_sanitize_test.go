package authropic

import "testing"

// TestSanitizeClaudeToolID verifies tool ids are made to conform to Claude's
// ^[a-zA-Z0-9_-]+$ rule, that conforming ids pass through byte-for-byte, and that
// the mapping is deterministic so a tool_use.id and its paired
// tool_result.tool_use_id (both from the same original id) stay equal.
func TestSanitizeClaudeToolID(t *testing.T) {
	cases := []struct{ in, want string }{
		{"call_abc-123", "call_abc-123"}, // already valid -> unchanged
		{"toolu_01ABC", "toolu_01ABC"},   // already valid -> unchanged
		{"call/abc.123:x", "call_abc_123_x"},
		{"a b", "a_b"},
		{"", ""}, // empty left as-is (avoid mismatching the pair)
	}
	for _, c := range cases {
		// A tool_use.id and its paired tool_result.tool_use_id come from the same
		// original id; because this mapping is a pure, deterministic replacement,
		// both sanitize to the same value and the pairing is preserved.
		if got := sanitizeClaudeToolID(c.in); got != c.want {
			t.Errorf("sanitizeClaudeToolID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestToolUseInput verifies Claude tool_use.input is always a JSON object:
// object arguments pass through verbatim; empty, invalid, or non-object valid
// JSON collapse to "{}".
func TestToolUseInput(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"city":"Tokyo"}`, `{"city":"Tokyo"}`}, // object -> verbatim
		{"", "{}"},                               // empty -> {}
		{"const r = tools.exec();", "{}"},        // freeform / invalid JSON -> {}
		{`[1,2,3]`, "{}"},                        // valid but non-object -> {}
		{`"hello"`, "{}"},                        // valid JSON string, not object -> {}
		{`  {"a":1}  `, `  {"a":1}  `},           // leading space, still an object -> verbatim
	}
	for _, c := range cases {
		if got := string(toolUseInput(c.in)); got != c.want {
			t.Errorf("toolUseInput(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
