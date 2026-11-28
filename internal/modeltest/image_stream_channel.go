package modeltest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/relay"
	transformermodel "github.com/zhizhishu/octopus/internal/transformer/model"
)

// ── next-1129 第 10 件：流式出图检测 ──────────────────────────────────────────
//
// 需求（用户 2026-11-29 02:56 拍板）：检测页多一个「流式出图」类型，走 SSE 累积出图
// 事件，记首图时间/总时间/尺寸。**先只做模拟上游**（httptest 假上游推事件序列），
// 真渠道（线上 vertex 最多 3 张）是用户上线后的动作——本文件里没有任何真实渠道地址、
// 没有写死的模型名、没有任何"额度上限"实现，全部经过 relay.ImagesHandler 这条生产
// 转发链路（image_channel.go 的 forwardImageTest），走哪条渠道由渠道配置决定。
//
// 为什么单独一个端点、不把老图片端点改成跟随 stream 开关：
//  1. 非流式出图的判定是「整份 JSON 里有没有 data[] 图片」，流式出图的判定是
//     「SSE 帧里累积出几张图、首图多久到」，两套口径不能共用一套解析；
//  2. 老端点的请求体、判定与预览文案是既有检测类型的行为，接了流式就必须在同一个
//     函数里分叉，容易把老类型一起改坏（本批要求"不改动现有检测类型的行为"）；
//  3. 用户点哪个测哪个：想测非流式就点非流式，不会因为上游恰好推了 SSE 就把一次
//     非流式检测判成"没图"。
//
// 判定口径（本轮写死在这里；改口径必须同时改 image_stream_channel_test.go）：
//  1. 只解析 `data: {json}` 帧。`event:`、`:` 注释帧、空行只当分隔符，`data:`
//     后可有可无一个空格（SSE 规范两种写法都合法）。
//  2. 一帧里出现非空 `b64_json` 或非空 `url`，算"一张图"；
//     一帧里 `data` 是数组时，数组里每个带 `b64_json`/`url` 的项各算一张
//     （OpenAI Images 非流式形状顺带被兼容）；
//     一帧里 `inlineData.data` / `inline_data.data` 非空、或
//     `candidates[].content.parts[].inlineData.data` 非空，也算一张
//     （Gemini/Vertex 家族推图的形状）。
//  3. **首图时间** = 第一次命中第 2 条的帧到达的时刻；**总时间** = 响应流结束
//     （relay.ImagesHandler 返回）的时刻，两者都相对本次上游请求开始。
//  4. **尺寸**优先取事件里的 `size` 字段；没有就回落到请求里发的 size。
//  5. 判定成功要求 ≥1 张图。只有 `image_generation.completed` 却没有图片字节不算
//     出图（那是"上游说完成了但没给图"，必须报红，不许当成功）。
//  6. 单行上限 8 MiB：一张 1024x1024 PNG 的 base64 大约 1–2 MiB，留足余量；超过就
//     停手报错，宁可报"上游数据过大"也不把进程内存拖垮。
const (
	// imageStreamMaxLineBytes 是单条 SSE 行的上限（含 data: 前缀）。
	imageStreamMaxLineBytes = 8 << 20
	// imageStreamDiagLimit 是留作错误诊断的原始字节上限：错误信封很小，8 KiB 足够，
	// 而且**绝不**把图片 base64 抄一份进内存。
	imageStreamDiagLimit = 8 << 10
)

// imageStreamJudgeSummary 是判定口径的一句话版本，进错误文案，让人在日志里就能看懂
// 为什么被判红。
const imageStreamJudgeSummary = "count a frame as one image when it carries a non-empty b64_json / url / inlineData.data; success needs at least one image"

// isStreamImageTestEndpoint 判断端点名是不是「流式出图」端点（含别名，归一化规则
// 与 normalizeEndpoint 一致）。
func isStreamImageTestEndpoint(endpoint string) bool {
	normalized, err := normalizeEndpoint(endpoint)
	if err != nil {
		return false
	}
	return normalized.name == streamImageGenerationTestEndpointName
}

// imageStreamUsage 是流式出图事件里能抄到的 token 用量。字段名两套都认：
// OpenAI Responses/Images 用 input_tokens/output_tokens，chat 家族用 prompt_tokens/
// completion_tokens。
type imageStreamUsage struct {
	inputTokens  int64
	outputTokens int64
	totalTokens  int64
}

// imageStreamSummary 是一次流式出图的累积结果。
type imageStreamSummary struct {
	images   int
	size     string
	firstAt  time.Time
	usage    *imageStreamUsage
	rawBadge int // 收到的 data 帧数量，只用于诊断输出
}

// imageStreamTestRecorder 把真链路写给下游的 SSE 原样收下并当场判图。
//
// 它实现 http.ResponseWriter，所以 gin 把它当普通 ResponseWriter 用：转发完全不变，
// 只是"写下去"的字节顺手过一遍增量 SSE 扫描器。**不缓存图片**：每帧解析完就丢，
// 只留计数/首图时刻/尺寸与 8 KiB 诊断前缀。
type imageStreamTestRecorder struct {
	header      http.Header
	statusCode  int
	wroteHeader bool
	startedAt   time.Time

	lineBuf  []byte
	overflow bool

	summary imageStreamSummary

	diag      bytes.Buffer
	diagLimit int
}

func newImageStreamTestRecorder() *imageStreamTestRecorder {
	return &imageStreamTestRecorder{
		header:    http.Header{},
		startedAt: time.Now(),
		diagLimit: imageStreamDiagLimit,
	}
}

func (r *imageStreamTestRecorder) Header() http.Header { return r.header }

// Flush 必须实现：gin 的 responseWriter.Flush() 是
// `w.ResponseWriter.(http.Flusher).Flush()` 的**无检查**类型断言，底层不实现就直接 panic。
// relay 的 SSE 透传（proxySSE）每个事件后都 Flush，所以流式出图的 recorder 必须满足
// http.Flusher；探针没有真下游要推，空实现就是全部语义。
func (r *imageStreamTestRecorder) Flush() {}

func (r *imageStreamTestRecorder) WriteHeader(statusCode int) {
	if r.wroteHeader {
		return
	}
	r.statusCode = statusCode
	r.wroteHeader = true
}

// Write 收下一块下游字节。返回 (len(data), nil) 让转发方的 io.Copy/写入循环按
// 正常成功继续——检测记录**不能**改变转发行为，更不能因为探针的内部上限把一次
// 真实的上游流转成半截。
func (r *imageStreamTestRecorder) Write(data []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	if remaining := r.diagLimit - r.diag.Len(); remaining > 0 {
		if len(data) <= remaining {
			_, _ = r.diag.Write(data)
		} else {
			_, _ = r.diag.Write(data[:remaining])
		}
	}
	r.feed(data)
	return len(data), nil
}

// feed 把新到字节拼成完整行再逐行解析，避免跨块的 data 行被截断（SSE 帧边界与
// 网络块边界无关）。
func (r *imageStreamTestRecorder) feed(data []byte) {
	if len(data) == 0 {
		return
	}
	r.lineBuf = append(r.lineBuf, data...)
	for {
		index := bytes.IndexByte(r.lineBuf, '\n')
		if index < 0 {
			break
		}
		line := r.lineBuf[:index]
		r.lineBuf = r.lineBuf[index+1:]
		r.consumeLine(line)
	}
	if len(r.lineBuf) > imageStreamMaxLineBytes {
		// 一行都超过上限了：停手，别继续吞内存（已解析过的帧照样算数）。
		r.overflow = true
		r.lineBuf = nil
	}
}

func (r *imageStreamTestRecorder) consumeLine(line []byte) {
	line = bytes.TrimSuffix(line, []byte("\r"))
	if len(line) == 0 {
		return
	}
	if line[0] == ':' {
		// SSE 注释帧（keepalive）不携带数据。
		return
	}
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	payload := bytes.TrimSpace(line[len("data:"):])
	if len(payload) == 0 || payload[0] != '{' {
		return
	}
	var event map[string]json.RawMessage
	if err := json.Unmarshal(payload, &event); err != nil {
		return
	}
	r.summary.rawBadge++
	if usage := streamImageEventUsage(event); usage != nil {
		r.summary.usage = usage
	}
	if size := rawJSONStringField(event, "size"); size != "" {
		r.summary.size = size
	}
	if images := streamImageEventImages(event); images > 0 {
		if r.summary.images == 0 {
			r.summary.firstAt = time.Now()
		}
		r.summary.images += images
	}
}

// StatusCode 返回下游状态码；真链路没显式写过状态码时按 net/http 的语义算 200。
func (r *imageStreamTestRecorder) StatusCode() int {
	if r.statusCode == 0 {
		return http.StatusOK
	}
	return r.statusCode
}

// Overflow 表示单行超过上限、后续字节不再解析。
func (r *imageStreamTestRecorder) Overflow() bool { return r.overflow }

// DiagnosticBody 返回响应开头的有界副本（≤ 8 KiB），只给错误信息与错误码解析用。
func (r *imageStreamTestRecorder) DiagnosticBody() []byte { return r.diag.Bytes() }

// FirstImageLatency 是首图相对请求开始的耗时；没有图时为 0。
func (r *imageStreamTestRecorder) FirstImageLatency() time.Duration {
	if r.summary.firstAt.IsZero() {
		return 0
	}
	return r.summary.firstAt.Sub(r.startedAt)
}

// TotalLatency 是自请求开始到当前（调用点即流结束）的耗时。
func (r *imageStreamTestRecorder) TotalLatency() time.Duration {
	return time.Since(r.startedAt)
}

// finishStreamImageTestOutcome 是流式出图分支的收尾：状态码/转发错误/判图三件事，
// 口径与非流式分支（image_channel.go 的 forwardImageTest 后半段）一一对应，只是
// "有没有图"从整份 JSON 的 data[] 换成了 SSE 帧累积。
func finishStreamImageTestOutcome(recorder *imageStreamTestRecorder, c *gin.Context) imageTestOutcome {
	outcome := imageTestOutcome{
		status:       recorder.StatusCode(),
		stream:       true,
		upstreamPath: relay.ImagesChannelTestUpstreamPath(c),
		keyID:        relay.ImagesChannelTestChannelKeyID(c),
	}
	if recorder.Overflow() {
		outcome.err = fmt.Errorf("upstream streamed a single SSE event larger than %d bytes; refusing to buffer it in the model test", imageStreamMaxLineBytes)
		return outcome
	}
	body := recorder.DiagnosticBody()
	if outcome.status < 200 || outcome.status >= 300 {
		outcome.errorCode = imageTestErrorCode(body)
		// 本地渠道闸门终态（relay channel_rate_limited，一次都没打到上游）不是上游
		// 拒收：归因分开写，与非流式分支同一口径。
		if outcome.status == http.StatusTooManyRequests && bytes.Contains(body, []byte("channel_rate_limited")) {
			outcome.err = ErrChannelGateBusy
			return outcome
		}
		outcome.err = fmt.Errorf("upstream status %d: %s", outcome.status, imageTestErrorMessage(body))
		return outcome
	}
	// 状态码 2xx 不等于转发成功：响应头/正文可能已经写出，随后上游连接断了。
	if forwardErr := relay.ImagesChannelTestForwardErr(c); forwardErr != nil {
		outcome.err = fmt.Errorf("upstream stream read failed after response was written: %w", forwardErr)
		return outcome
	}
	response, err := imageStreamTestResponse(recorder)
	if err != nil {
		outcome.err = err
		return outcome
	}
	outcome.response = response
	return outcome
}

// imageStreamTestResponse 把累积结果收成一份内部响应，只为复用统一的 succeed 收尾。
// 预览里写"几张图 + 首图/总耗时 + 尺寸"，绝不写图片本身（base64 不进日志）。
func imageStreamTestResponse(recorder *imageStreamTestRecorder) (*transformermodel.InternalLLMResponse, error) {
	if recorder == nil {
		return nil, fmt.Errorf("stream image test: nil recorder")
	}
	summary := recorder.summary
	if summary.images <= 0 {
		return nil, fmt.Errorf("upstream streaming response carried no image event after %d data frame(s) (judge: %s)",
			summary.rawBadge, imageStreamJudgeSummary)
	}
	preview := fmt.Sprintf("%d image(s) (stream, first %dms / total %dms",
		summary.images, recorder.FirstImageLatency().Milliseconds(), recorder.TotalLatency().Milliseconds())
	if size := strings.TrimSpace(summary.size); size != "" {
		preview += ", " + size
	}
	preview += ")"

	message := &transformermodel.Message{Role: "assistant"}
	previewCopy := preview
	message.Content.Content = &previewCopy
	response := &transformermodel.InternalLLMResponse{
		Choices: []transformermodel.Choice{{Index: 0, Message: message}},
	}
	if usage := summary.usage; usage != nil {
		response.Usage = &transformermodel.Usage{
			PromptTokens:     usage.inputTokens,
			CompletionTokens: usage.outputTokens,
			TotalTokens:      usage.totalTokens,
		}
	}
	return response, nil
}

// streamImageEventImages 返回这一帧能证明的图片张数与尺寸（尺寸单独由调用方按
// 顶层 size 字段覆盖，这里只负责"几张"）。
func streamImageEventImages(event map[string]json.RawMessage) int {
	images := 0
	if rawJSONStringField(event, "b64_json") != "" {
		images++
	}
	if rawJSONStringField(event, "url") != "" {
		images++
	}
	if raw, ok := event["data"]; ok {
		var items []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &items); err == nil {
			for _, item := range items {
				if rawJSONStringField(item, "b64_json") != "" || rawJSONStringField(item, "url") != "" {
					images++
				}
			}
		}
	}
	for _, key := range []string{"inlineData", "inline_data"} {
		if raw, ok := event[key]; ok && streamInlineDataCarriesImage(raw) {
			images++
		}
	}
	if raw, ok := event["candidates"]; ok {
		var candidates []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &candidates); err == nil {
			for _, candidate := range candidates {
				images += streamCandidateImages(candidate)
			}
		}
	}
	return images
}

// streamInlineDataCarriesImage 判断一段 inlineData/inline_data 对象里有没有非空 data。
func streamInlineDataCarriesImage(raw json.RawMessage) bool {
	var inline map[string]json.RawMessage
	if err := json.Unmarshal(raw, &inline); err != nil {
		return false
	}
	return rawJSONStringField(inline, "data") != ""
}

// streamCandidateImages 数一条 candidate 里 content.parts[].inlineData.data 的图片数。
func streamCandidateImages(candidate map[string]json.RawMessage) int {
	rawContent, ok := candidate["content"]
	if !ok {
		return 0
	}
	var content map[string]json.RawMessage
	if err := json.Unmarshal(rawContent, &content); err != nil {
		return 0
	}
	rawParts, ok := content["parts"]
	if !ok {
		return 0
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(rawParts, &parts); err != nil {
		return 0
	}
	images := 0
	for _, part := range parts {
		for _, key := range []string{"inlineData", "inline_data"} {
			if raw, ok := part[key]; ok && streamInlineDataCarriesImage(raw) {
				images++
			}
		}
	}
	return images
}

// streamImageEventUsage 从一帧事件里读 token 用量；没有就返回 nil。
func streamImageEventUsage(event map[string]json.RawMessage) *imageStreamUsage {
	rawUsage, ok := event["usage"]
	if !ok {
		return nil
	}
	var usage map[string]json.RawMessage
	if err := json.Unmarshal(rawUsage, &usage); err != nil {
		return nil
	}
	read := func(names ...string) int64 {
		for _, name := range names {
			if raw, ok := usage[name]; ok {
				var value int64
				if err := json.Unmarshal(raw, &value); err == nil {
					return value
				}
			}
		}
		return 0
	}
	input := read("input_tokens", "prompt_tokens")
	output := read("output_tokens", "completion_tokens")
	total := read("total_tokens")
	if total == 0 {
		total = input + output
	}
	if input == 0 && output == 0 && total == 0 {
		return nil
	}
	return &imageStreamUsage{inputTokens: input, outputTokens: output, totalTokens: total}
}

// rawJSONStringField 读一个 JSON 字符串字段；字段不存在、不是字符串都返回空。
func rawJSONStringField(raw map[string]json.RawMessage, name string) string {
	value, ok := raw[name]
	if !ok || len(value) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(value, &text); err != nil {
		return ""
	}
	return text
}
