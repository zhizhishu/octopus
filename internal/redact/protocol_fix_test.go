package redact

// Tests for the oct-only protocol-boundary patch (extractor transform 6, applied to
// cosy/worker-core.js): top-level protocol envelopes (tools/tool_choice, plus
// per-protocol identity/cursor/cache/schema keys) must survive redaction verbatim,
// while real business payloads (nested tool JSON and anthropic tool_use input) must
// still be scanned even when they use name/id/url envelope key names.
//
// All fixtures use synthetic credentials and the neutral upstream.example domain
// (public-repo rule: no real secrets, no upstream brand names).

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// he32 / he32b are synthetic 32-char high-entropy strings (no real secret).
const he32 = "q7X9v2L5m8N4r6T1w3Y0z5A8b2C9d7F4"
const he32b = "Z3n8P1q6R4t9W2y7U5iO0aS3d6F1g8H2"

// synthSk builds a synthetic sk- credential long enough for both the secret detector
// (>= 60 alnum after "sk-") and the gitleaks openai-api-key rule (>= 20).
func synthSk(n int) string { return "sk-" + strings.Repeat("A1", n) }

func decodeBody(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode body: %v (%s)", err, b)
	}
	return m
}

func hasRedactToken(s string) bool { return strings.Contains(s, "{{Redact:") }

// T1: anthropic metadata (identity device/session user_id) must be returned verbatim
// while the messages text is still redacted.
func TestAnthropicMetadataUntouched(t *testing.T) {
	e := newTestEngine(t)
	s, err := e.NewSession("HE", "anthropic_messages", false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	body := []byte(fmt.Sprintf(`{"model":"claude-test","max_tokens":16,"metadata":{"user_id":"device_id_%s_session_id_s-1"},"messages":[{"role":"user","content":"please mail a@example.com"}]}`, he32))
	out, err := s.RedactJSONBody(body)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(out), he32) {
		t.Fatalf("metadata.user_id was modified: %s", out)
	}
	in, got := decodeBody(t, body), decodeBody(t, out)
	if !reflect.DeepEqual(got["metadata"], in["metadata"]) {
		t.Fatalf("metadata changed: %v -> %v", in["metadata"], got["metadata"])
	}
	if !hasRedactToken(string(out)) {
		t.Fatalf("message text was not redacted: %s", out)
	}
	if strings.Contains(string(out), "a@example.com") {
		t.Fatalf("email survived redaction: %s", out)
	}
}

// T2: responses cursor/cache envelopes (previous_response_id, prompt_cache_key) must be
// returned verbatim while the input text is still redacted.
func TestResponsesEnvelopeUntouched(t *testing.T) {
	e := newTestEngine(t)
	s, err := e.NewSession("HE", "openai_responses", false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	body := []byte(fmt.Sprintf(`{"model":"g","previous_response_id":"resp_%s","prompt_cache_key":"%s","input":"please mail a@example.com"}`, he32, he32b))
	out, err := s.RedactJSONBody(body)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(out), "resp_"+he32) {
		t.Fatalf("previous_response_id was modified: %s", out)
	}
	if !strings.Contains(string(out), he32b) {
		t.Fatalf("prompt_cache_key was modified: %s", out)
	}
	if !hasRedactToken(string(out)) {
		t.Fatalf("input text was not redacted: %s", out)
	}
	if strings.Contains(string(out), "a@example.com") {
		t.Fatalf("input email survived redaction: %s", out)
	}
}

// T3: a top-level tools declaration (required list, property schemas, descriptions) must
// be byte-identical after redaction, even when it contains redactable-looking text.
func TestChatToolsUntouched(t *testing.T) {
	e := newTestEngine(t)
	s, err := e.NewSession("HES", "openai_chat", false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	sk := synthSk(40)
	body := []byte(fmt.Sprintf(`{"model":"g","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"lookup","description":"find by email a@example.com using %s","parameters":{"type":"object","properties":{"%s":{"type":"string","description":"api key %s"}},"required":["%s"]}}}]}`, sk, he32, sk, he32))
	// Sanity: the fixture really does contain redactable text inside tools.
	if !strings.Contains(string(body), "a@example.com") || !strings.Contains(string(body), sk) {
		t.Fatal("fixture missing redactable content inside tools")
	}

	out, err := s.RedactJSONBody(body)
	if err != nil {
		t.Fatal(err)
	}

	in, got := decodeBody(t, body), decodeBody(t, out)
	if !reflect.DeepEqual(got["tools"], in["tools"]) {
		t.Fatalf("tools subtree changed:\n in=%v\nout=%v", in["tools"], got["tools"])
	}
}

// T4: business data inside an anthropic tool_use input object (including nested JSON tool
// arguments) must be scanned even when it lives under name/id/url envelope key names.
func TestAnthropicToolUseInputScanned(t *testing.T) {
	e := newTestEngine(t)
	s, err := e.NewSession("ES", "anthropic_messages", false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	sk1 := synthSk(40)
	sk2 := "sk-" + strings.Repeat("B2", 40)
	sk3 := "sk-" + strings.Repeat("C3", 40)
	// A JSON-encoded tool-argument string whose *decoded* value is a business object.
	nested := fmt.Sprintf(`{\"url\":\"https://upstream.example/x?k=%s\",\"name\":\"bob@example.com\",\"id\":\"%s\"}`, sk3, sk3)

	body := []byte(fmt.Sprintf(`{"model":"claude-test","max_tokens":16,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"send","input":{"url":"https://upstream.example/y?k=%s","name":"alice@example.com","id":"%s"}},{"type":"tool_use","id":"toolu_2","name":"send2","input":{"json_args":"%s"}}]}]}`, sk1, sk2, nested))
	out, err := s.RedactJSONBody(body)
	if err != nil {
		t.Fatal(err)
	}

	ob := string(out)
	for _, raw := range []string{sk1, sk2, sk3, "alice@example.com", "bob@example.com"} {
		if strings.Contains(ob, raw) {
			t.Fatalf("business payload value survived redaction: %q in %s", raw, ob)
		}
	}
	if !hasRedactToken(ob) {
		t.Fatalf("no placeholder emitted for tool_use input: %s", ob)
	}
}

// T5: chat attachments (image_url data URI) and anthropic image source.data must never be
// touched, while surrounding text is still redacted.
func TestAttachmentsUntouched(t *testing.T) {
	b64 := strings.Repeat("QWxhZGRpbjpvcGVuIHNlc2FtZQ", 8) // 192 base64 chars, no escapes
	e := newTestEngine(t)

	t.Run("openai_chat_image_url", func(t *testing.T) {
		s, err := e.NewSession("HES", "openai_chat", false)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		body := []byte(`{"model":"g","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + b64 + `"}},{"type":"text","text":"mail a@example.com"}]}]}`)
		out, err := s.RedactJSONBody(body)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(out), b64) {
			t.Fatalf("image_url data was modified: %s", out)
		}
		if strings.Contains(string(out), "a@example.com") {
			t.Fatalf("text was not redacted: %s", out)
		}
	})

	t.Run("anthropic_source_data", func(t *testing.T) {
		s, err := e.NewSession("HES", "anthropic_messages", false)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		body := []byte(`{"model":"c","max_tokens":16,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + b64 + `"}},{"type":"text","text":"mail a@example.com"}]}]}`)
		out, err := s.RedactJSONBody(body)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(out), b64) {
			t.Fatalf("source.data was modified: %s", out)
		}
		if strings.Contains(string(out), "a@example.com") {
			t.Fatalf("text was not redacted: %s", out)
		}
	})
}

// T6: redact -> restore is an identity on the whole body (including the skipped envelopes)
// for all three inbound protocols.
func TestRedactRestoreRoundTripProtocols(t *testing.T) {
	e := newTestEngine(t)
	sk := synthSk(40)
	cases := []struct {
		name     string
		protocol string
		body     string
	}{
		{
			"openai_chat", "openai_chat",
			fmt.Sprintf(`{"model":"g","prompt_cache_key":"%s","messages":[{"role":"user","content":"mail a@example.com key %s"}],"tools":[{"type":"function","function":{"name":"f","description":"d"}}]}`, he32, sk),
		},
		{
			"openai_responses", "openai_responses",
			fmt.Sprintf(`{"model":"g","previous_response_id":"resp_%s","prompt_cache_key":"%s","input":"mail a@example.com key %s","tools":[{"type":"function","name":"f"}]}`, he32, he32b, sk),
		},
		{
			"anthropic_messages", "anthropic_messages",
			fmt.Sprintf(`{"model":"c","max_tokens":16,"metadata":{"user_id":"%s"},"messages":[{"role":"user","content":"mail a@example.com key %s"}],"tools":[{"name":"f","description":"d"}]}`, he32, sk),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := e.NewSession("HES", tc.protocol, false)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()

			body := []byte(tc.body)
			red, err := s.RedactJSONBody(body)
			if err != nil {
				t.Fatalf("redact: %v", err)
			}
			if s.Count() == 0 {
				t.Fatal("fixture produced no redactions")
			}
			back, err := s.RestoreJSONBody(red)
			if err != nil {
				t.Fatalf("restore: %v", err)
			}
			if !reflect.DeepEqual(decodeBody(t, back), decodeBody(t, body)) {
				t.Fatalf("round trip mismatch:\n in=%s\nout=%s", body, back)
			}
		})
	}
}
