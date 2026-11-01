package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/relay/balancer"
	"github.com/zhizhishu/octopus/internal/telemetry"
	"github.com/zhizhishu/octopus/internal/transformer/inbound"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
	"github.com/zhizhishu/octopus/internal/utils/log"
)

// 日志模式 = 调试 只许「多看」，不许「多做」：发往上游的每一个字节（头名顺序与大小写、
// 头值、路径、body）在调试开 / 关两种情况下必须完全一样。
//
// 做法与 telemetry_outbound_unchanged_test.go 相同：假上游用 telemetry.Listener 记下
// **原始**出站头名顺序 + 大小写。同一组请求先在「普通」下跑两遍（找出本来就每次不同的
// 头，只在比较值时跳过；头序和 body 不跳过任何东西），再在「调试」下跑一遍逐项比对。
// 覆盖：OpenAI chat、Anthropic messages、聊天换家链（503 → 第二家），以及其余几条
// 自己发请求的路径：图片（含换家）、聊天里的图片生成、原样转发（rerank）、视频创建、
// Midjourney 提交、异步图片任务的执行器。

const (
	debugTestChannelKey = "sk-debug-chan-secret-0123456789abcdef"
	debugTestInboundKey = "sk-octopus-inbound-secret-abcdef0123456789"
	debugTestCookie     = "session=cookie-secret-value-42"
	debugTestErrSecret  = "sk-live-upstream-echo-secret-9876543210"
)

type debugShot struct {
	names  []string
	values map[string][]string
	body   string
	path   string
}

type debugUpstream struct {
	mu    sync.Mutex
	shots []debugShot
	url   string
}

// startDebugUpstream 起一个记原始头序的假上游；fail=true 时一律回 503 + 带密钥的错误原文。
func startDebugUpstream(t *testing.T, fail bool) *debugUpstream {
	t.Helper()
	up := &debugUpstream{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{
		ConnContext: telemetry.ConnContext,
		Handler: telemetry.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			names, _ := telemetry.RawHeaderNames(r)
			up.mu.Lock()
			up.shots = append(up.shots, debugShot{names: names, values: r.Header.Clone(), body: string(b), path: r.URL.RequestURI()})
			up.mu.Unlock()
			if fail {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Set-Cookie", "upstream-session=should-not-be-stored")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"error":{"message":"overloaded, key ` + debugTestErrSecret + ` busy","type":"overloaded_error"}}`))
				return
			}
			if strings.HasPrefix(r.URL.Path, "/v1/messages") {
				if strings.Contains(string(b), `"stream":true`) {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-debug\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n"))
					_, _ = w.Write([]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"))
					_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n"))
					_, _ = w.Write([]byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"))
					_, _ = w.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n"))
					_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-debug","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			switch {
			case strings.HasPrefix(r.URL.Path, "/v1/images/"):
				// 内联 b64（不给 URL）：聊天→图片桥收到 URL 会去下载，测试里不许出网。
				_, _ = w.Write([]byte(`{"created":1,"data":[{"b64_json":"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="}]}`))
				return
			case strings.HasPrefix(r.URL.Path, "/v1/videos"):
				_, _ = w.Write([]byte(`{"id":"vid_debug_1","object":"video","status":"queued"}`))
				return
			case strings.HasPrefix(r.URL.Path, "/mj/"):
				_, _ = w.Write([]byte(`{"code":1,"description":"submitted","result":"1730000000000001"}`))
				return
			case strings.HasPrefix(r.URL.Path, "/v1/rerank"):
				_, _ = w.Write([]byte(`{"id":"rr-1","results":[{"index":0,"relevance_score":0.9}],"usage":{"total_tokens":3}}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-debug","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
		})),
	}
	go func() { _ = srv.Serve(telemetry.Listener{Listener: ln}) }()
	t.Cleanup(func() { _ = srv.Close() })
	up.url = "http://" + ln.Addr().String()
	return up
}

func (u *debugUpstream) all() []debugShot {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]debugShot(nil), u.shots...)
}

type debugScenario struct {
	name        string
	inbound     inbound.InboundType
	channelType outbound.OutboundType
	model       string
	path        string
	body        string
	failFirst   bool
	// handler 为空时走聊天主入口 Handler(inbound, c)。
	handler func(c *gin.Context)
	// asyncImage：不走 HTTP 入口，直接按执行器的顺序跑 NewImageTaskLog → ImageTaskRunUpstream → Finish。
	asyncImage bool
	// noRoutes：这条路径本身不选路（异步任务执行器直接用提交时定下的渠道）。
	noRoutes bool
	// secretHeaders：入站额外带的凭据头（名 → 值），调试详情里必须是「[已隐藏]」。
	secretHeaders map[string]string
	// upstreamPath：上游必须收到的路径前缀（证明真的走了这条路径，而不是别的入口）。
	upstreamPath string
}

func setLogModeForTest(t *testing.T, mode string) {
	t.Helper()
	if err := op.SettingSetString(dbmodel.SettingKeyLogMode, mode); err != nil {
		t.Fatalf("set log mode: %v", err)
	}
	op.LogModeOnSettingChanged(dbmodel.SettingKeyLogMode)
}

// runDebugScenario 在一个全新的库里跑一次请求，返回上游收到的全部调用与这条日志的完整详情。
func runDebugScenario(t *testing.T, sc debugScenario, mode string) ([]debugShot, *dbmodel.RelayLog) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	prevLevel := log.GetLevel()
	t.Cleanup(func() { log.SetLevel(prevLevel) })
	ctx := setupRelayErrorDB(t)
	_ = op.RelayLogClear(context.Background(), nil)
	setLogModeForTest(t, mode)

	ok := startDebugUpstream(t, false)
	ups := []*debugUpstream{}
	mkChannel := func(name, url string, priority int) {
		ch := dbmodel.Channel{
			Name:     name,
			Type:     sc.channelType,
			Enabled:  true,
			Model:    sc.model,
			Priority: priority,
			BaseUrls: []dbmodel.BaseUrl{{URL: url}},
			Keys:     []dbmodel.ChannelKey{{Enabled: true, ChannelKey: debugTestChannelKey}},
		}
		if err := op.ChannelCreate(&ch, ctx); err != nil {
			t.Fatalf("create channel: %v", err)
		}
		balancer.ResetChannel(ch.ID)
	}
	if sc.failFirst {
		bad := startDebugUpstream(t, true)
		ups = append(ups, bad)
		mkChannel("debug-bad", bad.url, 1)
		mkChannel("debug-good", ok.url, 2)
	} else {
		mkChannel("debug-good", ok.url, 1)
	}
	ups = append(ups, ok)

	if sc.asyncImage {
		runDebugAsyncImageTask(t, ctx, ok.url)
		return collectDebugRun(t, ctx, ups)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, sc.path, strings.NewReader(sc.body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+debugTestInboundKey)
	req.Header.Set("X-Api-Key", debugTestInboundKey)
	req.Header.Set("Cookie", debugTestCookie)
	req.Header.Set("User-Agent", "debug-test-client/1.0")
	for k, v := range sc.secretHeaders {
		req.Header.Set(k, v)
	}
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")
	if sc.handler != nil {
		sc.handler(c)
	} else {
		Handler(sc.inbound, c)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("%s/%s: status %d body %s", sc.name, mode, rec.Code, rec.Body.String())
	}
	return collectDebugRun(t, ctx, ups)
}

// runDebugAsyncImageTask 按执行器（internal/task/image_task_poll.go）的顺序跑一条异步图片任务。
func runDebugAsyncImageTask(t *testing.T, ctx context.Context, upstreamURL string) {
	t.Helper()
	channels, err := op.ChannelList(ctx)
	if err != nil || len(channels) == 0 {
		t.Fatalf("list channels: %v (n=%d)", err, len(channels))
	}
	ch := channels[0]
	task := dbmodel.ImageTask{
		TaskID:       "imgtask-debug-1",
		CreatedAt:    1,
		ChannelID:    ch.ID,
		ChannelKeyID: ch.Keys[0].ID,
		Model:        "img-debug",
		RequestModel: "img-debug",
		Endpoint:     dbmodel.ImageTaskEndpointGenerations,
		RequestBody:  `{"model":"img-debug","prompt":"a fox <b>&&</b>"}`,
	}
	taskLog := NewImageTaskLog(task)
	taskLog.SetChannel(&ch)
	taskLog.BeginUpstream()
	status, body, failReason, runErr := ImageTaskRunUpstream(ctx, &ch, debugTestChannelKey, task.Endpoint, task.Model, []byte(task.RequestBody), taskLog)
	taskLog.EndUpstream()
	if runErr != nil || failReason != "" || status != http.StatusOK {
		t.Fatalf("async image upstream: status=%d fail=%q err=%v", status, failReason, runErr)
	}
	result, _ := ImageTaskResultPayload(body)
	taskLog.Finish(true, status, result, "")
}

func collectDebugRun(t *testing.T, ctx context.Context, ups []*debugUpstream) ([]debugShot, *dbmodel.RelayLog) {
	t.Helper()
	var shots []debugShot
	for _, u := range ups {
		shots = append(shots, u.all()...)
	}
	logs, err := op.RelayLogList(ctx, nil, nil, 1, 5, nil)
	if err != nil || len(logs) == 0 {
		t.Fatalf("list logs: %v (n=%d)", err, len(logs))
	}
	if logs[0].DebugInfo != nil {
		t.Fatalf("list payload must omit debug_info")
	}
	full, err := op.RelayLogGetByID(ctx, logs[0].ID, nil)
	if err != nil {
		t.Fatalf("get log: %v", err)
	}
	return shots, full
}

func debugScenarios() []debugScenario {
	return []debugScenario{
		{
			name: "openai chat", upstreamPath: "/v1/chat/completions", inbound: inbound.InboundTypeOpenAIChat, channelType: outbound.OutboundTypeOpenAIChat,
			model: "gpt-debug", path: "/v1/chat/completions",
			body: `{"model":"gpt-debug","messages":[{"role":"user","content":"hi <b>&&</b>"}]}`,
		},
		{
			name: "anthropic messages", upstreamPath: "/v1/messages", inbound: inbound.InboundTypeAnthropic, channelType: outbound.OutboundTypeAnthropic,
			model: "claude-debug", path: "/v1/messages",
			body: `{"model":"claude-debug","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "failover chain", upstreamPath: "/v1/chat/completions", inbound: inbound.InboundTypeOpenAIChat, channelType: outbound.OutboundTypeOpenAIChat,
			model: "gpt-debug", path: "/v1/chat/completions", failFirst: true,
			body: `{"model":"gpt-debug","messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "images generations", upstreamPath: "/v1/images/generations", channelType: outbound.OutboundTypeOpenAIChat,
			model: "img-debug", path: "/v1/images/generations",
			body:    `{"model":"img-debug","prompt":"a fox <b>&&</b>","size":"1024x1024"}`,
			handler: func(c *gin.Context) { ImagesHandler("/images/generations", c) },
		},
		{
			name: "images failover", upstreamPath: "/v1/images/generations", channelType: outbound.OutboundTypeOpenAIChat,
			model: "img-debug", path: "/v1/images/generations", failFirst: true,
			body:    `{"model":"img-debug","prompt":"a fox"}`,
			handler: func(c *gin.Context) { ImagesHandler("/images/generations", c) },
		},
		{
			name: "chat image bridge", upstreamPath: "/v1/images/generations", inbound: inbound.InboundTypeOpenAIChat, channelType: outbound.OutboundTypeOpenAIChat,
			model: "gpt-debug", path: "/v1/chat/completions",
			body: `{"model":"gpt-debug","modalities":["image"],"messages":[{"role":"user","content":"draw a fox <b>&&</b>"}]}`,
		},
		{
			name: "raw passthrough rerank", upstreamPath: "/v1/rerank", channelType: outbound.OutboundTypeOpenAIChat,
			model: "rerank-debug", path: "/v1/rerank",
			body:    `{"model":"rerank-debug","query":"hi <b>&&</b>","documents":["a","b"]}`,
			handler: func(c *gin.Context) { RawProtocolHandler(RawProtocolOptions{Endpoint: "/rerank", Name: "rerank"}, c) },
		},
		{
			name: "raw passthrough failover", upstreamPath: "/v1/rerank", channelType: outbound.OutboundTypeOpenAIChat,
			model: "rerank-debug", path: "/v1/rerank", failFirst: true,
			body:    `{"model":"rerank-debug","query":"hi","documents":["a"]}`,
			handler: func(c *gin.Context) { RawProtocolHandler(RawProtocolOptions{Endpoint: "/rerank", Name: "rerank"}, c) },
		},
		{
			name: "videos create", upstreamPath: "/v1/videos", channelType: outbound.OutboundTypeOpenAIChat,
			model: "video-debug", path: "/v1/videos",
			body:    `{"model":"video-debug","prompt":"a sunset <b>&&</b>"}`,
			handler: VideosHandler,
		},
		{
			name: "midjourney imagine", upstreamPath: "/mj/submit/imagine", channelType: outbound.OutboundTypeOpenAIChat,
			model: "mj_imagine", path: "/mj/submit/imagine",
			body:          `{"prompt":"a cat <b>&&</b>"}`,
			handler:       func(c *gin.Context) { MidjourneySubmitHandler(c, MidjourneyModeImagine) },
			secretHeaders: map[string]string{"mj-api-secret": debugTestInboundKey},
		},
		{
			name: "async image task", upstreamPath: "/v1/images/generations", channelType: outbound.OutboundTypeOpenAIChat,
			model: "img-debug", asyncImage: true, noRoutes: true,
		},
	}
}

func TestLogDebugModeOutboundIsByteIdentical(t *testing.T) {
	for _, sc := range debugScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			var off1, off2, on []debugShot
			var offLog, onLog *dbmodel.RelayLog
			t.Run("normal-1", func(t *testing.T) { off1, offLog = runDebugScenario(t, sc, dbmodel.LogModeNormal) })
			t.Run("normal-2", func(t *testing.T) { off2, _ = runDebugScenario(t, sc, dbmodel.LogModeNormal) })
			t.Run("debug", func(t *testing.T) { on, onLog = runDebugScenario(t, sc, dbmodel.LogModeDebug) })
			if t.Failed() {
				return
			}
			wantCalls := 1
			if sc.failFirst {
				wantCalls = 2
			}
			if len(off1) != wantCalls || len(off2) != wantCalls || len(on) != wantCalls {
				t.Fatalf("upstream call count: normal=%d/%d debug=%d, want %d", len(off1), len(off2), len(on), wantCalls)
			}
			for i := range on {
				if !strings.HasPrefix(on[i].path, sc.upstreamPath) {
					t.Fatalf("call %d went to %q, want %s* (wrong path exercised)", i, on[i].path, sc.upstreamPath)
				}
			}
			for i := range off1 {
				a1, a2, b := off1[i], off2[i], on[i]
				if !reflect.DeepEqual(a1.names, a2.names) {
					t.Fatalf("call %d: baseline header order not deterministic: %v vs %v", i, a1.names, a2.names)
				}
				if !reflect.DeepEqual(a1.names, b.names) {
					t.Fatalf("call %d: header order/case changed by debug mode:\n normal %v\n debug  %v", i, a1.names, b.names)
				}
				if a1.body != b.body || a1.path != b.path {
					t.Fatalf("call %d: body/path changed by debug mode:\n normal %s %q\n debug  %s %q", i, a1.path, a1.body, b.path, b.body)
				}
				if len(a1.values) != len(b.values) {
					t.Fatalf("call %d: header set changed: normal %d debug %d", i, len(a1.values), len(b.values))
				}
				keys := make([]string, 0, len(a1.values))
				for k := range a1.values {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					if !reflect.DeepEqual(a1.values[k], a2.values[k]) {
						continue // 本来每次就不同（随机 id 等），只比头名和顺序
					}
					if !reflect.DeepEqual(a1.values[k], b.values[k]) {
						t.Fatalf("call %d: header %s changed by debug mode: normal %q debug %q", i, k, a1.values[k], b.values[k])
					}
				}
			}

			// 普通模式不落调试详情；调试模式落，并且内容对得上。
			if offLog.DebugInfo != nil {
				t.Fatalf("normal mode must not store debug_info, got %+v", offLog.DebugInfo)
			}
			info := onLog.DebugInfo
			if info == nil {
				t.Fatal("debug mode must store debug_info")
			}
			if len(info.Calls) != wantCalls || info.Timing.UpstreamCalls != wantCalls {
				t.Fatalf("debug calls = %d (timing %d), want %d", len(info.Calls), info.Timing.UpstreamCalls, wantCalls)
			}
			if sc.noRoutes {
				if len(info.Routes) != 0 {
					t.Fatalf("path without routing must not invent a routing record: %+v", info.Routes)
				}
			} else if len(info.Routes) == 0 || info.Routes[0].Stage != "primary" || info.Routes[0].Mode != "fill_first" || info.Routes[0].Trace == "" {
				t.Fatalf("routing decision missing or wrong: %+v", info.Routes)
			}
			last := info.Calls[len(info.Calls)-1]
			if last.Status != http.StatusOK || len(last.RequestHeaders) == 0 || len(last.ResponseHeaders) == 0 || last.URL == "" {
				t.Fatalf("last call not fully captured: %+v", last)
			}
			if last.ChannelKeyID == 0 || last.ChannelID == 0 {
				t.Fatalf("call must name its channel and key id: %+v", last)
			}
			if sc.failFirst {
				first := info.Calls[0]
				if first.Status != http.StatusServiceUnavailable || first.ChannelName != "debug-bad" {
					t.Fatalf("failover chain first call wrong: %+v", first)
				}
				if !strings.Contains(first.ErrorBody, "overloaded") {
					t.Fatalf("upstream raw error body not captured: %q", first.ErrorBody)
				}
				if last.ChannelName != "debug-good" {
					t.Fatalf("failover chain second call wrong: %+v", last)
				}
			}
			if info.Timing.TotalMs < 0 || info.Timing.BeforeSendMs < 0 || info.Timing.HeaderWaitMs < 0 {
				t.Fatalf("timing breakdown missing: %+v", info.Timing)
			}

			// 凭据一个字符都不许落库。
			raw, _ := json.Marshal(info)
			for _, secret := range []string{debugTestChannelKey, debugTestInboundKey, "cookie-secret-value-42", "should-not-be-stored", debugTestErrSecret} {
				if strings.Contains(string(raw), secret) {
					t.Fatalf("debug_info leaked a secret %q: %s", secret, raw)
				}
			}
			assertHidden := func(where string, hs []dbmodel.RelayLogDebugHeader, name string) {
				for _, h := range hs {
					if strings.EqualFold(h.Name, name) {
						if h.Value != dbmodel.RelayLogDebugHeaderHiddenTag {
							t.Fatalf("%s header %s must be hidden, got %q", where, name, h.Value)
						}
						return
					}
				}
			}
			if !sc.asyncImage {
				// 异步任务执行时，提交请求早已结束：没有入站头可记。
				for _, name := range []string{"Authorization", "X-Api-Key", "Cookie"} {
					assertHidden("inbound", info.InboundHeaders, name)
				}
			}
			for name := range sc.secretHeaders {
				assertHidden("inbound", info.InboundHeaders, name)
			}
			for _, call := range info.Calls {
				assertHidden("outbound", call.RequestHeaders, "Authorization")
				assertHidden("outbound", call.RequestHeaders, "x-api-key")
				assertHidden("outbound", call.RequestHeaders, "mj-api-secret")
				assertHidden("response", call.ResponseHeaders, "Set-Cookie")
			}
			if len(raw) > dbmodel.RelayLogDebugMaxTotalBytes {
				t.Fatalf("debug_info exceeds cap: %d bytes", len(raw))
			}

			// 非管理员视角（Redact）不许看到调试详情。
			if op.RelayLogRedact(*onLog).DebugInfo != nil {
				t.Fatal("redacted log must drop debug_info")
			}
		})
	}
}

func TestDebugHeaderSecretsAreFullyHidden(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer sk-abc")
	h.Set("Proxy-Authorization", "Basic xyz")
	h.Set("X-Api-Key", "k1")
	h.Set("X-Goog-Api-Key", "k2")
	h.Set("Cookie", "a=b")
	h.Set("Set-Cookie", "c=d")
	h.Set("X-Auth-Token", "t")
	h.Set("Anthropic-Version", "2023-06-01")
	h.Set("X-Custom", "contains sk-ant-api03-"+strings.Repeat("A", 40))
	h.Set("X-Long", strings.Repeat("x", 4096))
	got := map[string]string{}
	for _, item := range debugHeaders(h) {
		got[item.Name] = item.Value
	}
	for _, name := range []string{"Authorization", "Proxy-Authorization", "X-Api-Key", "X-Goog-Api-Key", "Cookie", "Set-Cookie", "X-Auth-Token"} {
		if got[name] != dbmodel.RelayLogDebugHeaderHiddenTag {
			t.Fatalf("%s must be hidden, got %q", name, got[name])
		}
	}
	if got["Anthropic-Version"] != "2023-06-01" {
		t.Fatalf("ordinary header must be kept, got %q", got["Anthropic-Version"])
	}
	if strings.Contains(got["X-Custom"], strings.Repeat("A", 40)) {
		t.Fatalf("secret-looking value must be masked, got %q", got["X-Custom"])
	}
	if len(got["X-Long"]) > dbmodel.RelayLogDebugMaxHeaderValue {
		t.Fatalf("header value not capped: %d", len(got["X-Long"]))
	}
	// 只读：原头不许被改动。
	if h.Get("Authorization") != "Bearer sk-abc" {
		t.Fatal("debugHeaders must not mutate the source header")
	}
}

func TestCapDebugInfoStaysUnderTotalLimit(t *testing.T) {
	big := strings.Repeat("v", dbmodel.RelayLogDebugMaxHeaderValue)
	headers := make([]dbmodel.RelayLogDebugHeader, dbmodel.RelayLogDebugMaxHeaders)
	for i := range headers {
		headers[i] = dbmodel.RelayLogDebugHeader{Name: fmt.Sprintf("X-H-%02d", i), Value: big}
	}
	info := &dbmodel.RelayLogDebugInfo{InboundHeaders: headers}
	for i := 0; i < dbmodel.RelayLogDebugMaxCalls; i++ {
		info.Calls = append(info.Calls, dbmodel.RelayLogDebugCall{
			ChannelID: i, RequestHeaders: headers, ResponseHeaders: headers,
			ErrorBody: strings.Repeat("e", dbmodel.RelayLogDebugMaxErrorBody),
		})
	}
	capDebugInfo(info)
	raw, _ := json.Marshal(info)
	if len(raw) > dbmodel.RelayLogDebugMaxTotalBytes {
		t.Fatalf("capped debug_info still %d bytes (cap %d)", len(raw), dbmodel.RelayLogDebugMaxTotalBytes)
	}
	if !info.Truncated {
		t.Fatal("capping must set Truncated")
	}
	if len(info.Calls) == 0 {
		t.Fatal("capping must keep at least the latest call")
	}
}

// 调试关闭时 metrics 不挂记录器：所有钩子都是空操作。
func TestDebugCaptureOffByDefault(t *testing.T) {
	setupRelayErrorDB(t)
	m := NewRelayMetrics(0, 0, "", "m", nil)
	m.enableDebugCapture(http.Header{"Authorization": {"Bearer x"}})
	if m.debug != nil {
		t.Fatal("normal (default) mode must not attach a debug recorder")
	}
	ra := &relayAttempt{relayRequest: &relayRequest{metrics: m}}
	if call := ra.debugBeginCall(); call != nil {
		t.Fatal("no debug call may be recorded when debug mode is off")
	}
	if m.buildDebugInfo(0) != nil {
		t.Fatal("no debug info may be built when debug mode is off")
	}
}
