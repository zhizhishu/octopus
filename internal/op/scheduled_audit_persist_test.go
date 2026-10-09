package op

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
)

// TestScheduledAuditFollowUpCallbackCarriesLogID 钉住 follow-up 回调签名带 logID:
// 回显不一致触发跟进时, 只有拿到日志行 ID 才能把审计结论写回那一条日志(先到先得),
// 否则结论永远只躺在内存快照里、重启即丢。
func TestScheduledAuditFollowUpCallbackCarriesLogID(t *testing.T) {
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "octopus.db"), false); err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
	})
	if err := settingRefreshCache(context.Background()); err != nil {
		t.Fatalf("refresh setting cache: %v", err)
	}

	type payload struct {
		channelID int
		model     string
		logID     int64
	}
	got := make(chan payload, 1)
	SetScheduledAuditFollowUp(func(channelID int, modelName string, logID int64) {
		got <- payload{channelID, modelName, logID}
	})
	t.Cleanup(func() { SetScheduledAuditFollowUp(nil) })

	if err := RelayLogAdd(context.Background(), model.RelayLog{
		ChannelId:             7,
		RequestModelName:      "claude-opus-4-8",
		UpstreamModelMismatch: boolPtr(true),
	}); err != nil {
		t.Fatalf("add relay log: %v", err)
	}

	select {
	case p := <-got:
		if p.channelID != 7 || p.model != "claude-opus-4-8" {
			t.Fatalf("callback args = (%d, %s), want (7, claude-opus-4-8)", p.channelID, p.model)
		}
		if p.logID <= 0 {
			t.Fatalf("callback logID = %d, want the snowflake id of the relay log row", p.logID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("follow-up callback was not invoked within 2s")
	}
}

func boolPtr(v bool) *bool { return &v }

// setupAuditMarkTest 起一个临时 SQLite 库(AutoMigrate 会建 relay_logs 表),
// 返回 ctx 供单条 UPDATE 走真实 DB。
func setupAuditMarkTest(t *testing.T) context.Context {
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
	if err := db.GetDB().WithContext(ctx).Create(&model.RelayLog{
		ID:               4242,
		Time:             4242,
		RequestModelName: "claude-opus-4-8",
	}).Error; err != nil {
		t.Fatalf("seed relay log: %v", err)
	}
	return ctx
}

func getAuditColumns(t *testing.T, ctx context.Context, id int64) model.RelayLog {
	t.Helper()
	var row model.RelayLog
	if err := db.GetDB().WithContext(ctx).First(&row, "id = ?", id).Error; err != nil {
		t.Fatalf("load relay log %d: %v", id, err)
	}
	return row
}

// TestRelayLogMarkAuditedWritesFirstToWin 覆盖四件事: 空值才写 / report 落库 /
// 已有结论不覆盖(先到先得) / 不覆盖时返回 nil 而不是报错。
func TestRelayLogMarkAuditedWritesFirstToWin(t *testing.T) {
	ctx := setupAuditMarkTest(t)

	// 现状(改代码前): RelayLogMarkAudited 还不存在、日志行也没有审计列——RED。
	if err := RelayLogMarkAudited(ctx, 4242, "high", 88, 2, "echo_mismatch", `{"score":88}`); err != nil {
		t.Fatalf("mark audited: %v", err)
	}
	row := getAuditColumns(t, ctx, 4242)
	if row.ModelAuditVerdict != "high" || row.ModelAuditScore != 88 || row.ModelAuditFindingN != 2 ||
		row.ModelAuditTrigger != "echo_mismatch" || row.ModelAuditReport != `{"score":88}` {
		t.Fatalf("audit columns not persisted, got %#v", row)
	}

	// 第二个结论来晚了: 不得覆盖先到的, 且必须静默(nil)。
	if err := RelayLogMarkAudited(ctx, 4242, "none", 0, 0, "manual", `{"score":0}`); err != nil {
		t.Fatalf("second mark must not error, got %v", err)
	}
	row = getAuditColumns(t, ctx, 4242)
	if row.ModelAuditVerdict != "high" || row.ModelAuditReport != `{"score":88}` || row.ModelAuditTrigger != "echo_mismatch" {
		t.Fatalf("first verdict must win, got %#v", row)
	}

	// 不存在的行: 影响行数为 0, 不报错。
	if err := RelayLogMarkAudited(ctx, 999999, "low", 1, 1, "manual", "{}"); err != nil {
		t.Fatalf("mark on missing row must not error, got %v", err)
	}
}

// TestRelayLogMarkAuditedOverwritesBlankVerdictOnLegacyRow 老行由 AutoMigrate 补列后
// verdict 是 NULL/空串, 此时第一次写入必须成功。
func TestRelayLogMarkAuditedOverwritesBlankVerdictOnLegacyRow(t *testing.T) {
	ctx := setupAuditMarkTest(t)

	// 直接置 NULL, 模拟 AutoMigrate 补列后的历史行。
	if err := db.GetDB().WithContext(ctx).Exec(
		`UPDATE relay_logs SET model_audit_verdict = NULL WHERE id = ?`, 4242).Error; err != nil {
		t.Fatalf("null out verdict: %v", err)
	}
	if err := RelayLogMarkAudited(ctx, 4242, "medium", 40, 1, "schedule", "{}"); err != nil {
		t.Fatalf("mark audited on NULL verdict row: %v", err)
	}
	row := getAuditColumns(t, ctx, 4242)
	if row.ModelAuditVerdict != "medium" || row.ModelAuditScore != 40 {
		t.Fatalf("expected NULL-verdict row to be writable, got %#v", row)
	}
}
