package redact

import (
	"strings"
	"testing"
)

// newTestEngine builds an engine or fails the test.
func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	e, err := NewEngine()
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return e
}

// TestCosyCoreParseFlags ports Cosy's "flags default to all and subsets are explicit".
func TestCosyCoreParseFlags(t *testing.T) {
	e := newTestEngine(t)
	s, err := e.NewSession("", "", true)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	defer s.Close()
	parse, err := s.fn("parseFlags")
	if err != nil {
		t.Fatal(err)
	}
	// default: all detectors
	v, err := parse(nil, s.vm.ToValue(""))
	if err != nil {
		t.Fatalf("parseFlags(''): %v", err)
	}
	for _, k := range []string{"highEntropy", "phone", "secret", "identity", "bank", "email", "gitleaks"} {
		if got := v.ToObject(s.vm).Get(k).Export(); got != true {
			t.Fatalf("default flags: %s = %v, want true", k, got)
		}
	}
	// subset eS
	v, err = parse(nil, s.vm.ToValue("eS"))
	if err != nil {
		t.Fatalf("parseFlags('eS'): %v", err)
	}
	o := v.ToObject(s.vm)
	if o.Get("secret").Export() != true || o.Get("email").Export() != true {
		t.Fatal("eS: secret/email should be true")
	}
	if o.Get("highEntropy").Export() != false || o.Get("phone").Export() != false {
		t.Fatal("eS: highEntropy/phone should be false")
	}
	// unknown flag errors
	if _, err := parse(nil, s.vm.ToValue("EX")); err == nil {
		t.Fatal("parseFlags('EX') should throw Unknown flag")
	}
}

// TestCosyCoreTokenizeBlocks ports Cosy's lossless-style block offsets test.
func TestCosyCoreTokenizeBlocks(t *testing.T) {
	e := newTestEngine(t)
	s, err := e.NewSession("", "", true)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	defer s.Close()
	fn, _ := s.fn("tokenizeBlocks")
	v, err := fn(nil, s.vm.ToValue("abc_def-123.foo"))
	if err != nil {
		t.Fatal(err)
	}
	arr := v.Export().([]any)
	want := []struct {
		value string
		start int
		end   int
	}{{"abc", 0, 3}, {"def", 4, 7}, {"123", 8, 11}, {"foo", 12, 15}}
	if len(arr) != len(want) {
		t.Fatalf("tokenizeBlocks: got %d blocks, want %d", len(arr), len(want))
	}
	for i, w := range want {
		m := arr[i].(map[string]any)
		if m["value"] != w.value || int(m["start"].(int64)) != w.start || int(m["end"].(int64)) != w.end {
			t.Fatalf("block %d = %v, want %v", i, m, w)
		}
	}
}

// TestCosyCoreDetectorCoverage ports "email, phone, sk, bank and Chinese ID candidates".
func TestCosyCoreDetectorCoverage(t *testing.T) {
	e := newTestEngine(t)
	s, err := e.NewSession("", "", true)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	defer s.Close()
	sk := "sk-" + strings.Repeat("A1", 30)
	text := "a@example.com 13800138000 " + sk + " 4111111111111111 11010519491231002X"
	fn, _ := s.fn("findSensitiveSpans")
	flags, _ := s.flagsValue()
	v, err := fn(nil, s.vm.ToValue(text), flags)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, item := range v.Export().([]any) {
		got[item.(map[string]any)["type"].(string)] = true
	}
	for _, want := range []string{"email", "phone", "bank", "identity"} {
		if !got[want] {
			t.Fatalf("detector %s missing; got %v", want, got)
		}
	}
	if !got["secret"] && !got["gitleaks"] {
		t.Fatalf("sk- key not detected; got %v", got)
	}
}

// TestSessionRedactRestoreRoundTrip ports "same plaintext reuses token and restore is
// exact" plus the full-body round trip through the Go API.
func TestSessionRedactRestoreRoundTrip(t *testing.T) {
	e := newTestEngine(t)
	s, err := e.NewSession("E", "openai_chat", false)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	defer s.Close()
	body := []byte(`{"model":"gpt-test","messages":[{"role":"user","content":"mail a@example.com and a@example.com"}]}`)
	out, err := s.RedactJSONBody(body)
	if err != nil {
		t.Fatalf("redact: %v", err)
	}
	if strings.Contains(string(out), "a@example.com") {
		t.Fatalf("secret survived redaction: %s", out)
	}
	if n := strings.Count(string(out), "{{Redact:"); n != 2 {
		t.Fatalf("want 2 placeholders, got %d: %s", n, out)
	}
	if s.Count() != 1 {
		t.Fatalf("want 1 distinct mapping, got %d", s.Count())
	}
	// model echo: the response contains only one of the placeholders
	tokStart := strings.Index(string(out), "{{Redact:")
	tokEnd := strings.Index(string(out)[tokStart:], "}}") + tokStart + 2
	token := string(out)[tokStart:tokEnd]
	resp := []byte(`{"id":"resp_1","choices":[{"message":{"role":"assistant","content":"the address is ` + token + `"}}]}`)
	restored, err := s.RestoreJSONBody(resp)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !strings.Contains(string(restored), "a@example.com") {
		t.Fatalf("placeholder not restored: %s", restored)
	}
	if strings.Count(string(restored), "a@example.com") != 1 {
		t.Fatalf("want exactly 1 restored email, got: %s", restored)
	}
}

// TestSessionNoHitPassthrough verifies a clean body comes back byte-identical.
func TestSessionNoHitPassthrough(t *testing.T) {
	e := newTestEngine(t)
	s, _ := e.NewSession("E", "openai_chat", false)
	defer s.Close()
	body := []byte(`{"model":"gpt-test","messages":[{"role":"user","content":"hello world"}]}`)
	out, err := s.RedactJSONBody(body)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(body) {
		t.Fatalf("clean body was rewritten: %s", out)
	}
	if s.Count() != 0 {
		t.Fatalf("unexpected redactions: %d", s.Count())
	}
}

// TestSessionHighEntropySyntheticCredential verifies the H detector catches an
// unlabeled random credential (Cosy's headline feature) and the round trip restores it.
func TestSessionHighEntropySyntheticCredential(t *testing.T) {
	e := newTestEngine(t)
	s, _ := e.NewSession("H", "openai_chat", false)
	defer s.Close()
	cred := "q7X9v2L5m8N4r6T1w3Y0z5A8b2C9d7F4"
	body := []byte(`{"model":"gpt-test","messages":[{"role":"user","content":"check this value: ` + cred + `"}]}`)
	out, err := s.RedactJSONBody(body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), cred) {
		t.Fatalf("high-entropy credential survived: %s", out)
	}
	restored, err := s.RestoreJSONBody(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(restored), cred) {
		t.Fatalf("credential not restored: %s", restored)
	}
}

// TestSessionNoticeInjection ports the notice tests: injected after redaction, into
// the first user message, and image data untouched.
func TestSessionNoticeInjection(t *testing.T) {
	e := newTestEngine(t)
	s, _ := e.NewSession("E", "openai_chat", true)
	defer s.Close()
	body := []byte(`{"model":"gpt-test","messages":[{"role":"user","content":"mail a@example.com"}]}`)
	out, err := s.RedactJSONBody(body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "Sensitive values are redacted") {
		t.Fatalf("notice missing: %s", out)
	}
	if !strings.Contains(string(out), "{{Redact:") {
		t.Fatalf("placeholder missing: %s", out)
	}
	if strings.Contains(string(out), "a@example.com") {
		t.Fatalf("email survived: %s", out)
	}
}

// TestSessionAnthropicImageUntouched ports the Anthropic image-block test: base64
// image data must never be redacted.
func TestSessionAnthropicImageUntouched(t *testing.T) {
	e := newTestEngine(t)
	s, _ := e.NewSession("HE", "anthropic_messages", false)
	defer s.Close()
	raw := strings.Repeat("A", 200)
	body := []byte(`{"model":"claude-test","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + raw + `"}},{"type":"text","text":"a@example.com"}]}]}`)
	out, err := s.RedactJSONBody(body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), raw) {
		t.Fatalf("image data was modified: %s", out)
	}
	if strings.Contains(string(out), "a@example.com") {
		t.Fatalf("email survived: %s", out)
	}
}

// TestSessionToolResultRedacted ports "tool results are redacted before they are
// forwarded to the model" — nested JSON inside a tool message string is parsed and
// redacted too.
func TestSessionToolResultRedacted(t *testing.T) {
	e := newTestEngine(t)
	s, _ := e.NewSession("E", "openai_chat", false)
	defer s.Close()
	body := []byte(`{"model":"gpt-test","messages":[{"role":"user","content":"check the tool result"},{"role":"tool","tool_call_id":"call_1","content":"{\"email\":\"alice@example.com\"}"}]}`)
	out, err := s.RedactJSONBody(body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "alice@example.com") {
		t.Fatalf("tool-result email survived: %s", out)
	}
	if !strings.Contains(string(out), "{{Redact:") {
		t.Fatalf("no placeholder in tool result: %s", out)
	}
}
