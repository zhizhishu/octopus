package openai

import (
	"context"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"

	inboundOpenAI "github.com/bestruirui/octopus/internal/transformer/inbound/openai"
)

// relayResponsesBody runs a Responses request body through the real inbound -> internal ->
// outbound path and returns the exact bytes the relay would put on the wire.
func relayResponsesBody(t *testing.T, body string) string {
	t.Helper()

	internalReq, err := (&inboundOpenAI.ResponseInbound{}).TransformRequest(context.Background(), []byte(body))
	if err != nil {
		t.Fatalf("inbound TransformRequest error: %v", err)
	}
	req, err := (&ResponseOutbound{}).TransformRequest(context.Background(), internalReq, "https://upstream.example", "test-key")
	if err != nil {
		t.Fatalf("outbound TransformRequest error: %v", err)
	}
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read outbound body: %v", err)
	}
	return string(raw)
}

// wireKeySequence reports an object's top-level keys in wire order.
func wireKeySequence(t *testing.T, wire string) []string {
	t.Helper()

	dec := json.NewDecoder(strings.NewReader(wire))
	if _, err := dec.Token(); err != nil {
		t.Fatalf("relayed body is not a JSON object: %v (%s)", err, wire)
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("decode key: %v (%s)", err, wire)
		}
		key, ok := tok.(string)
		if !ok {
			t.Fatalf("expected string key, got %T (%s)", tok, wire)
		}
		keys = append(keys, key)
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatalf("decode value of %q: %v (%s)", key, err, wire)
		}
	}
	return keys
}

// A captured genuine Codex CLI body carries <, > and & raw. Escaping them was worth 535
// bytes on an otherwise identical request, so the relay must not escape what the client
// sent. The prompt text below stands in for the markup and shell operators a real body
// carries in its instructions and tool descriptions.
func TestResponsesRelayDoesNotHTMLEscapeBody(t *testing.T) {
	const body = `{"model":"gpt-6-astra","stream":true,"input":[{"type":"message","role":"user",` +
		`"content":[{"type":"input_text","text":"if a < b && c > d then run"}]}]}`

	wire := relayResponsesBody(t, body)

	for _, escaped := range []string{`\u003c`, `\u003e`, `\u0026`} {
		if strings.Contains(wire, escaped) {
			t.Fatalf("relayed body still HTML-escapes %s: %s", escaped, wire)
		}
	}
	if !strings.Contains(wire, `if a < b && c > d then run`) {
		t.Fatalf("relayed body lost the client's raw characters: %s", wire)
	}
}

// The order below is captured, not invented: it is the top-level key sequence a real Codex
// CLI request carried on /v1/responses. Members in a different order are a different
// request shape, so a same-protocol relay must replay the client's order.
func TestResponsesRelayReplaysClientTopLevelKeyOrder(t *testing.T) {
	const body = `{"model":"gpt-6-astra","stream":true,` +
		`"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"pong"}]}],` +
		`"tool_choice":"auto","parallel_tool_calls":false,"reasoning":{"effort":"high"},` +
		`"store":false,"include":["reasoning.encrypted_content"],"prompt_cache_key":"cache-key",` +
		`"text":{"verbosity":"low"},"client_metadata":{"origin":"test"}}`
	want := []string{
		"model", "stream", "input", "tool_choice", "parallel_tool_calls", "reasoning",
		"store", "include", "prompt_cache_key", "text", "client_metadata",
	}

	got := wireKeySequence(t, relayResponsesBody(t, body))
	if len(got) < len(want) {
		t.Fatalf("relayed body dropped client keys: got %v want at least %v", got, want)
	}
	// Fields this transformer adds itself carry no client order and must trail the client's.
	if !reflect.DeepEqual(got[:len(want)], want) {
		t.Fatalf("relayed top-level key order does not replay the client's order: got %v want %v", got, want)
	}
}

// An ordinary Responses body (no extra client keys) still leads with its model: the
// reorder pass must never move a member the client itself put first.
func TestResponsesRelayKeepsModelFirstForOrdinaryBody(t *testing.T) {
	const body = `{"model":"gpt-6-astra","stream":true,"input":[{"type":"message","role":"user",` +
		`"content":[{"type":"input_text","text":"pong"}]}]}`

	wire := relayResponsesBody(t, body)
	got := wireKeySequence(t, wire)
	if len(got) == 0 || got[0] != "model" {
		t.Fatalf("relayed body lost its model field: %v (%s)", got, wire)
	}
}
