package relay

import (
	"strings"
	"testing"
)

// 入库正文必须把三种钥匙打成 [redacted]，普通文字原样保留。
// 出站请求体不走 redactLogBody，所以这里不存在“打码误伤上游”的路径。
func TestRedactLogBodyStripsCredentials(t *testing.T) {
	in := `{"messages":[{"content":"key sk-CANARYKEY00000000000000000000000000000000 then ` +
		`eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJjYW5hcnkifQ.c2lnbmF0dXJl and ` +
		`deadbeefdeadbeefdeadbeefdeadbeef"}],"model":"claude-fable-5-1"}`

	got := redactLogBody(in)

	for _, leak := range []string{"sk-CANARY", "eyJhbGci", "deadbeefdeadbeef"} {
		if strings.Contains(got, leak) {
			t.Errorf("logged body still contains %q: %s", leak, got)
		}
	}
	if !strings.Contains(got, "[redacted]") {
		t.Errorf("expected [redacted] marker, got %s", got)
	}
	if !strings.Contains(got, "claude-fable-5-1") {
		t.Errorf("model name must survive redaction, got %s", got)
	}
	if redactLogBody("plain text") != "plain text" {
		t.Fatal("text without credentials must be unchanged")
	}
}

// 空白正文和空数组是校验日志要保留的信号，不是钥匙。
func TestRedactLogBodyKeepsValidationSignals(t *testing.T) {
	for _, signal := range []string{`"content":"   "`, `"input":"   "`, `"messages":[]`, `"content":""`, `"text":"   "`} {
		if !strings.Contains(redactLogBody(signal), signal) {
			t.Fatalf("validation signal %s was redacted", signal)
		}
	}
}

// 图片 / 原始协议 / 视频这几条落库路径在赋值处打码，不访问网络。
func TestRedactLogBodyOnPersistPaths(t *testing.T) {
	const sk = "sk-CANARYKEY00000000000000000000000000000000"
	const jwt = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJjYW5hcnkifQ.c2lnbmF0dXJl"
	const hexKey = "deadbeefdeadbeefdeadbeefdeadbeef"
	payload := map[string]any{
		"model":  "claude-fable-5-1",
		"prompt": "key " + sk + " then " + jwt + " and " + hexKey,
	}

	// 与 images.go / raw_protocol.go / videos.go / client_validation.go 的赋值表达式相同。
	bodies := []string{
		redactLogBody(buildImagesRequestContentForLog(false, nil, payload)),
		redactLogBody(buildRawProtocolRequestContent(false, nil, payload, "responses")),
		redactLogBody(buildRawProtocolRequestContent(false, nil, payload, "videos")),
		redactLogBody(truncateString(strings.TrimSpace(`{"model":"claude-fable-5-1","messages":[{"content":"`+sk+" "+jwt+" "+hexKey+`"}]}`), 8*1024)),
	}
	for i, got := range bodies {
		for _, leak := range []string{"sk-CANARY", "eyJhbGci", "deadbeefdeadbeef"} {
			if strings.Contains(got, leak) {
				t.Errorf("path %d still contains %q: %s", i, leak, got)
			}
		}
		if strings.Count(got, "[redacted]") < 3 {
			t.Errorf("path %d want 3 [redacted], got %s", i, got)
		}
		if !strings.Contains(got, "claude-fable-5-1") {
			t.Errorf("path %d dropped model name: %s", i, got)
		}
	}
}
