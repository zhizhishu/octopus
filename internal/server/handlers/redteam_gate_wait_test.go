package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/modeltest"
	"github.com/zhizhishu/octopus/internal/modelverify"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/relay/balancer"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// 红队发送遇本地闸门忙不再一枪定胜负（fix-1128 件3）：与 hlwy 同一条共享分档等待，
// 等名额空出来再发；ctx 结束仍忙才按原样返回闸门忙错误（归因文案由调用方照旧写
// 「渠道限流（本地）」）。
func newRedTeamGateBusyUpstream(t *testing.T, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-gate","object":"chat.completion","model":"model-a","choices":[{"index":0,"message":{"role":"assistant","content":"OCT_BOUNDARY_MARKER"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

func newRedTeamGateChannel(t *testing.T, ctx context.Context, upstreamURL string) model.Channel {
	t.Helper()
	channel := model.Channel{
		Name: "redteam-gate-channel", Type: outbound.OutboundTypeOpenAIChat, Enabled: true,
		Model: "model-a", MaxConcurrent: 1,
		BaseUrls: []model.BaseUrl{{URL: upstreamURL}},
		Keys:     []model.ChannelKey{{Enabled: true, ChannelKey: "sk-example-upstream-test"}},
	}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatal(err)
	}
	return channel
}

// shortenGateWait 把分档等待换成 20ms 短等待，测试不等分钟桶/整秒。
func shortenGateWait(t *testing.T) {
	t.Helper()
	orig := modeltest.ChannelGateTierWait
	modeltest.ChannelGateTierWait = func(int, int) time.Duration { return 20 * time.Millisecond }
	t.Cleanup(func() { modeltest.ChannelGateTierWait = orig })
}

// 闸门忙一次后有名额：红队发送等名额空出来再发，最终成功。
func TestRedTeamLabSendWaitsForGateRoomThenSucceeds(t *testing.T) {
	ctx := setupSettingAuthTestDB(t)
	shortenGateWait(t)
	balancer.ResetChannelGates()
	t.Cleanup(balancer.ResetChannelGates)

	var hits atomic.Int32
	upstream := newRedTeamGateBusyUpstream(t, &hits)
	channel := newRedTeamGateChannel(t, ctx, upstream.URL)

	// 占住唯一并发位，100ms 后释放。
	release, reason := balancer.AdmitChannelGate(context.Background(), channel.ID, 1, 0, 0, 0, 0, nil)
	if reason != "" {
		t.Fatalf("occupy gate: %q", reason)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		release.Release()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := redTeamLabSend(ctx, channel, "model-a", "openai_chat", modelverify.Ask{Prompt: "gate wait test"}); err != nil {
		t.Fatalf("red team send should succeed after gate freed: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("expected exactly one upstream hit after the gate freed, got %d", hits.Load())
	}
}

// 一直忙到 ctx 结束：返回闸门忙错误（归因文案由调用方照旧写「渠道限流（本地）」），
// 期间不panic、不超时卡死。
func TestRedTeamLabSendGateBusyUntilContextExpires(t *testing.T) {
	ctx := setupSettingAuthTestDB(t)
	shortenGateWait(t)
	balancer.ResetChannelGates()
	t.Cleanup(balancer.ResetChannelGates)

	var hits atomic.Int32
	upstream := newRedTeamGateBusyUpstream(t, &hits)
	channel := newRedTeamGateChannel(t, ctx, upstream.URL)

	// 占住唯一并发位，整个用例期间不放。
	release, reason := balancer.AdmitChannelGate(context.Background(), channel.ID, 1, 0, 0, 0, 0, nil)
	if reason != "" {
		t.Fatalf("occupy gate: %q", reason)
	}
	defer release.Release()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := redTeamLabSend(ctx, channel, "model-a", "openai_chat", modelverify.Ask{Prompt: "gate busy test"})
	if err == nil {
		t.Fatal("gate held for whole ctx: send must fail")
	}
	if !modeltest.IsChannelGateBusy(err) {
		t.Fatalf("error = %v, want local gate-busy (归因文案由调用方写「渠道限流（本地）」)", err)
	}
}
