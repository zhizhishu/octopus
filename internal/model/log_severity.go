package model

import (
	"strings"

	"gorm.io/gorm"
)

const (
	// RelayLogSeverityUnset marks a row whose bucket has not been written yet.
	// It is the ADD COLUMN default, so rows that predate this column stay 0
	// until the backfill reaches them. Counts fall back to the old TRIM
	// predicate for these rows, which keeps the badges correct mid-backfill.
	RelayLogSeverityUnset = 0
	// Success/warn/error start at 1 on purpose. 0 is the unset sentinel, and
	// GORM omits a zero int from INSERT, so a success encoded as 0 would be
	// stored as "not yet backfilled".
	RelayLogSeveritySuccess = 1
	RelayLogSeverityWarn    = 2
	RelayLogSeverityError   = 3
)

// ClassifyRelayLogSeverity is the only severity implementation.
//
// It matches the web getRelayLogSeverity (String.prototype.trim):
//   - error:   TrimSpace(error) or TrimSpace(error_code) is non-empty, or error_status >= 400
//   - warn:    not an error, and total_attempts > 1
//   - success: everything else, including zero attempts and whitespace-only errors
//
// Do not replace this with SQL TRIM(). SQLite, MySQL and Postgres TRIM strip
// spaces only, not tab/newline, so a hand-written predicate drifts from Go
// and from the web client.
func ClassifyRelayLogSeverity(relayLog RelayLog) int {
	if strings.TrimSpace(relayLog.Error) != "" ||
		strings.TrimSpace(relayLog.ErrorCode) != "" ||
		relayLog.ErrorStatus >= 400 {
		return RelayLogSeverityError
	}
	if relayLog.TotalAttempts > 1 {
		return RelayLogSeverityWarn
	}
	return RelayLogSeveritySuccess
}

// RelayLogSeverityLabel maps a persisted bucket to the API/filter token.
// Unset (0) returns "" — it is not a fourth bucket the page can ask for.
func RelayLogSeverityLabel(severity int) string {
	switch severity {
	case RelayLogSeveritySuccess:
		return "success"
	case RelayLogSeverityWarn:
		return "warn"
	case RelayLogSeverityError:
		return "error"
	default:
		return ""
	}
}

// BeforeCreate stamps the bucket on every GORM insert (flush, backup import,
// new-api migration). A caller that leaves Severity at 0 would otherwise
// insert a row the counter treats as not-yet-backfilled.
func (l *RelayLog) BeforeCreate(_ *gorm.DB) error {
	if l == nil {
		return nil
	}
	l.Severity = ClassifyRelayLogSeverity(*l)
	return nil
}
