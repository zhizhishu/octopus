package model

import (
	"encoding/json"
	"testing"
)

func TestNormalizeClaudeModelID(t *testing.T) {
	cases := map[string]string{
		"claude-opus-4-7":                "claude-opus-4-7",
		"anthropic/claude-opus-4.7":      "claude-opus-4-7",
		"us.anthropic.claude-sonnet-4-6": "claude-sonnet-4-6",
		"claude-opus-4-7@20260101":       "claude-opus-4-7",
		"Claude-Opus-4-7[1m]":            "claude-opus-4-7",
	}
	for in, want := range cases {
		if got := NormalizeClaudeModelID(in); got != want {
			t.Errorf("NormalizeClaudeModelID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClampClaudeEffort(t *testing.T) {
	cases := []struct{ model, level, want string }{
		{"claude-opus-4-7", "xhigh", "xhigh"},
		{"claude-opus-4-7", "max", "max"},
		{"claude-sonnet-4-6", "xhigh", "high"}, // no xhigh -> high (never up to max)
		{"claude-opus-4-6", "max", "max"},
		{"claude-sonnet-4-6", "low", "low"},
		{"claude-opus-5", "medium", "medium"},
		{"claude-sonnet-4-5", "high", ""}, // no adaptive thinking -> skip
		{"claude-opus-4-5", "high", ""},
		{"claude-3-7-sonnet", "high", ""},
		{"gpt-5", "high", ""},
		{"claude-opus-4-7", "bogus", ""},
	}
	for _, c := range cases {
		if got := ClampClaudeEffort(c.model, c.level); got != c.want {
			t.Errorf("ClampClaudeEffort(%q,%q) = %q, want %q", c.model, c.level, got, c.want)
		}
	}
}

func claudeReq(model string) *InternalLLMRequest {
	return &InternalLLMRequest{Model: model}
}

func TestApplyClaudeDefaultEffortFillsWhenClientSentNothing(t *testing.T) {
	req := claudeReq("claude-opus-4-7")
	filled, skip := ApplyClaudeDefaultEffort(req, "high")
	if filled != "high" || skip != "" || req.ReasoningEffort != "high" || !req.AdaptiveThinking {
		t.Fatalf("filled=%q skip=%q effort=%q adaptive=%v", filled, skip, req.ReasoningEffort, req.AdaptiveThinking)
	}
	req = claudeReq("claude-sonnet-4-6")
	if filled, _ := ApplyClaudeDefaultEffort(req, "xhigh"); filled != "high" || req.ReasoningEffort != "high" {
		t.Fatalf("xhigh on sonnet-4-6 must clamp to high, got %q", filled)
	}
}

func TestApplyClaudeDefaultEffortClientWins(t *testing.T) {
	budget := int64(2048)
	on := true
	clientShapes := map[string]func(*InternalLLMRequest){
		"raw thinking":          func(r *InternalLLMRequest) { r.AnthropicThinking = json.RawMessage(`{"type":"disabled"}`) },
		"output_config effort":  func(r *InternalLLMRequest) { r.AnthropicOutputConfig = json.RawMessage(`{"effort":"low"}`) },
		"reasoning_effort":      func(r *InternalLLMRequest) { r.ReasoningEffort = "low" },
		"reasoning_effort none": func(r *InternalLLMRequest) { r.ReasoningEffort = "none" },
		"adaptive flag":         func(r *InternalLLMRequest) { r.AdaptiveThinking = true },
		"reasoning budget":      func(r *InternalLLMRequest) { r.ReasoningBudget = &budget },
		"qwen enable_thinking":  func(r *InternalLLMRequest) { r.EnableThinking = &on },
		"qwen thinking_budget":  func(r *InternalLLMRequest) { r.ThinkingBudget = &budget },
	}
	for name, set := range clientShapes {
		req := claudeReq("claude-opus-4-7")
		set(req)
		before := *req
		filled, skip := ApplyClaudeDefaultEffort(req, "max")
		if filled != "" || skip != ClaudeEffortSkipClientOwned {
			t.Errorf("%s: filled=%q skip=%q, want client_owned", name, filled, skip)
		}
		if req.ReasoningEffort != before.ReasoningEffort || req.AdaptiveThinking != before.AdaptiveThinking ||
			string(req.AnthropicOutputConfig) != string(before.AnthropicOutputConfig) {
			t.Errorf("%s: request changed", name)
		}
	}
	// output_config carrying only a format is not a thinking choice.
	req := claudeReq("claude-opus-4-7")
	req.AnthropicOutputConfig = json.RawMessage(`{"format":{"type":"json_schema"}}`)
	if ClaudeRequestHasThinkingIntent(req) {
		t.Fatal("format-only output_config must not count as a thinking choice")
	}
}

func TestApplyClaudeDefaultEffortGuards(t *testing.T) {
	temp, topP := 0.2, 0.9
	topK := int64(5)
	any := "any"
	required := "required"
	auto := "auto"
	cases := []struct {
		name  string
		model string
		level string
		set   func(*InternalLLMRequest)
		want  string
	}{
		{"setting none", "claude-opus-4-7", "none", nil, ClaudeEffortSkipSettingNone},
		{"invalid setting", "claude-opus-4-7", "auto", nil, ClaudeEffortSkipInvalidConfig},
		{"old model", "claude-sonnet-4-5", "high", nil, ClaudeEffortSkipModel},
		{"temperature", "claude-opus-4-7", "high", func(r *InternalLLMRequest) { r.Temperature = &temp }, ClaudeEffortSkipSampling},
		{"top_p", "claude-opus-4-7", "high", func(r *InternalLLMRequest) { r.TopP = &topP }, ClaudeEffortSkipSampling},
		{"top_k", "claude-opus-4-7", "high", func(r *InternalLLMRequest) { r.TopK = &topK }, ClaudeEffortSkipSampling},
		{"tool_choice any", "claude-opus-4-7", "high", func(r *InternalLLMRequest) { r.ToolChoice = &ToolChoice{ToolChoice: &any} }, ClaudeEffortSkipForcedTool},
		{"tool_choice required", "claude-opus-4-7", "high", func(r *InternalLLMRequest) { r.ToolChoice = &ToolChoice{ToolChoice: &required} }, ClaudeEffortSkipForcedTool},
		{"named tool", "claude-opus-4-7", "high", func(r *InternalLLMRequest) {
			r.ToolChoice = &ToolChoice{NamedToolChoice: &NamedToolChoice{Type: "function", Function: ToolFunction{Name: "x"}}}
		}, ClaudeEffortSkipForcedTool},
	}
	for _, c := range cases {
		req := claudeReq(c.model)
		if c.set != nil {
			c.set(req)
		}
		filled, skip := ApplyClaudeDefaultEffort(req, c.level)
		if filled != "" || skip != c.want {
			t.Errorf("%s: filled=%q skip=%q, want skip %q", c.name, filled, skip, c.want)
		}
		if req.ReasoningEffort != "" || req.AdaptiveThinking {
			t.Errorf("%s: request was changed", c.name)
		}
	}
	// tool_choice auto is not forced.
	req := claudeReq("claude-opus-4-7")
	req.ToolChoice = &ToolChoice{ToolChoice: &auto}
	if filled, _ := ApplyClaudeDefaultEffort(req, "high"); filled != "high" {
		t.Fatalf("tool_choice auto must still fill, got %q", filled)
	}
}

func TestClaudeThinkingAcceptsClearThinking(t *testing.T) {
	for typ, want := range map[string]bool{"enabled": true, "adaptive": true, "disabled": false, "": false} {
		if got := ClaudeThinkingAcceptsClearThinking(typ); got != want {
			t.Errorf("%q = %v, want %v", typ, got, want)
		}
	}
}
