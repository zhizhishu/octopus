package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/transformer/inbound"
	"github.com/zhizhishu/octopus/internal/transformer/inbound/gemini"
)

// The per-protocol in-band failure frames the truncated branch reuses
// (relay.go:3350 writeTruncatedStreamTerminal -> writeResponsesFailedSSE /
// writeAnthropicErrorSSE / writeChatErrorSSE). They are asserted verbatim as the
// EXISTING wire format: the chat frame carries the octopus code and closes with the
// [DONE] transport sentinel, the responses frame is event: response.failed, and the
// anthropic frame is a plain api_error event that carries the neutral message but no
// octopus code at all.
var (
	chatTruncationFrame      = []string{"octopus_upstream_stream_truncated", `"error"`}
	responsesTruncationFrame = []string{"event: response.failed", "octopus_upstream_stream_truncated"}
	anthropicTruncationFrame = []string{"event: error", "upstream stream ended before a terminal event"}
)

// writeMidEventChatBody delivers real content and then closes the socket in the
// middle of an event ("unexpected end of input"), i.e. a second EOF shape.
//
// Coverage note: the gate run shows go-sse drops the unterminated trailing fragment
// and closes the result channel, so this shape exits through the same branch as the
// clean-EOF writer (relay.go:3064-3066, the ordinary stream-end case). The
// isBenignUpstreamStreamEnd scanner-error branch (relay.go:3088-3098) is therefore NOT
// covered by any test here — that remains unverified.
func writeMidEventChatBody(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	writeSSEWire(w, flusher, chatRoleChunk(), chatChunk("A-1"))
	// No "\n\n": the event never completes and the socket goes away mid-payload.
	_, _ = w.Write([]byte(`data: {"id":"chatcmpl_trunc","object":"chat.completion.chunk","created":1,"model":"` +
		truncationTestModel + `","choices":[{"index":0,"delta":{"content":"A-2`))
	if flusher != nil {
		flusher.Flush()
	}
}

func chatThinkingChunk(reasoning string) string {
	return `{"id":"chatcmpl_trunc","object":"chat.completion.chunk","created":1,"model":"` +
		truncationTestModel + `","choices":[{"index":0,"delta":{"reasoning_content":"` + reasoning + `"}}]}`
}

// writeThinkingThenTruncatedChatBody commits the stream with reasoning deltas only —
// the model was mid-thought when the socket died — and never sends a terminal marker.
func writeThinkingThenTruncatedChatBody(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	writeSSEWire(w, flusher, chatRoleChunk(), chatThinkingChunk("T-1"), chatThinkingChunk("T-2"))
}

// latestRelayLog polls the merged relay log view: the row is appended to the
// process-global in-memory cache by op.RelayLogAdd and only flushed to the DB by the
// background task, so the cache is the visible source here and the list also merges
// already-flushed DB rows. Polling keeps the assertion independent of flush timing.
func latestRelayLog(t *testing.T, ctx context.Context) *dbmodel.RelayLog {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		logs, err := op.RelayLogList(ctx, nil, nil, 1, 20, nil)
		if err != nil {
			t.Fatalf("list relay logs: %v", err)
		}
		for i := range logs {
			row := logs[i]
			if row.RequestModelName == truncationTestModel {
				return &row
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no relay log was written for %q", truncationTestModel)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The marker is observation only: it rides on the attempt of the very branch that
// writes the in-band failure frame, while the request keeps its success bookkeeping
// and the client keeps receiving the pre-existing per-protocol failure frame.
func TestTruncatedStreamMarksAttemptOutputIncomplete(t *testing.T) {
	cases := []struct {
		name        string
		inboundType inbound.InboundType
		upstream    http.HandlerFunc
		truncated   bool
		// failureFrame is the existing in-band error frame of the inbound protocol:
		// every substring must be present on a truncated stream and absent on a
		// legitimate end.
		failureFrame []string
		// noSynthesizedSuccess is the success terminal the protocol would synthesize
		// on a legitimate end; the truncated branch must never emit it. For chat this
		// is the finish_reason chunk, NOT the [DONE] transport sentinel (the chat error
		// frame legitimately closes with [DONE] — writeChatErrorSSE).
		noSynthesizedSuccess string
		// controlTerminal is the terminal a completed stream must still carry.
		controlTerminal string
	}{
		// Clean EOF at an event boundary.
		{"chat-clean-eof", inbound.InboundTypeOpenAIChat, writeTruncatedChatBody, true, chatTruncationFrame, `"finish_reason":"stop"`, "data: [DONE]"},
		{"responses-clean-eof", inbound.InboundTypeOpenAIResponse, writeTruncatedChatBody, true, responsesTruncationFrame, "response.completed", "response.completed"},
		{"anthropic-clean-eof", inbound.InboundTypeAnthropic, writeTruncatedChatBody, true, anthropicTruncationFrame, "message_stop", "message_stop"},
		// EOF in the middle of an event (same ordinary stream-end branch, see the
		// writer's note: the scanner-error branch is not covered).
		{"chat-mid-event", inbound.InboundTypeOpenAIChat, writeMidEventChatBody, true, chatTruncationFrame, `"finish_reason":"stop"`, "data: [DONE]"},
		{"responses-mid-event", inbound.InboundTypeOpenAIResponse, writeMidEventChatBody, true, responsesTruncationFrame, "response.completed", "response.completed"},
		{"anthropic-mid-event", inbound.InboundTypeAnthropic, writeMidEventChatBody, true, anthropicTruncationFrame, "message_stop", "message_stop"},
		// Only thinking had been delivered before the socket died: still incomplete.
		{"chat-thinking-only", inbound.InboundTypeOpenAIChat, writeThinkingThenTruncatedChatBody, true, chatTruncationFrame, `"finish_reason":"stop"`, "data: [DONE]"},
		// Controls: a real terminal marker keeps the old success path and must NOT be
		// flagged (chat finish_reason without [DONE], [DONE], message_stop,
		// response.completed).
		{"chat-finish-then-eof", inbound.InboundTypeOpenAIChat, writeFinishThenEOFChatBody, false, chatTruncationFrame, `"finish_reason":"stop"`, "data: [DONE]"},
		{"chat-complete", inbound.InboundTypeOpenAIChat, writeCompleteChatBody, false, chatTruncationFrame, `"finish_reason":"stop"`, "data: [DONE]"},
		{"responses-complete", inbound.InboundTypeOpenAIResponse, writeCompleteChatBody, false, responsesTruncationFrame, "response.completed", "response.completed"},
		{"anthropic-complete", inbound.InboundTypeAnthropic, writeCompleteChatBody, false, anthropicTruncationFrame, "message_stop", "message_stop"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			ctx := setupRelayErrorDB(t)
			if err := op.SettingSetString(dbmodel.SettingKeyRelayLogKeepEnabled, "true"); err != nil {
				t.Fatalf("enable relay log persistence: %v", err)
			}
			if err := op.RelayLogClear(ctx, nil); err != nil {
				t.Fatalf("clear relay logs: %v", err)
			}
			t.Cleanup(func() { _ = op.RelayLogClear(context.Background(), nil) })

			upstream := httptest.NewServer(tc.upstream)
			t.Cleanup(upstream.Close)
			truncationChatChannel(t, ctx, upstream.URL)

			body := driveTruncationRequest(t, tc.inboundType)
			t.Logf("%s wire=%q", tc.name, body)

			// Wire contract, unchanged by the marker: the truncated branch writes the
			// protocol's existing in-band failure frame, and never a success terminal.
			frameSeen := 0
			for _, want := range tc.failureFrame {
				if strings.Contains(body, want) {
					frameSeen++
				}
			}
			if tc.truncated {
				if frameSeen != len(tc.failureFrame) {
					t.Fatalf("a truncated stream must keep the existing %v frame (%d/%d substrings), got %q",
						tc.failureFrame, frameSeen, len(tc.failureFrame), body)
				}
				if strings.Contains(body, tc.noSynthesizedSuccess) {
					t.Fatalf("a truncated stream must not synthesize %q, got %q", tc.noSynthesizedSuccess, body)
				}
			} else {
				if frameSeen != 0 {
					t.Fatalf("a completed stream must not carry the %v failure frame, got %q", tc.failureFrame, body)
				}
				if !strings.Contains(body, tc.controlTerminal) {
					t.Fatalf("a completed stream must keep its terminal %q, got %q", tc.controlTerminal, body)
				}
			}

			row := latestRelayLog(t, ctx)
			if len(row.Attempts) != 1 {
				t.Fatalf("expected a single forward attempt, got %#v", row.Attempts)
			}
			attempt := row.Attempts[0]
			if tc.truncated {
				if attempt.OutputIncompleteReason != dbmodel.RelayLogOutputIncompleteUpstreamMissingTerminal {
					t.Fatalf("truncated stream must mark the attempt, got reason %q (wire=%q)", attempt.OutputIncompleteReason, body)
				}
				if !row.OutputIncomplete {
					t.Fatal("the log row must derive output_incomplete from the marked attempt")
				}
			} else {
				if attempt.OutputIncompleteReason != "" {
					t.Fatalf("a completed stream must not be marked, got reason %q (wire=%q)", attempt.OutputIncompleteReason, body)
				}
				if row.OutputIncomplete {
					t.Fatal("a completed stream must not be flagged")
				}
			}
			// Business bookkeeping is untouched either way: the attempt stays success,
			// carries no message and the row stays in the success severity bucket.
			if attempt.Status != dbmodel.AttemptSuccess || attempt.Msg != "" {
				t.Fatalf("the observation must not change the attempt result, got %#v", attempt)
			}
			if row.Error != "" || row.ErrorCode != "" {
				t.Fatalf("the observation must not invent an error, got error=%q code=%q", row.Error, row.ErrorCode)
			}
			if got := op.RelayLogSeverityValue(*row); got != "success" {
				t.Fatalf("severity bucket changed to %q", got)
			}
			if row.TotalAttempts != 1 {
				t.Fatalf("total attempts = %d, want 1", row.TotalAttempts)
			}
		})
	}
}

// Gemini 的入站没有带内失败帧, 它的 clean-EOF [DONE] 合成是合法收尾: 行为必须完全不变,
// 也就绝不能出现「输出不完整」标记。
func TestGeminiTruncatedStreamIsNotMarked(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	if err := op.SettingSetString(dbmodel.SettingKeyRelayLogKeepEnabled, "true"); err != nil {
		t.Fatalf("enable relay log persistence: %v", err)
	}
	if err := op.RelayLogClear(ctx, nil); err != nil {
		t.Fatalf("clear relay logs: %v", err)
	}
	t.Cleanup(func() { _ = op.RelayLogClear(context.Background(), nil) })

	upstream := httptest.NewServer(http.HandlerFunc(writeTruncatedChatBody))
	t.Cleanup(upstream.Close)
	truncationChatChannel(t, ctx, upstream.URL)

	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1beta/models/"+truncationTestModel+":streamGenerateContent?alt=sse",
		strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`),
	)
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(gemini.WithRequestOptions(request.Context(), truncationTestModel, true))
	ginContext.Request = request
	ginContext.Set("api_key_id", 0)
	ginContext.Set("user_id", 0)
	ginContext.Set("request_ip", "127.0.0.1")
	Handler(inbound.InboundTypeGemini, ginContext)

	body := recorder.Body.String()
	t.Logf("gemini truncation wire=%q", body)
	// Gemini is excluded from the detector, so it has no in-band failure frame and
	// must keep its old clean-EOF [DONE] synthesis.
	if strings.Contains(body, "octopus_upstream_stream_truncated") || strings.Contains(body, "event: error") {
		t.Fatalf("gemini has no in-band failure frame; behaviour must not change, got %q", body)
	}

	row := latestRelayLog(t, ctx)
	if len(row.Attempts) != 1 {
		t.Fatalf("expected a single forward attempt, got %#v", row.Attempts)
	}
	if row.Attempts[0].OutputIncompleteReason != "" {
		t.Fatalf("gemini must never be marked, got %q", row.Attempts[0].OutputIncompleteReason)
	}
	if row.OutputIncomplete {
		t.Fatal("gemini row must not be flagged")
	}
}
