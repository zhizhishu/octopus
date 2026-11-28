package relay

// pack-1128 监工纠偏双向钉：同一条带假 key 的上游错误——
// 落库/审计（admin）原样保留，回下游客户端的报错必须打码。

import (
	"net/http"
	"strings"
	"testing"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
)

func TestUpstreamErrorLogVerbatimClientRedacted(t *testing.T) {
	setupRelayErrorDB(t)
	if err := op.SettingSetString(dbmodel.SettingKeyUpstreamErrorStatusPass, "true"); err != nil {
		t.Fatalf("set passthrough: %v", err)
	}
	const fakeKey = "sk-FAKEFAKEFAKE0000-test-only"
	upErr := newUpstreamError(http.StatusUnauthorized,
		[]byte(`{"error":{"message":"bad key `+fakeKey+` rejected, see https://up.example/v1?key=`+fakeKey+`"}}`))

	// 审计/落库侧：原样。
	audit := auditErrorMessage(upErr)
	if !strings.Contains(audit, fakeKey) {
		t.Fatalf("audit message must preserve upstream text verbatim: %q", audit)
	}

	// 客户端侧：打码在唯一出口 relayErrorResponse 统一做。
	_, _, client := relayErrorResponse(upErr)
	if strings.Contains(client, fakeKey) {
		t.Fatalf("client-facing message leaked platform upstream key: %q", client)
	}
}

// Hark 00:3x 决策三路径钉：JSON / SSE / responses 流——客户端看不到 key 原文与渠道
// host，日志/审计原样。假 key，绝不用真实值。
func TestClientSafeUpstreamMessageExactKeyAndHost(t *testing.T) {
	const fakeKey = "sk-FAKECHANKEY0000-test-only"
	const fakeHost = "upstream-fake.example.internal"
	secrets := attemptSecretValues(&dbmodel.Channel{
		BaseUrls: []dbmodel.BaseUrl{{URL: "https://" + fakeHost + "/v1"}},
	}, fakeKey)
	if len(secrets) < 3 {
		t.Fatalf("collector must yield key + url + host, got %v", secrets)
	}
	raw := "Post \"https://upstream-fake.example.internal/v1/chat?key=sk-FAKECHANKEY0000-test-only\": dial tcp: connection refused"
	out := clientSafeUpstreamMessage(raw, secrets...)
	for _, leak := range []string{fakeKey, fakeHost} {
		if strings.Contains(out, leak) {
			t.Fatalf("client message leaked %q: %s", leak, out)
		}
	}
	if !strings.Contains(out, "[已省略]") {
		t.Fatalf("exact replacement marker missing: %s", out)
	}
	// 日志侧从不走这个函数：原文原样。
	if strings.Contains(raw, fakeKey) != true || strings.Contains(raw, fakeHost) != true {
		t.Fatal("test premise broken")
	}
}

func TestRelayErrorResponseExactRedactionExit(t *testing.T) {
	setupRelayErrorDB(t)
	if err := op.SettingSetString(dbmodel.SettingKeyUpstreamErrorStatusPass, "true"); err != nil {
		t.Fatalf("set passthrough: %v", err)
	}
	const fakeKey = "sk-FAKEEXITKEY0000-test-only"
	upErr := newUpstreamError(http.StatusUnauthorized,
		[]byte(`{"error":{"message":"bad key `+fakeKey+` rejected"}}`))
	// JSON 路径出口：relayErrorResponse 统一精确替换 + 形状打码。
	_, _, message := relayErrorResponse(upErr, fakeKey)
	if strings.Contains(message, fakeKey) {
		t.Fatalf("json client exit leaked key: %q", message)
	}
	// 不带精确名单时 xredact 形状兜底仍打码 sk- 形状。
	_, _, fallback := relayErrorResponse(upErr)
	if strings.Contains(fallback, fakeKey) {
		t.Fatalf("pattern fallback must redact sk- shaped key: %q", fallback)
	}
	// 审计侧同一条错误原样。
	if audit := auditErrorMessage(upErr); !strings.Contains(audit, fakeKey) {
		t.Fatalf("audit must be verbatim: %q", audit)
	}
}
