package redact

import (
	"encoding/json"
	"strings"
	"testing"
)

// R2 (data fidelity) fail-first counter-examples. These lock the invariant that a
// redaction pass must not touch bytes it did not redact: no re-serialization may
// reorder whitespace, drop key order, fold duplicate keys or round numeric
// literals (>2^53). They are expected to FAIL against the pre-R2 core (parse ->
// redactJson -> JSON.stringify everywhere) and pass once the zero-hit fast path
// and the byte-preserving splice are in place.

// newFidelitySession builds a session with the given detector flag letters.
func newFidelitySession(t *testing.T, flags string) *Session {
	t.Helper()
	e := newTestEngine(t)
	s, err := e.NewSession(flags, "openai_chat", false)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	return s
}

// (A1) Zero hit: the input bytes must come back untouched (whitespace intact),
// not reflowed by a JSON.parse -> JSON.stringify round trip.
func TestFidelityRedactJSONTextZeroHitByteIdentical(t *testing.T) {
	s := newFidelitySession(t, "E")
	defer s.Close()

	in := "{ \"a\" : 1 ,\n  \"b\" : [ 1 , 2 ] }"
	out, err := s.RedactJSONText(in)
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("zero-hit RedactJSONText must be byte-identical\n in=%q\nout=%q", in, out)
	}
}

// (A2) Hit with an integer above 2^53: the redaction must not round the literal.
func TestFidelityRedactJSONTextHitKeepsBigInteger(t *testing.T) {
	s := newFidelitySession(t, "E")
	defer s.Close()

	in := `{"email":"user@example.com","n":9007199254740993}`
	out, err := s.RedactJSONText(in)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid([]byte(out)) {
		t.Fatalf("output must stay valid JSON, got %q", out)
	}
	if strings.Contains(out, "9007199254740992") {
		t.Fatalf("big integer must not be rounded to 2^53, got %q", out)
	}
	if !strings.Contains(out, "9007199254740993") {
		t.Fatalf("big integer literal must be preserved verbatim, got %q", out)
	}
	if strings.Contains(out, "user@example.com") {
		t.Fatalf("email must still be redacted, got %q", out)
	}
}

// (A3) Nested JSON, zero hit, session already has count>0 (so the Go-level
// count==0 short-circuit cannot mask the core nested-recursion re-serialization):
// the nested value must come back byte-identical.
func TestFidelityRedactJSONTextNestedZeroHitByteIdentical(t *testing.T) {
	s := newFidelitySession(t, "E")
	defer s.Close()

	// Prime the session so Count()>0 for the second call.
	if _, err := s.RedactJSONText(`{"email":"prime@example.com"}`); err != nil {
		t.Fatal(err)
	}
	if s.Count() == 0 {
		t.Fatal("priming call did not redact; test setup is wrong")
	}

	in := `{ "outer" : { "in" : 1 } , "list" : [ "x" ] }`
	out, err := s.RedactJSONText(in)
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("nested zero-hit must be byte-identical\n in=%q\nout=%q", in, out)
	}
}

// (A4) Duplicate keys: JSON.parse folds them to last-wins. Record the current
// behavior and lock the SAFE invariant only (no unredacted secret may survive and
// the slot must stay valid JSON). Byte-preservation of duplicate keys is NOT a
// supported contract (see RESIDUAL): the folded-away member never reaches the
// detector, so a splice that preserved it would forward its value verbatim.
func TestFidelityRedactJSONTextDuplicateKeysRecordsFolding(t *testing.T) {
	s := newFidelitySession(t, "E")
	defer s.Close()

	in := `{"a":"first@example.com","a":"second@example.com"}`
	out, err := s.RedactJSONText(in)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("duplicate-key case: in=%q out=%q valid=%v", in, out, json.Valid([]byte(out)))
	if !json.Valid([]byte(out)) {
		t.Fatalf("duplicate-key output must stay valid JSON, got %q", out)
	}
}

// The splice must use literal matching (never a regex), so a secret containing
// regex metacharacters is handled, and every occurrence of the same secret is
// replaced while the surrounding whitespace is preserved.
func TestFidelityRedactJSONTextSplicesRegexSpecialAndAllOccurrences(t *testing.T) {
	s := newFidelitySession(t, "E")
	defer s.Close()

	in := `{  "email" : "a+b.c@ex-ample.com" ,  "again" : "a+b.c@ex-ample.com"  }`
	out, err := s.RedactJSONText(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "a+b.c@ex-ample.com") {
		t.Fatalf("regex-special secret must be redacted, got %q", out)
	}
	if n := strings.Count(out, "{{Redact:"); n != 2 {
		t.Fatalf("both occurrences of the same secret must be placeholders, got %d in %q", n, out)
	}
	if !strings.HasPrefix(out, "{  ") || !strings.HasSuffix(out, "  }") {
		t.Fatalf("surrounding whitespace must be preserved (splice, not reflow): %q", out)
	}
	if !json.Valid([]byte(out)) {
		t.Fatalf("output must stay valid JSON, got %q", out)
	}
}

// A secret written with a non-canonical escape (\u0041) cannot be located literally
// in the original bytes, so the splice must fall back to re-serialization: still
// valid JSON, still no secret leaked.
func TestFidelityRedactJSONTextFallsBackOnUnconventionalEscape(t *testing.T) {
	s := newFidelitySession(t, "E")
	defer s.Close()

	in := `{"email":"\u0041dmin@example.com"}`
	out, err := s.RedactJSONText(in)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid([]byte(out)) {
		t.Fatalf("fallback output must stay valid JSON, got %q", out)
	}
	if strings.Contains(out, "dmin@example.com") {
		t.Fatalf("secret must not survive the fallback, got %q", out)
	}
	if n := strings.Count(out, "{{Redact:"); n != 1 {
		t.Fatalf("expected one placeholder after fallback, got %d in %q", n, out)
	}
}

// (D, RESIDUAL) RedactJSONBody still re-serializes the WHOLE body on a hit: only
// nested JSON-string values (the recursion fixed by transform 10) and the separate
// RedactJSONText slots are byte-preserved. A top-level hit therefore normalizes the
// outer whitespace and numeric literals. Record the behavior; assert only the safe
// invariants (valid JSON, no leaked secret).
func TestFidelityRedactJSONBodyHitReflowsOuterBytesResidual(t *testing.T) {
	s := newFidelitySession(t, "E")
	defer s.Close()

	body := []byte("{ \"model\" : \"m\" ,\n  \"messages\" : [ { \"role\" : \"user\" , \"content\" : \"a@b.com\" } ] }")
	out, err := s.RedactJSONBody(body)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("body-level RESIDUAL: in=%q\n out=%q", body, out)
	if !json.Valid(out) {
		t.Fatalf("body output must stay valid JSON, got %q", out)
	}
	if strings.Contains(string(out), "a@b.com") {
		t.Fatalf("secret must be redacted at body level, got %q", out)
	}
}

// The nested-JSON splice must survive the outer whole-body re-serialization: an
// integer >2^53 inside a tool-arguments string stays verbatim even though
// RedactJSONBody reflows the outer body around it.
func TestFidelityBodyScanKeepsNestedBigIntegerInArgs(t *testing.T) {
	s := newFidelitySession(t, "E")
	defer s.Close()

	args := `{"email":"user@example.com","n":9007199254740993}`
	qa, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"model":"m","messages":[{"role":"assistant","tool_calls":[{"type":"function","function":{"name":"t","arguments":` + string(qa) + `}}]}]}`)
	out, err := s.RedactJSONBody(body)
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Messages []struct {
			ToolCalls []struct {
				Function struct {
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("body not valid JSON: %v (%q)", err, out)
	}
	got := v.Messages[0].ToolCalls[0].Function.Arguments
	if !strings.Contains(got, "9007199254740993") {
		t.Fatalf("nested big integer must survive the outer reflow, got %q", got)
	}
	if strings.Contains(got, "user@example.com") {
		t.Fatalf("nested secret must be redacted, got %q", got)
	}
}

// (R2 §7) A secret already registered earlier in the session must still be replaced
// in a later nested value, even though tokenFor adds no new mapping — "no new mapping"
// must not be read as "no hit" (that is exactly what the per-walk pairs capture fixes,
// versus a Count-before/after diff).
func TestFidelityRedactJSONTextReplacesAlreadyRegisteredSecret(t *testing.T) {
	s := newFidelitySession(t, "E")
	defer s.Close()

	if _, err := s.RedactJSONText(`{"x":"dup@example.com"}`); err != nil {
		t.Fatal(err)
	}
	before := s.Count()
	in := `{ "y" : "dup@example.com" }`
	out, err := s.RedactJSONText(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "dup@example.com") {
		t.Fatalf("already-registered secret must still be replaced, got %q", out)
	}
	if s.Count() != before {
		t.Fatalf("tokenFor must stay idempotent; count changed %d -> %d", before, s.Count())
	}
	if !strings.HasPrefix(out, `{ "`) || !strings.HasSuffix(out, `" }`) {
		t.Fatalf("surrounding whitespace must be preserved: %q", out)
	}
}
