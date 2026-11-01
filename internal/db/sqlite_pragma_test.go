package db

import (
	"context"
	"path/filepath"
	"testing"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// journal_size_limit 必须对连接池里的**每条**连接生效：它是连接级设置，只在某一条连接上
// 设过等于没设（WAL 回绕发生在哪条连接上是随机的）。所以同时借出几条不同的连接逐一核对。
func TestInitSQLiteAppliesJournalSizeLimitOnEveryConn(t *testing.T) {
	// 直接走 initSQLite（与 InitDB 用的是同一条 DSN），不跑迁移：迁移里有进程级的一次性
	// 状态，在本包里先跑一遍 InitDB 会干扰同包其它"首次启动"语义的用例。
	gdb, err := initSQLite(filepath.Join(t.TempDir(), "pragma.db"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	sqlDB, err := gdb.DB()
	if err != nil {
		t.Fatalf("sql db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		conn, err := sqlDB.Conn(ctx)
		if err != nil {
			t.Fatalf("conn %d: %v", i, err)
		}
		// 不立刻归还，保证下一轮拿到的是另一条物理连接。
		defer conn.Close()

		var limit int64
		if err := conn.QueryRowContext(ctx, "PRAGMA journal_size_limit").Scan(&limit); err != nil {
			t.Fatalf("conn %d journal_size_limit: %v", i, err)
		}
		if limit != SQLiteJournalSizeLimitBytes {
			t.Fatalf("conn %d journal_size_limit=%d, want %d", i, limit, SQLiteJournalSizeLimitBytes)
		}
		var busy int64
		if err := conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busy); err != nil {
			t.Fatalf("conn %d busy_timeout: %v", i, err)
		}
		if busy != SQLiteBusyTimeoutMs {
			t.Fatalf("conn %d busy_timeout=%d, want %d", i, busy, SQLiteBusyTimeoutMs)
		}
		var mode string
		if err := conn.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
			t.Fatalf("conn %d journal_mode: %v", i, err)
		}
		if mode != "wal" {
			t.Fatalf("conn %d journal_mode=%q, want wal", i, mode)
		}
	}
}
