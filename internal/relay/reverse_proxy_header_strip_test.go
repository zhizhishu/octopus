package relay

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/telemetry"
	"github.com/zhizhishu/octopus/internal/transformer/inbound"
	transformermodel "github.com/zhizhishu/octopus/internal/transformer/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// 反代给入站请求加的头（转交的 JA3 / JA4 / HTTP 版本 / 国家、Via）只给本服务的指纹看，
// 真 Claude / Codex CLI 从来不发，所以出站一律剥掉。除了这些头，其它出站内容
// （头名、顺序、大小写、值、请求体）必须和不带这些头时完全一样。

// proxyHeaderLines 是反代可能加的全部头，值用真实形态。
var proxyHeaderLines = [][2]string{
	{"X-JA3", "771,4865-4866-4867-49195,0-23-65281-10-11,29-23-24,0"},
	{"X-JA3-Hash", "e7d705a3286e19ea42f587b344ee6865"},
	{"CF-JA3-Hash", "e7d705a3286e19ea42f587b344ee6865"},
	{"X-TLS-JA3", "e7d705a3286e19ea42f587b344ee6865"},
	{"X-JA4", "t13d1516h2_8daaf6152771_02713d6af862"},
	{"CF-JA4", "t13d1516h2_8daaf6152771_02713d6af862"},
	{"X-TLS-JA4", "t13d1516h2_8daaf6152771_02713d6af862"},
	{"CF-IPCountry", "US"},
	{"X-HTTP-Version", "HTTP/2.0"},
	{"X-Client-HTTP-Version", "h2"},
	{"Via", "2.0 caddy"},
}

func TestReverseProxyHeaderListCoversTelemetryReads(t *testing.T) {
	// 用户点名的这些必须在名单里；名单 = 指纹代码实际读的那些（同一个来源）。
	want := []string{"X-JA3", "X-JA3-Hash", "CF-JA3-Hash", "X-JA4", "CF-JA4", "CF-IPCountry", "X-HTTP-Version"}
	got := map[string]bool{}
	for _, h := range telemetry.ReverseProxyHeaders() {
		got[strings.ToLower(h)] = true
	}
	for _, h := range want {
		if !got[strings.ToLower(h)] {
			t.Fatalf("telemetry.ReverseProxyHeaders() missing %s: %v", h, telemetry.ReverseProxyHeaders())
		}
	}
	for _, p := range proxyHeaderLines {
		if !got[strings.ToLower(p[0])] {
			t.Fatalf("test list has %s but telemetry list does not", p[0])
		}
	}
	if len(got) != len(proxyHeaderLines) {
		t.Fatalf("test list (%d) and telemetry list (%d) differ: %v", len(proxyHeaderLines), len(got), telemetry.ReverseProxyHeaders())
	}
	for h := range got {
		for _, name := range []string{h, strings.ToUpper(h), http.CanonicalHeaderKey(h), " " + h + " "} {
			if shouldForwardClientHeader(name) {
				t.Fatalf("%q must not be forwarded upstream", name)
			}
			for _, cli := range []bool{false, true} {
				if shouldForwardClientHeaderForCaller(name, cli) {
					t.Fatalf("%q must not be forwarded upstream (cliShaped=%v)", name, cli)
				}
			}
		}
	}
	// 不在名单里的自定义头照常转发（只剥反代头，不是剥所有 X-*）。
	for _, h := range []string{"X-Region", "X-Custom-Business", "Anthropic-Beta", "Originator"} {
		if !shouldForwardClientHeader(h) {
			t.Fatalf("%s should still be forwarded", h)
		}
	}
}

// 入站头：一组 claude-cli 形态、一组 codex 形态、一组普通客户端。
var inboundHeaderSets = map[string][][2]string{
	"claude-cli": {
		{"User-Agent", "claude-cli/2.0.14 (external, cli)"},
		{"Anthropic-Beta", "claude-code-20250219,oauth-2025-04-20"},
		{"Anthropic-Version", "2023-06-01"},
		{"X-App", "cli"},
		{"X-Stainless-Lang", "js"},
		{"X-Stainless-Timeout", "111"},
		{"Accept", "application/json"},
		{"Content-Type", "application/json"},
		{"X-Region", "eu"},
	},
	"codex": {
		{"User-Agent", "codex_cli_rs/0.50.0 (Mac OS 15.1.0; arm64) iTerm.app/3.5"},
		{"Originator", "codex_exec"},
		{"Openai-Beta", "responses=experimental"},
		{"Accept", "text/event-stream"},
		{"Content-Type", "application/json"},
		{"X-Region", "eu"},
	},
	"plain": {
		{"User-Agent", "python-httpx/0.27"},
		{"Accept", "*/*"},
		{"Content-Type", "application/json"},
		{"X-Region", "eu"},
	},
}

func newInboundCtx(path string, base [][2]string, withProxy bool) *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, path, nil)
	for i, h := range base {
		c.Request.Header.Add(h[0], h[1])
		if withProxy && i == 1 {
			for _, p := range proxyHeaderLines {
				c.Request.Header.Add(p[0], p[1])
			}
		}
	}
	return c
}

func assertNoProxyHeaders(t *testing.T, h http.Header) {
	t.Helper()
	for _, p := range proxyHeaderLines {
		if v := h.Values(p[0]); len(v) > 0 {
			t.Fatalf("reverse-proxy header %s reached upstream: %q", p[0], v)
		}
	}
}

func TestCopyHeadersToUpstreamStripsReverseProxyHeadersOnly(t *testing.T) {
	for name, base := range inboundHeaderSets {
		t.Run(name, func(t *testing.T) {
			build := func(withProxy bool) http.Header {
				c := newInboundCtx("/v1/images/generations", base, withProxy)
				req := httptest.NewRequest(http.MethodPost, "https://upstream.example/v1/images/generations", nil)
				copyHeadersToUpstream(req, c, &dbmodel.Channel{}, "upstream-key", "application/json", false)
				return req.Header
			}
			without, with := build(false), build(true)
			assertNoProxyHeaders(t, with)
			if !reflect.DeepEqual(without, with) {
				t.Fatalf("outbound headers differ beyond the proxy-header strip:\n without %v\n with    %v", without, with)
			}
		})
	}
}

func TestRelayCopyHeadersStripsReverseProxyHeadersOnly(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		in      inbound.InboundType
		channel *dbmodel.Channel
		model   string
		base    string
	}{
		{"claude shape", "/v1/messages", inbound.InboundTypeAnthropic, &dbmodel.Channel{Type: outbound.OutboundTypeAnthropic}, "claude-opus-4-8", "claude-cli"},
		{"claude shape plain caller", "/v1/messages", inbound.InboundTypeAnthropic, &dbmodel.Channel{Type: outbound.OutboundTypeAnthropic}, "claude-opus-4-8", "plain"},
		{"codex shape", "/v1/responses", inbound.InboundTypeOpenAIResponse, &dbmodel.Channel{Type: outbound.OutboundTypeOpenAIResponse}, "gpt-5.6-sol", "codex"},
		{"generic chat", "/v1/chat/completions", inbound.InboundTypeOpenAIChat, &dbmodel.Channel{Type: outbound.OutboundTypeOpenAIChat}, "gpt-4o", "plain"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			build := func(withProxy bool) http.Header {
				c := newInboundCtx(tc.path, inboundHeaderSets[tc.base], withProxy)
				req := httptest.NewRequest(http.MethodPost, "https://upstream.example"+tc.path, nil)
				ra := &relayAttempt{
					relayRequest: &relayRequest{c: c, inboundType: tc.in, internalRequest: &transformermodel.InternalLLMRequest{Model: tc.model}},
					channel:      tc.channel,
				}
				ra.copyHeaders(req)
				return req.Header
			}
			a1, a2, b := build(false), build(false), build(true)
			assertNoProxyHeaders(t, b)
			keys := func(h http.Header) []string {
				out := make([]string, 0, len(h))
				for k := range h {
					out = append(out, k)
				}
				sort.Strings(out)
				return out
			}
			if !reflect.DeepEqual(keys(a1), keys(b)) {
				t.Fatalf("outbound header names differ:\n without %v\n with    %v", keys(a1), keys(b))
			}
			for k := range a1 {
				if !reflect.DeepEqual(a1[k], a2[k]) {
					continue // 本来就每次不同（若有）
				}
				if !reflect.DeepEqual(a1[k], b[k]) {
					t.Fatalf("outbound header %s changed: without %q with %q", k, a1[k], b[k])
				}
			}
		})
	}
}

// 线上字节级：真实 rawProtocolAttempt 发到假上游，假上游记原始头名顺序 + 大小写、值、body。
// 带反代头与不带反代头两次，上游看到的必须完全一样，且一个反代头都没有。
func sendRawInboundWithProxy(t *testing.T, base, path, body string, withProxy bool) {
	t.Helper()
	addr := strings.TrimPrefix(base, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	proxy := ""
	if withProxy {
		for _, p := range proxyHeaderLines {
			proxy += p[0] + ": " + p[1] + "\r\n"
		}
	}
	raw := "POST " + path + " HTTP/1.1\r\n" +
		"host: " + addr + "\r\n" +
		"user-agent: codex_cli_rs/0.50.0 (Mac OS 15.1.0; arm64) iTerm.app/3.5\r\n" +
		"originator: codex_exec\r\n" +
		proxy +
		"authorization: Bearer sk-octopus-inbound\r\n" +
		"session_id: 0199-session\r\n" +
		"x-region: eu\r\n" +
		"accept: text/event-stream\r\n" +
		"content-type: application/json\r\n" +
		fmt.Sprintf("content-length: %d\r\n", len(body)) +
		"connection: close\r\n\r\n" + body
	if _, err := conn.Write([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(conn)
}

func TestReverseProxyHeadersNeverReachUpstreamOnTheWire(t *testing.T) {
	cases := []struct {
		name    string
		options RawProtocolOptions
		path    string
		payload map[string]any
		body    string
	}{
		{"codex compact", RawProtocolOptions{Name: "responses_compact", Endpoint: "/responses/compact"}, "/v1/responses/compact",
			map[string]any{"model": "gpt-5.6-sol", "input": []any{}}, `{"model":"gpt-5.6-sol","input":[]}`},
		{"raw rerank", RawProtocolOptions{Name: "rerank", Endpoint: "/rerank"}, "/v1/rerank",
			map[string]any{"model": "gpt-5.6-sol", "query": "hi", "documents": []any{"a"}}, `{"model":"gpt-5.6-sol","query":"hi","documents":["a"]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := newOutboundRecorder(t)
			gin.SetMode(gin.TestMode)
			engine := gin.New()
			engine.POST("/*path", func(c *gin.Context) {
				_, _, _, _, _, err := rawProtocolAttempt(context.Background(), tc.options, c, nil,
					false, "", tc.payload, false, newResponsesChannelForRaw(up.url), "sk-test", 0, nil, "gpt-5.6-sol", "", false)
				if err != nil {
					t.Errorf("rawProtocolAttempt: %v", err)
				}
				c.Status(http.StatusOK)
			})
			srv := httptest.NewServer(engine)
			t.Cleanup(srv.Close)

			sendRawInboundWithProxy(t, srv.URL, tc.path, tc.body, false)
			a1 := up.last(t)
			sendRawInboundWithProxy(t, srv.URL, tc.path, tc.body, false)
			a2 := up.last(t)
			sendRawInboundWithProxy(t, srv.URL, tc.path, tc.body, true)
			b := up.last(t)

			for _, n := range b.names {
				for _, p := range proxyHeaderLines {
					if strings.EqualFold(n, p[0]) {
						t.Fatalf("reverse-proxy header %s reached upstream; raw names %v", n, b.names)
					}
				}
			}
			assertNoProxyHeaders(t, http.Header(b.values))
			if !reflect.DeepEqual(a1.names, a2.names) {
				t.Fatalf("baseline not deterministic: %v vs %v", a1.names, a2.names)
			}
			if !reflect.DeepEqual(a1.names, b.names) {
				t.Fatalf("outbound raw header order/case changed:\n without %v\n with    %v", a1.names, b.names)
			}
			if a1.body != b.body || a1.path != b.path {
				t.Fatalf("outbound body/path changed:\n without %s %q\n with    %s %q", a1.path, a1.body, b.path, b.body)
			}
			if len(a1.values) != len(b.values) {
				t.Fatalf("outbound header set changed: without %d with %d", len(a1.values), len(b.values))
			}
			for k, v := range a1.values {
				if !reflect.DeepEqual(v, a2.values[k]) {
					continue
				}
				if !reflect.DeepEqual(v, b.values[k]) {
					t.Fatalf("outbound header %s changed: without %q with %q", k, v, b.values[k])
				}
			}
			t.Logf("upstream raw header order (identical with/without proxy headers): %v", b.names)
		})
	}
}
