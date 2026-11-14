package op

import (
	"strings"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/utils/diskspace"
)

func TestRelayLogDiskPressurePreservesPendingAndResumes(t *testing.T) {
	ctx := setupRelayLogTest(t)
	resetRelayLogQueueForTest(t)
	before := relayLogDiskAvailable
	free := int64(1)
	relayLogDiskAvailable = func(string) (int64, bool) { return free, true }
	t.Cleanup(func() { relayLogDiskAvailable = before })
	row := model.RelayLog{ID: 9301, Time: time.Now().Unix(), UserID: 1, ResponseContent: "unchanged payload"}
	relayLogAddPending(row)
	_, queuedBytes := pendingSnapshot()
	relayLogCacheLock.Lock()
	failuresBefore := relayLogWriteFailures
	relayLogCacheLock.Unlock()
	err := relayLogFlushToDB(ctx)
	if err == nil || !strings.Contains(err.Error(), "disk pressure") {
		t.Fatalf("expected explicit pressure refusal: %v", err)
	}
	if relayLogPermanentWriteError(err) {
		t.Fatal("disk pressure is recoverable, not poison data")
	}
	var stored int64
	if err := db.GetDB().Model(&model.RelayLog{}).Count(&stored).Error; err != nil || stored != 0 {
		t.Fatalf("unexpected DB write: %d %v", stored, err)
	}
	if rows, bytes := pendingSnapshot(); rows != 1 || bytes != queuedBytes {
		t.Fatalf("pressure discarded pending: %d %d", rows, bytes)
	}
	relayLogCacheLock.Lock()
	if relayLogWriteFailures != failuresBefore+1 || relayLogPendingDropped != 0 {
		t.Error("failure/drop observability drifted")
	}
	relayLogCacheLock.Unlock()
	// The ordinary one-second backoff still applies, even after space recovers.
	free = 1 << 40
	if err := relayLogFlushToDB(ctx); err == nil {
		t.Fatal("producer signals bypassed retry backoff")
	}
	relayLogFlushLock.Lock()
	relayLogRetryAfter = time.Time{}
	relayLogFlushLock.Unlock()
	if err := relayLogFlushToDB(ctx); err != nil {
		t.Fatal(err)
	}
	var got model.RelayLog
	if err := db.GetDB().First(&got, "id = ?", row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.ResponseContent != row.ResponseContent {
		t.Fatal("pressure changed stored content")
	}
	if rows, bytes := pendingSnapshot(); rows != 0 || bytes != 0 {
		t.Fatalf("successful retry accounting drifted: %d %d", rows, bytes)
	}
}

func TestRelayLogDiskReserveIncludesBatchAndUnknownPreservesBehavior(t *testing.T) {
	ctx := setupRelayLogTest(t)
	before := relayLogDiskAvailable
	t.Cleanup(func() { relayLogDiskAvailable = before })
	batch := []model.RelayLog{{ResponseContent: strings.Repeat("x", 4096)}}
	needed := diskspace.LogReserveBytes + 2*relayLogPendingBytesOf(batch[0])
	for _, tc := range []struct {
		free            int64
		known, rejected bool
	}{{needed - 1, true, true}, {needed, true, false}, {0, false, false}} {
		relayLogDiskAvailable = func(string) (int64, bool) { return tc.free, tc.known }
		err := relayLogCheckDiskBudget(ctx, batch)
		if (err != nil) != tc.rejected {
			t.Fatalf("free=%d known=%v: %v", tc.free, tc.known, err)
		}
	}
}
