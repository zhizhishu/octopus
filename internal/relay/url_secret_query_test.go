package relay

// pack-1128（用户 2026-11-28 拍板）：URL query 里的凭据形状在 oct 本地错误文本/
// 日志中原样保留，不再打码。本文件替代原 url_secret_query_test.go 的打码断言。

import (
	"fmt"
	"strings"
	"testing"
)

func TestErrSafeMessagePreservesAPIKeyQuery(t *testing.T) {
	plain := fmt.Errorf(
		`Post "https://upstream.example/v1/models?apikey=%s&foo=1": dial tcp 203.0.113.1:443: connect: connection refused`,
		"channel-apikey-secret-test-only",
	)
	got := errSafeMessage(plain)
	if !strings.Contains(got, "apikey=channel-apikey-secret-test-only") {
		t.Fatalf("apikey query must be preserved verbatim: %s", got)
	}
	if !strings.Contains(got, "foo=1") || !strings.Contains(got, "connection refused") {
		t.Fatalf("rest of transport error altered: %s", got)
	}
	wrapped := &upstreamError{message: plain.Error()}
	if got := errSafeMessage(wrapped); !strings.Contains(got, "apikey=channel-apikey-secret-test-only") {
		t.Fatalf("wrapped upstream error must be preserved verbatim: %s", got)
	}
}
