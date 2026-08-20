// GLM-family models do not always place tool calls in the structured tool_calls
// field. They frequently emit the call as literal markup inside the assistant's
// text content, in one of two shapes:
//
//	<tool_call>function_name
//	<arg_key>param</arg_key><arg_value>value</arg_value>
//	</tool_call>
//
//	[TOOL_REQUEST]{"name":"function_name","arguments":{"param":"value"}}[END_TOOL_REQUEST]
//
// Forwarded verbatim, that markup reaches the client as a wall of tag soup and the
// tool is never executed. This file recovers the structured call and strips the
// markup from the visible text. The two markup shapes, the argument-value JSON
// coercion and the content cleaning mirror ziozzang/llm-toolcall-proxy's
// converters/glm.py, the reference proxy for this upstream behaviour.
package openai

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

// GLMInlineToolCall is a tool call recovered from assistant text content.
type GLMInlineToolCall struct {
	Name string
	// Arguments is always a valid JSON object string; it degrades to "{}" rather
	// than to malformed JSON, because a client that cannot parse the arguments
	// drops the whole call.
	Arguments string
}

var (
	glmTaggedToolCallPattern   = regexp.MustCompile(`(?s)<tool_call>(.*?)</tool_call>`)
	glmTaggedArgumentPattern   = regexp.MustCompile(`(?s)<arg_key>(.*?)</arg_key>\s*<arg_value>(.*?)</arg_value>`)
	glmToolRequestBlockPattern = regexp.MustCompile(`(?s)\[TOOL_REQUEST\]\s*(\{.*?\})\s*\[END_TOOL_REQUEST\]`)
)

// glmInlineToolCallMarkers lists every substring whose presence means the text may
// still be growing into a tool call. A half-received marker must keep the text
// buffered instead of being streamed out, so partial markers count as present.
var glmInlineToolCallMarkers = []string{
	"<tool_call>",
	"</tool_call>",
	"<arg_key>",
	"</arg_key>",
	"<arg_value>",
	"</arg_value>",
	"[TOOL_REQUEST]",
	"[END_TOOL_REQUEST]",
}

// emptyJSONObject is the fallback for any arguments payload that cannot be
// rendered as a JSON object.
const emptyJSONObject = "{}"

// maxGLMInlineBufferBytes caps how much text may be withheld while waiting for a
// tool call block to close. An upstream that emits an opening marker and then keeps
// streaming prose would otherwise grow the buffer without bound and starve the
// client of output. Mirrors the 100KB parse ceiling axonhub's XML tool call parser
// applies for the same reason (nanogpt/xml_parser.go maxXMLParseLength).
const maxGLMInlineBufferBytes = 100000

// modelUsesGLMInlineToolCalls reports whether the model belongs to the GLM family,
// which is the gate for the entire inline recovery path. Every other model —
// claude, codex, gemini — must keep byte-for-byte passthrough, so the check is a
// plain substring test rather than a regex: it runs on the streaming hot path and
// must also match vendor-prefixed ids such as "zai/glm-5.2-fast".
func modelUsesGLMInlineToolCalls(modelName string) bool {
	if modelName == "" {
		return false
	}
	return strings.Contains(strings.ToLower(modelName), "glm")
}

// glmInlineToolCallMarkerPresent reports whether the text contains any tool call
// marker, including a partial one left by a mid-marker stream split.
func glmInlineToolCallMarkerPresent(content string) bool {
	for _, marker := range glmInlineToolCallMarkers {
		if strings.Contains(content, marker) {
			return true
		}
	}
	return false
}

// glmInlineToolCallComplete reports whether the text holds at least one fully
// closed tool call block, meaning parsing can proceed without waiting for more
// stream fragments.
func glmInlineToolCallComplete(content string) bool {
	return glmTaggedToolCallPattern.MatchString(content) ||
		glmToolRequestBlockPattern.MatchString(content)
}

// glmInlineToolCallMatch pairs a recovered call with the offset of the markup it
// came from, so calls found by the two independent patterns can be restored to
// their original relative order.
type glmInlineToolCallMatch struct {
	startOffset int
	toolCall    GLMInlineToolCall
}

// parseGLMInlineToolCalls recovers every complete tool call from the text and
// returns them in source order together with the text stripped of all tool call
// markup. Malformed blocks yield no call but are still stripped: leaving the raw
// markup visible is worse than dropping a call the client could not have executed.
func parseGLMInlineToolCalls(content string) ([]GLMInlineToolCall, string) {
	if content == "" {
		return nil, ""
	}

	matches := make([]glmInlineToolCallMatch, 0)
	matches = append(matches, parseGLMTaggedToolCalls(content)...)
	matches = append(matches, parseGLMToolRequestBlocks(content)...)

	if len(matches) == 0 && !glmInlineToolCallMarkerPresent(content) {
		return nil, content
	}

	sort.SliceStable(matches, func(first, second int) bool {
		return matches[first].startOffset < matches[second].startOffset
	})

	toolCalls := make([]GLMInlineToolCall, 0, len(matches))
	for _, match := range matches {
		toolCalls = append(toolCalls, match.toolCall)
	}

	cleanedContent := glmTaggedToolCallPattern.ReplaceAllString(content, "")
	cleanedContent = glmToolRequestBlockPattern.ReplaceAllString(cleanedContent, "")
	cleanedContent = strings.TrimSpace(cleanedContent)

	if len(toolCalls) == 0 {
		return nil, cleanedContent
	}
	return toolCalls, cleanedContent
}

// parseGLMTaggedToolCalls handles the XML-style markup shape.
func parseGLMTaggedToolCalls(content string) []glmInlineToolCallMatch {
	blockLocations := glmTaggedToolCallPattern.FindAllStringSubmatchIndex(content, -1)
	matches := make([]glmInlineToolCallMatch, 0, len(blockLocations))

	for _, location := range blockLocations {
		blockStart := location[0]
		blockBody := content[location[2]:location[3]]

		// The function name is whatever precedes the first argument tag; a block
		// without argument tags is a no-argument call whose whole body is the name.
		nameSection := blockBody
		if firstArgumentIndex := strings.Index(blockBody, "<arg_key>"); firstArgumentIndex >= 0 {
			nameSection = blockBody[:firstArgumentIndex]
		}
		toolCallName := strings.TrimSpace(nameSection)
		if toolCallName == "" {
			continue
		}

		argumentPairs := glmTaggedArgumentPattern.FindAllStringSubmatch(blockBody, -1)
		parsedArguments := make(map[string]any, len(argumentPairs))
		for _, argumentPair := range argumentPairs {
			argumentName := strings.TrimSpace(argumentPair[1])
			if argumentName == "" {
				continue
			}
			parsedArguments[argumentName] = coerceGLMArgumentValue(strings.TrimSpace(argumentPair[2]))
		}

		matches = append(matches, glmInlineToolCallMatch{
			startOffset: blockStart,
			toolCall: GLMInlineToolCall{
				Name:      toolCallName,
				Arguments: marshalGLMArguments(parsedArguments),
			},
		})
	}

	return matches
}

// parseGLMToolRequestBlocks handles the JSON block markup shape.
func parseGLMToolRequestBlocks(content string) []glmInlineToolCallMatch {
	blockLocations := glmToolRequestBlockPattern.FindAllStringSubmatchIndex(content, -1)
	matches := make([]glmInlineToolCallMatch, 0, len(blockLocations))

	for _, location := range blockLocations {
		blockStart := location[0]
		payload := content[location[2]:location[3]]

		var decoded struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
			continue
		}
		toolCallName := strings.TrimSpace(decoded.Name)
		if toolCallName == "" {
			continue
		}

		matches = append(matches, glmInlineToolCallMatch{
			startOffset: blockStart,
			toolCall: GLMInlineToolCall{
				Name:      toolCallName,
				Arguments: normalizeGLMArgumentsJSON(decoded.Arguments),
			},
		})
	}

	return matches
}

// coerceGLMArgumentValue keeps a JSON-typed argument value typed. The markup
// carries every value as text, so a numeric or boolean argument would otherwise
// reach the tool as a string and fail its schema validation.
func coerceGLMArgumentValue(rawValue string) any {
	if rawValue == "" {
		return rawValue
	}
	var decodedValue any
	if err := json.Unmarshal([]byte(rawValue), &decodedValue); err != nil {
		return rawValue
	}
	return decodedValue
}

func marshalGLMArguments(arguments map[string]any) string {
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return emptyJSONObject
	}
	return normalizeGLMArgumentsJSON(encoded)
}

// normalizeGLMArgumentsJSON enforces the "always a JSON object" contract on an
// arguments payload, collapsing null, scalars and arrays to an empty object.
func normalizeGLMArgumentsJSON(encoded []byte) string {
	trimmed := strings.TrimSpace(string(encoded))
	if trimmed == "" || !strings.HasPrefix(trimmed, "{") {
		return emptyJSONObject
	}
	return trimmed
}
