package relay

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zhizhishu/octopus/internal/telemetry"
)

// 入站客户端指纹（internal/telemetry）只许观察入站，出站（cloak.shape）一个字节都不许变。
//
// 做法：同一组入站请求，分别从「生产接线」（telemetry.Listener + ConnContext + Handler，
// 并在 handler 里调 telemetry.Capture，和 APIKeyAuth 一样）和「完全不接」两条入站服务进来，
// 都走真实的 rawProtocolAttempt 发到同一个假上游；假上游用 telemetry.Listener 记下
// **原始**出站头名顺序 + 大小写、每个头的值、请求体字节。两边必须完全一样。
//
// 同一条路径先跑两遍不接线的，找出本来就每次不同的头（若有），只在比较值时跳过它们；
// 头名顺序和 body 不跳过任何东西。

type outboundShot struct {
	names  []string
	values map[string][]string
	body   string
	path   string
}

type outboundRecorder struct {
	mu    sync.Mutex
	shots []outboundShot
	url   string
}

func newOutboundRecorder(t *testing.T) *outboundRecorder {
	t.Helper()
	rec := &outboundRecorder{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{
		ConnContext: telemetry.ConnContext,
		Handler: telemetry.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			names, ok := telemetry.RawHeaderNames(r)
			if !ok {
				t.Errorf("upstream recorder lost raw header order")
			}
			rec.mu.Lock()
			rec.shots = append(rec.shots, outboundShot{names: names, values: r.Header.Clone(), body: string(b), path: r.URL.RequestURI()})
			rec.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		})),
	}
	go func() { _ = srv.Serve(telemetry.Listener{Listener: ln}) }()
	t.Cleanup(func() { _ = srv.Close() })
	rec.url = "http://" + ln.Addr().String()
	return rec
}

func (r *outboundRecorder) last(t *testing.T) outboundShot {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.shots) == 0 {
		t.Fatal("upstream got nothing")
	}
	return r.shots[len(r.shots)-1]
}

// startInbound 起一个入站服务；wired=true 时与生产同样接线并调用 Capture。
func startInbound(t *testing.T, wired bool, upstreamURL string, options RawProtocolOptions, payload map[string]any) string {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/*path", func(c *gin.Context) {
		if wired {
			s := telemetry.Capture(c.Request, true)
			if s.Hash == "" || !s.OrderKnown {
				t.Errorf("wired inbound must produce a fingerprint with raw order: %#v", s)
			}
		}
		channel := newResponsesChannelForRaw(upstreamURL)
		_, _, _, _, _, err := rawProtocolAttempt(
			context.Background(), options, c, nil,
			false, "", payload,
			false, channel, "sk-test", 0, nil, "gpt-5.6-sol", "", false,
		)
		if err != nil {
			t.Errorf("rawProtocolAttempt: %v", err)
		}
		c.Status(http.StatusOK)
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: engine}
	var l net.Listener = ln
	if wired {
		srv.Handler = telemetry.Handler(engine)
		srv.ConnContext = telemetry.ConnContext
		l = telemetry.Listener{Listener: ln}
	}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + ln.Addr().String()
}

// sendInbound 用原始字节发请求（头顺序、大小写完全由这里决定，两边一致）。
func sendInbound(t *testing.T, base, path, body string) {
	t.Helper()
	addr := strings.TrimPrefix(base, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	raw := "POST " + path + " HTTP/1.1\r\n" +
		"host: " + addr + "\r\n" +
		"user-agent: codex_cli_rs/0.50.0 (Mac OS 15.1.0; arm64) iTerm.app/3.5\r\n" +
		"originator: codex_exec\r\n" +
		"authorization: Bearer sk-octopus-inbound\r\n" +
		"session_id: 0199-session\r\n" +
		"x-stainless-lang: js\r\n" +
		"anthropic-beta: claude-code-20250219\r\n" +
		"X-JA3-Hash: e7d705a3286e19ea42f587b344ee6865\r\n" +
		"X-JA4: t13d1516h2_8daaf6152771_02713d6af862\r\n" +
		"accept: text/event-stream\r\n" +
		"content-type: application/json\r\n" +
		fmt.Sprintf("content-length: %d\r\n", len(body)) +
		"connection: close\r\n\r\n" + body
	if _, err := conn.Write([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(conn)
}

func TestTelemetryDoesNotChangeOutbound(t *testing.T) {
	cases := []struct {
		name    string
		options RawProtocolOptions
		path    string
		payload map[string]any
		body    string
	}{
		// compact = 真 codex leg：带 CLI 身份头 + codex 面容，最敏感的 shape 路径。
		{"codex compact", RawProtocolOptions{Name: "responses_compact", Endpoint: "/responses/compact"}, "/v1/responses/compact",
			map[string]any{"model": "gpt-5.6-sol", "input": []any{}}, `{"model":"gpt-5.6-sol","input":[]}`},
		// 非 compact raw：通用头路径。
		{"raw rerank", RawProtocolOptions{Name: "rerank", Endpoint: "/rerank"}, "/v1/rerank",
			map[string]any{"model": "gpt-5.6-sol", "query": "hi", "documents": []any{"a"}}, `{"model":"gpt-5.6-sol","query":"hi","documents":["a"]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := newOutboundRecorder(t)
			plain := startInbound(t, false, up.url, tc.options, tc.payload)
			wired := startInbound(t, true, up.url, tc.options, tc.payload)

			sendInbound(t, plain, tc.path, tc.body)
			a1 := up.last(t)
			sendInbound(t, plain, tc.path, tc.body)
			a2 := up.last(t)
			sendInbound(t, wired, tc.path, tc.body)
			b := up.last(t)

			if !reflect.DeepEqual(a1.names, b.names) {
				t.Fatalf("outbound header order/case changed:\n off %v\n on  %v", a1.names, b.names)
			}
			if !reflect.DeepEqual(a1.names, a2.names) {
				t.Fatalf("baseline itself not deterministic: %v vs %v", a1.names, a2.names)
			}
			if a1.body != b.body || a1.path != b.path {
				t.Fatalf("outbound body/path changed:\n off %s %q\n on  %s %q", a1.path, a1.body, b.path, b.body)
			}
			volatile := map[string]bool{}
			for k := range a1.values {
				if !reflect.DeepEqual(a1.values[k], a2.values[k]) {
					volatile[k] = true
				}
			}
			keys := make([]string, 0, len(a1.values))
			for k := range a1.values {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				if volatile[k] {
					continue
				}
				if !reflect.DeepEqual(a1.values[k], b.values[k]) {
					t.Fatalf("outbound header %s changed: off %q on %q", k, a1.values[k], b.values[k])
				}
			}
			if len(a1.values) != len(b.values) {
				t.Fatalf("outbound header set changed: off %d on %d", len(a1.values), len(b.values))
			}
			if len(volatile) > 0 {
				t.Logf("headers that differ between two identical plain runs (value not compared): %v", volatile)
			}
		})
	}
}
