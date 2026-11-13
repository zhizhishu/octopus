package model

import "testing"

func TestNormalizeGeminiImageSize(t *testing.T) {
	cases := map[string]string{
		"1K":       "1K",
		"2K":       "2K",
		"4K":       "4K",
		"2k":       "2K", // 大小写敏感会让客户端静默拿默认档
		"4k":       "4K",
		" hd ":     "2K",
		"high":     "2K",
		"standard": "1K",
		"medium":   "1K",
		"low":      "1K",
		"auto":     "1K",
		"uhd":      "4K",
		"ultra":    "4K",
		"":         "",
		"8K":       "", // 认不出来就不发，不替客户端猜
		"4096x4096": "",
	}
	for raw, want := range cases {
		if got := NormalizeGeminiImageSize(raw); got != want {
			t.Fatalf("NormalizeGeminiImageSize(%q) = %q, want %q", raw, got, want)
		}
	}
}
