package op

import (
	"context"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"gorm.io/gorm"
)

// relayLogSeveritySQLMode selects how a count or filter names an error/warn/success row.
//
//   - fast:    every row has a persisted bucket. The predicate is `severity = ?`,
//     which (time, severity) covers, so SQLite never opens the body overflow pages.
//   - hybrid:  some rows are still 0 (backfill in progress). Classified rows use the
//     column; unset rows use the old TRIM predicate, so the badges stay correct.
//   - legacy:  the column cannot be read (probe failed). The old predicate is
//     still a correct answer for rows this process did not reclassify.
type relayLogSeveritySQLMode int

const (
	relayLogSeveritySQLLegacy relayLogSeveritySQLMode = iota
	relayLogSeveritySQLHybrid
	relayLogSeveritySQLFast
)

// relayLogSeveritySQLModeForScope probes only when this scope will actually emit a
// severity predicate. A list/export/count-by-id with an empty severity pays nothing:
// relayLogApplyScopeRaw ignores mode unless scope.Severity is set. The severity
// *count* SELECT always needs a mode, so RelayLogSeverityCounts calls
// relayLogSeveritySQLModeOf directly and passes that one result into both the
// WHERE and the SUM(CASE).
func relayLogSeveritySQLModeForScope(query *gorm.DB, scope *model.RelayLogScope) relayLogSeveritySQLMode {
	if scope == nil || scope.Severity == "" {
		return relayLogSeveritySQLLegacy
	}
	return relayLogSeveritySQLModeOf(query)
}

// relayLogSeveritySQLModeOf reports which predicate is safe to run right now.
// The probe is `WHERE severity = 0 LIMIT 1` and is served by idx_relay_logs_severity.
// It is not cached: backfill clears the last 0 between requests, and a process-global
// flag would stick across tests that swap the database.
func relayLogSeveritySQLModeOf(query *gorm.DB) relayLogSeveritySQLMode {
	ctx := context.Background()
	if query != nil && query.Statement != nil && query.Statement.Context != nil {
		ctx = query.Statement.Context
	}
	base := db.GetDB()
	if base == nil {
		return relayLogSeveritySQLLegacy
	}
	var probe []struct {
		OK int
	}
	err := base.WithContext(ctx).Raw(
		"SELECT 1 AS ok FROM relay_logs WHERE severity = ? LIMIT 1",
		model.RelayLogSeverityUnset,
	).Scan(&probe).Error
	if err != nil {
		return relayLogSeveritySQLLegacy
	}
	if len(probe) > 0 {
		return relayLogSeveritySQLHybrid
	}
	return relayLogSeveritySQLFast
}

// relayLogSeverityBucketSQL is the WHERE / CASE predicate for one bucket.
// fast and hybrid agree on rows whose severity is already 1/2/3. hybrid also
// accepts severity=0 rows that the old TRIM rule would have placed in the bucket.
func relayLogSeverityBucketSQL(mode relayLogSeveritySQLMode, bucket string) (string, []any) {
	switch bucket {
	case "error":
		switch mode {
		case relayLogSeveritySQLFast:
			return "severity = ?", []any{model.RelayLogSeverityError}
		case relayLogSeveritySQLHybrid:
			return "(severity = ? OR (severity = ? AND " + relayLogErrorSQLCond + "))",
				[]any{model.RelayLogSeverityError, model.RelayLogSeverityUnset}
		default:
			return relayLogErrorSQLCond, nil
		}
	case "warn":
		warnLegacy := "NOT " + relayLogErrorSQLCond + " AND COALESCE(total_attempts,0) > 1"
		switch mode {
		case relayLogSeveritySQLFast:
			return "severity = ?", []any{model.RelayLogSeverityWarn}
		case relayLogSeveritySQLHybrid:
			return "(severity = ? OR (severity = ? AND " + warnLegacy + "))",
				[]any{model.RelayLogSeverityWarn, model.RelayLogSeverityUnset}
		default:
			return warnLegacy, nil
		}
	case "success":
		okLegacy := "NOT " + relayLogErrorSQLCond + " AND COALESCE(total_attempts,0) <= 1"
		switch mode {
		case relayLogSeveritySQLFast:
			return "severity = ?", []any{model.RelayLogSeveritySuccess}
		case relayLogSeveritySQLHybrid:
			return "(severity = ? OR (severity = ? AND " + okLegacy + "))",
				[]any{model.RelayLogSeveritySuccess, model.RelayLogSeverityUnset}
		default:
			return okLegacy, nil
		}
	default:
		return "1 = 1", nil
	}
}

// relayLogSeverityCountSelect is the one-query breakdown. Placeholder order is
// success, warn, error — the same order the aliases are written.
func relayLogSeverityCountSelect(mode relayLogSeveritySQLMode) (string, []any) {
	okExpr, okArgs := relayLogSeverityBucketSQL(mode, "success")
	warnExpr, warnArgs := relayLogSeverityBucketSQL(mode, "warn")
	errExpr, errArgs := relayLogSeverityBucketSQL(mode, "error")
	sql := "COUNT(*) AS total, " +
		"COALESCE(SUM(CASE WHEN " + okExpr + " THEN 1 ELSE 0 END),0) AS success_ok, " +
		"COALESCE(SUM(CASE WHEN " + warnExpr + " THEN 1 ELSE 0 END),0) AS warn, " +
		"COALESCE(SUM(CASE WHEN " + errExpr + " THEN 1 ELSE 0 END),0) AS success_error"
	args := make([]any, 0, len(okArgs)+len(warnArgs)+len(errArgs))
	args = append(args, okArgs...)
	args = append(args, warnArgs...)
	args = append(args, errArgs...)
	return sql, args
}
