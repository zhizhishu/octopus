package relay

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/price"
)

// 假上游：xAI /v1/realtime 的最小实现。记录握手头/查询参数，把收到的每条消息按原类型
// 交给 onMessage 决定回什么。
type fakeRealtimeUpstream struct {
	srv *httptest.Server

	mu          sync.Mutex
	path        string
	auth        string
	query       url.Values
	subprotocol []string
	userAgent   string
	received    []fakeRealtimeMsg
}

type fakeRealtimeMsg struct {
	Type int
	Data []byte
}

func newFakeRealtimeUpstream(t *testing.T, rejectStatus int, onOpen func(*websocket.Conn), onMessage func(*websocket.Conn, int, []byte)) *fakeRealtimeUpstream {
	t.Helper()
	f := &fakeRealtimeUpstream{}
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.path = r.URL.Path
		f.auth = r.Header.Get("Authorization")
		f.query = r.URL.Query()
		f.subprotocol = websocket.Subprotocols(r)
		f.userAgent = r.UserAgent()
		f.mu.Unlock()
		if rejectStatus != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(rejectStatus)
			_, _ = w.Write([]byte(`{"error":{"message":"rejected by fake","code":"fake_reject"}}`))
			return
		}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if onOpen != nil {
			onOpen(conn)
		}
		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			f.mu.Lock()
			f.received = append(f.received, fakeRealtimeMsg{mt, append([]byte(nil), data...)})
			f.mu.Unlock()
			if onMessage != nil {
				onMessage(conn, mt, data)
			}
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// newRealtimeGateway 起一个真 HTTP 服务（WebSocket 需要 Hijack，httptest.Recorder 不行），
// 模拟 APIKeyAuth 已经放行后写进上下文的字段。
func newRealtimeGateway(t *testing.T, userID int) *httptest.Server {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/v1/realtime", func(c *gin.Context) {
		c.Set("api_key_id", 0)
		c.Set("user_id", userID)
		c.Set("request_ip", "127.0.0.1")
		GrokRealtimeHandler(c)
	})
	srv := httptest.NewServer(engine)
	t.Cleanup(srv.Close)
	return srv
}

func realtimeWSURL(srv *httptest.Server, query string) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/realtime" + query
}

// realtimeLogWatcher 只认本测试里新写出的 audio_realtime 日志：日志列表在整包测试里会带着
// 前面用例留下的行（含写入缓冲），直接取 logs[0] 在全量跑时会拿错。
type realtimeLogWatcher struct {
	ctx  context.Context
	seen map[int64]bool
}

func newRealtimeLogWatcher(t *testing.T, ctx context.Context) *realtimeLogWatcher {
	t.Helper()
	w := &realtimeLogWatcher{ctx: ctx, seen: map[int64]bool{}}
	if logs, err := op.RelayLogList(ctx, nil, nil, 1, 200, nil); err == nil {
		for _, l := range logs {
			w.seen[l.ID] = true
		}
	}
	return w
}

func (w *realtimeLogWatcher) next(t *testing.T) dbmodel.RelayLog {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		logs, err := op.RelayLogList(w.ctx, nil, nil, 1, 200, nil)
		if err == nil {
			for _, l := range logs {
				if !w.seen[l.ID] && l.RequestEndpoint == grokRealtimeProtocol {
					w.seen[l.ID] = true
					return l
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("realtime relay log not written")
	return dbmodel.RelayLog{}
}

func TestGrokRealtimeRelaysBothDirectionsAndLogsUsage(t *testing.T) {
	ctx := setupRelayErrorDB(t)
	rtLogs := newRealtimeLogWatcher(t, ctx)
	t.Cleanup(func() { _ = op.RelayLogClear(context.Background(), nil) })

	audioOut := []byte{0x01, 0x02, 0x03, 0xFF}
	upstream := newFakeRealtimeUpstream(t, 0,
		func(conn *websocket.Conn) {
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.created","session":{"id":"s1"}}`))
		},
		func(conn *websocket.Conn, mt int, data []byte) {
			if mt == websocket.BinaryMessage {
				// 二进制音频原样回一帧二进制。
				_ = conn.WriteMessage(websocket.BinaryMessage, audioOut)
				return
			}
			var ev map[string]any
			_ = json.Unmarshal(data, &ev)
			if ev["type"] == "response.create" {
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_audio.delta","delta":"AAAA"}`))
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.done","response":{"usage":{"total_tokens":30,"input_tokens":12,"output_tokens":18,"input_token_details":{"cached_tokens":2,"text_tokens":10,"audio_tokens":2}}}}`))
			}
		})
	createRawProtocolGroup(t, ctx, upstream.srv.URL, "grok-voice-latest", "grok-voice-3.0", "xai-rt-key")
	price.SetLLMPrice("grok-voice-latest", dbmodel.LLMPrice{Input: 1, Output: 2})
	t.Cleanup(func() { price.SetLLMPrice("grok-voice-latest", dbmodel.LLMPrice{}) })

	gw := newRealtimeGateway(t, 0)
	dialer := websocket.Dialer{Subprotocols: []string{"realtime", "openai-insecure-api-key.sk-octopus-should-not-leak"}}
	client, resp, err := dialer.Dial(realtimeWSURL(gw, "?conversation_id=conv-9"), nil)
	if err != nil {
		t.Fatalf("dial gateway: %v (resp=%v)", err, resp)
	}
	if got := client.Subprotocol(); got != "realtime" {
		t.Fatalf("negotiated subprotocol %q, want realtime", got)
	}

	readJSON := func() map[string]any {
		t.Helper()
		_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
		mt, data, err := client.ReadMessage()
		if err != nil {
			t.Fatalf("client read: %v", err)
		}
		if mt != websocket.TextMessage {
			t.Fatalf("want text frame, got %d", mt)
		}
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("bad json %s", data)
		}
		return m
	}
	if ev := readJSON(); ev["type"] != "session.created" {
		t.Fatalf("first event %v", ev)
	}
	if err := client.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.update","session":{"voice":"eve"}}`)); err != nil {
		t.Fatal(err)
	}
	if err := client.WriteMessage(websocket.BinaryMessage, []byte{0xAA, 0xBB}); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	mt, data, err := client.ReadMessage()
	if err != nil || mt != websocket.BinaryMessage || string(data) != string(audioOut) {
		t.Fatalf("binary frame not relayed verbatim: mt=%d data=%x err=%v", mt, data, err)
	}
	if err := client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create"}`)); err != nil {
		t.Fatal(err)
	}
	if ev := readJSON(); ev["type"] != "response.output_audio.delta" {
		t.Fatalf("want audio delta, got %v", ev)
	}
	if ev := readJSON(); ev["type"] != "response.done" {
		t.Fatalf("want response.done, got %v", ev)
	}
	_ = client.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"), time.Now().Add(time.Second))
	_ = client.Close()

	entry := rtLogs.next(t)

	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if upstream.path != "/v1/realtime" {
		t.Fatalf("upstream path %q", upstream.path)
	}
	if upstream.auth != "Bearer xai-rt-key" {
		t.Fatalf("upstream auth %q, want channel key", upstream.auth)
	}
	if got := upstream.query.Get("model"); got != "grok-voice-3.0" {
		t.Fatalf("upstream model %q, want mapped grok-voice-3.0", got)
	}
	if got := upstream.query.Get("conversation_id"); got != "conv-9" {
		t.Fatalf("conversation_id not forwarded: %v", upstream.query)
	}
	if len(upstream.subprotocol) != 0 {
		t.Fatalf("client subprotocols (may carry the octopus key) must not reach upstream: %v", upstream.subprotocol)
	}
	if upstream.userAgent == "" || strings.Contains(upstream.userAgent, "Go-http-client") {
		t.Fatalf("upstream UA should be the unified generic UA, got %q", upstream.userAgent)
	}
	if len(upstream.received) != 3 {
		t.Fatalf("upstream received %d messages, want 3", len(upstream.received))
	}
	if upstream.received[0].Type != websocket.TextMessage || !strings.Contains(string(upstream.received[0].Data), `"voice":"eve"`) {
		t.Fatalf("session.update not relayed verbatim: %+v", upstream.received[0])
	}
	if upstream.received[1].Type != websocket.BinaryMessage || string(upstream.received[1].Data) != "\xAA\xBB" {
		t.Fatalf("binary client frame not relayed verbatim: %+v", upstream.received[1])
	}

	if entry.RequestEndpoint != "audio_realtime" || entry.RequestPath != "/v1/realtime" || entry.RequestModelName != "grok-voice-latest" || entry.ActualModelName != "grok-voice-3.0" {
		t.Fatalf("log identity: endpoint=%q path=%q model=%q actual=%q", entry.RequestEndpoint, entry.RequestPath, entry.RequestModelName, entry.ActualModelName)
	}
	if entry.InputTokens != 12 || entry.OutputTokens != 18 || entry.CacheHitTokens != 2 {
		t.Fatalf("usage from response.done not logged: in=%d out=%d cache=%d", entry.InputTokens, entry.OutputTokens, entry.CacheHitTokens)
	}
	// 没配 per_call → 按 token：(12-2)*1 + 18*2 = 46 / 1e6（缓存读价为 0）。
	if want := 46e-6; math.Abs(entry.Cost-want) > 1e-12 {
		t.Fatalf("token cost %.9f, want %.9f", entry.Cost, want)
	}
	detail, err := op.RelayLogGetByID(ctx, entry.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(detail.ResponseContent, "AAAA") || !strings.Contains(detail.ResponseContent, `"audio_observed":true`) {
		t.Fatalf("response log should be a session summary without audio: %s", detail.ResponseContent)
	}
	if strings.Contains(detail.RequestContent, "sk-octopus") {
		t.Fatalf("request log leaked the key: %s", detail.RequestContent)
	}
}

func TestGrokRealtimePerMinuteBillingWhenPerCallConfigured(t *testing.T) {
	ctx := setupRelayErrorDB(t)
	rtLogs := newRealtimeLogWatcher(t, ctx)
	t.Cleanup(func() { _ = op.RelayLogClear(context.Background(), nil) })

	upstream := newFakeRealtimeUpstream(t, 0, nil, func(conn *websocket.Conn, mt int, data []byte) {
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_audio.delta","delta":"UklGRg=="}`))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.done","response":{"usage":{"total_tokens":5,"input_tokens":2,"output_tokens":3}}}`))
	})
	createRawProtocolGroup(t, ctx, upstream.srv.URL, "grok-voice-latest", "grok-voice-latest", "k")
	// per_call 在 /v1/realtime 上是"每分钟"单价；故意把 token 价也配上，确认两者不叠加。
	price.SetLLMPrice("grok-voice-latest", dbmodel.LLMPrice{Input: 1000, Output: 1000, PerCall: 60})
	t.Cleanup(func() { price.SetLLMPrice("grok-voice-latest", dbmodel.LLMPrice{}) })

	gw := newRealtimeGateway(t, 0)
	header := http.Header{}
	client, _, err := websocket.DefaultDialer.Dial(realtimeWSURL(gw, ""), header)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = client.WriteMessage(websocket.TextMessage, []byte(`{"type":"input_audio_buffer.append","audio":"AAAA"}`))
	for i := 0; i < 2; i++ {
		_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, _, err := client.ReadMessage(); err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
	}
	time.Sleep(50 * time.Millisecond)
	_ = client.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
	_ = client.Close()

	entry := rtLogs.next(t)
	// 60/分钟 = 1/秒，金额≈会话秒数，必然 > 0 且远小于按 token 算出来的 (2+3)*1000/1e6=0.005 以外的量级无关；
	// 只需确认按时长计费、没有把 token 金额叠加进去。
	if entry.Cost <= 0 {
		t.Fatalf("per-minute billing produced cost %f", entry.Cost)
	}
	maxSeconds := float64(entry.UseTime)/1000 + 0.05
	if entry.Cost > maxSeconds {
		t.Fatalf("cost %f exceeds session seconds %f (token cost must not be added on top)", entry.Cost, maxSeconds)
	}
	if entry.InputTokens != 2 || entry.OutputTokens != 3 {
		t.Fatalf("tokens still logged for visibility: in=%d out=%d", entry.InputTokens, entry.OutputTokens)
	}
}

func TestGrokRealtimeCostUnits(t *testing.T) {
	const modelName = "grok-voice-cost-unit"
	price.SetLLMPrice(modelName, dbmodel.LLMPrice{PerCall: 0.08})
	t.Cleanup(func() { price.SetLLMPrice(modelName, dbmodel.LLMPrice{}) })

	m := newRawProtocolRelayMetrics(0, 0, "", modelName, "realtime", "/v1/realtime")
	applyGrokRealtimeCost(m, modelName, &grokRealtimeSession{Duration: 90 * time.Second, AudioObserved: true})
	if math.Abs(m.Stats.OutputCost-0.12) > 1e-9 {
		t.Fatalf("90s at 0.08/min = 0.12, got %f", m.Stats.OutputCost)
	}

	m = newRawProtocolRelayMetrics(0, 0, "", modelName, "realtime", "/v1/realtime")
	applyGrokRealtimeCost(m, modelName, &grokRealtimeSession{Duration: 90 * time.Second, AudioObserved: false})
	if m.Stats.OutputCost != 0 {
		t.Fatalf("session without audio must not bill minutes (sub2api audioObserved gate), got %f", m.Stats.OutputCost)
	}
}

func TestGrokRealtimeDialFailureFailsOverBeforeUpgrade(t *testing.T) {
	ctx := setupRelayErrorDB(t)
	rtLogs := newRealtimeLogWatcher(t, ctx)
	t.Cleanup(func() { _ = op.RelayLogClear(context.Background(), nil) })

	bad := newFakeRealtimeUpstream(t, http.StatusInternalServerError, nil, nil)
	good := newFakeRealtimeUpstream(t, 0, func(conn *websocket.Conn) {
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.created"}`))
	}, nil)
	createRawProtocolChannelWithModel(t, ctx, "rt-bad", bad.srv.URL, []string{"bad-key"}, "grok-voice-latest", nil)
	createRawProtocolChannelWithModel(t, ctx, "rt-good", good.srv.URL, []string{"good-key"}, "grok-voice-latest", nil)

	gw := newRealtimeGateway(t, 0)
	// 渠道顺序由负载均衡决定，跑几轮保证"坏渠道在前"的情形一定出现过。
	for i := 0; i < 4; i++ {
		_ = op.RelayLogClear(context.Background(), nil)
		client, _, err := websocket.DefaultDialer.Dial(realtimeWSURL(gw, ""), nil)
		if err != nil {
			t.Fatalf("round %d: dial should succeed via failover: %v", i, err)
		}
		_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, data, err := client.ReadMessage(); err != nil || !strings.Contains(string(data), "session.created") {
			t.Fatalf("round %d: expected session.created from good upstream, got %s err=%v", i, data, err)
		}
		_ = client.Close()
		entry := rtLogs.next(t)
		if entry.ChannelName != "rt-good" {
			t.Fatalf("round %d: want final channel rt-good, got %q attempts=%d", i, entry.ChannelName, entry.TotalAttempts)
		}
	}
	bad.mu.Lock()
	badHit := bad.auth != ""
	bad.mu.Unlock()
	if !badHit {
		t.Fatalf("test never exercised the failing upstream")
	}
	good.mu.Lock()
	defer good.mu.Unlock()
	if good.auth != "Bearer good-key" {
		t.Fatalf("good upstream auth %q", good.auth)
	}
}

func TestGrokRealtimeAllUpstreamsRejectReturnsHTTPError(t *testing.T) {
	ctx := setupRelayErrorDB(t)
	rtLogs := newRealtimeLogWatcher(t, ctx)
	t.Cleanup(func() { _ = op.RelayLogClear(context.Background(), nil) })

	bad := newFakeRealtimeUpstream(t, http.StatusUnauthorized, nil, nil)
	createRawProtocolGroup(t, ctx, bad.srv.URL, "grok-voice-latest", "grok-voice-latest", "revoked")

	gw := newRealtimeGateway(t, 0)
	_, resp, err := websocket.DefaultDialer.Dial(realtimeWSURL(gw, ""), nil)
	if err == nil {
		t.Fatal("handshake must fail when every upstream rejects")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("client should get the upstream status as a plain HTTP error before any upgrade, got %v", resp)
	}
	entry := rtLogs.next(t)
	if entry.Error == "" || entry.Cost != 0 {
		t.Fatalf("failed handshake must be logged as an error and not billed: err=%q cost=%f", entry.Error, entry.Cost)
	}
}

func TestGrokRealtimeRequiresUpgrade(t *testing.T) {
	setupRelayErrorDB(t)
	gw := newRealtimeGateway(t, 0)
	res, err := http.Get(gw.URL + "/v1/realtime?model=grok-voice-latest")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("plain GET: status %d, want 426", res.StatusCode)
	}
}

func TestGrokRealtimeUpstreamCloseIsForwarded(t *testing.T) {
	ctx := setupRelayErrorDB(t)
	rtLogs := newRealtimeLogWatcher(t, ctx)
	t.Cleanup(func() { _ = op.RelayLogClear(context.Background(), nil) })

	upstream := newFakeRealtimeUpstream(t, 0, func(conn *websocket.Conn) {
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(4001, "idle timeout"), time.Now().Add(time.Second))
	}, nil)
	createRawProtocolGroup(t, ctx, upstream.srv.URL, "grok-voice-latest", "grok-voice-latest", "k")

	gw := newRealtimeGateway(t, 0)
	client, _, err := websocket.DefaultDialer.Dial(realtimeWSURL(gw, ""), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, err = client.ReadMessage()
	var ce *websocket.CloseError
	if !errorsAsClose(err, &ce) || ce.Code != 4001 || ce.Text != "idle timeout" {
		t.Fatalf("upstream close code/reason must reach the client, got %v", err)
	}
	entry := rtLogs.next(t)
	if entry.Cost != 0 {
		t.Fatalf("no audio, no usage → no cost, got %f", entry.Cost)
	}
}

func errorsAsClose(err error, target **websocket.CloseError) bool {
	ce, ok := err.(*websocket.CloseError)
	if ok {
		*target = ce
	}
	return ok
}

func TestGrokRealtimeTargetURL(t *testing.T) {
	ch := &dbmodel.Channel{BaseUrls: []dbmodel.BaseUrl{{URL: "https://api.x.ai/v1"}}}
	got, err := grokRealtimeTargetURL(ch, "grok-voice-3.0", url.Values{"model": {"alias"}, "conversation_id": {"c1"}})
	if err != nil {
		t.Fatal(err)
	}
	if got != "wss://api.x.ai/v1/realtime?conversation_id=c1&model=grok-voice-3.0" {
		t.Fatalf("target url %q", got)
	}
}
