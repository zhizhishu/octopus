package imageprobe

import (
	"encoding/base64"
	"encoding/binary"
	"strings"
)

// PixelAreaFromBase64 返回这张图（base64）的像素面积 width*height；认不出格式时返回 0。
//
// 为什么需要它：上游生图模型在请求高分辨率时会先给一张低分辨率预览、再给成品，两张都在
// 同一个 candidate 里。按 base64 长度挑"最大的那张"只是启发式（高噪声小图可能比干净的
// 大图更大），按像素面积挑才是压缩无关的判据。
//
// 只解头部前缀：调用方通常只有几十 MB 的 base64，整串解码只为读宽高太浪费，所以这里
// 先看 base64 开头的一小段（PNG/WebP 的头在头 30 字节内，JPEG 的 SOFn 一般也在前几 KB）。
func PixelAreaFromBase64(b64 string) int {
	b64 = strings.TrimSpace(b64)
	if b64 == "" {
		return 0
	}
	const prefixChars = 4096 // 约 3KB 二进制，足够覆盖 PNG/WebP 头与常见 JPEG 的 SOFn
	if len(b64) > prefixChars {
		b64 = b64[:prefixChars]
	}
	// 按 4 的倍数截断，避免 base64 解码报长度错。
	if cut := len(b64) - len(b64)%4; cut > 0 {
		b64 = b64[:cut]
	} else {
		return 0
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		raw, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(b64, "="))
		if err != nil {
			return 0
		}
	}
	return PixelArea(raw)
}

// PixelArea 从文件头解析 GIF/PNG/WebP/JPEG 的像素面积，认不出返回 0。
func PixelArea(raw []byte) int {
	if len(raw) < 10 {
		return 0
	}
	switch {
	case isPNG(raw):
		if len(raw) < 24 {
			return 0
		}
		w := int(binary.BigEndian.Uint32(raw[16:20]))
		h := int(binary.BigEndian.Uint32(raw[20:24]))
		return area(w, h)
	case isGIF(raw):
		w := int(binary.LittleEndian.Uint16(raw[6:8]))
		h := int(binary.LittleEndian.Uint16(raw[8:10]))
		return area(w, h)
	case isWebP(raw):
		return webpArea(raw)
	case isJPEG(raw):
		return jpegArea(raw)
	default:
		return 0
	}
}

func area(w, h int) int {
	if w <= 0 || h <= 0 || w > 1<<20 || h > 1<<20 {
		return 0
	}
	// 用 int64 判断溢出后再转 int：面积上限远小于 2^31。
	a := int64(w) * int64(h)
	if a > 1<<31-1 {
		return 0
	}
	return int(a)
}

func isPNG(raw []byte) bool {
	return len(raw) >= 8 && raw[0] == 0x89 && raw[1] == 'P' && raw[2] == 'N' && raw[3] == 'G'
}

func isGIF(raw []byte) bool {
	return len(raw) >= 6 && raw[0] == 'G' && raw[1] == 'I' && raw[2] == 'F'
}

func isJPEG(raw []byte) bool {
	return len(raw) >= 3 && raw[0] == 0xFF && raw[1] == 0xD8 && raw[2] == 0xFF
}

func isWebP(raw []byte) bool {
	return len(raw) >= 16 && string(raw[0:4]) == "RIFF" && string(raw[8:12]) == "WEBP"
}

// webpArea 处理 VP8X（扩展）、VP8（有损）、VP8L（无损）三种头。
func webpArea(raw []byte) int {
	if len(raw) < 30 {
		return 0
	}
	switch string(raw[12:16]) {
	case "VP8X":
		w := 1 + int(uint32(raw[24])|uint32(raw[25])<<8|uint32(raw[26])<<16)
		h := 1 + int(uint32(raw[27])|uint32(raw[28])<<8|uint32(raw[29])<<16)
		return area(w, h)
	case "VP8 ":
		// 关键帧起始码 9d 01 2a 之后是 14 位宽高。
		idx := -1
		for i := 12; i+7 < len(raw); i++ {
			if raw[i] == 0x9d && raw[i+1] == 0x01 && raw[i+2] == 0x2a {
				idx = i
				break
			}
		}
		if idx < 0 || idx+7 >= len(raw) {
			return 0
		}
		w := int(binary.LittleEndian.Uint16(raw[idx+3:idx+5]) & 0x3FFF)
		h := int(binary.LittleEndian.Uint16(raw[idx+5:idx+7]) & 0x3FFF)
		return area(w, h)
	case "VP8L":
		if len(raw) < 25 {
			return 0
		}
		bits := binary.LittleEndian.Uint32(raw[21:25])
		w := int(bits&0x3FFF) + 1
		h := int((bits>>14)&0x3FFF) + 1
		return area(w, h)
	}
	return 0
}

// jpegArea 扫到第一个 SOFn（C0-CF，跳过 C4/C8/CC）取宽高。
//
// 局限：只在前缀里找。带大 EXIF/缩略图的 JPEG 可能把 SOFn 挤到前缀之外，这时返回 0；
// 上层用「面积优先，面积同为 0 时比数据量」的次序，不会因此选错成空图，但极端情况下
// 可能挑到体积更小的那张。Gemini 图模型实测只回 image/png，这条路径是兜底。
func jpegArea(raw []byte) int {
	if len(raw) < 4 || raw[0] != 0xFF || raw[1] != 0xD8 {
		return 0
	}
	i := 2
	for i+9 < len(raw) {
		if raw[i] != 0xFF {
			i++
			continue
		}
		marker := raw[i+1]
		if marker == 0xFF { // 填充字节
			i++
			continue
		}
		if marker == 0xD8 || marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) {
			i += 2
			continue
		}
		if i+4 > len(raw) {
			return 0
		}
		segLen := int(binary.BigEndian.Uint16(raw[i+2 : i+4]))
		if segLen < 2 {
			return 0
		}
		isSOF := marker >= 0xC0 && marker <= 0xCF && marker != 0xC4 && marker != 0xC8 && marker != 0xCC
		if isSOF {
			if i+9 > len(raw) {
				return 0
			}
			h := int(binary.BigEndian.Uint16(raw[i+5 : i+7]))
			w := int(binary.BigEndian.Uint16(raw[i+7 : i+9]))
			return area(w, h)
		}
		i += 2 + segLen
	}
	return 0
}
