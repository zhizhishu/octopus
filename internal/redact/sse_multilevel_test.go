package redact

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// nestedArgs builds a chat tool-args JSON text whose PEM secret sits `depth` JSON
// strings deep, returning the args text and the key chain down to the leaf object
// (so the decoded pem leaf can be read back after a streaming restore).
func nestedArgs(t *testing.T, secret string, depth int) (string, []string) {
	t.Helper()
	leaf := mustJSON(t, map[string]string{"pem": secret})
	switch depth {
	case 2:
		return mustJSON(t, map[string]string{"k": leaf}), []string{"k"}
	case 3:
		mid := mustJSON(t, map[string]string{"k2": leaf})
		return mustJSON(t, map[string]string{"k": mid}), []string{"k", "k2"}
	}
	t.Fatalf("unsupported depth %d", depth)
	return "", nil
}

// innerPem walks keys down from args and returns the decoded pem leaf value.
func innerPem(t *testing.T, args string, keys []string) string {
	t.Helper()
	cur := args
	for _, k := range keys {
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(cur), &m); err != nil {
			t.Fatalf("level %q not JSON: %v (cur=%q)", k, err, cur)
		}
		raw, ok := m[k]
		if !ok {
			t.Fatalf("key %q missing in %q", k, cur)
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Fatalf("key %q not a string: %v", k, err)
		}
		cur = s
	}
	var leaf struct {
		Pem string `json:"pem"`
	}
	if err := json.Unmarshal([]byte(cur), &leaf); err != nil {
		t.Fatalf("leaf not JSON: %v (cur=%q)", err, cur)
	}
	return leaf.Pem
}

// driveSplit feeds redArgs split at `cut` into a fresh stream restorer and returns
// everything it emitted (ingests + finish).
func driveSplit(t *testing.T, s *Session, redArgs string, cut int) string {
	t.Helper()
	r, err := s.NewSseRestorer()
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.Ingest(chatArgsDelta(t, redArgs[:cut]))
	if err != nil {
		t.Fatalf("ingest 1: %v", err)
	}
	out2, err := r.Ingest(chatArgsDelta(t, redArgs[cut:]))
	if err != nil {
		t.Fatalf("ingest 2: %v", err)
	}
	tail, err := r.Finish()
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	return out + out2 + tail
}

// TestSseRestoreMultiLevelToolArgsExactAcrossSplits: a placeholder nested >=2 JSON
// strings deep must restore byte-exactly no matter where the SSE deltas split it
// (every byte boundary), the reassembled arguments equal to the original, and the
// decoded pem leaf equal to the multi-line secret.
func TestSseRestoreMultiLevelToolArgsExactAcrossSplits(t *testing.T) {
	for _, depth := range []int{2, 3} {
		func() {
			s := newFidelitySession(t, "G")
			defer s.Close()
			pem := testPEM()
			args, keys := nestedArgs(t, pem, depth)
			red, err := s.RedactJSONText(args)
			if err != nil {
				t.Fatalf("depth %d redact: %v", depth, err)
			}
			if strings.Contains(red, "PRIVATE KEY") || !strings.Contains(red, "{{Redact:") {
				t.Fatalf("depth %d: expected a placeholder, got %q", depth, red)
			}
			for cut := 1; cut < len(red); cut++ {
				got := chatSseArgs(t, driveSplit(t, s, red, cut))
				if strings.Contains(got, "{{Redact:") {
					t.Fatalf("depth %d cut %d: placeholder leaked: %q", depth, cut, got)
				}
				if got != args {
					t.Fatalf("depth %d cut %d: reassembled args not byte-exact\n got=%q\nwant=%q", depth, cut, got, args)
				}
				if p := innerPem(t, got, keys); p != pem {
					t.Fatalf("depth %d cut %d: decoded pem mismatch", depth, cut)
				}
			}
		}()
	}
}

// TestSseRestoreStreamKeepsNonPlaceholderBytes: streaming must only rewrite the
// placeholder span -- a >2^53 integer literal and pre-existing escapes inside the
// arguments text survive byte-for-byte at several split points.
func TestSseRestoreStreamKeepsNonPlaceholderBytes(t *testing.T) {
	s := newFidelitySession(t, "G")
	defer s.Close()
	token, pem := redactPEM(t, s, "openai_chat")
	literal := `{"n":123456789012345678901234567890,"s":"a  b\tc","pem":"` + token + `"}`
	want := strings.Replace(literal, token, jsonEscapeForTest(pem), 1)
	for _, cut := range []int{1, 5, 13, 20, len(literal) / 2, len(literal) - 1} {
		got := chatSseArgs(t, driveSplit(t, s, literal, cut))
		if got != want {
			t.Fatalf("cut %d: non-placeholder bytes must be preserved\n got=%q\nwant=%q", cut, got, want)
		}
	}
}

// TestSseRestoreFinishFailsOnIndeterminateRegisteredPrefix: a stream that ends on a
// placeholder prefix whose hex is >=1 and IS a byte prefix of a registered token is
// byte-identical to a user literal, so Finish must FAIL CLOSED (explicit error) at
// every hex length 1..64 (full 64 hex without the closing braces) and every split
// point -- never emitting half a placeholder.
func TestSseRestoreFinishFailsOnIndeterminateRegisteredPrefix(t *testing.T) {
	s := newFidelitySession(t, "G")
	defer s.Close()
	token, _ := redactPEM(t, s, "openai_chat")

	for n := 1; n <= 64; n++ {
		frag := `{"pem":"` + token[:len("{{Redact:")+n]
		for cut := 1; cut < len(frag); cut++ {
			r, err := s.NewSseRestorer()
			if err != nil {
				t.Fatal(err)
			}
			got, err := r.Ingest(chatArgsDelta(t, frag[:cut]))
			if err != nil {
				t.Fatalf("hex %d cut %d: ingest 1: %v", n, cut, err)
			}
			got2, err := r.Ingest(chatArgsDelta(t, frag[cut:]))
			if err != nil {
				t.Fatalf("hex %d cut %d: ingest 2: %v", n, cut, err)
			}
			tail, err := r.Finish()
			if err == nil {
				t.Fatalf("hex %d cut %d: Finish must fail on an indeterminate registered prefix, emitted %q", n, cut, got+got2+tail)
			}
			if strings.Contains(got+got2+tail, token[:len("{{Redact:")+n]) {
				t.Fatalf("hex %d cut %d: half placeholder leaked: %q", n, cut, got+got2+tail)
			}
		}
	}
}

// TestSseRestoreFinishEmitsLiteralPlaceholderLikeTails: a bare prefix (zero hex), an
// unknown (non-hex) prefix and a hex prefix that is NOT a registered token's prefix
// are ordinary user text -- Finish succeeds and the bytes are emitted verbatim.
func TestSseRestoreFinishEmitsLiteralPlaceholderLikeTails(t *testing.T) {
	s := newFidelitySession(t, "G")
	defer s.Close()
	token, _ := redactPEM(t, s, "openai_chat")
	hex := token[len("{{Redact:"):]
	alt := "0"
	if hex[0] == '0' {
		alt = "1"
	}
	cases := map[string]string{
		"bare":       "{{Redact:",
		"unknown":    "{{Redact:zzzz",
		"nonreg-hex": "{{Redact:" + alt + hex[1:8],
	}
	for name, lit := range cases {
		r, err := s.NewSseRestorer()
		if err != nil {
			t.Fatal(err)
		}
		got, err := r.Ingest(chatArgsDelta(t, `{"pem":"`+lit))
		if err != nil {
			t.Fatalf("%s: ingest: %v", name, err)
		}
		tail, err := r.Finish()
		if err != nil {
			t.Fatalf("%s: a literal tail must not fail Finish: %v", name, err)
		}
		if combined := chatSseArgs(t, got+tail); !strings.Contains(combined, lit) {
			t.Fatalf("%s: literal tail must be emitted verbatim\n got=%q\nwant substring=%q", name, combined, lit)
		}
	}
}

// TestSseRestoreQueueCapCoversHeartbeatAndDone: heartbeat / [DONE] events that cannot
// drain behind an unsafe head must still hit the pending-event cap (the three ingest
// branches share one enqueue), so a blocked stream cannot grow the queue unbounded.
func TestSseRestoreQueueCapCoversHeartbeatAndDone(t *testing.T) {
	s := newFidelitySession(t, "G")
	defer s.Close()
	token, _ := redactPEM(t, s, "openai_chat")

	for _, filler := range []string{": keepalive", "data: [DONE]"} {
		r, err := s.NewSseRestorer()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.Ingest(chatArgsDelta(t, `{"pem":"`+token[:20])); err != nil {
			t.Fatalf("head ingest: %v", err)
		}
		emitted := ""
		var capErr error
		for i := 0; i < 5000; i++ {
			out, err := r.Ingest(filler)
			if err != nil {
				capErr = err
				break
			}
			emitted += out
		}
		if capErr == nil {
			t.Fatalf("%q: blocked heartbeats must hit the pending-event cap", filler)
		}
		if strings.Contains(emitted, "{{Redact:") {
			t.Fatalf("%q: half placeholder leaked: %q", filler, emitted)
		}
	}
}

// TestSseRestoreHugeLeadingWhitespaceKeepsJsonEscaping: arbitrarily long leading
// whitespace before the JSON container must never latch the channel to plain text
// (which would drop the per-level escaping). The whitespace is scanned, not buffered.
func TestSseRestoreHugeLeadingWhitespaceKeepsJsonEscaping(t *testing.T) {
	s := newFidelitySession(t, "G")
	defer s.Close()
	token, pem := redactPEM(t, s, "openai_chat")

	r, err := s.NewSseRestorer()
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	got, err := r.Ingest(chatArgsDelta(t, strings.Repeat(" ", 100000)))
	if err != nil {
		t.Fatalf("whitespace ingest: %v", err)
	}
	out.WriteString(got)
	got, err = r.Ingest(chatArgsDelta(t, `{"pem":"`+token+`"}`))
	if err != nil {
		t.Fatalf("json ingest: %v", err)
	}
	out.WriteString(got)
	tail, err := r.Finish()
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	out.WriteString(tail)
	if strings.Contains(out.String(), "{{Redact:") {
		t.Fatalf("placeholder leaked: %q", out.String())
	}
	assertPemArgs(t, chatSseArgs(t, out.String()), pem)
}

// TestSseRestoreLongStreamsDoNotAccumulate: a large stream that drains each event
// (no-hit passthrough, or completed placeholders) must never be rejected by the
// pending budget, which measures PENDING payload, not the cumulative stream.
func TestSseRestoreLongStreamsDoNotAccumulate(t *testing.T) {
	s := newFidelitySession(t, "G")
	defer s.Close()
	token, _ := redactPEM(t, s, "openai_chat")

	r, err := s.NewSseRestorer()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3000; i++ {
		if _, err := r.Ingest(chatArgsDelta(t, fmt.Sprintf(`{"n":%d,"s":"plain text %d with no secret"}`, i, i))); err != nil {
			t.Fatalf("no-hit event %d rejected: %v", i, err)
		}
	}
	for i := 0; i < 1000; i++ {
		if _, err := r.Ingest(chatArgsDelta(t, `{"pem":"`+token+`"}`)); err != nil {
			t.Fatalf("token event %d rejected: %v", i, err)
		}
	}
	if _, err := r.Finish(); err != nil {
		t.Fatalf("finish: %v", err)
	}
}

// Drain used to JSON.stringify the whole SSE data object, which reflowed
// whitespace and rounded integers >2^53. The envelope must keep original bytes
// except the restored string slot.
func TestSseRestoreKeepsEnvelopeWhitespaceAndBigInteger(t *testing.T) {
	s := newFidelitySession(t, "G")
	defer s.Close()
	token, pem := redactPEM(t, s, "openai_chat")

	raw := `data: { "n" : 9007199254740993, "choices" : [ { "index" : 0, "delta" : { "tool_calls" : [ { "index" : 0, "function" : { "arguments" : "{\"pem\":\"` + token + `\"}" } } ] } } ] }`
	r, err := s.NewSseRestorer()
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Ingest(raw)
	if err != nil {
		t.Fatal(err)
	}
	tail, err := r.Finish()
	if err != nil {
		t.Fatal(err)
	}
	out := got + tail
	if strings.Contains(out, "{{Redact:") {
		t.Fatalf("placeholder leaked: %q", out)
	}
	if !strings.Contains(out, "9007199254740993") {
		t.Fatalf("big integer must stay verbatim, got %q", out)
	}
	if !strings.Contains(out, `{ "n" : 9007199254740993, "choices" : [`) {
		t.Fatalf("envelope whitespace must be preserved, got %q", out)
	}
	if p := innerPem(t, chatSseArgs(t, out), nil); p != pem {
		t.Fatalf("decoded pem mismatch: %q", p)
	}
}
