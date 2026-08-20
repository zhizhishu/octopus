package openai

import (
	"encoding/json"
	"testing"
)

func TestGLMInlineToolCallModelGateOnlyMatchesGLMFamily(t *testing.T) {
	glmModelNames := []string{
		"glm-4",
		"GLM-5.2",
		"chatglm-6b",
		"zai/glm-5.2-fast",
		"glm-4.6",
	}
	for _, modelName := range glmModelNames {
		if !modelUsesGLMInlineToolCalls(modelName) {
			t.Errorf("expected %q to be gated as a GLM model", modelName)
		}
	}

	nonGLMModelNames := []string{
		"claude-opus-5",
		"gpt-5.6-sol",
		"gemini-3-pro",
		"grok-4.5",
		"deepseek-v4",
		"",
	}
	for _, modelName := range nonGLMModelNames {
		if modelUsesGLMInlineToolCalls(modelName) {
			t.Errorf("expected %q to stay on the byte-for-byte passthrough path", modelName)
		}
	}
}

func TestParseGLMInlineToolCallsRecoversSingleArgumentTaggedCall(t *testing.T) {
	content := "Let me check.\n<tool_call>read_file\n<arg_key>path</arg_key>\n<arg_value>main.go</arg_value>\n</tool_call>"

	toolCalls, cleanedContent := parseGLMInlineToolCalls(content)

	if len(toolCalls) != 1 {
		t.Fatalf("expected exactly one recovered tool call, got %d: %#v", len(toolCalls), toolCalls)
	}
	if toolCalls[0].Name != "read_file" {
		t.Fatalf("expected tool name read_file, got %q", toolCalls[0].Name)
	}
	assertArgumentsEqual(t, toolCalls[0].Arguments, map[string]any{"path": "main.go"})
	if cleanedContent != "Let me check." {
		t.Fatalf("expected markup stripped from visible text, got %q", cleanedContent)
	}
}

func TestParseGLMInlineToolCallsRecoversMultipleArgumentsTaggedCall(t *testing.T) {
	content := "<tool_call>edit_file<arg_key>path</arg_key><arg_value>a.go</arg_value><arg_key>line</arg_key><arg_value>42</arg_value></tool_call>"

	toolCalls, cleanedContent := parseGLMInlineToolCalls(content)

	if len(toolCalls) != 1 {
		t.Fatalf("expected exactly one recovered tool call, got %d", len(toolCalls))
	}
	assertArgumentsEqual(t, toolCalls[0].Arguments, map[string]any{"path": "a.go", "line": float64(42)})
	if cleanedContent != "" {
		t.Fatalf("expected empty visible text, got %q", cleanedContent)
	}
}

func TestParseGLMInlineToolCallsRecoversArgumentlessTaggedCall(t *testing.T) {
	content := "<tool_call>list_files</tool_call>"

	toolCalls, _ := parseGLMInlineToolCalls(content)

	if len(toolCalls) != 1 {
		t.Fatalf("expected exactly one recovered tool call, got %d", len(toolCalls))
	}
	if toolCalls[0].Name != "list_files" {
		t.Fatalf("expected tool name list_files, got %q", toolCalls[0].Name)
	}
	if toolCalls[0].Arguments != "{}" {
		t.Fatalf("expected empty JSON object arguments, got %q", toolCalls[0].Arguments)
	}
}

func TestParseGLMInlineToolCallsRecoversToolRequestJSONBlock(t *testing.T) {
	content := `Working on it.
[TOOL_REQUEST]
{"name": "search", "arguments": {"query": "octopus", "limit": 5}}
[END_TOOL_REQUEST]`

	toolCalls, cleanedContent := parseGLMInlineToolCalls(content)

	if len(toolCalls) != 1 {
		t.Fatalf("expected exactly one recovered tool call, got %d", len(toolCalls))
	}
	if toolCalls[0].Name != "search" {
		t.Fatalf("expected tool name search, got %q", toolCalls[0].Name)
	}
	assertArgumentsEqual(t, toolCalls[0].Arguments, map[string]any{"query": "octopus", "limit": float64(5)})
	if cleanedContent != "Working on it." {
		t.Fatalf("expected markup stripped from visible text, got %q", cleanedContent)
	}
}

func TestParseGLMInlineToolCallsKeepsSourceOrderAcrossBothMarkupShapes(t *testing.T) {
	content := `[TOOL_REQUEST]{"name":"first_call","arguments":{}}[END_TOOL_REQUEST]` +
		`<tool_call>second_call<arg_key>a</arg_key><arg_value>1</arg_value></tool_call>` +
		`[TOOL_REQUEST]{"name":"third_call","arguments":{}}[END_TOOL_REQUEST]`

	toolCalls, _ := parseGLMInlineToolCalls(content)

	expectedNames := []string{"first_call", "second_call", "third_call"}
	if len(toolCalls) != len(expectedNames) {
		t.Fatalf("expected %d recovered tool calls, got %d: %#v", len(expectedNames), len(toolCalls), toolCalls)
	}
	for index, expectedName := range expectedNames {
		if toolCalls[index].Name != expectedName {
			t.Fatalf("expected call %d to be %q, got %q (full: %#v)", index, expectedName, toolCalls[index].Name, toolCalls)
		}
	}
}

func TestParseGLMInlineToolCallsPreservesJSONTypedArgumentValues(t *testing.T) {
	content := "<tool_call>configure" +
		"<arg_key>count</arg_key><arg_value>7</arg_value>" +
		"<arg_key>enabled</arg_key><arg_value>true</arg_value>" +
		"<arg_key>nested</arg_key><arg_value>{\"deep\":\"value\"}</arg_value>" +
		"<arg_key>plain</arg_key><arg_value>just text</arg_value>" +
		"</tool_call>"

	toolCalls, _ := parseGLMInlineToolCalls(content)

	if len(toolCalls) != 1 {
		t.Fatalf("expected exactly one recovered tool call, got %d", len(toolCalls))
	}

	var decodedArguments map[string]any
	if err := json.Unmarshal([]byte(toolCalls[0].Arguments), &decodedArguments); err != nil {
		t.Fatalf("expected arguments to be valid JSON, got %q (%v)", toolCalls[0].Arguments, err)
	}
	if decodedArguments["count"] != float64(7) {
		t.Errorf("expected numeric argument to stay numeric, got %#v", decodedArguments["count"])
	}
	if decodedArguments["enabled"] != true {
		t.Errorf("expected boolean argument to stay boolean, got %#v", decodedArguments["enabled"])
	}
	nestedArgument, ok := decodedArguments["nested"].(map[string]any)
	if !ok || nestedArgument["deep"] != "value" {
		t.Errorf("expected nested object argument to stay an object, got %#v", decodedArguments["nested"])
	}
	if decodedArguments["plain"] != "just text" {
		t.Errorf("expected non-JSON argument to stay a string, got %#v", decodedArguments["plain"])
	}
}

func TestGLMInlineToolCallMarkerDetectionDistinguishesPartialFromComplete(t *testing.T) {
	partialContent := "thinking... <tool_call>read_file<arg_key>path</arg_key>"

	if !glmInlineToolCallMarkerPresent(partialContent) {
		t.Fatalf("expected a partial marker to be detected so the text stays buffered")
	}
	if glmInlineToolCallComplete(partialContent) {
		t.Fatalf("expected a partial marker to not count as a complete block")
	}

	completeContent := partialContent + "<arg_value>main.go</arg_value></tool_call>"
	if !glmInlineToolCallComplete(completeContent) {
		t.Fatalf("expected a closed block to count as complete")
	}
}

func TestParseGLMInlineToolCallsLeavesPlainTextUntouched(t *testing.T) {
	content := "This is just a normal answer with no markup at all."

	toolCalls, cleanedContent := parseGLMInlineToolCalls(content)

	if len(toolCalls) != 0 {
		t.Fatalf("expected no recovered tool calls, got %#v", toolCalls)
	}
	if cleanedContent != content {
		t.Fatalf("expected plain text to pass through unchanged, got %q", cleanedContent)
	}
}

func TestParseGLMInlineToolCallsStripsMalformedBlockWithoutEmittingCall(t *testing.T) {
	content := "before[TOOL_REQUEST]{not valid json}[END_TOOL_REQUEST]after"

	toolCalls, cleanedContent := parseGLMInlineToolCalls(content)

	if len(toolCalls) != 0 {
		t.Fatalf("expected a malformed block to yield no tool call, got %#v", toolCalls)
	}
	if cleanedContent != "beforeafter" {
		t.Fatalf("expected malformed markup to still be stripped, got %q", cleanedContent)
	}
}

func TestParseGLMInlineToolCallsCoercesNonObjectArgumentsToEmptyObject(t *testing.T) {
	content := `[TOOL_REQUEST]{"name":"noop","arguments":null}[END_TOOL_REQUEST]`

	toolCalls, _ := parseGLMInlineToolCalls(content)

	if len(toolCalls) != 1 {
		t.Fatalf("expected exactly one recovered tool call, got %d", len(toolCalls))
	}
	if toolCalls[0].Arguments != "{}" {
		t.Fatalf("expected null arguments to degrade to an empty JSON object, got %q", toolCalls[0].Arguments)
	}
}

func assertArgumentsEqual(t *testing.T, actualArgumentsJSON string, expectedArguments map[string]any) {
	t.Helper()

	var decodedArguments map[string]any
	if err := json.Unmarshal([]byte(actualArgumentsJSON), &decodedArguments); err != nil {
		t.Fatalf("expected arguments to be valid JSON, got %q (%v)", actualArgumentsJSON, err)
	}
	if len(decodedArguments) != len(expectedArguments) {
		t.Fatalf("expected %d arguments, got %d: %#v", len(expectedArguments), len(decodedArguments), decodedArguments)
	}
	for expectedKey, expectedValue := range expectedArguments {
		if decodedArguments[expectedKey] != expectedValue {
			t.Fatalf("expected argument %q to be %#v, got %#v", expectedKey, expectedValue, decodedArguments[expectedKey])
		}
	}
}
