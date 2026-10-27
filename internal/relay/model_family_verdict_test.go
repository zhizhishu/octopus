package relay

import "testing"

// 三档判定: 一致同族 → trusted; 别名疑似 → suspect; 跨家族 → suspect; 上游没自报 → unknown。
func TestEvalModelFamilyVerdictThreeTiers(t *testing.T) {
	for _, tc := range []struct {
		name           string
		requestModel   string
		actualModel    string
		upstreamEcho   string
		mismatchCode   string
		wantVerdict    string
		wantVendorHint string
	}{
		{
			name:         "same family consistent -> trusted",
			requestModel: "deepseek-v4",
			actualModel:  "deepseek-v4",
			upstreamEcho: "deepseek-v4",
			wantVerdict:  ModelFamilyVerdictTrusted,
		},
		{
			// 生产实证（2026-10-27，150 条）：请求 cn:kimi-k3.1-pro，渠道映射发出
			// global:kimi-k3.1-pro，上游回显裸名。两个命名空间都是路由层加的前缀，
			// 不是模型身份——同一模型不许判成「回显不符」。
			name:         "cn/global namespace prefix around the same model -> trusted",
			requestModel: "cn:kimi-k3.1-pro",
			actualModel:  "global:kimi-k3.1-pro",
			upstreamEcho: "kimi-k3.1-pro",
			wantVerdict:  ModelFamilyVerdictTrusted,
		},
		{
			name:         "consistent case/whitespace only -> trusted",
			requestModel: "DeepSeek-V4",
			actualModel:  " deepseek-v4 ",
			upstreamEcho: "deepseek-v4",
			wantVerdict:  ModelFamilyVerdictTrusted,
		},
		{
			name:         "same-family alias variant date suffix -> suspect",
			requestModel: "claude-fable-5-1",
			actualModel:  "claude-fable-5-1",
			upstreamEcho: "claude-fable-5-1-20251101",
			mismatchCode: UpstreamMismatchCodeAliasVariant,
			wantVerdict:  ModelFamilyVerdictSuspect,
		},
		{
			name:         "cross-family echo differs (deepseek sent, gemini echoed) -> suspect",
			requestModel: "deepseek-v3",
			actualModel:  "deepseek-v3",
			upstreamEcho: "gemini-2.5-pro",
			wantVerdict:  ModelFamilyVerdictSuspect,
		},
		{
			name:         "cross-family mapping: echo matches what we sent but not what client asked -> suspect",
			requestModel: "deepseek-v3",
			actualModel:  "gemini-2.5-pro",
			upstreamEcho: "gemini-2.5-pro",
			wantVerdict:  ModelFamilyVerdictSuspect,
		},
		{
			name:         "empty echo -> unknown",
			requestModel: "deepseek-v3",
			actualModel:  "deepseek-v3",
			upstreamEcho: "",
			wantVerdict:  ModelFamilyVerdictUnknown,
		},
		{
			name:         "no outbound name -> unknown",
			requestModel: "deepseek-v3",
			actualModel:  "",
			upstreamEcho: "deepseek-v3",
			wantVerdict:  ModelFamilyVerdictUnknown,
		},
		{
			name:           "vendor-prefixed consistent -> trusted + vendor hint from echo",
			requestModel:   "deepseek-v4",
			actualModel:    "vercel-deepseek/deepseek-v4",
			upstreamEcho:   "vercel-deepseek/deepseek-v4",
			wantVerdict:    ModelFamilyVerdictTrusted,
			wantVendorHint: "vercel-deepseek",
		},
		{
			name:           "colon-prefixed echo -> trusted + vendor hint",
			requestModel:   "gpt-5",
			actualModel:    "vendor:gpt-5",
			upstreamEcho:   "vendor:gpt-5",
			wantVerdict:    ModelFamilyVerdictTrusted,
			wantVendorHint: "vendor",
		},
		{
			name:         "cn namespace echo vs bare sent -> trusted (not cross-family)",
			requestModel: "gpt-5",
			actualModel:  "cn:gpt-5",
			upstreamEcho: "gpt-5",
			wantVerdict:  ModelFamilyVerdictTrusted,
		},
		{
			name:           "cn namespace-only difference -> trusted + vendor hint from echo",
			requestModel:   "deepseek-v4",
			actualModel:    "deepseek-v4",
			upstreamEcho:   "cn:deepseek-v4",
			wantVerdict:    ModelFamilyVerdictTrusted,
			wantVendorHint: "cn",
		},
		{
			name:           "cn namespace does not hide a cross-family echo -> suspect",
			requestModel:   "gpt-5",
			actualModel:    "gpt-5",
			upstreamEcho:   "cn:gemini-2.5-pro",
			wantVerdict:    ModelFamilyVerdictSuspect,
			wantVendorHint: "cn",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotVerdict, gotVendorHint := evalModelFamilyVerdict(tc.requestModel, tc.actualModel, tc.upstreamEcho, tc.mismatchCode)
			if gotVerdict != tc.wantVerdict {
				t.Fatalf("evalModelFamilyVerdict(%q,%q,%q,%q) verdict = %q, want %q",
					tc.requestModel, tc.actualModel, tc.upstreamEcho, tc.mismatchCode, gotVerdict, tc.wantVerdict)
			}
			if gotVendorHint != tc.wantVendorHint {
				t.Fatalf("evalModelFamilyVerdict(%q,%q,%q,%q) vendorHint = %q, want %q",
					tc.requestModel, tc.actualModel, tc.upstreamEcho, tc.mismatchCode, gotVendorHint, tc.wantVendorHint)
			}
		})
	}
}

// vendor 前缀提取: 只认最后一个 '/' 或 ':', 无前缀/分隔符在首尾时留空——事实提取, 不猜造。
func TestModelVendorHintExtraction(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"vercel-deepseek/deepseek-v4", "vercel-deepseek"},
		{"vendor:gpt-x", "vendor"},
		{"openai/gpt-x", "openai"},
		{"a/b/c", "a/b"}, // 取最后一个分隔符之前, 与 price 的 LastIndexAny 一致
		{"deepseek-v4", ""},
		{"/gpt-x", ""},  // 分隔符在首位, 前缀为空
		{"vendor:", ""}, // 分隔符在末位, 模型部分为空
		{"", ""},
		{"   ", ""},
	} {
		t.Run(tc.in, func(t *testing.T) {
			if got := modelVendorHint(tc.in); got != tc.want {
				t.Fatalf("modelVendorHint(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
