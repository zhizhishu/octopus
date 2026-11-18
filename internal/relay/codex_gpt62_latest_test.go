package relay

import (
	"github.com/zhizhishu/octopus/internal/transformer/model"
	"testing"
)

func TestGPT62CodexReasoningEffortNewModels(t *testing.T) {
	for _, tc := range []struct{ model, from, want string }{
		{"gpt-6.2-astra", "none", "low"},
		{"gpt-6.2-sol", "minimal", "low"},
		{"openai/gpt-6.2-sol-2026-11-17", "x-high", "xhigh"},
		{"gpt-6.2-astra", "max", "max"},
		{"gpt-6.2-sol", "", ""},
		{"gpt-6.2-sol-preview", "none", "none"},
		{"gpt-6.1-terra", "none", "none"},
	} {
		t.Run(tc.model+"/"+tc.from, func(t *testing.T) {
			req := &model.InternalLLMRequest{Model: tc.model, ReasoningEffort: tc.from}
			normalizeCodexReasoningEffort(req)
			if req.ReasoningEffort != tc.want {
				t.Fatalf("effort %q => %q, want %q", tc.from, req.ReasoningEffort, tc.want)
			}
		})
	}
}
