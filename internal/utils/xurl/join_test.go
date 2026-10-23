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
			name:     "custom relative endpoint keeps provider prefix",
			baseURL:  "https://open.bigmodel.cn/api/coding/paas/v4",
			endpoint: "/chat/completions",
			want:     "https://open.bigmodel.cn/api/coding/paas/v4/chat/completions",
		},
		{
			name:     "full endpoint override is used directly",
			baseURL:  "https://ignored.example/v1",
			endpoint: "https://open.bigmodel.cn/api/coding/paas/v4/chat/completions",
			want:     "https://open.bigmodel.cn/api/coding/paas/v4/chat/completions",
		},
		{
			name:     "base already includes full domestic chat endpoint",
			baseURL:  "https://open.bigmodel.cn/api/coding/paas/v4/chat/completions",
			endpoint: "/v1/chat/completions",
			want:     "https://open.bigmodel.cn/api/coding/paas/v4/chat/completions",
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
		{"chat 端点只给后缀，base 的 /v1 必须保留", JoinCustomOpenAIChatPath,
			"https://api.example/v1", "/chat/completions", "https://api.example/v1/chat/completions"},
		{"chat 端点给完整路径", JoinCustomOpenAIChatPath,
			"https://api.example/v1", "/v1/chat/completions", "https://api.example/v1/chat/completions"},
		{"chat 端点重复 v1 不得叠成 /v1/v1", JoinCustomOpenAIChatPath,
			"https://api.example/v1", "/v1/v1/chat/completions", "https://api.example/v1/chat/completions"},
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
