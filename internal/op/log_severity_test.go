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

	// Fuzzy search filter tests (user_name, request_api_key_name, error, error_code).
	searchLogs := []model.RelayLog{
		{ID: 6001, Time: 6001, RequestModelName: "m", UserName: "AliceAdmin", RequestAPIKeyName: "key-1", Error: "Connection timeout"},
		{ID: 6002, Time: 6002, RequestModelName: "m", UserName: "BobUser", RequestAPIKeyName: "SecretProdKey", ErrorCode: "rate_limit_exceeded"},
		{ID: 6003, Time: 6003, RequestModelName: "m", UserName: "Charlie", RequestAPIKeyName: "dev-key", Error: "all upstream failed: dial tcp timeout"},
	}
	if err := db.GetDB().WithContext(ctx).Create(&searchLogs).Error; err != nil {
		t.Fatalf("create search logs: %v", err)
	}

	// Search by user_name substring (case-insensitive)
	found, err := RelayLogList(ctx, nil, nil, 1, 20, &model.RelayLogScope{Search: "alice"})
	if err != nil || len(found) != 1 || found[0].ID != 6001 {
		t.Errorf("search alice: got %v (len %d), want id 6001", found, len(found))
	}

	// Search by request_api_key_name substring (case-insensitive)
	found, err = RelayLogList(ctx, nil, nil, 1, 20, &model.RelayLogScope{Search: "prodkey"})
	if err != nil || len(found) != 1 || found[0].ID != 6002 {
		t.Errorf("search prodkey: got %v (len %d), want id 6002", found, len(found))
	}

	// Search by error substring (case-insensitive)
	found, err = RelayLogList(ctx, nil, nil, 1, 20, &model.RelayLogScope{Search: "TIMEOUT"})
	if err != nil || len(found) != 2 {
		t.Errorf("search TIMEOUT: got %d rows, want 2 (6001 and 6003)", len(found))
	}

	// Search by error_code substring
	found, err = RelayLogList(ctx, nil, nil, 1, 20, &model.RelayLogScope{Search: "rate_limit"})
	if err != nil || len(found) != 1 || found[0].ID != 6002 {
		t.Errorf("search rate_limit: got %v (len %d), want id 6002", found, len(found))
	}

	// Extended fuzzy search & provider/model filter tests
	extendedLogs := []model.RelayLog{
		{ID: 7001, Time: 7001, RequestModelName: "gpt-4o", ActualModelName: "gpt-4o-2024-08-06", ChannelId: 12, ChannelName: "OpenAI Main", RequestEndpoint: "chat", RequestPath: "/v1/chat/completions", SessionKey: "sess-abc-123"},
		{ID: 7002, Time: 7002, RequestModelName: "claude-3-5-sonnet", ActualModelName: "claude-3-5-sonnet-20241022", ChannelId: 34, ChannelName: "Anthropic Direct", RequestEndpoint: "messages", RequestPath: "/v1/messages", SessionKey: "sess-def-456"},
		{ID: 7003, Time: 7003, RequestModelName: "gemini-1.5-pro", ActualModelName: "gemini-1.5-pro-002", ChannelId: 56, ChannelName: "Google Vertex", RequestEndpoint: "gemini_generate_content", RequestPath: "/v1beta/models/gemini-1.5-pro:generateContent"},
		{ID: 7004, Time: 7004, RequestModelName: "deepseek-chat", ActualModelName: "deepseek-v3", ChannelId: 78, ChannelName: "DeepSeek Official", RequestEndpoint: "chat", RequestPath: "/v1/chat/completions"},
	}
	if err := db.GetDB().WithContext(ctx).Create(&extendedLogs).Error; err != nil {
		t.Fatalf("create extended logs: %v", err)
	}

	// 1. Search by channel_name
	found, err = RelayLogList(ctx, nil, nil, 1, 20, &model.RelayLogScope{Search: "vertex"})
	if err != nil || len(found) != 1 || found[0].ID != 7003 {
		t.Errorf("search channel_name vertex: got %v (len %d), want id 7003", found, len(found))
	}

	// 2. Search by request_endpoint / request_path
	found, err = RelayLogList(ctx, nil, nil, 1, 20, &model.RelayLogScope{Search: "generateContent"})
	if err != nil || len(found) != 1 || found[0].ID != 7003 {
		t.Errorf("search request_path generateContent: got %v (len %d), want id 7003", found, len(found))
	}

	// 3. Search by session_key
	found, err = RelayLogList(ctx, nil, nil, 1, 20, &model.RelayLogScope{Search: "sess-abc"})
	if err != nil || len(found) != 1 || found[0].ID != 7001 {
		t.Errorf("search session_key sess-abc: got %v (len %d), want id 7001", found, len(found))
	}

	// 4. Search by numeric log ID or channel ID
	found, err = RelayLogList(ctx, nil, nil, 1, 20, &model.RelayLogScope{Search: "7002"})
	if err != nil || len(found) != 1 || found[0].ID != 7002 {
		t.Errorf("search numeric log id 7002: got %v (len %d), want id 7002", found, len(found))
	}
	found, err = RelayLogList(ctx, nil, nil, 1, 20, &model.RelayLogScope{Search: "56"})
	foundChannelID := false
	for _, relayLog := range found {
		if relayLog.ID == 7003 {
			foundChannelID = true
			break
		}
	}
	if err != nil || !foundChannelID {
		t.Errorf("search numeric channel id 56: got %v, want results to include id 7003", found)
	}

	// 5. Filter by provider taxonomy (anthropic, openai, google, deepseek)
	found, err = RelayLogList(ctx, nil, nil, 1, 20, &model.RelayLogScope{Provider: "anthropic"})
	if err != nil || len(found) != 1 || found[0].ID != 7002 {
		t.Errorf("filter provider anthropic: got %v (len %d), want id 7002", found, len(found))
	}
	found, err = RelayLogList(ctx, nil, nil, 1, 20, &model.RelayLogScope{Provider: "deepseek"})
	if err != nil || len(found) != 1 || found[0].ID != 7004 {
		t.Errorf("filter provider deepseek: got %v (len %d), want id 7004", found, len(found))
	}

	// 6. Filter by model (request or actual)
	found, err = RelayLogList(ctx, nil, nil, 1, 20, &model.RelayLogScope{Model: "gpt-4o"})
	if err != nil || len(found) != 1 || found[0].ID != 7001 {
		t.Errorf("filter request_model gpt-4o: got %v (len %d), want id 7001", found, len(found))
	}
	found, err = RelayLogList(ctx, nil, nil, 1, 20, &model.RelayLogScope{Model: "gpt-4o-2024-08-06"})
	if err != nil || len(found) != 1 || found[0].ID != 7001 {
		t.Errorf("filter actual_model gpt-4o-2024-08-06: got %v (len %d), want id 7001", found, len(found))
	}

	// 7. Filter by endpoint family (e.g. "gemini" family matches "gemini_generate_content")
	found, err = RelayLogList(ctx, nil, nil, 1, 20, &model.RelayLogScope{Endpoint: "gemini"})
	if err != nil || len(found) != 1 || found[0].ID != 7003 {
		t.Errorf("filter endpoint family gemini: got %v (len %d), want id 7003", found, len(found))
	}

	// 8. Search numeric matching channel_id
	numericLogs := []model.RelayLog{
		{ID: 8001, Time: 8001, RequestModelName: "gpt-4o", ChannelId: 999, ChannelName: "Custom Channel 999"},
	}
	if err := db.GetDB().WithContext(ctx).Create(&numericLogs).Error; err != nil {
		t.Fatalf("create numeric logs: %v", err)
	}
	found, err = RelayLogList(ctx, nil, nil, 1, 20, &model.RelayLogScope{Search: "999"})
	if err != nil || len(found) != 1 || found[0].ID != 8001 {
		t.Errorf("search channel_id 999: got %v (len %d), want id 8001", found, len(found))
	}

	// 9. Provider SQL and memory consistency tests
	// Test slash provider prefix "openai/custom-model" matches provider "openai"
	slashLogs := []model.RelayLog{
		{ID: 8002, Time: 8002, RequestModelName: "openai/custom-agent", ActualModelName: "openai/custom-agent", ChannelId: 10},
		{ID: 8003, Time: 8003, RequestModelName: "meta-llama/Llama-3-70B", ActualModelName: "meta-llama/Llama-3-70B", ChannelId: 11},
		{ID: 8004, Time: 8004, RequestModelName: "not-a-google/my-model", ActualModelName: "not-a-google/my-model", ChannelId: 12},
	}
	if err := db.GetDB().WithContext(ctx).Create(&slashLogs).Error; err != nil {
		t.Fatalf("create slash logs: %v", err)
	}

	// Provider "openai" should match 8002
	found, err = RelayLogList(ctx, nil, nil, 1, 20, &model.RelayLogScope{Provider: "openai"})
	found8002 := false
	for _, l := range found {
		if l.ID == 8002 {
			found8002 = true
		}
	}
	if !found8002 {
		t.Errorf("expected provider openai to match openai/custom-agent (id 8002)")
	}

	// Provider "meta" should match 8003
	found, err = RelayLogList(ctx, nil, nil, 1, 20, &model.RelayLogScope{Provider: "meta"})
	if err != nil || len(found) != 1 || found[0].ID != 8003 {
		t.Errorf("filter provider meta: got %v (len %d), want id 8003", found, len(found))
	}

	// Provider "google" should NOT match 8004
	found, err = RelayLogList(ctx, nil, nil, 1, 20, &model.RelayLogScope{Provider: "google"})
	for _, l := range found {
		if l.ID == 8004 {
			t.Errorf("provider google erroneously matched not-a-google/my-model (id 8004)")
		}
	}

	// Unknown provider should match 0 rows
	found, err = RelayLogList(ctx, nil, nil, 1, 20, &model.RelayLogScope{Provider: "unknown-provider-xyz"})
	if err != nil || len(found) != 0 {
		t.Errorf("unknown provider should match 0 rows, got %d", len(found))
	}

	// Verify memory matcher consistency
	for _, log := range append(extendedLogs, slashLogs...) {
		for _, p := range []string{"openai", "anthropic", "google", "deepseek", "meta", "unknown-xyz"} {
			sqlScope := &model.RelayLogScope{Provider: p}
			memMatch := RelayLogProviderMatches(log, p)
			sqlFound, sqlErr := RelayLogList(ctx, nil, nil, 1, 100, sqlScope)
			if sqlErr != nil {
				t.Fatalf("SQL list error: %v", sqlErr)
			}
			foundInSQL := false
			for _, sl := range sqlFound {
				if sl.ID == log.ID {
					foundInSQL = true
					break
				}
			}
			if memMatch != foundInSQL {
				t.Errorf("log %d provider %s: memory=%v vs SQL=%v mismatch (model=%s/%s)",
					log.ID, p, memMatch, foundInSQL, log.RequestModelName, log.ActualModelName)
			}
		}
	}
}

// TestRelayLogSeverityCountsSingleQueryParity guards the one-query rewrite of
// RelayLogSeverityCounts:
//   - per-severity counts must stay identical to what the List path
//     (relayLogApplyScope + relayLogSeverityValue) would return, i.e. the
//     SUM(CASE) buckets are in lockstep with the severity WHERE branches;
//   - total must equal success+warn+error on the DB path;
//   - the in-memory path (persistence off, relayLogCache) must produce the same
//     numbers and the same total-sum invariant.
func TestRelayLogSeverityCountsSingleQueryParity(t *testing.T) {
	ctx := setupRelayLogTest(t)
	if err := SettingSetString(model.SettingKeyRelayLogKeepEnabled, "true"); err != nil {
		t.Fatalf("enable persistence: %v", err)
	}

	logs := []model.RelayLog{
		{ID: 9001, Time: 9001, RequestModelName: "s", TotalAttempts: 1},
		{ID: 9002, Time: 9002, RequestModelName: "s", TotalAttempts: 0},
		{ID: 9003, Time: 9003, RequestModelName: "w", TotalAttempts: 2},
		{ID: 9004, Time: 9004, RequestModelName: "e", Error: "boom"},
		{ID: 9005, Time: 9005, RequestModelName: "e", Error: "   ", TotalAttempts: 3}, // whitespace error → warn, not error
		{ID: 9006, Time: 9006, RequestModelName: "e", ErrorStatus: 400, TotalAttempts: 9},
		{ID: 9007, Time: 9007, RequestModelName: "s", RequestEndpoint: "chat", UserID: 7, APIKeyID: 8},
		{ID: 9008, Time: 9008, RequestModelName: "s", RequestEndpoint: "chat_v2", TotalAttempts: 2},
	}
	if err := db.GetDB().WithContext(ctx).Create(&logs).Error; err != nil {
		t.Fatalf("create logs: %v", err)
	}

	scopes := []*model.RelayLogScope{
		nil,
		{Severity: "error"},
		{Severity: "warn"},
		{Severity: "success"},
		{Endpoint: "chat"}, // family match crosses both chat rows
		{UserID: 7, APIKeyID: 8},
		{RetriedOnly: true},
		{HideModelTest: true},
	}
	for _, scope := range scopes {
		counts, err := RelayLogSeverityCounts(ctx, nil, nil, scope)
		if err != nil {
			t.Fatalf("counts(%+v): %v", scope, err)
		}
		if counts.Total != counts.Success+counts.Warn+counts.Error {
			t.Errorf("scope %+v: total=%d != success(%d)+warn(%d)+error(%d)",
				scope, counts.Total, counts.Success, counts.Warn, counts.Error)
		}
		// When the scope itself narrows to one severity, the other two buckets must
		// be empty and total must equal the scoped per-severity count — a mismatch
		// here would mean the SUM(CASE) buckets leak rows across severities.
		if scope != nil && scope.Severity != "" {
			buckets := map[string]int64{"success": counts.Success, "warn": counts.Warn, "error": counts.Error}
			for name, n := range buckets {
				if name != scope.Severity && n != 0 {
					t.Errorf("scope severity=%s: bucket %s = %d, want 0", scope.Severity, name, n)
				}
			}
			want, err := RelayLogCount(ctx, nil, nil, scope)
			if err != nil {
				t.Fatalf("count(%+v): %v", scope, err)
			}
			if counts.Total != want {
				t.Errorf("scope severity=%s: total=%d, per-severity count=%d", scope.Severity, counts.Total, want)
			}
		}
		// Parity with the per-severity COUNT path (relayLogApplyScope severity WHERE
		// branches — the same clauses RelayLogCount/List use): each bucket must equal
		// the count for that severity under the same base scope. Only evaluated on
		// scopes that do NOT already narrow by severity: re-running COUNT with
		// scope.Severity = bucket under a severity-narrowed base scope would always
		// return 0 for the other two buckets.
		for _, severity := range []string{"success", "warn", "error"} {
			if scope != nil && scope.Severity != "" {
				continue
			}
			scoped := model.RelayLogScope{}
			if scope != nil {
				scoped = *scope
			}
			scoped.Severity = severity
			rows, err := RelayLogList(ctx, nil, nil, 1, 100, &scoped)
			if err != nil {
				t.Fatalf("list(%+v): %v", &scoped, err)
			}
			// The listed rows must all classify into the requested bucket.
			for _, r := range rows {
				if relayLogSeverityValue(r) != severity {
					t.Errorf("scope %+v severity %s: list row id=%d classified %q", scope, severity, r.ID, relayLogSeverityValue(r))
				}
			}
			// And the SUM(CASE) bucket must equal the authoritative per-severity count.
			want, err := RelayLogCount(ctx, nil, nil, &scoped)
			if err != nil {
				t.Fatalf("count(%+v): %v", &scoped, err)
			}
			got := map[string]int64{"success": counts.Success, "warn": counts.Warn, "error": counts.Error}[severity]
			if got != want {
				t.Errorf("scope %+v severity %s: SUM(CASE)=%d, per-severity count=%d", scope, severity, got, want)
			}
		}
	}

	// Time-filtered counts must agree with RelayLogCount for each severity.
	start, end := int64(9003), int64(9006)
	st, et := int(start), int(end)
	for _, severity := range []string{"success", "warn", "error"} {
		scoped := model.RelayLogScope{Severity: severity}
		counts, err := RelayLogSeverityCounts(ctx, &st, &et, &scoped)
		if err != nil {
			t.Fatalf("time-filtered counts(%s): %v", severity, err)
		}
		want, err := RelayLogCount(ctx, &st, &et, &scoped)
		if err != nil {
			t.Fatalf("time-filtered count(%s): %v", severity, err)
		}
		got := map[string]int64{"success": counts.Success, "warn": counts.Warn, "error": counts.Error}[severity]
		if got != want {
			t.Errorf("time-filtered severity %s: counts=%d, RelayLogCount=%d", severity, got, want)
		}
	}

	// In-memory path (persistence off): same rows via RelayLogAdd must give the
	// same bucket sizes and the same total-sum invariant.
	if err := RelayLogClear(ctx, nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if err := SettingSetString(model.SettingKeyRelayLogKeepEnabled, "false"); err != nil {
		t.Fatalf("disable persistence: %v", err)
	}
	for _, l := range logs {
		l.ID = 0
		if err := RelayLogAdd(ctx, l); err != nil {
			t.Fatalf("add to cache: %v", err)
		}
	}
	// The in-memory path must classify the same rows into the same buckets and keep
	// the total-sum invariant.
	memCounts, err := RelayLogSeverityCounts(ctx, nil, nil, nil)
	if err != nil {
		t.Fatalf("memory counts: %v", err)
	}
	if memCounts.Total != int64(len(logs)) {
		t.Errorf("memory: total=%d, want %d", memCounts.Total, len(logs))
	}
	if memCounts.Total != memCounts.Success+memCounts.Warn+memCounts.Error {
		t.Errorf("memory: total=%d != success(%d)+warn(%d)+error(%d)",
			memCounts.Total, memCounts.Success, memCounts.Warn, memCounts.Error)
	}
	// 双路径一致性：同样的行经 flush 落库后，DB 路径必须给出完全相同的桶。
	// relayLogFlushToDB 把缓存整批搬进 DB（两条路径对行做划分，不重复计数）。
	if err := SettingSetString(model.SettingKeyRelayLogKeepEnabled, "true"); err != nil {
		t.Fatalf("re-enable persistence: %v", err)
	}
	if err := relayLogFlushToDB(ctx); err != nil {
		t.Fatalf("flush cache to db: %v", err)
	}
	dbCounts, err := RelayLogSeverityCounts(ctx, nil, nil, nil)
	if err != nil {
		t.Fatalf("db counts: %v", err)
	}
	if dbCounts != memCounts {
		t.Errorf("DB path %+v != memory path %+v", dbCounts, memCounts)
	}
	if dbCounts != (model.RelayLogSeverityCounts{Total: 8, Success: 3, Warn: 3, Error: 2}) {
		t.Errorf("db counts = %+v, want total 8 (success 3 / warn 3 / error 2)", dbCounts)
	}
}
