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

// (A4) Duplicate keys are scanned at EVERY occurrence (the old JSON.parse fold is
// gone): neither the first nor the second value may leak, existing contract restored.
func TestFidelityRedactJSONTextDuplicateKeysEachScanned(t *testing.T) {
	s := newFidelitySession(t, "E")
	defer s.Close()

	in := `{"a":"first@example.com","a":"second@example.com"}`
	out, err := s.RedactJSONText(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "first@example.com") || strings.Contains(out, "second@example.com") {
		t.Fatalf("both duplicate-key values must be redacted, got %q", out)
	}
	if !json.Valid([]byte(out)) {
		t.Fatalf("duplicate-key output must stay valid JSON, got %q", out)
	}
	if n := strings.Count(out, "{{Redact:"); n != 2 {
		t.Fatalf("both occurrences must become placeholders, got %d in %q", n, out)
	}
	if strings.Count(out, `"a"`) != 2 {
		t.Fatalf("duplicate keys must be preserved verbatim, got %q", out)
	}

	// secret first, safe second: the first (folded away by the old code) must be redacted.
	in2 := `{"a":"only@example.com","a":"safe"}`
	out2, err := s.RedactJSONText(in2)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out2, "only@example.com") || !strings.Contains(out2, `"a":"safe"`) {
		t.Fatalf("secret-before-safe duplicate key must redact the first value, got %q", out2)
	}

	// safe first, secret second: the second must be redacted.
	in3 := `{"a":"safe","a":"later@example.com"}`
	out3, err := s.RedactJSONText(in3)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out3, "later@example.com") || !strings.Contains(out3, `"a":"safe"`) {
		t.Fatalf("safe-before-secret duplicate key must redact the second value, got %q", out3)
	}
}

// No-hit duplicate keys come back byte-identical (no fold, no reflow).
func TestFidelityRedactJSONTextDuplicateKeysNoHitByteIdentical(t *testing.T) {
	s := newFidelitySession(t, "E")
	defer s.Close()

	in := `{"a":"x","a":"y"}`
	out, err := s.RedactJSONText(in)
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("no-hit duplicate keys must be byte-identical\n in=%q\nout=%q", in, out)
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

// A secret written with a JSON escape (\u0041) must be decoded, detected and spliced
// at the ORIGINAL byte offsets (no re-serialization). The companion >2^53 integer in
// the same value must survive verbatim -- the old JSON.stringify fallback rounded it.
func TestFidelityRedactJSONTextEscapedSecretKeepsBigInteger(t *testing.T) {
	s := newFidelitySession(t, "E")
	defer s.Close()

	in := `{"email":"\u0041dmin@example.com","n":9007199254740993}`
	out, err := s.RedactJSONText(in)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid([]byte(out)) {
		t.Fatalf("output must stay valid JSON, got %q", out)
	}
	if strings.Contains(out, "dmin@example.com") {
		t.Fatalf("escaped secret must be redacted, got %q", out)
	}
	if n := strings.Count(out, "{{Redact:"); n != 1 {
		t.Fatalf("expected one placeholder, got %d in %q", n, out)
	}
	if !strings.Contains(out, `"n":9007199254740993`) || strings.Contains(out, "9007199254740992") {
		t.Fatalf(">2^53 integer must be preserved verbatim (no rounding), got %q", out)
	}
}

// A top-level hit must splice only the matched string span: surrounding whitespace
// and integer literals stay verbatim (no JSON.stringify reflow).
func TestFidelityRedactJSONBodyHitKeepsOuterBytes(t *testing.T) {
	s := newFidelitySession(t, "E")
	defer s.Close()

	body := []byte("{ \"model\" : \"m\" ,\n  \"n\" : 9007199254740993,\n  \"messages\" : [ { \"role\" : \"user\" , \"content\" : \"a@b.com\" } ] }")
	out, err := s.RedactJSONBody(body)
	if err != nil {
		t.Fatal(err)
	}
	os := string(out)
	if !json.Valid(out) {
		t.Fatalf("body output must stay valid JSON, got %q", os)
	}
	if strings.Contains(os, "a@b.com") {
		t.Fatalf("secret must be redacted at body level, got %q", os)
	}
	if !strings.Contains(os, "9007199254740993") {
		t.Fatalf("big integer must stay verbatim, got %q", os)
	}
	if !strings.Contains(os, "{ \"model\" : \"m\" ,\n  \"n\" : 9007199254740993,") {
		t.Fatalf("outer whitespace must be preserved, got %q", os)
	}

	back, err := s.RestoreJSONBody(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(back) != string(body) {
		t.Fatalf("restore must be byte-exact\n in=%q\nout=%q", body, back)
	}
}

// Notice injection into an existing string content slot must keep surrounding
// whitespace and integer literals; only the user content string is prefixed.
func TestFidelityNoticeInjectKeepsOuterBytes(t *testing.T) {
	e := newTestEngine(t)
	s, err := e.NewSession("E", "openai_chat", true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	body := []byte("{ \"model\" : \"m\" ,\n  \"n\" : 9007199254740993,\n  \"messages\" : [ { \"role\" : \"user\" , \"content\" : \"a@b.com\" } ] }")
	out, err := s.RedactJSONBody(body)
	if err != nil {
		t.Fatal(err)
	}
	os := string(out)
	if !strings.Contains(os, "Sensitive values are redacted") {
		t.Fatalf("notice missing, got %q", os)
	}
	if strings.Contains(os, "a@b.com") {
		t.Fatalf("secret must still be redacted, got %q", os)
	}
	if !strings.Contains(os, "{ \"model\" : \"m\" ,\n  \"n\" : 9007199254740993,") {
		t.Fatalf("outer whitespace and big integer must stay, got %q", os)
	}
	if !strings.Contains(os, `"content" : "`) {
		t.Fatalf("content key/colon/quote bytes must stay, got %q", os)
	}
}

// Notice-only (zero secrets) must still splice, not re-serialize the envelope.
func TestFidelityNoticeOnlyZeroHitKeepsOuterBytes(t *testing.T) {
	e := newTestEngine(t)
	s, err := e.NewSession("E", "openai_chat", true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	body := []byte("{ \"n\" : 9007199254740993,\n  \"messages\" : [ { \"role\" : \"user\" , \"content\" : \"hello\" } ] }")
	out, err := s.RedactJSONBody(body)
	if err != nil {
		t.Fatal(err)
	}
	os := string(out)
	if s.Count() != 0 {
		t.Fatalf("expected zero redactions, got %d", s.Count())
	}
	if !strings.Contains(os, "Sensitive values are redacted") {
		t.Fatalf("notice missing, got %q", os)
	}
	if !strings.Contains(os, "{ \"n\" : 9007199254740993,\n  \"messages\"") {
		t.Fatalf("outer bytes must stay on notice-only, got %q", os)
	}
}

// Notice into an existing content[].text slot must splice inside that string, not
// add a sibling part and re-serialize the envelope.
func TestFidelityNoticeInjectIntoContentBlockKeepsOuterBytes(t *testing.T) {
	e := newTestEngine(t)
	s, err := e.NewSession("E", "openai_chat", true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	body := []byte(`{ "n" : 9007199254740993, "messages" : [ { "role" : "user" , "content" : [ { "type" : "text" , "text" : "a@b.com" } ] } ] }`)
	out, err := s.RedactJSONBody(body)
	if err != nil {
		t.Fatal(err)
	}
	os := string(out)
	if !strings.Contains(os, "Sensitive values are redacted") {
		t.Fatalf("notice missing, got %q", os)
	}
	if strings.Contains(os, "a@b.com") {
		t.Fatalf("secret must still be redacted, got %q", os)
	}
	if !strings.Contains(os, `{ "n" : 9007199254740993, "messages"`) {
		t.Fatalf("outer bytes must stay, got %q", os)
	}
	if strings.Count(os, `"type"`) != 1 {
		t.Fatalf("must not insert a sibling content part, got %q", os)
	}
}

// Image-only user content has no string slot: injection adds a part and may
// re-serialize. The notice must still land; this is the documented fallback.
func TestFidelityNoticeInjectStructureChangeFallsBack(t *testing.T) {
	e := newTestEngine(t)
	s, err := e.NewSession("E", "openai_chat", true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	body := []byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/x.png"}}]}]}`)
	out, err := s.RedactJSONBody(body)
	if err != nil {
		t.Fatal(err)
	}
	os := string(out)
	if !json.Valid(out) {
		t.Fatalf("fallback must stay valid JSON, got %q", os)
	}
	if !strings.Contains(os, "Sensitive values are redacted") {
		t.Fatalf("notice must still be injected, got %q", os)
	}
}

// Chat-to-responses synthesized input: notice splice must keep array whitespace
// and integer literals; unwrap must not re-serialize the array.
func TestFidelityInjectNoticeInputRawKeepsOuterBytes(t *testing.T) {
	e := newTestEngine(t)
	s, err := e.NewSession("E", "openai_chat", true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	raw := []byte(`[ { "role" : "user" , "content" : [ { "type" : "input_text" , "text" : "hello" } ] } , { "n" : 9007199254740993 } ]`)
	out, err := s.InjectNoticeInputRaw(raw)
	if err != nil {
		t.Fatal(err)
	}
	os := string(out)
	if !strings.Contains(os, "Sensitive values are redacted") {
		t.Fatalf("notice missing, got %q", os)
	}
	if !strings.Contains(os, `{ "n" : 9007199254740993 }`) {
		t.Fatalf("sibling object whitespace and big integer must stay, got %q", os)
	}
	if !strings.HasPrefix(os, "[ { \"role\"") {
		t.Fatalf("array prefix bytes must stay, got %q", os)
	}
	if strings.Count(os, `"type"`) != 1 {
		t.Fatalf("must not insert a sibling content part, got %q", os)
	}
}

func TestFidelityInjectNoticeInputRawDisabledIsByteIdentical(t *testing.T) {
	e := newTestEngine(t)
	s, err := e.NewSession("E", "openai_chat", false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	raw := []byte(`[ { "role" : "user" , "content" : "hello" } ]`)
	out, err := s.InjectNoticeInputRaw(raw)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(raw) {
		t.Fatalf("notice-off must be byte-identical\n in=%q\nout=%q", raw, out)
	}
}

// Duplicate keys on the whole-body path must be scanned at every occurrence and
// kept as two keys (JSON.parse would fold them).
func TestFidelityRedactJSONBodyDuplicateKeysEachScanned(t *testing.T) {
	s := newFidelitySession(t, "E")
	defer s.Close()

	body := []byte(`{ "a" : "first@example.com" , "a" : "second@example.com" , "n" : 9007199254740993 }`)
	out, err := s.RedactJSONBody(body)
	if err != nil {
		t.Fatal(err)
	}
	os := string(out)
	if strings.Contains(os, "first@example.com") || strings.Contains(os, "second@example.com") {
		t.Fatalf("both duplicate-key values must be redacted, got %q", os)
	}
	if n := strings.Count(os, "{{Redact:"); n != 2 {
		t.Fatalf("both occurrences must become placeholders, got %d in %q", n, os)
	}
	if strings.Count(os, `"a"`) != 2 {
		t.Fatalf("duplicate keys must be preserved verbatim, got %q", os)
	}
	if !strings.Contains(os, `"n" : 9007199254740993`) {
		t.Fatalf("big integer and surrounding whitespace must stay, got %q", os)
	}

	back, err := s.RestoreJSONBody(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(back) != string(body) {
		t.Fatalf("duplicate-key restore must be byte-exact\n in=%q\nout=%q", body, back)
	}
}

// The nested-JSON splice must survive a whole-body scan: an integer >2^53 inside
// a tool-arguments string stays verbatim, and the outer envelope is not reflowed.
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
// must not be read as "no hit" (the lexical scan runs the detector on every decoded
// value; it never uses a Count-before/after diff).
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

// A secret sitting in a KEY name must not be altered (keys are never redacted) and no
// value is minted for it.
func TestFidelityRedactJSONTextSecretInKeyUntouched(t *testing.T) {
	s := newFidelitySession(t, "E")
	defer s.Close()

	in := `{"a@b.com":"plainvalue"}`
	out, err := s.RedactJSONText(in)
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("a secret in a key name must not be redacted, in=%q out=%q", in, out)
	}
	if s.Count() != 0 {
		t.Fatalf("no value should be minted for a key-only secret, count=%d", s.Count())
	}
}

// A JSON string value that itself contains JSON with a nested secret: the innermost
// secret is redacted and the enclosing JSON escapes/structure are preserved.
func TestFidelityRedactJSONTextNestedJsonInStringPreservesEscapes(t *testing.T) {
	s := newFidelitySession(t, "E")
	defer s.Close()

	in := `{"outer":"{\"inner\":\"user@example.com\"}"}`
	out, err := s.RedactJSONText(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "user@example.com") {
		t.Fatalf("nested secret must be redacted, got %q", out)
	}
	if !strings.Contains(out, `"outer":"{\"inner\":\"{{Redact:`) {
		t.Fatalf("enclosing JSON escapes/structure must be preserved, got %q", out)
	}
	if !json.Valid([]byte(out)) {
		t.Fatalf("output must stay valid JSON, got %q", out)
	}
}

// Unicode (astral / surrogate pair) and leading whitespace: only the matched span
// changes; the emoji bytes and the leading whitespace are preserved.
func TestFidelityRedactJSONTextUnicodeAndLeadingWhitespace(t *testing.T) {
	s := newFidelitySession(t, "E")
	defer s.Close()

	in := `  {"emoji":"😀","e":"\u0041dmin@example.com"}`
	out, err := s.RedactJSONText(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "dmin@example.com") {
		t.Fatalf("escaped secret must be redacted, got %q", out)
	}
	if !strings.HasPrefix(out, "  {") || !strings.Contains(out, "😀") {
		t.Fatalf("leading whitespace and astral char must be preserved, got %q", out)
	}
}

// Restore is value-exact: the redacted string restores byte-for-byte to the original
// input (a plain comparison of the restored text, no JS-float round trip).
func TestFidelityRedactJSONTextRestoreIsExact(t *testing.T) {
	s := newFidelitySession(t, "E")
	defer s.Close()

	in := `{"email":"user@example.com","n":9007199254740993}`
	out, err := s.RedactJSONText(in)
	if err != nil {
		t.Fatal(err)
	}
	back, err := s.RestoreJSONText(out)
	if err != nil {
		t.Fatal(err)
	}
	if back != in {
		t.Fatalf("restore must be byte-exact\n in=%q\nout=%q", in, back)
	}
}

// Structural-vs-business gating for media key NAMES: a tool-argument (business
// payload) value under a key literally named audio/image_url/file_data is business
// content, so the media skip must NOT apply and the value must be scanned. (This
// replaces the earlier "unchanged" characterization that locked the missed scan.)
func TestFidelityBusinessMediaKeyIsScanned(t *testing.T) {
	s := newFidelitySession(t, "E")
	defer s.Close()

	for _, in := range []string{
		`{"audio":"user@example.com"}`,
		`{"image_url":"user@example.com"}`,
		`{"nested":{"file_data":"user@example.com"}}`,
	} {
		out, err := s.RedactJSONText(in)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "user@example.com") {
			t.Fatalf("business media-key value must be scanned, in=%q out=%q", in, out)
		}
		if !strings.Contains(out, "{{Redact:") {
			t.Fatalf("expected a placeholder, in=%q out=%q", in, out)
		}
		if !json.Valid([]byte(out)) {
			t.Fatalf("output must stay valid JSON, out=%q", out)
		}
	}
}

// The media skip is UNCHANGED for real attachment/source positions (non-business): a
// chat message image_url payload is left byte-for-byte even though it holds an email.
func TestFidelityNonBusinessMediaKeyStillSkipped(t *testing.T) {
	s := newFidelitySession(t, "E")
	defer s.Close()

	body := []byte(`{"model":"m","messages":[{"role":"user","content":[` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,user@example.com"}}]}]}`)
	out, err := s.RedactJSONBody(body)
	if err != nil {
		t.Fatal(err)
	}
	os := string(out)
	if !strings.Contains(os, "base64,user@example.com") {
		t.Fatalf("non-business media value must be untouched, got %q", os)
	}
	if strings.Contains(os, "{{Redact:") {
		t.Fatalf("non-business media value must not be redacted, got %q", os)
	}
}

// Chat tool-call arguments are a JSON string (business payload via "<nested-json>"):
// a secret under a key literally named audio/image_url must be scanned on the
// RedactJSONBody path, not only on the RedactJSONText helper.
func TestFidelityBusinessMediaKeyInToolArgumentsIsScanned(t *testing.T) {
	s := newFidelitySession(t, "E")
	defer s.Close()

	body := []byte(`{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"t","arguments":"{\"audio\":\"user@example.com\",\"image_url\":\"user@example.com\"}"}}]}]}`)
	out, err := s.RedactJSONBody(body)
	if err != nil {
		t.Fatal(err)
	}
	os := string(out)
	if strings.Contains(os, "user@example.com") {
		t.Fatalf("tool-argument media-key secrets must be scanned, got %q", os)
	}
	if !strings.Contains(os, "{{Redact:") {
		t.Fatalf("expected placeholders in tool arguments, got %q", os)
	}
	if !json.Valid(out) {
		t.Fatalf("body must stay valid JSON, got %q", os)
	}
}

// Tool declarations and real attachment bytes are structural positions: even with a
// hit elsewhere in the body, the declaration text and the attachment payload must not
// be rewritten.
func TestFidelityBodyScanLeavesToolDeclarationAndAttachment(t *testing.T) {
	s := newFidelitySession(t, "E")
	defer s.Close()

	body := []byte(`{"model":"m","tools":[{"type":"function","function":{"name":"t","description":"tool@example.com"}}],` +
		`"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"real@example.com"},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,user@example.com"}}]}]}`)
	out, err := s.RedactJSONBody(body)
	if err != nil {
		t.Fatal(err)
	}
	os := string(out)
	if !strings.Contains(os, "tool@example.com") {
		t.Fatalf("tool declaration text must be untouched, got %q", os)
	}
	if !strings.Contains(os, "base64,user@example.com") {
		t.Fatalf("attachment payload must be untouched, got %q", os)
	}
	if strings.Contains(os, "real@example.com") {
		t.Fatalf("user content secret must be redacted, got %q", os)
	}
	if !json.Valid(out) {
		t.Fatalf("body must stay valid JSON, got %q", os)
	}
}

// Multi-level complete-slot restore must be EXACT (this replaces the earlier
// "blocked" characterization now that the transform-11 lexer mirror landed). A
// synthetic PEM secret (newlines + quote + backslash) is nested in 2 and 3 JSON
// strings; the outer container also carries an email, a >2^53 integer and an astral
// char. Redaction keeps every level parseable and removes the secrets; restore is
// byte-exact and the decoded innermost value equals the original -- asserted by
// parsing, never by a JSON.stringify/float round trip.
func TestFidelityMultiLevelNestedSecretRestoreIsExact(t *testing.T) {
	s := newFidelitySession(t, "EG")
	defer s.Close()

	const emoji = "\U0001F600"
	secret := "-----BEGIN PRIVATE KEY-----\n\"q\\b\"" + strings.Repeat("SYNTHETIC", 12) + "\n-----END PRIVATE KEY-----"

	// depth = number of JSON-string levels around the secret.
	mk := func(depth int) string {
		leaf, err := json.Marshal(secret)
		if err != nil {
			t.Fatal(err)
		}
		body := `{"c":` + string(leaf) + `}`
		for i := 0; i < depth-2; i++ {
			w, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			body = `{"k":` + string(w) + `}`
		}
		w, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		return `{"a":` + string(w) + `,"email":"nested@example.com","n":9007199254740993,"u":"` + emoji + `"}`
	}

	for _, depth := range []int{2, 3} {
		in := mk(depth)

		red, err := s.RedactJSONText(in)
		if err != nil {
			t.Fatalf("depth %d: redact: %v", depth, err)
		}
		if strings.Contains(red, "PRIVATE KEY") || strings.Contains(red, "nested@example.com") {
			t.Fatalf("depth %d: secrets must be redacted", depth)
		}
		if !strings.Contains(red, "9007199254740993") {
			t.Fatalf("depth %d: >2^53 integer must be preserved verbatim", depth)
		}
		if !strings.Contains(red, emoji) {
			t.Fatalf("depth %d: astral char must be preserved", depth)
		}
		_, last := parseNestedChain(t, red, depth)
		if !strings.Contains(last, "{{Redact:") {
			t.Fatalf("depth %d: innermost value must be a placeholder, got %q", depth, last)
		}

		back, err := s.RestoreJSONText(red)
		if err != nil {
			t.Fatalf("depth %d: restore: %v", depth, err)
		}
		if back != in {
			t.Fatalf("depth %d: restore must be byte-exact\n in=%q\nout=%q", depth, in, back)
		}
		outer2, last2 := parseNestedChain(t, back, depth)
		if outer2 != "nested@example.com" {
			t.Fatalf("depth %d: restored email must equal the original", depth)
		}
		if last2 != secret {
			t.Fatalf("depth %d: restored innermost decoded value must equal the original secret", depth)
		}
	}
}

// parseNestedChain walks `{"a":<json string>}` then (depth-2) `{"k":...}` levels then
// a final `{"c":...}`, returning the outer email and the innermost string value.
func parseNestedChain(t *testing.T, s string, depth int) (email, innermost string) {
	t.Helper()
	var outer struct {
		A     string `json:"a"`
		Email string `json:"email"`
	}
	if err := json.Unmarshal([]byte(s), &outer); err != nil {
		t.Fatalf("outer JSON must parse: %v", err)
	}
	cur := outer.A
	for i := 1; i < depth-1; i++ {
		var m struct {
			K string `json:"k"`
		}
		if err := json.Unmarshal([]byte(cur), &m); err != nil {
			t.Fatalf("nested level %d must parse: %v", i, err)
		}
		cur = m.K
	}
	var last struct {
		C string `json:"c"`
	}
	if err := json.Unmarshal([]byte(cur), &last); err != nil {
		t.Fatalf("innermost JSON must parse: %v", err)
	}
	return outer.Email, last.C
}

// Control: ONE level of nesting around the SAME special-char secret also round-trips
// byte-exactly (must not regress while the multi-level case is fixed).
func TestFidelityOneLevelNestedSecretRestoreIsExact(t *testing.T) {
	s := newFidelitySession(t, "G")
	defer s.Close()

	secret := "-----BEGIN PRIVATE KEY-----\n" + strings.Repeat("SYNTHETIC", 15) + "\n-----END PRIVATE KEY-----"
	b, err := json.Marshal(secret)
	if err != nil {
		t.Fatal(err)
	}
	in := `{"value":` + string(b) + `}`
	red, err := s.RedactJSONText(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(red, "PRIVATE KEY") {
		t.Fatalf("redaction must remove the secret")
	}
	back, err := s.RestoreJSONText(red)
	if err != nil {
		t.Fatal(err)
	}
	if back != in {
		t.Fatalf("one-level nested restore must be byte-exact")
	}
}
