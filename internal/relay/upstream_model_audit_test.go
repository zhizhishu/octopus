package relay

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/inbound"
	openaiInbound "github.com/bestruirui/octopus/internal/transformer/inbound/openai"
	"github.com/bestruirui/octopus/internal/transformer/model"
	geminiOutbound "github.com/bestruirui/octopus/internal/transformer/outbound/gemini"
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

// captureUpstreamDeclaredModel 优先读审计专用字段（TrimSpace 后非空），只有
// 它为空时才回落到 Model —— 这样写审计字段的转换器（Gemini）不必污染客户端
// 可见的 Model，而走 Model 的转换器（OpenAI/Anthropic）维持旧采集路径。
func TestCaptureUpstreamDeclaredModelPrefersAuditField(t *testing.T) {
	ra := &relayAttempt{relayRequest: &relayRequest{metrics: &RelayMetrics{}}}

	// 审计字段优先于 Model。
	ra.captureUpstreamDeclaredModel(&model.InternalLLMResponse{
		UpstreamDeclaredModel: "  audit-name  ",
		Model:                 "client-visible-name",
	})
	if ra.upstreamDeclaredModel != "audit-name" {
		t.Fatalf("审计字段应优先并去空白, 得到 %q", ra.upstreamDeclaredModel)
	}

	// 审计字段为空时回落到 Model（OpenAI/Anthropic 旧路径）。
	ra2 := &relayAttempt{relayRequest: &relayRequest{metrics: &RelayMetrics{}}}
	ra2.captureUpstreamDeclaredModel(&model.InternalLLMResponse{Model: "  openai-declared  "})
	if ra2.upstreamDeclaredModel != "openai-declared" {
		t.Fatalf("空审计字段应回落 Model, 得到 %q", ra2.upstreamDeclaredModel)
	}

	// 两者都空（或只有空白）= 未自报。
	ra3 := &relayAttempt{relayRequest: &relayRequest{metrics: &RelayMetrics{}}}
	ra3.captureUpstreamDeclaredModel(&model.InternalLLMResponse{UpstreamDeclaredModel: "   ", Model: "  "})
	if ra3.upstreamDeclaredModel != "" {
		t.Fatalf("全空白应视为未自报, 得到 %q", ra3.upstreamDeclaredModel)
	}
}

// json:"-" 必须真的不进 JSON：审计字段里装的是上游真名，绝不能通过任何
// 序列化（客户端 body / SSE chunk / 存档）泄给调用端；客户端可见的 Model
// 字段则照旧序列化。
func TestInternalLLMResponseAuditFieldNeverSerialized(t *testing.T) {
	body, err := json.Marshal(model.InternalLLMResponse{
		Object:                "chat.completion",
		Model:                 "client-visible",
		UpstreamDeclaredModel: "upstream-secret-name",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	js := string(body)
	if strings.Contains(js, "upstream-secret-name") {
		t.Fatalf("审计字段值泄露进 JSON: %s", js)
	}
	if strings.Contains(js, "UpstreamDeclaredModel") {
		t.Fatalf("审计字段名泄露进 JSON: %s", js)
	}
	if !strings.Contains(js, `"model":"client-visible"`) {
		t.Fatalf("客户端 model 字段应照旧序列化, 得到 %s", js)
	}
}

// M2 端到端（非流，未映射）：真实 Gemini 出站 + 真实 Chat 入站。客户端 body
// 必须保持旧契约（model 为空、不含上游真名），同时审计捕获仍拿到上游名。
func TestGeminiUpstreamModelAuditKeepsUnmappedChatBodyContract(t *testing.T) {
	ra, rec := newGeminiChatAttempt(t, false)
	metrics := ra.relayRequest.metrics

	if err := ra.handleResponse(context.Background(), geminiJSONResponse(t), &geminiOutbound.MessagesOutbound{}); err != nil {
		t.Fatalf("handleResponse: %v", err)
	}
	// 审计：上游真名被捕获（日志/跟进比对用）。
	if ra.upstreamDeclaredModel != "gemini-2.5-pro" {
		t.Fatalf("审计捕获 = %q, 应为 gemini-2.5-pro", ra.upstreamDeclaredModel)
	}
	// 客户端契约：未映射时 model 仍为空，且上游真名不出现在 body 里。
	wire := rec.Body.String()
	if got := bodyModelField(t, wire); got != "" {
		t.Fatalf("未映射渠道客户端 model 应保持旧行为(空), 得到 %q", got)
	}
	if strings.Contains(wire, "gemini-2.5-pro") || strings.Contains(wire, "UpstreamDeclaredModel") {
		t.Fatalf("上游真名/审计字段泄露到客户端 body: %s", wire)
	}

	ra.collectResponse()
	if metrics.UpstreamDeclaredModel != "gemini-2.5-pro" {
		t.Fatalf("请求级审计提交 = %q, 应为 gemini-2.5-pro", metrics.UpstreamDeclaredModel)
	}
}

// M2 端到端（非流，已映射）：客户端仍只看得到请求名（旧契约），审计仍拿到
// 上游真名；计费/统计的实际模型名仍取发送名，不被自报名污染。
func TestGeminiUpstreamModelAuditKeepsMappedChatBodyContract(t *testing.T) {
	ra, rec := newGeminiChatAttempt(t, true)
	metrics := ra.relayRequest.metrics

	if err := ra.handleResponse(context.Background(), geminiJSONResponse(t), &geminiOutbound.MessagesOutbound{}); err != nil {
		t.Fatalf("handleResponse: %v", err)
	}
	if ra.upstreamDeclaredModel != "gemini-2.5-pro" {
		t.Fatalf("审计捕获 = %q, 应为 gemini-2.5-pro（须在 model_mapping 改写之前）", ra.upstreamDeclaredModel)
	}
	wire := rec.Body.String()
	if got := bodyModelField(t, wire); got != "client-visible-name" {
		t.Fatalf("映射渠道客户端 model 应为请求名, 得到 %q", got)
	}
	if strings.Contains(wire, "gemini-2.5-pro") {
		t.Fatalf("上游真名泄露到客户端 body: %s", wire)
	}

	ra.collectResponse()
	if metrics.UpstreamDeclaredModel != "gemini-2.5-pro" {
		t.Fatalf("请求级审计提交 = %q, 应为 gemini-2.5-pro", metrics.UpstreamDeclaredModel)
	}
	// 计费/统计：实际模型名仍是发送名，自报名只进审计观测。
	if metrics.ActualModel != "sent-upstream-name" {
		t.Fatalf("计费/统计实际模型应为发送名, 得到 %q", metrics.ActualModel)
	}
}

// M2 端到端（流/SSE 捕获点 relay.go:3172，未映射）：Gemini 流转换器写审计字段，
// 客户端 SSE 既要有真实内容、又要按旧契约终结（恰一个 [DONE]），且不得出现上游
// 真名或审计字段名。断言真实输出 + 终结，避免"空 wire 不含真名"式空过。
func TestGeminiStreamUpstreamModelAuditKeepsChatSseContract(t *testing.T) {
	ra, rec := newGeminiChatAttempt(t, false)

	sse := "data: " + `{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"hi"}]}}],"modelVersion":"gemini-2.5-pro"}` + "\n\n"
	response := &http.Response{
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:   io.NopCloser(strings.NewReader(sse)),
	}
	if err := ra.handleStreamResponse(ra.c.Request.Context(), response, &geminiOutbound.MessagesOutbound{}); err != nil {
		t.Fatalf("handleStreamResponse: %v", err)
	}
	if ra.upstreamDeclaredModel != "gemini-2.5-pro" {
		t.Fatalf("流分片审计捕获 = %q, 应为 gemini-2.5-pro", ra.upstreamDeclaredModel)
	}

	wire := rec.Body.String()
	assertChatSseContentAndTermination(t, wire, "hi")
	if got := sseChunkModelField(t, wire); got != "" {
		t.Fatalf("未映射渠道流式 model 应保持旧行为(空), 得到 %q", got)
	}
	if strings.Contains(wire, "gemini-2.5-pro") || strings.Contains(wire, "UpstreamDeclaredModel") {
		t.Fatalf("上游真名/审计字段泄露到客户端 SSE: %s", wire)
	}
}

// M2 端到端（流/SSE，已映射）：即使开了 model_mapping，客户端 SSE 也不得出现上游
// 真名，内容与终结照旧。注：transformStreamChunk 的"上游自报名非空才改写成请求名"
// 守卫使 Gemini 流（Model 保持空）客户端 model 仍为空——即与写 Model 之前的旧契约
// 一致（非流路径无该守卫，映射后客户端 model = 请求名，见 body 用例）。
func TestGeminiStreamUpstreamModelAuditKeepsMappedChatSseContract(t *testing.T) {
	ra, rec := newGeminiChatAttempt(t, true)

	sse := "data: " + `{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"hi"}]}}],"modelVersion":"gemini-2.5-pro"}` + "\n\n"
	response := &http.Response{
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:   io.NopCloser(strings.NewReader(sse)),
	}
	if err := ra.handleStreamResponse(ra.c.Request.Context(), response, &geminiOutbound.MessagesOutbound{}); err != nil {
		t.Fatalf("handleStreamResponse: %v", err)
	}
	if ra.upstreamDeclaredModel != "gemini-2.5-pro" {
		t.Fatalf("流分片审计捕获 = %q, 应为 gemini-2.5-pro（须在 model_mapping 改写之前）", ra.upstreamDeclaredModel)
	}

	wire := rec.Body.String()
	assertChatSseContentAndTermination(t, wire, "hi")
	if got := sseChunkModelField(t, wire); got != "" {
		t.Fatalf("映射渠道流式 model 应保持旧契约(空), 得到 %q", got)
	}
	if strings.Contains(wire, "gemini-2.5-pro") || strings.Contains(wire, "UpstreamDeclaredModel") {
		t.Fatalf("上游真名/审计字段泄露到客户端 SSE: %s", wire)
	}
}

// M2 端到端（non-stream→stream 第三捕获点 relay.go:2916，未映射）：上游返回非流
// JSON、下游是流。必须有真实内容并按旧契约产出终止帧（finish_reason），审计仍
// 拿到上游真名，且不泄露。
func TestGeminiNonStreamToStreamUpstreamModelAudit(t *testing.T) {
	ra, rec := newGeminiChatAttempt(t, false)

	if err := ra.handleNonStreamResponseAsStream(context.Background(), geminiJSONResponse(t), &geminiOutbound.MessagesOutbound{}); err != nil {
		t.Fatalf("handleNonStreamResponseAsStream: %v", err)
	}
	if ra.upstreamDeclaredModel != "gemini-2.5-pro" {
		t.Fatalf("非流转流捕获点审计捕获 = %q, 应为 gemini-2.5-pro", ra.upstreamDeclaredModel)
	}

	wire := rec.Body.String()
	if !strings.Contains(wire, `"content":"hi"`) {
		t.Fatalf("non-stream→stream 未产出真实内容: %q", wire)
	}
	if !strings.Contains(wire, `"finish_reason":"stop"`) {
		t.Fatalf("non-stream→stream 未按旧契约产出终止帧: %q", wire)
	}
	if got := sseChunkModelField(t, wire); got != "" {
		t.Fatalf("未映射渠道 non-stream→stream model 应保持旧行为(空), 得到 %q", got)
	}
	if strings.Contains(wire, "gemini-2.5-pro") || strings.Contains(wire, "UpstreamDeclaredModel") {
		t.Fatalf("上游真名/审计字段泄露到客户端 SSE: %s", wire)
	}
}

// assertChatSseContentAndTermination asserts the Chat SSE wire carries the real
// content AND terminates under the old contract (exactly one `data: [DONE]`), so a
// test cannot "pass" merely because nothing was written.
func assertChatSseContentAndTermination(t *testing.T, wire, content string) {
	t.Helper()
	if !strings.Contains(wire, `"content":"`+content+`"`) {
		t.Fatalf("SSE wire missing real content %q: %s", content, wire)
	}
	if got := strings.Count(wire, "data: [DONE]"); got != 1 {
		t.Fatalf("SSE wire must terminate with exactly one [DONE], got %d: %s", got, wire)
	}
}

// sseChunkModelField returns the `model` value from the first JSON data event on a
// client Chat SSE wire.
func sseChunkModelField(t *testing.T, wire string) string {
	t.Helper()
	for _, line := range strings.Split(wire, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") || strings.Contains(line, "[DONE]") {
			continue
		}
		var got struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &got); err != nil {
			continue
		}
		return got.Model
	}
	t.Fatalf("no JSON data event on SSE wire: %s", wire)
	return ""
}

// newGeminiChatAttempt builds a relay attempt wired like a real Gemini upstream
// request routed to an OpenAI Chat client. mapped flips model_mapping on so the
// client-visible name rewrite path is exercised too. Reused by the stream,
// non-stream and non-stream→stream (fallback) capture-point tests.
func newGeminiChatAttempt(t *testing.T, mapped bool) (*relayAttempt, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			c:               c,
			inboundType:     inbound.InboundTypeOpenAIChat,
			inAdapter:       &openaiInbound.ChatInbound{},
			metrics:         &RelayMetrics{},
			requestModel:    "client-visible-name",
			internalRequest: &model.InternalLLMRequest{Model: "sent-upstream-name"},
		},
		modelMapped: mapped,
	}
	return ra, rec
}

func geminiJSONResponse(t *testing.T) *http.Response {
	t.Helper()
	return &http.Response{
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(
			`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"hi"}]}}],"modelVersion":"gemini-2.5-pro"}`)),
	}
}

func bodyModelField(t *testing.T, wire string) string {
	t.Helper()
	var got struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal([]byte(wire), &got); err != nil {
		t.Fatalf("客户端 body 不是 JSON: %v (%s)", err, wire)
	}
	return got.Model
}
