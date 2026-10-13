package relay

import (
	"encoding/json"
	"testing"
)

// client_metadata is carried as opaque bytes (json.RawMessage): a genuine codex
// client sends it in its own serde key order, so the relay must replay those exact
// bytes rather than decode -> Go map -> marshal (which reorders keys
// alphabetically). This pins the passthrough ORDER guarantee — the client's
// member sequence, including session_id, is preserved byte-for-byte.
func TestEnsureCodexClientMetadataPreservesClientKeyOrder(t *testing.T) {
	// The real codex 0.161 client order observed on the wire (turn-metadata first,
	// session_id after turn_id and before root_turn_id).
	raw := json.RawMessage(`{"x-codex-turn-metadata":"{}","thread_id":"t",` +
		`"x-codex-installation-id":"i","x-codex-window-id":"s:0",` +
		`"turn_id":"u","session_id":"s","root_turn_id":"r"}`)

	got := ensureCodexClientMetadata(raw, "sess", "install")
	if string(got) != string(raw) {
		t.Fatalf("complete client_metadata must pass through byte-for-byte, got %s", got)
	}

	// The passthrough bytes keep the client's key order (JSON object order is the
	// source order, not sorted).
	var m map[string]json.RawMessage
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("passthrough is not valid JSON: %v", err)
	}
	if len(m) != 7 {
		t.Fatalf("expected the client's 7 keys to survive, got %d (%v)", len(m), m)
	}
}

// Only a metadata block MISSING installation_id / window_id / turn-metadata (a
// non-codex client being cloaked) takes the rebuild path.
func TestEnsureCodexClientMetadataRebuildsOnlyWhenIncomplete(t *testing.T) {
	raw := json.RawMessage(`{"thread_id":"t"}`)
	got := ensureCodexClientMetadata(raw, "sess", "install")
	if string(got) == string(raw) {
		t.Fatalf("incomplete client_metadata should be rebuilt")
	}
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("rebuilt metadata invalid: %v", err)
	}
	if m["x-codex-installation-id"] != "install" {
		t.Fatalf("rebuilt metadata missing installation id: %v", m)
	}
	if m["x-codex-window-id"] != "sess:0" {
		t.Fatalf("rebuilt metadata missing window id: %v", m)
	}
	if _, ok := m["x-codex-turn-metadata"]; !ok {
		t.Fatalf("rebuilt metadata missing turn-metadata: %v", m)
	}
}
