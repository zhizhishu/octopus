package op

import (
	"strings"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
)

func mustStatsRange(t *testing.T, from, to string, now time.Time) StatsRange {
	t.Helper()
	rng, err := parseStatsRangeAt(from, to, now)
	if err != nil {
		t.Fatalf("parse %s..%s: %v", from, to, err)
	}
	return rng
}

func TestParseStatsRangeBounds(t *testing.T) {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 30, 9, 56, 0, 0, loc)

	rng := mustStatsRange(t, "2026-10-24", "2026-10-30", now)
	if rng.Days != 7 || rng.Start != time.Date(2026, 10, 24, 0, 0, 0, 0, loc).Unix() || rng.End != time.Date(2026, 10, 31, 0, 0, 0, 0, loc).Unix() {
		t.Fatalf("7 days: %+v", rng)
	}
	if got := mustStatsRange(t, "20261030", "20261030", now); got.Days != 1 || got.FromDate != "2026-10-30" {
		t.Fatalf("compact: %+v", got)
	}
	// 90 天正好到上限；91 天拒。
	if got := mustStatsRange(t, "2026-08-02", "2026-10-30", now); got.Days != 90 {
		t.Fatalf("90 days: %d", got.Days)
	}
	for _, tc := range []struct{ from, to, want string }{
		{"2026-08-01", "2026-10-30", "最多看 90 天"},
		{"2026-10-30", "2026-10-31", "不能晚于今天"},
		{"2026-10-30", "2026-10-29", "早于开始日期"},
		{"", "2026-10-29", "都要填"},
		{"2026-13-01", "2026-10-29", "from 格式"},
		{"2026-10-01", "1730000000", "to 格式"},
		{"1999-12-31", "2000-01-01", "太早"},
	} {
		_, err := parseStatsRangeAt(tc.from, tc.to, now)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s..%s: err=%v, want %q", tc.from, tc.to, err, tc.want)
		}
		if _, ok := err.(*StatsRangeError); !ok {
			t.Errorf("%s..%s: error type %T, want *StatsRangeError (handler maps it to 400)", tc.from, tc.to, err)
		}
	}

	// 一个跨夏令时的 90 天：天数按日历算，边界按本地零点。
	spring := mustStatsRange(t, "2026-02-01", "2026-03-31", time.Date(2026, 4, 1, 12, 0, 0, 0, loc))
	if spring.Days != 59 {
		t.Fatalf("spring days: %d", spring.Days)
	}
	bounds := spring.dayBounds()
	if len(bounds) != 60 || bounds[59] != spring.End {
		t.Fatalf("day bounds: %d", len(bounds))
	}
	for i := 1; i < len(bounds); i++ {
		if time.Unix(bounds[i], 0).In(loc).Hour() != 0 {
			t.Fatalf("day bound %d not at local midnight", i)
		}
	}
}

func TestStatsHourBoundsFollowLocalClock(t *testing.T) {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	normal := mustStatsRange(t, "2026-10-29", "2026-10-29", time.Date(2026, 10, 30, 1, 0, 0, 0, loc))
	if b := normal.hourBounds(); len(b) != 25 || b[0] != normal.Start || b[24] != normal.End {
		t.Fatalf("normal day: %d bounds", len(b))
	}
	// 2026-03-08 只有 23 小时，2026-11-01 有 25 小时（1 点那格是两小时）。
	short := mustStatsRange(t, "2026-03-08", "2026-03-08", time.Date(2026, 3, 9, 1, 0, 0, 0, loc))
	if b := short.hourBounds(); len(b) != 24 || b[len(b)-1] != short.End {
		t.Fatalf("spring-forward day: %d bounds", len(b))
	}
	long := mustStatsRange(t, "2026-11-01", "2026-11-01", time.Date(2026, 11, 2, 1, 0, 0, 0, loc))
	b := long.hourBounds()
	if len(b) != 25 || b[len(b)-1] != long.End || long.End-long.Start != 25*3600 {
		t.Fatalf("fall-back day: %d bounds, span %d", len(b), long.End-long.Start)
	}
	for i := 1; i < len(b); i++ {
		if b[i] <= b[i-1] {
			t.Fatalf("bounds not increasing at %d", i)
		}
	}
	// 每一秒都落进某一格，格子号和 SQL CASE 的结果一样（最后一格是 ELSE）。
	for ts := long.Start; ts < long.End; ts += 600 {
		i := statsBucketIndex(ts, b)
		if i < 0 || i >= len(b)-1 || ts < b[i] || ts >= b[i+1] {
			t.Fatalf("ts %d -> bucket %d", ts, i)
		}
	}
}

// 生产库 18GB：每个维度的聚合都必须走 time 索引的范围查找，不能是 SCAN relay_logs。
func TestStatsAggregateUsesTimeIndex(t *testing.T) {
	ctx := setupRelayLogTest(t)
	hint := statsTimeIndexHint()
	if hint != " INDEXED BY "+statsTimeIndexName {
		t.Fatalf("expected the INDEXED BY hint on sqlite, got %q", hint)
	}
	now := time.Now()
	day := mustStatsRange(t, now.Format("2006-01-02"), now.Format("2006-01-02"), now)
	week := mustStatsRange(t, now.AddDate(0, 0, -6).Format("2006-01-02"), now.Format("2006-01-02"), now)

	specs := map[string]statsAggSpec{"user-day": statsUserDaySpec(week)}
	for _, by := range []string{StatsByModel, StatsByChannel, StatsByPlan, StatsByUser, StatsByAPIKey, StatsByHour} {
		rng := week
		if by == StatsByHour {
			rng = day
		}
		spec, err := statsAggSpecFor(by, rng)
		if err != nil {
			t.Fatalf("%s: %v", by, err)
		}
		specs[by] = spec
	}
	for name, spec := range specs {
		rng := week
		if name == StatsByHour {
			rng = day
		}
		query, args := statsAggSQL(spec, rng, hint)
		var plan []struct {
			Detail string `gorm:"column:detail"`
		}
		if err := db.GetDB().WithContext(ctx).Raw("EXPLAIN QUERY PLAN "+query, args...).Scan(&plan).Error; err != nil {
			t.Fatalf("%s: explain: %v", name, err)
		}
		var details []string
		for _, row := range plan {
			details = append(details, row.Detail)
		}
		joined := strings.Join(details, " | ")
		if !strings.Contains(joined, "USING INDEX "+statsTimeIndexName+" (time>? AND time<?)") || strings.Contains(joined, "SCAN relay_logs") {
			t.Errorf("%s: query plan is not a bounded time-index search: %s", name, joined)
		}
	}
}

// 索引不在（比如别的库名不同）时不加提示，查询照样能跑，不直接报错。
func TestStatsAggregateWithoutTimeIndexStillWorks(t *testing.T) {
	ctx := setupRelayLogTest(t)
	if err := db.GetDB().Exec("DROP INDEX " + statsTimeIndexName).Error; err != nil {
		t.Fatalf("drop index: %v", err)
	}
	if hint := statsTimeIndexHint(); hint != "" {
		t.Fatalf("hint should be empty without the index, got %q", hint)
	}
	now := time.Now()
	if err := db.GetDB().Create(&model.RelayLog{ID: 1, Time: now.Unix(), RequestModelName: "m", Cost: 1}).Error; err != nil {
		t.Fatal(err)
	}
	rng := mustStatsRange(t, now.Format("2006-01-02"), now.Format("2006-01-02"), now)
	got, err := StatsBreakdownGet(ctx, rng, StatsByModel, 0)
	if err != nil || got.Total.TotalCost != 1 {
		t.Fatalf("without index: %+v %v", got, err)
	}
}

// flush 进行中时同一条日志既在库里、又还在内存队列里：只能算一次（与 model_rank 同一去重）。
func TestStatsAggregateDedupesPendingAgainstDB(t *testing.T) {
	ctx := setupRelayLogTest(t)
	now := time.Now()
	stored := model.RelayLog{ID: 501, Time: now.Unix(), UserID: 7, RequestModelName: "m", Cost: 1}
	if err := db.GetDB().Create(&stored).Error; err != nil {
		t.Fatal(err)
	}
	pendingOnly := model.RelayLog{ID: 502, Time: now.Unix(), UserID: 7, RequestModelName: "m", Cost: 0.5, Severity: model.RelayLogSeveritySuccess}
	outside := model.RelayLog{ID: 503, Time: now.AddDate(0, 0, -2).Unix(), UserID: 7, RequestModelName: "m", Cost: 8, Severity: model.RelayLogSeveritySuccess}
	notCounted := model.RelayLog{ID: 504, Time: now.Unix(), UserID: 7, RequestModelName: "m", Cost: 16, ErrorStrategy: model.RelayLogErrorStrategyLocalValidation}
	relayLogCacheLock.Lock()
	relayLogCache = append(relayLogCache, stored, pendingOnly, outside, notCounted)
	relayLogCacheLock.Unlock()
	t.Cleanup(func() {
		relayLogCacheLock.Lock()
		relayLogCache = relayLogCache[:0]
		relayLogCacheLock.Unlock()
	})

	rng := mustStatsRange(t, now.Format("2006-01-02"), now.Format("2006-01-02"), now)
	got, err := StatsBreakdownGet(ctx, rng, StatsByUser, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Total.TotalCost != 1.5 || got.Total.Requests() != 2 || len(got.Items) != 1 || got.Items[0].ID != 7 {
		t.Fatalf("dedupe: %+v", got)
	}
}
