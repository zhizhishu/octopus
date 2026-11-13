package model

import "strings"

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
