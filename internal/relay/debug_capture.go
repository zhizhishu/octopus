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
// 但这个指针是同一个），所以所有写入都要加锁。
type debugRecorder struct {
	mu      sync.Mutex
	start   time.Time
	inbound []dbmodel.RelayLogDebugHeader
	routes  []dbmodel.RelayLogDebugRoute
	calls   []*debugCall
	dropped int
}

type debugCall struct {
	rec    dbmodel.RelayLogDebugCall
	sentAt time.Time
}

// enableDebugCapture 在调试模式打开时给本次请求挂上记录器。关闭时什么都不做。
func (m *RelayMetrics) enableDebugCapture(inbound http.Header) {
	if m == nil || !op.LogModeDebugActive() {
		return
	}
	m.debug = &debugRecorder{
		start:   m.StartTime,
		inbound: debugHeaders(inbound),
	}
}

// debugRoute 记下一次选路：分组、模式（优先/轮询）、候选顺序与原因。
func (m *RelayMetrics) debugRoute(stage string, group dbmodel.Group, iter *balancer.Iterator, sticky bool) {
	if m == nil || m.debug == nil {
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
	m.debug.mu.Lock()
	m.debug.routes = append(m.debug.routes, route)
	m.debug.mu.Unlock()
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

// debugBeginCall 在发往上游之前调用，只记时间和渠道，不碰请求。
func (ra *relayAttempt) debugBeginCall() *debugCall {
	if ra == nil || ra.relayRequest == nil || ra.metrics == nil || ra.metrics.debug == nil {
		return nil
	}
	d := ra.metrics.debug
	now := time.Now()
	call := &debugCall{sentAt: now}
	call.rec.StartMs = now.Sub(d.start).Milliseconds()
	if ra.channel != nil {
		call.rec.ChannelID = ra.channel.ID
		call.rec.ChannelName = ra.channel.Name
	}
	call.rec.ChannelKeyID = ra.usedKey.ID
	d.mu.Lock()
	if len(d.calls) >= dbmodel.RelayLogDebugMaxCalls {
		d.dropped++
		d.mu.Unlock()
		return nil
	}
	d.calls = append(d.calls, call)
	d.mu.Unlock()
	ra.debugCall = call
	return call
}

// debugEndCall 在发送返回之后调用：读（只读）已经发出去的请求头、响应头和状态。
func (ra *relayAttempt) debugEndCall(call *debugCall, req *http.Request, resp *http.Response, err error) {
	if call == nil {
		return
	}
	d := ra.metrics.debug
	headerMs := time.Since(call.sentAt).Milliseconds()
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
	d.mu.Lock()
	call.rec.Method = method
	call.rec.URL = url
	call.rec.HeaderMs = headerMs
	call.rec.Status = status
	call.rec.RequestHeaders = reqHeaders
	call.rec.ResponseHeaders = respHeaders
	call.rec.Error = errText
	d.mu.Unlock()
}

// debugErrorBody 记下上游非 2xx 的原文（打码、上限 8KB）。body 是已经读出来的副本。
func (ra *relayAttempt) debugErrorBody(body []byte) {
	if ra == nil || ra.debugCall == nil || len(body) == 0 {
		return
	}
	text := truncateUTF8(xredact.URLSecrets(xredact.Secrets(string(body))), dbmodel.RelayLogDebugMaxErrorBody)
	d := ra.metrics.debug
	d.mu.Lock()
	ra.debugCall.rec.ErrorBody = text
	d.mu.Unlock()
}

// debugNote 给当前这次上游调用追加一句「网关接下来做了什么」。
func (ra *relayAttempt) debugNote(note string) {
	if ra == nil || ra.debugCall == nil || note == "" {
		return
	}
	d := ra.metrics.debug
	d.mu.Lock()
	if len(ra.debugCall.rec.Notes) < dbmodel.RelayLogDebugMaxNotes {
		ra.debugCall.rec.Notes = append(ra.debugCall.rec.Notes, truncateUTF8(note, 256))
	}
	d.mu.Unlock()
}

// buildDebugInfo 在落库前组装调试详情，并保证整份不超过总上限。
func (m *RelayMetrics) buildDebugInfo(duration time.Duration) *dbmodel.RelayLogDebugInfo {
	if m == nil || m.debug == nil {
		return nil
	}
	d := m.debug
	d.mu.Lock()
	info := &dbmodel.RelayLogDebugInfo{
		InboundHeaders: append([]dbmodel.RelayLogDebugHeader(nil), d.inbound...),
		Routes:         append([]dbmodel.RelayLogDebugRoute(nil), d.routes...),
		DroppedCalls:   d.dropped,
	}
	for _, c := range d.calls {
		rec := c.rec
		rec.RequestHeaders = append([]dbmodel.RelayLogDebugHeader(nil), c.rec.RequestHeaders...)
		rec.ResponseHeaders = append([]dbmodel.RelayLogDebugHeader(nil), c.rec.ResponseHeaders...)
		rec.Notes = append([]string(nil), c.rec.Notes...)
		info.Calls = append(info.Calls, rec)
	}
	d.mu.Unlock()
	sort.SliceStable(info.Calls, func(i, j int) bool { return info.Calls[i].StartMs < info.Calls[j].StartMs })

	info.Timing = m.debugTiming(duration, info.Calls)
	if info.DroppedCalls > 0 {
		info.Truncated = true
	}
	capDebugInfo(info)
	return info
}

func (m *RelayMetrics) debugTiming(duration time.Duration, calls []dbmodel.RelayLogDebugCall) dbmodel.RelayLogDebugTiming {
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
	if !m.FirstTokenTime.IsZero() {
		t.FirstTokenMs = m.FirstTokenTime.Sub(m.StartTime).Milliseconds()
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
