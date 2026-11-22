package op

import (
	"context"
	"encoding/csv"
	"errors"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
)

var ErrUsageReportForbidden = errors.New("API key does not belong to current user")

type UsageReportFilter struct {
	Group    string
	UserID   int
	APIKeyID int
	Model    string
}

func ParseUsageReportRange(preset, start, end string) (StatsRange, error) {
	now := statsNow()
	days := 1
	switch preset {
	case "", "today":
	case "7d":
		days = 7
	case "30d":
		days = 30
	case "custom":
		return parseStatsRangeAt(start, end, now)
	default:
		return StatsRange{}, statsRangeErr("range must be today / 7d / 30d / custom")
	}
	return parseStatsRangeAt(now.AddDate(0, 0, 1-days).Format("2006-01-02"), now.Format("2006-01-02"), now)
}

// UsageReportGet reads only persisted relay logs; cost is the recorded charge, including zero.
func UsageReportGet(ctx context.Context, actor model.User, rng StatsRange, f UsageReportFilter) (model.UsageReport, error) {
	out := model.UsageReport{Start: rng.FromDate, End: rng.ToDate, Group: f.Group, Rows: []model.UsageReportRow{}, Daily: []model.UsageReportDay{}}
	if !actor.IsAdmin() {
		if actor.ID <= 0 {
			return out, ErrUsageReportForbidden
		}
		f.UserID = actor.ID
		if f.APIKeyID > 0 {
			var count int64
			if err := db.GetDB().WithContext(ctx).Model(&model.APIKey{}).Where("id = ? AND user_id = ?", f.APIKeyID, actor.ID).Count(&count).Error; err != nil {
				return out, err
			}
			if count != 1 {
				return out, ErrUsageReportForbidden
			}
		}
	}
	var spec statsAggSpec
	var err error
	if f.Group == "day" {
		expr, args := statsBucketSQL(rng.dayBounds())
		spec = statsAggSpec{k1: expr, k1Args: args, name: "''", owner: "0"}
	} else if f.Group == StatsByUser || f.Group == StatsByAPIKey || f.Group == StatsByModel {
		spec, err = statsAggSpecFor(f.Group, rng)
		if err != nil {
			return out, err
		}
	} else {
		return out, statsRangeErr("group must be user / apikey / model / day")
	}
	spec.k2 = statsModelKeySQL
	if f.UserID > 0 {
		spec.extraWhere += " AND user_id = ?"
		spec.extraArgs = append(spec.extraArgs, f.UserID)
	}
	if f.APIKeyID > 0 {
		spec.extraWhere += " AND api_key_id = ?"
		spec.extraArgs = append(spec.extraArgs, f.APIKeyID)
	}
	if f.Model != "" {
		spec.extraWhere += " AND " + statsModelKeySQL + " = ?"
		spec.extraArgs = append(spec.extraArgs, f.Model)
	}
	bucket, bucketArgs := statsBucketSQL(rng.dayBounds())
	query, args := statsAggSQL(spec, rng, statsTimeIndexHint())
	query = strings.Replace(query, "SELECT ", "SELECT "+bucket+" AS day_bucket, ", 1) + ", day_bucket"
	query = strings.Replace(query, statsCostSQL, "COALESCE(cost,0)", 1)
	query = strings.Replace(query, "COALESCE(error_strategy,'') NOT LIKE", "LOWER(COALESCE(error_strategy,'')) NOT LIKE", 1)
	args = append(bucketArgs, args...)
	select {
	case statsBreakdownSem <- struct{}{}:
		defer func() { <-statsBreakdownSem }()
	case <-ctx.Done():
		return out, ctx.Err()
	}
	type UsageAggregate statsAggRow
	var rows []struct {
		UsageAggregate
		DayBucket int
	}
	if err := db.GetDB().WithContext(ctx).Raw(query, args...).Scan(&rows).Error; err != nil {
		return out, err
	}
	dates := rng.dates()
	for _, date := range dates {
		out.Daily = append(out.Daily, model.UsageReportDay{Date: date, Models: map[string]float64{}})
	}
	groups := map[string]*model.UsageReportRow{}
	for _, r := range rows {
		u := model.StatsUsage{RequestSuccess: r.Requests - r.Failed, RequestFailed: r.Failed, InputToken: r.InputToken, OutputToken: r.OutputToken, CacheHitToken: r.CacheHitToken, CacheWriteToken: r.CacheWriteToken, CacheInputToken: r.CacheInputToken, TotalCost: r.Cost, WaitTime: r.WaitTime}
		id, name := r.K1, r.Name
		if f.Group == "day" {
			id = dates[r.DayBucket]
			name = id
		}
		if f.Group == "model" || name == "" {
			name = id
		}
		g := groups[id]
		if g == nil {
			g = &model.UsageReportRow{ID: id, Name: name, Models: []model.UsageReportRow{}}
			groups[id] = g
		}
		g.Usage.Add(u)
		found := false
		for i := range g.Models {
			if g.Models[i].ID == r.K2 {
				g.Models[i].Usage.Add(u)
				found = true
				break
			}
		}
		if !found {
			g.Models = append(g.Models, model.UsageReportRow{ID: r.K2, Name: r.K2, Usage: u, Models: []model.UsageReportRow{}})
		}
		out.Summary.Add(u)
		out.Daily[r.DayBucket].Cost += r.Cost
		out.Daily[r.DayBucket].Models[r.K2] += r.Cost
	}
	for _, g := range groups {
		sort.Slice(g.Models, func(i, j int) bool { return g.Models[i].ID < g.Models[j].ID })
		out.Rows = append(out.Rows, *g)
	}
	sort.Slice(out.Rows, func(i, j int) bool {
		if f.Group == "day" {
			return out.Rows[i].ID < out.Rows[j].ID
		}
		if out.Rows[i].Usage.TotalCost == out.Rows[j].Usage.TotalCost {
			return out.Rows[i].ID < out.Rows[j].ID
		}
		return out.Rows[i].Usage.TotalCost > out.Rows[j].Usage.TotalCost
	})
	return out, nil
}

func WriteUsageReportCSV(w io.Writer, report model.UsageReport) error {
	if _, err := io.WriteString(w, "\xef\xbb\xbf"); err != nil {
		return err
	}
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{"level", "id", "name", "model", "request_success", "request_failed", "input_token", "output_token", "cache_hit_token", "cache_write_token", "cache_input_token", "total_cost", "wait_time"}); err != nil {
		return err
	}
	safe := func(s string) string {
		trimmed := strings.TrimLeft(s, " \t\r\n")
		if trimmed != "" && strings.ContainsRune("=+-@", rune(trimmed[0])) {
			return "'" + s
		}
		return s
	}
	write := func(level, id, name, m string, u model.StatsUsage) error {
		return cw.Write([]string{level, safe(id), safe(name), safe(m), strconv.FormatInt(u.RequestSuccess, 10), strconv.FormatInt(u.RequestFailed, 10), strconv.FormatInt(u.InputToken, 10), strconv.FormatInt(u.OutputToken, 10), strconv.FormatInt(u.CacheHitToken, 10), strconv.FormatInt(u.CacheWriteToken, 10), strconv.FormatInt(u.CacheInputToken, 10), strconv.FormatFloat(u.TotalCost, 'g', -1, 64), strconv.FormatInt(u.WaitTime, 10)})
	}
	for _, row := range report.Rows {
		if err := write("group", row.ID, row.Name, "", row.Usage); err != nil {
			return err
		}
		for _, m := range row.Models {
			if err := write("model", row.ID, row.Name, m.Name, m.Usage); err != nil {
				return err
			}
		}
	}
	cw.Flush()
	return cw.Error()
}
