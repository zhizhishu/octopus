package anthropic

import (
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
