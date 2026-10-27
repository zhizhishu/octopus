package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	transformerModel "github.com/zhizhishu/octopus/internal/transformer/model"
	openaiOutbound "github.com/zhizhishu/octopus/internal/transformer/outbound/openai"
)

// 真实出站请求体日志（2026-10-27）：RequestContent 记的是内部结构还原后的假象，
// 不是真正发给上游的字节。这里钉死两者**必然不同**，防止有人把这份快照改回
// 「序列化 InternalRequest」——那样排查又变成猜谜。

func kimiInternalRequest() *transformerModel.InternalLLMRequest {
	stream := true
	content := "你好"
	return &transformerModel.InternalLLMRequest{
		Model:           "moonshotai/Kimi-K3",
		ReasoningEffort: "xhigh",
		Stream:          &stream,
		Messages: []transformerModel.Message{{
			Role:    "user",
			Content: transformerModel.MessageContent{Content: &content},
		}},
	}
}

func TestSnapshotOutboundRequestBodyKeepsBodySendable(t *testing.T) {
	out := &openaiOutbound.CustomChatOutbound{}
	req, err := out.TransformRequest(context.Background(), kimiInternalRequest(), "http://upstream.example", "k")
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	wantLen := req.ContentLength
	if wantLen <= 0 {
		t.Fatalf("outbound body should have a length, got %d", wantLen)
	}

	snapshot, err := snapshotOutboundRequestBody(req)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if int64(len(snapshot)) != wantLen {
		t.Fatalf("snapshot len %d != ContentLength %d", len(snapshot), wantLen)
	}

	// 🔴 快照之后请求必须还能正常发出：Body 与 GetBody 都要被重挂。
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("body unreadable after snapshot: %v", err)
	}
	if string(body) != string(snapshot) {
		t.Fatalf("re-mounted body differs from snapshot")
	}
	if req.GetBody == nil {
		t.Fatal("GetBody must be re-mounted for redirects/retries")
	}
	again, err := req.GetBody()
	if err != nil {
		t.Fatalf("GetBody: %v", err)
	}
	second, err := io.ReadAll(again)
	if err != nil || string(second) != string(snapshot) {
		t.Fatalf("GetBody content differs from snapshot: %v", err)
	}
	if req.ContentLength != int64(len(snapshot)) {
		t.Fatalf("ContentLength not restored: %d", req.ContentLength)
	}
}

// TestUpstreamRequestBodyDiffersFromInternalSerialization 是本轮 bug 的回归锁：
// 同一次尝试里，「真实出站 body」与「json.Marshal(InternalRequest)」（旧日志口径）
// 必须不同——出站发了 thinking 并清掉 reasoning_effort，而内部结构还原后恰好相反。
func TestUpstreamRequestBodyDiffersFromInternalSerialization(t *testing.T) {
	internalReq := kimiInternalRequest()
	out := &openaiOutbound.CustomChatOutbound{}
	req, err := out.TransformRequest(context.Background(), internalReq, "http://upstream.example", "k")
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	snapshot, err := snapshotOutboundRequestBody(req)
	if err != nil || len(snapshot) == 0 {
		t.Fatalf("snapshot: %v", err)
	}

	// 旧口径：直接把共享的 InternalRequest 序列化（transform 的 defer 已还原）。
	legacy, err := json.Marshal(internalReq)
	if err != nil {
		t.Fatalf("marshal internal: %v", err)
	}

	wire := string(snapshot)
	old := string(legacy)
	if wire == old {
		t.Fatal("真实出站 body 不能等于内部结构序列化——那样这份快照就没有意义")
	}
	// 出站：thinking 在、reasoning_effort 已清。
	if !strings.Contains(wire, `"thinking"`) {
		t.Fatalf("出站 body 应携带 thinking 字段，实际: %s", wire)
	}
	if strings.Contains(wire, `"reasoning_effort"`) {
		t.Fatalf("kimi 出站 body 不应残留 reasoning_effort，实际: %s", wire)
	}
	// 旧口径恰好相反——这正是它误导排查的原因。
	if !strings.Contains(old, `"reasoning_effort"`) {
		t.Fatalf("内部结构序列化应保留客户端原始 reasoning_effort，实际: %s", old)
	}
	if strings.Contains(old, `"thinking"`) {
		t.Fatalf("内部结构序列化不该含 transform 期间注入的 thinking，实际: %s", old)
	}
}

func TestUpstreamRequestBodySnapshotIsRedacted(t *testing.T) {
	metrics := NewRelayMetrics(0, 0, "", "", kimiInternalRequest())
	body := []byte(`{"model":"m","api_key":"sk-should-not-be-logged-as-is"}`)
	metrics.SetUpstreamRequestBody(body)
	if metrics.upstreamRequestBody == "" {
		t.Fatal("快照应被记录")
	}
	if strings.Contains(metrics.upstreamRequestBody, "sk-should-not-be-logged-as-is") &&
		metrics.upstreamRequestBody == string(body) {
		t.Fatal("落库前必须脱敏")
	}
}

// 开关关闭时不采集：默认行为逐字节不变。
func TestUpstreamRequestBodyLogDisabledByDefault(t *testing.T) {
	if upstreamRequestBodyLogEnabled() {
		t.Fatal("默认必须是关闭，否则每条请求都会多存一份 body")
	}
}

// 编译期确认 http 包在本文件被使用（快照辅助函数签名依赖它）。
var _ = http.MethodPost

// 存储上限：超长请求体只截断**存储的那份**，发给上游的字节永远完整。
func TestUpstreamRequestBodySnapshotIsTruncated(t *testing.T) {
	big := strings.Repeat("x", upstreamRequestBodyLogLimit+1024)
	metrics := NewRelayMetrics(0, 0, "", "", kimiInternalRequest())
	metrics.SetUpstreamRequestBody([]byte(big))

	if len(metrics.upstreamRequestBody) > upstreamRequestBodyLogLimit+128 {
		t.Fatalf("快照未被截断，长度 %d", len(metrics.upstreamRequestBody))
	}
	if !strings.Contains(metrics.upstreamRequestBody, "truncated") {
		t.Fatalf("截断必须显式标记，实际尾部: %q",
			metrics.upstreamRequestBody[len(metrics.upstreamRequestBody)-60:])
	}
	if !strings.Contains(metrics.upstreamRequestBody, fmt.Sprintf("%d bytes total", len(big))) {
		t.Fatal("截断标记必须写明原始总长，否则排查会误以为请求体就这么大")
	}
}

// 截断只作用于存储；出站 body 本身必须仍完整（否则会把请求发残）。
func TestSnapshotKeepsFullBodyWhileStorageTruncates(t *testing.T) {
	payload := strings.Repeat("y", upstreamRequestBodyLogLimit+512)
	req, err := http.NewRequest(http.MethodPost, "http://upstream.example/v1/chat/completions",
		strings.NewReader(payload))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	snapshot, err := snapshotOutboundRequestBody(req)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(snapshot) != len(payload) {
		t.Fatalf("快照应保留完整 body：%d != %d", len(snapshot), len(payload))
	}
	rest, err := io.ReadAll(req.Body)
	if err != nil || len(rest) != len(payload) {
		t.Fatalf("重挂后的 body 必须完整可发：%d (err=%v)", len(rest), err)
	}
}

// 顺序锁：必须先脱敏后截断。
//
// 打中的是脱敏规则里**有最小长度要求**的那一条（裸 hex 需 ≥32 位，
// internal/utils/xredact/redact.go）。截断把 40 位密钥切成 10 位后，
// 若先截断再脱敏，这 10 位不再匹配规则 → 半截密钥落进日志。
func TestUpstreamRequestBodyRedactsBeforeTruncating(t *testing.T) {
	// 前缀用非 hex 字符，避免它自己先被 hex 规则吃掉而改变截断位置。
	// body = `{"k":"`(6) + prefix + secret + `"}`(2)。要让截断点正好落在
	// secret 中间、只留下 10 位 hex，prefix 长度须为 limit-6-10。
	//
	// 🔴 prefix 必须用非 word 字符（这里用 '-'）：裸 hex 规则带词边界 ，
	// 若前缀是字母，边界不成立、规则本来就不会命中，这条测试就测不到东西。
	prefix := strings.Repeat("-", upstreamRequestBodyLogLimit-16)
	secret := strings.Repeat("c", 40) // 40 位裸 hex，命中 ≥32 规则
	metrics := NewRelayMetrics(0, 0, "", "", kimiInternalRequest())
	metrics.SetUpstreamRequestBody([]byte(`{"k":"` + prefix + secret + `"}`))

	leaked := strings.Repeat("c", 10)
	if strings.Contains(metrics.upstreamRequestBody, leaked) {
		t.Fatal("截断边界上的长 hex 密钥必须先被脱敏，否则留下 10 位碎片")
	}
}

// UTF-8 边界锁：中文正文被截断时不能产生半截字符。
func TestUpstreamRequestBodyTruncatesOnRuneBoundary(t *testing.T) {
	// 每个汉字 3 字节；总长刻意让截断点落在字符中间。
	body := strings.Repeat("啊", upstreamRequestBodyLogLimit)
	metrics := NewRelayMetrics(0, 0, "", "", kimiInternalRequest())
	metrics.SetUpstreamRequestBody([]byte(body))
	if !utf8.ValidString(metrics.upstreamRequestBody) {
		t.Fatal("截断后必须仍是合法 UTF-8，否则落库/渲染会乱码")
	}
}
