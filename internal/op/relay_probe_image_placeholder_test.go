package op

import "testing"

// 响应里的图另存后是带 id 的占位符：仍要算作「有可见产出」，不能报「空回复计了费」。
func TestHasAnyVisibleOutputSeesSavedImagePlaceholder(t *testing.T) {
	body := `{"choices":[{"index":0,"message":{"role":"assistant","content":"","images":[{"type":"image_url","image_url":{"url":"[image: image/png, 2KB, id=` +
		"9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08" + `]"}}]}}]}`
	if !hasAnyVisibleOutput(body) {
		t.Fatal("saved image placeholder must count as visible output")
	}
	if !hasAnyVisibleOutput(`{"choices":[{"index":0,"message":{"role":"assistant","content":[{"type":"image_url","image_url":{"url":"https://cdn.example.com/x.png"}}]}}]}`) {
		t.Fatal("kept http image link must count as visible output")
	}
	if hasAnyVisibleOutput(`{"choices":[{"index":0,"message":{"role":"assistant","content":""}}]}`) {
		t.Fatal("empty reply must stay empty")
	}
}
