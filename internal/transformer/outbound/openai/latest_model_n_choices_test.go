package openai

import (
	"context"
	"fmt"
	"github.com/zhizhishu/octopus/internal/transformer/model"
	"strings"
	"testing"
)

func TestGPT62OfficialChatSingleChoiceContract(t *testing.T) {
	for _, n := range []int64{1, 2} {
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			prompt := "hello"
			req := &model.InternalLLMRequest{Model: "gpt-6.2-astra", N: &n, Messages: []model.Message{{Role: "user", Content: model.MessageContent{Content: &prompt}}}}
			if n == 1 {
				body := chatBodyWithBase(t, req, "https://api.openai.com/v1")
				if _, ok := body["n"]; ok {
					t.Fatalf("n=1 must be omitted for GPT-6.2: %#v", body)
				}
			} else {
				_, err := (&ChatOutbound{}).TransformRequest(context.Background(), req, "https://api.openai.com/v1", "dummy")
				if err == nil || !strings.Contains(err.Error(), "single Chat choice") {
					t.Fatalf("n=2 should produce actionable error: %v", err)
				}
			}
			if req.N == nil || *req.N != n {
				t.Fatal("request n mutated across attempts")
			}
		})
	}
}
