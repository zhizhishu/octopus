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
