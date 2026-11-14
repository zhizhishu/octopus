package relay

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// 这些测试守的是「日志正文落库预算」这一层：request_content / response_content 两列
// 各自的存储上限（logContentColumnLimit），以及截断必须发生在图片交给写盘队列之前。
//
// 背景：这两列以前只做剥离内联二进制 + 脱敏，**没有任何长度上限**。凡是剥离认不出
// 的载荷（未知字段里的超长文本、不认识形状的 base64）都能以几十 MB 的单行进库，
// 直到下一轮容量清理才可能删掉。上限只作用于存储副本，真实上下游字节不动。

// oversizedJSONBody 造一份带未知字段的超大 JSON body（minBytes 以上）。
func oversizedJSONBody(minBytes int) string {
	var sb strings.Builder
	sb.WriteString(`{"model":"m","unknown_bulk_field":"`)
	sb.WriteString(strings.Repeat("x", minBytes))
	sb.WriteString(`"}`)
	return sb.String()
}

func requireMarker(t *testing.T, got string, before int) {
	t.Helper()
	marker := logContentTruncatedMarker(before)
	if !strings.Contains(got, marker) {
		t.Fatalf("truncation marker missing (want %q)", marker)
	}
	// 标记本身不能吃掉整份预算，否则正文一个字节都留不下。
	if len(marker) > logContentColumnLimit/2 {
		t.Fatalf("marker is too large: %d bytes", len(marker))
	}
}

// 23MB 未知字段 body 落库后必须 <= 1MiB，且标记写明截断前的字节数。
func TestCapLogColumnBoundsOversizedUnknownFieldBody(t *testing.T) {
	body := oversizedJSONBody(23 << 20)
	if len(body) < 23<<20 {
		t.Fatalf("test body is too small: %d", len(body))
	}

	got := capLogColumn(body)

	if len(got) > logContentColumnLimit {
		t.Fatalf("capped column = %d bytes, want <= %d", len(got), logContentColumnLimit)
	}
	if !utf8.ValidString(got) {
		t.Fatal("capped column is not valid UTF-8")
	}
	requireMarker(t, got, len(body))
	// 开头仍是原文，保留诊断价值。
	if !strings.HasPrefix(got, body[:256]) {
		t.Fatal("capped column lost its original head")
	}
	// 标记里的数字就是截断前这一列的实际字节数。
	if !strings.Contains(got, strconv.Itoa(len(body))) {
		t.Fatalf("marker should carry the pre-truncation byte count %d", len(body))
	}
}

// 未超限的正文必须逐字节原样返回：不能顺手重排、不能加标记。
func TestCapLogColumnLeavesSmallBodyByteIdentical(t *testing.T) {
	small := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	if got := capLogColumn(small); got != small {
		t.Fatalf("small column changed:\n got %q\nwant %q", got, small)
	}
	// 恰好等于上限也不该动。
	exact := strings.Repeat("z", logContentColumnLimit)
	if got := capLogColumn(exact); got != exact {
		t.Fatalf("column exactly at the limit must be untouched, got %d bytes", len(got))
	}
}

// 截断不能把多字节字符切半（半截 UTF-8 会让详情页与落库都出问题）。
func TestCapLogColumnKeepsUTF8ValidAtBoundary(t *testing.T) {
	// 全是 3 字节汉字：上限附近必然落在字符中间。
	body := strings.Repeat("啊", logContentColumnLimit/2)
	got := capLogColumn(body)
	if len(got) > logContentColumnLimit {
		t.Fatalf("capped column = %d bytes, want <= %d", len(got), logContentColumnLimit)
	}
	if !utf8.ValidString(got) {
		t.Fatal("truncation cut a multi-byte character in half")
	}
	if r := utf8.RuneCountInString(got); r == 0 {
		t.Fatal("capped column lost all runes")
	}
}

// stubLogImageEnqueue 把写盘队列换成记录器，返回已入队哈希的切片指针。
func stubLogImageEnqueue(t *testing.T) *[]string {
	t.Helper()
	prev := logImageEnqueue
	saved := make([]string, 0, 4)
	logImageEnqueue = func(hash, ext string, data []byte) bool {
		saved = append(saved, hash)
		return true
	}
	t.Cleanup(func() { logImageEnqueue = prev })
	return &saved
}

// 占位符被截掉时那张图不能入队：否则文件写下去了、正文里却没有任何引用，
// 只能等 1 小时的孤儿清扫才回收。
func TestContentWithImagesDoesNotEnqueueTruncatedAwayImage(t *testing.T) {
	withLogImages(t)
	saved := stubLogImageEnqueue(t)
	img := fakeImage(t, 2048)
	h := sha(img)
	filler := strings.Repeat("y", logContentColumnLimit+(64<<10))

	// 图片占位符排在上限之后，必然被截掉。
	var l upstreamBodyLog
	got := l.contentWithImages(func(c *imageCapture) string {
		inner, _ := c.register(img, -1)
		return `{"model":"m","filler":"` + filler + `","tail":"` + inner + `"}`
	})

	if len(got) > logContentColumnLimit {
		t.Fatalf("content = %d bytes, want <= %d", len(got), logContentColumnLimit)
	}
	if !strings.Contains(got, "bytes before truncation") {
		t.Fatal("expected a truncation marker")
	}
	if strings.Contains(got, h) {
		t.Fatal("placeholder hash must be gone once the placeholder is truncated away")
	}
	if len(*saved) != 0 {
		t.Fatalf("truncated-away image must not be enqueued, got %v", *saved)
	}
	if len(l.contentImageHashes) != 0 {
		t.Fatalf("no image hash should reach relay_log_images, got %v", l.contentImageHashes)
	}
}

// 反方向：占位符在上限之内时照旧入队、照旧带进 ImageHashes（不能因为加了上限就丢图）。
func TestContentWithImagesKeepsVisibleImagePlaceholder(t *testing.T) {
	withLogImages(t)
	saved := stubLogImageEnqueue(t)
	img := fakeImage(t, 2048)
	h := sha(img)
	filler := strings.Repeat("y", logContentColumnLimit+(64<<10))

	var l upstreamBodyLog
	got := l.contentWithImages(func(c *imageCapture) string {
		inner, _ := c.register(img, -1)
		return `{"model":"m","head":"` + inner + `","filler":"` + filler + `"}`
	})

	if len(got) > logContentColumnLimit {
		t.Fatalf("content = %d bytes, want <= %d", len(got), logContentColumnLimit)
	}
	if !strings.Contains(got, "id="+h) {
		t.Fatal("a placeholder inside the budget must be kept verbatim")
	}
	if len(*saved) != 1 || (*saved)[0] != h {
		t.Fatalf("visible image should be enqueued once, got %v", *saved)
	}
	if len(l.contentImageHashes) != 1 || l.contentImageHashes[0] != h {
		t.Fatalf("visible image hash should reach ImageHashes, got %v", l.contentImageHashes)
	}
}

// 小正文必须逐字节不变：与以前的 redactLogBody(build(nil)) 完全一致，没有标记。
func TestContentWithImagesLeavesSmallContentByteIdentical(t *testing.T) {
	withLogImages(t)
	stubLogImageEnqueue(t)
	small := `{"model":"m","messages":[{"role":"user","content":"看看这张图"}]}`

	var l upstreamBodyLog
	got := l.contentWithImages(func(c *imageCapture) string {
		return stripInlineBinaryWith(small, c)
	})

	if want := redactLogBody(small); got != want {
		t.Fatalf("small content changed:\n got %q\nwant %q", got, want)
	}
	if strings.Contains(got, "bytes before truncation") {
		t.Fatal("small content must not carry a truncation marker")
	}
}

// 23MB 的真实装配路径（剥离 → 脱敏 → 上限）落库必须 <= 1MiB。
func TestContentWithImagesBoundsOversizedBody(t *testing.T) {
	withLogImages(t)
	body := oversizedJSONBody(23 << 20)

	var l upstreamBodyLog
	got := l.contentWithImages(func(c *imageCapture) string {
		return stripInlineBinaryWith(body, c)
	})

	if len(got) > logContentColumnLimit {
		t.Fatalf("stored column = %d bytes, want <= %d", len(got), logContentColumnLimit)
	}
	if !utf8.ValidString(got) {
		t.Fatal("stored column is not valid UTF-8")
	}
	requireMarker(t, got, len(body))
}

// 真实源请求不能被改写：截断只发生在这份日志副本上。
func TestContentWithImagesDoesNotMutateSourceBytes(t *testing.T) {
	withLogImages(t)
	saved := stubLogImageEnqueue(t)
	img := fakeImage(t, 4096)
	h := sha(img)
	raw := []byte(chatImageBody("data:image/png;base64," + b64(img)))
	before := append([]byte(nil), raw...)

	var l upstreamBodyLog
	got := l.contentWithImages(func(c *imageCapture) string {
		return stripInlineBinaryWith(string(raw), c)
	})

	if !bytes.Equal(raw, before) {
		t.Fatal("contentWithImages mutated the source request bytes")
	}
	if !strings.Contains(got, "id="+h) {
		t.Fatal("image should have been stripped into an id placeholder")
	}
	if strings.Contains(got, b64(img)[:64]) {
		t.Fatal("base64 payload leaked into the stored column")
	}
	if len(*saved) != 1 || (*saved)[0] != h {
		t.Fatalf("image should be enqueued once, got %v", *saved)
	}
}
