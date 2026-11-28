package relay

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/relay/balancer"
	"github.com/zhizhishu/octopus/internal/relay/errorjudge"
	"github.com/zhizhishu/octopus/internal/transformer/inbound"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// 「首字超时后立即换渠道」(用户 2026-11-29 02:56 拍板; setting
// relay_first_token_timeout_switch_channel, 默认开).
//
// 判定由两半组成，缺一半就是协议级破坏：
//
//   - 换候选：上游回了响应头、但在分组 first_token_time_out 死线之前没有吐出任何真内容
//     → 这一次尝试判 upstream.first_token_timeout_uncommitted：换下一个候选（渠道/Key）、
//     不计熔断、不进健康分、仍可被自动救援重打。抄 axonhub
//     llm/pipeline/pipeline.go:344-390 的「超时跳过同渠道重试、直接换候选」，落到 octopus
//     就是「本轮判定可重试 → 继续跑候选」，没有别的协议处需要分别打补丁。
//   - 已提交（committed）为界：真内容一旦发给下游客户端，这次尝试就不能再换家，否则客户端会
//     看到重复输出／重复计费。判据抄 sub2api
//     backend/internal/service/openai_first_output_timeout.go:287 的 SafeToFailoverAfterWrite
//     与 openai_gateway_passthrough.go:1256-1267 的「keepalive 注释不算已开始输出」。
//     octopus 侧的对应物是 wroteMeaningfulDownstream（只有真内容才算），opener/心跳不算。
//
// 用例分工：
//   - judge 层三个子用例钉判定（开=换候选不计熔断；关=改动前的通用判定；已提交=永不换）；
//   - 端到端用例钉四个用户可见结果：有别的候选就换过去且每家只打 1 次、熔断不计失败；
//     没有别的候选就在当前渠道重试；已吐字后断流不换候选；开关关闭时维持现状；
//   - 同渠道换 Key 的用例是这套判定的可证伪点：开关开=打第二个 Key，开关关=一次就停。

// firstTokenTimeoutCaseSeconds 是每次尝试的首字死线（分组自身为 0 时取全局默认）。用例把
// 静默钟放远，保证只有「首字死线」一条路径能解释结果。
const firstTokenTimeoutCaseSeconds = "1"

// newFirstTokenTimeoutChannel 注册一个 chat 渠道：上游模型 upstream-model，把客户端的
// request-model 映射过去（与 newChatGin 的请求体配套），因此熔断/运行时分桶的 model 名
// 统一是 upstream-model。
func newFirstTokenTimeoutChannel(t *testing.T, upstreamURL, name string, priority int, keys ...string) dbmodel.Channel {
	t.Helper()
	channelKeys := make([]dbmodel.ChannelKey, 0, len(keys))
	for i, key := range keys {
		channelKeys = append(channelKeys, dbmodel.ChannelKey{
			Enabled:    true,
			ChannelKey: key,
			TotalCost:  float64(i), // 固定 Key 的尝试顺序：先 key[0]，再 key[1]
		})
	}
	channel := dbmodel.Channel{
		Name:         name,
		Type:         outbound.OutboundTypeOpenAIChat,
		Enabled:      true,
		BaseUrls:     []dbmodel.BaseUrl{{URL: upstreamURL}},
		Keys:         channelKeys,
		Model:        "upstream-model",
		ModelMapping: map[string]string{"request-model": "upstream-model"},
		Priority:     priority,
	}
	if err := op.ChannelCreate(&channel, context.Background()); err != nil {
		t.Fatalf("create channel %s: %v", name, err)
	}
	balancer.ResetChannel(channel.ID)
	t.Cleanup(func() { balancer.ResetChannel(channel.ID) })
	return channel
}

// writeFirstTokenStall 模拟「上游收下请求、回了响应头、从此不出字也不挂断」：只发一个非内容
// opener，然后一直占着连接，直到中转主动关掉它的 body（首字死线到点）。
func writeFirstTokenStall(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = w.Write([]byte(chatOpenerEvent()))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	<-r.Context().Done()
}

// writeFirstTokenAnswer 写一段正常的 chat 流（真内容 + 收尾 + [DONE]）。
func writeFirstTokenAnswer(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = w.Write([]byte(chatContentEvent(text)))
	_, _ = w.Write([]byte(`data: {"id":"chatcmpl_ok","object":"chat.completion.chunk","created":123,"model":"upstream-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"))
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// newFirstTokenUpstream 起一个假上游：hits 记每一次真实调用，serve 按第几次调用决定怎么答。
// cleanup 先断连接再关服务，避免被占着连接的 handler 卡住 Close。
func newFirstTokenUpstream(t *testing.T, serve func(w http.ResponseWriter, r *http.Request, hit int32)) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serve(w, r, atomic.AddInt32(&hits, 1))
	}))
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
	})
	return srv, &hits
}

// judge 层：三种判定必须一字不差地对应「用户规则 + committed 分界」。
func TestFirstTokenTimeoutUncommittedJudgment(t *testing.T) {
	setupRelayErrorDB(t)
	// 死线到点用具名错误携带：Error() 与改动前的普通错误逐字节一致，只在裁判层可辨识。
	err := newFirstTokenTimeoutError(3)
	if msg := err.Error(); msg != "first token timeout (3s)" {
		t.Fatalf("error text must stay byte-identical to the pre-change message, got %q", msg)
	}
	if !isFirstTokenTimeoutError(err) {
		t.Fatal("the typed deadline error must be recognisable")
	}
	// attempt() 会用 %w 把上游错误包成 channel <name> failed: ...，包一层之后仍要认得。
	if !isFirstTokenTimeoutError(fmt.Errorf("channel some-channel failed: %w", err)) {
		t.Fatal("the deadline error must survive the per-attempt channel wrapper")
	}
	if isFirstTokenTimeoutError(fmt.Errorf("first token timeout (3s)")) {
		t.Fatal("plain text with the same wording must NOT be treated as the relay's own deadline")
	}

	t.Run("switch on: next candidate, no breaker charge", func(t *testing.T) {
		setIdleSetting(t, dbmodel.SettingKeyRelayFirstTokenTimeoutSwitchChannel, "true")
		// 上游回过响应头（upstreamAnswered=true）、下游什么都没提交 → 与生产一致。
		d := judgeFailureEvidence(err, http.StatusOK, true, false)
		if d.RuleID != "upstream.first_token_timeout_uncommitted" || d.Category != errorjudge.CategoryUpstreamError {
			t.Fatalf("first-token timeout decision = %+v", d)
		}
		if d.Retry != errorjudge.RetryNextChannel {
			t.Fatalf("must retry the next candidate, got retry=%q", d.Retry)
		}
		if d.CountFailure || d.KeyEffect != errorjudge.EffectNone || d.ChannelEffect != errorjudge.EffectNone {
			t.Fatalf("a slow upstream is not a broken channel: %+v", d)
		}
		if !d.Rescue {
			t.Fatal("the failure must stay rescuable, otherwise the 300s auto-recovery is lost")
		}
	})

	t.Run("switch off: pre-change judgment", func(t *testing.T) {
		setIdleSetting(t, dbmodel.SettingKeyRelayFirstTokenTimeoutSwitchChannel, "false")
		t.Cleanup(func() {
			setIdleSetting(t, dbmodel.SettingKeyRelayFirstTokenTimeoutSwitchChannel, "true")
		})
		// 改动前的逐次判定：真 200 + 上游已应答 → fallback.ambiguous（不重试本渠道剩余 Key），
		// 首字超时照旧交给既有候选循环/救援循环，不在本渠道再多吃一次 Key。
		d := judgeFailureEvidence(err, http.StatusOK, true, false)
		if d.RuleID != "fallback.ambiguous" || d.Retry != errorjudge.RetryNone || d.Rescue {
			t.Fatalf("switch off must keep the legacy config judgment, got %+v", d)
		}
		// 终态重判（statusCode 0、未应答）在开关关闭时仍是改动前的 upstream.ambiguous_failure：
		// 可救援、计失败。这条只钉「关掉 = 一字不动」，不是新行为。
		terminal := judgeFailureEvidence(err, 0, false, false)
		if terminal.RuleID != "upstream.ambiguous_failure" || !terminal.Rescue {
			t.Fatalf("switch off must keep the legacy terminal judgment, got %+v", terminal)
		}
	})

	t.Run("committed: never a switch", func(t *testing.T) {
		setIdleSetting(t, dbmodel.SettingKeyRelayFirstTokenTimeoutSwitchChannel, "true")
		// 真内容已经到客户端：不管开关怎么说都不许换家（重复输出的协议级破坏）。
		d := judgeFailureEvidence(err, http.StatusOK, true, true)
		if d.ShouldRetry() || d.Rescue {
			t.Fatalf("a committed stream must never be replayed, got %+v", d)
		}
	})
}

// 端到端 1：首字超时 + 还有别的候选 → 换过去并成功；两家上游各只打 1 次；不计熔断。
func TestFirstTokenTimeoutSwitchesToAnotherCandidate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupRelayErrorDB(t)
	setIdleSetting(t, dbmodel.SettingKeyFirstTokenTimeOutDefault, firstTokenTimeoutCaseSeconds)
	setIdleSetting(t, dbmodel.SettingKeyRelayStreamDataTimeoutSec, "30")
	setIdleSetting(t, dbmodel.SettingKeyRelayStreamKeepaliveSec, "1")

	stalled, stalledHits := newFirstTokenUpstream(t, func(w http.ResponseWriter, r *http.Request, _ int32) {
		writeFirstTokenStall(w, r)
	})
	healthy, healthyHits := newFirstTokenUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ int32) {
		writeFirstTokenAnswer(w, "RESCUED-FROM-ANOTHER-CHANNEL")
	})

	stalledChannel := newFirstTokenTimeoutChannel(t, stalled.URL, "first-token-stalled", 1, "stalled-key")
	newFirstTokenTimeoutChannel(t, healthy.URL, "first-token-healthy", 2, "healthy-key")

	rec, c := newChatGin()
	Handler(inbound.InboundTypeOpenAIChat, c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected the healthy candidate to serve, got %d body %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "RESCUED-FROM-ANOTHER-CHANNEL") {
		t.Fatalf("the healthy candidate's answer must reach the caller, got %q", body)
	}
	if strings.Contains(body, `"type":"upstream_error"`) {
		t.Fatalf("a successful switch must not surface an error frame, got %q", body)
	}
	if got := atomic.LoadInt32(stalledHits); got != 1 {
		t.Fatalf("the stalled upstream must be dispatched exactly once, got %d", got)
	}
	if got := atomic.LoadInt32(healthyHits); got != 1 {
		t.Fatalf("the healthy upstream must be dispatched exactly once, got %d", got)
	}
	// 不计熔断：慢 ≠ 坏。渠道级熔断表与 Key 运行时分桶都必须干净。
	if circuits := balancer.ListChannelCircuits(stalledChannel.ID); len(circuits) != 0 {
		t.Fatalf("a first-token timeout must not trip the breaker: %+v", circuits)
	}
	if stats := balancer.SnapshotKeyRuntime(stalledChannel.ID, stalledChannel.Keys[0].ID, "upstream-model"); stats.RequestFailed != 0 || stats.CooldownRemainingMs != 0 {
		t.Fatalf("a first-token timeout must not charge key health: %+v", stats)
	}
}

// 端到端 2：没有别的候选 → 仍要在当前渠道重试，最终成功（不是直接报错）。
func TestFirstTokenTimeoutWithoutAnotherCandidateRetriesSameChannel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// 唯一候选的再次尝试由自动救援发起，所以这里用开着的 intervention。
	setupRescueDeadlineDB(t)
	withAutoRescueCap(t, 15*time.Second)
	setIdleSetting(t, dbmodel.SettingKeyFirstTokenTimeOutDefault, firstTokenTimeoutCaseSeconds)
	setIdleSetting(t, dbmodel.SettingKeyRelayStreamDataTimeoutSec, "30")

	upstream, hits := newFirstTokenUpstream(t, func(w http.ResponseWriter, r *http.Request, hit int32) {
		if hit == 1 {
			writeFirstTokenStall(w, r)
			return
		}
		writeFirstTokenAnswer(w, "SAME-CHANNEL-RETRY")
	})
	newFirstTokenTimeoutChannel(t, upstream.URL, "first-token-single", 1, "single-key")

	started := time.Now()
	rec, c := newChatGin()
	Handler(inbound.InboundTypeOpenAIChat, c)
	elapsed := time.Since(started)

	if rec.Code != http.StatusOK {
		t.Fatalf("a single-candidate first-token timeout must be retried, not failed, got %d body %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "SAME-CHANNEL-RETRY") {
		t.Fatalf("the retry on the same channel must deliver the answer, got %q", body)
	}
	if got := atomic.LoadInt32(hits); got < 2 {
		t.Fatalf("the single candidate must be retried on the current channel, only %d dispatch happened", got)
	}
	if elapsed > 12*time.Second {
		t.Fatalf("the same-channel retry must stay inside the rescue window, took %s", elapsed)
	}
}

// 端到端 3：已吐字后断流 → 不换候选，按现有断流终局（保留已发内容 + 带内错误帧 + 收尾）。
func TestCommittedStreamStallDoesNotSwitchChannels(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupRelayErrorDB(t)
	// 内容会到，所以首字死线必须离得远；切流的是静默钟。
	setIdleSetting(t, dbmodel.SettingKeyFirstTokenTimeOutDefault, "30")
	setIdleSetting(t, dbmodel.SettingKeyRelayStreamDataTimeoutSec, firstTokenTimeoutCaseSeconds)

	stalled, stalledHits := newFirstTokenUpstream(t, func(w http.ResponseWriter, r *http.Request, _ int32) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(chatContentEvent("PARTIAL-ANSWER")))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done() // 已吐字，然后沉默，连接不断
	})
	healthy, healthyHits := newFirstTokenUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ int32) {
		writeFirstTokenAnswer(w, "MUST-NOT-BE-USED")
	})

	newFirstTokenTimeoutChannel(t, stalled.URL, "committed-then-stalled", 1, "committed-key")
	newFirstTokenTimeoutChannel(t, healthy.URL, "committed-spare", 2, "spare-key")

	rec, c := newChatGin()
	Handler(inbound.InboundTypeOpenAIChat, c)

	body := rec.Body.String()
	if !strings.Contains(body, "PARTIAL-ANSWER") {
		t.Fatalf("content already handed to the caller must be preserved, got %q", body)
	}
	if !strings.Contains(body, `"type":"upstream_error"`) || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("a committed stall must end with the caller's own terminal frame, got %q", body)
	}
	if got := atomic.LoadInt32(healthyHits); got != 0 {
		t.Fatalf("a committed stream must never switch channels, but the spare was dispatched %d time(s)", got)
	}
	if got := atomic.LoadInt32(stalledHits); got != 1 {
		t.Fatalf("the committed channel must be dispatched exactly once, got %d", got)
	}
}

// 端到端 4：开关关闭 → 维持现状（首字超时走原有救援判定），成功路径不能被打断。
func TestFirstTokenTimeoutSwitchOffKeepsExistingBehaviour(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupRelayErrorDB(t)
	setIdleSetting(t, dbmodel.SettingKeyRelayFirstTokenTimeoutSwitchChannel, "false")
	setIdleSetting(t, dbmodel.SettingKeyFirstTokenTimeOutDefault, firstTokenTimeoutCaseSeconds)
	setIdleSetting(t, dbmodel.SettingKeyRelayStreamDataTimeoutSec, "30")
	setIdleSetting(t, dbmodel.SettingKeyRelayStreamKeepaliveSec, "1")

	stalled, stalledHits := newFirstTokenUpstream(t, func(w http.ResponseWriter, r *http.Request, _ int32) {
		writeFirstTokenStall(w, r)
	})
	healthy, healthyHits := newFirstTokenUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ int32) {
		writeFirstTokenAnswer(w, "LEGACY-PATH-ANSWER")
	})

	newFirstTokenTimeoutChannel(t, stalled.URL, "switch-off-stalled", 1, "stalled-key")
	newFirstTokenTimeoutChannel(t, healthy.URL, "switch-off-healthy", 2, "healthy-key")

	rec, c := newChatGin()
	Handler(inbound.InboundTypeOpenAIChat, c)

	if rec.Code != http.StatusOK {
		t.Fatalf("switch off must keep the pre-change recovery, got %d body %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "LEGACY-PATH-ANSWER") {
		t.Fatalf("switch off must still reach the healthy candidate, got %q", body)
	}
	if got := atomic.LoadInt32(stalledHits); got != 1 {
		t.Fatalf("switch off must not add attempts on the stalled channel, got %d", got)
	}
	if got := atomic.LoadInt32(healthyHits); got != 1 {
		t.Fatalf("switch off must dispatch the healthy candidate exactly once, got %d", got)
	}
}

// 端到端 5（可证伪点）：同渠道还有第二个 Key 时，开关决定「再打一次」还是「一次就停」。
// 这是「判定真的进了主链」的证据：撤掉 judge 分支，开的那一半会退化成只打 1 次。
func TestFirstTokenTimeoutSwitchDrivesSameChannelKeyRetry(t *testing.T) {
	cases := []struct {
		name        string
		switchValue string
		wantHits    int32
		wantOK      bool
	}{
		{name: "switch on retries the next key", switchValue: "true", wantHits: 2, wantOK: true},
		{name: "switch off stops after the first key", switchValue: "false", wantHits: 1, wantOK: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			setupRelayErrorDB(t)
			setIdleSetting(t, dbmodel.SettingKeyRelayFirstTokenTimeoutSwitchChannel, tc.switchValue)
			setIdleSetting(t, dbmodel.SettingKeyFirstTokenTimeOutDefault, firstTokenTimeoutCaseSeconds)
			setIdleSetting(t, dbmodel.SettingKeyRelayStreamDataTimeoutSec, "30")

			var mu sync.Mutex
			var seenAuth []string
			upstream, hits := newFirstTokenUpstream(t, func(w http.ResponseWriter, r *http.Request, hit int32) {
				mu.Lock()
				seenAuth = append(seenAuth, r.Header.Get("Authorization"))
				mu.Unlock()
				if hit == 1 {
					writeFirstTokenStall(w, r)
					return
				}
				writeFirstTokenAnswer(w, "SECOND-KEY")
			})
			newFirstTokenTimeoutChannel(t, upstream.URL, "first-token-multi-key", 1, "ftt-key-one", "ftt-key-two")

			rec, c := newChatGin()
			Handler(inbound.InboundTypeOpenAIChat, c)

			if got := atomic.LoadInt32(hits); got != tc.wantHits {
				t.Fatalf("switch=%s: expected %d dispatch(es), got %d", tc.switchValue, tc.wantHits, got)
			}
			if tc.wantOK {
				if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "SECOND-KEY") {
					t.Fatalf("switch on must deliver the second key's answer, got %d body %s", rec.Code, rec.Body.String())
				}
				mu.Lock()
				gotAuth := append([]string(nil), seenAuth...)
				mu.Unlock()
				if len(gotAuth) != 2 || gotAuth[0] == gotAuth[1] {
					t.Fatalf("the retry must use the channel's other key, saw %v", gotAuth)
				}
				return
			}
			if rec.Code < 400 {
				t.Fatalf("switch off leaves nothing to retry, so the caller must get an explicit failure, got %d body %s",
					rec.Code, rec.Body.String())
			}
		})
	}
}
