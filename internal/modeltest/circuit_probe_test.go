package modeltest

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// ── 断路器探测的假本地用例 ──────────────────────────────────────────────────
//
// 全部用 httptest 假上游：不发任何真实请求、不碰任何真实渠道、不写用户统计。
// 只钉行为契约：判读（对/错/答非所问/歧义/超时/取消）、归因（哪些算渠道故障）、
// 以及"只认那一个渠道/密钥/上游模型"这条铁律。

// circuitProbeChatChannel 造一个本地 chat 渠道：指定的密钥 + 假上游地址。
func circuitProbeChatChannel(url string, keys []dbmodel.ChannelKey) dbmodel.Channel {
	return dbmodel.Channel{
		ID:       9001,
		Name:     "breaker-probe-local",
		Type:     outbound.OutboundTypeOpenAIChat,
		Enabled:  true,
		BaseUrls: []dbmodel.BaseUrl{{URL: url}},
		Keys:     keys,
	}
}

// circuitProbeChatSSE 造一段 chat 流式回复（探针默认流式，与渠道检测一致）。
func circuitProbeChatSSE(content string) string {
	payload, err := json.Marshal(map[string]any{
		"choices": []map[string]any{{
			"index": 0,
			"delta": map[string]any{"content": content},
		}},
	})
	if err != nil {
		panic(err)
	}
	return "data: " + string(payload) + "\n\ndata: [DONE]\n\n"
}

// circuitProbePromptFromChatBody 取出发给上游的那道题，并顺便把整个 body 交回来，
// 供 shape 断言用。
func circuitProbePromptFromChatBody(t *testing.T, r *http.Request) (string, map[string]any) {
	t.Helper()
	var payload map[string]any
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		t.Errorf("decode upstream body: %v", err)
		return "", nil
	}
	messages, _ := payload["messages"].([]any)
	if len(messages) != 1 {
		return "", payload
	}
	message, _ := messages[0].(map[string]any)
	content, _ := message["content"].(string)
	return content, payload
}

// circuitProbeExpectedFromPrompt 从探针真正发出去的题面反推出正确答案。
//
// 题库有加法/减法/乘法三种题型、多种措辞与符号，假上游必须自己算出答案才能回一条
// “正确回答”。这里按题面里出现的运算符/字词判题型：先认乘法（× * 乘 times x），
// 再认减法（- − 减 minus），其余按加法。
func circuitProbeExpectedFromPrompt(t *testing.T, prompt string) int {
	t.Helper()
	values := circuitProbeIntegers(prompt)
	if len(values) != 2 {
		// 不能在 handler 协程里 Fatalf（那只会结束该协程，并把请求挂到超时），
		// 用 Errorf 报因由 + 返回 0；调用方的断言会把这次探测判成失败。
		t.Errorf("probe prompt %q must carry exactly two operands, got %v", prompt, values)
		return 0
	}
	lower := strings.ToLower(prompt)
	switch {
	case strings.ContainsAny(prompt, "×*") || strings.Contains(prompt, "乘") ||
		strings.Contains(lower, "times") || strings.Contains(lower, " x "):
		return values[0] * values[1]
	case strings.ContainsAny(prompt, "-−") || strings.Contains(prompt, "减") || strings.Contains(lower, "minus"):
		return values[0] - values[1]
	default:
		return values[0] + values[1]
	}
}

func TestCircuitProbeGeminiKeepsTheRequestPath(t *testing.T) {
	for _, tc := range []struct{ endpoint, method string }{
		{"gemini_generate_content", "generateContent"},
		{"gemini_stream_generate_content", "streamGenerateContent"},
	} {
		t.Run(tc.endpoint, func(t *testing.T) {
			ctx := setupModelTestDB(t)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if want := "/v1beta/models/probe-model:" + tc.method; r.URL.Path != want {
					t.Errorf("path = %q, want %q", r.URL.Path, want)
				}
				w.WriteHeader(http.StatusTooManyRequests)
			}))
			defer upstream.Close()
			channel := circuitProbeChatChannel(upstream.URL, []dbmodel.ChannelKey{{ID: 501, Enabled: true, ChannelKey: "key-a"}})
			channel.Type = outbound.OutboundTypeGemini
			if got := RunCircuitProbe(ctx, channel, 501, "probe-model", tc.endpoint, ""); got.StatusCode != http.StatusTooManyRequests {
				t.Fatalf("probe did not reach Gemini: %#v", got)
			}
		})
	}
}

// 正确回答 = 回答里出现正确答案（题库三种题型之一）：Recovered，且绝不算渠道故障。
// 顺带钉住出站 shape：一个请求、chat 形状、模型原样（不做第二次映射）。
func TestCircuitProbeRecoversOnExactAnswer(t *testing.T) {
	ctx := setupModelTestDB(t)

	var calls atomic.Int32
	var sawModel, sawAuth string
	var sawShape map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("probe hit %q, want /v1/chat/completions", r.URL.Path)
		}
		sawAuth = r.Header.Get("Authorization")
		prompt, payload := circuitProbePromptFromChatBody(t, r)
		sawShape = payload
		sawModel, _ = payload["model"].(string)
		values := circuitProbeIntegers(prompt)
		if len(values) != 2 {
			t.Errorf("probe prompt %q must carry exactly two operands", prompt)
			http.Error(w, "bad probe prompt", 400)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(circuitProbeChatSSE(fmt.Sprintf("%d", circuitProbeExpectedFromPrompt(t, prompt)))))
	}))
	t.Cleanup(upstream.Close)

	channel := circuitProbeChatChannel(upstream.URL, []dbmodel.ChannelKey{{ID: 501, Enabled: true, ChannelKey: "key-a"}})
	result := RunCircuitProbe(ctx, channel, 501, "probe-model", "openai_chat", "")

	if !result.Recovered || result.Outcome != CircuitProbeOutcomeOK {
		t.Fatalf("exact answer must recover: %#v", result)
	}
	if result.Attributable {
		t.Fatalf("a corrected answer is not a channel fault: %#v", result)
	}
	if result.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want 200", result.StatusCode)
	}
	if result.Duration <= 0 {
		t.Fatalf("Duration must be measured, got %v", result.Duration)
	}
	if result.RetryAfter != 0 {
		t.Fatalf("RetryAfter = %v, want 0 (upstream supplied none)", result.RetryAfter)
	}
	if len(circuitProbeIntegers(result.Prompt)) != 2 {
		t.Fatalf("Prompt = %q, want a probe question carrying two operands", result.Prompt)
	}
	// 探测题不得用固定前缀/探活惯用语（题目现在来自随机题库）。
	lowerPrompt := strings.ToLower(strings.TrimSpace(result.Prompt))
	if strings.Contains(lowerPrompt, "ping") || lowerPrompt == "ok" || lowerPrompt == "hi" {
		t.Fatalf("probe prompt must not use liveness wording: %q", result.Prompt)
	}
	if strings.TrimSpace(result.Answer) == "" {
		t.Fatalf("Answer must carry the bounded safe answer text, got empty")
	}
	if result.ChannelID != 9001 || result.ChannelKeyID != 501 || result.UpstreamModel != "probe-model" || result.Endpoint != "openai_chat" {
		t.Fatalf("result must report the exact probed object: %#v", result)
	}
	if calls.Load() != 1 {
		t.Fatalf("a probe is exactly one request, got %d", calls.Load())
	}
	// 出站 shape 未被探针改动：标准 chat 形状、默认 256 输出预算、流式。
	if sawModel != "probe-model" {
		t.Fatalf("upstream model = %q, want the exact one handed in", sawModel)
	}
	if sawShape["stream"] != true {
		t.Fatalf("probe must stream like channel-test/production traffic: %v", sawShape["stream"])
	}
	if sawShape["max_tokens"] != float64(defaultProbeMaxTokens) {
		t.Fatalf("max_tokens = %v, want %d", sawShape["max_tokens"], defaultProbeMaxTokens)
	}
	for _, forbidden := range []string{"input", "instructions"} {
		if _, ok := sawShape[forbidden]; ok {
			t.Fatalf("chat probe must not carry the responses field %q: %v", forbidden, sawShape)
		}
	}
	if sawAuth != "Bearer key-a" {
		t.Fatalf("Authorization = %q, want the exact key", sawAuth)
	}
}

// 答错：整段回答只有一个整数，但不是正确结果 ⇒ 渠道侧故障。
// 固定答 987654：题库三种题型的正确答案最大 19998，因此这个值永远不会“恰好对”。
func TestCircuitProbeWrongAnswerIsAttributable(t *testing.T) {
	ctx := setupModelTestDB(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(circuitProbeChatSSE("987654")))
	}))
	t.Cleanup(upstream.Close)

	channel := circuitProbeChatChannel(upstream.URL, []dbmodel.ChannelKey{{ID: 502, Enabled: true, ChannelKey: "key-a"}})
	result := RunCircuitProbe(ctx, channel, 502, "probe-model", "openai_chat", "")

	if result.Recovered {
		t.Fatalf("a wrong number must not recover: %#v", result)
	}
	if result.Outcome != CircuitProbeOutcomeWrongAnswer {
		t.Fatalf("Outcome = %q, want %q", result.Outcome, CircuitProbeOutcomeWrongAnswer)
	}
	if !result.Attributable {
		t.Fatalf("a wrong answer is a channel fault: %#v", result)
	}
	if result.Answer != "987654" {
		t.Fatalf("Answer = %q, want the bounded safe answer text", result.Answer)
	}
}

// 答非所问（没有整数）⇒ 渠道侧故障。
func TestCircuitProbeGibberishIsAttributable(t *testing.T) {
	ctx := setupModelTestDB(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(circuitProbeChatSSE("I am sorry, but I cannot answer that.")))
	}))
	t.Cleanup(upstream.Close)

	channel := circuitProbeChatChannel(upstream.URL, []dbmodel.ChannelKey{{ID: 503, Enabled: true, ChannelKey: "key-a"}})
	result := RunCircuitProbe(ctx, channel, 503, "probe-model", "openai_chat", "")

	if result.Recovered || result.Outcome != CircuitProbeOutcomeGibberish {
		t.Fatalf("gibberish must be judged gibberish: %#v", result)
	}
	if !result.Attributable {
		t.Fatalf("gibberish is a channel fault: %#v", result)
	}
}

// 正确答案后面还跟了别的整数（它不是末位）⇒ 本轮口径下仍算对，且不是渠道故障。
// 旧口径会把它判成“歧义”，那是把模型自带演算的正确回答误伤成故障。
func TestCircuitProbeSumBeforeTrailingIntegerIsRecovered(t *testing.T) {
	ctx := setupModelTestDB(t)

	var sum atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prompt, _ := circuitProbePromptFromChatBody(t, r)
		expected := circuitProbeExpectedFromPrompt(t, prompt)
		sum.Store(int64(expected))
		// 正确答案在前，后面还跟了一个无关整数。
		answer := fmt.Sprintf("%d，另外参考值是 7", expected)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(circuitProbeChatSSE(answer)))
	}))
	t.Cleanup(upstream.Close)

	channel := circuitProbeChatChannel(upstream.URL, []dbmodel.ChannelKey{{ID: 504, Enabled: true, ChannelKey: "key-a"}})
	result := RunCircuitProbe(ctx, channel, 504, "probe-model", "openai_chat", "")

	if sum.Load() == 0 {
		t.Fatal("fake upstream never saw the two addends")
	}
	if !result.Recovered || result.Outcome != CircuitProbeOutcomeOK {
		t.Fatalf("the sum appearing anywhere must recover: %#v", result)
	}
	if result.Attributable {
		t.Fatalf("a correct answer is never a channel fault: %#v", result)
	}
}

// 探针**自己的**时钟到点：渠道没在窗口内交卷 ⇒ 渠道侧故障。
// 这条与"调用方取消"必须分开，否则一个被调度方撤掉的探测会把渠道账算黑。
func TestCircuitProbeOwnTimeoutIsAttributable(t *testing.T) {
	ctx := setupModelTestDB(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(circuitProbeChatSSE("0")))
	}))
	t.Cleanup(upstream.Close)

	channel := circuitProbeChatChannel(upstream.URL, []dbmodel.ChannelKey{{ID: 505, Enabled: true, ChannelKey: "key-a"}})
	result := runCircuitProbe(ctx, channel, 505, "probe-model", "openai_chat", "", 60*time.Millisecond)

	if result.Outcome != CircuitProbeOutcomeTimeout {
		t.Fatalf("Outcome = %q, want %q (%#v)", result.Outcome, CircuitProbeOutcomeTimeout, result)
	}
	if !result.Attributable {
		t.Fatalf("the probe's own budget expiring is a channel fault: %#v", result)
	}
	if result.Recovered {
		t.Fatalf("a timed-out probe cannot recover: %#v", result)
	}
}

// 调用方取消：探测被撤掉，不是渠道故障。
func TestCircuitProbeCallerCancelIsNotAttributable(t *testing.T) {
	ctx := setupModelTestDB(t)

	started := make(chan struct{})
	release := make(chan struct{})
	var once atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if once.CompareAndSwap(false, true) {
			close(started)
		}
		<-release
	}))
	t.Cleanup(func() {
		close(release)
		upstream.Close()
	})

	channel := circuitProbeChatChannel(upstream.URL, []dbmodel.ChannelKey{{ID: 506, Enabled: true, ChannelKey: "key-a"}})
	probeCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	done := make(chan CircuitProbeResult, 1)
	go func() {
		done <- RunCircuitProbe(probeCtx, channel, 506, "probe-model", "openai_chat", "")
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the probe never reached the fake upstream")
	}
	cancel()

	var result CircuitProbeResult
	select {
	case result = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a caller cancel must abort the probe promptly")
	}
	if result.Outcome != CircuitProbeOutcomeCanceled {
		t.Fatalf("Outcome = %q, want %q (%#v)", result.Outcome, CircuitProbeOutcomeCanceled, result)
	}
	if result.Attributable {
		t.Fatalf("a canceled probe is not a channel fault: %#v", result)
	}
	if result.Recovered {
		t.Fatalf("a canceled probe cannot recover: %#v", result)
	}
}

// 非 2xx 的归因必须与 relay 的既有判定同源：typed 400/422 请求体拒收不算渠道故障，
// 无标记的 4xx 和 5xx 才算；上游 Retry-After 原样带出。
func TestCircuitProbeUpstreamErrorClassificationMatchesRelay(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		body        string
		retryAfter  string
		wantCount   bool
		wantRetry   time.Duration
		wantOutcome string
	}{
		{
			name:        "typed request-shape rejection is not a channel fault",
			status:      http.StatusBadRequest,
			body:        `{"error":{"type":"invalid_request_error","message":"parallel_tool_calls must be false"}}`,
			wantCount:   false,
			wantOutcome: CircuitProbeOutcomeUpstreamError,
		},
		{
			name:        "serde deserialize 422 is not a channel fault",
			status:      http.StatusUnprocessableEntity,
			body:        `{"error":{"message":"Failed to deserialize the JSON body into the target type"}}`,
			wantCount:   false,
			wantOutcome: CircuitProbeOutcomeUpstreamError,
		},
		{
			name:        "bare 4xx without markers stays counted",
			status:      http.StatusForbidden,
			body:        `{"error":{"message":"region blocked"}}`,
			wantCount:   true,
			wantOutcome: CircuitProbeOutcomeUpstreamError,
		},
		{
			name:        "429 carries the upstream Retry-After",
			status:      http.StatusTooManyRequests,
			body:        `{"error":{"code":"rate_limited","message":"slow down"}}`,
			retryAfter:  "7",
			wantCount:   true,
			wantRetry:   7 * time.Second,
			wantOutcome: CircuitProbeOutcomeUpstreamError,
		},
		{
			name:        "5xx stays counted",
			status:      http.StatusServiceUnavailable,
			body:        `{"error":{"message":"overloaded"}}`,
			wantCount:   true,
			wantOutcome: CircuitProbeOutcomeUpstreamError,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := setupModelTestDB(t)
			status, body, retryAfter := tc.status, tc.body, tc.retryAfter
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if retryAfter != "" {
					w.Header().Set("Retry-After", retryAfter)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(body))
			}))
			t.Cleanup(upstream.Close)

			channel := circuitProbeChatChannel(upstream.URL, []dbmodel.ChannelKey{{ID: 507, Enabled: true, ChannelKey: "key-a"}})
			result := RunCircuitProbe(ctx, channel, 507, "probe-model", "openai_chat", "")

			if result.Outcome != tc.wantOutcome {
				t.Fatalf("Outcome = %q, want %q", result.Outcome, tc.wantOutcome)
			}
			if result.Attributable != tc.wantCount {
				t.Fatalf("Attributable = %t, want %t (status %d, body %s)", result.Attributable, tc.wantCount, status, body)
			}
			if result.RetryAfter != tc.wantRetry {
				t.Fatalf("RetryAfter = %v, want %v", result.RetryAfter, tc.wantRetry)
			}
			if result.StatusCode != status {
				t.Fatalf("StatusCode = %d, want %d", result.StatusCode, status)
			}
			if result.Recovered {
				t.Fatalf("an upstream error cannot recover: %#v", result)
			}
			// 安全边界：答案里绝不能出现上游错误体原文。
			if strings.Contains(result.Answer, "region blocked") || strings.Contains(result.Reason, "region blocked") {
				t.Fatalf("raw upstream error body leaked into the result: %#v", result)
			}
		})
	}
}

// 只认指定的那一个密钥与那一个上游模型：密钥不轮转、模型不再做映射、只发一次。
func TestCircuitProbeUsesExactKeyAndModelWithoutRemap(t *testing.T) {
	ctx := setupModelTestDB(t)

	var calls atomic.Int32
	var sawAuth string
	var sawModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		sawAuth = r.Header.Get("Authorization")
		prompt, payload := circuitProbePromptFromChatBody(t, r)
		sawModel, _ = payload["model"].(string)
		answer := fmt.Sprintf("%d", circuitProbeExpectedFromPrompt(t, prompt))
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(circuitProbeChatSSE(answer)))
	}))
	t.Cleanup(upstream.Close)

	channel := circuitProbeChatChannel(upstream.URL, []dbmodel.ChannelKey{
		{ID: 601, Enabled: true, ChannelKey: "key-first"},
		{ID: 602, Enabled: true, ChannelKey: "key-second"},
	})
	// 渠道配了映射：探测**不得**参与第二次映射，上游必须收到调用方给的那个模型名。
	channel.ModelMapping = map[string]string{"probe-model": "mapped-model"}

	result := RunCircuitProbe(ctx, channel, 602, "probe-model", "openai_chat", "")

	if result.UpstreamModel != "probe-model" {
		t.Fatalf("UpstreamModel = %q, want the exact one handed in", result.UpstreamModel)
	}
	if sawModel != "probe-model" {
		t.Fatalf("upstream received model %q, want probe-model (no second remap)", sawModel)
	}
	if sawAuth != "Bearer key-second" {
		t.Fatalf("Authorization = %q, want the exact requested key", sawAuth)
	}
	if calls.Load() != 1 {
		t.Fatalf("probe must not fall back to another key: %d requests", calls.Load())
	}
}

// 前置条件不满足 ⇒ 压根不发（skipped，且绝不记成渠道故障）。
func TestCircuitProbeSkipsWithoutSending(t *testing.T) {
	ctx := setupModelTestDB(t)

	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(circuitProbeChatSSE("0")))
	}))
	t.Cleanup(upstream.Close)

	enabled := circuitProbeChatChannel(upstream.URL, []dbmodel.ChannelKey{{ID: 701, Enabled: true, ChannelKey: "key-a"}})
	disabled := enabled
	disabled.Enabled = false
	disabledKey := circuitProbeChatChannel(upstream.URL, []dbmodel.ChannelKey{{ID: 702, Enabled: false, ChannelKey: "key-b"}})

	cases := []struct {
		name    string
		channel dbmodel.Channel
		keyID   int
		model   string
	}{
		{"disabled channel", disabled, 701, "probe-model"},
		{"unknown key", enabled, 999, "probe-model"},
		{"disabled key", disabledKey, 702, "probe-model"},
		{"empty model", enabled, 701, "   "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := RunCircuitProbe(ctx, tc.channel, tc.keyID, tc.model, "openai_chat", "")
			if result.Outcome != CircuitProbeOutcomeSkipped {
				t.Fatalf("Outcome = %q, want %q", result.Outcome, CircuitProbeOutcomeSkipped)
			}
			if result.Attributable || result.Recovered {
				t.Fatalf("a skipped probe is not a verdict on the channel: %#v", result)
			}
			if result.Reason == "" {
				t.Fatalf("skipped probes carry a bounded reason: %#v", result)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("skipped probes must not reach the upstream, got %d requests", calls.Load())
	}
}

// 嵌入探测只在调度方显式签核（capability=embeddings）且渠道声明了能力位时才跑；
// 签核到之后，只有"非空且有效的向量"才算恢复。
func TestCircuitProbeEmbeddingSignOffAndVector(t *testing.T) {
	vector := `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2,0.3]}],"model":"embed-model","usage":{"prompt_tokens":2,"total_tokens":2}}`
	emptyVector := `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[]}],"model":"embed-model","usage":{"prompt_tokens":2,"total_tokens":2}}`

	for _, tc := range []struct {
		name        string
		capability  string
		caps        []string
		reply       string
		wantOutcome string
		wantRecover bool
		wantCalls   int32
	}{
		{name: "no sign-off", capability: "", caps: []string{dbmodel.CapabilityEmbeddings}, reply: vector, wantOutcome: CircuitProbeOutcomeSkipped, wantCalls: 0},
		{name: "no capability bit", capability: dbmodel.CapabilityEmbeddings, caps: nil, reply: vector, wantOutcome: CircuitProbeOutcomeSkipped, wantCalls: 0},
		{name: "signed off and valid", capability: dbmodel.CapabilityEmbeddings, caps: []string{dbmodel.CapabilityEmbeddings}, reply: vector, wantOutcome: CircuitProbeOutcomeEmbedding, wantRecover: true, wantCalls: 1},
		{name: "signed off but empty vector", capability: dbmodel.CapabilityEmbeddings, caps: []string{dbmodel.CapabilityEmbeddings}, reply: emptyVector, wantOutcome: CircuitProbeOutcomeNoAnswer, wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := setupModelTestDB(t)
			var calls atomic.Int32
			reply := tc.reply
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != "/v1/embeddings" {
					t.Errorf("embedding probe hit %q, want /v1/embeddings", r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(reply))
			}))
			t.Cleanup(upstream.Close)

			channel := circuitProbeChatChannel(upstream.URL, []dbmodel.ChannelKey{{ID: 801, Enabled: true, ChannelKey: "key-a"}})
			channel.Capabilities = tc.caps
			result := RunCircuitProbe(ctx, channel, 801, "embed-model", embeddingTestEndpointName, tc.capability)

			if result.Outcome != tc.wantOutcome {
				t.Fatalf("Outcome = %q, want %q (%#v)", result.Outcome, tc.wantOutcome, result)
			}
			if result.Recovered != tc.wantRecover {
				t.Fatalf("Recovered = %t, want %t (%#v)", result.Recovered, tc.wantRecover, result)
			}
			if calls.Load() != tc.wantCalls {
				t.Fatalf("upstream requests = %d, want %d", calls.Load(), tc.wantCalls)
			}
		})
	}
}

// DisableCircuitBreaker 的渠道不累积健康度状态：探测结论一律不记在它账上。
func TestCircuitProbeNeverAttributesOnBreakerDisabledChannel(t *testing.T) {
	ctx := setupModelTestDB(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// 同上：987654 永远不是题库里的正确答案。
		_, _ = w.Write([]byte(circuitProbeChatSSE("987654")))
	}))
	t.Cleanup(upstream.Close)

	channel := circuitProbeChatChannel(upstream.URL, []dbmodel.ChannelKey{{ID: 901, Enabled: true, ChannelKey: "key-a"}})
	channel.DisableCircuitBreaker = true
	result := RunCircuitProbe(ctx, channel, 901, "probe-model", "openai_chat", "")

	if result.Outcome != CircuitProbeOutcomeWrongAnswer {
		t.Fatalf("Outcome = %q, want wrong_answer", result.Outcome)
	}
	if result.Attributable {
		t.Fatalf("breaker-disabled channels never accumulate health state: %#v", result)
	}
}

// 答案展示必须有界：超长回答被截断，且按 rune 截断（不切半个汉字）。
func TestCircuitProbeAnswerIsBounded(t *testing.T) {
	long := strings.Repeat("探测结论很长", 200)
	safe := circuitProbeHistoryAnswer(long, "")
	if got := len([]rune(safe)); got > circuitProbeAnswerLimit+1 {
		t.Fatalf("bounded answer runes = %d, want <= %d (+ellipsis)", got, circuitProbeAnswerLimit)
	}
	if !strings.HasSuffix(safe, "…") {
		t.Fatalf("truncated answer must be marked with an ellipsis")
	}
	// 除脱敏/截断外正文保持原样：换行不能被折叠成一行。
	if withNewlines := circuitProbeHistoryAnswer("第一行\n第二行", ""); !strings.Contains(withNewlines, "\n") {
		t.Fatalf("history answer must keep the raw line breaks, got %q", withNewlines)
	}
}

// 整数抽取的边界：型号里的数字不算答案；负号保留；千位分隔符只在数学上合法时才合并。
func TestCircuitProbeIntegerExtractionSignedAndGrouping(t *testing.T) {
	cases := []struct {
		name string
		text string
		want []int
	}{
		{"identifier digits are dropped", "gpt-4o says 1234", []int{1234}},
		{"thousands separator joins", "the answer is 1,234", []int{1234}},
		{"two-digit first group joins", "12,345", []int{12345}},
		{"malformed comma stays two numbers", "1234,5678", []int{1234, 5678}},
		{"leading zero is not a valid group", "0,123", []int{0, 123}},
		{"four digits after the comma is not a group", "1,2345", []int{1, 2345}},
		{"negative keeps its sign", "-1234", []int{-1234}},
		{"negative with grouping keeps its sign", "-1,234", []int{-1234}},
		{"sign after a letter is not a sign", "x-1234", []int{1234}},
		{"two candidates", "1234 and 5678", []int{1234, 5678}},
		{"underscore identifier is dropped", "id_1234", nil},
		{"no digits", "no digits here", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := circuitProbeIntegers(tc.text)
			if len(got) != len(tc.want) {
				t.Fatalf("integers(%q) = %v, want %v", tc.text, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("integers(%q) = %v, want %v", tc.text, got, tc.want)
				}
			}
		})
	}
}

// 判读口径（本轮需求）：正确答案出现在回答任意位置就算对；有整数但没出现正确答案才是
// WrongAnswer；一个整数都没有仍 Gibberish；空回答仍 NoAnswer。
func TestCircuitProbeJudgeScoring(t *testing.T) {
	cases := []struct {
		name        string
		answer      string
		expected    int
		wantOutcome string
		wantRecover bool
		wantAttrib  bool
	}{
		{"chinese sentence", "答案是 6912。", 6912, CircuitProbeOutcomeOK, true, false},
		{"markdown bold", "**6912**", 6912, CircuitProbeOutcomeOK, true, false},
		{"equation form", "1234 + 5678 = 6912", 6912, CircuitProbeOutcomeOK, true, false},
		{"result then trailing integer", "6912，参考值 7", 6912, CircuitProbeOutcomeOK, true, false},
		{"result in the middle", "先算得 6912，再确认一次是 6913", 6912, CircuitProbeOutcomeOK, true, false},
		{"restated multiplication with ×", "12 × 12 = 144", 144, CircuitProbeOutcomeOK, true, false},
		{"restated multiplication with *", "12*12=144", 144, CircuitProbeOutcomeOK, true, false},
		{"restated multiplication with x", "12 x 12 = 144", 144, CircuitProbeOutcomeOK, true, false},
		{"restated multiplication without the product", "12 x 12", 144, CircuitProbeOutcomeWrongAnswer, false, true},
		{"positive subtraction result", "8000 - 3000 = 5000", 5000, CircuitProbeOutcomeOK, true, false},
		{"negative restatement is not the answer", "3000 - 8000 = -5000", 5000, CircuitProbeOutcomeWrongAnswer, false, true},
		{"wrong number only", "7000", 6912, CircuitProbeOutcomeWrongAnswer, false, true},
		{"equation without result", "1234 + 5678 = ?", 6912, CircuitProbeOutcomeWrongAnswer, false, true},
		{"signed negative is not the answer", "-6912", 6912, CircuitProbeOutcomeWrongAnswer, false, true},
		{"no integer", "I cannot answer that.", 6912, CircuitProbeOutcomeGibberish, false, true},
		{"empty", "   \n ", 6912, CircuitProbeOutcomeNoAnswer, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verdict := circuitProbeJudgeAnswer(tc.answer, tc.expected)
			if verdict.outcome != tc.wantOutcome {
				t.Fatalf("outcome = %q, want %q (answer %q)", verdict.outcome, tc.wantOutcome, tc.answer)
			}
			if verdict.recovered != tc.wantRecover {
				t.Fatalf("recovered = %t, want %t (answer %q)", verdict.recovered, tc.wantRecover, tc.answer)
			}
			if verdict.attributable != tc.wantAttrib {
				t.Fatalf("attributable = %t, want %t (answer %q)", verdict.attributable, tc.wantAttrib, tc.answer)
			}
		})
	}
}

// 歧义在渠道账上恒为假（当前口径已不会产出它，这是防止将来某条路径把它重新接上）。
func TestCircuitProbeAmbiguousOutcomeIsNeverAttributed(t *testing.T) {
	channel := circuitProbeChatChannel("http://127.0.0.1:1", nil)
	result := circuitProbeFinish(channel, CircuitProbeResult{}, circuitProbeVerdict{
		outcome:      CircuitProbeOutcomeAmbiguous,
		attributable: true,
		reason:       "undecidable",
	})
	if result.Attributable {
		t.Fatalf("ambiguous must never be charged to the channel: %#v", result)
	}
	if result.Outcome != CircuitProbeOutcomeAmbiguous {
		t.Fatalf("Outcome = %q, want %q", result.Outcome, CircuitProbeOutcomeAmbiguous)
	}
}

// 历史答案里必须把**本次选中密钥**的字节剔干净（xredact 认不出中转自定义格式），
// 同时保留对排障有用的正文。
func TestCircuitProbeAnswerRedactsSelectedKeyBytes(t *testing.T) {
	ctx := setupModelTestDB(t)
	const channelKey = "acme-live-deadbeef-key"

	var sum atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prompt, _ := circuitProbePromptFromChatBody(t, r)
		expected := circuitProbeExpectedFromPrompt(t, prompt)
		sum.Store(int64(expected))
		answer := fmt.Sprintf("答案是 %d。key=%s", expected, channelKey)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(circuitProbeChatSSE(answer)))
	}))
	t.Cleanup(upstream.Close)

	channel := circuitProbeChatChannel(upstream.URL, []dbmodel.ChannelKey{{ID: 551, Enabled: true, ChannelKey: channelKey}})
	result := RunCircuitProbe(ctx, channel, 551, "probe-model", "openai_chat", "")

	if !result.Recovered {
		t.Fatalf("the right answer must still recover when the body echoes the key: %#v", result)
	}
	if strings.Contains(result.Answer, channelKey) {
		t.Fatalf("the selected channel key must be redacted from the history answer: %q", result.Answer)
	}
	if !strings.Contains(result.Answer, "***") {
		t.Fatalf("redaction must leave a visible placeholder: %q", result.Answer)
	}
	if !strings.Contains(result.Answer, fmt.Sprintf("%d", sum.Load())) {
		t.Fatalf("the useful answer text must survive redaction: %q", result.Answer)
	}
}

// 判读必须在脱敏之前：这段回答里唯一的整数就藏在密钥中间，脱敏后一个数字都不剩了。
// 顺序反过来（先脱敏再判读）会把一个正确的回答判成答非所问。
func TestCircuitProbeJudgeRunsBeforeSanitization(t *testing.T) {
	const shortKey = "x-y"
	if preview := circuitProbeHistoryAnswer("6912 "+shortKey, shortKey); preview != "" {
		t.Fatalf("short echoed key must suppress the preview: %q", preview)
	}
	const channelKey = "acme-live-6912-deadbeef-key"
	const sum = 6912

	raw := circuitProbeJudgeAnswer(channelKey, sum)
	if !raw.recovered || raw.outcome != CircuitProbeOutcomeOK {
		t.Fatalf("judging the raw upstream text must recover: %#v", raw)
	}

	redacted := circuitProbeHistoryAnswer(channelKey, channelKey)
	if strings.Contains(redacted, channelKey) {
		t.Fatalf("history answer leaked the key: %q", redacted)
	}
	if values := circuitProbeIntegers(redacted); len(values) != 0 {
		t.Fatalf("redacted history answer must no longer carry the number (got %v, %q)", values, redacted)
	}
}

// 管理端 Relay Logs：探测行必须是“非计费 / 不计统计 / 无用户或密钥身份 / 无凭据无错误体”的那一条。
func TestCircuitProbeWritesNonBillingRelayLog(t *testing.T) {
	ctx := setupModelTestDB(t)
	// 确定性：不依赖测试库的默认值，先把日志保留打开并清空进程级缓存。
	if err := op.SettingSetString(dbmodel.SettingKeyRelayLogKeepEnabled, "true"); err != nil {
		t.Fatalf("enable relay log keep: %v", err)
	}
	if err := op.RelayLogClear(ctx, nil); err != nil {
		t.Fatalf("clear relay logs: %v", err)
	}

	const channelKey = "acme-live-deadbeef-key"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// 答错 + 把密钥回显进正文：两样都不许进日志。987654 永远不是正确答案。
		_, _ = w.Write([]byte(circuitProbeChatSSE("987654 key=" + channelKey)))
	}))
	t.Cleanup(upstream.Close)

	channel := circuitProbeChatChannel(upstream.URL, []dbmodel.ChannelKey{{ID: 561, Enabled: true, ChannelKey: channelKey}})
	channel.Name = "breaker-probe-log"
	result := RunCircuitProbe(ctx, channel, 561, "probe-model", "openai_chat", "")
	if result.Recovered {
		t.Fatalf("expected a wrong answer, got %#v", result)
	}

	logs, err := op.RelayLogList(ctx, nil, nil, 1, 20, &dbmodel.RelayLogScope{IncludeContent: true})
	if err != nil {
		t.Fatalf("list relay logs: %v", err)
	}
	var probe *dbmodel.RelayLog
	for i := range logs {
		if logs[i].RequestEndpoint == CircuitProbeMarker {
			probe = &logs[i]
			break
		}
	}
	if probe == nil {
		t.Fatalf("no %q row among relay logs", CircuitProbeMarker)
	}

	if probe.UserID != 0 || probe.APIKeyID != 0 {
		t.Fatalf("probe rows must not carry user/api-key identity: user=%d api_key=%d", probe.UserID, probe.APIKeyID)
	}
	if probe.Cost != 0 || probe.InputTokens != 0 || probe.OutputTokens != 0 {
		t.Fatalf("probe rows must not carry fees or usage: cost=%v in=%d out=%d", probe.Cost, probe.InputTokens, probe.OutputTokens)
	}
	for _, marker := range []string{"billable=false", "stats_counted=false", "breaker_counted=false"} {
		if !strings.Contains(probe.ErrorStrategy, marker) {
			t.Fatalf("error_strategy %q missing %q", probe.ErrorStrategy, marker)
		}
	}
	if probe.Error != result.Reason {
		t.Fatalf("error must be our own bounded reason, got %q want %q", probe.Error, result.Reason)
	}
	if probe.ErrorCode != CircuitProbeMarker+"_"+CircuitProbeOutcomeWrongAnswer {
		t.Fatalf("ErrorCode = %q, want the probe marker + outcome", probe.ErrorCode)
	}
	if probe.ErrorStatus != 0 {
		t.Fatalf("a wrong answer is not an HTTP error status, got %d", probe.ErrorStatus)
	}
	if probe.ChannelId != channel.ID || len(probe.Attempts) != 1 || probe.Attempts[0].ChannelKeyID != 561 || probe.TotalAttempts != 1 {
		t.Fatalf("probe row must point at the exact channel/key: %#v", probe)
	}
	if !strings.Contains(probe.ResponseContent, CircuitProbeOutcomeWrongAnswer) {
		t.Fatalf("response content should carry the outcome: %q", probe.ResponseContent)
	}
	if !strings.Contains(probe.RequestContent, result.Prompt) {
		t.Fatalf("request content should carry the question: %q", probe.RequestContent)
	}

	haystacks := []string{probe.RequestContent, probe.ResponseContent, probe.Error}
	for _, attempt := range probe.Attempts {
		haystacks = append(haystacks, attempt.Msg)
	}
	for _, text := range haystacks {
		if strings.Contains(text, channelKey) {
			t.Fatalf("the channel key leaked into the relay log: %q", text)
		}
	}
}

// 题库回归：用固定种子的本地 RNG 抽样，验证三种题型都出现、每种至少三种措辞、
// 不共享固定开头，且每道题的正确答案在数学上正确、操作数都在范围内。
func TestNextCircuitProbePromptBank(t *testing.T) {
	const samples = 600
	rnd := rand.New(rand.NewPCG(0x5eed, 0xc0ffee))
	digits := regexp.MustCompile(`[0-9]+`)

	kindCount := map[string]int{}
	kindShapes := map[string]map[string]bool{
		circuitProbeKindAddition:       {},
		circuitProbeKindSubtraction:    {},
		circuitProbeKindMultiplication: {},
	}
	openings := map[string]bool{}

	for i := 0; i < samples; i++ {
		question := circuitProbeQuestionWith(rnd.IntN)
		kindCount[question.kind]++

		if strings.TrimSpace(question.prompt) == "" || question.expected <= 0 {
			t.Fatalf("question must carry a prompt and a positive expected value: %#v", question)
		}
		lower := strings.ToLower(question.prompt)
		if strings.Contains(lower, "ping") || strings.TrimSpace(lower) == "ok" || strings.TrimSpace(lower) == "hi" {
			t.Fatalf("probe prompt must not use liveness wording: %q", question.prompt)
		}
		if !strings.Contains(question.prompt, strconv.Itoa(question.left)) ||
			!strings.Contains(question.prompt, strconv.Itoa(question.right)) {
			t.Fatalf("prompt %q must show both operands (%d, %d)", question.prompt, question.left, question.right)
		}

		switch question.kind {
		case circuitProbeKindAddition:
			if !fourDigit(question.left) || !fourDigit(question.right) {
				t.Fatalf("addition operands must be four-digit: %#v", question)
			}
			if question.expected != question.left+question.right {
				t.Fatalf("addition expected = %d, want %d: %#v", question.expected, question.left+question.right, question)
			}
		case circuitProbeKindSubtraction:
			if !fourDigit(question.left) || !fourDigit(question.right) {
				t.Fatalf("subtraction operands must be four-digit: %#v", question)
			}
			if question.left <= question.right {
				t.Fatalf("subtraction needs a > b: %#v", question)
			}
			if question.expected != question.left-question.right || question.expected <= 0 {
				t.Fatalf("subtraction expected = %d, want a positive a-b: %#v", question.expected, question)
			}
			// 差不得等于减数，否则“复述算式没算结果”会被宽松判分误判成答对。
			if question.expected == question.right {
				t.Fatalf("subtraction must not pick a == 2b: %#v", question)
			}
		case circuitProbeKindMultiplication:
			if question.left < 12 || question.left > 98 || question.right < 12 || question.right > 98 {
				t.Fatalf("multiplication operands must be in [12,98]: %#v", question)
			}
			if question.expected != question.left*question.right {
				t.Fatalf("multiplication expected = %d, want %d: %#v", question.expected, question.left*question.right, question)
			}
		default:
			t.Fatalf("unknown question kind %q", question.kind)
		}

		normalized := digits.ReplaceAllString(question.prompt, "#")
		kindShapes[question.kind][normalized] = true
		if runes := []rune(normalized); len(runes) >= 4 {
			openings[string(runes[:4])] = true
		}
	}

	for _, kind := range []string{circuitProbeKindAddition, circuitProbeKindSubtraction, circuitProbeKindMultiplication} {
		if kindCount[kind] == 0 {
			t.Fatalf("question kind %q never appeared in %d samples", kind, samples)
		}
		if len(kindShapes[kind]) < 3 {
			t.Fatalf("kind %q has %d distinct phrasings, want at least 3: %v", kind, len(kindShapes[kind]), kindShapes[kind])
		}
	}
	// 不共享固定开头：至少三种不同的开头指纹（题库不能退化成一句固定题）。
	if len(openings) < 3 {
		t.Fatalf("probe prompts share a fixed opening: %v", openings)
	}
}

func fourDigit(value int) bool { return value >= 1000 && value <= 9999 }

// 对外函数冒烟：nextCircuitProbePrompt 直接可用（题面非空、答案是正整数、题面含两个操作数）。
func TestNextCircuitProbePromptUsable(t *testing.T) {
	for i := 0; i < 50; i++ {
		prompt, expected := nextCircuitProbePrompt()
		if strings.TrimSpace(prompt) == "" || expected <= 0 {
			t.Fatalf("nextCircuitProbePrompt() = (%q, %d), want a usable question", prompt, expected)
		}
		if values := circuitProbeIntegers(prompt); len(values) != 2 {
			t.Fatalf("prompt %q must carry exactly two operands, got %v", prompt, values)
		}
	}
}
