package modeltest

import "testing"

// A2：清单必须逐条照抄 new-api 的 common/model.go ImageGenerationModels。
//
// 这条测试锁的是"清单本体"，不是某个模型名能不能命中：任何一条被删、被改名、
// 被重排，这里立刻红。上面那堆 IsImageGenerationModel 命中用例只能证明"删掉的
// 那条恰好没有用例覆盖"之外的部分，锁不住清单完整性。
func TestImageGenerationModelPatternsMatchNewAPIList(t *testing.T) {
	want := []string{
		"dall-e-3",
		"dall-e-2",
		"prefix:dall-e",
		"gpt-image-",
		"qwen-image",
		"z-image",
		"wan2.7-image-pro",
		"wan2.7-image",
		"wan2.6-image",
		"wan2.6-t2i",
		"wan2.5-t2i-preview",
		"wan2.2-t2i-flash",
		"wan2.2-t2i-plus",
		"wanx2.1-t2i-turbo",
		"wanx2.1-t2i-plus",
		"wanx2.0-t2i-turbo",
		"prefix:imagen-",
		"flux-",
		"flux.1-",
	}
	if len(imageGenerationModelPatterns) != len(want) {
		t.Fatalf("清单条数 = %d, want %d（照抄 new-api，一条都不许少）", len(imageGenerationModelPatterns), len(want))
	}
	for i, pattern := range want {
		if imageGenerationModelPatterns[i] != pattern {
			t.Fatalf("清单第 %d 条 = %q, want %q", i, imageGenerationModelPatterns[i], pattern)
		}
	}
}

// A2：清单里**每一种**模式至少一条命中用例。
//
// 重叠模式（wan2.7-image 是 wan2.7-image-pro 的子串）刻意用能区分的最短名字，
// 这样每条用例只依赖它自己那一条模式。
func TestIsImageGenerationModelHitsEveryPattern(t *testing.T) {
	cases := []struct {
		model  string
		reason string
	}{
		{"dall-e-3", "dall-e-3"},
		{"dall-e-2", "dall-e-2"},
		{"dall-e-9-ultra", "prefix:dall-e（前缀）"},
		{"gpt-image-1", "gpt-image-"},
		{"qwen-image", "qwen-image"},
		{"z-image", "z-image"},
		{"wan2.7-image-pro", "wan2.7-image-pro"},
		{"wan2.7-image-mini", "wan2.7-image（子串，不含 -pro）"},
		{"wan2.6-image", "wan2.6-image"},
		{"wan2.6-t2i", "wan2.6-t2i"},
		{"wan2.5-t2i-preview", "wan2.5-t2i-preview"},
		{"wan2.2-t2i-flash", "wan2.2-t2i-flash"},
		{"wan2.2-t2i-plus", "wan2.2-t2i-plus"},
		{"wanx2.1-t2i-turbo", "wanx2.1-t2i-turbo"},
		{"wanx2.1-t2i-plus", "wanx2.1-t2i-plus"},
		{"wanx2.0-t2i-turbo", "wanx2.0-t2i-turbo"},
		{"imagen-4.0-generate-001", "prefix:imagen-（前缀）"},
		{"flux-pro-1.1", "flux-"},
		{"flux.1-dev", "flux.1-"},
		// 大小写不敏感（两边都小写后比）。
		{"DALL-E-3", "大小写归一"},
		{"GPT-Image-1", "大小写归一"},
		{"  gpt-image-1  ", "首尾空格归一"},
	}
	for _, tc := range cases {
		if !IsImageGenerationModel(tc.model) {
			t.Fatalf("IsImageGenerationModel(%q) = false, want true（模式：%s）", tc.model, tc.reason)
		}
	}
}

// A2：prefix: 的语义是**前缀**，不是子串。这条用例是它的唯一证据：
// "imagen-" 出现在中间时必须不命中（清单里没有别能命中它的模式）。
func TestIsImageGenerationModelPrefixPatternIsNotSubstring(t *testing.T) {
	if !IsImageGenerationModel("imagen-4") {
		t.Fatalf("imagen-4 必须以 imagen- 前缀命中")
	}
	if IsImageGenerationModel("my-imagen-4") {
		t.Fatalf("my-imagen-4 里 imagen- 不是前缀，prefix:imagen- 不许命中")
	}
	if !IsImageGenerationModel("dall-e-any") {
		t.Fatalf("dall-e-any 必须以 dall-e 前缀命中")
	}
	if IsImageGenerationModel("my-dall-e-clone") {
		t.Fatalf("my-dall-e-clone 里 dall-e 不是前缀，prefix:dall-e 不许命中")
	}
}

// A2：必须不命中的反例。非图片模型混进图片端点，等于让一次渠道测试去打一条
// 它根本不支持的路径，测出来的红/绿都没有意义。
func TestIsImageGenerationModelRejectsNonImageModels(t *testing.T) {
	for _, model := range []string{
		"gpt-4o",
		"claude-sonnet-4-5",
		"text-embedding-3-small",
		"",            // 空值：用户还没选模型
		"   ",         // 全空白
		"flux",        // 少一个分隔符，清单里是 flux- / flux.1-
		"imagen4",     // 少一个连字符
		"wanx-image",  // 清单里没有这个名字
		"gpt-4o-mini", // gpt-image- 不是 gpt-
		"dalle3",      // 清单里没有这种写法
	} {
		if IsImageGenerationModel(model) {
			t.Fatalf("IsImageGenerationModel(%q) = true, want false", model)
		}
	}
}
