package migrate

import (
	"strings"
	"testing"

	"github.com/zhizhishu/octopus/internal/model"
	"gorm.io/gorm"
)

func unsetSeverity(t *testing.T, db *gorm.DB, rows []model.RelayLog) {
	t.Helper()
	if err := db.Session(&gorm.Session{SkipHooks: true}).Create(&rows).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := db.Model(&model.RelayLog{}).Where("1 = 1").Update("severity", model.RelayLogSeverityUnset).Error; err != nil {
		t.Fatalf("zero severity: %v", err)
	}
}

func TestMigration021BackfillsByIDAndIsResumable(t *testing.T) {
	db := newMigrateDB(t)
	rows := []model.RelayLog{
		{ID: 1, Time: 1, TotalAttempts: 1},
		{ID: 2, Time: 2, Error: "boom", TotalAttempts: 1},
		{ID: 3, Time: 3, TotalAttempts: 4},
		{ID: 4, Time: 4, Error: "   ", TotalAttempts: 2},  // spaces + retries = warn
		{ID: 5, Time: 5, Error: "\t\n", TotalAttempts: 1}, // tab-only = success, not SQL TRIM's error
	}
	unsetSeverity(t, db, rows)

	complete, err := BackfillRelayLogSeverity(db, RelayLogSeverityBackfillOptions{MaxRows: 2})
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if complete {
		t.Fatal("first pass stopped at 2 rows but reported complete")
	}
	if RelayLogSeverityBackfillComplete(db) {
		t.Fatal("marker must stay false while severity=0 rows remain")
	}

	var filled []int64
	if err := db.Table("relay_logs").Where("severity <> ?", model.RelayLogSeverityUnset).Order("id").Pluck("id", &filled).Error; err != nil {
		t.Fatalf("pluck filled: %v", err)
	}
	if len(filled) != 2 || filled[0] != 1 || filled[1] != 2 {
		t.Fatalf("id-ordered batch filled %v, want [1 2]", filled)
	}
	var sev2 int
	if err := db.Raw("SELECT severity FROM relay_logs WHERE id = 2").Scan(&sev2).Error; err != nil {
		t.Fatalf("read id 2: %v", err)
	}
	if sev2 != model.RelayLogSeverityError {
		t.Fatalf("id 2 severity=%d, want error", sev2)
	}

	complete, err = BackfillRelayLogSeverity(db, RelayLogSeverityBackfillOptions{})
	if err != nil || !complete {
		t.Fatalf("second pass complete=%v err=%v", complete, err)
	}
	if !RelayLogSeverityBackfillComplete(db) {
		t.Fatal("marker must be true once no unset rows remain")
	}

	var got []int
	if err := db.Table("relay_logs").Order("id").Pluck("severity", &got).Error; err != nil {
		t.Fatalf("pluck severity: %v", err)
	}
	want := []int{
		model.RelayLogSeveritySuccess,
		model.RelayLogSeverityError,
		model.RelayLogSeverityWarn,
		model.RelayLogSeverityWarn,
		model.RelayLogSeveritySuccess,
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("id %d severity=%d, want %d (all %v)", i+1, got[i], want[i], got)
		}
	}

	// A finished pass is a probe, not a rewrite.
	complete, err = BackfillRelayLogSeverity(db, RelayLogSeverityBackfillOptions{})
	if err != nil || !complete {
		t.Fatalf("third pass complete=%v err=%v", complete, err)
	}
	var again []int
	if err := db.Table("relay_logs").Order("id").Pluck("severity", &again).Error; err != nil {
		t.Fatalf("pluck again: %v", err)
	}
	for i := range want {
		if again[i] != want[i] {
			t.Fatalf("rerun changed id %d from %d to %d", i+1, want[i], again[i])
		}
	}
}

func TestMigration021EmptyDatabase(t *testing.T) {
	db := newMigrateDB(t)
	if err := migrateBackfillRelayLogSeverity(db); err != nil {
		t.Fatalf("empty db: %v", err)
	}
	if !RelayLogSeverityBackfillComplete(db) {
		t.Fatal("empty table is complete")
	}
	var names []string
	if err := db.Raw("SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name = 'relay_logs'").Scan(&names).Error; err != nil {
		t.Fatalf("list indexes: %v", err)
	}
	joined := strings.Join(names, " ")
	for _, want := range []string{"idx_relay_logs_severity", "idx_relay_logs_time_severity"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %s in %v", want, names)
		}
	}
}

func TestMigration021ReadFailureDoesNotBlockStartup(t *testing.T) {
	db := newMigrateDB(t)
	unsetSeverity(t, db, []model.RelayLog{{ID: 1, Time: 1, Error: "boom", TotalAttempts: 1}})

	// 删掉回填 SELECT 点名的列，让读批失败。不走 AutoMigrate 把列补回来——
	// 那样失败分支永远跑不到。
	if err := db.Migrator().DropColumn(&model.RelayLog{}, "error"); err != nil {
		t.Fatalf("drop error: %v", err)
	}

	var m Migration
	for _, candidate := range afterAutoMigrations {
		if candidate.Version == 21 {
			m = candidate
			break
		}
	}
	if m.Up == nil {
		t.Fatal("migration 021 is not registered")
	}
	if !m.AlwaysRun {
		t.Fatal("migration 021 must be AlwaysRun so an unfinished backfill resumes")
	}
	if err := m.Up(db); err != nil {
		t.Fatalf("Up must return nil when the backfill cannot finish, got %v", err)
	}
	if RelayLogSeverityBackfillComplete(db) {
		t.Fatal("a failed read must not mark the backfill complete")
	}
}

func TestMigration021BackfillsNullErrorColumns(t *testing.T) {
	db := newMigrateDB(t)
	// 旧行的 error/error_code/error_status/total_attempts 可以是 NULL。
	// 扫进 Go 的 string 若直接接 NULL 会失败，回填就会永远停在这一行上。
	if err := db.Exec(`INSERT INTO relay_logs (id, time, error, error_code, error_status, total_attempts, severity)
		VALUES (1, 1, NULL, NULL, NULL, NULL, 0)`).Error; err != nil {
		t.Fatalf("insert nulls: %v", err)
	}
	complete, err := BackfillRelayLogSeverity(db, RelayLogSeverityBackfillOptions{})
	if err != nil || !complete {
		t.Fatalf("backfill complete=%v err=%v", complete, err)
	}
	var stored int
	if err := db.Raw("SELECT severity FROM relay_logs WHERE id = 1").Scan(&stored).Error; err != nil {
		t.Fatalf("read: %v", err)
	}
	if stored != model.RelayLogSeveritySuccess {
		t.Fatalf("NULL error columns severity=%d, want success", stored)
	}
}

func TestMigration021CreateStampsSeverity(t *testing.T) {
	db := newMigrateDB(t)
	row := model.RelayLog{ID: 7, Time: 7, Error: "  x ", TotalAttempts: 3}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("create: %v", err)
	}
	if row.Severity != model.RelayLogSeverityError {
		t.Fatalf("hook severity=%d, want error", row.Severity)
	}
	var stored int
	if err := db.Raw("SELECT severity FROM relay_logs WHERE id = 7").Scan(&stored).Error; err != nil {
		t.Fatalf("read: %v", err)
	}
	if stored != model.RelayLogSeverityError {
		t.Fatalf("stored severity=%d, want error (success must not collapse to the unset sentinel)", stored)
	}
}
