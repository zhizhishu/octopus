package modeltest

// 指纹探针的出站 shape 契约。
//
// 用户硬规则：上游只能看到渠道 shape 选定的真 Claude CLI / Codex CLI 流量。检测探针
// 必须和真实流量走同一条出站链路——同样的请求行、同样的头（名字、大小写、顺序、取值）、
// 同样的请求体约定——上游无从分辨这是检测。
//
// 这里把"渠道检测按钮"（Run，本仓库其它测试已守住它与 relay 真实转发同形）当基准，
// 在**原始字节**层面（自建 TCP 服务端，逐行读请求头，不经 net/http 的 map）比对
// 指纹探针发出的每一个请求：
//   - 请求行、头名序列（含大小写）完全一致；
//   - 头取值一致，只放过 Content-Length 与逐请求变化的会话标识；
//   - 请求体 JSON 的键路径与顺序完全一致，取值只允许题目文字不同；
//   - 绝不出现 lm-detector 式的附加参数（reasoning_effort / reasoning / service_tier / speed）。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http/httputil"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/modelverify/behavior"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

type rawRequest struct {
	requestLine string
	headers     []string // "Name: value"，按线上顺序、保留大小写
	body        []byte
}

func (r rawRequest) headerNames() []string {
	out := make([]string, len(r.headers))
	for i, h := range r.headers {
		out[i] = strings.SplitN(h, ":", 2)[0]
	}
	return out
}

// rawCaptureServer 是一个只说 HTTP/1.1 的极简上游：原样记下请求字节，再按协议回一段流。
type rawCaptureServer struct {
	ln       net.Listener
	mu       sync.Mutex
	requests []rawRequest
	reply    func(index int) string // 返回完整 HTTP 响应
}

func newRawCaptureServer(t *testing.T, reply func(int) string) *rawCaptureServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &rawCaptureServer{ln: ln, reply: reply}
	go s.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

func (s *rawCaptureServer) url() string { return "http://" + s.ln.Addr().String() }

func (s *rawCaptureServer) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *rawCaptureServer) handle(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		req := rawRequest{requestLine: strings.TrimRight(line, "\r\n")}
		length, chunked := -1, false
		for {
			h, err := br.ReadString('\n')
			if err != nil {
				return
			}
			h = strings.TrimRight(h, "\r\n")
			if h == "" {
				break
			}
			req.headers = append(req.headers, h)
			name, value, _ := strings.Cut(h, ":")
			value = strings.TrimSpace(value)
			switch strings.ToLower(name) {
			case "content-length":
				length, _ = strconv.Atoi(value)
			case "transfer-encoding":
				chunked = strings.EqualFold(value, "chunked")
			}
		}
		var body []byte
		if chunked {
			body, err = io.ReadAll(httputil.NewChunkedReader(br))
			if err == nil {
				_, _ = br.ReadString('\n') // 末尾的空行
			}
		} else if length > 0 {
			body = make([]byte, length)
			_, err = io.ReadFull(br, body)
		}
		if err != nil {
			return
		}
		req.body = body
		s.mu.Lock()
		index := len(s.requests)
		s.requests = append(s.requests, req)
		s.mu.Unlock()
		_, _ = io.WriteString(conn, s.reply(index))
		return // Connection: close，每个请求一条连接
	}
}

func (s *rawCaptureServer) captured() []rawRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]rawRequest(nil), s.requests...)
}

func fpSSEResponse(body string) string {
	return "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nConnection: close\r\n\r\n" + body
}

func fpAnthropicSSE(text string) string {
	quoted, _ := json.Marshal(text)
	return fpSSEResponse("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-opus-4-6\",\"content\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":" + string(quoted) + "}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":10}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
}

func fpResponsesSSE(text string) string {
	quoted, _ := json.Marshal(text)
	return fpSSEResponse("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":123,\"model\":\"gpt-5.5\",\"status\":\"in_progress\",\"output\":[]}}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":" + string(quoted) + "}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":123,\"model\":\"gpt-5.5\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":2,\"input_tokens_details\":{\"cached_tokens\":0},\"output_tokens\":1,\"output_tokens_details\":{\"reasoning_tokens\":0},\"total_tokens\":3}}}\n\n" +
		"data: [DONE]\n\n")
}

func fpChatSSE(text string) string {
	quoted, _ := json.Marshal(text)
	return fpSSEResponse("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":" + string(quoted) + "},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n")
}

// goldenModelReply 取一条真模型回答，让指纹探针三道题都拿到可用回答、完整跑完。
func goldenModelReply(t *testing.T) string {
	t.Helper()
	blob, err := os.ReadFile("../modelverify/fingerprint/testdata/g_full.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden []struct {
		Texts []string `json:"texts"`
	}
	if err := json.Unmarshal(blob, &golden); err != nil || len(golden) == 0 {
		t.Fatalf("黄金数据不可用: %v", err)
	}
	return golden[0].Texts[0]
}

// jsonSkeleton 把请求体展开成按出现顺序排列的 "路径=值" 列表。
// 对象键顺序原样保留（逐 token 读，不经 map），这样键序变化也会被抓到。
func jsonSkeleton(t *testing.T, body []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var out []string
	var walk func(path string) error
	walk = func(path string) error {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		switch v := tok.(type) {
		case json.Delim:
			switch v {
			case '{':
				for dec.More() {
					keyTok, err := dec.Token()
					if err != nil {
						return err
					}
					if err := walk(path + "." + keyTok.(string)); err != nil {
						return err
					}
				}
				_, err = dec.Token()
				return err
			case '[':
				i := 0
				for dec.More() {
					if err := walk(fmt.Sprintf("%s[%d]", path, i)); err != nil {
						return err
					}
					i++
				}
				_, err = dec.Token()
				return err
			}
		default:
			out = append(out, fmt.Sprintf("%s=%v", path, v))
		}
		return nil
	}
	if err := walk("$"); err != nil {
		t.Fatalf("请求体不是合法 JSON: %v\n%s", err, body)
	}
	return out
}

type shapeCase struct {
	name     string
	channel  dbmodel.Channel
	model    string
	endpoint string
	reply    func(string) string
}

// volatileHeader：逐请求天然变化、真实 CLI 本身每次也不同的头（会话/请求标识、长度）。
// 名字与位置仍然必须一致，只是不比值。
func volatileHeader(name string) bool {
	switch strings.ToLower(name) {
	case "content-length", "x-client-request-id", "session_id", "session-id", "x-claude-code-session-id", "x-request-id",
		"x-codex-window-id", "thread-id": // Codex CLI 每个会话一个窗口 id，真实 CLI 每开一次也不同

		return true
	}
	return false
}

func TestFingerprintProbeOutboundMatchesChannelShape(t *testing.T) {
	cases := []shapeCase{
		{name: "anthropic-claude-cli", model: "claude-opus-4-6", endpoint: "anthropic_messages", reply: fpAnthropicSSE,
			channel: dbmodel.Channel{Type: outbound.OutboundTypeAnthropic}},
		{name: "responses-codex-cli", model: "gpt-5.5", endpoint: "openai_responses", reply: fpResponsesSSE,
			channel: dbmodel.Channel{Type: outbound.OutboundTypeOpenAIResponse}},
		{name: "chat-shape-claude", model: "claude-opus-4-6", endpoint: "openai_chat", reply: fpChatSSE,
			channel: dbmodel.Channel{Type: outbound.OutboundTypeOpenAIChat, Cloak: dbmodel.ChannelCloak{Shape: "claude"}}},
		{name: "chat-shape-codex", model: "gpt-5.5", endpoint: "openai_chat", reply: fpChatSSE,
			channel: dbmodel.Channel{Type: outbound.OutboundTypeOpenAIChat, Cloak: dbmodel.ChannelCloak{Shape: "codex"}}},
	}
	numbers := goldenModelReply(t)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := setupModelTestDB(t)
			srv := newRawCaptureServer(t, func(int) string { return tc.reply(numbers) })

			channel := tc.channel
			channel.Name = "shape-" + tc.name
			channel.Model = tc.model
			channel.BaseUrls = []dbmodel.BaseUrl{{URL: srv.url()}}
			channel.Keys = []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "sk-shape-test"}}
			if err := op.ChannelCreate(&channel, ctx); err != nil {
				t.Fatalf("create channel: %v", err)
			}
			stored, err := op.ChannelGet(channel.ID, ctx)
			if err != nil {
				t.Fatalf("get channel: %v", err)
			}

			// 基准：渠道检测按钮发出的请求。
			baseline, err := Run(ctx, dbmodel.ModelTestRequest{Model: tc.model, ChannelID: channel.ID, Endpoint: tc.endpoint})
			if err != nil || baseline.Summary.Success != 1 {
				t.Fatalf("baseline channel test: %v %+v", err, baseline)
			}
			// 指纹探针：默认随机出题，三道题。
			sender, err := NewProbeSender(*stored, tc.model, tc.endpoint)
			if err != nil {
				t.Fatalf("probe sender: %v", err)
			}
			// 随机出题（默认）与固定 36 题对照各跑一轮，共 6 个探针请求。
			for _, mode := range []string{behavior.FingerprintPromptRandom, behavior.FingerprintPromptFixed} {
				results := behavior.Run(behavior.WithFingerprintPromptMode(context.Background(), mode),
					[]behavior.ProbeID{behavior.ProbeFingerprint}, sender, tc.model)
				if len(results) != 1 || results[0].Err != nil {
					t.Fatalf("fingerprint probe (%s) failed: %+v", mode, results)
				}
			}

			got := srv.captured()
			if len(got) != 7 {
				t.Fatalf("应捕获 1 个基准 + 6 个探针请求，实际 %d", len(got))
			}
			t.Logf("%s 基准 %s\n  头序=%v", tc.name, got[0].requestLine, got[0].headerNames())
			base := got[0]
			baseSkeleton := jsonSkeleton(t, base.body)
			for i, probe := range got[1:] {
				label := fmt.Sprintf("%s 探针第 %d 个请求", tc.name, i+1)
				if probe.requestLine != base.requestLine {
					t.Fatalf("%s 请求行不同:\n  probe=%q\n  base =%q", label, probe.requestLine, base.requestLine)
				}
				if strings.Join(probe.headerNames(), "|") != strings.Join(base.headerNames(), "|") {
					t.Fatalf("%s 头名或头序不同:\n  probe=%v\n  base =%v", label, probe.headerNames(), base.headerNames())
				}
				for j, h := range probe.headers {
					name := strings.SplitN(h, ":", 2)[0]
					if strings.EqualFold(name, "x-codex-turn-metadata") {
						assertTurnMetadataSameShape(t, label, h, base.headers[j])
						continue
					}
					if !volatileHeader(name) && h != base.headers[j] {
						t.Fatalf("%s 头取值不同:\n  probe=%q\n  base =%q", label, h, base.headers[j])
					}
				}
				skeleton := jsonSkeleton(t, probe.body)
				diffs := skeletonDiff(baseSkeleton, skeleton)
				for _, d := range diffs {
					if d.path == "$.client_metadata.x-codex-turn-metadata" {
						// 与同名请求头同一份 JSON，同样只放过会话/轮次标识。
						_, pv, _ := strings.Cut(d.probe, "=")
						_, bv, _ := strings.Cut(d.base, "=")
						assertTurnMetadataSameShape(t, label, "m:"+pv, "m:"+bv)
						continue
					}
					if !allowedBodyDiff(d, tc.endpoint) {
						t.Fatalf("%s 请求体与渠道检测不同形（%s）:\n  probe=%s\n  base =%s", label, d.path, d.probe, d.base)
					}
				}
				if !hasPrefixLine(skeleton, "$.stream=true") {
					t.Fatalf("%s 探针必须和 CLI 一样走流式: %v", label, skeleton)
				}
				if tc.endpoint == "anthropic_messages" && !hasPrefixLine(skeleton, "$.max_tokens=64000") {
					t.Fatalf("%s Anthropic 链路必须保留 CLI 的 max_tokens=64000", label)
				}
				// 冗余保险：lm-detector 式的附加参数（推理强度注入等）一个都不许出现在基准没有的地方。
				for _, banned := range []string{"$.reasoning_effort", "$.reasoning.effort", "$.service_tier", "$.speed"} {
					if hasPrefixLine(skeleton, banned) && !hasPrefixLine(baseSkeleton, banned) {
						t.Fatalf("%s 出现了渠道 shape 没有的字段 %s", label, banned)
					}
				}
				var differing []string
				for _, d := range diffs {
					differing = append(differing, d.path)
				}
				t.Logf("%s：请求行、头名与头序、头取值、请求体键路径与键序一致；取值不同的只有 %v", label, differing)
			}
		})
	}
}

func hasPrefixLine(lines []string, prefix string) bool {
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	return false
}

type skeletonDelta struct{ path, probe, base string }

// skeletonDiff 逐行比对；路径序列必须完全一致，取值不同的行报出来交给白名单裁决。
func skeletonDiff(base, probe []string) []skeletonDelta {
	var out []skeletonDelta
	n := len(base)
	if len(probe) > n {
		n = len(probe)
	}
	for i := 0; i < n; i++ {
		var b, p string
		if i < len(base) {
			b = base[i]
		}
		if i < len(probe) {
			p = probe[i]
		}
		if b == p {
			continue
		}
		bp, _, _ := strings.Cut(b, "=")
		pp, _, _ := strings.Cut(p, "=")
		if bp != pp {
			return append(out, skeletonDelta{path: "键路径/顺序: " + pp + " vs " + bp, probe: p, base: b})
		}
		out = append(out, skeletonDelta{path: pp, probe: p, base: b})
	}
	return out
}

// allowedBodyDiff：只有"题目文字"本身可以不同。
//
// 唯一例外：Chat 渠道（openai_chat 出站）的 max_tokens 取值。这一路的 max_tokens 本来就是
// 客户端自己定的（真实转发原样透传调用方的数），渠道检测用 256、所有检测探针（不止指纹）
// 一直用 probeMaxTokens；键必须在同一位置出现，只是数值不比。Anthropic 原生链路钉死 CLI
// 的 64000、Responses 链路根本不带它，这两条都不放过。
func allowedBodyDiff(d skeletonDelta, endpoint string) bool {
	if strings.HasSuffix(d.path, ".text") || strings.HasSuffix(d.path, ".content") {
		return true
	}
	// Codex CLI 的 prompt_cache_key 就是会话 id，真 CLI 每开一个会话也换一个。
	// client_metadata 里的窗口 id 与请求头 x-codex-window-id 同源，同理。
	if endpoint == "openai_responses" && (d.path == "$.prompt_cache_key" || d.path == "$.client_metadata.x-codex-window-id") {
		return true
	}
	return endpoint == "openai_chat" && d.path == "$.max_tokens"
}

// assertTurnMetadataSameShape：Codex CLI 的 x-codex-turn-metadata 头是 JSON，里面的会话/轮次
// 标识与时间戳每轮都变（真 CLI 也一样），其它键（安装 id、请求种类、沙箱……）及键序必须一致。
func assertTurnMetadataSameShape(t *testing.T, label, probe, base string) {
	t.Helper()
	_, pv, _ := strings.Cut(probe, ":")
	_, bv, _ := strings.Cut(base, ":")
	volatile := map[string]bool{"session_id": true, "thread_id": true, "turn_id": true, "window_id": true, "turn_started_at_unix_ms": true}
	ps := jsonSkeleton(t, []byte(strings.TrimSpace(pv)))
	bs := jsonSkeleton(t, []byte(strings.TrimSpace(bv)))
	if len(ps) != len(bs) {
		t.Fatalf("%s x-codex-turn-metadata 键数不同:\n  probe=%v\n  base =%v", label, ps, bs)
	}
	for i := range ps {
		pp, _, _ := strings.Cut(ps[i], "=")
		bp, _, _ := strings.Cut(bs[i], "=")
		if pp != bp {
			t.Fatalf("%s x-codex-turn-metadata 键序不同: %s vs %s", label, pp, bp)
		}
		if !volatile[strings.TrimPrefix(pp, "$.")] && ps[i] != bs[i] {
			t.Fatalf("%s x-codex-turn-metadata 取值不同: %s vs %s", label, ps[i], bs[i])
		}
	}
}
