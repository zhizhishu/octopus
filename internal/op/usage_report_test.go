package op

import (
	"bytes"
	"encoding/csv"
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
)

func TestUsageReportFixtures(t *testing.T) {
	ctx := setupRelayLogTest(t)
	loc, _ := time.LoadLocation("Asia/Shanghai")
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, loc)
	rng := mustStatsRange(t, "2026-06-13", "2026-06-14", now)
	logs := []model.RelayLog{
		{ID: 1, Time: rng.Start, UserID: 10, UserName: "alice", APIKeyID: 101, RequestAPIKeyName: "alice-demo", RequestModelName: "m1", Cost: 1.25, InputTokens: 100, OutputTokens: 20, CacheHitTokens: 60, CacheWriteTokens: 10, CacheInputTokens: 100},
		{ID: 2, Time: rng.Start + 86400, UserID: 10, UserName: "alice", APIKeyID: 102, RequestAPIKeyName: "alice-ci", RequestModelName: "m2", Cost: 2.5, Error: "fixture failure", ErrorStatus: 502, Severity: model.RelayLogSeverityError, InputTokens: 40},
		{ID: 3, Time: rng.Start + 86400 - 1, UserID: 20, UserName: "bob", APIKeyID: 201, RequestAPIKeyName: "bob-demo", ActualModelName: "m1", Cost: 4},
		{ID: 4, Time: rng.End - 1, UserID: 10, UserName: "alice", APIKeyID: 101, RequestAPIKeyName: "alice-demo", RequestModelName: "m1", Cost: 0, FinalInputCost: 99},
		{ID: 5, Time: rng.Start + 10, UserID: 10, RequestModelName: "ignored", Cost: 1000, ErrorStrategy: "STATS_COUNTED=false"},
		{ID: 6, Time: rng.Start - 1, UserID: 10, Cost: 1000},
		{ID: 7, Time: rng.End, UserID: 10, Cost: 1000},
	}
	if err := db.GetDB().Create(&logs).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.GetDB().Create(&[]model.APIKey{{ID: 101, UserID: 10, Name: "alice-demo", APIKey: "fake-a"}, {ID: 201, UserID: 20, Name: "bob-demo", APIKey: "fake-b"}}).Error; err != nil {
		t.Fatal(err)
	}
	admin := model.User{ID: 1, Role: model.UserRoleAdmin}
	for _, group := range []string{"user", "apikey", "model", "day"} {
		t.Run(group, func(t *testing.T) {
			r, err := UsageReportGet(ctx, admin, rng, UsageReportFilter{Group: group})
			if err != nil {
				t.Fatal(err)
			}
			wantRows := 2
			if group == "apikey" {
				wantRows = 3
			}
			if len(r.Rows) != wantRows || r.Summary.TotalCost != 7.75 || r.Summary.RequestSuccess != 3 || r.Summary.RequestFailed != 1 || r.Summary.CacheHitToken != 60 || r.Summary.CacheWriteToken != 10 {
				t.Fatalf("report: %+v", r)
			}
			var total model.StatsUsage
			for _, row := range r.Rows {
				total.Add(row.Usage)
				var sub model.StatsUsage
				for _, m := range row.Models {
					sub.Add(m.Usage)
				}
				if sub != row.Usage {
					t.Fatalf("subrows: %+v", row)
				}
			}
			if total != r.Summary {
				t.Fatalf("summary: %+v vs %+v", total, r.Summary)
			}
			if r.Daily[0].Date != "2026-06-13" || r.Daily[0].Cost != 5.25 || r.Daily[1].Cost != 2.5 || r.Daily[0].Models["m1"] != 5.25 {
				t.Fatalf("timezone boundary: %+v", r.Daily)
			}
			var b bytes.Buffer
			if err := WriteUsageReportCSV(&b, r); err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(b.String(), "\xef\xbb\xbf") {
				t.Fatal("missing BOM")
			}
			csvRows, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(b.String(), "\xef\xbb\xbf"))).ReadAll()
			if err != nil {
				t.Fatal(err)
			}
			for _, level := range []string{"group", "model"} {
				cost := 0.0
				requests := int64(0)
				for _, row := range csvRows[1:] {
					if row[0] != level {
						continue
					}
					v, _ := strconv.ParseFloat(row[11], 64)
					cost += v
					s, _ := strconv.ParseInt(row[4], 10, 64)
					f, _ := strconv.ParseInt(row[5], 10, 64)
					requests += s + f
				}
				if math.Abs(cost-r.Summary.TotalCost) > 1e-9 || requests != r.Summary.Requests() {
					t.Fatalf("CSV %s cost=%v requests=%v", level, cost, requests)
				}
			}
		})
	}
	alice := model.User{ID: 10, Role: model.UserRoleUser}
	r, err := UsageReportGet(ctx, alice, rng, UsageReportFilter{Group: "user", UserID: 20})
	if err != nil || len(r.Rows) != 1 || r.Rows[0].Name != "alice" || r.Summary.TotalCost != 3.75 {
		t.Fatalf("scope: %+v %v", r, err)
	}
	_, err = UsageReportGet(ctx, alice, rng, UsageReportFilter{Group: "apikey", APIKeyID: 201})
	if !errors.Is(err, ErrUsageReportForbidden) {
		t.Fatalf("foreign key: %v", err)
	}
	r, err = UsageReportGet(ctx, alice, rng, UsageReportFilter{Group: "apikey", APIKeyID: 101})
	if err != nil || r.Summary.TotalCost != 1.25 {
		t.Fatalf("own key: %+v %v", r, err)
	}
	r, err = UsageReportGet(ctx, admin, rng, UsageReportFilter{Group: "model", Model: "m2", UserID: 10})
	if err != nil || r.Summary.TotalCost != 2.5 {
		t.Fatalf("filters: %+v %v", r, err)
	}
	r, err = UsageReportGet(ctx, admin, rng, UsageReportFilter{Group: "day", Model: "absent"})
	if err != nil || len(r.Rows) != 0 || r.Summary.Requests() != 0 || len(r.Daily) != 2 {
		t.Fatalf("empty: %+v %v", r, err)
	}
	_, err = UsageReportGet(ctx, admin, rng, UsageReportFilter{Group: "invalid"})
	if err == nil {
		t.Fatal("invalid group accepted")
	}
	if err := WriteUsageReportCSV(brokenUsageWriter{}, r); err == nil {
		t.Fatal("writer error ignored")
	}
	var b bytes.Buffer
	_ = WriteUsageReportCSV(&b, model.UsageReport{Rows: []model.UsageReportRow{{ID: "1", Name: "=FAKE()"}}})
	if !strings.Contains(b.String(), "'=FAKE()") {
		t.Fatal("CSV formula not escaped")
	}
}

type brokenUsageWriter struct{}

func (brokenUsageWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestUsageReportRange(t *testing.T) {
	for preset, days := range map[string]int{"today": 1, "7d": 7, "30d": 30} {
		r, err := ParseUsageReportRange(preset, "", "")
		if err != nil || r.Days != days {
			t.Fatalf("%s: %+v %v", preset, r, err)
		}
	}
	for _, tc := range [][3]string{{"invalid", "", ""}, {"custom", "", ""}, {"custom", "2026-01-01", "2026-06-01"}} {
		if _, err := ParseUsageReportRange(tc[0], tc[1], tc[2]); err == nil {
			t.Fatalf("accepted %v", tc)
		}
	}
}
