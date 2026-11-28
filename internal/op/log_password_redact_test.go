package op

import (
	"strings"
	"testing"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
)

// pack-1128 E：密码类文本在 RelayLogAdd 入队时统一打码（假值，非真实日志内容）；
// 计数键与正文其余部分不动。
func TestRelayLogAddRedactsPasswordClassFields(t *testing.T) {
	ctx := setupRelayLogTest(t)
	if err := SettingSetString(model.SettingKeyRelayLogKeepEnabled, "true"); err != nil {
		t.Fatalf("enable relay log persistence: %v", err)
	}

	entry := model.RelayLog{
		RequestModelName: "redact-test",
		RequestContent:   `{"prompt":"hello","password":"fakepw-req-1"}`,
		ResponseContent:  `{"access_token":"faketok-resp-1","max_tokens":4096}`,
		Error:            "upstream said password=fakepw-err-1 rejected",
		Attempts: []model.ChannelAttempt{
			{ChannelID: 1, Msg: `{"client_secret":"fakesec-att-1"} note`, AttemptNum: 1},
		},
	}
	if err := RelayLogAdd(ctx, entry); err != nil {
		t.Fatalf("add log: %v", err)
	}
	if err := relayLogFlushToDB(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	var rows []model.RelayLog
	if err := db.GetDB().WithContext(ctx).Find(&rows).Error; err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	row := rows[0]
	blob := row.RequestContent + "|" + row.ResponseContent + "|" + row.Error
	for _, leak := range []string{"fakepw-req-1", "faketok-resp-1", "fakepw-err-1"} {
		if strings.Contains(blob, leak) {
			t.Fatalf("%s leaked into stored log: %s", leak, blob)
		}
	}
	if len(row.Attempts) != 1 || strings.Contains(row.Attempts[0].Msg, "fakesec-att-1") {
		t.Fatalf("attempt msg secret leaked: %+v", row.Attempts)
	}
	if !strings.Contains(row.RequestContent, `"prompt":"hello"`) {
		t.Fatalf("ordinary request content distorted: %s", row.RequestContent)
	}
	if !strings.Contains(row.ResponseContent, `"max_tokens":4096`) {
		t.Fatalf("token count key distorted: %s", row.ResponseContent)
	}
}
