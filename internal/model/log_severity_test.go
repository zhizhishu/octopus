package model

import "testing"

// TestClassifyRelayLogSeverity locks the TrimSpace rule shared with the web
// client (String.prototype.trim) and with the persisted severity column.
// SQL TRIM() is intentionally not the oracle: it only strips U+0020.
func TestClassifyRelayLogSeverity(t *testing.T) {
	cases := []struct {
		name string
		log  RelayLog
		want int
	}{
		{"first attempt", RelayLog{TotalAttempts: 1}, RelayLogSeveritySuccess},
		{"zero attempts", RelayLog{TotalAttempts: 0}, RelayLogSeveritySuccess},
		{"retry is warn", RelayLog{TotalAttempts: 2}, RelayLogSeverityWarn},
		{"many retries", RelayLog{TotalAttempts: 7}, RelayLogSeverityWarn},
		{"error text", RelayLog{Error: "boom", TotalAttempts: 1}, RelayLogSeverityError},
		{"padded error text", RelayLog{Error: "  boom  ", TotalAttempts: 1}, RelayLogSeverityError},
		{"spaces only is success", RelayLog{Error: "   ", TotalAttempts: 1}, RelayLogSeveritySuccess},
		{"spaces only with retries is warn", RelayLog{Error: "   ", TotalAttempts: 3}, RelayLogSeverityWarn},
		{"blank error code with retries is warn", RelayLog{ErrorCode: "  ", TotalAttempts: 2}, RelayLogSeverityWarn},
		{"tab and newline are blank", RelayLog{Error: "\t\n", TotalAttempts: 1}, RelayLogSeveritySuccess},
		{"cr lf vt ff are blank", RelayLog{Error: "\r\n\v\f", TotalAttempts: 1}, RelayLogSeveritySuccess},
		{"nbsp is blank", RelayLog{Error: "\u00a0", TotalAttempts: 1}, RelayLogSeveritySuccess},
		{"tab-only error code is blank", RelayLog{ErrorCode: "\t", TotalAttempts: 4}, RelayLogSeverityWarn},
		{"error code", RelayLog{ErrorCode: "upstream_x"}, RelayLogSeverityError},
		{"error code zero is still an error", RelayLog{ErrorCode: "0", TotalAttempts: 1}, RelayLogSeverityError},
		{"status 400 beats retries", RelayLog{ErrorStatus: 400, TotalAttempts: 9}, RelayLogSeverityError},
		{"status 399 is not an error", RelayLog{ErrorStatus: 399, TotalAttempts: 1}, RelayLogSeveritySuccess},
		{"status 200 with retry is warn", RelayLog{ErrorStatus: 200, TotalAttempts: 2}, RelayLogSeverityWarn},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ClassifyRelayLogSeverity(c.log)
			if got != c.want {
				t.Fatalf("ClassifyRelayLogSeverity = %d (%s), want %d (%s)",
					got, RelayLogSeverityLabel(got), c.want, RelayLogSeverityLabel(c.want))
			}
			if c.want != RelayLogSeverityUnset && RelayLogSeverityLabel(got) == "" {
				t.Fatalf("label for %d is empty", got)
			}
		})
	}
	if RelayLogSeverityLabel(RelayLogSeverityUnset) != "" {
		t.Fatal("unset must not be a named bucket")
	}
}
