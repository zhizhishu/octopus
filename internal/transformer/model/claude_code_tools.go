package model

import (
	"encoding/json"
	"strings"
)

// IsClaudeCodeModel reports whether a model id/alias belongs to the Claude-family
// traffic that may be cloaked as Claude Code. Keep this broad enough for public aliases
// (opus/sonnet/haiku/fable) but narrow enough not to touch unrelated Anthropic-compatible
// models sharing an Anthropic channel.
func IsClaudeCodeModel(modelName string) bool {
	lower := strings.ToLower(strings.TrimSpace(modelName))
	lower = strings.ReplaceAll(lower, "[1m]", "")
	if lower == "" {
		return false
	}
	if strings.Contains(lower, "claude") {
		return true
	}
	for _, family := range []string{"opus", "sonnet", "haiku", "fable"} {
		if lower == family || strings.HasPrefix(lower, family+"-") || strings.Contains(lower, "-"+family+"-") {
			return true
		}
	}
	return false
}

// ClaudeCodeProbeTools returns a small representative subset of the genuine Claude
// Code tool set. Strict Claude-gated relays inspect the request body, not only headers;
// a bare prompt with no tools is visibly not an agent turn. The caller must gate this on
// cloaked Anthropic/Claude traffic so other protocols are never touched.
func ClaudeCodeProbeTools() []Tool {
	schema := json.RawMessage(`{"type":"object","properties":{},"additionalProperties":true}`)
	mk := func(name, desc string) Tool {
		return Tool{Type: "function", Function: Function{Name: name, Description: desc, Parameters: schema}}
	}
	return []Tool{
		mk("Bash", "Run a shell command."),
		mk("Read", "Read a file from the filesystem."),
		mk("Edit", "Make edits to a file."),
		mk("Glob", "Find files matching a glob pattern."),
		mk("Grep", "Search file contents with a regex."),
	}
}
