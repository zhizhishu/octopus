package relay

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/bestruirui/octopus/internal/transformer/inbound"
	geminiIn "github.com/bestruirui/octopus/internal/transformer/inbound/gemini"
	m "github.com/bestruirui/octopus/internal/transformer/model"
	chatOut "github.com/bestruirui/octopus/internal/transformer/outbound/openai"
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
