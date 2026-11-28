package relay

// pack-1128（用户 2026-11-28 拍板）：oct 本地日志/审计/管理端原样保留上游文本，
// 不再打码。本文件断言 errSafeMessage / redactUpstreamFreeText 原样透传。
// 原 fix-ui1127b-r2 的打码断言随打码行为一并拆除。

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

func TestUpstreamFreeTextPreservedForLog(t *testing.T) {
	raw := "bad key sk-FAKEFAKEFAKEFAKE0000 see https://h/?key=abc123SECRETxyz"
	if got := redactUpstreamFreeText(raw); got != raw {
		t.Fatalf("upstream free text altered: %q", got)
	}
}

func TestGrokCloseReasonPreserved(t *testing.T) {
	raw := "closed: sk-FAKEFAKEFAKEFAKE0000-test-only"
	out := truncateString(redactUpstreamFreeText(raw), 256)
	if !strings.Contains(out, raw) {
		t.Fatalf("close reason altered: %q", out)
	}
}
