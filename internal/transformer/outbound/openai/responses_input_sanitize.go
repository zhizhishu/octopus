package openai

import (
	"bytes"
	"encoding/json"
	"strings"
)

// sanitizeResponsesInputItemIDsRaw removes invalid `id` fields from client-replayed
// Responses input items before they are forwarded upstream.
//
// Why: octopus used to mint synthetic output-item ids with an "item_" prefix. A
// Responses client (codex/Cursor) faithfully echoes those ids back in the next
// request's input history, and strict upstreams validate the prefix per item type,
// rejecting the whole request: 400 "Invalid 'input[16].id': 'item_...'. Expected an
// ID that begins with 'msg'". Minting now uses official prefixes, but histories
// poisoned by older responses keep failing until the offending ids are dropped.
//
// Semantics mirror sub2api's sanitizeOpenAIResponsesInputItemIDs: an invalid id is
// DELETED, never rewritten (a fabricated msg_/fc_ id could collide with a real
// upstream object), the item itself is kept, and items whose id already carries a
// valid prefix — i.e. everything a genuine CLI echoes from a genuine backend — pass
// through untouched. Untouched items keep their exact original bytes (only the
// offending item is re-marshaled, with its field VALUES preserved as raw bytes), and
// when nothing needs fixing the original raw slice is returned unchanged — a no-op
// for healthy traffic.
func sanitizeResponsesInputItemIDsRaw(raw json.RawMessage) (json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' || !bytes.Contains(trimmed, []byte(`"id"`)) {
		return raw, false
	}
	var items []json.RawMessage
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return raw, false
	}
	changed := false
	for index, itemRaw := range items {
		var probe struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		}
		if err := json.Unmarshal(itemRaw, &probe); err != nil {
			continue
		}
		if !ShouldDropResponsesInputItemID(probe.Type, probe.ID) {
			continue
		}
		// Decode into RawMessage values so every kept field survives byte-for-byte
		// (no float64 round-trip, no string re-escaping); only the key set changes.
		var itemObject map[string]json.RawMessage
		if err := json.Unmarshal(itemRaw, &itemObject); err != nil {
			continue
		}
		delete(itemObject, "id")
		fixedItem, err := json.Marshal(itemObject)
		if err != nil {
			continue
		}
		items[index] = fixedItem
		changed = true
	}
	if !changed {
		return raw, false
	}
	sanitized, err := json.Marshal(items)
	if err != nil {
		return raw, false
	}
	return sanitized, true
}

// ShouldDropResponsesInputItemID reports whether a Responses input item's id must be
// removed before upstream forwarding. Only item types with a known enforced prefix
// are checked; unknown types (function_call_output, web_search_call, ...) always
// keep their id — mirrors the sub2api rules plus the reasoning/image types octopus
// used to poison with "item_" ids. Exported so the raw /responses/compact
// passthrough can apply the same rule to its map-shaped payload.
func ShouldDropResponsesInputItemID(itemType, id string) bool {
	if strings.TrimSpace(id) == "" {
		return false
	}
	switch itemType {
	case "message":
		return !strings.HasPrefix(id, "msg")
	case "function_call":
		return !strings.HasPrefix(id, "fc")
	case "custom_tool_call":
		// Real OpenAI mints ctc_ for custom tool call items; fc is accepted too since
		// octopus (and sub2api) key both tool-call families to the fc prefix.
		return !strings.HasPrefix(id, "fc") && !strings.HasPrefix(id, "ctc")
	case "reasoning":
		return !strings.HasPrefix(id, "rs")
	case "image_generation_call":
		return !strings.HasPrefix(id, "ig")
	default:
		return false
	}
}
