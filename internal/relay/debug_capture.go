package relay

// 调试模式下的中继日志明细采集（见 model.RelayLogDebugInfo）。
//
// 🔴 只观察，不改出站：
//   - 请求头在**发送返回之后**才读（只读 map，不 Clone 进请求、不重排、不改大小写）；
//   - 不读、不重挂请求体（出站 body 字节由「记录发给上游的实际请求」开关单独负责）；
//   - 不碰 TLS、头序、路由、计费。关闭时 m.debug == nil，所有钩子第一行就返回。
//
// 🔴 不存凭据：Authorization / x-api-key / cookie 等凭据类头整值换成「[已隐藏]」，
// 其余头值、URL、错误原文再过一遍 xredact 打码。每一项都有上限，整份再有总上限。

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/relay/balancer"
	"github.com/zhizhishu/octopus/internal/utils/xredact"
)

// debugRecorder 在一次请求内共享（竞速的每个 racer 拿到的是 metrics 副本，
// 但这个指针是同一个），所以所有写入都要加锁。所有方法都接受 nil 接收者
// （= 调试模式关着），什么都不做。
type debugRecorder struct {
	mu            sync.Mutex
	start         time.Time
	inbound       []dbmodel.RelayLogDebugHeader
	routes        []dbmodel.RelayLogDebugRoute
	routesDropped int
	calls         []*debugCall
	dropped       int

	// 图片 / 原样转发 / 视频 / MJ 这些路径的发送函数拿不到 Key 编号，
	// 由选路循环在每次尝试前用 target 告诉记录器「下一发属于谁」。这些路径都是
	// 串行尝试，不存在两发同时在途。
	targetChannelID   int
	targetChannelName string
	targetKeyID       int
}

type debugCall struct {
	d      *debugRecorder
	rec    dbmodel.RelayLogDebugCall
	sentAt time.Time
}

// newDebugRecorder 只在调试模式打开时返回记录器；关着返回 nil。
func newDebugRecorder(start time.Time, inbound http.Header) *debugRecorder {
	if !op.LogModeDebugActive() {
		return nil
	}
	if start.IsZero() {
		start = time.Now()
	}
	return &debugRecorder{start: start, inbound: debugHeaders(inbound)}
}

// enableDebugCapture 在调试模式打开时给本次请求挂上记录器。关闭时什么都不做。
func (m *RelayMetrics) enableDebugCapture(inbound http.Header) {
	if m == nil {
		return
	}
	m.debug = newDebugRecorder(m.StartTime, inbound)
}

// debugRoute 记下一次选路：分组、模式（优先/轮询）、候选顺序与原因。
func (m *RelayMetrics) debugRoute(stage string, group dbmodel.Group, iter *balancer.Iterator, sticky bool) {
	if m == nil {
		return
	}
	m.debug.route(stage, group, iter, sticky)
}

func (d *debugRecorder) route(stage string, group dbmodel.Group, iter *balancer.Iterator, sticky bool) {
	if d == nil {
		return
	}
	route := dbmodel.RelayLogDebugRoute{
		Stage:     stage,
		GroupID:   group.ID,
		GroupName: group.Name,
		Mode:      debugGroupModeName(group.Mode),
		Sticky:    sticky,
	}
	if iter != nil {
		route.Candidates = iter.Len()
		route.Trace = truncateUTF8(iter.DecisionTrace(), dbmodel.RelayLogDebugMaxRouteTrace)
	}
	d.mu.Lock()
	if len(d.routes) >= dbmodel.RelayLogDebugMaxRoutes {
		d.routesDropped++
	} else {
		d.routes = append(d.routes, route)
	}
	d.mu.Unlock()
}

func debugGroupModeName(mode dbmodel.GroupMode) string {
	switch mode {
	case dbmodel.GroupModeFillFirst:
		return "fill_first"
	case dbmodel.GroupModeSpread:
		return "spread"
	case dbmodel.GroupModeSpreadTiered:
		return "spread_tiered"
	default:
		return "mode_" + strconv.Itoa(int(mode))
	}
}

// target 告诉记录器下一次上游调用属于哪个渠道、哪把 Key（只记编号，不碰 Key 本身）。
func (d *debugRecorder) target(channel *dbmodel.Channel, keyID int) {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.targetChannelID, d.targetChannelName, d.targetKeyID = 0, "", keyID
	if channel != nil {
		d.targetChannelID, d.targetChannelName = channel.ID, channel.Name
	}
	d.mu.Unlock()
}

// begin 在发往上游之前调用，只记时间和渠道，不碰请求。超过次数上限返回 nil。
func (d *debugRecorder) begin(channelID int, channelName string, keyID int) *debugCall {
	if d == nil {
		return nil
	}
	now := time.Now()
	call := &debugCall{d: d, sentAt: now}
	call.rec.StartMs = now.Sub(d.start).Milliseconds()
	call.rec.ChannelID = channelID
	call.rec.ChannelName = channelName
	call.rec.ChannelKeyID = keyID
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.calls) >= dbmodel.RelayLogDebugMaxCalls {
		d.dropped++
		return nil
	}
	d.calls = append(d.calls, call)
	return call
}

func (d *debugRecorder) beginTarget() *debugCall {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	id, name, key := d.targetChannelID, d.targetChannelName, d.targetKeyID
	d.mu.Unlock()
	return d.begin(id, name, key)
}

type debugRecorderKey struct{}

// withDebugRecorder 把记录器挂在 ctx 上，让拿不到 metrics 的发送函数也能记。
// context 里的值不会出现在线上（不进头、不进 body），关着时原样返回 ctx。
func withDebugRecorder(ctx context.Context, d *debugRecorder) context.Context {
	if d == nil || ctx == nil {
		return ctx
	}
	return context.WithValue(ctx, debugRecorderKey{}, d)
}

func debugRecorderFrom(ctx context.Context) *debugRecorder {
	if ctx == nil {
		return nil
	}
	d, _ := ctx.Value(debugRecorderKey{}).(*debugRecorder)
	return d
}

// debugBegin 给「拿不到 metrics、只有 req」的发送点用：从 req 的 ctx 取记录器，
// 渠道 / Key 用选路循环 target 进来的那一份。调试关着时返回 nil。
func debugBegin(req *http.Request) *debugCall {
	if req == nil {
		return nil
	}
	return debugRecorderFrom(req.Context()).beginTarget()
}

// end 在发送返回之后调用：读（只读）已经发出去的请求头、响应头和状态。
func (c *debugCall) end(req *http.Request, resp *http.Response, err error) {
	if c == nil {
		return
	}
	headerMs := time.Since(c.sentAt).Milliseconds()
	var method, url string
	var reqHeaders []dbmodel.RelayLogDebugHeader
	if req != nil {
		method = req.Method
		if req.URL != nil {
			url = truncateUTF8(xredact.URLSecrets(req.URL.String()), dbmodel.RelayLogDebugMaxHeaderValue*2)
		}
		reqHeaders = debugHeaders(req.Header)
	}
	var status int
	var respHeaders []dbmodel.RelayLogDebugHeader
	if resp != nil {
		status = resp.StatusCode
		respHeaders = debugHeaders(resp.Header)
	}
	var errText string
	if err != nil {
		errText = truncateUTF8(xredact.URLSecrets(xredact.Secrets(err.Error())), 1024)
	}
	c.d.mu.Lock()
	c.rec.Method = method
	c.rec.URL = url
	c.rec.HeaderMs = headerMs
	c.rec.Status = status
	c.rec.RequestHeaders = reqHeaders
	c.rec.ResponseHeaders = respHeaders
	c.rec.Error = errText
	c.d.mu.Unlock()
}

// errorBody 记下上游非 2xx 的原文（打码、上限 8KB）。body 是已经读出来的副本。
func (c *debugCall) errorBody(body []byte) {
	if c == nil || len(body) == 0 {
		return
	}
	text := truncateUTF8(xredact.URLSecrets(xredact.Secrets(string(body))), dbmodel.RelayLogDebugMaxErrorBody)
	c.d.mu.Lock()
	c.rec.ErrorBody = text
	c.d.mu.Unlock()
}

// note 给这次上游调用追加一句「网关接下来做了什么」。
func (c *debugCall) note(note string) {
	if c == nil || note == "" {
		return
	}
	c.d.mu.Lock()
	if len(c.rec.Notes) < dbmodel.RelayLogDebugMaxNotes {
		c.rec.Notes = append(c.rec.Notes, truncateUTF8(note, 256))
	}
	c.d.mu.Unlock()
}

// debugBeginCall 是聊天主路径的入口：渠道和 Key 由 relayAttempt 直接给出。
func (ra *relayAttempt) debugBeginCall() *debugCall {
	if ra == nil || ra.relayRequest == nil || ra.metrics == nil || ra.metrics.debug == nil {
		return nil
	}
	var id int
	var name string
	if ra.channel != nil {
		id, name = ra.channel.ID, ra.channel.Name
	}
	call := ra.metrics.debug.begin(id, name, ra.usedKey.ID)
	// 超过次数上限时 call 为 nil：同样覆盖掉，免得报错原文写到上一发的记录里。
	ra.debugCall = call
	return call
}

func (ra *relayAttempt) debugEndCall(call *debugCall, req *http.Request, resp *http.Response, err error) {
	call.end(req, resp, err)
}

func (ra *relayAttempt) debugErrorBody(body []byte) {
	if ra == nil {
		return
	}
	ra.debugCall.errorBody(body)
}

func (ra *relayAttempt) debugNote(note string) {
	if ra == nil {
		return
	}
	ra.debugCall.note(note)
}

// buildDebugInfo 在落库前组装调试详情，并保证整份不超过总上限。
func (m *RelayMetrics) buildDebugInfo(duration time.Duration) *dbmodel.RelayLogDebugInfo {
	if m == nil {
		return nil
	}
	return m.debug.build(duration, m.FirstTokenTime)
}

func (d *debugRecorder) build(duration time.Duration, firstToken time.Time) *dbmodel.RelayLogDebugInfo {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	if len(d.calls) == 0 && len(d.routes) == 0 && len(d.inbound) == 0 && d.dropped == 0 {
		// 什么都没记到（例如异步任务过期、根本没打上游）：不落空壳。
		d.mu.Unlock()
		return nil
	}
	info := &dbmodel.RelayLogDebugInfo{
		InboundHeaders: append([]dbmodel.RelayLogDebugHeader(nil), d.inbound...),
		Routes:         append([]dbmodel.RelayLogDebugRoute(nil), d.routes...),
		DroppedCalls:   d.dropped,
	}
	routesDropped := d.routesDropped
	for _, c := range d.calls {
		rec := c.rec
		rec.RequestHeaders = append([]dbmodel.RelayLogDebugHeader(nil), c.rec.RequestHeaders...)
		rec.ResponseHeaders = append([]dbmodel.RelayLogDebugHeader(nil), c.rec.ResponseHeaders...)
		rec.Notes = append([]string(nil), c.rec.Notes...)
		info.Calls = append(info.Calls, rec)
	}
	start := d.start
	d.mu.Unlock()
	sort.SliceStable(info.Calls, func(i, j int) bool { return info.Calls[i].StartMs < info.Calls[j].StartMs })

	info.Timing = debugTiming(duration, start, firstToken, info.Calls)
	if info.DroppedCalls > 0 || routesDropped > 0 {
		info.Truncated = true
	}
	capDebugInfo(info)
	return info
}

func debugTiming(duration time.Duration, start, firstToken time.Time, calls []dbmodel.RelayLogDebugCall) dbmodel.RelayLogDebugTiming {
	t := dbmodel.RelayLogDebugTiming{
		TotalMs:       duration.Milliseconds(),
		BeforeSendMs:  -1,
		HeaderWaitMs:  -1,
		FirstTokenMs:  -1,
		GenerationMs:  -1,
		UpstreamCalls: len(calls),
	}
	if len(calls) > 0 {
		t.BeforeSendMs = calls[0].StartMs
		t.HeaderWaitMs = calls[len(calls)-1].HeaderMs
	}
	if !firstToken.IsZero() && !start.IsZero() && !firstToken.Before(start) {
		t.FirstTokenMs = firstToken.Sub(start).Milliseconds()
		if gen := duration.Milliseconds() - t.FirstTokenMs; gen >= 0 {
			t.GenerationMs = gen
		}
	}
	return t
}

// capDebugInfo 把整份详情压到 RelayLogDebugMaxTotalBytes 以内：先砍响应头，
// 再砍错误原文，再砍入站头，最后只留最近的几次调用。每砍一步都标 Truncated。
func capDebugInfo(info *dbmodel.RelayLogDebugInfo) {
	size := func() int {
		b, err := json.Marshal(info)
		if err != nil {
			return 0
		}
		return len(b)
	}
	if size() <= dbmodel.RelayLogDebugMaxTotalBytes {
		return
	}
	info.Truncated = true
	for i := range info.Calls {
		info.Calls[i].ResponseHeaders = nil
	}
	if size() <= dbmodel.RelayLogDebugMaxTotalBytes {
		return
	}
	for i := range info.Calls {
		if len(info.Calls[i].ErrorBody) > 1024 {
			info.Calls[i].ErrorBody = truncateUTF8(info.Calls[i].ErrorBody, 1024)
		}
	}
	if size() <= dbmodel.RelayLogDebugMaxTotalBytes {
		return
	}
	info.InboundHeaders = nil
	for size() > dbmodel.RelayLogDebugMaxTotalBytes && len(info.Calls) > 1 {
		info.Calls = info.Calls[1:]
		info.DroppedCalls++
	}
	if size() > dbmodel.RelayLogDebugMaxTotalBytes && len(info.Calls) == 1 {
		info.Calls[0].RequestHeaders = nil
		info.Calls[0].ErrorBody = truncateUTF8(info.Calls[0].ErrorBody, 256)
	}
	for i := range info.Routes {
		if size() <= dbmodel.RelayLogDebugMaxTotalBytes {
			break
		}
		info.Routes[i].Trace = truncateUTF8(info.Routes[i].Trace, 512)
	}
}

// debugHeaderIsSecret 判定一个头是不是凭据类：命中就整值隐藏，一个字符都不留。
func debugHeaderIsSecret(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	switch lower {
	case "authorization", "proxy-authorization", "x-api-key", "api-key", "x-goog-api-key",
		"cookie", "set-cookie", "x-amz-security-token", "x-amz-signature":
		return true
	}
	for _, part := range []string{"auth", "token", "secret", "password", "api-key", "apikey", "api_key", "cookie", "credential", "signature"} {
		if strings.Contains(lower, part) {
			return true
		}
	}
	return false
}

// debugHeaders 把一组头转成可落库的列表：按名字排序、凭据整值隐藏、其余值打码并截断。
// 只读 h，不修改。
func debugHeaders(h http.Header) []dbmodel.RelayLogDebugHeader {
	if len(h) == 0 {
		return nil
	}
	names := make([]string, 0, len(h))
	for name := range h {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]dbmodel.RelayLogDebugHeader, 0, min(len(names), dbmodel.RelayLogDebugMaxHeaders))
	for _, name := range names {
		if len(out) >= dbmodel.RelayLogDebugMaxHeaders {
			break
		}
		value := dbmodel.RelayLogDebugHeaderHiddenTag
		if !debugHeaderIsSecret(name) {
			value = truncateUTF8(xredact.Secrets(strings.Join(h[name], ", ")), dbmodel.RelayLogDebugMaxHeaderValue)
		}
		out = append(out, dbmodel.RelayLogDebugHeader{Name: truncateUTF8(name, 128), Value: value})
	}
	return out
}
