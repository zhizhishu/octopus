package op

import (
	"context"
	"errors"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"gorm.io/gorm"
)

func TestStoragePagedMeasurementSparseScopeAndLargeRow(t *testing.T) {
	ctx := setupRelayLogTest(t)
	rows := make([]model.RelayLog, relayLogStoragePageSize*3+7)
	for i := range rows {
		rows[i] = model.RelayLog{ID: int64(i + 1), UserID: 1, RequestContent: "small"}
		if i%257 == 0 {
			rows[i].UserID = 2
		}
	}
	rows[0].RequestContent = strings.Repeat("x", 2<<20)
	if err := db.GetDB().Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	for _, scope := range []*model.RelayLogScope{nil, {UserID: 2}, {UserID: 3}} {
		var all []model.RelayLog
		if err := relayLogApplyScope(db.GetDB(), scope).Find(&all).Error; err != nil {
			t.Fatal(err)
		}
		var want int64
		for _, row := range all {
			want += relayLogApproxBytes(row)
		}
		got, count, err := relayLogStoredBytes(ctx, scope)
		if err != nil || got != want || count != int64(len(all)) {
			t.Fatalf("scope=%+v bytes=%d/%d rows=%d/%d err=%v", scope, got, want, count, len(all), err)
		}
	}
}

func TestStorageQuotaIncludesProjectionAndKeepsExactPrefix(t *testing.T) {
	ctx := setupRelayLogTest(t)
	rows := []model.RelayLog{{ID: 1}, {ID: 2}, {ID: 3}}
	if err := db.GetDB().Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	relayLogSearchIndexUpsert(db.GetDB(), rows)
	if err := db.GetDB().Table(relayLogSearchIndexTable).Where("id=1").Update("error", strings.Repeat("x", 10000)).Error; err != nil {
		t.Fatal(err)
	}
	measured, err := relayLogMeasureStorage(ctx, db.GetDB(), nil, true)
	if err != nil {
		t.Fatal(err)
	}
	limit := measured.MainBytes + measured.IndexBytes - 1000
	if limit <= measured.MainBytes {
		t.Fatal("fixture must require index bytes to exceed quota")
	}
	if err := SettingSetString(model.SettingKeyRelayLogMaxStorageGB, strconv.FormatFloat(float64(limit)/bytesPerGB, 'f', 12, 64)); err != nil {
		t.Fatal(err)
	}
	deleted, err := relayLogCleanupStorageMeasured(ctx)
	if err != nil || deleted != 1 {
		t.Fatalf("delete shortest prefix: %d %v", deleted, err)
	}
	var ids []int64
	if err := db.GetDB().Model(&model.RelayLog{}).Order("id").Pluck("id", &ids).Error; err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != 2 || ids[1] != 3 {
		t.Fatalf("remaining=%v", ids)
	}
}

func TestStorageMeasurementRejectsMutationAndRefreshesCache(t *testing.T) {
	ctx := setupRelayLogTest(t)
	row := model.RelayLog{ID: 1, RequestContent: strings.Repeat("x", 1000)}
	if err := db.GetDB().Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	before, err := relayLogStorageMeasured(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	end, err := relayLogStorageMutation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := relayLogStorageMeasured(ctx, nil); !errors.Is(err, errLogStorageChanged) {
		end()
		t.Fatalf("active mutation: %v", err)
	}
	if err := db.GetDB().Model(&model.RelayLog{}).Where("id=1").Update("request_content", "").Error; err != nil {
		end()
		t.Fatal(err)
	}
	end()
	after, err := relayLogStorageMeasured(ctx, nil)
	if err != nil || after.Generation == before.Generation || after.MainBytes >= before.MainBytes {
		t.Fatalf("stale measurement published: before=%+v after=%+v err=%v", before, after, err)
	}
}

func TestStorageBackgroundOutlivesCanceledWaiter(t *testing.T) {
	ctx := setupRelayLogTest(t)
	_, release, err := relayLogStorageScanContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	limited, cancel := context.WithCancel(ctx)
	result := make(chan error, 1)
	go func() { _, err := relayLogStorageMeasured(limited, nil); result <- err }()
	// Use cache admission as a deterministic synchronization point.
	var running chan struct{}
	deadline := time.After(time.Second)
	for running == nil {
		runtime.Gosched()
		relayLogStorageCache.Lock()
		running = relayLogStorageCache.running
		relayLogStorageCache.Unlock()
		select {
		case <-deadline:
			release()
			cancel()
			t.Fatal("flight did not start")
		default:
		}
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		release()
		t.Fatal(err)
	}
	select {
	case <-running:
		release()
		t.Fatal("waiter cancellation stopped shared measurement")
	default:
	}
	release()
	select {
	case <-running:
	case <-time.After(time.Second):
		t.Fatal("measurement did not finish after gate release")
	}
	if _, err := relayLogStorageMeasured(ctx, nil); err != nil {
		t.Fatal(err)
	}
}

func TestStorageMeasurementUpperExcludesConcurrentInsert(t *testing.T) {
	ctx := setupRelayLogTest(t)
	rows := []model.RelayLog{{ID: 1}, {ID: 2}}
	if err := db.GetDB().Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	relayLogSearchIndexUpsert(db.GetDB(), rows)
	before, err := relayLogMeasureStorage(ctx, db.GetDB(), nil, true)
	if err != nil {
		t.Fatal(err)
	}
	inserted := false
	callback := "test:storage_insert_after_upper"
	if err := db.GetDB().Callback().Row().After("gorm:row").Register(callback, func(tx *gorm.DB) {
		if inserted || !strings.Contains(tx.Statement.SQL.String(), "page_ids") {
			return
		}
		inserted = true
		row := model.RelayLog{ID: 1000, RequestContent: strings.Repeat("n", 10000)}
		if err := db.GetDB().Create(&row).Error; err != nil {
			t.Error(err)
			return
		}
		relayLogSearchIndexUpsert(db.GetDB(), []model.RelayLog{row})
	}); err != nil {
		t.Fatal(err)
	}
	defer db.GetDB().Callback().Row().Remove(callback)
	after, err := relayLogMeasureStorage(ctx, db.GetDB(), nil, true)
	if err != nil || !inserted || after.Upper != before.Upper || after.MainBytes != before.MainBytes || after.IndexBytes != before.IndexBytes {
		t.Fatalf("new row leaked across upper: before=%+v after=%+v inserted=%v err=%v", before, after, inserted, err)
	}
}

func TestStorageOrphanProjectionDoesNotDeleteLiveLogs(t *testing.T) {
	ctx := setupRelayLogTest(t)
	rows := []model.RelayLog{{ID: 1}, {ID: 2}}
	if err := db.GetDB().Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.GetDB().Table(relayLogSearchIndexTable).Create(map[string]any{"id": 99, "error": strings.Repeat("o", 10000)}).Error; err != nil {
		t.Fatal(err)
	}
	if err := SettingSetString(model.SettingKeyRelayLogMaxStorageGB, "0.000001"); err != nil {
		t.Fatal(err)
	}
	deleted, err := relayLogCleanupStorageMeasured(ctx)
	if err == nil || deleted != 0 {
		t.Fatalf("orphan must report repair without deleting live rows: deleted=%d err=%v", deleted, err)
	}
	var count int64
	if err := db.GetDB().Model(&model.RelayLog{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("orphan caused live deletion: %d", count)
	}
}

func TestStorageCanceledClearPreservesPending(t *testing.T) {
	ctx := setupRelayLogTest(t)
	relayLogAddPending(model.RelayLog{ID: 99001, RequestContent: "keep pending"})
	end, err := relayLogStorageMutation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer end()
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := RelayLogClear(canceled, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled clear: %v", err)
	}
	if err := RelayLogClear(ctx, nil); err == nil {
		t.Fatal("held mutation must report busy instead of waiting")
	}
	relayLogCacheLock.Lock()
	defer relayLogCacheLock.Unlock()
	if len(relayLogCache) != 1 || relayLogCache[0].ID != 99001 || relayLogCache[0].RequestContent != "keep pending" {
		t.Fatalf("canceled/busy clear changed pending rows: %+v", relayLogCache)
	}
}
