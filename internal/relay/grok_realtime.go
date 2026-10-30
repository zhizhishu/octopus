package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/zhizhishu/octopus/internal/helper"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/price"
	"github.com/zhizhishu/octopus/internal/relay/balancer"
	"github.com/zhizhishu/octopus/internal/server/resp"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
	"github.com/zhizhishu/octopus/internal/utils/log"
	"github.com/zhizhishu/octopus/internal/utils/xurl"
)

// xAI（Grok）实时语音 WebSocket 中转：GET /v1/realtime?model=…（WebSocket 升级）。
//
// 形态照两个参考项目拼起来：
//   - 路由 / 握手 / 先拨上游再接受客户端：sub2api handler/grok_audio.go GrokRealtime
//     （"Keep the HTTP response uncommitted while selecting and probing an account"）。
//     这样上游 401/429/5xx 都还是普通 HTTP 错误，能像其它 raw 端点一样换 key / 换渠道，
//     客户端也拿到正常的 JSON 错误而不是一条莫名其妙断掉的 WebSocket。
//   - 双向泵 + 从 response.done 累计 usage：new-api relay/channel/openai/relay_realtime.go。
//   - 浏览器把 key 放在子协议里：new-api middleware/auth.go apiKeyFromWebSocketSubprotocol
//     （openai-insecure-api-key.<key>），以及 xAI 文档的 xai-client-secret.<token>。
//     这部分在 middleware.APIKeyAuth 里做，这里只负责回一个客户端提出过的子协议。
//
// 计费（两种口径二选一，不叠加）：
//   - 模型价格行 per_call > 0：按**会话分钟数**计费（per_call 在这个端点上就是"每分钟单价"），
//     与 xAI 官方计价（Speech to Speech $0.08/min）和 sub2api grokRealtimeBillingResult
//     （elapsed.Minutes()，且只在会话里真的出现过音频时计）一致。
//   - per_call = 0：按上游 response.done 里的 usage token 用 Input/Output/CacheRead 单价计
//     （new-api 口径）。两种都没配 → 0 元并留警告，与其它 raw 端点一致。
//
// 出站：自带 gorilla/websocket 拨号器，只沿用渠道的代理选择（helper.ChannelProxyURL）、
// 渠道 key、渠道自定义头与统一的非 CLI UA。不碰 cloak / shape / TLS 面容代码，也不带任何
// CLI 身份 —— 这不是 codex/claude 协议。

const (
	// GrokRealtimeDefaultModel 是 ?model= 缺省时用来选渠道、计费、并写给上游的模型名，
	// 与 xAI 文档和 sub2api 的缺省一致。
	GrokRealtimeDefaultModel = "grok-voice-latest"

	// grokRealtimeProtocol 是日志里的端点名。与 /v1/tts、/v1/stt 的 audio_tts / audio_stt 同一前缀，
	// 日志页现有的 "audio" 端点筛选（按 <family>_<variant> 匹配）直接就能筛到，不用改前端。
	grokRealtimeProtocol = "audio_realtime"
	// grokRealtimeDialTimeout 只约束"拨上游 + 握手"，建立后的会话不受它影响。
	// 取值同 sub2api service.DefaultGrokRealtimeDialTimeout。
	grokRealtimeDialTimeout = 12 * time.Second
	// grokRealtimeReadLimit 单条消息上限。音频以 base64 放在 JSON 里，100ms 一块，
	// 正常远小于这个值；设上限只是防一端塞超大帧把进程内存打爆。
	grokRealtimeReadLimit  = 16 << 20
	grokRealtimeCloseGrace = time.Second
)

// grokRealtimeDialer 可在测试里替换（例如注入 NetDialContext）。生产用默认值。
var grokRealtimeDialer = func(proxyURL *url.URL) *websocket.Dialer {
	d := &websocket.Dialer{
		HandshakeTimeout:  grokRealtimeDialTimeout,
		ReadBufferSize:    32 << 10,
		WriteBufferSize:   32 << 10,
		EnableCompression: false,
	}
	if proxyURL != nil {
		d.Proxy = http.ProxyURL(proxyURL)
	}
	return d
}

// IsWebSocketUpgradeRequest 判断是否是 WebSocket 升级请求（Connection: upgrade + Upgrade: websocket）。
func IsWebSocketUpgradeRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	return headerHasToken(r.Header, "Connection", "upgrade") && headerHasToken(r.Header, "Upgrade", "websocket")
}

func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// GrokRealtimeHandler 处理 GET /v1/realtime。
func GrokRealtimeHandler(c *gin.Context) {
	if !IsWebSocketUpgradeRequest(c.Request) {
		resp.ErrorWithCode(c, http.StatusUpgradeRequired, "websocket_upgrade_required", "WebSocket upgrade required (Upgrade: websocket)")
		return
	}
	ctx := c.Request.Context()
	apiKeyID := c.GetInt("api_key_id")
	userID := c.GetInt("user_id")

	requestModel := strings.TrimSpace(c.Query("model"))
	if requestModel == "" {
		requestModel = GrokRealtimeDefaultModel
	}
	if !op.IsModelSupported(strings.TrimSpace(c.GetString("supported_models")), requestModel) {
		resp.Error(c, http.StatusBadRequest, "model not supported")
		return
	}

	routeResult, status, message, err := selectRouteGroup(c, apiKeyID, requestModel)
	if err != nil || status != 0 {
		if message == "" && err != nil {
			message = err.Error()
		}
		resp.Error(c, status, message)
		return
	}
	group := enrichGroupForSmartRouting(ctx, routeResult.Group, true)
	clientSession := deriveClientSessionInfo(c.Request.Header, nil)
	stickyEnabled := routeStickyEnabled(group.Mode, clientSession.Source)
	iter := balancer.NewIteratorWithSession(group, apiKeyID, requestModel, clientSession.Key, stickyEnabled)
	if iter.Len() == 0 {
		resp.Error(c, http.StatusServiceUnavailable, "no available channel")
		return
	}

	metrics := newRawProtocolRelayMetrics(apiKeyID, userID, c.GetString("request_ip"), requestModel, grokRealtimeProtocol, c.Request.URL.Path)
	metrics.RequestUserAgent = c.Request.UserAgent()
	metrics.SetAccessPlan(routeResult.AccessPlan, routeResult.AccessRouteRule, routeResult.AccessRouteUsed)
	metrics.SetClientSession(clientSession)
	metrics.RequestContent = grokRealtimeRequestLog(requestModel, c.Request.URL.Query())

	var (
		lastErr          error
		allAttempts      []model.ChannelAttempt
		triedReturnGroup bool
	)

runIterator:
	for iter.Next() {
		if ctx.Err() != nil {
			metrics.Save(ctx, false, context.Canceled, append(allAttempts, iter.Attempts()...))
			return
		}
		item := iter.Item()
		channel, err := op.ChannelGet(item.ChannelID, ctx)
		if err != nil {
			iter.Skip(item.ChannelID, 0, fmt.Sprintf("channel_%d", item.ChannelID), fmt.Sprintf("channel not found: %v", err))
			lastErr = err
			continue
		}
		if !channel.Enabled {
			iter.Skip(channel.ID, 0, channel.Name, "channel disabled")
			continue
		}
		if !grokRealtimeAllowsChannel(channel.Type) {
			iter.Skip(channel.ID, 0, channel.Name, fmt.Sprintf("unsupported channel type for realtime: %d", channel.Type))
			continue
		}
		if mapped, ok := channel.ModelMapping[item.ModelName]; ok && mapped != "" {
			item.ModelName = mapped
			iter.RemapCurrentModel(mapped)
		}
		availableKeys := channel.GetAvailableChannelKeys()
		if len(availableKeys) == 0 {
			iter.Skip(channel.ID, 0, channel.Name, "no available key")
			continue
		}
		availableKeys = balancer.PrioritizeChannelKeysByHealth(availableKeys, channel.ID, item.ModelName, iter.StickyKeyIDForCurrentChannel(channel.ID))

		for _, usedKey := range availableKeys {
			if iter.SkipCircuitBreak(channel.ID, usedKey.ID, channel.Name) {
				continue
			}
			span := iter.StartAttempt(channel.ID, usedKey.ID, channel.Name)
			recordAttemptProxy(span, channel)
			finishRuntimeAttempt := balancer.BeginRuntimeAttempt(channel.ID, usedKey.ID, item.ModelName)
			upstream, statusCode, dialErr := dialGrokRealtimeUpstream(ctx, c, channel, usedKey.ChannelKey, item.ModelName)
			if dialErr != nil {
				finishRuntimeAttempt()
				usedAt := time.Now().Unix()
				recordStatusCode := attemptStatusCode(statusCode, dialErr)
				op.ChannelKeyRecordUse(usedKey, recordStatusCode, usedAt, 0)
				span.End(model.AttemptFailed, recordStatusCode, auditErrorMessage(dialErr))
				breakerCounted := shouldRecordBreakerFailure(recordStatusCode, dialErr)
				if breakerCounted {
					retryAfter, _ := retryAfterFromError(dialErr)
					balancer.RecordRuntimeFailure(channel.ID, usedKey.ID, item.ModelName, recordStatusCode, span.Duration(), retryAfter)
					balancer.RecordFailureWithStatus(channel.ID, usedKey.ID, item.ModelName, recordStatusCode)
				}
				channelStats := model.StatsMetrics{WaitTime: span.Duration().Milliseconds()}
				if breakerCounted {
					channelStats.RequestFailed = 1
				}
				op.StatsChannelUpdate(channel.ID, channelStats)
				log.Warnf("realtime upstream dial failed: channel=%s model=%s status=%d err=%v", channel.Name, item.ModelName, recordStatusCode, dialErr)
				lastErr = fmt.Errorf("channel %s failed: %w", channel.Name, dialErr)
				if !shouldTryNextChannelKey(recordStatusCode) {
					break
				}
				continue
			}

			// 上游握手成功，才接受客户端升级。
			clientConn, upErr := grokRealtimeUpgrader(c.Request).Upgrade(c.Writer, c.Request, nil)
			if upErr != nil {
				// Upgrade 失败时 gorilla 已经写了 HTTP 错误。这不是上游的错，不记熔断。
				finishRuntimeAttempt()
				_ = upstream.Close()
				span.End(model.AttemptFailed, http.StatusBadRequest, "client websocket upgrade failed")
				metrics.Save(ctx, false, fmt.Errorf("client websocket upgrade failed: %w", upErr), append(allAttempts, iter.Attempts()...))
				return
			}

			session := runGrokRealtimeSession(ctx, clientConn, upstream, metrics)
			finishRuntimeAttempt()
			usedAt := time.Now().Unix()

			metrics.ActualModel = item.ModelName
			applyGrokRealtimeCost(metrics, item.ModelName, session)
			metrics.ResponseContent = session.logContent()

			costDelta := metrics.Stats.InputCost + metrics.Stats.OutputCost
			op.ChannelKeyRecordUse(usedKey, http.StatusSwitchingProtocols, usedAt, costDelta)
			span.End(model.AttemptSuccess, http.StatusSwitchingProtocols, "")
			balancer.RecordRuntimeSuccess(channel.ID, usedKey.ID, item.ModelName, balancer.AttemptRuntimeMetrics{
				Duration:     span.Duration(),
				FirstToken:   firstTokenDurationSince(metrics.FirstToken, span.StartedAt()),
				OutputTokens: metrics.Stats.OutputToken,
				Stream:       true,
			})
			op.StatsChannelUpdate(channel.ID, model.StatsMetrics{WaitTime: span.Duration().Milliseconds(), RequestSuccess: 1})
			balancer.RecordSuccess(channel.ID, usedKey.ID, item.ModelName)
			if stickyEnabled {
				balancer.SetStickyWithSessionKey(apiKeyID, requestModel, clientSession.Key, channel.ID, usedKey.ID)
			}
			// 会话一旦建立就按成功落账：会话中途断开时，前面已交付的音频/文本同样要计费。
			// 异常关闭的原因写进 ResponseContent，便于排查。
			metrics.Save(ctx, true, nil, append(allAttempts, iter.Attempts()...))
			return
		}
	}

	allAttempts = append(allAttempts, iter.Attempts()...)
	if shouldReturnToOriginalGroup(routeResult, triedReturnGroup) {
		triedReturnGroup = true
		fallbackGroup, err := op.GroupGetEnabledMap(requestModel, ctx)
		if err != nil {
			lastErr = err
		} else {
			fallbackGroup = enrichGroupForSmartRouting(ctx, fallbackGroup, true)
			stickyEnabled = routeStickyEnabled(fallbackGroup.Mode, clientSession.Source)
			fallbackIter := balancer.NewIteratorWithSession(fallbackGroup, apiKeyID, requestModel, clientSession.Key, stickyEnabled)
			if fallbackIter.Len() > 0 {
				iter = fallbackIter
				goto runIterator
			}
		}
	}

	finalErr := lastErr
	if finalErr == nil {
		finalErr = routeSelectionErrorFromAttempts(allAttempts)
	}
	metrics.Save(ctx, false, finalErr, allAttempts)
	errStatus, errCode, errMessage := relayErrorResponse(finalErr)
	resp.ErrorWithCode(c, errStatus, errCode, errMessage)
}

// grokRealtimeAllowsChannel：OpenAI 线协议的渠道（xAI 本身就是 OpenAI 兼容 base URL）。
// 自定义完整 URL 的 chat 渠道没有 base URL 可拼 /v1/realtime；embedding 渠道不承接对话。
func grokRealtimeAllowsChannel(channelType outbound.OutboundType) bool {
	switch channelType {
	case outbound.OutboundTypeOpenAIChat, outbound.OutboundTypeOpenAIResponse:
		return true
	default:
		return false
	}
}

// grokRealtimeTargetURL 把渠道 base URL 拼成 ws(s)://…/v1/realtime，并带上 model 与客户端其余查询参数
// （例如 xAI 会话续接用的 conversation_id）。
func grokRealtimeTargetURL(channel *model.Channel, upstreamModel string, clientQuery url.Values) (string, error) {
	httpURL, err := xurl.JoinOpenAIPath(channel.GetBaseUrl(), "/v1/realtime")
	if err != nil {
		return "", err
	}
	u, err := url.Parse(httpURL)
	if err != nil {
		return "", err
	}
	switch strings.ToLower(u.Scheme) {
	case "https", "wss":
		u.Scheme = "wss"
	case "http", "ws":
		u.Scheme = "ws"
	default:
		return "", fmt.Errorf("unsupported base url scheme %q for realtime", u.Scheme)
	}
	q := u.Query()
	for k, values := range clientQuery {
		if strings.EqualFold(k, "model") {
			continue
		}
		for _, v := range values {
			q.Add(k, v)
		}
	}
	q.Set("model", upstreamModel)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func dialGrokRealtimeUpstream(ctx context.Context, c *gin.Context, channel *model.Channel, channelKey string, upstreamModel string) (*websocket.Conn, int, error) {
	target, err := grokRealtimeTargetURL(channel, upstreamModel, c.Request.URL.Query())
	if err != nil {
		return nil, 0, fmt.Errorf("failed to build realtime url: %w", err)
	}
	proxyURL, err := helper.ChannelProxyURL(channel)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to resolve channel proxy: %w", err)
	}
	header := http.Header{}
	header.Set("Authorization", "Bearer "+channelKey)
	for _, h := range channel.CustomHeader {
		if grokRealtimeHandshakeHeader(h.HeaderKey) {
			continue
		}
		header.Set(h.HeaderKey, h.HeaderValue)
	}
	if header.Get("User-Agent") == "" {
		header.Set("User-Agent", genericUAForChannel(channel))
	}
	dialCtx, cancel := context.WithTimeout(ctx, grokRealtimeDialTimeout)
	defer cancel()
	conn, httpResp, err := grokRealtimeDialer(proxyURL).DialContext(dialCtx, target, header)
	if err != nil {
		if httpResp != nil {
			body := []byte(nil)
			if httpResp.Body != nil {
				body, _ = io.ReadAll(io.LimitReader(httpResp.Body, upstreamErrorBodyLimit))
				_ = httpResp.Body.Close()
			}
			if httpResp.StatusCode >= 400 {
				return nil, httpResp.StatusCode, newUpstreamError(httpResp.StatusCode, body)
			}
			return nil, httpResp.StatusCode, fmt.Errorf("realtime upstream handshake failed (status %d): %w", httpResp.StatusCode, err)
		}
		return nil, 0, fmt.Errorf("realtime upstream dial failed: %w", err)
	}
	conn.SetReadLimit(grokRealtimeReadLimit)
	return conn, http.StatusSwitchingProtocols, nil
}

// grokRealtimeHandshakeHeader：这些头由拨号器自己生成，渠道自定义头里出现会让 gorilla 直接报错。
func grokRealtimeHandshakeHeader(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "upgrade", "connection", "sec-websocket-key", "sec-websocket-version",
		"sec-websocket-extensions", "sec-websocket-protocol", "host", "content-length":
		return true
	}
	return false
}

// grokRealtimeUpgrader 回一个客户端提出过的子协议（浏览器发了子协议、服务端却不回，
// 握手会被浏览器判失败）。优先 "realtime"（OpenAI SDK / new-api 口径）；
// 只发了带 key 的那一个（xAI 浏览器写法 xai-client-secret.<token>）时就回它本身。
func grokRealtimeUpgrader(r *http.Request) *websocket.Upgrader {
	offered := websocket.Subprotocols(r)
	var chosen []string
	for _, p := range offered {
		if p == "realtime" {
			chosen = []string{p}
			break
		}
	}
	if chosen == nil && len(offered) > 0 {
		chosen = []string{offered[0]}
	}
	return &websocket.Upgrader{
		ReadBufferSize:  32 << 10,
		WriteBufferSize: 32 << 10,
		Subprotocols:    chosen,
		// 与 new-api 一致不校验 Origin：鉴权靠 API key，不靠 cookie，跨站页面拿不到用户的 key。
		CheckOrigin: func(*http.Request) bool { return true },
	}
}

// grokRealtimeSession 汇总一次会话里计费与日志需要的事实。
type grokRealtimeSession struct {
	Started        time.Time
	Duration       time.Duration
	ClientMessages int64
	UpstreamMsgs   int64
	AudioObserved  bool
	ResponsesDone  int64
	Usage          imagesUsage
	UsageSeen      bool
	CloseCode      int
	CloseSide      string
	CloseText      string
}

func (s *grokRealtimeSession) logContent() string {
	type sessionLog struct {
		Protocol        string       `json:"protocol"`
		DurationMS      int64        `json:"duration_ms"`
		ClientMessages  int64        `json:"client_messages"`
		UpstreamMessage int64        `json:"upstream_messages"`
		ResponsesDone   int64        `json:"responses_done"`
		AudioObserved   bool         `json:"audio_observed"`
		Usage           *imagesUsage `json:"usage,omitempty"`
		ClosedBy        string       `json:"closed_by,omitempty"`
		CloseCode       int          `json:"close_code,omitempty"`
		CloseReason     string       `json:"close_reason,omitempty"`
		Note            string       `json:"note"`
	}
	obj := sessionLog{
		Protocol:        grokRealtimeProtocol,
		DurationMS:      s.Duration.Milliseconds(),
		ClientMessages:  s.ClientMessages,
		UpstreamMessage: s.UpstreamMsgs,
		ResponsesDone:   s.ResponsesDone,
		AudioObserved:   s.AudioObserved,
		ClosedBy:        s.CloseSide,
		CloseCode:       s.CloseCode,
		CloseReason:     truncateString(s.CloseText, 256),
		Note:            "realtime events and audio omitted for storage",
	}
	if s.UsageSeen {
		u := s.Usage
		obj.Usage = &u
	}
	b, _ := json.Marshal(obj)
	return string(b)
}

func grokRealtimeRequestLog(requestModel string, query url.Values) string {
	q := url.Values{}
	for k, v := range query {
		q[k] = v
	}
	b, _ := json.Marshal(map[string]any{
		"protocol":  grokRealtimeProtocol,
		"transport": "websocket",
		"model":     requestModel,
		"query":     q.Encode(),
	})
	return string(b)
}

// grokRealtimeEvent 只解析计费需要的字段。
type grokRealtimeEvent struct {
	Type     string `json:"type"`
	Delta    string `json:"delta"`
	Audio    string `json:"audio"`
	Response *struct {
		Usage *grokRealtimeUsage `json:"usage"`
	} `json:"response"`
}

// grokRealtimeUsage 是 OpenAI Realtime response.done 的 usage 形态（xAI 兼容）。
type grokRealtimeUsage struct {
	TotalTokens       int `json:"total_tokens"`
	InputTokens       int `json:"input_tokens"`
	OutputTokens      int `json:"output_tokens"`
	InputTokenDetails struct {
		CachedTokens int `json:"cached_tokens"`
		TextTokens   int `json:"text_tokens"`
		AudioTokens  int `json:"audio_tokens"`
	} `json:"input_token_details"`
}

// grokRealtimeEventHasAudio 同 sub2api grokRealtimeEventHasAudio：type 含 audio、不是转写、
// 且 audio/delta 非空。二进制帧（audio transport=binary）在调用处直接算音频。
func grokRealtimeEventHasAudio(ev *grokRealtimeEvent) bool {
	t := strings.ToLower(ev.Type)
	if !strings.Contains(t, "audio") || strings.Contains(t, "transcript") {
		return false
	}
	return strings.TrimSpace(ev.Delta) != "" || strings.TrimSpace(ev.Audio) != ""
}

// runGrokRealtimeSession 双向转发直到任一端关闭，原样保留消息类型（文本/二进制）。
// 每个连接只有一个 goroutine 写数据帧；收尾的 close 帧走 WriteControl（gorilla 允许并发）。
func runGrokRealtimeSession(ctx context.Context, client, upstream *websocket.Conn, metrics *rawProtocolRelayMetrics) *grokRealtimeSession {
	client.SetReadLimit(grokRealtimeReadLimit)
	s := &grokRealtimeSession{Started: time.Now()}
	var (
		clientMsgs, upstreamMsgs, responsesDone atomic.Int64
		audio                                   atomic.Bool
		usageMu                                 sync.Mutex
		firstOnce                               sync.Once
	)

	type endReason struct {
		side string
		err  error
	}
	done := make(chan endReason, 2)

	// 上游 → 客户端
	go func() {
		for {
			mt, msg, err := upstream.ReadMessage()
			if err != nil {
				done <- endReason{"upstream", err}
				return
			}
			upstreamMsgs.Add(1)
			firstOnce.Do(func() { metrics.SetFirstTokenTime(time.Now()) })
			if mt == websocket.BinaryMessage {
				audio.Store(true)
			} else if mt == websocket.TextMessage {
				var ev grokRealtimeEvent
				if json.Unmarshal(msg, &ev) == nil {
					if grokRealtimeEventHasAudio(&ev) {
						audio.Store(true)
					}
					if ev.Type == "response.done" {
						responsesDone.Add(1)
						if ev.Response != nil && ev.Response.Usage != nil {
							u := ev.Response.Usage
							usageMu.Lock()
							s.UsageSeen = true
							s.Usage.InputTokens += u.InputTokens
							s.Usage.OutputTokens += u.OutputTokens
							s.Usage.TotalTokens += u.TotalTokens
							if u.InputTokenDetails.CachedTokens > 0 {
								if s.Usage.InputTokensDetails == nil {
									s.Usage.InputTokensDetails = &imagesInputTokensDetails{}
								}
								s.Usage.InputTokensDetails.CachedTokens += u.InputTokenDetails.CachedTokens
							}
							usageMu.Unlock()
						}
					}
				}
			}
			if err := client.WriteMessage(mt, msg); err != nil {
				done <- endReason{"client", err}
				return
			}
		}
	}()

	// 客户端 → 上游
	go func() {
		for {
			mt, msg, err := client.ReadMessage()
			if err != nil {
				done <- endReason{"client", err}
				return
			}
			clientMsgs.Add(1)
			if mt == websocket.BinaryMessage {
				audio.Store(true)
			} else if mt == websocket.TextMessage && !audio.Load() {
				var ev grokRealtimeEvent
				if json.Unmarshal(msg, &ev) == nil && grokRealtimeEventHasAudio(&ev) {
					audio.Store(true)
				}
			}
			if err := upstream.WriteMessage(mt, msg); err != nil {
				done <- endReason{"upstream", err}
				return
			}
		}
	}()

	var first endReason
	select {
	case first = <-done:
	case <-ctx.Done():
		first = endReason{"server", ctx.Err()}
	}

	// 把关闭原因原样转给另一端，然后两边都关掉；另一个泵会因读错误退出。
	code, text := websocket.CloseNormalClosure, ""
	var ce *websocket.CloseError
	if errors.As(first.err, &ce) {
		code, text = ce.Code, ce.Text
	} else if first.err != nil {
		code, text = websocket.CloseGoingAway, "peer connection lost"
	}
	if code == websocket.CloseNoStatusReceived || code == websocket.CloseAbnormalClosure || code == websocket.CloseTLSHandshake {
		// 这几个保留码不能出现在 close 帧里。
		code = websocket.CloseGoingAway
	}
	deadline := time.Now().Add(grokRealtimeCloseGrace)
	msg := websocket.FormatCloseMessage(code, text)
	switch first.side {
	case "upstream":
		_ = client.WriteControl(websocket.CloseMessage, msg, deadline)
	case "client":
		_ = upstream.WriteControl(websocket.CloseMessage, msg, deadline)
	default:
		_ = client.WriteControl(websocket.CloseMessage, msg, deadline)
		_ = upstream.WriteControl(websocket.CloseMessage, msg, deadline)
	}
	_ = upstream.Close()
	_ = client.Close()
	<-done // 等另一个泵退出，保证下面读到的计数是终值

	s.Duration = time.Since(s.Started)
	s.ClientMessages = clientMsgs.Load()
	s.UpstreamMsgs = upstreamMsgs.Load()
	s.ResponsesDone = responsesDone.Load()
	s.AudioObserved = audio.Load()
	s.CloseSide = first.side
	if ce != nil {
		s.CloseCode, s.CloseText = ce.Code, ce.Text
	} else if first.err != nil {
		s.CloseText = errSafeMessage(first.err)
	}
	return s
}

// applyGrokRealtimeCost 见文件头"计费"一节。
func applyGrokRealtimeCost(metrics *rawProtocolRelayMetrics, actualModel string, s *grokRealtimeSession) {
	if metrics == nil || s == nil {
		return
	}
	if s.UsageSeen {
		// 先把 token 落账（日志里能看到用量），按分钟计费时再覆盖金额。
		metrics.SetUsage(actualModel, s.Usage)
	}
	snapshot := metrics.currentBillingSnapshot(actualModel)
	modelPrice := price.GetLLMPrice(snapshot.BillingModelName)
	if modelPrice == nil || modelPrice.PerCall <= 0 {
		if !s.UsageSeen {
			log.Warnf("realtime cost: model %q has no per_call (per-minute) price and upstream reported no usage, billing 0 for this session", snapshot.BillingModelName)
		}
		return
	}
	minutes := grokRealtimeBillableMinutes(s)
	multiplier := snapshot.FinalMultiplier
	if multiplier <= 0 {
		multiplier = 1
	}
	snapshot.FinalInputCost, snapshot.FinalCacheReadCost, snapshot.FinalCacheWriteCost = 0, 0, 0
	snapshot.FinalOutputCost = modelPrice.PerCall * minutes * multiplier
	metrics.BillingSnapshot = snapshot
	metrics.Stats.InputCost = 0
	metrics.Stats.OutputCost = snapshot.FinalOutputCost
}

// grokRealtimeBillableMinutes：会话里没出现过音频（例如只握手就断、或纯文本会话）→ 0 分钟，
// 与 sub2api grokRealtimeBillingResult 的 audioObserved 闸门一致；否则按实际时长（分钟，小数）。
func grokRealtimeBillableMinutes(s *grokRealtimeSession) float64 {
	if s == nil || !s.AudioObserved || s.Duration <= 0 {
		return 0
	}
	return math.Round(s.Duration.Minutes()*1e6) / 1e6
}
