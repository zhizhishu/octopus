package openai

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
)

func TestOpenAIOutboundMergesInboundQuery(t *testing.T) {
	content := "hello"
	tests := []struct {
		name    string
		adapter interface {
			TransformRequest(context.Context, *model.InternalLLMRequest, string, string) (*http.Request, error)
		}
		wantPath string
	}{
		{name: "chat", adapter: &ChatOutbound{}, wantPath: "/api/provider/openai/v1/chat/completions"},
		{name: "responses", adapter: &ResponseOutbound{}, wantPath: "/api/provider/openai/v1/responses"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &model.InternalLLMRequest{
				Model: "gpt-test",
				Messages: []model.Message{{
					Role: "user",
					Content: model.MessageContent{
						Content: &content,
					},
				}},
				Query: url.Values{
					"trace": []string{"client"},
					"debug": []string{"1"},
				},
			}
			httpReq, err := tt.adapter.TransformRequest(context.Background(), req, "https://example.test/api/provider/openai?trace=base&base=1", "sk-test")
			if err != nil {
				t.Fatalf("transform request: %v", err)
			}
			if httpReq.URL.Path != tt.wantPath {
				t.Fatalf("unexpected path: %q", httpReq.URL.Path)
			}
			q := httpReq.URL.Query()
			if q.Get("base") != "1" || q.Get("trace") != "client" || q.Get("debug") != "1" {
				t.Fatalf("unexpected query: %s", httpReq.URL.RawQuery)
			}
		})
	}
}
