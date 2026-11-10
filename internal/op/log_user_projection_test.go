package op

import (
	"bytes"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/db/migrate"
	"github.com/zhizhishu/octopus/internal/model"
)

func TestRelayLogUserProjectionSummary(t *testing.T) {
	for _, requested := range []string{"A", ""} {
		row := model.RelayLog{RequestModelName: requested, ActualModelName: "B", UpstreamResponseModel: "C", BillingModel: "B", Cost: 0.37, InputTokens: 41, BaseInputPrice: 2, BaseOutputPrice: 3, BillingProfileName: "snapshot"}
		before := row
		summary := RelayLogUserSummary(row)
		if summary.RequestModelName != requested || summary.ActualModelName != requested || summary.BillingModel != requested {
			t.Fatalf("requested %q: unexpected identity %+v", requested, summary)
		}
		if summary.Cost != row.Cost || summary.InputTokens != row.InputTokens || summary.BaseInputPrice != row.BaseInputPrice || summary.BaseOutputPrice != row.BaseOutputPrice || !reflect.DeepEqual(row, before) {
			t.Fatal("projection changed billing values or source snapshot")
		}
		admin := RelayLogExportSummary(row, false)
		if admin.ActualModelName != "B" || admin.BillingModel != "B" {
			t.Fatalf("admin light export lost actual identity: %+v", admin)
		}
	}
	user, _ := json.Marshal(model.RelayLogScope{UserID: 1, UserProjection: true})
	admin, _ := json.Marshal(model.RelayLogScope{UserID: 1})
	if bytes.Equal(user, admin) {
		t.Fatal("serialized storage scope cache keys overlap")
	}
}

func TestRelayLogUserProjectionPaths(t *testing.T) {
	ctx := setupRelayLogTest(t)
	testDB := db.GetDB()
	if err := SettingSetString(model.SettingKeyRelayLogKeepEnabled, "true"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		relayLogCacheLock.Lock()
		relayLogCache = nil
		relayLogCacheLock.Unlock()
	})
	// Migrations are registered once per process, so later test DBs need their
	// own migration receipt and backfill marker rather than an UPDATE of no rows.
	if err := testDB.AutoMigrate(&migrate.MigrationRecord{}, &migrate.RelayLogSearchBackfillState{}); err != nil {
		t.Fatal(err)
	}
	if err := testDB.Save(&migrate.MigrationRecord{Version: relayLogSearchIndexMigrationVersion, Status: migrate.MigrationRecordStatusSuccess}).Error; err != nil {
		t.Fatal(err)
	}
	yes, no := true, false
	rows := []model.RelayLog{
		{ID: 8101, Time: 8101, UserID: 1, RequestModelName: "A", ActualModelName: "B", UpstreamResponseModel: "C", BillingModel: "B", ChannelId: 991, ChannelName: "hidden-channel", UserName: "hidden-user", Error: "hidden-error", UpstreamModelMismatch: &yes, Cost: 0.37, RequestUserAgent: "ua-test"},
		{ID: 8102, Time: 8102, UserID: 1, RequestModelName: "", ActualModelName: "B", UpstreamResponseModel: "C", BillingModel: "B", UpstreamModelMismatch: &no},
		{ID: 8103, Time: 8103, UserID: 2, RequestModelName: "A", ActualModelName: "B"},
		{ID: 8104, Time: 8104, UserID: 1, RequestModelName: "openai/A", ActualModelName: "anthropic/B", UpstreamResponseModel: "C"},
	}
	if err := db.GetDB().Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		scope model.RelayLogScope
		want  []int64
	}{
		{"all", model.RelayLogScope{}, []int64{8101, 8102, 8104}},
		{"request", model.RelayLogScope{Model: "A"}, []int64{8101}},
		{"actual", model.RelayLogScope{Model: "B"}, nil},
		{"search-request", model.RelayLogScope{Search: "A"}, []int64{8101, 8104}},
		{"search-actual", model.RelayLogScope{Search: "B"}, nil},
		{"search-echo", model.RelayLogScope{Search: "C"}, nil},
		{"channel", model.RelayLogScope{Search: "hidden-channel"}, nil},
		{"channel-id", model.RelayLogScope{Search: "991"}, nil},
		{"raw-error", model.RelayLogScope{Search: "hidden-error"}, nil},
		{"user-name", model.RelayLogScope{Search: "hidden-user"}, nil},
		{"id", model.RelayLogScope{Search: "8101"}, []int64{8101}},
		{"provider-request", model.RelayLogScope{Provider: "openai"}, []int64{8104}},
		{"provider-actual", model.RelayLogScope{Provider: "anthropic"}, nil},
		{"mismatch-true", model.RelayLogScope{UpstreamModelMismatch: &yes}, []int64{8101, 8102, 8104}},
		{"mismatch-false", model.RelayLogScope{UpstreamModelMismatch: &no}, []int64{8101, 8102, 8104}},
	}
	ids := func(logs []model.RelayLog) []int64 {
		var out []int64
		for _, row := range logs {
			out = append(out, row.ID)
		}
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
		return out
	}
	for _, phase := range []string{"indexed", "partial", "missing", "cache"} {
		t.Run(phase, func(t *testing.T) {
			if db.GetDB() != testDB {
				t.Fatal("projection test database was replaced")
			}
			switch phase {
			case "indexed":
				if err := relayLogSearchIndexRebuild(ctx); err != nil {
					t.Fatal(err)
				}
				if err := testDB.Save(&migrate.RelayLogSearchBackfillState{ID: 1, Complete: true, Cursor: 8104}).Error; err != nil {
					t.Fatal(err)
				}
				if complete, cursor := relayLogSearchIndexState(ctx); !complete || cursor != 8104 {
					t.Fatalf("complete-index branch was not activated: complete=%v cursor=%d", complete, cursor)
				}
			case "partial":
				if err := testDB.Save(&migrate.RelayLogSearchBackfillState{ID: 1, Complete: false, Cursor: 8101}).Error; err != nil {
					t.Fatal(err)
				}
				if complete, cursor := relayLogSearchIndexState(ctx); complete || cursor != 8101 {
					t.Fatalf("partial-index branch was not activated: complete=%v cursor=%d", complete, cursor)
				}
				if err := db.GetDB().Where("id > ?", 8101).Delete(&model.RelayLogSearchIndex{}).Error; err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := db.GetDB().Migrator().DropTable(&model.RelayLogSearchIndex{}); err != nil {
					t.Fatal(err)
				}
			case "cache":
				if err := SettingSetString(model.SettingKeyRelayLogKeepEnabled, "false"); err != nil {
					t.Fatal(err)
				}
				relayLogCacheLock.Lock()
				relayLogCache = append([]model.RelayLog(nil), rows...)
				relayLogCacheLock.Unlock()
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					scope := tc.scope
					scope.UserID = 1
					scope.UserProjection = true
					var matched []model.RelayLog
					for _, row := range rows {
						if RelayLogMatchesScope(row, &scope) {
							matched = append(matched, row)
						}
					}
					if got := ids(matched); !reflect.DeepEqual(got, tc.want) {
						t.Fatalf("memory got %v want %v", got, tc.want)
					}
					for _, size := range []int{20, relayLogTwoPhaseMaxIDs + 1} {
						logs, err := RelayLogList(ctx, nil, nil, 1, size, &scope)
						if err != nil {
							t.Fatal(err)
						}
						if got := ids(logs); !reflect.DeepEqual(got, tc.want) {
							t.Fatalf("list size %d got %v want %v", size, got, tc.want)
						}
					}
					counts, err := RelayLogSeverityCounts(ctx, nil, nil, &scope)
					if err != nil {
						t.Fatal(err)
					}
					if counts.Total != int64(len(tc.want)) {
						t.Fatalf("count %d want %d", counts.Total, len(tc.want))
					}
					replay, err := RelayLogListSinceRange(ctx, 8000, 20, &scope, nil, nil)
					if err != nil {
						t.Fatal(err)
					}
					if got := ids(replay); !reflect.DeepEqual(got, tc.want) {
						t.Fatalf("replay got %v want %v", got, tc.want)
					}
				})
			}
			adminScope := model.RelayLogScope{UserID: 1, Search: "hidden-channel"}
			adminRows, err := RelayLogList(ctx, nil, nil, 1, 20, &adminScope)
			if err != nil {
				t.Fatal(err)
			}
			if len(adminRows) != 1 || adminRows[0].ActualModelName != "B" || adminRows[0].UpstreamResponseModel != "C" {
				t.Fatalf("admin lost full data: %+v", adminRows)
			}
			for _, userProjection := range []bool{false, true} {
				var out bytes.Buffer
				scope := model.RelayLogScope{UserID: 1, Model: "A", UserProjection: userProjection}
				if err := RelayLogStreamExportNDJSON(ctx, &out, nil, nil, 10, &scope, true); err != nil {
					t.Fatal(err)
				}
				lines := strings.Split(strings.TrimSpace(out.String()), "\n")
				if len(lines) != 2 {
					t.Fatalf("export rows: %s", out.String())
				}
				var row model.RelayLogUserSummary
				if err := json.Unmarshal([]byte(lines[1]), &row); err != nil {
					t.Fatal(err)
				}
				want := "B"
				if userProjection {
					want = "A"
				}
				if row.ActualModelName != want || row.BillingModel != want || row.Cost != 0.37 {
					t.Fatalf("export projection=%v: %+v", userProjection, row)
				}
			}
		})
	}
	var stored model.RelayLog
	if err := db.GetDB().First(&stored, 8101).Error; err != nil {
		t.Fatal(err)
	}
	if stored.ActualModelName != "B" || stored.BillingModel != "B" || stored.UpstreamResponseModel != "C" || stored.Cost != 0.37 {
		t.Fatalf("stored source changed: %+v", stored)
	}
}
