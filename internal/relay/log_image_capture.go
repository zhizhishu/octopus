package relay

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/zhizhishu/octopus/internal/logimage"
	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
)

// 「真实出站请求体」里的内联图片单独存档（2026-10-30，用户决定）。
//
// 正文仍按发出去的字节原样保存、不打码、照旧 128KB 截断；只有能识别的图片
// （按文件头判断的 png / jpeg / gif / webp）换成带内容哈希的占位符
//
//	"[image: image/png, 245KB, id=<sha256>]"
//
// 原图交给 logimage 后台写盘，详情页凭 id 去 GET /api/v1/log/image/:hash 取。
// 存不了的（过大、超张数/总量、队列满）保持旧格式并写明原因：
//
//	"[image omitted: image/png, 12MB, too large]"
//
// 🔴 所有解码都在 string(body) 这份副本上做，真正发出去的 []byte 一个字节不动。
//
// 2026-10-31 起「请求内容」(request_content) 和「响应内容」(response_content)
// 也走同一套：客户端发来的图、模型回出来的图同样换成带 id 的占位符，文件按哈希
// 去重（和出站请求体里的同一张图共用一个文件）。这两列落库前还要过
// redactLogBody，而 64 位十六进制会被当成密钥打掉，所以用 redactKeepingImages
// 先把占位符换成不会被打码的临时记号、打码完再换回来。

const (
	omitReasonTooLarge   = "too large"
	omitReasonTooMany    = "too many images"
	omitReasonOverBudget = "request image limit reached"
	omitReasonQueueFull  = "save queue full"
)

// logImageEnqueue / logImageCaptureEnabled 是包级变量，测试可替换。
var (
	logImageEnqueue        = logimage.Enqueue
	logImageCaptureEnabled = defaultLogImageCaptureEnabled
)

// defaultLogImageCaptureEnabled：历史日志不落库时一张图都不存——
// 日志只在内存里，没有引用行，清理任务永远够不着这些文件。
// 调用方（SetUpstreamRequestBody）本身只在出站请求体开关打开时才会被调用。
func defaultLogImageCaptureEnabled() bool {
	keep, err := op.SettingGetBool(dbmodel.SettingKeyRelayLogKeepEnabled)
	return err == nil && keep
}

type capturedImage struct {
	hash string
	mime string
	ext  string
	data []byte
	// inner 是占位符的文字本体（不含 JSON 引号）。它只由 [a-z0-9/.,=: ] 组成，
	// 在 JSON 文本里原样出现，所以可以直接 strings.Contains / ReplaceAll。
	inner string
}

// imageCapture 收集一次请求体里可保存的图片。不是并发安全的：一次
// SetUpstreamRequestBody 内单线程使用。
type imageCapture struct {
	byHash map[string]*capturedImage
	order  []string
	total  int
}

func newImageCapture() *imageCapture {
	return &imageCapture{byHash: make(map[string]*capturedImage)}
}

// placeholder 返回 raw 这个 JSON 字符串值的替代文本（已加引号）。c 为 nil、
// 不是图片、或解不出可识别的图片时与 placeholderJSON 完全一致。
func (c *imageCapture) placeholder(kind, mime string, size int, raw string) string {
	if c == nil || kind != "image" {
		return placeholderJSON(kind, mime, size)
	}
	inner, reason := c.capture(raw, size)
	if inner != "" {
		return quoteLogString(inner)
	}
	if reason != "" {
		return omittedPlaceholderJSON(kind, mime, size, reason)
	}
	return placeholderJSON(kind, mime, size)
}

// capture 尝试登记一张图。成功返回带 id 的占位符本体；存不了但需要写明原因时
// 返回 reason；认不出是图片（或根本不是 base64）时两者都为空。
func (c *imageCapture) capture(raw string, declaredSize int) (inner, reason string) {
	b64, ok := inlineBase64Payload(raw)
	if !ok {
		return "", ""
	}
	// 先按长度估算，过大的图连解码都不做。
	if decodedBase64SizeStr(b64) > logimage.MaxImageBytes || declaredSize > logimage.MaxImageBytes {
		return "", omitReasonTooLarge
	}
	data, err := decodeLogBase64(b64)
	if err != nil || len(data) == 0 {
		return "", ""
	}
	if len(data) > logimage.MaxImageBytes {
		return "", omitReasonTooLarge
	}
	return c.register(data, len(strings.TrimSpace(raw)))
}

// register 登记一张已解码的图。maxInnerLen >= 0 时占位符（含两侧引号）不许比它长，
// 这是 JSON 里原地替换的规矩；multipart 摘要里没有被替换的原值，传 -1 不做这项检查。
// 认不出是图片时两个返回值都为空。
func (c *imageCapture) register(data []byte, maxInnerLen int) (inner, reason string) {
	if len(data) > logimage.MaxImageBytes {
		return "", omitReasonTooLarge
	}
	mime, ext, ok := logimage.Sniff(data)
	if !ok {
		return "", ""
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	if e, ok := c.byHash[hash]; ok {
		// 同一张图在一个请求里出现多次（多轮对话每轮都带着）：共用一份，不重复计数。
		return e.inner, ""
	}
	if len(c.order) >= logimage.MaxImagesPerRequest {
		return "", omitReasonTooMany
	}
	if c.total+len(data) > logimage.MaxBytesPerRequest {
		return "", omitReasonOverBudget
	}
	inner = fmt.Sprintf("[image: %s, %s, id=%s]", mime, formatByteSize(len(data)), hash)
	// 占位符不能比原值长（acceptReplacement 的规矩）。小到这个地步的图不值得存。
	if maxInnerLen >= 0 && len(inner)+2 > maxInnerLen {
		return "", ""
	}
	c.byHash[hash] = &capturedImage{hash: hash, mime: mime, ext: ext, data: data, inner: inner}
	c.order = append(c.order, hash)
	c.total += len(data)
	return inner, ""
}

// commit 在剥离、截断都做完之后调用：只把**最终正文里还看得见**占位符的图交给
// 写盘队列（被截断掉的那些不存，免得留下没人引用的文件）；入队失败的把占位符
// 换回 omitted（更短，不会突破截断上限）。返回改好的正文与已入队的哈希。
func (c *imageCapture) commit(logged string) (string, []string) {
	if c == nil || len(c.order) == 0 {
		return logged, nil
	}
	var saved []string
	for _, h := range c.order {
		e := c.byHash[h]
		if !strings.Contains(logged, e.inner) {
			continue
		}
		if logImageEnqueue(e.hash, e.ext, e.data) {
			saved = append(saved, e.hash)
			continue
		}
		omitted := fmt.Sprintf("[image omitted: %s, %s, %s]", e.mime, formatByteSize(len(e.data)), omitReasonQueueFull)
		logged = strings.ReplaceAll(logged, e.inner, omitted)
	}
	// 交出去之后不再持有字节，让大图尽早被回收。
	c.byHash = nil
	c.order = nil
	return logged, saved
}

// inlineBase64Payload 从 JSON 字符串值里取出 base64 正文：data:…;base64,xxx 或裸 base64。
func inlineBase64Payload(raw string) (string, bool) {
	s, ok := unquoteSimple(strings.TrimSpace(raw))
	if !ok {
		return "", false
	}
	if strings.HasPrefix(s, "data:") {
		comma := strings.IndexByte(s, ',')
		if comma < 0 {
			return "", false
		}
		isB64 := false
		for _, part := range strings.Split(s[len("data:"):comma], ";") {
			if strings.TrimSpace(part) == "base64" {
				isB64 = true
				break
			}
		}
		if !isB64 {
			return "", false
		}
		s = s[comma+1:]
	}
	if strings.ContainsAny(s, "\r\n\t ") {
		s = strings.Map(func(r rune) rune {
			switch r {
			case '\r', '\n', '\t', ' ':
				return -1
			}
			return r
		}, s)
	}
	if s == "" {
		return "", false
	}
	return s, true
}

func decodeLogBase64(s string) ([]byte, error) {
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(s, "=")); err == nil {
		return b, nil
	}
	if b, err := base64.URLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

// isSavedImagePlaceholder 认出已带 id 的占位符，第二遍剥离时原样保留（幂等）。
func isSavedImagePlaceholder(inner string) bool {
	if !strings.HasPrefix(inner, "[image: ") || !strings.HasSuffix(inner, "]") {
		return false
	}
	i := strings.LastIndex(inner, ", id=")
	return i >= 0 && logimage.ValidHash(inner[i+len(", id="):len(inner)-1])
}

func omittedPlaceholderJSON(kind, mime string, size int, reason string) string {
	if kind == "" {
		kind = "file"
	}
	return quoteLogString(fmt.Sprintf("[%s omitted: %s, %s, %s]", kind, sanitizeLogMIME(mime), formatByteSize(size), reason))
}

func quoteLogString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

// redactKeepingImages 对一份日志正文打码（redactLogBody），但保留 c 里登记过的
// 占位符原文：占位符里的 sha256 长得像密钥，直接打码会变成 id=[redacted]。
// c 为 nil 或没有登记任何图时就是 redactLogBody(s)。
func redactKeepingImages(s string, c *imageCapture) string {
	if c == nil || len(c.order) == 0 {
		return redactLogBody(s)
	}
	type swap struct{ token, inner string }
	swaps := make([]swap, 0, len(c.order))
	for i, h := range c.order {
		e := c.byHash[h]
		if e == nil || !strings.Contains(s, e.inner) {
			continue
		}
		// 记号只含字母和连字符，打码规则不会碰它；正文里本来就有同样文字的概率可以忽略。
		token := fmt.Sprintf("[octopus-log-image-ref-%c%c]", 'a'+rune(i/26), 'a'+rune(i%26))
		if strings.Contains(s, token) {
			// 极端情况：正文里恰好有这个记号。宁可让这张图的 id 被打码（commit 时
			// 看不见占位符，就不存文件），也不能把客户端的原文换掉。
			continue
		}
		s = strings.ReplaceAll(s, e.inner, token)
		swaps = append(swaps, swap{token: token, inner: e.inner})
	}
	s = redactLogBody(s)
	for _, sw := range swaps {
		s = strings.ReplaceAll(s, sw.token, sw.inner)
	}
	return s
}

// contentWithImages 生成一列落库正文（请求内容 / 响应内容）：build 在副本上产出
// 已剥离（可传 capture 登记图片）、已截断的文本，这里再打码、把截断后仍看得见的
// 图片交给写盘队列，并把哈希记进 l，落库时一并写引用行。
//
// 图片另存关闭（历史日志不落库）时 capture 为 nil，结果与以前
// redactLogBody(build(nil)) 完全一样。
func (l *upstreamBodyLog) contentWithImages(build func(c *imageCapture) string) string {
	capture := newLogImageCapture()
	content := redactKeepingImages(build(capture), capture)
	content, saved := capture.commit(content)
	if l != nil && len(saved) > 0 {
		l.contentImageHashes = append(l.contentImageHashes, saved...)
	}
	return content
}

// captureDataURL 给一个已解码的 Go 字符串（不是 JSON 文本）里的 data:image/...;base64,
// 登记图片，成功返回占位符本体。认不出、存不了都返回空串（调用方退回旧占位）。
func (c *imageCapture) captureDataURL(u string) string {
	if c == nil || !strings.HasPrefix(u, "data:") {
		return ""
	}
	raw := quoteLogString(u)
	inner, _ := c.capture(raw, 0)
	return inner
}
