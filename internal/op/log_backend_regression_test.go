package op

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
)

func resetLogRetryForTest() {
	relayLogFlushLock.Lock()
	relayLogRetryAfter = time.Time{}
	relayLogRetryError = nil
	relayLogFlushLock.Unlock()
}

func TestRelayLogLockedWriteRetainsAndRetries(t *testing.T) {
	ctx := setupRelayLogTest(t)
	sqlDB, err := db.GetDB().DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(2)
	writer, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	other, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.ExecContext(ctx, "PRAGMA busy_timeout=1"); err != nil {
		t.Fatal(err)
	}
	other.Close()
	if _, err := writer.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer writer.ExecContext(ctx, "ROLLBACK")
	relayLogAddPending(model.RelayLog{ID: 71001, Time: 1})
	err = relayLogFlushToDB(ctx)
	if !isSQLiteBusyErr(err) {
		t.Fatalf("expected actual SQLite lock error, got %v", err)
	}
	relayLogCacheLock.Lock()
	pending, failures := len(relayLogCache), relayLogWriteFailures
	relayLogCacheLock.Unlock()
	if pending != 1 {
		t.Fatalf("lost locked row: pending=%d", pending)
	}
	if err := relayLogFlushToDB(ctx); err == nil {
		t.Fatal("cooldown must retain failure")
	}
	relayLogCacheLock.Lock()
	if relayLogWriteFailures != failures {
		t.Error("cooldown attempted another write")
	}
	relayLogCacheLock.Unlock()
	if _, err := writer.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	resetLogRetryForTest()
	if err := relayLogFlushToDB(ctx); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.GetDB().Model(&model.RelayLog{}).Where("id = ?", 71001).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("retry rows=%d", count)
	}
}

func TestRelayLogPartialBatchReplayIsIdempotent(t *testing.T) {
	ctx := setupRelayLogTest(t)
	rows := make([]model.RelayLog, 101)
	for i := range rows {
		rows[i] = model.RelayLog{ID: int64(72000 + i), Time: 1}
	}
	// Reproduce the previous nontransactional GORM path: chunk one commits,
	// chunk two fails. Retry must preserve all rows without duplicate-ID poison.
	if err := db.GetDB().Exec("CREATE TRIGGER fail_log_chunk BEFORE INSERT ON relay_logs WHEN NEW.id = 72100 BEGIN SELECT RAISE(ABORT, 'test batch failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.GetDB().Create(&rows).Error; err == nil {
		t.Fatal("expected second chunk failure")
	}
	var count int64
	if err := db.GetDB().Model(&model.RelayLog{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 100 {
		t.Fatalf("fixture did not partially commit: %d", count)
	}
	if err := db.GetDB().Exec("DROP TRIGGER fail_log_chunk").Error; err != nil {
		t.Fatal(err)
	}
	relayLogCacheLock.Lock()
	relayLogCache = rows
	relayLogCacheLock.Unlock()
	if err := relayLogFlushToDB(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.GetDB().Model(&model.RelayLog{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 101 {
		t.Fatalf("replayed rows=%d", count)
	}
}

func TestRelayLogPoisonBatchRollsBackAndCountsDrops(t *testing.T) {
	ctx := setupRelayLogTest(t)
	if err := db.GetDB().Exec("CREATE TRIGGER reject_log BEFORE INSERT ON relay_logs WHEN NEW.id = 73100 BEGIN SELECT RAISE(ABORT, 'constraint failed'); END").Error; err != nil {
		t.Fatal(err)
	}
	relayLogCacheLock.Lock()
	before := relayLogPendingDropped
	for i := 0; i < 101; i++ {
		relayLogCache = append(relayLogCache, model.RelayLog{ID: int64(73000 + i), Time: 1})
	}
	relayLogCacheLock.Unlock()
	if err := relayLogFlushToDB(ctx); err == nil {
		t.Fatal("expected poison failure")
	}
	var count int64
	if err := db.GetDB().Model(&model.RelayLog{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("transaction partially committed: %d", count)
	}
	relayLogCacheLock.Lock()
	defer relayLogCacheLock.Unlock()
	if len(relayLogCache) != 0 || relayLogPendingDropped-before != 101 {
		t.Fatalf("pending=%d drops=%d", len(relayLogCache), relayLogPendingDropped-before)
	}
}

func TestRelayLogStorageCacheScopeConfigAndErrors(t *testing.T) {
	ctx := setupRelayLogTest(t)
	rows := []model.RelayLog{{ID: 74001, UserID: 1, RequestContent: "small"}, {ID: 74002, UserID: 2, RequestContent: "larger content"}}
	if err := db.GetDB().Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	one := &model.RelayLogScope{UserID: 1}
	two := &model.RelayLogScope{UserID: 2}
	first, err := RelayLogStorageInfo(ctx, one)
	if err != nil {
		t.Fatal(err)
	}
	second, err := RelayLogStorageInfo(ctx, two)
	if err != nil {
		t.Fatal(err)
	}
	if second.StoredBytes <= first.StoredBytes {
		t.Fatal("scope totals mixed")
	}
	if err := SettingSetString(model.SettingKeyRelayLogMaxStorageGB, "3"); err != nil {
		t.Fatal(err)
	}
	if err := db.GetDB().Migrator().DropTable(&model.RelayLog{}); err != nil {
		t.Fatal(err)
	}
	cached, err := RelayLogStorageInfo(ctx, one)
	if err != nil || cached.StoredBytes != first.StoredBytes || cached.MaxGB != 3 {
		t.Fatalf("cached totals/live config: %+v %v", cached, err)
	}
	if _, err := RelayLogStorageInfo(ctx, &model.RelayLogScope{UserID: 3}); err == nil {
		t.Fatal("database failure reported as zero")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := RelayLogStorageInfo(canceled, one); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}

func TestRelayLogStorageSingleFlightAndWaiterCancellation(t *testing.T) {
	ctx := setupRelayLogTest(t)
	sqlDB, err := db.GetDB().DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	held, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	leaderCtx, cancelLeader := context.WithCancel(ctx)
	defer cancelLeader()
	leader := make(chan error, 1)
	go func() { _, err := RelayLogStorageInfo(leaderCtx, nil); leader <- err }()
	deadline := time.After(time.Second)
	for {
		relayLogStorageCache.Lock()
		running := relayLogStorageCache.running != nil
		relayLogStorageCache.Unlock()
		if running {
			break
		}
		select {
		case <-deadline:
			t.Fatal("leader never started")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	var waiters sync.WaitGroup
	for i := 0; i < 8; i++ {
		waiters.Add(1)
		go func() {
			defer waiters.Done()
			waitCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
			defer cancel()
			if _, err := RelayLogStorageInfo(waitCtx, nil); !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("waiter: %v", err)
			}
		}()
	}
	waiters.Wait()
	// Only the leader may wait for a database connection.
	if waits := sqlDB.Stats().WaitCount; waits != 1 {
		t.Fatalf("SQL waiters=%d, want 1", waits)
	}
	cancelLeader()
	select {
	case err := <-leader:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("leader: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled reader still pinned")
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := RelayLogStorageInfo(ctx, nil); err != nil {
		t.Fatal(err)
	}
}

func TestDBCompactStatsValidityAndFileSizes(t *testing.T) {
	ctx := setupRelayLogTest(t)
	info := DBCompactInfoGet(ctx)
	if !info.StatsValid || info.ErrorCode != "" || info.MainBytes <= 0 || info.FileBytes != info.MainBytes+info.WALBytes+info.SHMBytes {
		t.Fatalf("valid stats: %+v", info)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	info = DBCompactInfoGet(canceled)
	if info.StatsValid || info.ErrorCode != "canceled" {
		t.Fatalf("canceled stats: %+v", info)
	}
}
