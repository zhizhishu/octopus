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
		// Retype before dropping (sub2api responses_client_tools.go:310
		// retypedResponsesToolCallItemID): a custom_tool_call replayed with the fc_
		// prefix octopus used to mint is re-prefixed to ctc_ with the suffix kept,
		// so the id stays stable and unique and the upstream accepts it. Only ids
		// carrying no known tool-call prefix fall through to deletion.
		if retyped := RetypeResponsesToolCallItemID(probe.ID, probe.Type); retyped != probe.ID && !ShouldDropResponsesInputItemID(probe.Type, retyped) {
			idJSON, err := json.Marshal(retyped)
			if err == nil {
				itemObject["id"] = json.RawMessage(idJSON)
				fixedItem, err := encodeJSONNoHTMLEscape(itemObject)
				if err != nil {
					continue
				}
				items[index] = fixedItem
				changed = true
				continue
			}
		}
		delete(itemObject, "id")
		fixedItem, err := encodeJSONNoHTMLEscape(itemObject)
		if err != nil {
			continue
		}
		items[index] = fixedItem
		changed = true
	}
	if !changed {
		return raw, false
	}
	sanitized, err := encodeJSONNoHTMLEscape(items)
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
		return !hasResponsesIDPrefix(id, "msg_")
	case "function_call":
		return !hasResponsesIDPrefix(id, "fc_")
	case "custom_tool_call":
		// Real OpenAI mints ctc_ for custom tool call items and strict upstreams
		// reject anything else ("Expected an ID that begins with 'ctc'"). The
		// earlier fc_-accepting premise was wrong for strict upstreams: fc_ ids on
		// custom_tool_call are now retyped (see sanitizeResponsesInputItemIDsRaw)
		// or dropped, never forwarded as-is.
		return !hasResponsesIDPrefix(id, "ctc_")
	case "reasoning":
		return !hasResponsesIDPrefix(id, "rs_")
	case "image_generation_call":
		return !hasResponsesIDPrefix(id, "ig_")
	default:
		return false
	}
}

func hasResponsesIDPrefix(id string, prefix string) bool {
	return strings.HasPrefix(strings.TrimSpace(id), prefix)
}

// responsesToolCallItemIDPrefixes lists the Responses item id prefixes tied to a
// specific tool-call item type. Copied from sub2api
// backend/internal/pkg/apicompat/responses_client_tools.go:299.
var responsesToolCallItemIDPrefixes = []string{"fc_", "ctc_", "tsc_"}

// responsesToolCallItemIDPrefix reports the id prefix the Responses API validates
// for itemType, or "" when the type constrains no prefix. Copied from sub2api
// responses_client_tools.go:303 responsesToolCallItemIDPrefix.
func responsesToolCallItemIDPrefix(itemType string) string {
	switch itemType {
	case "custom_tool_call":
		return "ctc_"
	case "tool_search_call":
		return "tsc_"
	case "function_call":
		return "fc_"
	default:
		return ""
	}
}

// RetypeResponsesToolCallItemID re-prefixes an item id so it agrees with the item
// type. A client replaying a custom_tool_call whose id still carries the fc_
// prefix octopus used to mint would otherwise be rejected upstream with
// "Invalid 'input[N].id' ... Expected an ID that begins with 'ctc'".
// The suffix is preserved so the id stays stable and unique per item. ids that
// carry no known tool-call prefix are returned unchanged rather than guessed at.
// Copied from sub2api responses_client_tools.go:310 retypedResponsesToolCallItemID.
func RetypeResponsesToolCallItemID(id, itemType string) string {
	want := responsesToolCallItemIDPrefix(itemType)
	if want == "" || id == "" || strings.HasPrefix(id, want) {
		return id
	}
	for _, known := range responsesToolCallItemIDPrefixes {
		if known != want && strings.HasPrefix(id, known) {
			return want + strings.TrimPrefix(id, known)
		}
	}
	return id
}
