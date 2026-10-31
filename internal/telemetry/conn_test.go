package telemetry

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
)

type seen struct {
	names    []string
	known    bool
	bodyHash string
}

// startServer 起一个和生产同样接线的入站服务（Listener + ConnContext + Handler），
// handler 记下每个请求拿到的原始头名和 body 的哈希。
func startServer(t *testing.T) (addr string, got func() []seen) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var out []seen
	srv := &http.Server{
		ConnContext: ConnContext,
		Handler: Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			sum := sha256.Sum256(b)
			names, known := RawHeaderNames(r)
			mu.Lock()
			out = append(out, seen{names: names, known: known, bodyHash: hex.EncodeToString(sum[:8])})
			mu.Unlock()
			w.Header().Set("Content-Length", "2")
			_, _ = w.Write([]byte("ok"))
		})),
	}
	go func() { _ = srv.Serve(Listener{Listener: ln}) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String(), func() []seen {
		mu.Lock()
		defer mu.Unlock()
		return append([]seen(nil), out...)
	}
}

func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

// 同一条长连接上连发：Content-Length body、chunked body（带扩展和 trailer）、无 body；
// 每个请求都要拿到自己的头名（原始顺序、原始大小写），body 原样到达。
func TestRecorderKeepAliveSequence(t *testing.T) {
	addr, got := startServer(t)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	body1 := `{"model":"x","messages":[{"role":"user","content":"hi\r\n\r\nGET / HTTP/1.1"}]}`
	reqs := []string{
		"POST /v1/messages HTTP/1.1\r\nhost: a\r\nX-Stainless-Lang: js\r\nuser-agent: claude-cli/2.0.1\r\ncontent-length: " + fmt.Sprint(len(body1)) + "\r\n\r\n" + body1,
		"POST /v1/responses HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked\r\noriginator: codex_cli_rs\r\n\r\n5;ext=1\r\nhello\r\n6\r\n world\r\n0\r\nX-Trailer: t\r\n\r\n",
		"GET /v1/models HTTP/1.1\r\nHost: a\r\nAccept: */*\r\n\r\n",
	}
	r := bufio.NewReader(conn)
	for _, q := range reqs {
		if _, err := conn.Write([]byte(q)); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(r, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	want := []seen{
		{names: []string{"host", "X-Stainless-Lang", "user-agent", "content-length"}, known: true, bodyHash: hashOf(body1)},
		{names: []string{"Host", "Transfer-Encoding", "originator"}, known: true, bodyHash: hashOf("hello world")},
		{names: []string{"Host", "Accept"}, known: true, bodyHash: hashOf("")},
	}
	if g := got(); !reflect.DeepEqual(g, want) {
		t.Fatalf("got %#v\nwant %#v", g, want)
	}
}

// 流水线（一次写两个请求）也按顺序对上。
func TestRecorderPipelined(t *testing.T) {
	addr, got := startServer(t)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = conn.Write([]byte("POST /a HTTP/1.1\r\nHost: a\r\nContent-Length: 3\r\nB: 1\r\n\r\nabcGET /b HTTP/1.1\r\nHost: a\r\nC: 2\r\n\r\n"))
	r := bufio.NewReader(conn)
	for i := 0; i < 2; i++ {
		resp, err := http.ReadResponse(r, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	g := got()
	if len(g) != 2 || !reflect.DeepEqual(g[0].names, []string{"Host", "Content-Length", "B"}) || !reflect.DeepEqual(g[1].names, []string{"Host", "C"}) {
		t.Fatalf("pipelined: %#v", g)
	}
}

// 看不懂的字节（这里用 TLS ClientHello 开头）：记录器停掉，服务照常回 400，不崩。
func TestRecorderStopsOnGarbage(t *testing.T) {
	rec := newRecorder()
	rec.feed([]byte{0x16, 0x03, 0x01, 0x02, 0x00, '\n'})
	if rec.state != stStopped {
		t.Fatalf("state %v", rec.state)
	}
	if _, ok := rec.next("GET", "/"); ok {
		t.Fatal("stopped recorder must not return names")
	}
	rec = newRecorder()
	rec.feed([]byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"))
	if rec.state != stStopped {
		t.Fatal("h2 preface must stop")
	}
}

// Upgrade（WebSocket）之后是帧，不再解析；之前那个请求照样拿到头名。
func TestRecorderStopsAfterUpgrade(t *testing.T) {
	rec := newRecorder()
	rec.feed([]byte("GET /v1/realtime HTTP/1.1\r\nHost: a\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n\x81\x05hello"))
	names, ok := rec.next("GET", "/v1/realtime")
	if !ok || !reflect.DeepEqual(names, []string{"Host", "Connection", "Upgrade"}) {
		t.Fatalf("names %v %v", names, ok)
	}
	if rec.state != stStopped {
		t.Fatal("must stop after upgrade")
	}
}

// 逐字节喂（最坏的分包）结果一样；对不上的请求让记录器停掉而不是张冠李戴。
func TestRecorderByteByByteAndMismatch(t *testing.T) {
	rec := newRecorder()
	for _, b := range []byte("POST /x HTTP/1.1\r\nHost: a\r\nContent-Length: 2\r\n\r\nhiGET /y HTTP/1.1\r\nHost: b\r\n\r\n") {
		rec.feed([]byte{b})
	}
	if n, ok := rec.next("POST", "/x"); !ok || len(n) != 2 {
		t.Fatalf("first %v %v", n, ok)
	}
	if _, ok := rec.next("GET", "/other"); ok {
		t.Fatal("mismatch must not return names")
	}
	if rec.state != stStopped {
		t.Fatal("mismatch must stop recording")
	}
}

// 没走 Listener 的请求（例如测试里直接调 handler）：RawHeaderNames 返回 false，不 panic。
func TestRawHeaderNamesWithoutRecorder(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "http://a/", nil)
	if _, ok := RawHeaderNames(req); ok {
		t.Fatal("no recorder → not known")
	}
	if _, ok := RawHeaderNames(nil); ok {
		t.Fatal("nil")
	}
}

// 超长头行不会让内存无限涨：单行只留 maxLineBytes。
func TestRecorderLineCap(t *testing.T) {
	rec := newRecorder()
	rec.feed([]byte("GET / HTTP/1.1\r\nX-Big: " + strings.Repeat("a", 100_000)))
	if len(rec.line) > maxLineBytes {
		t.Fatalf("line buffer %d", len(rec.line))
	}
	rec.feed([]byte("\r\n\r\n"))
	if n, ok := rec.next("GET", "/"); !ok || !reflect.DeepEqual(n, []string{"X-Big"}) {
		t.Fatalf("%v %v", n, ok)
	}
}

