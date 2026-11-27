package relay

import (
	"strings"
	"testing"
)

// 落库正文的脱敏边界（2026-12 用户决定，见 metrics.go 文件头注释）：下游客户端
// 请求/响应正文完整收集、不打码——密钥形态的字符串照原样保留，抓滥用要靠原文。
// 这里的用例钉住各条落库路径都不再把正文打成 [redacted]；上游凭据的打码在
// 调试捕获（debug_capture.go）与上游错误路径（upstream_error.go），另有测试钉住，
// 出站请求体也不经过这套路径。

// 图片 / 原始协议 / 视频 / 客户端校验这几条落库路径，密钥形态的正文必须原样保留。
func TestLogBodyPersistPathsKeepClientTextRaw(t *testing.T) {
	withLogImages(t)
	const sk = "sk-CANARYKEY00000000000000000000000000000000"
	const jwt = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJjYW5hcnkifQ.c2lnbmF0dXJl"
	const hexKey = "deadbeefdeadbeefdeadbeefdeadbeef"
	payload := map[string]any{
		"model":  "claude-fable-5-1",
		"prompt": "key " + sk + " then " + jwt + " and " + hexKey,
	}

	// 与 images.go / raw_protocol.go / videos.go / client_validation.go 的赋值表达式
	// 同构：前三条走 contentWithImages（剥离 → 原样 → 截断），校验路径直接截 8KB。
	var l upstreamBodyLog
	bodies := []string{
		l.contentWithImages(func(capture *imageCapture) string {
			return buildImagesRequestContentForLogWith(false, nil, payload, capture)
		}),
		l.contentWithImages(func(capture *imageCapture) string {
			return buildRawProtocolRequestContentWith(false, nil, payload, "responses", capture)
		}),
		l.contentWithImages(func(capture *imageCapture) string {
			return buildRawProtocolRequestContentWith(false, nil, payload, "videos", capture)
		}),
		truncateString(strings.TrimSpace(`{"model":"claude-fable-5-1","messages":[{"content":"`+sk+" "+jwt+" "+hexKey+`"}]}`), 8*1024),
	}
	for i, got := range bodies {
		for _, want := range []string{sk, jwt, hexKey, "claude-fable-5-1"} {
			if !strings.Contains(got, want) {
				t.Errorf("path %d lost client text %q: %s", i, want, got)
			}
		}
		if strings.Contains(got, "[redacted]") {
			t.Errorf("path %d must not redact client text: %s", i, got)
		}
	}
}

// 普通文字与空白校验信号（空 content / 空 messages）都原样保留，不引入任何改写。
// body 里带一个图片 key（payload marker），确保真的进了 JSON 剥离走查而不是旁路。
func TestLogBodyPersistPathsKeepValidationSignals(t *testing.T) {
	withLogImages(t)
	var l upstreamBodyLog
	for _, signal := range []string{`"content":"   "`, `"input":"   "`, `"messages":[]`, `"content":""`, `"text":"   "`} {
		body := `{"model":"m",` + signal + `,"image":"https://example.com/a.png"}`
		got := l.contentWithImages(func(capture *imageCapture) string {
			return stripInlineBinaryWith(body, capture)
		})
		if !strings.Contains(got, signal) {
			t.Fatalf("validation signal %s was rewritten: %s", signal, got)
		}
		if !strings.Contains(got, `"image":"https://example.com/a.png"`) {
			t.Fatalf("http reference must be kept: %s", got)
		}
	}
}
