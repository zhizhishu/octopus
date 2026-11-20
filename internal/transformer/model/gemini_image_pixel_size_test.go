package model

import "testing"

func TestGeminiImageSizeFromPixelSize(t *testing.T) {
	cases := map[string]string{
		"":          "",
		"auto":      "",
		"1024x1024": "",
		"1792x1024": "",
		"1536x1024": "",
		"1200x900":  "",
		"2048x2048": "2K",
		"2400x1792": "2K",
		"2752x1536": "2K",
		"3168x1344": "2K",
		"4096x4096": "4K",
		"4800x3584": "4K",
		"5504x3072": "4K",
		"2048X2048": "2K",
		"2048×2048": "2K",
		"2048*2048": "2K",
		"0x2048":    "",
		"2Kx2K":     "",
	}
	for raw, want := range cases {
		if got := GeminiImageSizeFromPixelSize(raw); got != want {
			t.Fatalf("GeminiImageSizeFromPixelSize(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestGeminiAspectRatioFromPixelSize(t *testing.T) {
	cases := map[string]string{
		"":          "",
		"1:1":       "",
		"1024x1024": "1:1",
		"2048x2048": "1:1",
		"1200x900":  "4:3",
		"2400x1792": "4:3",
		"1792x2400": "3:4",
		"1536x1024": "3:2",
		"1024x1536": "2:3",
		"1792x1024": "16:9",
		"1024x1792": "9:16",
		"1920x1080": "16:9",
		"1152x928":  "5:4",
		"928x1152":  "4:5",
		"3168x1344": "21:9",
		"3000x1000": "",
	}
	for raw, want := range cases {
		if got := GeminiAspectRatioFromPixelSize(raw); got != want {
			t.Fatalf("GeminiAspectRatioFromPixelSize(%q) = %q, want %q", raw, got, want)
		}
	}
}
