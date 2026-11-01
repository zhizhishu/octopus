package model

import "testing"

func TestClaudeEffortCapabilityAdaptiveOnlyTable(t *testing.T) {
	cases := []struct {
		model                                      string
		noManual, noDisable, betweenTools, offHigh bool
		defaultEffort                              string
	}{
		{"claude-opus-5-5", true, true, false, false, "medium"},
		{"claude-opus-5.5", true, true, false, false, "medium"},          // dotted (OpenRouter)
		{"claude-opus-5-5-thinking", true, true, false, false, "medium"}, // -thinking alias
		{"anthropic/claude-opus-5.5", true, true, false, false, "medium"},
		{"claude-sonnet-5-5", true, true, true, true, "high"},
		{"claude-haiku-5-5", true, false, false, true, "medium"},
		{"claude-opus-5", true, false, false, true, "high"},
		{"claude-sonnet-5", true, false, false, false, "high"},
		{"claude-opus-4-8", true, false, false, false, "high"},
		{"claude-opus-4-7-20260101", true, false, false, false, "high"},
		{"claude-fable-5", true, true, false, false, "high"},
		{"claude-mythos-5", true, true, false, false, "high"},
		{"claude-mythos-preview", false, true, false, false, "high"},
		{"claude-opus-4-6", false, false, false, false, "high"},
		{"claude-sonnet-4-6", false, false, false, false, "high"},
		{"claude-opus-4-5", false, false, false, false, ""},
		{"claude-sonnet-4-5-thinking", false, false, false, false, ""},
		{"claude-3-7-sonnet", false, false, false, false, ""},
		{"gpt-5", false, false, false, false, ""},
	}
	for _, c := range cases {
		got := ClaudeEffortCapabilityFor(c.model)
		if got.NoManual != c.noManual || got.NoDisable != c.noDisable || got.DisableAsBetweenTools != c.betweenTools ||
			got.OffEffortAtMostHigh != c.offHigh || got.DefaultEffort != c.defaultEffort {
			t.Errorf("%s: got %+v", c.model, got)
		}
	}
}

func TestClaudeAdaptiveEffortFromBudget(t *testing.T) {
	cases := []struct {
		model  string
		budget int64
		want   string
	}{
		{"claude-opus-5-5", 0, ""},
		{"claude-opus-5-5", -1, ""},
		{"claude-opus-5-5", 1024, "low"},
		{"claude-opus-5-5", 1025, "medium"},
		{"claude-opus-5-5", 8192, "medium"},
		{"claude-opus-5-5", 8193, "high"},
		{"claude-opus-5-5", 64000, "high"}, // budget is a cap, never xhigh/max
		{"claude-opus-4-5", 10000, ""},     // no adaptive effort on manual-only models
	}
	for _, c := range cases {
		if got := ClaudeAdaptiveEffortFromBudget(c.model, c.budget); got != c.want {
			t.Errorf("ClaudeAdaptiveEffortFromBudget(%q,%d)=%q want %q", c.model, c.budget, got, c.want)
		}
	}
}

func TestClaudeThinkingOff(t *testing.T) {
	cases := map[string][2]string{
		"claude-opus-5-5":   {`{"type":"adaptive"}`, `{"effort":"low"}`},
		"claude-fable-5":    {`{"type":"adaptive"}`, `{"effort":"low"}`},
		"claude-sonnet-5-5": {`{"type":"between_tools"}`, `{"effort":"low"}`},
		"claude-haiku-5-5":  {`{"type":"disabled"}`, ``},
		"claude-opus-4-7":   {`{"type":"disabled"}`, ``},
		"claude-opus-4-5":   {`{"type":"disabled"}`, ``},
	}
	for model, want := range cases {
		thinking, outputConfig := ClaudeThinkingOff(model)
		if string(thinking) != want[0] || string(outputConfig) != want[1] {
			t.Errorf("%s: thinking=%s output_config=%s, want %s %s", model, thinking, outputConfig, want[0], want[1])
		}
	}
}
