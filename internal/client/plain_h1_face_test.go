package client

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
)

// 明文 HTTP/1.1 的头序必须跟 face 走，不能只看路径。
//
// 缺陷形态：chat 渠道 cloak.shape=claude（或 codex）出站打到 http:// 上游的
// /v1/chat/completions。TLS 腿的拨号闭包用的是 http1WireHeaderOrderForFace，
// 明文腿却把 http1RequestHeaderOrder 封进连接 —— 那个函数只看路径，chat 路径
// 两张表都对不上，返回 nil。于是头是 CLI 的，头块顺序却是 Go 的：自相矛盾，
// 比不伪装更容易被 shape check 抓。
//
// face 的来源与 uTLS 拨号相同（tlsFamilyForDial：context 里的 WithTLSFace 优先，
// 否则按路径）。传输在进程内缓存，所以 face 是连接池的 key，由拨号闭包捕获，
// 而不是在第一次 Dial 时从 ctx 读出来冻上。

func chatOrderRequest(t *testing.T, rawURL, face string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, rawURL, strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "ua-test")
	if face != "" {
		req = req.WithContext(WithTLSFace(req.Context(), face))
	}
	return req
}

func wireHeaderNames(lines []string) []string {
	if len(lines) < 2 {
		return nil
	}
	names := make([]string, 0, len(lines)-2)
	for _, line := range lines[1 : len(lines)-1] {
		name, _, ok := strings.Cut(strings.TrimRight(line, "\r\n"), ":")
		if !ok {
			continue
		}
		names = append(names, name)
	}
	return names
}

// 最小 chat 请求（Content-Type + User-Agent + 正文）在三套顺序下的头名。
// Go 的默认序列化是 Host、User-Agent、Content-Length，然后其余按 ASCII。
var (
	claudeChatWireNames = []string{"Content-Type", "User-Agent", "Connection", "Host", "Content-Length"}
	codexChatWireNames  = []string{"content-type", "user-agent", "host", "content-length"}
	goChatWireNames     = []string{"Host", "User-Agent", "Content-Length", "Content-Type"}
)

func assertWireNames(t *testing.T, lines, want []string) {
	t.Helper()
	got := wireHeaderNames(lines)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("header names = %v, want %v\nraw=%q", got, want, lines)
	}
}

// TestPlaintextDialClosureHonorsFace 钉住明文拨号闭包本身。
//
// 只测 http1WireHeaderOrderForFace / plainHTTP1HeaderOrder 守不住调用点：
// 把 plainHTTP1OrderingDial 里的 newOrderedRequestConn 退回 http1RequestHeaderOrder
// 时，那些纯函数测试全绿，而这条会红 —— chat 路径 + claude/codex face 将退回
// Go 的默认顺序。
func TestPlaintextDialClosureHonorsFace(t *testing.T) {
	t.Run("shape_claude", func(t *testing.T) {
		lines := roundTripDialFamily(t, tlsFaceClaude, "/v1/chat/completions")
		assertWireNames(t, lines, claudeChatWireNames)
	})
	t.Run("shape_codex", func(t *testing.T) {
		lines := roundTripDialFamily(t, tlsFaceCodex, "/v1/chat/completions")
		assertWireNames(t, lines, codexChatWireNames)
	})
	// shape 空/未知、cloak never/off：channelTLSFace 返回 ""，tlsFamilyForDial
	// 对 chat 路径给出 chrome，头序必须是 nil（Go 原样）。
	t.Run("shape_off", func(t *testing.T) {
		lines := roundTripDialFamily(t, tlsFaceChrome, "/v1/chat/completions")
		assertWireNames(t, lines, goChatWireNames)
	})
	t.Run("cloak_never", func(t *testing.T) {
		lines := roundTripDialFamily(t, "", "/v1/chat/completions")
		assertWireNames(t, lines, goChatWireNames)
	})
	// 没 face 时路径规则照旧：Anthropic / Responses 跟改动前逐字节相同。
	t.Run("messages_path_rule", func(t *testing.T) {
		lines := roundTripDialFamily(t, tlsFaceClaude, "/v1/messages")
		assertWireNames(t, lines, claudeChatWireNames)
	})
	t.Run("responses_path_rule", func(t *testing.T) {
		lines := roundTripDialFamily(t, tlsFaceCodex, "/v1/responses")
		assertWireNames(t, lines, codexChatWireNames)
	})
}

// roundTripDialFamily 走生产的 plainHTTP1OrderingDial，不经 faceKeyed 中转，
// 这样拨号闭包退回 http1RequestHeaderOrder 时本测试必定失败。
func roundTripDialFamily(t *testing.T, family, path string) []string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	tr := &http.Transport{DisableCompression: true, MaxIdleConnsPerHost: 2}
	tr.DialContext = plainHTTP1OrderingDial(nil, family)
	req := chatOrderRequest(t, "http://"+ln.Addr().String()+path, "")
	return roundTripCaptureBlock(t, tr, req, ln)
}

func TestPlaintextDialClosureLeavesConnectAlone(t *testing.T) {
	// 带 claude face 的传输也会承接 HTTP 代理的 CONNECT（target 是 host:port）。
	// 重写这段握手会把 Connection: keep-alive 塞进 CONNECT，历史路径规则这里返回 nil。
	if got := plainHTTP1HeaderOrder(tlsFaceClaude)(http.MethodConnect, "example.com:443"); got != nil {
		t.Fatalf("CONNECT must stay nil under a claude face, got %v", got)
	}
	got := plainHTTP1HeaderOrder(tlsFaceClaude)(http.MethodPost, "/v1/chat/completions")
	if !reflect.DeepEqual(got, claudeHTTP1WireHeaderOrder) {
		t.Fatalf("POST chat under claude face = %v, want claude table", got)
	}
}

func TestPlaintextHeaderOrderSettingOff(t *testing.T) {
	setUpstreamHTTP1HeaderOrder(t, "false")
	// 开关按请求读。face 即使是 claude，关闭时也必须是 nil，头块与 Go 序列化逐字节相同。
	lines := roundTripDialFamily(t, tlsFaceClaude, "/v1/chat/completions")
	assertWireNames(t, lines, goChatWireNames)

	if got := plainHTTP1HeaderOrder(tlsFaceCodex)(http.MethodPost, "/v1/chat/completions"); got != nil {
		t.Fatalf("setting off must force nil order, got %v", got)
	}
	if got := plainHTTP1HeaderOrder(tlsFaceClaude)(http.MethodPost, "/v1/messages"); got != nil {
		t.Fatalf("setting off must force nil even on /v1/messages, got %v", got)
	}
}

func setUpstreamHTTP1HeaderOrder(t *testing.T, value string) {
	t.Helper()
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "octopus.db"), false); err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() {
		// 本包其余测试把「读不到设置」当成默认开。测完把开关拨回开，
		// 避免 ReplaceAll 之后的缓存把它们打成关。
		_ = op.SettingSetString(model.SettingKeyUpstreamHTTP1HeaderOrder, "true")
		if err := db.Close(); err != nil {
			t.Errorf("close db: %v", err)
		}
	})
	if err := op.InitCache(); err != nil {
		t.Fatalf("init cache: %v", err)
	}
	if err := op.SettingSetString(model.SettingKeyUpstreamHTTP1HeaderOrder, value); err != nil {
		t.Fatalf("set %s: %v", model.SettingKeyUpstreamHTTP1HeaderOrder, err)
	}
}

// TestPlaintextFaceKeyedRoundTripIsolatesFaces 走生产的 RoundTrip：face 从请求
// context 解出（与 uTLS 同一函数），再选对应连接池。
//
// 同一传输上连续发 claude、codex、无 face 三条 chat 请求，服务端保持连接。
// 若拨号时把第一次的 face 冻进共享连接，后两条会沿用 claude 顺序，本测试失败。
func TestPlaintextFaceKeyedRoundTripIsolatesFaces(t *testing.T) {
	base, err := clonedDefaultTransport()
	if err != nil {
		t.Fatalf("clonedDefaultTransport: %v", err)
	}
	base.Proxy = nil
	rt := newFaceKeyedPlainTransport(base)

	addr, blocks := startKeepAliveCapture(t, 5, nil)
	client := &http.Client{Transport: rt, Timeout: 5 * time.Second}

	do := func(path, face string) {
		t.Helper()
		req := chatOrderRequest(t, "http://"+addr+path, face)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("do %s face=%q: %v", path, face, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	do("/v1/chat/completions", tlsFaceClaude)
	do("/v1/chat/completions", tlsFaceCodex)
	do("/v1/chat/completions", "") // shape 空 / cloak never：没有 WithTLSFace
	do("/v1/messages", "")         // Anthropic 路径规则，无 context face
	do("/v1/responses", "")

	want := [][]string{
		claudeChatWireNames,
		codexChatWireNames,
		goChatWireNames,
		claudeChatWireNames,
		codexChatWireNames,
	}
	// 每条服务端连接只能见到一种头序：不同 face 共用一条连接就是指纹串味。
	faceByConn := make(map[int][]string)
	for i, names := range want {
		select {
		case b := <-blocks:
			assertWireNames(t, b.lines, names)
			if prev, ok := faceByConn[b.conn]; ok && !reflect.DeepEqual(prev, names) {
				t.Fatalf("request %d reused server conn %d that already carried %v, now %v", i, b.conn, prev, names)
			}
			faceByConn[b.conn] = names
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for request %d", i)
		}
	}

	// 三个 face 必须是三份传输，否则空闲连接会串顺序。
	claudeTr := rt.transportFor(tlsFaceClaude)
	codexTr := rt.transportFor(tlsFaceCodex)
	chromeTr := rt.transportFor(tlsFaceChrome)
	if claudeTr == codexTr || claudeTr == chromeTr || codexTr == chromeTr {
		t.Fatal("plaintext transports for claude/codex/chrome must not be shared")
	}
	if rt.transportFor(tlsFaceClaude) != claudeTr {
		t.Fatal("same face must reuse its transport")
	}
}

// TestProductionShapedClientPlaintextUsesFace 走真实的「有 face → uTLS 客户端」
// 这条生产路径。http:// 被 utlsRoundTripper 交给 fallback；fallback 必须是按 face
// 分池的明文传输，否则 chat + shape=claude 仍是 Go 的头序。
func TestProductionShapedClientPlaintextUsesFace(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	c, err := GetHTTPClientUTLSForProxy(nil)
	if err != nil {
		t.Fatalf("GetHTTPClientUTLSForProxy: %v", err)
	}
	req := chatOrderRequest(t, "http://"+ln.Addr().String()+"/v1/chat/completions", tlsFaceClaude)
	lines := roundTripCaptureBlock(t, c.Transport, req, ln)
	assertWireNames(t, lines, claudeChatWireNames)
}

// capturedBlock is one request header block and the server-side connection
// (numbered by accept order, from 1) it arrived on.
type capturedBlock struct {
	conn  int
	lines []string
}

// startKeepAliveCapture serves n requests over keep-alive connections and emits
// each header block in the order the client sent them.
//
// The block is queued BEFORE the response is written. The client cannot send its
// next request until it reads this response, so queue order equals send order
// even when consecutive requests ride different connections (different faces
// always do, and the transport may also dial a fresh conn for the same face).
// Queuing after the write let the next connection's goroutine overtake this one,
// which reordered blocks and failed the face assertions under CI load although
// every block carried its own request's correct face.
//
// afterReply, if non-nil, runs after each response is written; tests use it to
// stall a handler goroutine at exactly that point.
func startKeepAliveCapture(t *testing.T, n int, afterReply func(conn int)) (string, <-chan capturedBlock) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	out := make(chan capturedBlock, n)
	var mu sync.Mutex
	got := 0
	go func() {
		for id := 1; ; id++ {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn, id int) {
				defer conn.Close()
				br := bufio.NewReader(conn)
				for {
					_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
					lines, err := readHTTP1HeaderBlock(br)
					if err != nil {
						return
					}
					discardCapturedBody(br, lines)
					out <- capturedBlock{conn: id, lines: lines}
					if _, err := conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")); err != nil {
						return
					}
					if afterReply != nil {
						afterReply(id)
					}
					mu.Lock()
					got++
					done := got >= n
					mu.Unlock()
					if done {
						return
					}
				}
			}(conn, id)
		}
	}()
	return ln.Addr().String(), out
}

// TestKeepAliveCaptureKeepsSendOrderAcrossConns pins the capture helper itself.
// Two faces force two connections; the first connection's handler stalls right
// after replying. Blocks must still come out in send order. With the block
// queued after the write (the old helper), the second connection's block is
// queued first every time and this test fails.
func TestKeepAliveCaptureKeepsSendOrderAcrossConns(t *testing.T) {
	base, err := clonedDefaultTransport()
	if err != nil {
		t.Fatalf("clonedDefaultTransport: %v", err)
	}
	base.Proxy = nil
	rt := newFaceKeyedPlainTransport(base)

	addr, blocks := startKeepAliveCapture(t, 2, func(conn int) {
		if conn == 1 {
			time.Sleep(200 * time.Millisecond)
		}
	})
	client := &http.Client{Transport: rt, Timeout: 5 * time.Second}
	for _, face := range []string{tlsFaceClaude, tlsFaceCodex} {
		resp, err := client.Do(chatOrderRequest(t, "http://"+addr+"/v1/chat/completions", face))
		if err != nil {
			t.Fatalf("do face=%q: %v", face, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	for i, names := range [][]string{claudeChatWireNames, codexChatWireNames} {
		select {
		case b := <-blocks:
			assertWireNames(t, b.lines, names)
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for request %d", i)
		}
	}
}

func discardCapturedBody(br *bufio.Reader, lines []string) {
	for _, line := range lines {
		name, value, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || n <= 0 {
			return
		}
		_, _ = io.CopyN(io.Discard, br, int64(n))
		return
	}
}
