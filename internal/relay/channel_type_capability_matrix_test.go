package relay

import (
	"testing"

	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// 这条矩阵用例把「每种渠道类型 × {图片, 视频}」的可用性逐格钉死。
//
// 为什么必须有：图片与视频是**两张各自独立的表** ——
//
//	图片：isOpenAIImagesCompatibleChannelType = OpenAIChat / OpenAIResponse / CustomOpenAIChat，
//	      再经 isImagesEndpointCompatibleChannelType 加上 Gemini（Gemini 图片走它自己的
//	      predict / generateContent 适配）；
//	视频：isVideoCapableChannelType = OpenAIChat / OpenAIResponse / CustomOpenAIChat，**刻意不含 Gemini**
//	      （视频走 Gemini 原生协议还没接）。
//
// 两张表长得像不等于它们相同。差异必须是**写下来的决定**，不能是两处巧合。
// 2026-10 的缺陷正是这种"看起来对齐、其实没对齐"：视频侧复用了 raw 协议那张表
// （isOpenAIWireChannelType），于是 OpenAIEmbedding 混进了"能接视频"的名单。
// 嵌入渠道是算向量的账号，生成视频无意义 —— 真配上只会把"配置错了"推迟成上游 502。
//
// 改任何一张表都必须同步改这里，否则这条用例红。
func TestChannelTypeCapabilityMatrixForImagesAndVideo(t *testing.T) {
	cases := []struct {
		name       string
		typ        outbound.OutboundType
		wantImages bool
		wantVideo  bool
	}{
		{"OpenAIChat", outbound.OutboundTypeOpenAIChat, true, true},
		{"OpenAIResponse", outbound.OutboundTypeOpenAIResponse, true, true},
		{"CustomOpenAIChat", outbound.OutboundTypeCustomOpenAIChat, true, true},
		// Gemini：图片能接（走它自己的适配），视频不能接（原生协议没接）。
		{"Gemini", outbound.OutboundTypeGemini, true, false},
		{"Anthropic", outbound.OutboundTypeAnthropic, false, false},
		// 嵌入渠道：两张表都不收。视频这一格就是本次修复的靶心。
		{"OpenAIEmbedding", outbound.OutboundTypeOpenAIEmbedding, false, false},
		// 4 号位是退役的 Volcengine，编号永久空着：两张表都必须拒。
		{"退役的 Volcengine 4 号位", outbound.OutboundType(4), false, false},
		{"未知类型 7", outbound.OutboundType(7), false, false},
	}

	for _, tc := range cases {
		if got, want := isImagesEndpointCompatibleChannelType(tc.typ), tc.wantImages; got != want {
			t.Errorf("%s (type=%d) 图片可接 = %v, want %v", tc.name, int(tc.typ), got, want)
		}
		if got, want := isVideoCapableChannelType(tc.typ), tc.wantVideo; got != want {
			t.Errorf("%s (type=%d) 视频可接 = %v, want %v", tc.name, int(tc.typ), got, want)
		}
	}
}

// 两表的差异集合必须恰好是「图片多一个 Gemini」：
//   - 「图片能接但视频不能接」== {Gemini}，多一个都说明有人单边加了类型；
//   - 「视频能接但图片不能接」== 空，视频若比图片还宽，说明视频又用了别的表。
//
// 单独写这条是因为逐格矩阵只能证明"这一格是什么"，证明不了"差异就是我声明的那些"。
func TestImageAndVideoCapabilityDiffIsOnlyGemini(t *testing.T) {
	imagesOnly := []outbound.OutboundType{}
	videoOnly := []outbound.OutboundType{}
	for i := -1; i <= 15; i++ {
		typ := outbound.OutboundType(i)
		images := isImagesEndpointCompatibleChannelType(typ)
		video := isVideoCapableChannelType(typ)
		switch {
		case images && !video:
			imagesOnly = append(imagesOnly, typ)
		case video && !images:
			videoOnly = append(videoOnly, typ)
		}
	}

	if len(imagesOnly) != 1 || imagesOnly[0] != outbound.OutboundTypeGemini {
		t.Fatalf("「图片能接、视频不能接」的类型集合 = %v, want 恰好 [Gemini]", imagesOnly)
	}
	if len(videoOnly) != 0 {
		t.Fatalf("「视频能接、图片不能接」的类型集合 = %v, want 空（视频表不许比图片表宽）", videoOnly)
	}
}

// 视频表 == 图片表去掉 Gemini：这是本次「对齐」的正式表述，用代码写下来，
// 而不是靠两条 switch 碰巧长得一样。
func TestVideoTableEqualsImageTableWithoutGemini(t *testing.T) {
	for i := -1; i <= 15; i++ {
		typ := outbound.OutboundType(i)
		want := isOpenAIImagesCompatibleChannelType(typ)
		if got := isVideoCapableChannelType(typ); got != want {
			t.Errorf("type=%d 视频可接 = %v, want %v（= 图片基础表，不含 Gemini 那一支）", i, got, want)
		}
	}
	if isVideoCapableChannelType(outbound.OutboundTypeGemini) {
		t.Fatal("视频表不许含 Gemini：视频走 Gemini 原生协议还没接")
	}
}
