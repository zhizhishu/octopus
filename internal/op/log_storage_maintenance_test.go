package op

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
)

func TestLogStorageMaintenanceSharesCancelableScanGate(t *testing.T) {
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
	// Hold the same gate an active UI aggregate owns, without timing a SQL scan.
	scanCtx, release, err := relayLogStorageScanContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if release != nil {
			release()
		}
	}()
	deadline, ok := scanCtx.Deadline()
	if !ok || time.Until(deadline) > relayLogStorageQueryTimeout {
		t.Fatal("background maintenance scan has no bounded deadline")
	}
	waitsBefore := sqlDB.Stats().WaitCount
	for name, run := range map[string]func(context.Context) error{
		"main":       func(ctx context.Context) error { _, _, err := relayLogStoredBytes(ctx, nil); return err },
		"projection": func(ctx context.Context) error { _, err := relayLogSearchIndexBytes(ctx); return err },
		"ui":         func(ctx context.Context) error { _, err := RelayLogStorageInfo(ctx, nil); return err },
	} {
		t.Run(name, func(t *testing.T) {
			limited, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
			defer cancel()
			if err := run(limited); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("blocked scan must honor cancellation: %v", err)
			}
		})
	}
	if waits := sqlDB.Stats().WaitCount - waitsBefore; waits != 0 {
		t.Fatalf("concurrent aggregate reached the DB while gate held: %d waits", waits)
	}
	release()
	release = nil
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	// Both helpers can run sequentially after cancellation; no leaked permit
	// and no nested acquisition across main and projection aggregation.
	if _, _, err := relayLogStoredBytes(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := relayLogSearchIndexBytes(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := RelayLogStorageInfo(ctx, nil); err != nil {
		t.Fatal(err)
	}
}

func TestLogStorageVacuumQueriesHonorCancellation(t *testing.T) {
	ctx := setupRelayLogTest(t)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := relayLogIncrementalVacuum(canceled, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("incremental probe ignored cancellation: %v", err)
	}
	if _, err := sqliteVacuumLocked(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled incremental probe fell through to full vacuum: %v", err)
	}
	if safe, reason := relayLogVacuumSafe(canceled); safe || reason == "" {
		t.Fatalf("canceled safety probe must fail closed: safe=%v reason=%q", safe, reason)
	}
}
