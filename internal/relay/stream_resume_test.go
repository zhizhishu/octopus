package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tmaxmax/go-sse"
	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/price"
	"github.com/zhizhishu/octopus/internal/relay/balancer"
	"github.com/zhizhishu/octopus/internal/transformer/inbound"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
	"github.com/zhizhishu/octopus/internal/utils/tokenizer"
)

const resumeTestPrefix = "第一句 还没"
const resumeTestSuffix = "说完。"

type resumeCancelWriter struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
}

func (w *resumeCancelWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(p)
	if strings.Contains(string(p), resumeTestPrefix) {
		w.cancel()
	}
	return n, err
}

func resumeChatEvent(id, delta, tail string) string {
	return fmt.Sprintf(`{"id":%q,"object":"chat.completion.chunk","created":1,"model":%q,"choices":[{"index":0,"delta":%s%s}]}`,
		id, truncationTestModel, delta, tail)
}

func resumeChatText(id, text string) string {
	return resumeChatEvent(id, fmt.Sprintf(`{"content":%q}`, text), "")
}

func resumeAnthropicStart(id string) []string {
	return []string{
		fmt.Sprintf(`{"type":"message_start","message":{"id":%q,"role":"assistant","model":%q,"content":[],"usage":{"input_tokens":27,"output_tokens":0}}}`, id, truncationTestModel),
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
	}
}

func resumeAnthropicText(text string) string {
	return fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%q}}`, text)
}

func TestStreamResumeSimulatedUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		name      string
		mode      streamPrefill
		condition string
		extra     string
		enabled   bool
		calls     int32
		finished  bool
	}{
		{name: "chat EOF with split echo", mode: chatStreamPrefix, enabled: true, calls: 2, finished: true},
		{name: "chat EOF without echo", mode: chatStreamPrefix, condition: "no_echo", enabled: true, calls: 2, finished: true},
		{name: "standalone whitespace in delivered prefix", mode: chatStreamPrefix, condition: "whitespace", enabled: true, calls: 2, finished: true},
		{name: "chat partial SSE frame", mode: chatStreamPrefix, condition: "half_frame", enabled: true, calls: 2, finished: true},
		{name: "chat temporary SSE error", mode: chatStreamPrefix, condition: "temporary", enabled: true, calls: 2, finished: true},
		{name: "chat upstream idle", mode: chatStreamPrefix, condition: "idle", enabled: true, calls: 2, finished: true},
		{name: "chat already finished", mode: chatStreamPrefix, condition: "finished", enabled: true, calls: 1, finished: true},
		{name: "chat DONE without waiting for EOF", mode: chatStreamPrefix, condition: "done_only", enabled: true, calls: 1, finished: true},
		{name: "chat finish reason then idle", mode: chatStreamPrefix, condition: "finish_idle", enabled: true, calls: 1, finished: true},
		{name: "chat usage without waiting for EOF", mode: chatStreamPrefix, condition: "usage_only", enabled: true, calls: 1, finished: true},
		{name: "chat null usage is not a terminal", mode: chatStreamPrefix, condition: "null_usage", enabled: true, calls: 2, finished: true},
		{name: "resumed chat ends at usage", mode: chatStreamPrefix, condition: "resume_usage_only", enabled: true, calls: 2, finished: true},
		{name: "anthropic EOF with split echo", mode: anthropicStreamPrefill, enabled: true, calls: 2, finished: true},
		{name: "anthropic temporary SSE error", mode: anthropicStreamPrefill, condition: "temporary", enabled: true, calls: 2, finished: true},
		{name: "anthropic message stop without waiting for EOF", mode: anthropicStreamPrefill, condition: "message_stop", enabled: true, calls: 1, finished: true},
		{name: "resumed anthropic ends at message stop", mode: anthropicStreamPrefill, condition: "resume_message_stop", enabled: true, calls: 2, finished: true},
		{name: "disabled", mode: chatStreamPrefix, calls: 1},
		{name: "unconfirmed endpoint", mode: chatStreamPrefix, condition: "unconfirmed", enabled: true, calls: 1},
		{name: "already started reasoning", mode: chatStreamPrefix, condition: "thinking", enabled: true, calls: 1},
		{name: "already started a tool", mode: chatStreamPrefix, condition: "tool_delta", enabled: true, calls: 1},
		{name: "structured output", mode: chatStreamPrefix, extra: `,"response_format":{"type":"json_object"}`, enabled: true, calls: 1},
		{name: "audio modality", mode: chatStreamPrefix, extra: `,"modalities":["text","audio"]`, enabled: true, calls: 1},
		{name: "store request", mode: chatStreamPrefix, extra: `,"store":true`, enabled: true, calls: 1},
		{name: "request tools", mode: chatStreamPrefix, extra: `,"tools":[{"type":"function","function":{"name":"action","parameters":{"type":"object"}}}]`, enabled: true, calls: 1},
		{name: "idempotency header", mode: chatStreamPrefix, condition: "idempotency", enabled: true, calls: 1},
		{name: "non-transient SSE error", mode: chatStreamPrefix, condition: "rejection", enabled: true, calls: 1},
		{name: "HTTP shape rejection during continuation", mode: chatStreamPrefix, condition: "shape_on_resume", enabled: true, calls: 2},
		{name: "SSE shape rejection during continuation", mode: chatStreamPrefix, condition: "sse_shape_on_resume", enabled: true, calls: 2},
		{name: "cancel after delivery", mode: chatStreamPrefix, condition: "cancel", enabled: true, calls: 1},
		{name: "open breaker", mode: chatStreamPrefix, condition: "breaker", enabled: true, calls: 1},
		{name: "second EOF no third call", mode: chatStreamPrefix, condition: "double_eof", enabled: true, calls: 2},
		{name: "partial echo is not forwarded", mode: chatStreamPrefix, condition: "partial_echo", enabled: true, calls: 2},
		{name: "tool during continuation is not forwarded", mode: chatStreamPrefix, condition: "tool_on_resume", enabled: true, calls: 2},
		{name: "only echoed text times out", mode: chatStreamPrefix, condition: "stalled", enabled: true, calls: 2},
		{name: "one balance charge and separate provider usage", mode: chatStreamPrefix, condition: "billing", enabled: true, calls: 2, finished: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := setupRelayErrorDB(t)
			setIdleSetting(t, dbmodel.SettingKeyRelayStreamResumeEnabled, fmt.Sprint(tc.enabled))
			setIdleSetting(t, dbmodel.SettingKeyRelayLogKeepEnabled, "true")
			if err := op.RelayLogClear(ctx, nil); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = op.RelayLogClear(context.Background(), nil) })
			switch tc.condition {
			case "idle", "done_only", "finish_idle", "usage_only", "resume_usage_only", "message_stop", "resume_message_stop":
				setIdleSetting(t, dbmodel.SettingKeyRelayStreamDataTimeoutSec, "1")
			}
			var calls atomic.Int32
			var channel dbmodel.Channel
			rec := httptest.NewRecorder()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				call := calls.Add(1)
				var request struct {
					Model    string
					Messages []struct {
						Role    string
						Content json.RawMessage
						Prefix  bool
					}
					MaxTokens int `json:"max_tokens"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				if request.Model != truncationTestModel {
					t.Errorf("request model=%q", request.Model)
				}
				if call == 2 {
					if len(request.Messages) != 2 {
						t.Errorf("want original message plus prefix, got %d messages", len(request.Messages))
						return
					}
					last := request.Messages[len(request.Messages)-1]
					var text string
					_ = json.Unmarshal(last.Content, &text)
					if last.Role != "assistant" || text != resumeTestPrefix ||
						last.Prefix != (tc.mode == chatStreamPrefix) || request.MaxTokens <= 0 || request.MaxTokens >= 64 {
						t.Errorf("wrong prefill or remaining budget: %+v", request)
					}
				}
				if tc.condition == "half_frame" && call == 1 {
					w.Header().Set("Content-Length", "4096")
				}
				if tc.condition == "shape_on_resume" && call == 2 {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					_, _ = fmt.Fprint(w, `{"error":{"type":"invalid_request_error","message":"unsupported request shape"}}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				f := w.(http.Flusher)
				send := func(events ...string) { writeSSEWire(w, f, events...) }
				if call == 1 {
					if tc.mode == anthropicStreamPrefill {
						send(resumeAnthropicStart("msg_original")...)
						send(resumeAnthropicText(resumeTestPrefix))
						if tc.condition == "temporary" {
							send(`{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`)
						}
						if tc.condition == "message_stop" {
							send(`{"type":"message_stop"}`, `{"type":"error","error":{"type":"api_error","message":"late failure"}}`)
							<-r.Context().Done()
						}
						return
					}
					send(resumeChatEvent("chat_original", `{"role":"assistant"}`, ""))
					if tc.condition == "thinking" {
						send(resumeChatEvent("chat_original", `{"reasoning_content":"reasoning"}`, ""))
					}
					if tc.condition == "tool_delta" {
						send(resumeChatEvent("chat_original", `{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"test"}}]}`, ""))
					}
					if tc.condition == "whitespace" {
						left, right, _ := strings.Cut(resumeTestPrefix, " ")
						send(resumeChatText("chat_original", left), resumeChatText("chat_original", " "), resumeChatText("chat_original", right))
					} else {
						send(resumeChatText("chat_original", resumeTestPrefix))
					}
					switch tc.condition {
					case "temporary":
						send(`{"error":{"type":"server_error","code":"internal_error","message":"temporary error"}}`)
					case "rejection":
						send(`{"error":{"type":"invalid_request_error","code":"content_filter","message":"filtered"}}`)
					case "breaker":
						balancer.RecordFailureWithStatusScoped(channel.ID, channel.Keys[0].ID, truncationTestModel, "chat", "stream", 500)
					case "half_frame":
						_, _ = fmt.Fprint(w, `data: {"unfinished":`)
						f.Flush()
					case "finished":
						send(resumeChatEvent("chat_original", "{}", `,"finish_reason":"stop"`), "[DONE]")
					case "done_only":
						send("[DONE]")
						<-r.Context().Done()
					case "finish_idle":
						send(resumeChatEvent("chat_original", "{}", `,"finish_reason":"stop"`))
						<-r.Context().Done()
					case "usage_only":
						send(`{"id":"chat_original","object":"chat.completion.chunk","created":5,"model":"trunc-model","choices":[],"usage":{"prompt_tokens":9,"completion_tokens":8,"total_tokens":17}}`)
						<-r.Context().Done()
					case "null_usage":
						send(`{"id":"chat_original","object":"chat.completion.chunk","created":5,"model":"trunc-model","choices":[],"usage":null}`)
					case "idle", "cancel":
						<-r.Context().Done()
					}
					return
				}
				if tc.mode == anthropicStreamPrefill {
					send(resumeAnthropicStart("msg_new")...)
					send(resumeAnthropicText("第一"), resumeAnthropicText(strings.TrimPrefix(resumeTestPrefix, "第一")+resumeTestSuffix))
					if tc.condition == "resume_message_stop" {
						send(`{"type":"message_stop"}`)
						<-r.Context().Done()
						return
					}
					send(`{"type":"content_block_stop","index":0}`,
						`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"input_tokens":499,"output_tokens":76}}`,
						`{"type":"message_stop"}`)
					return
				}
				send(resumeChatEvent("chat_new", `{"role":"assistant"}`, ""))
				if tc.condition == "sse_shape_on_resume" {
					send(`{"error":{"type":"invalid_request_error","message":"unsupported request shape"}}`)
					return
				}
				if tc.condition == "tool_on_resume" {
					send(resumeChatEvent("chat_new", `{"tool_calls":[{"index":0,"id":"call_not_sent","type":"function","function":{"name":"test"}}]}`, ""))
					return
				}
				if tc.condition == "partial_echo" {
					send(resumeChatText("chat_new", "第一"))
					return
				}
				if tc.condition == "stalled" {
					send(resumeChatText("chat_new", resumeTestPrefix))
					<-r.Context().Done()
					return
				}
				if tc.condition != "no_echo" {
					send(resumeChatText("chat_new", "第一"), resumeChatText("chat_new", strings.TrimPrefix(resumeTestPrefix, "第一")+resumeTestSuffix))
				} else {
					send(resumeChatText("chat_new", resumeTestSuffix))
				}
				if tc.condition == "double_eof" {
					return
				}
				const usage = `{"id":"chat_new","object":"chat.completion.chunk","created":5,"model":"trunc-model","choices":[],"usage":{"prompt_tokens":499,"completion_tokens":76,"total_tokens":575}}`
				if tc.condition == "resume_usage_only" {
					send(usage)
					<-r.Context().Done()
					return
				}
				send(resumeChatEvent("chat_new", "{}", `,"finish_reason":"stop"`), usage, "[DONE]")
			}))
			t.Cleanup(upstream.Close)
			balancer.ResetRuntimeTelemetry()
			t.Cleanup(balancer.ResetRuntimeTelemetry)
			channel = dbmodel.Channel{
				Name: "resume-fixture", Type: outbound.OutboundTypeOpenAIChat, Enabled: true,
				Model: truncationTestModel, BaseUrls: []dbmodel.BaseUrl{{URL: upstream.URL}},
				Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: t.Name()}},
			}
			path, typ := "/v1/chat/completions", inbound.InboundTypeOpenAIChat
			extra := `,"stream_options":{"include_usage":true}` + tc.extra
			if tc.mode == anthropicStreamPrefill {
				channel.Type = outbound.OutboundTypeAnthropic
				channel.Cloak.Mode = "off"
				path, typ, extra = "/v1/messages", inbound.InboundTypeAnthropic, tc.extra
			}
			if err := op.ChannelCreate(&channel, ctx); err != nil {
				t.Fatal(err)
			}
			balancer.ResetChannel(channel.ID)
			t.Cleanup(func() { balancer.ResetChannel(channel.ID) })
			if tc.condition == "breaker" {
				setIdleSetting(t, dbmodel.SettingKeyCircuitBreakerThreshold, "1")
			}
			var writer http.ResponseWriter = rec
			requestCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			if tc.condition == "cancel" {
				writer = &resumeCancelWriter{ResponseRecorder: rec, cancel: cancel}
			}
			var user dbmodel.User
			if tc.condition == "billing" {
				price.SetLLMPrice(truncationTestModel, dbmodel.LLMPrice{Input: 1, Output: 2})
				t.Cleanup(func() { price.SetLLMPrice(truncationTestModel, dbmodel.LLMPrice{}) })
				created, err := op.UserCreate(dbmodel.UserCreateRequest{
					Username: "resume-billing-user", Password: "test-only-password",
					Role: dbmodel.UserRoleUser, Status: dbmodel.UserStatusActive, Balance: 10,
				}, ctx)
				if err != nil {
					t.Fatal(err)
				}
				user = created
			}
			c, _ := gin.CreateTestContext(writer)
			body := fmt.Sprintf(`{"model":%q,"max_tokens":64,"stream":true,"messages":[{"role":"user","content":"Tell me something."}]%s}`, truncationTestModel, extra)
			c.Request = httptest.NewRequestWithContext(requestCtx, http.MethodPost, path, strings.NewReader(body))
			c.Request.Header.Set("Content-Type", "application/json")
			if tc.condition == "idempotency" {
				c.Request.Header.Set("Idempotency-Key", "test-operation")
			}
			c.Set("api_key_id", 0)
			c.Set("user_id", user.ID)
			c.Set("request_ip", "127.0.0.1")
			start := time.Now()
			handleRelay(typ, c, func(u *url.URL, name string) streamPrefill {
				if tc.condition != "unconfirmed" && u.Host == strings.TrimPrefix(upstream.URL, "http://") && name == truncationTestModel {
					return tc.mode
				}
				return noStreamPrefill
			})
			if got := calls.Load(); got != tc.calls {
				t.Fatalf("calls=%d; want %d; downstream=%s", got, tc.calls, rec.Body.String())
			}
			assertStreamResumeOutput(t, tc.mode, tc.condition, tc.finished, tc.finished && tc.calls == 2 || tc.condition == "double_eof", rec.Body.String())
			if tc.condition == "shape_on_resume" || tc.condition == "sse_shape_on_resume" {
				stats := op.StatsChannelGet(channel.ID)
				if stats.RequestFailed != 2 {
					t.Fatalf("want one interrupted hop and one failed client response, not a provider failure for the shape rejection: %+v", stats.StatsMetrics)
				}
			}
			if tc.condition == "stalled" {
				if elapsed := time.Since(start); elapsed < 15*time.Second {
					t.Fatalf("echo-only continuation ended early: %v", elapsed)
				}
				row := latestRelayLog(t, ctx)
				if len(row.Attempts) != 2 || row.Attempts[1].Status != dbmodel.AttemptFailed ||
					!strings.Contains(row.Attempts[1].Msg, errStreamResumeWait.Error()) {
					t.Fatalf("expected continuation timeout, got %+v", row.Attempts)
				}
			}
			if tc.calls == 2 && tc.finished {
				row := latestRelayLog(t, ctx)
				if len(row.Attempts) != 2 || row.Attempts[0].Status != dbmodel.AttemptFailed ||
					row.Attempts[1].Status != dbmodel.AttemptSuccess || row.Attempts[1].ChannelKeyID != row.Attempts[0].ChannelKeyID {
					t.Fatalf("unexpected attempt tracking: %+v", row.Attempts)
				}
				if row.UsageSource != dbmodel.RelayLogUsageSourceLocalEstimate || row.OutputTokens <= 0 || row.OutputTokens >= 76 ||
					row.InputTokens <= 0 || row.InputTokens >= 499 || tc.mode == anthropicStreamPrefill && row.InputTokens != 27 {
					t.Fatalf("expected logical usage, got input=%d output=%d source=%s", row.InputTokens, row.OutputTokens, row.UsageSource)
				}
				if tc.condition == "billing" {
					wantInput := tokenizer.CountTokens("Tell me something.", truncationTestModel)
					wantOutput := tokenizer.CountTokens(resumeTestPrefix+resumeTestSuffix, truncationTestModel)
					wantCost := float64(wantInput+2*wantOutput) * 1e-6
					if row.InputTokens != wantInput || row.OutputTokens != wantOutput || math.Abs(row.Cost-wantCost) > 1e-12 {
						t.Fatalf("billed usage: input=%d output=%d cost=%g; want %d/%d/%g", row.InputTokens, row.OutputTokens, row.Cost, wantInput, wantOutput, wantCost)
					}
					freshUser, err := op.UserGet(user.ID)
					if err != nil {
						t.Fatal(err)
					}
					if charged := user.Balance - freshUser.Balance; math.Abs(charged-wantCost) > 1e-9 {
						t.Fatalf("charged %g, want a single charge of %g", charged, wantCost)
					}
					stats := op.StatsChannelGet(channel.ID)
					freshChannel, err := op.ChannelGet(channel.ID, ctx)
					if err != nil {
						t.Fatal(err)
					}
					if stats.InputToken <= 499 || stats.OutputToken <= 76 ||
						math.Abs(stats.InputCost+stats.OutputCost-freshChannel.Keys[0].TotalCost) > 1e-12 ||
						freshChannel.Keys[0].TotalCost <= wantCost {
						t.Fatalf("provider usage was not accounted for separately: %+v", stats.StatsMetrics)
					}
				}
			}
		})
	}
}

func assertStreamResumeOutput(t *testing.T, mode streamPrefill, condition string, finished, wantSuffix bool, wire string) {
	t.Helper()
	var text strings.Builder
	var stops, errors, roles, blocks int
	for ev, err := range sse.Read(strings.NewReader(wire), nil) {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Data == "[DONE]" {
			stops++
			continue
		}
		var frame struct {
			ID      string
			Type    string
			Error   json.RawMessage
			Choices []struct {
				Delta struct{ Content, Role string }
			}
			Delta struct{ Text string }
		}
		if err := json.Unmarshal([]byte(ev.Data), &frame); err != nil {
			t.Fatal(err)
		}
		if mode == anthropicStreamPrefill {
			text.WriteString(frame.Delta.Text)
			switch frame.Type {
			case "message_start":
				roles++
			case "content_block_start":
				blocks++
			case "message_stop":
				stops++
			}
		}
		for _, choice := range frame.Choices {
			text.WriteString(choice.Delta.Content)
			if choice.Delta.Role != "" {
				roles++
			}
		}
		if len(frame.Error) > 0 {
			errors++
		}
		if frame.ID != "" && frame.ID != "chat_original" {
			t.Fatalf("new response id escaped: %q", frame.ID)
		}
	}
	want := resumeTestPrefix
	if wantSuffix {
		want += resumeTestSuffix
	}
	if text.String() != want || strings.Contains(wire, "chat_new") || strings.Contains(wire, "msg_new") ||
		strings.Contains(wire, "call_not_sent") || strings.Contains(wire, "499") || strings.Contains(wire, "575") {
		t.Fatalf("text=%q, want %q; wire=%s", text.String(), want, wire)
	}
	if finished {
		if stops != 1 || errors != 0 || roles != 1 || mode == anthropicStreamPrefill && blocks != 1 {
			t.Fatalf("wire framing was not preserved; wire=%s", wire)
		}
	} else if condition == "cancel" {
		if errors != 0 || stops != 0 {
			t.Fatalf("wrote after client cancellation: %s", wire)
		}
	} else if errors != 1 || mode == chatStreamPrefix && stops != 1 || mode == anthropicStreamPrefill && stops != 0 {
		t.Fatalf("want exactly one error event, got %d: %s", errors, wire)
	}
}

func TestStreamResumeAllowlist(t *testing.T) {
	for _, tc := range []struct {
		url, model string
		want       streamPrefill
	}{
		{"https://api.deepseek.com/beta/chat/completions", "deepseek-chat", chatStreamPrefix},
		{"https://api.deepseek.com/chat/completions", "deepseek-chat", noStreamPrefill},
		{"https://api.deepseek.com/beta/chat/completions", "deepseek-reasoner", noStreamPrefill},
		{"https://api.deepseek.com.example.com/beta/chat/completions", "deepseek-chat", noStreamPrefill},
		{"http://api.deepseek.com/beta/chat/completions", "deepseek-chat", noStreamPrefill},
		{"https://api.anthropic.com/v1/messages", "claude-sonnet-4-5-20250929", anthropicStreamPrefill},
		{"https://api.anthropic.com/v1/messages", "claude-opus-4-5-20251101", anthropicStreamPrefill},
		{"https://api.anthropic.com/v1/messages", "claude-haiku-4-5-20251001", anthropicStreamPrefill},
		{"https://api.anthropic.com/v1/messages", "claude-sonnet-4-6", noStreamPrefill},
		{"https://api.anthropic.com/v1/messages", "claude-opus-4-6", noStreamPrefill},
		{"https://api.anthropic.com/v1/messages", "claude-fable-5", noStreamPrefill},
		{"https://api.anthropic.com/v1/messages", "claude-opus-4-5", noStreamPrefill},
		{"https://api.openai.com/v1/responses", "gpt-5", noStreamPrefill},
	} {
		u, err := url.Parse(tc.url)
		if err != nil {
			t.Fatal(err)
		}
		if got := streamPrefillCapability(u, tc.model); got != tc.want {
			t.Errorf("%s %s: got %d, want %d", tc.url, tc.model, got, tc.want)
		}
	}
}

func TestStreamResumeRejectsAmbiguousEcho(t *testing.T) {
	first := &streamResumeCapture{}
	first.text.WriteString("already")
	resume := &streamResumeRun{first: first}
	if got, err := resume.splice("al"); got != "" || err != nil {
		t.Fatalf("partial echo escaped: %q %v", got, err)
	}
	if got, err := resume.splice("tered"); got != "" || err != errStreamResumeUnsafe {
		t.Fatalf("ambiguous echo escaped: %q %v", got, err)
	}
}
