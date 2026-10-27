package relay

import "testing"

// 别名归一三态：同族不同后缀 → mismatch=true + code=alias_variant；
// 完全不同 → mismatch=true + code 空；任一名字为空 → nil 不回归。
func TestUpstreamModelMismatchAliasVariant(t *testing.T) {
	for _, tc := range []struct {
		name     string
		sent     string
		response string
		wantNil  bool
		want     bool
		wantCode string
	}{
		{name: "same family dated suffix is alias_variant", sent: "model-family-alpha", response: "model-family-alpha-20251101", want: true, wantCode: UpstreamMismatchCodeAliasVariant},
		{name: "claude-fable dated suffix is alias_variant", sent: "claude-fable-5-1", response: "claude-fable-5-1-20251101", want: true, wantCode: UpstreamMismatchCodeAliasVariant},
		{name: "claude-fable versus unrelated model", sent: "claude-fable-5-1", response: "totally-other-model", want: true, wantCode: ""},
		{name: "same family different minor suffix", sent: "model-family-alpha-8", response: "model-family-alpha-8-20251101", want: true, wantCode: UpstreamMismatchCodeAliasVariant},
		{name: "case and whitespace only, same name", sent: " Model-Family-Alpha ", response: "model-family-alpha", want: false, wantCode: ""},
		{name: "completely different names", sent: "model-family-alpha", response: "model-family-beta", want: true, wantCode: ""},
		{name: "same stem with non-separator suffix is not the same family", sent: "model-family-alpha", response: "model-family-alphaX", want: true, wantCode: ""},
		{name: "shared prefix too short is not alias", sent: "mod", response: "modular-something-20251101", want: true, wantCode: ""},
		// 受控命名空间归一: 只剥一层 "cn:"(大小写不敏感), 之后完整 EqualFold。
		{name: "cn namespace vs bare is the same model", sent: "cn:model-family-alpha", response: "model-family-alpha", want: false, wantCode: ""},
		{name: "bare vs cn namespace is the same model", sent: "model-family-alpha", response: "cn:model-family-alpha", want: false, wantCode: ""},
		// 生产误报: 请求 cn:kimi-k3.1-pro, 映射发出 global:kimi-k3.1-pro, 上游回显裸名。
		{name: "global namespace vs bare is the same model", sent: "global:kimi-k3.1-pro", response: "kimi-k3.1-pro", want: false, wantCode: ""},
		{name: "cn vs global namespace is the same model", sent: "cn:kimi-k3.1-pro", response: "global:kimi-k3.1-pro", want: false, wantCode: ""},
		{name: "GLOBAL namespace is case insensitive", sent: "GLOBAL:Kimi-K3.1-Pro", response: "kimi-k3.1-pro", want: false, wantCode: ""},
		{name: "uppercase CN namespace still the same model", sent: "CN:model-family-alpha", response: "model-family-alpha", want: false, wantCode: ""},
		{name: "namespace does not hide a different model", sent: "cn:model-family-alpha", response: "cn:model-family-beta", want: true, wantCode: ""},
		{name: "namespace does not unify versions", sent: "cn:model-family-alpha-1", response: "cn:model-family-alpha-2", want: true, wantCode: ""},
		{name: "colon version suffix is not a namespace", sent: "model-family-alpha:free", response: "model-family-alpha", want: true, wantCode: ""},
		// moonshot 规范回显家族 (astra 裁决 2026-10-27): 上游只回自家规范 id,
		// 与请求名没有公共字符串前缀, 前缀判据必漏。降级到 alias_variant, 不否认 mismatch 事实。
		{name: "kimi request, vendor-prefixed canonical echo", sent: "kimi-3.1-pro", response: "moonshotai/Kimi-K3", want: true, wantCode: UpstreamMismatchCodeAliasVariant},
		{name: "kimi request, older version echo", sent: "kimi-3.1-pro", response: "kimi-k2.5", want: true, wantCode: UpstreamMismatchCodeAliasVariant},
		{name: "kimi request, sibling version echo", sent: "kimi-3.1-pro", response: "kimi-k3", want: true, wantCode: UpstreamMismatchCodeAliasVariant},
		{name: "kimi canonical echo is case insensitive", sent: "Kimi-3.1-Pro", response: "moonshotai/KIMI-K3", want: true, wantCode: UpstreamMismatchCodeAliasVariant},
		{name: "kimi canonical echo with other vendor prefix", sent: "kimi-3.1-pro", response: "moonshot/kimi-k2.5", want: true, wantCode: UpstreamMismatchCodeAliasVariant},
		// 白名单不外溢: 真掺假仍然跨族报; 同族不同款仍不给变体码。
		{name: "kimi swapped for gpt still cross-family", sent: "kimi-3.1-pro", response: "gpt-5.2", want: true, wantCode: UpstreamMismatchCodeCrossFamily},
		{name: "kimi swapped for claude still cross-family", sent: "kimi-3.1-pro", response: "claude-sonnet-4-5", want: true, wantCode: UpstreamMismatchCodeCrossFamily},
		{name: "kimi swapped for deepseek still cross-family", sent: "kimi-3.1-pro", response: "deepseek-v4-pro", want: true, wantCode: UpstreamMismatchCodeCrossFamily},
		{name: "claude opus versus sonnet keeps name-mismatch", sent: "claude-opus-4-8", response: "claude-sonnet-4-5", want: true, wantCode: ""},
		{name: "deepseek pro versus lite keeps name-mismatch", sent: "deepseek-v4-pro", response: "deepseek-v4-lite", want: true, wantCode: ""},
		// 跨族 code 优先于别名: 两名均能被家族表识别且无交集。
		{name: "cross-family names get cross_family code", sent: "deepseek-v3", response: "gemini-2.5-pro", want: true, wantCode: UpstreamMismatchCodeCrossFamily},
		{name: "cn namespace does not stop cross-family detection", sent: "cn:deepseek-v3", response: "gemini-2.5-pro", want: true, wantCode: UpstreamMismatchCodeCrossFamily},
		{name: "empty sent", sent: "", response: "model-family-alpha", wantNil: true, wantCode: ""},
		{name: "empty response", sent: "model-family-alpha", response: "", wantNil: true, wantCode: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mismatch := upstreamModelMismatch(tc.sent, tc.response)
			if tc.wantNil {
				if mismatch != nil {
					t.Fatalf("upstreamModelMismatch(%q,%q) = %v, want nil", tc.sent, tc.response, *mismatch)
				}
				if code := UpstreamModelMismatchCode(mismatch, tc.sent, tc.response); code != "" {
					t.Fatalf("code for nil mismatch = %q, want empty", code)
				}
				return
			}
			if mismatch == nil {
				t.Fatalf("upstreamModelMismatch(%q,%q) = nil, want %v", tc.sent, tc.response, tc.want)
			}
			if *mismatch != tc.want {
				t.Fatalf("upstreamModelMismatch(%q,%q) = %v, want %v", tc.sent, tc.response, *mismatch, tc.want)
			}
			if got := UpstreamModelMismatchCode(mismatch, tc.sent, tc.response); got != tc.wantCode {
				t.Fatalf("UpstreamModelMismatchCode(%q,%q) = %q, want %q", tc.sent, tc.response, got, tc.wantCode)
			}
		})
	}
}

func TestUpstreamAliasNormalize(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Model-Family-Alpha-20251101", "model-family-alpha"},
		{"model_family_alpha_20251101", "model_family_alpha"},
		{"model.alpha.20251101", "model.alpha"},
		{"model-family-alpha", "model-family-alpha"},
		{"20251101", "20251101"}, // 整名就是 8 位数字：剥掉后为空，判 nil，不算同族
		{"", ""},
		{"  Model-Family-Beta  ", "model-family-beta"},
	} {
		if got := upstreamAliasNormalize(tc.in); got != tc.want {
			t.Fatalf("upstreamAliasNormalize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// 命名空间归一: 只剥一层 "cn:"(trim 后大小写不敏感), 不碰 '/' 前缀、多层名或冒号版本后缀。
func TestModelNamespaceStripped(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"cn:deepseek-v4", "deepseek-v4"},
		{"global:kimi-k3.1-pro", "kimi-k3.1-pro"},
		{"GLOBAL:Kimi-K3.1-Pro", "Kimi-K3.1-Pro"},
		{"global:", ""},
		{"CN:deepseek-v4", "deepseek-v4"},
		{"  cn: deepseek-v4  ", "deepseek-v4"},
		{"cn:gpt-5:free", "gpt-5:free"},
		{"vendor/deepseek-v4", "vendor/deepseek-v4"}, // 只剥 cn:, 不碰 '/' 前缀
		{"gpt-5", "gpt-5"},
		{"cn:", ""},
		{"", ""},
	} {
		if got := modelNamespaceStripped(tc.in); got != tc.want {
			t.Fatalf("modelNamespaceStripped(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
