package anthropic

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestToolUnmarshalPreservesRawAndClassifies(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		isBuiltin bool
	}{
		{
			name:      "computer use",
			raw:       `{"type":"computer_20250124","name":"computer","display_width_px":1280,"display_height_px":800,"display_number":1}`,
			isBuiltin: true,
		},
		{
			name:      "bash",
			raw:       `{"type":"bash_20250101","name":"bash"}`,
			isBuiltin: true,
		},
		{
			name:      "text editor",
			raw:       `{"type":"text_editor_20250728","name":"str_replace_based_edit_tool"}`,
			isBuiltin: true,
		},
		{
			name:      "custom tool without type",
			raw:       `{"name":"lookup","description":"lookup data","input_schema":{"type":"object"}}`,
			isBuiltin: false,
		},
		{
			name:      "custom tool with explicit type",
			raw:       `{"type":"custom","name":"lookup","input_schema":{"type":"object"}}`,
			isBuiltin: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var tool Tool
			if err := json.Unmarshal([]byte(tc.raw), &tool); err != nil {
				t.Fatalf("unmarshal error: %v", err)
			}
			if tool.IsBuiltin() != tc.isBuiltin {
				t.Fatalf("IsBuiltin()=%v, want %v (type=%q)", tool.IsBuiltin(), tc.isBuiltin, tool.Type)
			}
			if len(tool.Raw) == 0 {
				t.Fatalf("expected Raw to be populated")
			}
			// Raw must be a byte-faithful copy of the input object.
			var got, want map[string]any
			if err := json.Unmarshal(tool.Raw, &got); err != nil {
				t.Fatalf("Raw not valid JSON: %v", err)
			}
			if err := json.Unmarshal([]byte(tc.raw), &want); err != nil {
				t.Fatalf("input not valid JSON: %v", err)
			}
			gb, _ := json.Marshal(got)
			wb, _ := json.Marshal(want)
			if string(gb) != string(wb) {
				t.Fatalf("Raw mismatch:\n got %s\nwant %s", gb, wb)
			}
		})
	}
}

func TestBuiltinToolMarshalEmitsRawVerbatim(t *testing.T) {
	raw := `{"type":"computer_20250124","name":"computer","display_width_px":1280,"display_height_px":800,"display_number":1}`
	var tool Tool
	if err := json.Unmarshal([]byte(raw), &tool); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	out, err := json.Marshal(tool)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}
	for _, field := range []string{"computer_20250124", "display_width_px", "display_height_px", "display_number"} {
		if !strings.Contains(string(out), field) {
			t.Fatalf("expected marshalled builtin tool to contain %q, got %s", field, out)
		}
	}
}

func TestStandardToolMarshalOmitsType(t *testing.T) {
	raw := `{"name":"lookup","description":"lookup data","input_schema":{"type":"object"}}`
	var tool Tool
	if err := json.Unmarshal([]byte(raw), &tool); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	out, err := json.Marshal(tool)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("marshalled output not an object: %v", err)
	}
	if _, ok := obj["type"]; ok {
		t.Fatalf("standard tool must not emit a type field, got %s", out)
	}
	if _, ok := obj["name"]; !ok {
		t.Fatalf("expected name field, got %s", out)
	}
	if _, ok := obj["input_schema"]; !ok {
		t.Fatalf("expected input_schema field, got %s", out)
	}
}

// jsonObjectKeyOrder returns the top-level keys of a JSON object in the order
// they appear on the wire (encoding/json emits struct fields in declaration
// order, so this is exactly the byte order a client would observe).
func jsonObjectKeyOrder(t *testing.T, data []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(data))
	open, err := dec.Token()
	if err != nil {
		t.Fatalf("decode opening token: %v", err)
	}
	if d, ok := open.(json.Delim); !ok || d != '{' {
		t.Fatalf("expected JSON object, got %v (%s)", open, data)
	}
	var keys []string
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			t.Fatalf("decode key token: %v", err)
		}
		key, ok := kt.(string)
		if !ok {
			t.Fatalf("expected string key, got %v", kt)
		}
		keys = append(keys, key)
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			t.Fatalf("decode value for %q: %v", key, err)
		}
	}
	return keys
}

// TestMessageContentBlockJSONKeyOrder pins the on-wire key order of
// MessageContentBlock. A genuine claude-cli emits a tool_result block as
// tool_use_id, type, content (captured 2026-10-10, round 3/6/7/9); encoding/json
// serialises struct fields in declaration order, so the struct must declare
// ToolUseID, then Type, then Content.
func TestMessageContentBlockJSONKeyOrder(t *testing.T) {
	t.Run("tool_result", func(t *testing.T) {
		toolUseID := "toolu_test_1"
		ok := "ok"
		block := MessageContentBlock{
			ToolUseID: &toolUseID,
			Type:      "tool_result",
			Content:   &MessageContent{Content: &ok},
		}
		out, err := json.Marshal(block)
		if err != nil {
			t.Fatalf("marshal error: %v", err)
		}
		got := jsonObjectKeyOrder(t, out)
		if len(got) != 3 || got[0] != "tool_use_id" || got[1] != "type" || got[2] != "content" {
			t.Fatalf("tool_result key order = %v, want [tool_use_id type content] (raw: %s)", got, out)
		}
	})

	t.Run("text", func(t *testing.T) {
		hi := "hi"
		block := MessageContentBlock{Type: "text", Text: &hi}
		out, err := json.Marshal(block)
		if err != nil {
			t.Fatalf("marshal error: %v", err)
		}
		got := jsonObjectKeyOrder(t, out)
		if len(got) != 2 || got[0] != "type" || got[1] != "text" {
			t.Fatalf("text key order = %v, want [type text] (raw: %s)", got, out)
		}
	})
}
