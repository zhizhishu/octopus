package relay

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zhizhishu/octopus/internal/transformer/inbound"
	geminiIn "github.com/zhizhishu/octopus/internal/transformer/inbound/gemini"
	m "github.com/zhizhishu/octopus/internal/transformer/model"
	chatOut "github.com/zhizhishu/octopus/internal/transformer/outbound/openai"
)

// newGeminiStreamTestAttempt builds a minimal relay attempt wired like a real
// Gemini-native streamGenerateContent request routed to an OpenAI Chat
// upstream, capturing the downstream wire bytes in the recorder.
func newGeminiStreamTestAttempt(t *testing.T) (*relayAttempt, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1beta/models/test:streamGenerateContent", nil)
	adapter := &geminiIn.GenerateContentInbound{}
	ra := &relayAttempt{
		relayRequest: &relayRequest{
			c:            c,
			inboundType:  inbound.InboundTypeGemini,
			inAdapter:    adapter,
			requestModel: "test",
		},
	}
	return ra, rec
}

func geminiSSEToolCallChunks(t *testing.T, repeatedName bool) string {
	t.Helper()
	mkChunk := func(delta *m.Message, finish *string) string {
		chunk := m.InternalLLMResponse{Object: "chat.completion.chunk", Choices: []m.Choice{{Index: 0, Delta: delta, FinishReason: finish}}}
		body, err := json.Marshal(chunk)
		if err != nil {
			t.Fatalf("marshal chunk: %v", err)
		}
		return "data: " + string(body) + "\n\n"
	}
	name := "oct9_lookup"
	finish := "tool_calls"
	var b strings.Builder
	if repeatedName {
		// 上游在每个分片里重复完整函数名（既有 chat_tool_name_dedup_test.go
		// 记录的真实形态）。
		b.WriteString(mkChunk(&m.Message{ToolCalls: []m.ToolCall{{ID: "call_1", Index: 0, Function: m.FunctionCall{Name: name, Arguments: ""}}}}, nil))
		b.WriteString(mkChunk(&m.Message{ToolCalls: []m.ToolCall{{Index: 0, Function: m.FunctionCall{Name: name, Arguments: `{"q": "OCT9-`}}}}, nil))
		b.WriteString(mkChunk(&m.Message{ToolCalls: []m.ToolCall{{Index: 0, Function: m.FunctionCall{Name: name, Arguments: `R1}"}`}}}}, &finish))
	} else {
		b.WriteString(mkChunk(&m.Message{ToolCalls: []m.ToolCall{{ID: "call_1", Index: 0, Function: m.FunctionCall{Name: name, Arguments: `{"q": "OCT9-FULL"}`}}}}, nil))
	}
	return b.String()
}

// 反例（协同审查 R2）：上游已发完整工具调用，但流在没有 finish_reason、
// 也没有字面 [DONE] 的情况下正常 EOF。Gemini 入站此前被排除在成功收尾
// 合成之外，缓冲的完整调用被无声丢弃、请求却按成功返回。正确行为：正常
// EOF 视为成功结束，补发内部 [DONE] 让缓冲调用完整送达客户端。
func TestGeminiStreamToolCallFlushedOnCleanEOF(t *testing.T) {
	ra, rec := newGeminiStreamTestAttempt(t)
	sse := geminiSSEToolCallChunks(t, false)
	response := &http.Response{
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:   io.NopCloser(strings.NewReader(sse)),
	}
	if err := ra.handleStreamResponse(ra.relayRequest.c.Request.Context(), response, &chatOut.ChatOutbound{}); err != nil {
		t.Fatalf("clean EOF must be a successful stream: %v", err)
	}
	wire := rec.Body.String()
	if !strings.Contains(wire, `"functionCall"`) {
		t.Fatalf("complete tool call lost on clean EOF; wire=%s", wire)
	}
	if !strings.Contains(wire, `"name":"oct9_lookup"`) || !strings.Contains(wire, `OCT9-FULL`) {
		t.Fatalf("flushed call incomplete; wire=%s", wire)
	}
	if strings.Contains(wire, "data: [DONE]") {
		t.Fatalf("OpenAI [DONE] sentinel leaked into Gemini stream; wire=%s", wire)
	}
}

// 字面 [DONE] 到达时缓冲调用恰好冲刷一次；Gemini 下游不得收到 OpenAI 的
// [DONE] 哨兵字节。
func TestGeminiStreamToolCallFlushedOnceOnLiteralDone(t *testing.T) {
	ra, rec := newGeminiStreamTestAttempt(t)
	sse := geminiSSEToolCallChunks(t, false) + "data: [DONE]\n\n"
	response := &http.Response{
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:   io.NopCloser(strings.NewReader(sse)),
	}
	if err := ra.handleStreamResponse(ra.relayRequest.c.Request.Context(), response, &chatOut.ChatOutbound{}); err != nil {
		t.Fatalf("literal [DONE] stream failed: %v", err)
	}
	wire := rec.Body.String()
	if got := strings.Count(wire, `"functionCall"`); got != 1 {
		t.Fatalf("expected exactly one functionCall, got %d; wire=%s", got, wire)
	}
	if strings.Contains(wire, "data: [DONE]") {
		t.Fatalf("OpenAI [DONE] sentinel leaked into Gemini stream; wire=%s", wire)
	}
}

// 反例（协同审查 R1，经真实 ChatOutbound 集成）：上游重复完整函数名时，
// 客户端不得收到拼接名（oct9_lookupoct9_lookup）。
func TestGeminiStreamRepeatedNameThroughChatOutbound(t *testing.T) {
	ra, rec := newGeminiStreamTestAttempt(t)
	finish := "tool_calls"
	sse := geminiSSEToolCallChunks(t, true)
	// 收尾帧 + [DONE]，与真实上游一致。
	sse += "data: " + mustMarshal(t, m.InternalLLMResponse{Object: "chat.completion.chunk", Choices: []m.Choice{{Index: 0, Delta: &m.Message{}, FinishReason: &finish}}}) + "\n\n"
	sse += "data: [DONE]\n\n"
	response := &http.Response{
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:   io.NopCloser(strings.NewReader(sse)),
	}
	if err := ra.handleStreamResponse(ra.relayRequest.c.Request.Context(), response, &chatOut.ChatOutbound{}); err != nil {
		t.Fatalf("repeated-name stream failed: %v", err)
	}
	wire := rec.Body.String()
	if strings.Contains(wire, "oct9_lookupoct9_lookup") {
		t.Fatalf("tool name concatenated on client wire: %s", wire)
	}
	if got := strings.Count(wire, `"name":"oct9_lookup"`); got != 1 {
		t.Fatalf("expected exactly one complete call, got %d; wire=%s", got, wire)
	}
	if !strings.Contains(wire, `OCT9-R1}`) {
		t.Fatalf("complete args missing; wire=%s", wire)
	}
}

func mustMarshal(t *testing.T, v interface{}) string {
	t.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(body)
}

// fix-1127 第 4 项：gemini 流式中途断流（帧中断开）不得按干净结束收尾——
// 半截工具调用不冲刷、不合成 [DONE]、记下输出不完整。gemini 客户端认哪种
// 错误帧无法确认，按用户拍板只记日志、不写带内帧。
func TestGeminiStreamMidFrameTruncationIsNotACleanEnd(t *testing.T) {
	ra, rec := newGeminiStreamTestAttempt(t)
	mkChunk := func(delta *m.Message) string {
		chunk := m.InternalLLMResponse{Object: "chat.completion.chunk", Choices: []m.Choice{{Index: 0, Delta: delta}}}
		return "data: " + mustMarshal(t, chunk) + "\n\n"
	}
	name := "oct9_lookup"
	var b strings.Builder
	// 先有已交付的正文内容（工具调用碎片在 gemini 入站是缓冲不转发的，
	// 没有已交付内容就不算 committed，断流走的是另一路）。再发完整名 +
	// 部分参数碎片（没有 finish_reason），然后流在一个事件写到一半时断开。
	content := "A-1"
	b.WriteString(mkChunk(&m.Message{Content: m.MessageContent{Content: &content}}))
	b.WriteString(mkChunk(&m.Message{ToolCalls: []m.ToolCall{{ID: "call_1", Index: 0, Function: m.FunctionCall{Name: name, Arguments: ""}}}}))
	b.WriteString(mkChunk(&m.Message{ToolCalls: []m.ToolCall{{Index: 0, Function: m.FunctionCall{Name: name, Arguments: `{"q": "OCT9-`}}}}))
	b.WriteString(`data: {"id":"chatcmpl_trunc_mid","object":"chat.completion.chunk","cho`)
	response := &http.Response{
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:   io.NopCloser(strings.NewReader(b.String())),
	}
	err := ra.handleStreamResponse(ra.relayRequest.c.Request.Context(), response, &chatOut.ChatOutbound{})
	if err != nil {
		t.Fatalf("committed mid-frame truncation must not error (no channel swap): %v", err)
	}
	wire := rec.Body.String()
	if strings.Contains(wire, `"functionCall"`) {
		t.Fatalf("partial tool call must NOT be flushed on mid-frame truncation; wire=%s", wire)
	}
	if strings.Contains(wire, `"finishReason"`) {
		t.Fatalf("mid-frame truncation must not carry any finish terminal; wire=%s", wire)
	}
	if ra.outputIncompleteReason == "" {
		t.Fatalf("mid-frame truncation must record output incomplete")
	}
}

// 对照：事件边界上的干净 EOF 仍是合法收尾（既有钉死行为不变）——缓冲的
// 完整工具调用照常冲刷送达。
func TestGeminiStreamCleanBoundaryEOFStillFlushes(t *testing.T) {
	ra, rec := newGeminiStreamTestAttempt(t)
	sse := geminiSSEToolCallChunks(t, false)
	response := &http.Response{
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:   io.NopCloser(strings.NewReader(sse)),
	}
	if err := ra.handleStreamResponse(ra.relayRequest.c.Request.Context(), response, &chatOut.ChatOutbound{}); err != nil {
		t.Fatalf("clean boundary EOF must stay a successful end: %v", err)
	}
	wire := rec.Body.String()
	if !strings.Contains(wire, `"functionCall"`) {
		t.Fatalf("clean boundary EOF must still flush the complete tool call; wire=%s", wire)
	}
	if ra.outputIncompleteReason != "" {
		t.Fatalf("clean boundary EOF must not be marked incomplete, got %q", ra.outputIncompleteReason)
	}
}
