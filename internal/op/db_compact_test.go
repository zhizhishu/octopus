package op

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
)

func TestDBCompactSupportedOnlySQLite(t *testing.T) {
	cases := map[string]bool{"sqlite": true, "mysql": false, "postgres": false, "": false}
	for dialect, want := range cases {
		if got := dbCompactSupported(dialect); got != want {
			t.Fatalf("dbCompactSupported(%q)=%v, want %v", dialect, got, want)
		}
	}
}

// 压缩前后大小要如实回报，且确实变小：先灌一批大日志、再删掉，文件里留下空闲页，
// 压缩之后磁盘占用必须下降、空闲页清零。
func TestDBCompactShrinksFileAndReportsSizes(t *testing.T) {
	ctx := setupRelayLogTest(t)
	prev := relayLogLastReclaimUnix.Load()
	t.Cleanup(func() { relayLogLastReclaimUnix.Store(prev) })

	body := strings.Repeat("x", 8<<10)
	logs := make([]model.RelayLog, 0, 600)
	for i := 0; i < 600; i++ {
		logs = append(logs, model.RelayLog{ID: int64(10_000 + i), Time: int64(10_000 + i), RequestModelName: "m", RequestContent: body, ResponseContent: body})
	}
	if err := db.GetDB().WithContext(ctx).Create(&logs).Error; err != nil {
		t.Fatalf("seed logs: %v", err)
	}
	if err := db.GetDB().WithContext(ctx).Exec("DELETE FROM relay_logs").Error; err != nil {
		t.Fatalf("delete logs: %v", err)
	}
	if err := db.GetDB().WithContext(ctx).Exec("PRAGMA wal_checkpoint(TRUNCATE)").Error; err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	info := DBCompactInfoGet(ctx)
	if !info.Supported || info.Dialect != "sqlite" {
		t.Fatalf("sqlite 应当支持压缩: %+v", info)
	}
	if info.FreeBytes < 1<<20 {
		t.Fatalf("删掉 ~9MB 日志后应有可回收空间，实际 %d", info.FreeBytes)
	}
	if info.Running {
		t.Fatal("没有压缩在跑时 running 不该为 true")
	}
	if !info.NeedsFull {
		t.Fatal("新库 auto_vacuum=none，应提示下一次是完整 VACUUM")
	}

	res, err := DBCompactNow(ctx)
	if err != nil {
		t.Fatalf("DBCompactNow: %v", err)
	}
	if !res.StatsValid || res.CheckpointStatus != "truncated" || res.CheckpointErrorCode != "" {
		t.Fatalf("expected validated sizes and truncated WAL: %+v", res)
	}
	if res.BeforeBytes <= 0 || res.AfterBytes <= 0 {
		t.Fatalf("前后大小应为正数: %+v", res)
	}
	if res.AfterBytes >= res.BeforeBytes || res.FreedBytes != res.BeforeBytes-res.AfterBytes {
		t.Fatalf("压缩后应变小且 freed=before-after: %+v", res)
	}
	if res.FreedBytes < 1<<20 {
		t.Fatalf("应至少还回 1MB，实际 %d", res.FreedBytes)
	}
	if res.Mode != "full" {
		t.Fatalf("新库 auto_vacuum=none，首次应走完整 VACUUM，实际 %q", res.Mode)
	}
	if after := DBCompactInfoGet(ctx); after.FreeBytes != 0 || after.NeedsFull {
		t.Fatalf("压缩后空闲页应清零且已切到增量模式，实际 %+v", after)
	}
	if relayLogLastReclaimUnix.Load() == 0 {
		t.Fatal("手动压缩后应重置自动回收冷却期")
	}

	// 第二次：库已切到 incremental，走便宜路径，也不能报错。
	res2, err := DBCompactNow(ctx)
	if err != nil {
		t.Fatalf("second DBCompactNow: %v", err)
	}
	if res2.Mode != "incremental" {
		t.Fatalf("第二次应走增量回收，实际 %q", res2.Mode)
	}
}

// 手动压缩与自动回收共用一把锁：任一在跑，另一路立即拒绝而不是排队。
func TestDBCompactRefusesConcurrentRuns(t *testing.T) {
	ctx := setupRelayLogTest(t)

	dbVacuumMu.Lock()
	info := DBCompactInfoGet(ctx)
	_, err := DBCompactNow(ctx)
	vacErr := relayLogVacuum(ctx)
	dbVacuumMu.Unlock()

	if !info.Running {
		t.Fatal("锁被占用时 info.running 应为 true")
	}
	if !errors.Is(err, ErrDBCompactBusy) {
		t.Fatalf("并发压缩应返回 ErrDBCompactBusy，实际 %v", err)
	}
	if !errors.Is(vacErr, ErrDBCompactBusy) {
		t.Fatalf("压缩进行中，自动回收应直接跳过，实际 %v", vacErr)
	}

	// 锁释放后恢复正常。
	if _, err := DBCompactNow(ctx); err != nil {
		t.Fatalf("锁释放后压缩应成功: %v", err)
	}
}

func TestDBCompactReportsBlockedWALRelease(t *testing.T) {
	ctx := setupRelayLogTest(t)
	prev := relayLogLastReclaimUnix.Load()
	t.Cleanup(func() { relayLogLastReclaimUnix.Store(prev) })
	// Set up incremental mode before opening the reader so VACUUM itself
	// can finish without requiring a schema change while the reader is active.
	if _, err := DBCompactNow(ctx); err != nil {
		t.Fatal(err)
	}
	walTestWrite(t, ctx, 2)
	_, release := walTestOpenReader(t, ctx)
	defer release()
	walTestWrite(t, ctx, 4)
	result, err := DBCompactNow(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.CheckpointStatus != "busy" || !result.StatsValid {
		t.Fatalf("reader must leave explicit incomplete release: %+v", result)
	}
	if result.FreedBytes != 0 || result.AfterBytes < result.BeforeBytes {
		t.Fatalf("pinned WAL must not be reported as reclaimed: %+v", result)
	}
	release()
	result, err = DBCompactNow(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.CheckpointStatus != "truncated" || !result.StatsValid || result.FreedBytes <= 0 {
		t.Fatalf("released reader must allow measured WAL reclamation: %+v", result)
	}
}

func TestSQLiteWALCheckpointCanceledIsNotSkipped(t *testing.T) {
	ctx := setupRelayLogTest(t)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	result, err := sqliteWALCheckpoint(canceled, true)
	if !errors.Is(err, context.Canceled) || result.Skipped != "" {
		t.Fatalf("cancellation must not masquerade as no WAL work: result=%+v err=%v", result, err)
	}
}
