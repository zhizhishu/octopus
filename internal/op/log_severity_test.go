package op

import (
	"testing"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
)

// TestRelayLogSeverityValue locks the success/warn/error rule that the SQL filter
// (relayLogApplyScope), the cache filter (relayLogMatchScope), the counts
// (RelayLogSeverityCounts) and the web getRelayLogSeverity all share. If this
// changes, all four must move together or the badges/pagination will diverge.
func TestRelayLogSeverityValue(t *testing.T) {
	cases := []struct {
		name string
		log  model.RelayLog
		want string
	}{
		{"first-attempt success", model.RelayLog{TotalAttempts: 1}, "success"},
		{"zero attempts still success", model.RelayLog{TotalAttempts: 0}, "success"},
		{"retry/failover is warn", model.RelayLog{TotalAttempts: 3}, "warn"},
		{"error message wins", model.RelayLog{Error: "boom", TotalAttempts: 1}, "error"},
		{"error code wins", model.RelayLog{ErrorCode: "upstream_x", TotalAttempts: 1}, "error"},
		{"4xx+ status is error", model.RelayLog{ErrorStatus: 502, TotalAttempts: 5}, "error"},
		{"error beats warn even with retries", model.RelayLog{ErrorStatus: 400, TotalAttempts: 3}, "error"},
		{"sub-400 status is not an error", model.RelayLog{ErrorStatus: 200, TotalAttempts: 1}, "success"},
		{"whitespace-only error is not an error", model.RelayLog{Error: "   ", TotalAttempts: 1}, "success"},
	}
	for _, c := range cases {
		if got := relayLogSeverityValue(c.log); got != c.want {
			t.Errorf("%s: relayLogSeverityValue = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestRelayLogSeverityCountsSQL exercises the real SQL WHERE clauses (relayLogApplyScope)
// through RelayLogSeverityCounts on SQLite, guarding against Go/SQL divergence and the
// nullable-column NOT(...) trap the COALESCE guards protect against.
func TestRelayLogSeverityCountsSQL(t *testing.T) {
	ctx := setupRelayLogTest(t)

	logs := []model.RelayLog{
		{ID: 5001, Time: 5001, RequestModelName: "s", TotalAttempts: 1},                   // success
		{ID: 5002, Time: 5002, RequestModelName: "s", TotalAttempts: 0},                   // success (no attempts recorded)
		{ID: 5003, Time: 5003, RequestModelName: "w", TotalAttempts: 2},                   // warn (retry)
		{ID: 5004, Time: 5004, RequestModelName: "w", TotalAttempts: 5},                   // warn
		{ID: 5005, Time: 5005, RequestModelName: "e", Error: "boom", TotalAttempts: 1},    // error
		{ID: 5006, Time: 5006, RequestModelName: "e", ErrorCode: "upstream_x"},            // error
		{ID: 5007, Time: 5007, RequestModelName: "e", ErrorStatus: 502, TotalAttempts: 4}, // error (beats warn)
	}
	if err := db.GetDB().WithContext(ctx).Create(&logs).Error; err != nil {
		t.Fatalf("create logs: %v", err)
	}

	counts, err := RelayLogSeverityCounts(ctx, nil, nil, nil)
	if err != nil {
		t.Fatalf("severity counts: %v", err)
	}
	if counts.Success != 2 {
		t.Errorf("success = %d, want 2", counts.Success)
	}
	if counts.Warn != 2 {
		t.Errorf("warn = %d, want 2", counts.Warn)
	}
	if counts.Error != 3 {
		t.Errorf("error = %d, want 3", counts.Error)
	}
	if counts.Total != 7 {
		t.Errorf("total = %d, want 7", counts.Total)
	}

	// A severity-scoped list must return exactly the matching rows (SQL filter path).
	warnLogs, err := RelayLogList(ctx, nil, nil, 1, 20, &model.RelayLogScope{Severity: "warn"})
	if err != nil {
		t.Fatalf("list warn: %v", err)
	}
	if len(warnLogs) != 2 {
		t.Fatalf("warn list len = %d, want 2", len(warnLogs))
	}
	for _, l := range warnLogs {
		if relayLogSeverityValue(l) != "warn" {
			t.Errorf("warn list contained non-warn log id=%d", l.ID)
		}
	}
}
