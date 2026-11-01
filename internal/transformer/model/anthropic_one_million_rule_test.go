package model

import "testing"

// 统一的 [1m] 规则：结尾、大小写不敏感、可重复；认 claude-* 和短名 opus/sonnet/fable；
// haiku 永不要 1M。见 internal/model/channel_toggles.md。
func TestStripOneMillionSuffix(t *testing.T) {
	cases := []struct {
		in, want string
		stripped bool
	}{
		{"claude-opus-4-8[1m]", "claude-opus-4-8", true},
		{"claude-opus-4-8[1M]", "claude-opus-4-8", true},
		{"claude-opus-4-8[1M][1m]", "claude-opus-4-8", true},
		{" sonnet[1m] ", "sonnet", true},
		{"claude-opus-4-8[1m]-preview", "claude-opus-4-8[1m]-preview", false},
		{"[1m]", "[1m]", false},
		{"claude-opus-4-8", "claude-opus-4-8", false},
	}
	for _, c := range cases {
		got, stripped := StripOneMillionSuffix(c.in)
		if got != c.want || stripped != c.stripped {
			t.Fatalf("StripOneMillionSuffix(%q) = (%q,%v), want (%q,%v)", c.in, got, stripped, c.want, c.stripped)
		}
	}
}

func TestOneMillionShortNamesAndHaiku(t *testing.T) {
	wants := map[string]bool{
		"opus[1m]":               true,
		"sonnet[1m]":             true,
		"fable[1m]":              true,
		"FABLE[1M]":              true,
		"claude-sonnet-4-5[1m]":  true,
		"claude-fable-5[1M][1m]": true,
		"claude-haiku-4-5[1m]":   false,
		"haiku[1m]":              false,
		"gpt-5[1m]":              false,
		"claude-opus-4-8":        false,
		"claude-opus-4-8[1m]-x":  false,
	}
	for in, want := range wants {
		if got := AnthropicModelWantsOneMillionBeta(in); got != want {
			t.Fatalf("AnthropicModelWantsOneMillionBeta(%q) = %v, want %v", in, got, want)
		}
	}
	norm := map[string]string{
		"sonnet[1m]":             "sonnet",
		"fable[1m]":              "claude-fable-5",
		"claude-fable-5[1M][1m]": "claude-fable-5",
		"OPUS[1M]":               "claude-opus-4-8",
		"claude-haiku-4-5[1m]":   "claude-haiku-4-5",
		"gpt-5[1m]":              "gpt-5[1m]", // not a Claude name: untouched on the request path
		"fable":                  "fable",     // no suffix: unchanged (pre-existing behaviour)
	}
	for in, want := range norm {
		if got := NormalizeAnthropicModelAlias(in); got != want {
			t.Fatalf("NormalizeAnthropicModelAlias(%q) = %q, want %q", in, got, want)
		}
	}
}

// Path 0 of 3 (the request-level gate every path calls): the channel opt-in
// never turns 1M on for haiku.
func TestAnthropicRequestWantsOneMillionNeverForHaiku(t *testing.T) {
	req := &InternalLLMRequest{Model: "claude-haiku-4-5-20251001"}
	req.TransformOptions.AnthropicOneMillionBeta = true
	if AnthropicRequestWantsOneMillionBeta(req) {
		t.Fatal("channel 1M opt-in must not apply to haiku")
	}
	req.Model = "claude-opus-4-8"
	if !AnthropicRequestWantsOneMillionBeta(req) {
		t.Fatal("channel 1M opt-in must still apply to opus")
	}
}
