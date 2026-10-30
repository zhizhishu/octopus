package modeltest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/relay"
	transformermodel "github.com/zhizhishu/octopus/internal/transformer/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
	"github.com/zhizhishu/octopus/internal/utils/xredact"
	"github.com/zhizhishu/octopus/internal/utils/xurl"
)

const (
	// 图片探针的固定请求形状，照 new-api controller/channel-test.go：
	// Prompt "a cute cat"、N 1、Size "1024x1024"。
	imageTestPrompt = "a cute cat"
	imageTestN      = 1
	imageTestSize   = "1024x1024"
	// imageTestEndpointPath 是传给 relay.ImagesHandler 的 endpoint（不含 /v1 前缀），
	// 与生产路由 /v1/images/generations 同一个入口。
	imageTestEndpointPath = "/images/generations"
)

// isImageTestEndpoint 判断端点名是否为图片端点。空值按默认端点（openai_chat）处理，
// 与 isEmbeddingTestEndpoint 的语义一致。
func isImageTestEndpoint(endpoint string) bool {
	normalized, err := normalizeEndpoint(endpoint)
	if err != nil {
		return false
	}
	return normalized.name == imageGenerationTestEndpointName
}

// imageTestOutcome 是一次图片渠道测试的结果载体。
type imageTestOutcome struct {
	status       int
	stream       bool
	upstreamPath string
	keyID        int
	errorCode    string
	response     *transformermodel.InternalLLMResponse
	err          error
}

// tryImageChannel 是图片端点的渠道测试分支。
//
// 与 chat / 嵌入那一套最大的区别：图片**不**走本包"自建 InternalLLMRequest →
// 出站适配器 transform → 手发"的路径，而是把请求交给 relay.ImagesHandler —— 也就是
// 生产 /v1/images/generations 真正在用的那个入口和转发实现。测的就是用的那条路，
// 上游看到的是同一份 URL / header / body。
func (r *modelRunner) tryImageChannel(ctx context.Context, channel *dbmodel.Channel, upstreamModel string) bool {
	channelName := channelDisplayName(channel)

	// 准入：只允许 relay 真的会把图片流量调度过去的渠道类型。图片没有能力位，
	// 判据就是 ImagesHandler 调度时用的同一个函数（relay.ChannelTypeSupportsImages）。
	// 不支持的类型明确跳过并记录原因，不硬塞一个假端点换一条看不懂的报错。
	if channel.Type == outbound.OutboundTypeCustomOpenAIChat {
		if _, ok, err := xurl.ParseEndpointOverride(channel.OpenAIImagesPath); err != nil || !ok {
			r.recordAttempt(channel.ID, 0, channelName, upstreamModel, "", dbmodel.AttemptSkipped, 0, 0, relay.CustomChannelNoFullURLReason)
			return false
		}
	}
	if !relay.ChannelTypeSupportsImages(channel.Type) {
		r.recordAttempt(channel.ID, 0, channelName, upstreamModel, "", dbmodel.AttemptSkipped, 0, 0,
			fmt.Sprintf("channel type %d does not support the images endpoint", channel.Type))
		return false
	}
	if strings.TrimSpace(channel.GetBaseUrl()) == "" {
		r.recordAttempt(channel.ID, 0, channelName, upstreamModel, "", dbmodel.AttemptSkipped, 0, 0, "channel has no base url")
		return false
	}
	// 这里**不能**先探一次 key 再转发：GetAvailableChannelKeys 在"所有 key 都在冷却"时
	// 会退回冷却中的 key，但那条兜底受 admitCooldownProbe 限流——每渠道每
	// ChannelKeyProbeInterval(1s) 只发一张探测名额。先探一次就把名额吃掉，紧接着转发
	// 那次拿到空集合直接 503，请求根本没发出上游；顺带还偷走了该渠道这一秒的生产探测名额。
	// 所以"有没有 key"只由转发分支判一次，那边已经会返回 "no available key"。

	started := time.Now()
	outcome := r.forwardImageTest(ctx, channel, upstreamModel)
	duration := int(time.Since(started).Milliseconds())

	r.result.UpstreamPath = outcome.upstreamPath
	r.result.IsStream = outcome.stream
	if outcome.err != nil {
		r.recordAttempt(channel.ID, outcome.keyID, channelName, upstreamModel, outcome.upstreamPath, dbmodel.AttemptFailed, outcome.status, duration, outcome.err.Error())
		r.result.Error = xredact.Secrets(outcome.err.Error())
		r.result.StatusCode = outcome.status
		if outcome.errorCode != "" {
			r.result.ErrorCode = outcome.errorCode
		}
		return false
	}

	// 成功收尾复用 succeed：渠道名 / 上游模型 / 预览 / token 用量一次写齐，避免图片
	// 分支再养一份与其它端点不一致的收尾逻辑。key 只用到 ID（转发内部自己挑 key），
	// 所以这里给一个只带 ID 的 ChannelKey。
	r.succeed(channel, dbmodel.ChannelKey{ID: outcome.keyID}, upstreamModel, outcome.status, duration, outcome.response)
	return true
}

// forwardImageTest 造一个 gin 上下文，设置好 ImagesHandler 依赖的四个键
// （api_key_id / user_id / request_ip / supported_models），再把这次转发钉在
// 目标渠道上，交给真链路去发。
func (r *modelRunner) forwardImageTest(ctx context.Context, channel *dbmodel.Channel, upstreamModel string) imageTestOutcome {
	// 图片探针**固定非流式**，不读 r.request.Stream。
	//
	// 理由（宁可少测一种模式，也不许测出一条假红）：OpenAI Images 的流式响应没有
	// 统一的判定口径——gpt-image 系列推的是 image_generation.partial_image 事件，
	// 别的厂商干脆只在最后一帧给 data[]，而 relay 的 Images 流式分支（proxySSE）
	// 是原样透传。若在这里按流式打，结果解析只能猜形状，猜错就会把一条好渠道报成
	// "上游没给图片数据"。所以探针与 new-api 保持一致：一条普通的非流式 ImageRequest。
	// 流式图片渠道要测，得先定义清楚 SSE 判定口径，那是另一轮的事（已写进交接报告）。
	stream := false
	prompt := strings.TrimSpace(r.request.Prompt)
	if prompt == "" || prompt == defaultPromptSentinel {
		prompt = imageTestPrompt
	}
	body, err := json.Marshal(map[string]any{
		"model":  upstreamModel,
		"prompt": prompt,
		"n":      imageTestN,
		"size":   imageTestSize,
	})
	if err != nil {
		return imageTestOutcome{stream: stream, err: fmt.Errorf("build images request: %w", err)}
	}

	recorder := newImageTestRecorder()
	c, _ := gin.CreateTestContext(recorder)
	// URL 里的主机没有意义：ImagesHandler 用渠道的 base_url 拼上游地址，这里只是给
	// 真链路一个形状正确的下游请求。路径必须与生产路由一致。
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://octopus.internal/v1/images/generations", bytes.NewReader(body))
	if err != nil {
		return imageTestOutcome{stream: stream, err: fmt.Errorf("build images test request: %w", err)}
	}
	request.Header.Set("Content-Type", "application/json")
	c.Request = request
	// 渠道测试没有 API key 归属：两个 id 置 0；supported_models 留空 = 不限
	// （测的是"这个渠道行不行"，不叠 API key 的白名单）。
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "")
	c.Set("supported_models", "")
	// 钉住目标渠道：绝不允许这次测试跑到别的渠道上。
	relay.WithImagesChannelTest(c, channel)

	relay.ImagesHandler(imageTestEndpointPath, c)

	outcome := imageTestOutcome{
		status:       recorder.StatusCode(),
		stream:       stream,
		upstreamPath: relay.ImagesChannelTestUpstreamPath(c),
		keyID:        relay.ImagesChannelTestChannelKeyID(c),
	}
	responseBody := recorder.Body()
	if recorder.Overflow() {
		outcome.err = fmt.Errorf("upstream response exceeded %d bytes; refusing to buffer it in the model test", imageTestRecorderMaxBytes)
		return outcome
	}
	if outcome.status < 200 || outcome.status >= 300 {
		outcome.errorCode = imageTestErrorCode(responseBody)
		outcome.err = fmt.Errorf("upstream status %d: %s", outcome.status, imageTestErrorMessage(responseBody))
		return outcome
	}
	// 状态码是 2xx 不等于这次转发成功了：响应头/正文可能已经写出，随后上游读取失败
	// （典型是被截断的 body）。录下来的 JSON 看着完整，只有真链路手里的错误知道真相。
	// 先看这个错误，再解析 body——顺序反过来就会把一次失败的转发报成成功。
	if forwardErr := relay.ImagesChannelTestForwardErr(c); forwardErr != nil {
		outcome.err = fmt.Errorf("upstream read failed after response was written: %w", forwardErr)
		return outcome
	}
	response, err := imageTestResponseFromBody(responseBody)
	if err != nil {
		outcome.err = err
		return outcome
	}
	outcome.response = response
	return outcome
}

// imageTestResponseFromBody 把下游收到的 OpenAI Images 响应收成一份内部响应，
// 只为了复用统一的 succeed 收尾：Content 里放"几张图 + url/b64_json"的摘要，
// Usage 里带上游给的 token 用量。图片本身（b64/URL）不进预览，别把 base64 写进日志。
func imageTestResponseFromBody(body []byte) (*transformermodel.InternalLLMResponse, error) {
	var payload struct {
		Data []struct {
			URL     string `json:"url"`
			B64JSON string `json:"b64_json"`
		} `json:"data"`
		Usage *struct {
			InputTokens      int64 `json:"input_tokens"`
			OutputTokens     int64 `json:"output_tokens"`
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("parse images response: %w", err)
	}
	if len(payload.Data) == 0 {
		return nil, fmt.Errorf("upstream returned no image data")
	}
	urlCount, b64Count := 0, 0
	for _, item := range payload.Data {
		switch {
		case strings.TrimSpace(item.B64JSON) != "":
			b64Count++
		case strings.TrimSpace(item.URL) != "":
			urlCount++
		}
	}
	if urlCount+b64Count == 0 {
		return nil, fmt.Errorf("upstream returned no image data")
	}
	preview := fmt.Sprintf("%d image(s)", len(payload.Data))
	switch {
	case b64Count == len(payload.Data):
		preview += " (b64_json)"
	case urlCount == len(payload.Data):
		preview += " (url)"
	}

	message := &transformermodel.Message{Role: "assistant"}
	message.Content.Content = &preview
	response := &transformermodel.InternalLLMResponse{
		Choices: []transformermodel.Choice{{Index: 0, Message: message}},
	}
	if payload.Usage != nil {
		inputTokens := payload.Usage.InputTokens
		if inputTokens == 0 {
			inputTokens = payload.Usage.PromptTokens
		}
		outputTokens := payload.Usage.OutputTokens
		if outputTokens == 0 {
			outputTokens = payload.Usage.CompletionTokens
		}
		totalTokens := payload.Usage.TotalTokens
		if totalTokens == 0 {
			totalTokens = inputTokens + outputTokens
		}
		response.Usage = &transformermodel.Usage{
			PromptTokens:     inputTokens,
			CompletionTokens: outputTokens,
			TotalTokens:      totalTokens,
		}
	}
	return response, nil
}

// imageTestErrorMessage 取下游错误体里的人话。真链路出错时写的是 relay 自己的
// {code,error_code,message} 信封；万一上游原始错误体被透传出来，则退到 OpenAI 的
// {"error":{"message":...}} 形状，再不行就把 body 截一段。
func imageTestErrorMessage(body []byte) string {
	var payload struct {
		Message string `json:"message"`
		Error   struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err == nil {
		if message := strings.TrimSpace(payload.Message); message != "" {
			return xredact.Secrets(trimErrorText(message))
		}
		if message := strings.TrimSpace(payload.Error.Message); message != "" {
			return xredact.Secrets(trimErrorText(message))
		}
	}
	if text := strings.TrimSpace(string(body)); text != "" {
		return xredact.Secrets(trimErrorText(text))
	}
	return "images upstream failed without a response body"
}

// imageTestErrorCode 取错误码：先认 relay 信封的 error_code，再认 OpenAI 的 error.code。
func imageTestErrorCode(body []byte) string {
	var payload struct {
		ErrorCode string `json:"error_code"`
		Error     struct {
			Code any `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	if code := strings.TrimSpace(payload.ErrorCode); code != "" {
		return code
	}
	return stringify(payload.Error.Code)
}

// imageTestRecorder 是渠道测试用的最小 http.ResponseWriter：把真链路写给客户端的
// 响应（状态码 + body）收进内存。用它而不是 net/http/httptest，是为了不让测试专用
// 包出现在生产代码路径上；gin 会把它当普通 ResponseWriter 包进自己的 writermem。
//
// **必须设上限**：生产实现是按块转发、只扫 usage 的，内存占用与响应大小无关；探针却把
// 整份 body 收进内存再整体 json.Unmarshal，一份超大 2xx 就能吃掉进程内存，并发测试还会放大。
// 超限时立刻停下并记录错误，由调用方判红——宁可报"响应过大"，也不许把服务拖垮。
const imageTestRecorderMaxBytes = 16 << 20 // 16 MiB：足够容纳 1024x1024 b64 图片，又不会失控

type imageTestRecorder struct {
	header      http.Header
	body        bytes.Buffer
	statusCode  int
	wroteHeader bool
	overflow    bool
}

func newImageTestRecorder() *imageTestRecorder {
	return &imageTestRecorder{header: http.Header{}}
}

func (r *imageTestRecorder) Header() http.Header { return r.header }

func (r *imageTestRecorder) WriteHeader(statusCode int) {
	if r.wroteHeader {
		return
	}
	r.statusCode = statusCode
	r.wroteHeader = true
}

func (r *imageTestRecorder) Write(data []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	if r.body.Len()+len(data) > imageTestRecorderMaxBytes {
		r.overflow = true
		// 返回错误让写入方停下；已收下的部分保留，方便报错时给出上下文。
		return 0, fmt.Errorf("image test response exceeds %d bytes", imageTestRecorderMaxBytes)
	}
	return r.body.Write(data)
}

// Overflow 表示本次响应超过上限、已被截断。
func (r *imageTestRecorder) Overflow() bool { return r.overflow }

// StatusCode 返回下游状态码；真链路没显式写过状态码时按 net/http 的语义算 200。
func (r *imageTestRecorder) StatusCode() int {
	if r.statusCode == 0 {
		return http.StatusOK
	}
	return r.statusCode
}

// Body 返回真链路写给下游的响应体（SSE 流式时是原始 SSE 文本）。
func (r *imageTestRecorder) Body() []byte { return r.body.Bytes() }
