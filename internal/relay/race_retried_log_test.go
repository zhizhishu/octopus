package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// 竞速模式下「一个赢、其余被取消」是正常成功路径: 输家仍留在 attempts 里做审计,
// 但不能算成 total_attempts —— 否则每个竞速成功的请求都被标成「重试过」,
// RetriedOnly 筛选在竞速环境下等于全量。
func TestRaceWinnerLosersAreNotCountedAsRetry(t *testing.T) {
	winnerServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		// 让三个 contender 都先越过 winnerFound 检查, 输家才确定性地走 cancel 路径。
		time.Sleep(150 * time.Millisecond)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"id":"race-win","choices":[{"message":{"role":"assistant","content":"real answer"},"finish_reason":"stop"}]}`))
	}))
	defer winnerServer.Close()

	slow := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			// 比胜者慢很多: 它的请求会在回包前被胜者的取消掐掉。固定 sleep 而非等
			// ctx.Done(), 否则断言失败时 Close 会挂在没回包的连接上。
			time.Sleep(500 * time.Millisecond)
		}))
	}
	loserA, loserB := slow(), slow()
	defer loserA.Close()
	defer loserB.Close()

	_, request, _ := setupRaceTestRequest(t, winnerServer.URL)
	if err := op.InitCache(); err != nil {
		t.Fatalf("init cache: %v", err)
	}
	if err := op.SettingSetString(dbmodel.SettingKeyRelayLogKeepEnabled, "true"); err != nil {
		t.Fatalf("enable relay log keep: %v", err)
	}
	// relayLogCache 是进程级的, 兄弟用例的行会残留 —— 先清掉才能断言"只有一行"。
	if err := op.RelayLogClear(context.Background(), nil); err != nil {
		t.Fatalf("clear relay logs: %v", err)
	}
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
		newChannel(2, "loser-a", loserA.URL),
		newChannel(3, "loser-b", loserB.URL),
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

	// 审计不丢: 输家仍然是 attempts 里的一条 raced_out。
	racedOut := 0
	for _, attempt := range request.iter.Attempts() {
		if attempt.Msg == dbmodel.AttemptMsgRacedOut {
			racedOut++
		}
	}
	if racedOut != 2 {
		t.Fatalf("expected 2 raced_out audit spans, got %d: %#v", racedOut, request.iter.Attempts())
	}
	if got := len(request.iter.Attempts()); got != 3 {
		t.Fatalf("attempts audit must keep all 3 contenders, got %d: %#v", got, request.iter.Attempts())
	}

	request.metrics.Save(context.Background(), true, nil, request.iter.Attempts())

	logs, err := op.RelayLogList(context.Background(), nil, nil, 1, 10, nil)
	if err != nil {
		t.Fatalf("list relay logs: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("expected 1 relay log, got %d", len(logs))
	}
	entry := logs[0]
	if entry.TotalAttempts != 1 {
		t.Fatalf("a raced success has exactly 1 real attempt, got total_attempts=%d (attempts=%d)", entry.TotalAttempts, len(entry.Attempts))
	}
	if severity := op.RelayLogSeverityValue(entry); severity != "success" {
		t.Fatalf("raced success must be severity success, got %q", severity)
	}

	retried, err := op.RelayLogList(context.Background(), nil, nil, 1, 10, &dbmodel.RelayLogScope{RetriedOnly: true})
	if err != nil {
		t.Fatalf("list retried relay logs: %v", err)
	}
	if len(retried) != 0 {
		t.Fatalf("RetriedOnly must not return a clean raced success, got %d rows", len(retried))
	}
}

// 串行故障转移(第一个渠道 500、第二个成功)必须仍然算「重试过」: 这次修的是竞速输家
// 的误计, 不是把重试计数废掉。竞速输家以外的任何失败照旧计入 total_attempts。
func TestSerialFailoverAttemptsStillCountedAsRetry(t *testing.T) {
	serialFailover := []dbmodel.ChannelAttempt{
		{ChannelID: 1, Status: dbmodel.AttemptFailed, Msg: "upstream 500: boom"},
		{ChannelID: 2, Status: dbmodel.AttemptSuccess},
	}
	if got := retryAttemptCount(serialFailover); got != 2 {
		t.Fatalf("serial failover must count 2 attempts, got %d", got)
	}
	if severity := op.RelayLogSeverityValue(dbmodel.RelayLog{TotalAttempts: retryAttemptCount(serialFailover)}); severity != "warn" {
		t.Fatalf("serial failover must be severity warn, got %q", severity)
	}

	// 竞速里一个真正的 500 输家 + 一个赢家: 真实失败照旧计数。
	realFailureLoser := []dbmodel.ChannelAttempt{
		{ChannelID: 1, Status: dbmodel.AttemptFailed, Msg: "upstream 500: boom"},
		{ChannelID: 2, Status: dbmodel.AttemptSuccess},
		{ChannelID: 3, Status: dbmodel.AttemptFailed, Msg: dbmodel.AttemptMsgRacedOut},
	}
	if got := retryAttemptCount(realFailureLoser); got != 2 {
		t.Fatalf("real failure next to a race loser must count 2, got %d", got)
	}
}
