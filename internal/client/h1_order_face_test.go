package client

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
)

// HTTP/1.1 的头块顺序必须跟着 face 走，不能只看路径。
//
// 缺陷形态：TLS 握手自称 claude-code（或 codex），头块却是 Go 的序列化。
// 这是**自相矛盾**，比不伪装更容易被 shape check 抓 —— 真 CLI 的 21/14 项
// 顺序一个都对不上，而且连 Connection: keep-alive 都没有。
//
// 触发很具体：chat 渠道设了 cloak.shape=claude，出站路径是 /v1/chat/completions，
// 旧的 http1WireHeaderOrderForTarget 只按路径判，落进 default 返回 nil。
// h2 侧早已按 face 优先（canonicalHeaderOrderForFace），h1 是漏的那条腿。
func TestHTTP1WireHeaderOrderFollowsFace(t *testing.T) {
	// 关键：路径是 chat，两张表都不含 "messages"/"responses"，
	// 所以这条**只有** face 生效时才可能拿到非 nil。
	const chatPath = "/v1/chat/completions"

	t.Run("claude_face_on_chat_path", func(t *testing.T) {
		got := http1WireHeaderOrderForFace("POST", chatPath, tlsFaceClaude)
		if len(got) == 0 {
			t.Fatal("chat 路径 + claude face 拿到 nil：握手伪装成 claude-cli，头块却回落到 Go 的序列化")
		}
		if len(got) != len(claudeHTTP1WireHeaderOrder) {
			t.Fatalf("拿到的不是 claude 表：len=%d want %d", len(got), len(claudeHTTP1WireHeaderOrder))
		}
		for i := range got {
			if got[i] != claudeHTTP1WireHeaderOrder[i] {
				t.Fatalf("第 %d 项 = %q, want %q", i, got[i], claudeHTTP1WireHeaderOrder[i])
			}
		}
	})

	t.Run("codex_face_on_chat_path", func(t *testing.T) {
		got := http1WireHeaderOrderForFace("POST", chatPath, tlsFaceCodex)
		if len(got) == 0 {
			t.Fatal("chat 路径 + codex face 拿到 nil")
		}
		if got[0] != codexHTTP1WireHeaderOrder[0] {
			t.Fatalf("首项 = %q, want %q", got[0], codexHTTP1WireHeaderOrder[0])
		}
	})

	// 没有 face 时保持历史行为：仍按路径判（防"修 face 把默认路径改坏"）。
	t.Run("no_face_keeps_path_rule", func(t *testing.T) {
		if got := http1WireHeaderOrderForFace("POST", "/v1/messages", ""); len(got) == 0 {
			t.Fatal("/v1/messages 无 face 时应当拿到 claude 顺序")
		}
		if got := http1WireHeaderOrderForFace("POST", "/v1/responses", ""); len(got) == 0 {
			t.Fatal("/v1/responses 无 face 时应当拿到 codex 顺序")
		}
		if got := http1WireHeaderOrderForFace("POST", chatPath, ""); got != nil {
			t.Fatalf("无 face + 未知路径应当返回 nil（保持 Go 原样），got %v", got)
		}
	})

	// chrome face 是"没有可模仿的 CLI"，必须回落 nil 而不是硬套某张表。
	t.Run("chrome_face_returns_nil", func(t *testing.T) {
		if got := http1WireHeaderOrderForFace("POST", "/v1/messages", tlsFaceChrome); got != nil {
			t.Fatalf("chrome face 应当返回 nil，got %v", got)
		}
	})
}

// 上面那条只验证了选择器函数。这条钉住**拨号闭包真的用了 face 版选择器**。
//
// 为什么必须单独有：把拨号闭包退回 `http1RequestHeaderOrder`（丢掉 family）时，
// 选择器函数的测试**依然全绿** —— 它们根本不经过拨号闭包。变异实测确认。
// 这正是"测了函数、没测调用点"的老坑，所以这里直接取闭包构造出来的函数来调。
func TestH1DialerUsesFaceAwareOrder(t *testing.T) {
	// chat 路径：两张表都不匹配，只有 face 生效才能拿到非 nil 顺序。
	const chatPath = "/v1/chat/completions"

	claudeOrder := h1OrderFuncForFace(tlsFaceClaude)("POST", chatPath)
	if len(claudeOrder) == 0 {
		t.Fatal("claude face 的拨号闭包在 chat 路径返回 nil：" +
			"握手伪装成 claude-cli，头块却是 Go 的序列化")
	}

	codexOrder := h1OrderFuncForFace(tlsFaceCodex)("POST", chatPath)
	if len(codexOrder) == 0 {
		t.Fatal("codex face 的拨号闭包在 chat 路径返回 nil")
	}

	// 两个 face 必须给出不同的顺序（否则 face 被忽略了）。
	if len(claudeOrder) == len(codexOrder) && claudeOrder[0] == codexOrder[0] {
		t.Fatal("claude 与 codex face 给出相同顺序：face 没有真正参与选择")
	}

	// chrome 是"没有可模仿的 CLI"，必须 nil（保持 Go 原样）。
	if got := h1OrderFuncForFace(tlsFaceChrome)("POST", chatPath); got != nil {
		t.Fatalf("chrome face 应当返回 nil，got %v", got)
	}
}

// 每个 family 必须有独立的 h1 transport（否则 claude 的头序会被 codex 复用）。
func TestH1TransportIsPerFamily(t *testing.T) {
	rt := &utlsRoundTripper{h1Transports: make(map[string]*http.Transport)}

	claude, err := rt.h1TransportForFamily(tlsFaceClaude, nil)
	if err != nil {
		t.Fatalf("claude: %v", err)
	}
	codex, err := rt.h1TransportForFamily(tlsFaceCodex, nil)
	if err != nil {
		t.Fatalf("codex: %v", err)
	}
	if claude == codex {
		t.Fatal("claude 与 codex 复用了同一个 h1 transport：连接池会串 family，头序跟着串")
	}

	again, err := rt.h1TransportForFamily(tlsFaceClaude, nil)
	if err != nil {
		t.Fatalf("claude again: %v", err)
	}
	if again != claude {
		t.Fatal("同一 family 没有复用 transport：每次请求都会重新握手")
	}
}

// 端到端：真的发一个 HTTP/1.1 请求出去，抓**线上头块**，确认 face 顺序生效。
//
// 前面两条只碰纯函数，把拨号闭包退回 http1RequestHeaderOrder 时它们全绿
// （变异实测两次都是 ok）—— 这就是"测了函数、没测调用点"。这条走完整
// RoundTrip → 拨号闭包 → orderedRequestConn → 线上字节，绕不过去。
func TestH1WireOrderUsesFaceEndToEnd(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	// chat 路径：两张表都不含 messages/responses，只有 face 参与选择时才拿得到顺序。
	req, err := http.NewRequest(http.MethodPost, "http://"+ln.Addr().String()+"/v1/chat/completions", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	// 直接复用生产的 per-family transport 构造，但把 TLS 拨号换成明文拨号
	// （本测试只关心头块顺序，不关心握手）。
	rt := &utlsRoundTripper{h1Transports: make(map[string]*http.Transport)}
	tr := &http.Transport{DisableCompression: true, MaxIdleConnsPerHost: 2}
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, dialErr := (&net.Dialer{}).DialContext(ctx, network, addr)
		if dialErr != nil {
			return nil, dialErr
		}
		// 🔴 这一行是生产拨号闭包的等价物：face 必须传到头块。
		return newOrderedRequestConn(conn, h1OrderFuncForFace(tlsFaceClaude)), nil
	}
	rt.h1Transports[tlsFaceClaude] = tr

	lines := roundTripCaptureBlock(t, tr, req, ln)
	if len(lines) == 0 {
		t.Fatal("没有抓到请求头块")
	}
	// claude 表把 host 放在第 0 位、user-agent 紧随其后；
	// Go 的默认顺序会先 Host 再 User-Agent 之后按 ASCII 排 ——
	// 用"user-agent 是否紧跟 host"这个可判定的信号区分两者。
	hostIdx, uaIdx := -1, -1
	for i, ln := range lines {
		lower := strings.ToLower(ln)
		if strings.HasPrefix(lower, "host:") && hostIdx < 0 {
			hostIdx = i
		}
		if strings.HasPrefix(lower, "user-agent:") && uaIdx < 0 {
			uaIdx = i
		}
	}
	if hostIdx < 0 || uaIdx < 0 {
		t.Fatalf("头块里缺 host 或 user-agent：%v", lines)
	}
	// claudeHTTP1WireHeaderOrder 把 Host 排在**最后一批**（第 18-21 项，真 CLI 的
	// 自家 HTTP 栈习惯），而 Go 的默认序列化把 Host 放在最前。这个差异方向稳定、
	// 可判定：face 生效 ⇒ host 在 ua 之后；回落 Go ⇒ host 在最前。
	if hostIdx < uaIdx {
		t.Fatalf("Host@%d 排在 User-Agent@%d 之前：头块仍是 Go 的序列化，face 的 h1 顺序没生效 —— "+
			"握手说 claude、头块说 Go，这种自相矛盾比不伪装更容易被 shape check 抓", hostIdx, uaIdx)
	}
}
