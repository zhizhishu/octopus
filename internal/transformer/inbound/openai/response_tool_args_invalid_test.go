package openai

import "testing"

// TestNormalizeToolArgumentsInvalid verifies a function_call's arguments are
// always valid JSON for the codex client: empty or syntactically invalid values
// collapse to "{}", while any valid JSON passes through byte-for-byte (real tool
// arguments are never rewritten).
func TestNormalizeToolArgumentsInvalid(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "{}"},                               // empty -> {}
		{`{"city":"Tokyo"}`, `{"city":"Tokyo"}`}, // valid object -> verbatim
		{`{"a":1}`, `{"a":1}`},                   // valid -> verbatim
		{`{"a":`, "{}"},                          // truncated / invalid -> {}
		{"not json", "{}"},                       // garbage -> {}
		{`[1,2]`, `[1,2]`},                       // valid JSON (array) is left as-is; only invalid collapses
	}
	for _, c := range cases {
		if got := normalizeToolArguments(c.in); got != c.want {
			t.Errorf("normalizeToolArguments(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
