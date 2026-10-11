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
		{name: "same family different minor suffix", sent: "model-family-alpha-8", response: "model-family-alpha-8-20251101", want: true, wantCode: UpstreamMismatchCodeAliasVariant},
		{name: "case and whitespace only, same name", sent: " Model-Family-Alpha ", response: "model-family-alpha", want: false, wantCode: ""},
		{name: "completely different names", sent: "model-family-alpha", response: "model-family-beta", want: true, wantCode: ""},
		{name: "shared prefix too short is not alias", sent: "mod", response: "modular-something-20251101", want: true, wantCode: ""},
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
