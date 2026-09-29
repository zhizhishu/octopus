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

// declaredModelInbound 可配置内部响应, 让 collectResponse 走到提交点。
type declaredModelInbound struct {
	internal *model.InternalLLMResponse
}

func (declaredModelInbound) TransformRequest(context.Context, []byte) (*model.InternalLLMRequest, error) {
	return nil, nil
}

func (declaredModelInbound) TransformResponse(context.Context, *model.InternalLLMResponse) ([]byte, error) {
	return []byte(`{}`), nil
}

func (declaredModelInbound) TransformStream(context.Context, *model.InternalLLMResponse) ([]byte, error) {
	return nil, nil
}

func (d declaredModelInbound) GetInternalResponse(context.Context) (*model.InternalLLMResponse, error) {
	return d.internal, nil
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

// 「首个非空」的判定层从请求级移到了尝试级(F10): 请求级 metrics 只反映赢家,
// 失败尝试的声明不再挤占成功尝试的日志归属。旧测试钉的是请求级首个非空——
// 那正是把失败渠道的自报型号记到成功渠道头上的规则, 按新契约改写而非删除。
func TestUpstreamDeclaredModelCaptureIsAttemptScopedFirstNonEmpty(t *testing.T) {
	metrics := &RelayMetrics{}
	ra := &relayAttempt{relayRequest: &relayRequest{metrics: metrics}}

	// 空声明不落值。
	ra.captureUpstreamDeclaredModel(&model.InternalLLMResponse{Model: ""})
	if ra.upstreamDeclaredModel != "" {
		t.Fatalf("empty declaration must not become a value, got %q", ra.upstreamDeclaredModel)
	}

	// 尝试内首个非空胜出并去空白; 流式分片重复回显同一型号, 后续分片不改写。
	ra.captureUpstreamDeclaredModel(&model.InternalLLMResponse{Model: "  upstream-first  "})
	if ra.upstreamDeclaredModel != "upstream-first" {
		t.Fatalf("attempt declaration = %q, want trimmed upstream-first", ra.upstreamDeclaredModel)
	}
	ra.captureUpstreamDeclaredModel(&model.InternalLLMResponse{Model: "upstream-second"})
	if ra.upstreamDeclaredModel != "upstream-first" {
		t.Fatalf("attempt declaration = %q, want first declaration to win", ra.upstreamDeclaredModel)
	}

	// 请求级提交是无条件覆盖: 赢家不自报就是未知(空), 不沿用先前失败尝试的声明。
	metrics.CommitUpstreamDeclaredModel("winner-model")
	if metrics.UpstreamDeclaredModel != "winner-model" {
		t.Fatalf("UpstreamDeclaredModel = %q, want winner-model", metrics.UpstreamDeclaredModel)
	}
	metrics.CommitUpstreamDeclaredModel("")
	if metrics.UpstreamDeclaredModel != "" {
		t.Fatalf("winner without declaration must read as unknown, got %q", metrics.UpstreamDeclaredModel)
	}
}

// 这是本特性的核心不变量: 开启 model_mapping 时 relay 会把 internalResponse.Model 覆盖成
// 客户端可见名(防止上游身份外泄)。捕获必须发生在那一步之前, 否则"上游回显审计"记下来的
// 永远是请求模型名, 这个字段就废了。捕获落在尝试级, 由 collectResponse 提交到请求级。
func TestHandleResponseCapturesUpstreamModelBeforeMappingRewrite(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())

	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	metrics := &RelayMetrics{}
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			c:               c,
			inAdapter:       declaredModelInbound{internal: &model.InternalLLMResponse{Model: "client-visible-name"}},
			metrics:         metrics,
			requestModel:    "client-visible-name",
			internalRequest: &model.InternalLLMRequest{Model: "sent-upstream-name"},
		},
		modelMapped: true,
	}
	response := &http.Response{Body: io.NopCloser(strings.NewReader("{}"))}

	if err := ra.handleResponse(context.Background(), response, declaredModelOutbound{declared: "upstream-real-name"}); err != nil {
		t.Fatalf("handleResponse: %v", err)
	}

	if ra.upstreamDeclaredModel != "upstream-real-name" {
		t.Fatalf("attempt declaration = %q, want %q — capture must run before the model_mapping rewrite",
			ra.upstreamDeclaredModel, "upstream-real-name")
	}
	// 尝试收尾(collectResponse)把观测提交到请求级。
	ra.collectResponse()
	if metrics.UpstreamDeclaredModel != "upstream-real-name" {
		t.Fatalf("UpstreamDeclaredModel = %q, want %q after the winning attempt commits", metrics.UpstreamDeclaredModel, "upstream-real-name")
	}
}

func TestHandleResponseLeavesUpstreamModelEmptyWhenUpstreamDeclaresNothing(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())

	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	metrics := &RelayMetrics{}
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			c:               c,
			inAdapter:       declaredModelInbound{internal: &model.InternalLLMResponse{Model: "gpt-5.6-sol"}},
			metrics:         metrics,
			internalRequest: &model.InternalLLMRequest{Model: "gpt-5.6-sol"},
		},
	}
	response := &http.Response{Body: io.NopCloser(strings.NewReader("{}"))}

	if err := ra.handleResponse(context.Background(), response, declaredModelOutbound{declared: ""}); err != nil {
		t.Fatalf("handleResponse: %v", err)
	}
	ra.collectResponse()

	if metrics.UpstreamDeclaredModel != "" {
		t.Fatalf("UpstreamDeclaredModel = %q, want empty when the upstream declares nothing", metrics.UpstreamDeclaredModel)
	}
	if mismatch := upstreamModelMismatch("gpt-5.6-sol", metrics.UpstreamDeclaredModel); mismatch != nil {
		t.Fatalf("mismatch = %v, want nil (third state) when the upstream declares nothing", *mismatch)
	}
}

// F10 核心场景: 失败尝试 A 自报了型号, 但它的观测留在尝试级(失败路径不提交);
// 成功尝试 B 的声明才是日志归属。B 不自报 → 未知, 而不是沿用 A。
func TestFailedAttemptDeclarationDoesNotLeakIntoWinnerLog(t *testing.T) {
	gin.SetMode(gin.TestMode)

	newAttempt := func(declared string) *relayAttempt {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		return &relayAttempt{
			relayRequest: &relayRequest{
				c:               c,
				inAdapter:       declaredModelInbound{internal: &model.InternalLLMResponse{Model: "sent-model"}},
				metrics:         &RelayMetrics{},
				internalRequest: &model.InternalLLMRequest{Model: "sent-model"},
			},
		}
	}

	// 场景一: A 失败(只捕获不提交), B 成功并自报 → 日志归 B。
	metrics := &RelayMetrics{}
	attemptA := &relayAttempt{relayRequest: &relayRequest{metrics: metrics}}
	attemptA.captureUpstreamDeclaredModel(&model.InternalLLMResponse{Model: "model-a"})
	if metrics.UpstreamDeclaredModel != "" {
		t.Fatalf("failed attempt's declaration must stay attempt-scoped, got %q", metrics.UpstreamDeclaredModel)
	}
	attemptB := newAttempt("model-b")
	attemptB.relayRequest.metrics = metrics
	attemptB.captureUpstreamDeclaredModel(&model.InternalLLMResponse{Model: "model-b"})
	attemptB.collectResponse()
	if metrics.UpstreamDeclaredModel != "model-b" {
		t.Fatalf("UpstreamDeclaredModel = %q, want model-b (the winning attempt's declaration)", metrics.UpstreamDeclaredModel)
	}

	// 场景二: A 失败自报, B 成功但不自报 → 未知, 不沿用 A。
	metrics2 := &RelayMetrics{}
	attemptA2 := &relayAttempt{relayRequest: &relayRequest{metrics: metrics2}}
	attemptA2.captureUpstreamDeclaredModel(&model.InternalLLMResponse{Model: "model-a"})
	attemptB2 := newAttempt("")
	attemptB2.relayRequest.metrics = metrics2
	attemptB2.captureUpstreamDeclaredModel(&model.InternalLLMResponse{Model: ""})
	attemptB2.collectResponse()
	if metrics2.UpstreamDeclaredModel != "" {
		t.Fatalf("winner without declaration must read as unknown, got %q", metrics2.UpstreamDeclaredModel)
	}
	if mismatch := upstreamModelMismatch("sent-model", metrics2.UpstreamDeclaredModel); mismatch != nil {
		t.Fatalf("mismatch = %v, want nil (cannot judge) when the winner declares nothing", *mismatch)
	}
}
