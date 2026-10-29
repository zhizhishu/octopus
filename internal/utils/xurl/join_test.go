package xurl

import "testing"

func TestJoinPathNormalizesVersionedEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		baseURL  string
		endpoint string
		want     string
	}{
		{
			name:     "base without version",
			baseURL:  "https://api.anthropic.com",
			endpoint: "/v1/messages",
			want:     "https://api.anthropic.com/v1/messages",
		},
		{
			name:     "base with version",
			baseURL:  "https://api.anthropic.com/v1",
			endpoint: "/v1/messages",
			want:     "https://api.anthropic.com/v1/messages",
		},
		{
			name:     "base with v1beta version",
			baseURL:  "https://generativelanguage.googleapis.com/v1beta",
			endpoint: "/v1beta/models/gemini-pro:generateContent",
			want:     "https://generativelanguage.googleapis.com/v1beta/models/gemini-pro:generateContent",
		},
		{
			name:     "proxy path with version",
			baseURL:  "https://proxy.example/anthropic/v1/",
			endpoint: "/v1/models",
			want:     "https://proxy.example/anthropic/v1/models",
		},
		{
			name:     "proxy path without version",
			baseURL:  "https://proxy.example/anthropic",
			endpoint: "/v1/models",
			want:     "https://proxy.example/anthropic/v1/models",
		},
		{
			name:     "base already includes endpoint",
			baseURL:  "https://api.anthropic.com/v1/messages",
			endpoint: "/v1/messages",
			want:     "https://api.anthropic.com/v1/messages",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := JoinPath(tt.baseURL, tt.endpoint)
			if err != nil {
				t.Fatalf("JoinPath returned error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("JoinPath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestJoinOpenAIPathCanonicalizesCommonBaseURLSuffixes(t *testing.T) {
	tests := []struct {
		name     string
		baseURL  string
		endpoint string
		want     string
	}{
		{
			name:     "root base chat",
			baseURL:  "https://api.openai.com",
			endpoint: "/v1/chat/completions",
			want:     "https://api.openai.com/v1/chat/completions",
		},
		{
			name:     "base with v1 suffix responses",
			baseURL:  "https://api.openai.com/v1",
			endpoint: "/v1/responses",
			want:     "https://api.openai.com/v1/responses",
		},
		{
			name:     "base accidentally includes chat endpoint",
			baseURL:  "https://api.openai.com/v1/chat/completions",
			endpoint: "/v1/responses",
			want:     "https://api.openai.com/v1/responses",
		},
		{
			name:     "legacy chat suffix upgrades to standard v1 chat",
			baseURL:  "https://api.openai.com/chat/completions",
			endpoint: "/v1/chat/completions",
			want:     "https://api.openai.com/v1/chat/completions",
		},
		{
			name:     "cliproxy provider prefix is preserved",
			baseURL:  "https://cliproxy.example/api/provider/openai",
			endpoint: "/v1/chat/completions",
			want:     "https://cliproxy.example/api/provider/openai/v1/chat/completions",
		},
		{
			name:     "model sync strips accidental responses suffix",
			baseURL:  "https://cliproxy.example/api/provider/openai/v1/responses",
			endpoint: "/v1/models",
			want:     "https://cliproxy.example/api/provider/openai/v1/models",
		},
		{
			name:     "responses compact strips accidental compact suffix",
			baseURL:  "https://cliproxy.example/api/provider/openai/responses/compact",
			endpoint: "/v1/responses/compact",
			want:     "https://cliproxy.example/api/provider/openai/v1/responses/compact",
		},
		{
			name:     "raw rerank uses canonical v1 path",
			baseURL:  "https://api.example.test/rerank",
			endpoint: "/v1/rerank",
			want:     "https://api.example.test/v1/rerank",
		},
		// systemone（决策类上游）的四种 base 填法都必须收敛到同一个正确地址。
		//
		// 这些用例钉的是一个真实缺陷：渠道面板上把 base 填成
		// https://host/v1/systemone（用户照着上游文档原样抄），旧实现只剥
		// /v1/chat/completions 那类后缀，systemone 原样留着，于是拼出
		// https://host/v1/systemone/v1/systemone —— 上游 404 Unsupported serving path。
		// 修法是在 JoinOpenAIPath 的后缀表里登记 /v1/systemone 与 /systemone。
		{
			name:     "systemone base without path uses canonical v1 path",
			baseURL:  "https://api.example.test",
			endpoint: "/v1/systemone",
			want:     "https://api.example.test/v1/systemone",
		},
		{
			name:     "systemone base with v1 suffix is not doubled",
			baseURL:  "https://api.example.test/v1",
			endpoint: "/v1/systemone",
			want:     "https://api.example.test/v1/systemone",
		},
		{
			name:     "systemone base accidentally includes full endpoint",
			baseURL:  "https://api.example.test/v1/systemone",
			endpoint: "/v1/systemone",
			want:     "https://api.example.test/v1/systemone",
		},
		{
			name:     "systemone base accidentally includes bare endpoint",
			baseURL:  "https://api.example.test/systemone",
			endpoint: "/v1/systemone",
			want:     "https://api.example.test/v1/systemone",
		},
		{
			name:     "systemone preserves vendor path prefix",
			baseURL:  "https://gateway.example/api/decision",
			endpoint: "/v1/systemone",
			want:     "https://gateway.example/api/decision/v1/systemone",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := JoinOpenAIPath(tt.baseURL, tt.endpoint)
			if err != nil {
				t.Fatalf("JoinOpenAIPath returned error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("JoinOpenAIPath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestJoinCustomOpenAIChatPathAcceptsPathOrFullEndpointOverride(t *testing.T) {
	tests := []struct {
		name     string
		baseURL  string
		endpoint string
		want     string
	}{
		{
			name:     "relative endpoint is not appended",
			baseURL:  "https://open.bigmodel.cn/api/coding/paas/v4",
			endpoint: "/chat/completions",
			want:     "https://open.bigmodel.cn/api/coding/paas/v4",
		},
		{
			name:     "full endpoint override is used directly",
			baseURL:  "https://ignored.example/v1",
			endpoint: "https://open.bigmodel.cn/api/coding/paas/v4/chat/completions",
			want:     "https://open.bigmodel.cn/api/coding/paas/v4/chat/completions",
		},
		{
			name:     "base is sent verbatim even when it is not a chat endpoint",
			baseURL:  "https://proxy.example/custom/v1/chat/completions/",
			endpoint: "/v1/chat/completions",
			want:     "https://proxy.example/custom/v1/chat/completions/",
		},
		{
			name:     "query string and trailing slash stay",
			baseURL:  "https://proxy.example/custom?foo=1",
			endpoint: "/v1/chat/completions",
			want:     "https://proxy.example/custom?foo=1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := JoinCustomOpenAIChatPath(tt.baseURL, tt.endpoint)
			if err != nil {
				t.Fatalf("JoinCustomOpenAIChatPath returned error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("JoinCustomOpenAIChatPath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLegacyJoinCustomOpenAIChatPathStillAppendsChatEndpoint(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		want    string
	}{
		{name: "root", baseURL: "https://api.example.com", want: "https://api.example.com/v1/chat/completions"},
		{name: "v1", baseURL: "https://api.example.com/v1", want: "https://api.example.com/v1/chat/completions"},
		{name: "already chat", baseURL: "https://api.example.com/v1/chat/completions", want: "https://api.example.com/v1/chat/completions"},
		{name: "trailing slash on v1", baseURL: "https://api.example.com/v1/", want: "https://api.example.com/v1/chat/completions"},
		{name: "trailing slash on chat", baseURL: "https://api.example.com/v1/chat/completions/", want: "https://api.example.com/v1/chat/completions"},
		{name: "query", baseURL: "https://api.example.com/v1?foo=bar", want: "https://api.example.com/v1/chat/completions?foo=bar"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := LegacyJoinCustomOpenAIChatPath(tt.baseURL, "/v1/chat/completions")
			if err != nil {
				t.Fatalf("LegacyJoinCustomOpenAIChatPath: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
			again, err := LegacyJoinCustomOpenAIChatPath(got, "/v1/chat/completions")
			if err != nil {
				t.Fatalf("second join: %v", err)
			}
			if again != got {
				t.Fatalf("legacy join is not stable: %q -> %q", got, again)
			}
		})
	}
}

func TestJoinCustomOpenAIModelsOverrideAcceptsPathOrFullEndpointOverride(t *testing.T) {
	tests := []struct {
		name     string
		baseURL  string
		endpoint string
		want     string
	}{
		{
			name:     "custom relative models endpoint keeps provider prefix",
			baseURL:  "https://open.bigmodel.cn/api/coding/paas/v4",
			endpoint: "/models",
			want:     "https://open.bigmodel.cn/api/coding/paas/v4/models",
		},
		{
			name:     "full models endpoint override is used directly",
			baseURL:  "https://ignored.example/v1",
			endpoint: "https://open.bigmodel.cn/api/coding/paas/v4/models",
			want:     "https://open.bigmodel.cn/api/coding/paas/v4/models",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := JoinCustomOpenAIModelsOverride(tt.baseURL, tt.endpoint)
			if err != nil {
				t.Fatalf("JoinCustomOpenAIModelsOverride returned error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("JoinCustomOpenAIModelsOverride() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestJoinGeminiPathCanonicalizesCommonBaseURLSuffixes(t *testing.T) {
	tests := []struct {
		name     string
		baseURL  string
		endpoint string
		want     string
	}{
		{
			name:     "root base generate content",
			baseURL:  "https://generativelanguage.googleapis.com",
			endpoint: "/v1beta/models/gemini-pro:generateContent",
			want:     "https://generativelanguage.googleapis.com/v1beta/models/gemini-pro:generateContent",
		},
		{
			name:     "base with v1beta suffix",
			baseURL:  "https://generativelanguage.googleapis.com/v1beta",
			endpoint: "/v1beta/models/gemini-pro:generateContent",
			want:     "https://generativelanguage.googleapis.com/v1beta/models/gemini-pro:generateContent",
		},
		{
			name:     "base accidentally includes models suffix",
			baseURL:  "https://generativelanguage.googleapis.com/v1beta/models",
			endpoint: "/v1beta/models/gemini-pro:generateContent",
			want:     "https://generativelanguage.googleapis.com/v1beta/models/gemini-pro:generateContent",
		},
		{
			name:     "base accidentally includes concrete generate endpoint",
			baseURL:  "https://generativelanguage.googleapis.com/v1beta/models/gemini-pro:generateContent",
			endpoint: "/v1beta/models/gemini-pro:streamGenerateContent",
			want:     "https://generativelanguage.googleapis.com/v1beta/models/gemini-pro:streamGenerateContent",
		},
		{
			name:     "cliproxy provider prefix is preserved",
			baseURL:  "https://cliproxy.example/api/provider/gemini",
			endpoint: "/v1beta/models/gemini-pro:generateContent",
			want:     "https://cliproxy.example/api/provider/gemini/v1beta/models/gemini-pro:generateContent",
		},
		{
			name:     "model sync strips accidental stream endpoint",
			baseURL:  "https://cliproxy.example/api/provider/gemini/v1beta/models/gemini-pro:streamGenerateContent",
			endpoint: "/v1beta/models",
			want:     "https://cliproxy.example/api/provider/gemini/v1beta/models",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := JoinGeminiPath(tt.baseURL, tt.endpoint)
			if err != nil {
				t.Fatalf("JoinGeminiPath returned error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("JoinGeminiPath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestJoinAnthropicPathCanonicalizesCommonBaseURLSuffixes(t *testing.T) {
	tests := []struct {
		name     string
		baseURL  string
		endpoint string
		want     string
	}{
		{
			name:     "root base",
			baseURL:  "https://api.anthropic.com",
			endpoint: "/v1/messages",
			want:     "https://api.anthropic.com/v1/messages",
		},
		{
			name:     "base with v1 suffix",
			baseURL:  "https://api.anthropic.com/v1",
			endpoint: "/v1/messages",
			want:     "https://api.anthropic.com/v1/messages",
		},
		{
			name:     "base accidentally includes messages endpoint",
			baseURL:  "https://api.anthropic.com/v1/messages",
			endpoint: "/v1/messages",
			want:     "https://api.anthropic.com/v1/messages",
		},
		{
			name:     "legacy messages suffix upgrades to standard v1 messages",
			baseURL:  "https://api.anthropic.com/messages",
			endpoint: "/v1/messages",
			want:     "https://api.anthropic.com/v1/messages",
		},
		{
			name:     "cliproxy provider prefix is preserved",
			baseURL:  "https://cliproxy.example/api/provider/anthropic",
			endpoint: "/v1/messages",
			want:     "https://cliproxy.example/api/provider/anthropic/v1/messages",
		},
		{
			name:     "cliproxy provider prefix with v1 suffix is preserved",
			baseURL:  "https://cliproxy.example/api/provider/anthropic/v1",
			endpoint: "/v1/messages",
			want:     "https://cliproxy.example/api/provider/anthropic/v1/messages",
		},
		{
			name:     "model sync strips accidental messages suffix",
			baseURL:  "https://cliproxy.example/api/provider/anthropic/v1/messages",
			endpoint: "/v1/models",
			want:     "https://cliproxy.example/api/provider/anthropic/v1/models",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := JoinAnthropicPath(tt.baseURL, tt.endpoint)
			if err != nil {
				t.Fatalf("JoinAnthropicPath returned error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("JoinAnthropicPath() = %q, want %q", got, tt.want)
			}
		})
	}
}

// 用户按表单提示填「完整路径」时，拼出来的地址必须是对的。
//
// 两处曾经一起出错：
//  1. TrimPathSuffixes 里列了裸的 "/v1"，把用户故意填在 base 里的版本段
//     无条件剥掉 —— base=https://x/v1 + 端点=/embeddings 拼出
//     https://x/embeddings，版本段消失。
//  2. JoinPath 只去掉「第一段重名」，于是火山那种
//     base=/api/v3 + 端点=/api/v3/embeddings 拼成 /api/v3/api/v3/embeddings。
//
// 这些都不是边角：表单明确推荐「火山这类上游请填完整路径」，照做就坏。
func TestCustomPathJoinKeepsBasePathAndAvoidsDoublePrefix(t *testing.T) {
	cases := []struct {
		name string
		join func(string, string) (string, error)
		base string
		ep   string
		want string
	}{
		{"chat 相对路径不再拼接，base 原样保留", JoinCustomOpenAIChatPath,
			"https://api.example/v1", "/chat/completions", "https://api.example/v1"},
		{"chat 端点给完整路径也不拼接", JoinCustomOpenAIChatPath,
			"https://api.example/v1", "/v1/chat/completions", "https://api.example/v1"},
		{"chat 端点重复 v1 也不拼接", JoinCustomOpenAIChatPath,
			"https://api.example/v1", "/v1/v1/chat/completions", "https://api.example/v1"},
		{"emb 端点只给后缀，base 的 /v1 必须保留", JoinCustomOpenAIEmbeddingsPath,
			"https://api.example/v1", "/embeddings", "https://api.example/v1/embeddings"},
		{"emb 端点给完整路径", JoinCustomOpenAIEmbeddingsPath,
			"https://api.example/v1", "/v1/embeddings", "https://api.example/v1/embeddings"},
		{"火山：base 带 /api/v3 + 完整端点，不得重复前置", JoinCustomOpenAIEmbeddingsPath,
			"https://api.volc.example/api/v3", "/api/v3/embeddings", "https://api.volc.example/api/v3/embeddings"},
		{"火山：base 带 /api/v3 + 只给后缀", JoinCustomOpenAIEmbeddingsPath,
			"https://api.volc.example/api/v3", "/embeddings", "https://api.volc.example/api/v3/embeddings"},
		{"base 无路径时补上标准路径", JoinCustomOpenAIEmbeddingsPath,
			"https://api.example", "/v1/embeddings", "https://api.example/v1/embeddings"},
	}
	for _, c := range cases {
		got, err := c.join(c.base, c.ep)
		if err != nil {
			t.Errorf("%s: 意外错误 %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: base=%q ep=%q -> %q, want %q", c.name, c.base, c.ep, got, c.want)
		}
	}
}

// 新增 "/systemone" 后缀的**影响面锁定**。
//
// 后缀表是 chat / embeddings / images / videos / models 等端点共用的，所以任何
// 新加的后缀都有可能顺带改变别的端点的拼接结果。2026-10-25 加 systemone 时，
// 唯一被波及的形态是「base 尾巴恰好是 /systemone」；正常上游（根地址、…/v1、
// 厂商前缀）拼接结果逐个钉在这里，改动一旦外溢立刻红。
//
// 这不是为了证明"能跑"，是为了证明**没有波及其他端点** —— 两者不是一回事。
func TestSystemoneSuffixDoesNotDisturbOtherEndpoints(t *testing.T) {
	// 1) 正常 base 形态：加 systemone 后缀前后必须完全不变。
	unaffected := []struct{ base, endpoint, want string }{
		{"https://upstream.example", "/v1/chat/completions", "https://upstream.example/v1/chat/completions"},
		{"https://upstream.example/v1", "/v1/chat/completions", "https://upstream.example/v1/chat/completions"},
		{"https://upstream.example/openai/v1", "/v1/chat/completions", "https://upstream.example/openai/v1/chat/completions"},
		{"https://upstream.example/v1", "/v1/embeddings", "https://upstream.example/v1/embeddings"},
		{"https://upstream.example/v1", "/v1/models", "https://upstream.example/v1/models"},
		{"https://upstream.example/v1", "/v1/images/generations", "https://upstream.example/v1/images/generations"},
		// 尾巴是常见服务段名的 base：新加任何后缀都必须不碰这里。
		{"https://upstream.example/api", "/v1/chat/completions", "https://upstream.example/api/v1/chat/completions"},
		{"https://upstream.example/openai", "/v1/chat/completions", "https://upstream.example/openai/v1/chat/completions"},
		{"https://upstream.example/v1beta", "/v1/chat/completions", "https://upstream.example/v1beta/v1/chat/completions"},
	}
	for _, c := range unaffected {
		got, err := JoinOpenAIPath(c.base, c.endpoint)
		if err != nil {
			t.Fatalf("JoinOpenAIPath(%q, %q): %v", c.base, c.endpoint, err)
		}
		if got != c.want {
			t.Fatalf("base=%q endpoint=%q 拼出 %q，要 %q。说明新增的 /systemone 后缀外溢到了正常端点。",
				c.base, c.endpoint, got, c.want)
		}
	}

	// 2) 唯一被波及的形态：base 尾巴是 /systemone 但目标是别的端点。
	//    这是**已知且接受**的代价（与 /rerank、/models 同源），钉在这里是为了
	//    它不会在无人知晓的情况下扩散或加重；哪天真要消歧，这里会提醒改造范围。
	collateral := []struct{ base, endpoint, want string }{
		{"https://upstream.example/my/systemone", "/v1/chat/completions", "https://upstream.example/my/v1/chat/completions"},
	}
	for _, c := range collateral {
		got, err := JoinOpenAIPath(c.base, c.endpoint)
		if err != nil {
			t.Fatalf("JoinOpenAIPath(%q, %q): %v", c.base, c.endpoint, err)
		}
		if got != c.want {
			t.Fatalf("已知副作用的形态变了：base=%q endpoint=%q 现在拼出 %q，记录的是 %q。"+
				"如果这是有意修正，请同时更新 join.go 的注释和这条用例。", c.base, c.endpoint, got, c.want)
		}
	}
}
