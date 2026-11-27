package relay

// fix-ui1127b-r2 N1/N2/N3：上游生成的自由文本落库/回下游前统一过
// xredact.URLSecrets+xredact.Secrets——只护我们自己的上游密钥，下游客户端正文不受影响。

import (
	"errors"
	"strings"
	"testing"
)

func TestErrSafeMessageRedactsKeyShapedStrings(t *testing.T) {
	// 非 upstreamError 分支：错误文本里的密钥形状串必须被打掉（N3）。
	keyLike := "sk-live1234567890abcdef1234567890abcdef1234567890abcdef"
	msg := errSafeMessage(errors.New("upstream replied with key " + keyLike + " in body"))
	if strings.Contains(msg, keyLike) {
		t.Fatalf("key-shaped string survived errSafeMessage: %q", msg)
	}
	// URL 里的 key 参数同样（原有 URLSecrets 行为保持）。
	urlMsg := errSafeMessage(errors.New("dial https://api.example.com/v1/x?key=abc123SECRETxyz fail"))
	if strings.Contains(urlMsg, "abc123SECRETxyz") {
		t.Fatalf("url key survived errSafeMessage: %q", urlMsg)
	}
}

func TestMidjourneyDescriptionRedactedInLog(t *testing.T) {
	// N1：上游 description 里的密钥形状串/URL key 进不了 response_content。
	keyLike := "sk-live1234567890abcdef1234567890abcdef1234567890abcdef"
	redacted := redactUpstreamFreeText("bad key " + keyLike + " see https://h/?key=abc123SECRETxyz")
	if strings.Contains(redacted, keyLike) || strings.Contains(redacted, "abc123SECRETxyz") {
		t.Fatalf("upstream free text not redacted: %q", redacted)
	}
}

func TestGrokCloseReasonRedacted(t *testing.T) {
	// N2：grok 关闭帧文本同样打码——直接测生产用的同一组合。
	keyLike := "sk-live1234567890abcdef1234567890abcdef1234567890abcdef"
	out := truncateString(redactUpstreamFreeText("closed: "+keyLike), 256)
	if strings.Contains(out, keyLike) {
		t.Fatalf("close reason not redacted: %q", out)
	}
}
