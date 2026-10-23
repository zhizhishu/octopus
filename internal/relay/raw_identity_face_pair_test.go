package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
)

// 「戴 CLI 面容」与「发 CLI 身份头」必须同生同死。
//
// 【缺陷形态】raw 路径上这两件事曾经是两段独立判断：
//   - 发不发 codex 身份头 ← `isResponsesCompactRawProtocol(options)`
//   - 套不套 codex 面容   ← `channelTLSFace(channel)`（对**所有** raw endpoint 都套）
//
// 于是 /completions、/audio/transcriptions、/rerank 这些非 compact 端点在
// Responses 类型渠道上，线上是：
//
//	ClientHello = 真 codex rustls（ALPN h2+http/1.1）
//	User-Agent  = 通用 Chrome 桌面 UA（copyHeadersToUpstream 只补通用 UA）
//	Originator  = 没有
//
// 也就是「握手声称 codex_cli_rs、头声称浏览器、还缺真 CLI 必带的身份对」——
// 一个自相矛盾的强指纹组合，比两边都不装更像机器。
//
// 【为什么这条测试要真发请求】缺陷在调用点，不在任何一个选择函数里：
// channelTLSFace 本身没错、isResponsesCompactRawProtocol 也没错，错的是它们
// 被放在了互不相干的两个 if 里。只测这两个函数一定全绿。

// rawTLSFaceProbe 起一个真上游，记录它收到的 face（从请求 context 里读不到 ——
// context 不过网线），所以改从**出站请求本身**判断：codex 面容必然伴随
// codex 身份头，两者要么都有、要么都没有。
type rawProbeUpstream struct {
	srv       *httptest.Server
	lastUA    string
	lastOrig  string
	lastCodex string
	hits      int
}

func newRawProbeUpstream(t *testing.T) *rawProbeUpstream {
	t.Helper()
	p := &rawProbeUpstream{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.hits++
		p.lastUA = r.Header.Get("User-Agent")
		p.lastOrig = r.Header.Get("Originator")
		p.lastCodex = r.Header.Get("X-Codex-Beta-Features")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func newResponsesChannelForRaw(baseURL string) *dbmodel.Channel {
	return &dbmodel.Channel{
		ID:       7,
		Type:     1, // OutboundTypeOpenAIResponse -> channelTLSFace = "codex"
		Name:     "upstream.example",
		BaseUrls: []dbmodel.BaseUrl{{URL: baseURL}},
		Cloak:    dbmodel.ChannelCloak{Mode: "auto"},
		Keys:     []dbmodel.ChannelKey{{ID: 1, ChannelKey: "sk-test", Enabled: true}},
	}
}

func newRawGinContext(t *testing.T, body string) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	req := httptest.NewRequest(http.MethodPost, "/v1/rerank", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	return c
}

// 非 compact 端点：不许出现「codex 面容 + 通用头」这种半套伪装。
//
// 判定方式是"要么两边都有、要么两边都没有"：只要发出去的 UA 还是通用 Chrome UA
// （不是 codex CLI 的 UA），就不能同时戴着 codex 的握手身份。
// 由于 face 不在 HTTP 头里，这里用**渠道带 codex 面容**这个前提 +
// 「身份头是否配套」推出结论：identity 头缺失 = 这个 leg 不宣称 codex，
// 那它也不许穿 codex 的握手。
func TestRawNonCompactEndpointDoesNotWearCodexFaceWithoutCodexHeaders(t *testing.T) {
	up := newRawProbeUpstream(t)
	channel := newResponsesChannelForRaw(up.srv.URL)

	c := newRawGinContext(t, `{"model":"gpt-5.6-sol","query":"hi","documents":["a"]}`)
	options := RawProtocolOptions{Name: "rerank", Endpoint: "/rerank"}

	_, _, _, _, _, err := rawProtocolAttempt(
		context.Background(), options, c, nil,
		false, "", map[string]any{"model": "gpt-5.6-sol", "query": "hi", "documents": []any{"a"}},
		false, channel, "sk-test", 0, nil, "gpt-5.6-sol", "", false,
	)
	if err != nil {
		t.Fatalf("rawProtocolAttempt: %v", err)
	}
	if up.hits == 0 {
		t.Fatal("上游没收到请求，本测试没有观察到任何出站形态")
	}

	isCodexIdentity := strings.Contains(strings.ToLower(up.lastUA), "codex") ||
		up.lastOrig != "" || up.lastCodex != ""
	if !isCodexIdentity {
		// 这正是修复后的期望形态：这个 leg 不宣称 codex 身份。
		// 若它还穿着 codex 面容，就是"半套伪装" —— 而面容不在头里，无法从这里直接读，
		// 所以用下面那条 AST 用例守调用点。
		if strings.Contains(strings.ToLower(up.lastUA), "codex") {
			t.Fatalf("非 compact 端点发了 codex UA 却没发 Originator：身份对不配套")
		}
		return
	}
	// 若确实宣称了 codex 身份，则三件套必须齐全且配套。
	if up.lastOrig == "" {
		t.Fatalf("发了 codex UA(%q) 却没有 Originator：真 CLI 的身份对是配套的，缺一个反而更可疑", up.lastUA)
	}
	if up.lastCodex == "" {
		t.Fatalf("发了 codex 身份却没有 x-codex-* 头：三件套不齐，线上是半套伪装")
	}
}

// compact 端点（真 codex leg）必须**照旧**发全套 codex 身份头 ——
// 不能为了修上面那条把正常路径一起改坏。
func TestRawCompactEndpointStillSendsCodexIdentity(t *testing.T) {
	up := newRawProbeUpstream(t)
	channel := newResponsesChannelForRaw(up.srv.URL)

	payload := map[string]any{"model": "gpt-5.6-sol", "input": []any{}}
	c := newRawGinContext(t, `{"model":"gpt-5.6-sol","input":[]}`)
	options := RawProtocolOptions{Name: "responses_compact", Endpoint: "/responses/compact"}

	_, _, _, _, _, err := rawProtocolAttempt(
		context.Background(), options, c, nil,
		false, "", payload,
		false, channel, "sk-test", 0, nil, "gpt-5.6-sol", "", false,
	)
	if err != nil {
		t.Fatalf("rawProtocolAttempt: %v", err)
	}
	if up.hits == 0 {
		t.Fatal("上游没收到请求")
	}
	if up.lastOrig == "" {
		t.Fatalf("compact（真 codex leg）没发 Originator，UA=%q："+
			"这条 leg 必须保持全套 codex 身份，否则就是把它一起改坏了", up.lastUA)
	}
	if !strings.Contains(strings.ToLower(up.lastUA), "codex") {
		t.Fatalf("compact leg 的 UA 不是 codex CLI：%q", up.lastUA)
	}
}

// 守调用点：raw 路径里对 channelTLSFace 结果的赋值必须受 codex leg 判定约束，
// 不许再出现"对所有 endpoint 无条件套 face"的独立 if。
//
// 为什么要钉调用点：缺陷从来不在 channelTLSFace 这个函数里 —— 它按渠道类型返回
// 面容完全正确。错的是它被放在了一个与"发不发身份头"互不相干的 if 里。
// 只测那个函数永远抓不到。
func TestRawProtocolFaceBoundToCodexLeg(t *testing.T) {
	src, err := os.ReadFile("raw_protocol.go")
	if err != nil {
		t.Fatalf("read raw_protocol.go: %v", err)
	}
	text := string(src)

	// 必须存在把 codex leg 判定落成变量的那一行（修复后的接缝）。
	if !strings.Contains(text, "isCodexLeg") {
		t.Fatal("raw_protocol.go 里没有 isCodexLeg 这类判定变量：" +
			"「戴面容」与「发身份头」又会变成两段互不相干的判断，" +
			"非 compact 端点会重新穿上 codex 面容却不发 codex 头")
	}

	// face 的赋值必须来自 channelTLSFace，且在 isCodexLeg 的 else 分支里被清空。
	if !strings.Contains(text, "face := channelTLSFace(channel)") {
		t.Fatal("raw_protocol.go 里没有 face := channelTLSFace(channel)，接缝被挪走了")
	}
	if !strings.Contains(text, "face = \"\"") {
		t.Fatal("raw_protocol.go 里没有在非 codex leg 上清空 face 的分支：" +
			"戴面容与发身份头没有绑在一起")
	}
}
