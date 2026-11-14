package op

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/utils/diskspace"
)

var relayLogDiskAvailable = diskspace.Available

// Only the background SQLite log writer is paused. Forwarding and primary
// business writes are not gated here; pending logs retain their bounded queue.
// Existing stored logs are never deleted by this guard.
func relayLogCheckDiskBudget(ctx context.Context, batch []model.RelayLog) error {
	database := db.GetDB()
	if database == nil || database.Dialector.Name() != "sqlite" {
		return nil
	}
	rows, err := database.WithContext(ctx).Raw("PRAGMA database_list").Rows()
	if err != nil {
		return err
	}
	var file string
	for rows.Next() {
		var seq int
		var name, candidate string
		if err := rows.Scan(&seq, &name, &candidate); err != nil {
			rows.Close()
			return err
		}
		if name == "main" {
			file = candidate
			break
		}
	}
	readErr := rows.Err()
	rows.Close()
	if readErr != nil {
		return readErr
	}
	if file == "" {
		return nil
	} // In-memory SQLite has no local database file.
	free, known := relayLogDiskAvailable(filepath.Dir(file))
	if !known {
		return nil
	} // Preserve logging when this platform cannot measure it.
	var payload int64
	for _, row := range batch {
		payload += relayLogPendingBytesOf(row)
	}
	// Allow headroom for SQLite pages, WAL and the bound payload; not a claim of
	// exact physical accounting or immunity to concurrent external disk usage.
	needed := diskspace.LogReserveBytes + 2*payload
	if free < needed {
		return fmt.Errorf("relay log persistence paused for disk pressure: available=%d reserve=%d", free, needed)
	}
	return nil
}
