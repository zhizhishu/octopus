package relay

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/gin-gonic/gin"
)

// declaredModelOutbound 模拟一个上游: 它在响应体里自报 model 名字。
type declaredModelOutbound struct{ declared string }

func (declaredModelOutbound) TransformRequest(context.Context, *model.InternalLLMRequest, string, string) (*http.Request, error) {
	return nil, nil
}

func (o declaredModelOutbound) TransformResponse(_ context.Context, response *http.Response) (*model.InternalLLMResponse, error) {
	if _, err := io.ReadAll(response.Body); err != nil {
		return nil, err
	}
	return &model.InternalLLMResponse{Model: o.declared}, nil
}

func (declaredModelOutbound) TransformStream(context.Context, []byte) (*model.InternalLLMResponse, error) {
	return nil, nil
}

type declaredModelInbound struct{}

func (declaredModelInbound) TransformRequest(context.Context, []byte) (*model.InternalLLMRequest, error) {
	return nil, nil
}

func (declaredModelInbound) TransformResponse(context.Context, *model.InternalLLMResponse) ([]byte, error) {
	return []byte(`{}`), nil
}

func (declaredModelInbound) TransformStream(context.Context, *model.InternalLLMResponse) ([]byte, error) {
	return nil, nil
}

func (declaredModelInbound) GetInternalResponse(context.Context) (*model.InternalLLMResponse, error) {
	return nil, nil
}

func TestUpstreamModelMismatchIsTriState(t *testing.T) {
	// 两种"没数据"都必须回 nil(无法判定), 不能被读成"有问题"——上游不回 model 的渠道很常见,
	// 失败/本地校验路径也经常没有出站模型名。
	for _, tc := range []struct {
		name     string
		sent     string
		response string
		wantNil  bool
		want     bool
	}{
		{name: "both empty", sent: "", response: "", wantNil: true},
		{name: "upstream declared nothing", sent: "gpt-5.6-sol", response: "", wantNil: true},
		{name: "upstream declared whitespace", sent: "gpt-5.6-sol", response: "   ", wantNil: true},
		{name: "we do not know what we sent", sent: "  ", response: "gpt-5.6-sol", wantNil: true},
		{name: "identical", sent: "claude-opus-4-8", response: "claude-opus-4-8", want: false},
		{name: "case differs only", sent: "Claude-Opus-4-8", response: "claude-opus-4-8", want: false},
		{name: "different model", sent: "claude-opus-4-8", response: "claude-haiku-4-5", want: true},
		{name: "dated suffix counts as mismatch", sent: "claude-opus-4-8", response: "claude-opus-4-8-20251101", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := upstreamModelMismatch(tc.sent, tc.response)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("upstreamModelMismatch(%q, %q) = %v, want nil (cannot judge)", tc.sent, tc.response, *got)
				}
				return
			}
			if got == nil {
				t.Fatalf("upstreamModelMismatch(%q, %q) = nil, want %v", tc.sent, tc.response, tc.want)
			}
			if *got != tc.want {
				t.Fatalf("upstreamModelMismatch(%q, %q) = %v, want %v", tc.sent, tc.response, *got, tc.want)
			}
		})
	}
}

func TestSetUpstreamDeclaredModelKeepsFirstNonEmpty(t *testing.T) {
	m := &RelayMetrics{}

	m.SetUpstreamDeclaredModel("")
	if m.UpstreamDeclaredModel != "" {
		t.Fatalf("empty declaration must not become a value, got %q", m.UpstreamDeclaredModel)
	}

	m.SetUpstreamDeclaredModel("  upstream-first  ")
	if m.UpstreamDeclaredModel != "upstream-first" {
		t.Fatalf("UpstreamDeclaredModel = %q, want trimmed upstream-first", m.UpstreamDeclaredModel)
	}

	// 流式每个分片通常都带同一个 model; 首个非空胜出, 后续不得改写已记录的观测。
	m.SetUpstreamDeclaredModel("upstream-second")
	if m.UpstreamDeclaredModel != "upstream-first" {
		t.Fatalf("UpstreamDeclaredModel = %q, want first declaration to win", m.UpstreamDeclaredModel)
	}
}

// 这是本特性的核心不变量: 开启 model_mapping 时 relay 会把 internalResponse.Model 覆盖成
// 客户端可见名(防止上游身份外泄)。捕获必须发生在那一步之前, 否则"上游回显审计"记下来的
// 永远是请求模型名, 这个字段就废了。
func TestHandleResponseCapturesUpstreamModelBeforeMappingRewrite(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())

	metrics := &RelayMetrics{}
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			c:            c,
			inAdapter:    declaredModelInbound{},
			metrics:      metrics,
			requestModel: "client-visible-name",
		},
		modelMapped: true,
	}
	response := &http.Response{Body: io.NopCloser(strings.NewReader("{}"))}

	if err := ra.handleResponse(context.Background(), response, declaredModelOutbound{declared: "upstream-real-name"}); err != nil {
		t.Fatalf("handleResponse: %v", err)
	}

	if metrics.UpstreamDeclaredModel != "upstream-real-name" {
		t.Fatalf("UpstreamDeclaredModel = %q, want %q — capture must run before the model_mapping rewrite",
			metrics.UpstreamDeclaredModel, "upstream-real-name")
	}
}

func TestHandleResponseLeavesUpstreamModelEmptyWhenUpstreamDeclaresNothing(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())

	metrics := &RelayMetrics{}
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			c:         c,
			inAdapter: declaredModelInbound{},
			metrics:   metrics,
		},
	}
	response := &http.Response{Body: io.NopCloser(strings.NewReader("{}"))}

	if err := ra.handleResponse(context.Background(), response, declaredModelOutbound{declared: ""}); err != nil {
		t.Fatalf("handleResponse: %v", err)
	}

	if metrics.UpstreamDeclaredModel != "" {
		t.Fatalf("UpstreamDeclaredModel = %q, want empty when the upstream declares nothing", metrics.UpstreamDeclaredModel)
	}
	if mismatch := upstreamModelMismatch("gpt-5.6-sol", metrics.UpstreamDeclaredModel); mismatch != nil {
		t.Fatalf("mismatch = %v, want nil (third state) when the upstream declares nothing", *mismatch)
	}
}
