package op

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
)

// walTestSetup 起一个临时 SQLite 库，并把 TRUNCATE 阈值调到 1 字节，让测试里几 MB 的 WAL 也算"大"。
func walTestSetup(t *testing.T) (context.Context, string) {
	t.Helper()
	ctx := setupRelayLogTest(t)
	prev := sqliteWALTruncateThresholdBytes
	sqliteWALTruncateThresholdBytes = 1
	t.Cleanup(func() { sqliteWALTruncateThresholdBytes = prev })
	path := sqliteMainPath(ctx)
	if path == "" {
		t.Fatal("临时库应当有文件路径")
	}
	return ctx, path + "-wal"
}

var walTestNextID int64 = 1_000_000

// walTestWrite 写 n 行带 8KB 正文的日志（每行一条独立语句，模拟请求热路径的短写）。
func walTestWrite(t *testing.T, ctx context.Context, n int) {
	t.Helper()
	body := strings.Repeat("w", 8<<10)
	for i := 0; i < n; i++ {
		walTestNextID++
		row := model.RelayLog{ID: walTestNextID, Time: walTestNextID, RequestModelName: "m", RequestContent: body}
		if err := db.GetDB().WithContext(ctx).Create(&row).Error; err != nil {
			t.Fatalf("write row: %v", err)
		}
	}
}

// walTestOpenReader 在一条独立连接上开一个读事务并真正读一行，钉住当前快照（WAL 里的读标记）。
func walTestOpenReader(t *testing.T, ctx context.Context) (*sql.Conn, func()) {
	t.Helper()
	sqlDB, err := db.GetDB().DB()
	if err != nil {
		t.Fatalf("sql db: %v", err)
	}
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatalf("reader conn: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
		t.Fatalf("reader begin: %v", err)
	}
	var cnt int64
	if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM relay_logs").Scan(&cnt); err != nil {
		t.Fatalf("reader select: %v", err)
	}
	done := false
	return conn, func() {
		if done {
			return
		}
		done = true
		_, _ = conn.ExecContext(context.Background(), "COMMIT")
		_ = conn.Close()
	}
}

// 把 WAL 撑大的方式与生产一致：写入期间有长读者挡住 WAL 回绕，自动检查点（PASSIVE）
// 只能一直往后追加。读者结束后 WAL 能回绕了，但文件仍停在高水位——直到这个任务截短它。
func walTestBloat(t *testing.T, ctx context.Context, walPath string) int64 {
	t.Helper()
	_, release := walTestOpenReader(t, ctx)
	walTestWrite(t, ctx, 600) // ~5MB+，远超 autocheckpoint 的 1000 页
	release()
	walTestWrite(t, ctx, 5) // 读者走了以后再写一点，让 PASSIVE 能够把帧全部搬完
	size := fileSizeOrZero(walPath)
	if size < 4<<20 {
		t.Fatalf("WAL 应被撑到 4MB 以上，实际 %d", size)
	}
	return size
}

func TestSQLiteWALCheckpointTruncatesBloatedWAL(t *testing.T) {
	ctx, walPath := walTestSetup(t)
	before := walTestBloat(t, ctx, walPath)

	res, err := sqliteWALCheckpoint(ctx, false)
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if res.Skipped != "" || res.Mode != "truncate" || res.Busy {
		t.Fatalf("无读者时应完成 TRUNCATE: %+v", res)
	}
	if res.BeforeBytes != before {
		t.Fatalf("BeforeBytes=%d, 实际文件 %d", res.BeforeBytes, before)
	}
	if after := fileSizeOrZero(walPath); after != 0 || res.AfterBytes != 0 {
		t.Fatalf("TRUNCATE 后 -wal 应为 0 字节，实际 file=%d res=%d", after, res.AfterBytes)
	}

	// 截短后库照常可写可读，且借用连接的 busy_timeout 已恢复成默认值。
	walTestWrite(t, ctx, 3)
	sqlDB, _ := db.GetDB().DB()
	for i := 0; i < 5; i++ {
		var bt int64
		if err := sqlDB.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&bt); err != nil {
			t.Fatalf("busy_timeout: %v", err)
		}
		if bt != db.SQLiteBusyTimeoutMs {
			t.Fatalf("连接池里出现 busy_timeout=%d 的连接，检查点没恢复默认值", bt)
		}
	}
}

// 阈值以下只做 PASSIVE，不碰 TRUNCATE（平时不去抢写锁）。
func TestSQLiteWALCheckpointPassiveBelowThreshold(t *testing.T) {
	ctx, walPath := walTestSetup(t)
	sqliteWALTruncateThresholdBytes = 1 << 40
	walTestWrite(t, ctx, 20)
	before := fileSizeOrZero(walPath)

	res, err := sqliteWALCheckpoint(ctx, false)
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if res.Mode != "passive" || res.Busy {
		t.Fatalf("阈值以下应只做 PASSIVE: %+v", res)
	}
	if after := fileSizeOrZero(walPath); after != before {
		t.Fatalf("PASSIVE 不应改变 -wal 文件大小: %d → %d", before, after)
	}
}

// 与 VACUUM/压缩互斥：dbVacuumMu 被占着时直接跳过，不等待。
func TestSQLiteWALCheckpointSkipsWhileVacuumRunning(t *testing.T) {
	ctx, walPath := walTestSetup(t)
	before := walTestBloat(t, ctx, walPath)

	dbVacuumMu.Lock()
	start := time.Now()
	res, err := sqliteWALCheckpoint(ctx, false)
	dbVacuumMu.Unlock()
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if res.Skipped != "vacuum running" {
		t.Fatalf("VACUUM 在跑时应跳过: %+v", res)
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatalf("跳过不应等待，实际 %s", time.Since(start))
	}
	if after := fileSizeOrZero(walPath); after != before {
		t.Fatalf("跳过时不应动 -wal: %d → %d", before, after)
	}
}

// 有读者停在旧快照上（PASSIVE 搬不完）：不去试 TRUNCATE，立刻返回 busy，写者不受影响。
// 读者结束后下一轮就能截短——这正是部署后 9.5G 会自己缩回去的路径。
func TestSQLiteWALCheckpointSkipsTruncateWhileOldReader(t *testing.T) {
	ctx, walPath := walTestSetup(t)
	walTestBloat(t, ctx, walPath)

	_, release := walTestOpenReader(t, ctx)
	defer release()
	walTestWrite(t, ctx, 50) // 读者快照之后的新帧：PASSIVE 搬不了

	start := time.Now()
	res, err := sqliteWALCheckpoint(ctx, false)
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("有旧读者时应很快放弃，实际 %s", elapsed)
	}
	if !res.Busy || res.Mode != "passive" || res.CheckpointedFrames >= res.LogFrames {
		t.Fatalf("PASSIVE 没搬完时应标 busy 且不试 TRUNCATE: %+v", res)
	}
	if fileSizeOrZero(walPath) == 0 {
		t.Fatal("读者还在时 -wal 不应被截掉")
	}
	// 写者照常写入。
	wStart := time.Now()
	walTestWrite(t, ctx, 1)
	if time.Since(wStart) > time.Second {
		t.Fatalf("检查点之后写入不应被卡住，实际 %s", time.Since(wStart))
	}

	release()
	res, err = sqliteWALCheckpoint(ctx, false)
	if err != nil {
		t.Fatalf("checkpoint after reader: %v", err)
	}
	if res.Busy || res.Mode != "truncate" || fileSizeOrZero(walPath) != 0 {
		t.Fatalf("读者结束后应截短成功: %+v (file=%d)", res, fileSizeOrZero(walPath))
	}
}

// 读者在最新快照上（PASSIVE 已全部搬完，但读者仍占着 WAL）：TRUNCATE 只等短 busy 预算
// （200ms），拿不到就放弃，不能像默认 5s busy_timeout 那样把请求写入长时间按住。
func TestSQLiteWALCheckpointTruncateGivesUpQuicklyOnCurrentReader(t *testing.T) {
	ctx, walPath := walTestSetup(t)
	walTestBloat(t, ctx, walPath)

	_, release := walTestOpenReader(t, ctx)
	defer release()

	start := time.Now()
	res, err := sqliteWALCheckpoint(ctx, false)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if res.Mode != "truncate" || !res.Busy {
		t.Fatalf("读者占着 WAL 时 TRUNCATE 应报 busy: %+v", res)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("TRUNCATE 应在短 busy 预算内放弃，实际 %s", elapsed)
	}
	if fileSizeOrZero(walPath) == 0 {
		t.Fatal("读者还在时 -wal 不应被截掉")
	}
	wStart := time.Now()
	walTestWrite(t, ctx, 1)
	if time.Since(wStart) > time.Second {
		t.Fatalf("放弃 TRUNCATE 后写入不应被卡住，实际 %s", time.Since(wStart))
	}
}

// 自动回收路径（relayLogVacuum）之后顺手截一次 WAL。
func TestRelayLogVacuumTruncatesWAL(t *testing.T) {
	ctx, walPath := walTestSetup(t)
	sqliteWALTruncateThresholdBytes = 1 << 40 // 证明走的是 forceTruncate，而不是阈值
	walTestBloat(t, ctx, walPath)
	if err := db.GetDB().WithContext(ctx).Exec("DELETE FROM relay_logs").Error; err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := relayLogVacuum(ctx); err != nil {
		t.Fatalf("vacuum: %v", err)
	}
	if after := fileSizeOrZero(walPath); after != 0 {
		t.Fatalf("回收之后 -wal 应被截为 0，实际 %d", after)
	}
}
