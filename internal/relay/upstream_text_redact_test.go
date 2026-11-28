package relay

// pack-1128（用户 2026-11-28 拍板）：oct 本地日志/审计/管理端原样保留上游文本，
// 不再打码。本文件断言 errSafeMessage 与 grok 关闭原因原样透传。
// pack-1128 审查 P5：恒等函数 redactUpstreamFreeText 已拆除，断言改走真正存活的出口。

import (
	"errors"
	"strings"
	"testing"
)

func TestErrSafeMessagePreservesOriginalText(t *testing.T) {
	keyLike := "sk-FAKEFAKEFAKEFAKE0000-test-only"
	raw := "upstream replied with key " + keyLike + " in body"
	if got := errSafeMessage(errors.New(raw)); got != raw {
		t.Fatalf("errSafeMessage altered text: %q", got)
	}
	rawURL := "dial https://api.example.com/v1/x?key=abc123SECRETxyz fail"
	if got := errSafeMessage(errors.New(rawURL)); got != rawURL {
		t.Fatalf("errSafeMessage altered url text: %q", got)
	}
}

func TestGrokCloseReasonPreserved(t *testing.T) {
	raw := "closed: sk-FAKEFAKEFAKEFAKE0000-test-only"
	out := truncateString(raw, 256)
	if !strings.Contains(out, raw) {
		t.Fatalf("close reason altered: %q", out)
	}
}
