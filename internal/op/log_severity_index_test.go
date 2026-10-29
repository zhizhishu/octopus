package op

import (
	"strings"
	"testing"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/db/migrate"
	"github.com/zhizhishu/octopus/internal/model"
	"gorm.io/gorm"
)

// legacySeverityCountSQL is the pre-column aggregate. It is the oracle for rows
// that are still severity=0. It is deliberately not what the fast path runs.
func legacySeverityCountSQL() string {
	return "SELECT COUNT(*) AS total, " +
		"COALESCE(SUM(CASE WHEN NOT " + relayLogErrorSQLCond + " AND COALESCE(total_attempts,0) <= 1 THEN 1 ELSE 0 END),0) AS success_ok, " +
		"COALESCE(SUM(CASE WHEN NOT " + relayLogErrorSQLCond + " AND COALESCE(total_attempts,0) > 1 THEN 1 ELSE 0 END),0) AS warn, " +
		"COALESCE(SUM(CASE WHEN " + relayLogErrorSQLCond + " THEN 1 ELSE 0 END),0) AS success_error " +
		"FROM relay_logs"
}

type severityCountRow struct {
	Total        int64
	SuccessOK    int64
	Warn         int64
	SuccessError int64
}

func (r severityCountRow) counts() model.RelayLogSeverityCounts {
	return model.RelayLogSeverityCounts{
		Total:   r.Total,
		Success: r.SuccessOK,
		Warn:    r.Warn,
		Error:   r.SuccessError,
	}
}

// insertUnsetSeverityRows writes rows whose severity stays 0, the shape of a
// database that has the column but has not been backfilled. SkipHooks bypasses
// BeforeCreate, which would otherwise stamp the bucket.
func insertUnsetSeverityRows(t *testing.T, rows []model.RelayLog) {
	t.Helper()
	if err := db.GetDB().Session(&gorm.Session{SkipHooks: true}).Create(&rows).Error; err != nil {
		t.Fatalf("insert unset rows: %v", err)
	}
	if err := db.GetDB().Model(&model.RelayLog{}).Where("1 = 1").Update("severity", model.RelayLogSeverityUnset).Error; err != nil {
		t.Fatalf("force severity 0: %v", err)
	}
}

// TestRelayLogSeverityUnsetRowsMatchLegacySQL is the parity lock:
// while rows are still severity=0, RelayLogSeverityCounts must equal the old
// TRIM aggregate; after the Go backfill, it must equal ClassifyRelayLogSeverity.
// The tab-only row is the one case where those two oracles disagree.
func TestRelayLogSeverityUnsetRowsMatchLegacySQL(t *testing.T) {
	ctx := setupRelayLogTest(t)

	rows := []model.RelayLog{
		{ID: 1, Time: 10, TotalAttempts: 1},
		{ID: 2, Time: 20, TotalAttempts: 0},
		{ID: 3, Time: 30, TotalAttempts: 3},
		{ID: 4, Time: 40, Error: "boom", TotalAttempts: 1},
		{ID: 5, Time: 50, Error: "   ", TotalAttempts: 1},       // spaces: both oracles say success
		{ID: 6, Time: 60, Error: "   ", TotalAttempts: 4},       // spaces + retries: both say warn
		{ID: 7, Time: 70, ErrorCode: "  ", TotalAttempts: 2},    // blank code: warn
		{ID: 8, Time: 80, Error: "  boom ", TotalAttempts: 1},   // padded text: error
		{ID: 9, Time: 90, ErrorStatus: 500, TotalAttempts: 2},   // status beats retries
		{ID: 10, Time: 100, ErrorStatus: 200, TotalAttempts: 1}, // 200 is success
		{ID: 11, Time: 110, Error: "\t\n", TotalAttempts: 1},    // tab: SQL TRIM says error, Go says success
		{ID: 12, Time: 120, ErrorCode: "\t", TotalAttempts: 1},  // tab code: same split
		{ID: 13, Time: 130, RequestContent: strings.Repeat("x", 5000), Error: "after-body", TotalAttempts: 1},
	}
	insertUnsetSeverityRows(t, rows)

	if mode := relayLogSeveritySQLModeOf(db.GetDB()); mode != relayLogSeveritySQLHybrid {
		t.Fatalf("unset rows should force the hybrid predicate, mode=%d", mode)
	}

	var legacy severityCountRow
	if err := db.GetDB().Raw(legacySeverityCountSQL()).Scan(&legacy).Error; err != nil {
		t.Fatalf("legacy aggregate: %v", err)
	}
	got, err := RelayLogSeverityCounts(ctx, nil, nil, nil)
	if err != nil {
		t.Fatalf("counts during backfill: %v", err)
	}
	if got != legacy.counts() {
		t.Fatalf("unset rows: counts %+v, legacy TRIM aggregate %+v", got, legacy.counts())
	}
	// The tab rows are why the two oracles are not the same function.
	if legacy.SuccessError < 2 {
		t.Fatalf("legacy aggregate should still count tab-only strings as errors, got %+v", legacy.counts())
	}

	complete, err := migrate.BackfillRelayLogSeverity(db.GetDB(), migrate.RelayLogSeverityBackfillOptions{})
	if err != nil || !complete {
		t.Fatalf("backfill complete=%v err=%v", complete, err)
	}

	if mode := relayLogSeveritySQLModeOf(db.GetDB()); mode != relayLogSeveritySQLFast {
		t.Fatalf("after backfill mode=%d, want fast", mode)
	}
	got, err = RelayLogSeverityCounts(ctx, nil, nil, nil)
	if err != nil {
		t.Fatalf("counts after backfill: %v", err)
	}
	want := goSeverityCounts(rows)
	if got != want {
		t.Fatalf("backfilled counts %+v, Go classification %+v", got, want)
	}
	if got == legacy.counts() {
		t.Fatalf("backfill did not move the tab-only rows off the SQL TRIM answer: %+v", got)
	}

	// The body that sits in front of `error` must not hide the message.
	var stored int
	if err := db.GetDB().Raw("SELECT severity FROM relay_logs WHERE id = ?", 13).Scan(&stored).Error; err != nil {
		t.Fatalf("read backfilled severity: %v", err)
	}
	if stored != model.RelayLogSeverityError {
		t.Fatalf("row with a body in front of error: severity=%d, want error", stored)
	}
}

func goSeverityCounts(rows []model.RelayLog) model.RelayLogSeverityCounts {
	var c model.RelayLogSeverityCounts
	for _, row := range rows {
		c.Total++
		switch model.RelayLogSeverityLabel(model.ClassifyRelayLogSeverity(row)) {
		case "success":
			c.Success++
		case "warn":
			c.Warn++
		default:
			c.Error++
		}
	}
	return c
}

// TestRelayLogSeverityCountsUsesCoveringIndex is the plan lock. A regression
// that goes back to TRIM(error), or that lets the planner use idx_relay_logs_time
// and then read severity off the row, fails here.
func TestRelayLogSeverityCountsUsesCoveringIndex(t *testing.T) {
	ctx := setupRelayLogTest(t)
	rows := []model.RelayLog{
		{ID: 1, Time: 1_700_000_000, TotalAttempts: 1},
		{ID: 2, Time: 1_700_000_100, TotalAttempts: 2},
		{ID: 3, Time: 1_700_000_200, Error: "boom", TotalAttempts: 1},
	}
	if err := db.GetDB().WithContext(ctx).Create(&rows).Error; err != nil {
		t.Fatalf("create: %v", err)
	}
	if mode := relayLogSeveritySQLModeOf(db.GetDB()); mode != relayLogSeveritySQLFast {
		t.Fatalf("mode=%d, want fast (no unset rows)", mode)
	}

	start, end := 1_700_000_000, 1_700_000_200
	mode := relayLogSeveritySQLFast
	fastSQL := db.GetDB().ToSQL(func(tx *gorm.DB) *gorm.DB {
		q := tx.Model(&model.RelayLog{}).Where("time >= ? AND time <= ?", start, end)
		q = relayLogApplyScopeMode(q, nil, mode)
		sel, args := relayLogSeverityCountSelect(mode)
		var row severityCountRow
		return q.Select(sel, args...).Scan(&row)
	})
	if strings.Contains(fastSQL, "TRIM(") || strings.Contains(fastSQL, "error_code") || strings.Contains(fastSQL, "request_content") {
		t.Fatalf("fast count SQL still touches body/error columns:\n%s", fastSQL)
	}

	fastPlan := explainQueryPlan(t, fastSQL)
	if !strings.Contains(fastPlan, "USING COVERING INDEX idx_relay_logs_time_severity") {
		t.Fatalf("fast plan:\n%s\nSQL:\n%s", fastPlan, fastSQL)
	}

	legacySQL := legacySeverityCountSQL() + " WHERE time >= 1700000000 AND time <= 1700000200"
	legacyPlan := explainQueryPlan(t, legacySQL)
	if strings.Contains(legacyPlan, "COVERING INDEX idx_relay_logs_time_severity") {
		t.Fatalf("legacy TRIM aggregate unexpectedly used the severity covering index:\n%s", legacyPlan)
	}
	t.Logf("legacy plan:\n%s\nfast plan:\n%s", legacyPlan, fastPlan)

	// The pinned plan has to count the same rows the Go rule does.
	got, err := RelayLogSeverityCounts(ctx, &start, &end, nil)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	want := goSeverityCounts(rows)
	if got != want {
		t.Fatalf("indexed counts %+v, want %+v", got, want)
	}
}

func explainQueryPlan(t *testing.T, sql string) string {
	t.Helper()
	var plans []struct {
		ID      int
		Parent  int
		Notused int
		Detail  string
	}
	if err := db.GetDB().Raw("EXPLAIN QUERY PLAN " + sql).Scan(&plans).Error; err != nil {
		t.Fatalf("explain: %v\nSQL: %s", err, sql)
	}
	parts := make([]string, 0, len(plans))
	for _, p := range plans {
		parts = append(parts, p.Detail)
	}
	return strings.Join(parts, "\n")
}
