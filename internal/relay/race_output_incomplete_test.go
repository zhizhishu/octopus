package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// 竞速赢家收尾在截断分支时也要带上标记, 而且只能落在赢家自己那条 attempt 上:
// 输家的 span 各自 End, 标记是 attempt 级字段, 不会从赢家漏过去。
func TestRaceWinnerMarksTruncatedStreamWithoutLoserContamination(t *testing.T) {
	winnerServer := httptest.NewServer(http.HandlerFunc(writeTruncatedChatBody))
	defer winnerServer.Close()

	slowLoser := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		// 输家必须输在回包前: 固定 sleep 而不是等 ctx.Done()，否则断言失败时 Close 会挂住。
		time.Sleep(500 * time.Millisecond)
	}))
	defer slowLoser.Close()

	recorder, request, _ := setupRaceTestRequest(t, winnerServer.URL)
	if err := op.InitCache(); err != nil {
		t.Fatalf("init cache: %v", err)
	}
	if err := op.SettingSetString(dbmodel.SettingKeyRelayLogKeepEnabled, "true"); err != nil {
		t.Fatalf("enable relay log keep: %v", err)
	}
	if err := op.RelayLogClear(context.Background(), nil); err != nil {
		t.Fatalf("clear relay logs: %v", err)
	}
	t.Cleanup(func() { _ = op.RelayLogClear(context.Background(), nil) })

	// 竞速 + 流式: 赢家在 handleStreamResponse 里走到截断分支。
	stream := true
	request.internalRequest.Stream = &stream

	newChannel := func(id int, name, baseURL string) *dbmodel.Channel {
		return &dbmodel.Channel{
			ID:       id,
			Name:     name,
			Type:     outbound.OutboundTypeOpenAIChat,
			Enabled:  true,
			RaceMode: true,
			BaseUrls: []dbmodel.BaseUrl{{URL: baseURL}},
		}
	}
	channels := []*dbmodel.Channel{
		newChannel(1, "winner", winnerServer.URL),
		newChannel(2, "loser", slowLoser.URL),
	}
	contenders := make([]raceContender, 0, len(channels))
	for _, channel := range channels {
		contenders = append(contenders, raceContender{
			channel:         channel,
			key:             dbmodel.ChannelKey{ID: channel.ID, ChannelID: channel.ID, Enabled: true, ChannelKey: "k"},
			internalRequest: request.internalRequest,
			runtimeModel:    "gpt-4o",
		})
	}

	result, winner := runRaceContenders(request, contenders, "chat", 60)
	if !result.Success || winner == nil {
		t.Fatalf("race must succeed: result=%#v winner=%#v", result, winner)
	}
	if winner.channel.ID != 1 {
		t.Fatalf("winner = %d, want the fast channel 1", winner.channel.ID)
	}

	// 既有行为不变: 客户端仍收到 chat 既有的带内错误帧(内含 octopus 码, 并以 [DONE]
	// 传输哨兵收尾 —— 那是 writeChatErrorSSE 的既有格式), 且绝不合成成功
	// finish_reason 收尾。
	body := recorder.Body.String()
	t.Logf("race truncation wire=%q", body)
	if !strings.Contains(body, "octopus_upstream_stream_truncated") || !strings.Contains(body, `"error"`) {
		t.Fatalf("race truncation must keep the chat in-band error frame, got %q", body)
	}
	if strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("race truncation must not synthesize a success finish_reason, got %q", body)
	}

	attempts := request.iter.Attempts()
	marked := 0
	losers := 0
	for _, attempt := range attempts {
		if attempt.Status == dbmodel.AttemptSuccess {
			marked++
			if attempt.OutputIncompleteReason != dbmodel.RelayLogOutputIncompleteUpstreamMissingTerminal {
				t.Fatalf("the winning attempt must be marked, got %#v", attempt)
			}
			continue
		}
		losers++
		if attempt.OutputIncompleteReason != "" {
			t.Fatalf("a race loser must never carry the marker, got %#v", attempt)
		}
	}
	if marked != 1 {
		t.Fatalf("expected exactly 1 winning attempt, got %d: %#v", marked, attempts)
	}
	if losers != 1 {
		t.Fatalf("expected 1 raced-out loser attempt, got %d: %#v", losers, attempts)
	}

	request.metrics.Save(context.Background(), true, nil, attempts)
	logs, err := op.RelayLogList(context.Background(), nil, nil, 1, 10, nil)
	if err != nil {
		t.Fatalf("list relay logs: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("expected 1 relay log, got %d", len(logs))
	}
	entry := logs[0]
	if !entry.OutputIncomplete {
		t.Fatalf("the raced log row must be flagged, attempts=%#v", entry.Attempts)
	}
	// 业务账本完全不变: 竞速成功只有 1 次真实尝试, 仍是 success 桶。
	if entry.TotalAttempts != 1 {
		t.Fatalf("total_attempts = %d, want 1", entry.TotalAttempts)
	}
	if severity := op.RelayLogSeverityValue(entry); severity != "success" {
		t.Fatalf("raced truncation must stay severity success, got %q", severity)
	}
	if entry.Error != "" || entry.ErrorCode != "" {
		t.Fatalf("the observation must not invent an error, got error=%q code=%q", entry.Error, entry.ErrorCode)
	}
}
