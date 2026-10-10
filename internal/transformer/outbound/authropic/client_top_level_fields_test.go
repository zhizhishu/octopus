package authropic

import (
	"context"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"

	inboundAnthropic "github.com/bestruirui/octopus/internal/transformer/inbound/anthropic"
)

// relayAnthropicBody runs a Claude request body through the real inbound -> internal ->
// outbound path and returns the exact bytes the relay would put on the wire. The
// outbound TransformRequest is used rather than a bare json.Marshal so the assertions
// cover the encoder the relay actually ships, including its HTML-escaping setting.
func relayAnthropicBody(t *testing.T, body string) string {
	t.Helper()

	internalReq, err := (&inboundAnthropic.MessagesInbound{}).TransformRequest(context.Background(), []byte(body))
	if err != nil {
		t.Fatalf("inbound TransformRequest error: %v", err)
	}
	req, err := (&MessageOutbound{}).TransformRequest(context.Background(), internalReq, "https://upstream.example", "test-key")
	if err != nil {
		t.Fatalf("outbound TransformRequest error: %v", err)
	}
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read outbound body: %v", err)
	}
	return string(raw)
}

// A Claude client's unmodelled top-level keys must survive a same-protocol relay. The
// client sends them for a reason; a relay that silently drops them hands the provider a
// different request than the CLI produced.
func TestAnthropicRelayPreservesUnknownTopLevelFields(t *testing.T) {
	const body = `{"model":"claude-opus-4-8","max_tokens":64,"stream":true,` +
		`"messages":[{"role":"user","content":"hi"}],` +
		`"safeguards":{"policy":"default","enabled":true},` +
		`"client_feature_flag":{"nested":[1,2,3]}}`

	wire := relayAnthropicBody(t, body)

	var decoded map[string]json.RawMessage
	if err := json.Unmarshal([]byte(wire), &decoded); err != nil {
		t.Fatalf("relayed body is not valid JSON: %v (%s)", err, wire)
	}
	for _, key := range []string{"safeguards", "client_feature_flag"} {
		raw, ok := decoded[key]
		if !ok {
			t.Fatalf("relayed body dropped top-level key %q: %s", key, wire)
		}
		if !json.Valid(raw) {
			t.Fatalf("relayed value for %q is not valid JSON: %s", key, raw)
		}
	}

	// The value must come through unchanged, not re-shaped or emptied.
	var safeguard struct {
		Policy  string `json:"policy"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.Unmarshal(decoded["safeguards"], &safeguard); err != nil {
		t.Fatalf("decode safeguards: %v", err)
	}
	if safeguard.Policy != "default" || !safeguard.Enabled {
		t.Fatalf("safeguards value changed in transit: %s", decoded["safeguards"])
	}
	if got := strings.TrimSpace(string(decoded["client_feature_flag"])); got != `{"nested":[1,2,3]}` {
		t.Fatalf("nested value changed in transit: %s", got)
	}
}

// Modelled keys keep the order the struct declares. The previous implementation
// round-tripped the whole object through map[string]json.RawMessage whenever it had to
// force "tools":[], which re-sorted EVERY key alphabetically and so rewrote the body
// shape of every no-tool Claude turn.
func TestAnthropicRelayKeepsFieldOrderWithForcedEmptyTools(t *testing.T) {
	const body = `{"model":"claude-opus-4-8","max_tokens":64,"temperature":1,"system":"be brief",` +
		`"tools":[],"stream":true,"messages":[{"role":"user","content":"hi"}]}`

	wire := relayAnthropicBody(t, body)

	// Declaration order puts `model` before `metadata`; alphabetical sorting would put
	// `metadata` first. `temperature` before `system` discriminates the same way.
	modelAt := strings.Index(wire, `"model"`)
	metadataAt := strings.Index(wire, `"metadata"`)
	if modelAt < 0 {
		t.Fatalf("relayed body lost the model field: %s", wire)
	}
	if metadataAt >= 0 && metadataAt < modelAt {
		t.Fatalf("relayed body was re-sorted alphabetically: %s", wire)
	}
	tempAt, systemAt := strings.Index(wire, `"temperature"`), strings.Index(wire, `"system"`)
	if tempAt < 0 || systemAt < 0 || systemAt < tempAt {
		t.Fatalf("expected temperature before system in declaration order: %s", wire)
	}

	// Claude Code's no-tool shape must survive, and it belongs before tool_choice/stream.
	toolsAt := strings.Index(wire, `"tools":[]`)
	if toolsAt < 0 {
		t.Fatalf("forced empty tools array was dropped: %s", wire)
	}
	if streamAt := strings.Index(wire, `"stream"`); streamAt >= 0 && streamAt < toolsAt {
		t.Fatalf("tools must precede stream: %s", wire)
	}
}

// Go's json.Marshal rewrites <, > and & as \u003c / \u003e / \u0026. A relayed body
// carrying markup or a shell `&&` must keep the client's bytes instead.
func TestAnthropicRelayDoesNotHTMLEscapeBodyContent(t *testing.T) {
	const body = `{"model":"claude-opus-4-8","max_tokens":64,` +
		`"messages":[{"role":"user","content":"if a < b && c > d then emit <div>&amp;</div>"}],` +
		`"system":"<policy>be terse</policy>"}`

	wire := relayAnthropicBody(t, body)

	if strings.Contains(wire, `\u003c`) || strings.Contains(wire, `\u0026`) || strings.Contains(wire, `\u003e`) {
		t.Fatalf("relayed body was HTML-escaped: %s", wire)
	}
	if !strings.Contains(wire, "a < b && c > d") {
		t.Fatalf("expected the client's literal comparison text to survive: %s", wire)
	}
}

// A tools array carried by the client is modelled normally, so the fidelity channel must
// not also replay it as an unknown key and emit a duplicate member.
func TestAnthropicRelayDoesNotDuplicateModelledKeys(t *testing.T) {
	const body = `{"model":"claude-opus-4-8","max_tokens":64,"stream":true,` +
		`"messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"name":"lookup","description":"d","input_schema":{"type":"object"}}]}`

	wire := relayAnthropicBody(t, body)

	if got := strings.Count(wire, `"stream"`); got != 1 {
		t.Fatalf("expected exactly one stream member, found %d: %s", got, wire)
	}
	if got := strings.Count(wire, `"tools"`); got != 1 {
		t.Fatalf("expected exactly one tools member, found %d: %s", got, wire)
	}
	if got := strings.Count(wire, `"max_tokens"`); got != 1 {
		t.Fatalf("expected exactly one max_tokens member, found %d: %s", got, wire)
	}
}

// The fidelity channel replays values it never interpreted, so it must hand back the
// client's own JSON rather than a re-encoded approximation of it.
func TestAnthropicExtraReplaysValueBytesVerbatim(t *testing.T) {
	const body = `{"model":"claude-opus-4-8","max_tokens":64,` +
		`"messages":[{"role":"user","content":"hi"}],` +
		`"safeguards":{"b":1,"a":2}}`

	wire := relayAnthropicBody(t, body)

	// Key order inside the preserved value is the client's business, not ours.
	if !strings.Contains(wire, `"safeguards":{"b":1,"a":2}`) {
		t.Fatalf("preserved value was re-encoded instead of replayed: %s", wire)
	}
}

// topLevelKeySequence reports an object's top-level keys in wire order.
func topLevelKeySequence(t *testing.T, wire string) []string {
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

// The sequence below is captured, not invented: it is the top-level key order a real
// Claude Code 2.1.294 request carried to the upstream on /v1/messages?beta=true,
// including `safeguards`, which this relay does not model. Members in a different order
// are a different request shape, so a same-protocol relay must replay the client's order.
func TestAnthropicRelayReplaysClientTopLevelKeyOrder(t *testing.T) {
	const body = `{"model":"claude-opus-4-8",` +
		`"messages":[{"role":"user","content":"pong"}],` +
		`"system":[{"type":"text","text":"sys"}],` +
		`"tools":[{"name":"Read","description":"read","input_schema":{"type":"object"}}],` +
		`"metadata":{"user_id":"u"},"max_tokens":64,` +
		`"thinking":{"type":"enabled","budget_tokens":1024},` +
		`"context_management":{"edits":[]},` +
		`"safeguards":{"policy":"default"},` +
		`"output_config":{"effort":"high"},"stream":true}`
	want := []string{
		"model", "messages", "system", "tools", "metadata", "max_tokens",
		"thinking", "context_management", "safeguards", "output_config", "stream",
	}

	wire := relayAnthropicBody(t, body)
	got := topLevelKeySequence(t, wire)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("relayed top-level key order does not replay the client's order: got %v want %v body: %s", got, want, wire)
	}
}

// A forced empty tools array (Claude Code's no-tool shape) is emitted at the position the
// client put `tools` in, not appended at the end.
func TestAnthropicRelayReplaysClientOrderWithForcedEmptyTools(t *testing.T) {
	const body = `{"model":"claude-opus-4-8","messages":[{"role":"user","content":"pong"}],` +
		`"system":[{"type":"text","text":"sys"}],"tools":[],` +
		`"safeguards":{"policy":"default"},"max_tokens":64,"stream":true}`
	want := []string{"model", "messages", "system", "tools", "safeguards", "max_tokens", "stream"}

	wire := relayAnthropicBody(t, body)
	if !strings.Contains(wire, `"tools":[]`) {
		t.Fatalf("forced empty tools array was dropped: %s", wire)
	}
	got := topLevelKeySequence(t, wire)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("relayed top-level key order with forced empty tools: got %v want %v body: %s", got, want, wire)
	}
}

// Fields this relay adds itself (the client never sent them) keep a stable position at the
// end, and a client key the relay does not model still keeps its own slot.
func TestAnthropicRelayAppendsRelayAddedFieldsAfterClientKeys(t *testing.T) {
	const body = `{"model":"claude-opus-4-8","messages":[{"role":"user","content":"pong"}],` +
		`"safeguards":{"policy":"default"},"stream":true}`
	want := []string{"model", "messages", "safeguards", "stream"}

	got := topLevelKeySequence(t, relayAnthropicBody(t, body))
	if !reflect.DeepEqual(got[:len(want)], want) {
		t.Fatalf("client keys did not keep their order: got %v want prefix %v", got, want)
	}
}
