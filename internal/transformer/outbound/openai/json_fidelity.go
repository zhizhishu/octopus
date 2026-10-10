package openai

import (
	"bytes"
	"encoding/json"
	"sort"
)

// encodeJSONNoHTMLEscape serialises v the way the CLI's own JSON serialiser does. Go's
// json.Marshal rewrites <, > and & as \u003c / \u003e / \u0026 in every string it
// touches - including strings carried inside a json.RawMessage, which the encoder
// re-compacts with the same escaping. A captured genuine Codex CLI body carries those
// characters raw, and escaping them inflated the relayed body by 535 bytes on an
// otherwise identical request (53x `\u003c` + 54x `\u003e`, five bytes each).
func encodeJSONNoHTMLEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// Encoder.Encode appends a newline the CLI's serialiser does not emit.
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// memberKeyStart walks back from the offset just past a member's key token to the opening
// quote of that key. The encoded object is compact, so only whitespace and the separating
// colon sit in between.
func memberKeyStart(data []byte, afterKeyToken int64) int {
	i := int(afterKeyToken) - 1
	for i >= 0 && (data[i] == ' ' || data[i] == ':' || data[i] == '\n' || data[i] == '\t' || data[i] == '\r') {
		i--
	}
	if i < 0 || data[i] != '"' {
		return -1
	}
	for j := i - 1; j >= 0; j-- {
		if data[j] != '"' {
			continue
		}
		backslashes := 0
		for k := j - 1; k >= 0 && data[k] == '\\'; k-- {
			backslashes++
		}
		if backslashes%2 == 0 {
			return j // an unescaped quote opens the key
		}
	}
	return -1
}

// reorderTopLevelMembers re-emits a compact JSON object with its members in `order`.
// Members the client did not send (fields this transformer adds) keep their relative
// position at the end. Member bytes are copied verbatim - only the sequence changes.
// An empty order, or any object this scanner cannot follow, returns data untouched.
func reorderTopLevelMembers(data []byte, order []string) []byte {
	if len(order) == 0 {
		return data
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return data
	}
	if delim, isDelim := tok.(json.Delim); !isDelim || delim != '{' {
		return data
	}

	type member struct {
		key   string
		start int
		end   int
	}
	var members []member
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return data
		}
		key, isString := keyTok.(string)
		if !isString {
			return data
		}
		afterKey := dec.InputOffset()
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return data
		}
		start := memberKeyStart(data, afterKey)
		if start < 0 {
			return data
		}
		members = append(members, member{key: key, start: start, end: int(dec.InputOffset())})
	}
	if len(members) == 0 {
		return data
	}

	// Each member's span runs from its key's opening quote to the end of its value, so the
	// separating commas are excluded and the members can be re-emitted in any order.
	text := make([]string, len(members))
	for i, mem := range members {
		if mem.start < 0 || mem.start >= mem.end || mem.end > len(data) {
			return data
		}
		text[i] = string(data[mem.start:mem.end])
	}

	rank := make(map[string]int, len(order))
	for i, key := range order {
		if _, seen := rank[key]; !seen {
			rank[key] = i
		}
	}
	idx := make([]int, len(members))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		ra, oka := rank[members[idx[a]].key]
		rb, okb := rank[members[idx[b]].key]
		switch {
		case oka && okb:
			return ra < rb
		case oka:
			return true
		case okb:
			return false
		default:
			return false // neither key came from the client: keep the encoded order
		}
	})

	var buf bytes.Buffer
	buf.Grow(len(data) + 8)
	buf.WriteByte('{')
	for i, j := range idx {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteString(text[j])
	}
	buf.WriteByte('}')
	return buf.Bytes()
}
