package authropic

import (
	"context"
	"encoding/json"
	"testing"

	inboundAnthropic "github.com/bestruirui/octopus/internal/transformer/inbound/anthropic"
	"github.com/bestruirui/octopus/internal/transformer/model"
)

// roundTripTools parses an Anthropic Messages request body into the internal
// model and re-emits it as an Anthropic outbound request, returning the marshalled
// tools array as decoded JSON objects. This exercises the full
// inbound -> internal -> outbound path for tool definitions.
func roundTripTools(t *testing.T, body string) []map[string]json.RawMessage {
	t.Helper()

	internalReq, err := (&inboundAnthropic.MessagesInbound{}).TransformRequest(context.Background(), []byte(body))
	if err != nil {
		t.Fatalf("inbound TransformRequest error: %v", err)
	}

	outboundReq := convertToAnthropicRequest(internalReq)

	raw, err := json.Marshal(outboundReq)
	if err != nil {
		t.Fatalf("marshal outbound request error: %v", err)
	}

	var decoded struct {
		Tools []map[string]json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal outbound request error: %v", err)
	}
	return decoded.Tools
}

func fieldString(t *testing.T, obj map[string]json.RawMessage, key string) string {
	t.Helper()
	raw, ok := obj[key]
	if !ok {
		t.Fatalf("expected field %q to be present in %v", key, obj)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("field %q is not a string: %v", key, err)
	}
	return s
}

func fieldInt(t *testing.T, obj map[string]json.RawMessage, key string) int64 {
	t.Helper()
	raw, ok := obj[key]
	if !ok {
		t.Fatalf("expected field %q to be present in %v", key, obj)
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil {
		t.Fatalf("field %q is not a number: %v", key, err)
	}
	return n
}

func TestBuiltinComputerUseToolRoundTrip(t *testing.T) {
	body := `{
		"model":"claude-sonnet-4-5",
		"max_tokens":1024,
		"tools":[
			{
				"type":"computer_20250124",
				"name":"computer",
				"display_width_px":1280,
				"display_height_px":800,
				"display_number":1
			}
		],
		"messages":[{"role":"user","content":"take a screenshot"}]
	}`

	tools := roundTripTools(t, body)
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d: %v", len(tools), tools)
	}
	tool := tools[0]

	if got := fieldString(t, tool, "type"); got != "computer_20250124" {
		t.Fatalf("expected type computer_20250124, got %q", got)
	}
	if got := fieldString(t, tool, "name"); got != "computer" {
		t.Fatalf("expected name computer, got %q", got)
	}
	if got := fieldInt(t, tool, "display_width_px"); got != 1280 {
		t.Fatalf("expected display_width_px 1280, got %d", got)
	}
	if got := fieldInt(t, tool, "display_height_px"); got != 800 {
		t.Fatalf("expected display_height_px 800, got %d", got)
	}
	if got := fieldInt(t, tool, "display_number"); got != 1 {
		t.Fatalf("expected display_number 1, got %d", got)
	}
}

func TestBuiltinBashAndTextEditorToolRoundTrip(t *testing.T) {
	body := `{
		"model":"claude-sonnet-4-5",
		"max_tokens":1024,
		"tools":[
			{"type":"bash_20250101","name":"bash"},
			{"type":"text_editor_20250728","name":"str_replace_based_edit_tool"}
		],
		"messages":[{"role":"user","content":"edit a file"}]
	}`

	tools := roundTripTools(t, body)
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d: %v", len(tools), tools)
	}

	if got := fieldString(t, tools[0], "type"); got != "bash_20250101" {
		t.Fatalf("expected bash type, got %q", got)
	}
	if got := fieldString(t, tools[0], "name"); got != "bash" {
		t.Fatalf("expected bash name, got %q", got)
	}

	if got := fieldString(t, tools[1], "type"); got != "text_editor_20250728" {
		t.Fatalf("expected text_editor type, got %q", got)
	}
	if got := fieldString(t, tools[1], "name"); got != "str_replace_based_edit_tool" {
		t.Fatalf("expected text_editor name, got %q", got)
	}
}

func TestStandardFunctionToolDoesNotRegress(t *testing.T) {
	body := `{
		"model":"claude-sonnet-4-5",
		"max_tokens":256,
		"tools":[
			{
				"name":"lookup",
				"description":"lookup data",
				"input_schema":{"type":"object","properties":{"query":{"type":"string"}}}
			}
		],
		"messages":[{"role":"user","content":"find data"}]
	}`

	tools := roundTripTools(t, body)
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d: %v", len(tools), tools)
	}
	tool := tools[0]

	if got := fieldString(t, tool, "name"); got != "lookup" {
		t.Fatalf("expected name lookup, got %q", got)
	}
	if got := fieldString(t, tool, "description"); got != "lookup data" {
		t.Fatalf("expected description, got %q", got)
	}
	if _, ok := tool["input_schema"]; !ok {
		t.Fatalf("expected input_schema to be present: %v", tool)
	}
	// Standard custom/function tools must not gain a "type" field — preserving the
	// pre-existing wire shape that omits type for custom tools.
	if _, ok := tool["type"]; ok {
		t.Fatalf("standard function tool should not emit a type field: %v", tool)
	}
}

func TestMixedFunctionAndBuiltinToolsRoundTrip(t *testing.T) {
	body := `{
		"model":"claude-sonnet-4-5",
		"max_tokens":1024,
		"tools":[
			{
				"name":"lookup",
				"description":"lookup data",
				"input_schema":{"type":"object","properties":{"query":{"type":"string"}}}
			},
			{
				"type":"computer_20250124",
				"name":"computer",
				"display_width_px":1024,
				"display_height_px":768
			}
		],
		"messages":[{"role":"user","content":"do work"}]
	}`

	tools := roundTripTools(t, body)
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d: %v", len(tools), tools)
	}

	// Order is preserved: function first, builtin second.
	if got := fieldString(t, tools[0], "name"); got != "lookup" {
		t.Fatalf("expected first tool lookup, got %q", got)
	}
	if _, ok := tools[0]["type"]; ok {
		t.Fatalf("function tool should not emit type: %v", tools[0])
	}

	if got := fieldString(t, tools[1], "type"); got != "computer_20250124" {
		t.Fatalf("expected builtin type computer_20250124, got %q", got)
	}
	if got := fieldInt(t, tools[1], "display_width_px"); got != 1024 {
		t.Fatalf("expected display_width_px 1024, got %d", got)
	}
}

func TestBuiltinToolCacheControlMergedIntoRaw(t *testing.T) {
	body := `{
		"model":"claude-sonnet-4-5",
		"max_tokens":1024,
		"tools":[
			{
				"type":"computer_20250124",
				"name":"computer",
				"display_width_px":1280,
				"display_height_px":800,
				"cache_control":{"type":"ephemeral"}
			}
		],
		"messages":[{"role":"user","content":"take a screenshot"}]
	}`

	tools := roundTripTools(t, body)
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d: %v", len(tools), tools)
	}
	tool := tools[0]

	// Proprietary fields survive alongside a preserved cache_control.
	if got := fieldInt(t, tool, "display_width_px"); got != 1280 {
		t.Fatalf("expected display_width_px 1280, got %d", got)
	}
	ccRaw, ok := tool["cache_control"]
	if !ok {
		t.Fatalf("expected cache_control to survive on builtin tool: %v", tool)
	}
	var cc struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(ccRaw, &cc); err != nil {
		t.Fatalf("cache_control not an object: %v", err)
	}
	if cc.Type != "ephemeral" {
		t.Fatalf("expected cache_control type ephemeral, got %q", cc.Type)
	}
}

// TestInboundClassifiesBuiltinAsRawNotFunction verifies the internal model carries
// the built-in tool as a raw-bearing non-function tool. Outbound transformers that
// only understand function tools (openai/gemini) gate on Type=="function", so a
// non-function type means they skip it safely rather than emit a malformed function.
func TestInboundClassifiesBuiltinAsRawNotFunction(t *testing.T) {
	body := `{
		"model":"claude-sonnet-4-5",
		"max_tokens":1024,
		"tools":[{"type":"computer_20250124","name":"computer","display_width_px":1280}],
		"messages":[{"role":"user","content":"hi"}]
	}`

	internalReq, err := (&inboundAnthropic.MessagesInbound{}).TransformRequest(context.Background(), []byte(body))
	if err != nil {
		t.Fatalf("inbound TransformRequest error: %v", err)
	}
	if len(internalReq.Tools) != 1 {
		t.Fatalf("expected 1 internal tool, got %d", len(internalReq.Tools))
	}
	tool := internalReq.Tools[0]
	if tool.Type != model.ToolTypeAnthropicBuiltin {
		t.Fatalf("expected internal type %q, got %q", model.ToolTypeAnthropicBuiltin, tool.Type)
	}
	if tool.Type == "function" {
		t.Fatalf("builtin tool must not be classified as a function (would corrupt openai/gemini outbound)")
	}
	if len(tool.RawTool) == 0 {
		t.Fatalf("expected RawTool to be populated for builtin tool")
	}
}
