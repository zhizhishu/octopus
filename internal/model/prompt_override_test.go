package model

import "testing"

func TestNormalizePromptOverride(t *testing.T) {
	cases := []struct {
		enabled     bool
		mode        PromptOverrideMode
		text        string
		wantEnabled bool
		wantMode    PromptOverrideMode
	}{
		{false, "", "x", false, PromptOverrideModeBefore},
		{true, PromptOverrideModeAfter, "x", true, PromptOverrideModeAfter},
		{false, PromptOverrideModeAfter, "x", false, PromptOverrideModeAfter},
		// old clients that never send the switch
		{false, PromptOverrideModeAppendSystem, "x", true, PromptOverrideModeBefore},
		{false, PromptOverrideModeReplaceSystem, "x", true, PromptOverrideModeBefore},
		{false, PromptOverrideModeAppendSystem, "  ", false, PromptOverrideModeBefore},
		// unknown stays for validation to reject
		{true, "bogus", "x", true, "bogus"},
	}
	for _, c := range cases {
		enabled, mode := c.enabled, c.mode
		NormalizePromptOverride(&enabled, &mode, c.text)
		if enabled != c.wantEnabled || mode != c.wantMode {
			t.Fatalf("%#v -> (%v,%q)", c, enabled, mode)
		}
	}
	if !PromptOverrideActive(false, PromptOverrideModeReplaceSystem, "x") || PromptOverrideActive(false, PromptOverrideModeBefore, "x") ||
		PromptOverrideActive(true, PromptOverrideModeAfter, " ") || !PromptOverrideActive(true, PromptOverrideModeAfter, "x") {
		t.Fatal("PromptOverrideActive")
	}
	if ValidPromptOverrideMode("fill_missing") {
		t.Fatal("fill_missing was never shipped and is not a mode")
	}
}
