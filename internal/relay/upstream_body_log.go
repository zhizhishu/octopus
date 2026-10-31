package relay

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/zhizhishu/octopus/internal/logimage"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/relay/bodycache"
	"github.com/zhizhishu/octopus/internal/utils/log"
	"github.com/zhizhishu/octopus/internal/utils/safe"
)

// upstreamBodyLog 是「真实出站请求体」这一列在各类 metrics 上的共用存放处。
// 聊天主路径（RelayMetrics）、图片（imagesRelayMetrics）、raw 透传与视频
// （rawProtocolRelayMetrics）都嵌入它，写入规则只有这一份。
//
// 开关与聊天主路径同一个（upstreamRequestBodyLogEnabled）。关闭时任何路径都不读
// body、不碰这里，出站字节与日志行和以前完全一样。
type upstreamBodyLog struct {
	// upstreamRequestBody 是落库的快照文本。空 = 未采集（开关关闭或采集失败）。
	upstreamRequestBody string
	// upstreamImageHashes 是 upstreamRequestBody 里带 id 占位符、且已交给写盘队列的
	// 图片哈希；落库时写进 relay_log_images 引用表，清理任务据此判断文件还要不要。
	upstreamImageHashes []string
	// contentImageHashes 是请求内容 / 响应内容两列里另存的图片哈希（见
	// contentWithImages）。和上面分开放：竞速时出站 body 跟着赢家换，这两列不换。
	contentImageHashes []string
}

// set 记录一份 JSON（或其它文本）出站请求体。
//
// 🔴 body 往往就是重挂回即将发出的 http.Request 的那份 []byte。这里只能读，
// 不能改它的任何一个字节。
//
// 按用户决定，这个字段原样记录、不脱敏。只做两件事，顺序是「先在副本上剥内联
// 二进制、再截断」：先截断会把 base64 从中间切断，日志里留下一大段无用载荷。
// 截断标记里的总长仍是真正发出去的字节数，不是剥过之后的日志长度。
//
// 图片另存（见 log_image_capture.go）：历史日志落库时，能识别的内联图片换成带
// 哈希的占位符，原图交给后台写盘。顺序是「剥离 → 截断 → 只存截断后还看得见的」。
func (l *upstreamBodyLog) set(body []byte) {
	if l == nil || len(body) == 0 {
		return
	}
	capture := newLogImageCapture()
	l.store(stripInlineBinaryWith(string(body), capture), int64(len(body)), capture)
}

// store 截断到 upstreamRequestBodyLogLimit，再把截断后仍可见的图片交给写盘队列。
// totalBytes 是真正发出去的字节数，只用于截断标记。
func (l *upstreamBodyLog) store(logged string, totalBytes int64, capture *imageCapture) {
	if len(logged) > upstreamRequestBodyLogLimit {
		logged = truncateUTF8(logged, upstreamRequestBodyLogLimit) +
			fmt.Sprintf("\n...[truncated: %d bytes total]", totalBytes)
	}
	logged, l.upstreamImageHashes = capture.commit(logged)
	l.upstreamRequestBody = logged
}

// applyTo 把快照写进日志行。没采到就什么都不写，不假装有值。
//
// 图片哈希（出站请求体 + 请求/响应内容）追加到 ImageHashes，不覆盖调用方已经
// 放进去的；重复的哈希在写引用行时去重。
func (l *upstreamBodyLog) applyTo(relayLog *model.RelayLog) {
	if l == nil || relayLog == nil {
		return
	}
	relayLog.ImageHashes = append(relayLog.ImageHashes, l.contentImageHashes...)
	if l.upstreamRequestBody == "" {
		return
	}
	relayLog.UpstreamRequestBody = l.upstreamRequestBody
	relayLog.ImageHashes = append(relayLog.ImageHashes, l.upstreamImageHashes...)
}

// newLogImageCapture 在「图片另存」可用时返回一个收集器，否则返回 nil（占位符
// 退回不带 id 的旧格式）。
func newLogImageCapture() *imageCapture {
	if logImageCaptureEnabled() {
		return newImageCapture()
	}
	return nil
}

// captureOutboundJSONBody 给一条即将发出的请求拍出站 body 快照（开关关闭时什么都
// 不做、不读 body）。与聊天主路径同一个开关、同一个 snapshotOutboundRequestBody。
//
// 读失败返回 error：此时 Body 已被部分消费，调用方必须中止这次尝试，不能把残缺
// 请求体发出去。
func captureOutboundJSONBody(req *http.Request, sink *upstreamBodyLog) error {
	if sink == nil || !upstreamRequestBodyLogEnabled() {
		return nil
	}
	snapshot, err := snapshotOutboundRequestBody(req)
	if err != nil {
		return err
	}
	sink.set(snapshot)
	return nil
}

// captureOutboundMultipartBody 记录一份 multipart 出站请求体的摘要（开关关闭时什么
// 都不做）。
//
// multipart 里是图片 / 音频这类二进制，按用户决定不许原样落库。真正发出去的
// body 是 io.Pipe 流式写的，读它会把请求本身吃掉，所以这里用**同一个函数**
// （copyMultipartReplaceModel）、同一个 boundary、同一份缓存再生成一遍——
// 字节与线上那份完全相同——边读边写成摘要：
//
//   - 普通文本字段原样记值（含已替换成上游名的 model）；
//   - 能认出的图片按图片另存规则存文件，记 "[image: mime, size, id=…]"；
//   - 其它文件（音频等）只记 "[audio omitted: mime, size]" 这样的短占位符。
//
// 出站请求一个字节不动；摘要失败只打一条警告，不影响这次尝试。
func captureOutboundMultipartBody(sink *upstreamBodyLog, bc *bodycache.BodyCache, srcBoundary, outBoundary, newModel string) {
	if sink == nil || bc == nil || !upstreamRequestBodyLogEnabled() {
		return
	}
	capture := newLogImageCapture()
	summary, total, err := summarizeOutboundMultipart(bc, srcBoundary, outBoundary, newModel, capture)
	if err != nil {
		log.Warnf("failed to summarize multipart upstream request body for log: %v", err)
		return
	}
	sink.store(summary, total, capture)
}

// multipartLogPart 是 multipart 摘要里的一个分段。
type multipartLogPart struct {
	Name        string          `json:"name"`
	Filename    string          `json:"filename,omitempty"`
	ContentType string          `json:"content_type,omitempty"`
	Size        int64           `json:"size"`
	Value       *string         `json:"value,omitempty"`
	Data        json.RawMessage `json:"data,omitempty"`
}

type multipartLogSummary struct {
	ContentType string             `json:"content_type"`
	SizeBytes   int64              `json:"size_bytes"`
	Parts       []multipartLogPart `json:"parts"`
}

// multipartLogTextLimit 是单个文本字段读进内存的上限；整份摘要最终还会按
// upstreamRequestBodyLogLimit 截断，这里多读也存不下。
const multipartLogTextLimit = upstreamRequestBodyLogLimit

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func summarizeOutboundMultipart(bc *bodycache.BodyCache, srcBoundary, outBoundary, newModel string, capture *imageCapture) (string, int64, error) {
	pr, pw := io.Pipe()
	defer pr.Close()
	safe.SafeGo("upstream-body-log-multipart", func() {
		src, err := bc.NewReader()
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		defer src.Close()
		mw := multipart.NewWriter(pw)
		if err := mw.SetBoundary(outBoundary); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		if err := copyMultipartReplaceModel(src, srcBoundary, mw, newModel); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		if err := mw.Close(); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		_ = pw.Close()
	})

	counter := &countingReader{r: pr}
	mr := multipart.NewReader(counter, outBoundary)
	summary := multipartLogSummary{
		ContentType: "multipart/form-data; boundary=" + outBoundary,
		Parts:       []multipartLogPart{},
	}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", 0, err
		}
		entry, perr := summarizeMultipartPart(part, capture)
		_ = part.Close()
		if perr != nil {
			return "", 0, perr
		}
		summary.Parts = append(summary.Parts, entry)
	}
	// 读完结束 boundary 之后的尾巴，size_bytes 才是线上的完整字节数。
	if _, err := io.Copy(io.Discard, counter); err != nil {
		return "", 0, err
	}
	summary.SizeBytes = counter.n
	b, err := json.Marshal(summary)
	if err != nil {
		return "", 0, err
	}
	return string(b), counter.n, nil
}

func summarizeMultipartPart(part *multipart.Part, capture *imageCapture) (multipartLogPart, error) {
	entry := multipartLogPart{
		Name:        part.FormName(),
		Filename:    part.FileName(),
		ContentType: part.Header.Get("Content-Type"),
	}
	if entry.Filename == "" {
		// 文本字段：原样记值。读不下的部分只计长度（整份摘要反正会截断）。
		head, err := io.ReadAll(io.LimitReader(part, multipartLogTextLimit))
		if err != nil {
			return entry, err
		}
		rest, err := io.Copy(io.Discard, part)
		if err != nil {
			return entry, err
		}
		entry.Size = int64(len(head)) + rest
		if utf8.Valid(head) && !looksLikeInlineDataURL(head) {
			value := string(head)
			entry.Value = &value
			return entry, nil
		}
		// 不是文本（或是 data: URL 内联的二进制）：同文件一样处理。
		entry.Data = json.RawMessage(binaryPartPlaceholder(head, entry.Size, entry.ContentType, capture))
		return entry, nil
	}

	// 文件：最多读一张可保存图片的上限，多出来的只计长度，不进内存。
	head, err := io.ReadAll(io.LimitReader(part, int64(logimage.MaxImageBytes)+1))
	if err != nil {
		return entry, err
	}
	rest, err := io.Copy(io.Discard, part)
	if err != nil {
		return entry, err
	}
	entry.Size = int64(len(head)) + rest
	entry.Data = json.RawMessage(binaryPartPlaceholder(head, entry.Size, entry.ContentType, capture))
	return entry, nil
}

// looksLikeInlineDataURL 认出 "data:<mime>;base64,..." 形式的文本字段值。
func looksLikeInlineDataURL(b []byte) bool {
	if len(b) < len("data:") || string(b[:len("data:")]) != "data:" {
		return false
	}
	head := b
	if len(head) > 256 {
		head = head[:256]
	}
	comma := strings.IndexByte(string(head), ',')
	return comma > 0 && strings.Contains(string(head[:comma]), ";base64")
}

// binaryPartPlaceholder 返回一个 JSON 字符串（已加引号）：能存的图片给带 id 的
// 占位符，其余给 "[kind omitted: mime, size(, reason)]"。
func binaryPartPlaceholder(head []byte, size int64, declaredType string, capture *imageCapture) string {
	data := head
	if looksLikeInlineDataURL(head) {
		raw := quoteLogString(string(head))
		if decoded, ok := inlineBase64Payload(raw); ok {
			if b, err := decodeLogBase64(decoded); err == nil {
				data = b
			}
		}
		if comma := strings.IndexByte(string(head), ','); comma > 0 && declaredType == "" {
			declaredType = strings.TrimSuffix(string(head[len("data:"):comma]), ";base64")
		}
	}
	mime := strings.TrimSpace(declaredType)
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = strings.TrimSpace(mime[:i])
	}
	if sniffed, _, ok := logimage.Sniff(data); ok {
		mime = sniffed
		if capture != nil && size <= int64(logimage.MaxImageBytes) {
			inner, reason := capture.register(data, -1)
			if inner != "" {
				return quoteLogString(inner)
			}
			if reason != "" {
				return omittedPlaceholderJSON("image", mime, int(size), reason)
			}
		}
		if capture != nil && size > int64(logimage.MaxImageBytes) {
			return omittedPlaceholderJSON("image", mime, int(size), omitReasonTooLarge)
		}
		return placeholderJSON("image", mime, int(size))
	}
	if mime == "" {
		mime = http.DetectContentType(data)
		if i := strings.IndexByte(mime, ';'); i >= 0 {
			mime = strings.TrimSpace(mime[:i])
		}
	}
	return placeholderJSON(multipartBinaryKind(mime), mime, int(size))
}

func multipartBinaryKind(mime string) string {
	switch {
	case strings.HasPrefix(mime, "audio/"):
		return "audio"
	case strings.HasPrefix(mime, "video/"):
		return "video"
	case strings.HasPrefix(mime, "image/"):
		return "image"
	default:
		return "file"
	}
}
