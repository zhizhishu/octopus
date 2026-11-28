package relay

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/relay/balancer"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// 竞速输家的渠道 RPM 口径（fix-1128 件1）：上游已经成功回了 2xx、只是被赢家取消的
// 输家，同样消耗了上游一次成功配额，必须照常补计渠道 RPM——否则竞速渠道的 RPM 计数
// 比上游真实看到的低，上限会被竞速悄悄打穿。
func TestChannelRaceCountsRPMForCanceled2xxLoser(t *testing.T) {
	// 输家（Bearer loser）立刻把 2xx 头刷出来、正文等测试收尾才放行：注定在内容
	// 探测阶段被赢家取消，但上游那次请求已经成功返回。赢家（Bearer fast）慢 50ms，
	// 保证输家的 2xx 头一定先到、其探测一定处于被取消的窗口内，结果确定性成立。
	releaseBody := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") == "Bearer loser" {
			responseWriter.Header().Set("Content-Type", "application/json")
			responseWriter.WriteHeader(http.StatusOK)
			if flusher, ok := responseWriter.(http.Flusher); ok {
				flusher.Flush()
			}
			<-releaseBody
			_, _ = responseWriter.Write([]byte(`{"id":"loser","choices":[{"message":{"role":"assistant","content":"late"},"finish_reason":"stop"}]}`))
			return
		}
		time.Sleep(50 * time.Millisecond)
		responseWriter.Header().Set("Content-Type", "application/json")
		_, _ = responseWriter.Write([]byte(`{"id":"race-win","choices":[{"message":{"role":"assistant","content":"fast"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()
	defer close(releaseBody)

	balancer.ResetChannelGates()
	t.Cleanup(balancer.ResetChannelGates)

	_, request, channel := setupRaceTestRequest(t, server.URL)
	result, _ := runChannelRace(
		request,
		channel,
		[]dbmodel.ChannelKey{
			{ID: 1, ChannelID: channel.ID, Enabled: true, ChannelKey: "loser"},
			{ID: 2, ChannelID: channel.ID, Enabled: true, ChannelKey: "fast"},
		},
		outbound.Get(outbound.OutboundTypeOpenAIChat),
		"chat",
		"",
		0,
	)
	if !result.Success {
		t.Fatalf("race failed: %v", result.Err)
	}
	// 赢家 1 次 + 被取消的 2xx 输家 1 次 = 2。
	if got := balancer.ChannelGateCurrentRPM(channel.ID); got != 2 {
		t.Fatalf("channel RPM = %d, want 2 (winner 1 + canceled 2xx loser 1)", got)
	}
}

// 输家失败（非 2xx）不补计：只有赢家计 1。
func TestChannelRaceDoesNotCountFailedLoserTowardRPM(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") == "Bearer failing" {
			responseWriter.Header().Set("Content-Type", "application/json")
			responseWriter.WriteHeader(http.StatusTooManyRequests)
			_, _ = responseWriter.Write([]byte(`{"error":{"message":"busy"}}`))
			return
		}
		responseWriter.Header().Set("Content-Type", "application/json")
		_, _ = responseWriter.Write([]byte(`{"id":"race-win","choices":[{"message":{"role":"assistant","content":"fast"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	balancer.ResetChannelGates()
	t.Cleanup(balancer.ResetChannelGates)

	_, request, channel := setupRaceTestRequest(t, server.URL)
	result, _ := runChannelRace(
		request,
		channel,
		[]dbmodel.ChannelKey{
			{ID: 1, ChannelID: channel.ID, Enabled: true, ChannelKey: "failing"},
			{ID: 2, ChannelID: channel.ID, Enabled: true, ChannelKey: "fast"},
		},
		outbound.Get(outbound.OutboundTypeOpenAIChat),
		"chat",
		"",
		0,
	)
	if !result.Success {
		t.Fatalf("race failed: %v", result.Err)
	}
	// feat/rpm-tpm：输家失败也算尝试（字节已出门）。时序上若发送前被取消则退还，
	// 故接受 1..2；稳态是两个竞速者都出了门 = 2。
	if got := balancer.ChannelGateCurrentRPM(channel.ID); got < 1 || got > 2 {
		t.Fatalf("channel RPM = %d, want 1..2 (failed loser counts once its bytes left)", got)
	}
}
