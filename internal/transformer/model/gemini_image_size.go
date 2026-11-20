package model

import (
	"math"
	"strconv"
	"strings"
)

// NormalizeGeminiImageSize 把客户端五花八门的写法收敛成 Gemini 图像模型认的三档。
//
// 为什么放在这里：出图桥（relay/）和 chat 出图（transformer/outbound/gemini）原本各
// 自带一份同名映射函数互相复制，参考实现（CLIProxyAPI 的 gemini translator、new-api 的
// gemini adaptor）都只维持一张表。分辨率档位是上游枚举，多一份副本就多一处漂移——
// 实际情况就是漏掉 4K、且大小写敏感让 "2k"/"4k" 静默失效，客户端只能拿到默认的 1K。
//
// 认不出来的值返回空串：宁可不发，也不替客户端猜一个档位。
func NormalizeGeminiImageSize(raw string) string {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "1K", "STANDARD", "MEDIUM", "LOW", "AUTO":
		return "1K"
	case "2K", "HD", "HIGH":
		return "2K"
	case "4K", "UHD", "ULTRA":
		return "4K"
	default:
		return ""
	}
}

// geminiStandardAspectRatios 是 Gemini 3 图像模型（Pro / Flash）共有的宽高比。
var geminiStandardAspectRatios = []struct {
	label string
	ratio float64
}{
	{"1:1", 1}, {"2:3", 2.0 / 3}, {"3:2", 3.0 / 2}, {"3:4", 3.0 / 4}, {"4:3", 4.0 / 3},
	{"4:5", 4.0 / 5}, {"5:4", 5.0 / 4}, {"9:16", 9.0 / 16}, {"16:9", 16.0 / 9}, {"21:9", 21.0 / 9},
}

// ParseImagePixelSize 解析 OpenAI 风格的 "WxH"（也认 X、×、*），失败返回 ok=false。
func ParseImagePixelSize(raw string) (width, height int, ok bool) {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.NewReplacer("×", "x", "*", "x", " ", "").Replace(s)
	w, h, found := strings.Cut(s, "x")
	if !found {
		return 0, 0, false
	}
	width, errW := strconv.Atoi(w)
	height, errH := strconv.Atoi(h)
	if errW != nil || errH != nil || width <= 0 || height <= 0 {
		return 0, 0, false
	}
	return width, height, true
}

// GeminiAspectRatioFromPixelSize 把像素尺寸归到最近的标准宽高比（误差约 6% 以内）。
//
// 参考 new-api 的 Gemini 尺寸表和 Google 官方分辨率表：2400x1792 是 4:3，
// 2752x1536 是 16:9。离所有标准比例都太远时返回空串，交给上游按提示词/参考图决定。
func GeminiAspectRatioFromPixelSize(raw string) string {
	width, height, ok := ParseImagePixelSize(raw)
	if !ok {
		return ""
	}
	target := math.Log(float64(width) / float64(height))
	best, bestDiff := "", 0.06
	for _, candidate := range geminiStandardAspectRatios {
		if diff := math.Abs(target - math.Log(candidate.ratio)); diff <= bestDiff {
			best, bestDiff = candidate.label, diff
		}
	}
	return best
}

// GeminiImageSizeFromPixelSize 按像素长边推分辨率档位，只给 2K / 4K。
//
// Google 官方表里 1K 档长边最大 1584（21:9），2K 档长边 2048–3168，4K 档从 4096 起，
// 所以 1800 / 3600 两条线不会误判。1K 级尺寸（1024x1024、1792x1024 等）返回空串，
// 不主动发 imageSize：老模型（如 2.5 Flash Image）带上这个字段会报错。
func GeminiImageSizeFromPixelSize(raw string) string {
	width, height, ok := ParseImagePixelSize(raw)
	if !ok {
		return ""
	}
	longEdge := max(width, height)
	switch {
	case longEdge >= 3600:
		return "4K"
	case longEdge >= 1800:
		return "2K"
	default:
		return ""
	}
}
