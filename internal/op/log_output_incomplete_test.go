package op

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
)

func setupOutputIncompleteTest(t *testing.T) context.Context {
	t.Helper()
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "octopus.db"), false); err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
	})
	ctx := context.Background()
	if err := settingRefreshCache(ctx); err != nil {
		t.Fatalf("refresh setting cache: %v", err)
	}
	t.Cleanup(clearRelayLogCacheForOutputIncompleteTest)
	clearRelayLogCacheForOutputIncompleteTest()
	return ctx
}

func clearRelayLogCacheForOutputIncompleteTest() {
	relayLogCacheLock.Lock()
	relayLogCache = nil
	relayLogCacheLock.Unlock()
}

// 这条用例证明「不建列」也能持久化: 标记只活在 attempts JSON 里, 读回时由
// RelayLog.AfterFind 重算成中性布尔量。
func TestRelayLogOutputIncompleteReloadsFromAttemptsJSON(t *testing.T) {
	ctx := setupOutputIncompleteTest(t)

	if db.GetDB().Migrator().HasColumn(&model.RelayLog{}, "output_incomplete") {
		t.Fatal("output_incomplete must not become a DB column — it is derived from attempts")
	}

	marked := model.RelayLog{
		ID:            9101,
		Time:          time.Now().Unix(),
		TotalAttempts: 1,
		Attempts: []model.ChannelAttempt{{
			ChannelID:              7,
			ChannelName:            "ch-7",
			AttemptNum:             1,
			Status:                 model.AttemptSuccess,
			OutputIncompleteReason: model.RelayLogOutputIncompleteUpstreamMissingTerminal,
		}},
	}
	if err := db.GetDB().WithContext(ctx).Create(&marked).Error; err != nil {
		t.Fatalf("create relay log: %v", err)
	}

	var hydrated model.RelayLog
	if err := db.GetDB().WithContext(ctx).First(&hydrated, "id = ?", marked.ID).Error; err != nil {
		t.Fatalf("reload relay log: %v", err)
	}
	if len(hydrated.Attempts) != 1 {
		t.Fatalf("attempts must round-trip through the JSON column, got %#v", hydrated.Attempts)
	}
	if hydrated.Attempts[0].OutputIncompleteReason != model.RelayLogOutputIncompleteUpstreamMissingTerminal {
		t.Fatalf("the per-attempt reason must survive the reload, got %q", hydrated.Attempts[0].OutputIncompleteReason)
	}
	if !hydrated.OutputIncomplete {
		t.Fatal("AfterFind must re-derive output_incomplete on the hydrated row")
	}
	// 业务字段不许被这个标记碰到。
	if hydrated.Attempts[0].Status != model.AttemptSuccess || hydrated.Severity != model.ClassifyRelayLogSeverity(hydrated) {
		t.Fatalf("the mark must not change status/severity, got %#v", hydrated)
	}

	// 未标记的历史行照旧是 false。
	clean := model.RelayLog{
		ID:            9102,
		Time:          time.Now().Unix(),
		TotalAttempts: 1,
		Attempts:      []model.ChannelAttempt{{ChannelID: 8, Status: model.AttemptSuccess}},
	}
	if err := db.GetDB().WithContext(ctx).Create(&clean).Error; err != nil {
		t.Fatalf("create clean relay log: %v", err)
	}
	var hydratedClean model.RelayLog
	if err := db.GetDB().WithContext(ctx).First(&hydratedClean, "id = ?", clean.ID).Error; err != nil {
		t.Fatalf("reload clean relay log: %v", err)
	}
	if hydratedClean.OutputIncomplete {
		t.Fatal("a row without the mark must stay unflagged")
	}
}

// RelayLogAdd 是每条 relay 日志的唯一入口: 它必须在入队前就把值算在内存副本上,
// 因为非管理员的摘要路径随后会把 attempts 丢掉。
func TestRelayLogAddDerivesOutputIncompleteOnLiveCopy(t *testing.T) {
	ctx := setupOutputIncompleteTest(t)
	if err := SettingSetString(model.SettingKeyRelayLogKeepEnabled, "false"); err != nil {
		t.Fatalf("disable relay log persistence: %v", err)
	}
	t.Cleanup(func() {
		if err := SettingSetString(model.SettingKeyRelayLogKeepEnabled, "true"); err != nil {
			t.Logf("restore relay log persistence=true: %v", err)
		}
	})

	if err := RelayLogAdd(ctx, model.RelayLog{
		ChannelId:        9,
		RequestModelName: "m",
		Attempts: []model.ChannelAttempt{{
			ChannelID:              9,
			Status:                 model.AttemptSuccess,
			OutputIncompleteReason: model.RelayLogOutputIncompleteUpstreamMissingTerminal,
		}},
	}); err != nil {
		t.Fatalf("add relay log: %v", err)
	}

	relayLogCacheLock.Lock()
	defer relayLogCacheLock.Unlock()
	if len(relayLogCache) != 1 {
		t.Fatalf("expected 1 cached relay log, got %d", len(relayLogCache))
	}
	if !relayLogCache[0].OutputIncomplete {
		t.Fatal("RelayLogAdd must derive output_incomplete before the attempts are dropped")
	}
	if relayLogCache[0].Severity != model.ClassifyRelayLogSeverity(relayLogCache[0]) {
		t.Fatal("the derivation must not disturb the severity bucket")
	}
}

// 非管理员路径: 先删 attempts, 但中性布尔量必须留下并进摘要。
func TestRelayLogRedactKeepsOutputIncompleteForSummary(t *testing.T) {
	raw := model.RelayLog{
		Attempts: []model.ChannelAttempt{{
			ChannelID:              1,
			ChannelName:            "ch-1",
			Status:                 model.AttemptSuccess,
			OutputIncompleteReason: model.RelayLogOutputIncompleteUpstreamMissingTerminal,
		}},
		Error: "",
	}
	redacted := RelayLogRedact(raw)
	if len(redacted.Attempts) != 0 {
		t.Fatalf("redaction must still drop the attempts, got %#v", redacted.Attempts)
	}
	if !redacted.OutputIncomplete {
		t.Fatal("redaction must keep the neutral flag")
	}
	summary := RelayLogUserSummary(redacted)
	if !summary.OutputIncomplete {
		t.Fatal("the user summary must carry the flag")
	}

	// 已派生过的行(从 DB 读回)重复脱敏也不许把它算丢掉。
	preDerived := model.RelayLog{OutputIncomplete: true}
	if !RelayLogRedact(preDerived).OutputIncomplete {
		t.Fatal("an already derived flag must survive redaction")
	}
}
