package op

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
)

// 仪表盘（管理员）的按时间段拆分统计。
//
// 做法照 model_rank.go：数据来自请求日志，库里的行 + 还没落库的内存队列合在一起、按 id
// 去重；模型名、费用、成功/失败的口径与模型排行一致。区别只有一个——这里**必须带时间段**：
//
//   - 起止都是统计时区（stats_timezone）里的日期，最多 StatsRangeMaxDays 天，结束日不能晚于今天；
//   - 查询只走 `time >= ? AND time < ?`，在 SQLite 上用 INDEXED BY 钉死现有的
//     idx_relay_logs_time，不让规划器为了省一次排序改走 user_id / channel 之类的索引
//     去扫全表（生产库 18GB）；
//   - 聚合在 SQL 里做（GROUP BY），每个分组只回一行，内存占用与日志条数无关。
//
// 日志被清理（保留天数 / 容量上限）过的日子没有原料，算不出来；返回里带 oldest_log_time，
// 页面据此提示。被标成 stats_counted=false 的行（模型测试、本地校验拦下的空请求）
// 不计入，和主页的全站统计一致。

const (
	// StatsRangeMaxDays 是一次最多能看的天数（含首尾）。
	StatsRangeMaxDays = 90

	statsBreakdownDefaultLimit = 50
	statsBreakdownMaxLimit     = 200
	statsUserTrendDefaultTop   = 5
	statsUserTrendMaxTop       = 12

	// statsTimeIndexName 是 RelayLog.Time 上 `gorm:"index"` 生成的单列索引。
	statsTimeIndexName = "idx_relay_logs_time"
	// statsNotCountedMarker 出现在 error_strategy 里的行不进统计（模型测试、本地校验）。
	statsNotCountedMarker = "stats_counted=false"
	// statsPendingIDChunk 是去重时一次 `id IN (...)` 的上限，远低于 SQLite 绑定参数上限。
	statsPendingIDChunk = 500
)

// 维度。
const (
	StatsByModel   = "model"
	StatsByChannel = "channel"
	StatsByPlan    = "plan"
	StatsByUser    = "user"
	StatsByAPIKey  = "apikey"
	StatsByHour    = "hour"
)

// StatsRangeError 是调用方传错了参数（时间段 / 维度），handler 回 400。
type StatsRangeError struct{ Msg string }

func (e *StatsRangeError) Error() string { return e.Msg }

func statsRangeErr(format string, args ...any) error {
	return &StatsRangeError{Msg: fmt.Sprintf(format, args...)}
}

// StatsRange 是解析好的时间段：[Start, End) unix 秒，按统计时区的整天切。
type StatsRange struct {
	FromDate string
	ToDate   string
	Start    int64
	End      int64
	Days     int
	loc      *time.Location
	first    time.Time
}

// ParseStatsRange 解析 from/to（统计时区里的日期，含首尾；2006-01-02 或 20060102）。
func ParseStatsRange(fromRaw, toRaw string) (StatsRange, error) {
	return parseStatsRangeAt(fromRaw, toRaw, statsNow())
}

func parseStatsDate(raw string, loc *time.Location) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	for _, layout := range []string{"2006-01-02", "20060102"} {
		if t, err := time.ParseInLocation(layout, raw, loc); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func parseStatsRangeAt(fromRaw, toRaw string, now time.Time) (StatsRange, error) {
	loc := now.Location()
	if strings.TrimSpace(fromRaw) == "" || strings.TrimSpace(toRaw) == "" {
		return StatsRange{}, statsRangeErr("from 和 to 都要填，格式 2006-01-02")
	}
	from, ok := parseStatsDate(fromRaw, loc)
	if !ok {
		return StatsRange{}, statsRangeErr("from 格式不对，要写成 2006-01-02")
	}
	to, ok := parseStatsDate(toRaw, loc)
	if !ok {
		return StatsRange{}, statsRangeErr("to 格式不对，要写成 2006-01-02")
	}
	if from.Year() < 2000 {
		return StatsRange{}, statsRangeErr("开始日期太早")
	}
	if to.Before(from) {
		return StatsRange{}, statsRangeErr("结束日期早于开始日期")
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	if to.After(today) {
		return StatsRange{}, statsRangeErr("结束日期不能晚于今天")
	}
	days := 0
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		days++
		if days > StatsRangeMaxDays {
			return StatsRange{}, statsRangeErr("一次最多看 %d 天", StatsRangeMaxDays)
		}
	}
	return StatsRange{
		FromDate: from.Format("2006-01-02"),
		ToDate:   to.Format("2006-01-02"),
		Start:    from.Unix(),
		End:      to.AddDate(0, 0, 1).Unix(),
		Days:     days,
		loc:      loc,
		first:    from,
	}, nil
}

// dayBounds 返回每天开始的 unix 秒，最后一个是 End（长度 Days+1）。
func (r StatsRange) dayBounds() []int64 {
	bounds := make([]int64, 0, r.Days+1)
	for i := 0; i <= r.Days; i++ {
		bounds = append(bounds, r.first.AddDate(0, 0, i).Unix())
	}
	return bounds
}

// dates 返回每一天（2006-01-02），与 dayBounds 的前 Days 个对应。
func (r StatsRange) dates() []string {
	out := make([]string, 0, r.Days)
	for i := 0; i < r.Days; i++ {
		out = append(out, r.first.AddDate(0, 0, i).Format("2006-01-02"))
	}
	return out
}

// hourBounds 只用于一天的时间段：每小时开始的 unix 秒 + End，严格递增。
// 夏令时那天会少一格或某一格是两小时，按本地钟点切，不会丢数据。
func (r StatsRange) hourBounds() []int64 {
	bounds := make([]int64, 0, 26)
	for h := 0; h < 24; h++ {
		t := time.Date(r.first.Year(), r.first.Month(), r.first.Day(), h, 0, 0, 0, r.loc).Unix()
		if t < r.Start || (len(bounds) > 0 && t <= bounds[len(bounds)-1]) {
			continue
		}
		bounds = append(bounds, t)
	}
	if len(bounds) == 0 || bounds[0] != r.Start {
		bounds = append([]int64{r.Start}, bounds...)
	}
	return append(bounds, r.End)
}

// statsBucketSQL 把 time 映射到 bounds 的格子序号；所有行已在 [bounds[0], bounds[n]) 内。
// 用 CASE 而不是整除，三种数据库写法一样，夏令时也按本地钟点算。
func statsBucketSQL(bounds []int64) (string, []any) {
	cells := len(bounds) - 1
	if cells <= 1 {
		return "0", nil
	}
	var b strings.Builder
	args := make([]any, 0, cells-1)
	b.WriteString("CASE")
	for i := 1; i < cells; i++ {
		b.WriteString(" WHEN time < ? THEN ")
		b.WriteString(strconv.Itoa(i - 1))
		args = append(args, bounds[i])
	}
	b.WriteString(" ELSE ")
	b.WriteString(strconv.Itoa(cells - 1))
	b.WriteString(" END")
	return b.String(), args
}

func statsBucketIndex(t int64, bounds []int64) int {
	// 第一个 > t 的边界的前一格。
	i := sort.Search(len(bounds), func(i int) bool { return bounds[i] > t })
	return i - 1
}

// statsAggSpec 描述一次分组：SQL 端怎么取键，以及内存里还没落库的日志用同样的规则怎么取键。
type statsAggSpec struct {
	k1, k2       string
	k1Args       []any
	k2Args       []any
	name, owner  string
	extraWhere   string
	extraArgs    []any
	goKey        func(model.RelayLog) (k1, k2, name string, owner int)
	pendingAllow func(model.RelayLog) bool
}

const statsModelKeySQL = "COALESCE(NULLIF(TRIM(request_model_name),''), NULLIF(TRIM(actual_model_name),''), NULLIF(TRIM(billing_model),''), 'unknown')"

// statsModelKey 与 statsModelKeySQL 同一规则；SQL 的 TRIM 只去空格，所以这里也只去空格。
func statsModelKey(l model.RelayLog) string {
	for _, v := range []string{l.RequestModelName, l.ActualModelName, l.BillingModel} {
		if s := strings.Trim(v, " "); s != "" {
			return s
		}
	}
	return "unknown"
}

func statsAggSpecFor(by string, rng StatsRange) (statsAggSpec, error) {
	switch by {
	case StatsByModel:
		return statsAggSpec{k1: statsModelKeySQL, k2: "''", name: "''", owner: "0",
			goKey: func(l model.RelayLog) (string, string, string, int) { return statsModelKey(l), "", "", 0 }}, nil
	case StatsByChannel:
		return statsAggSpec{k1: "COALESCE(channel_id,0)", k2: "''", name: "channel_name", owner: "0",
			goKey: func(l model.RelayLog) (string, string, string, int) {
				return strconv.Itoa(l.ChannelId), "", l.ChannelName, 0
			}}, nil
	case StatsByPlan:
		return statsAggSpec{k1: "COALESCE(access_plan_id,0)", k2: "''", name: "COALESCE(NULLIF(access_plan_name,''), access_plan_slug)", owner: "0",
			goKey: func(l model.RelayLog) (string, string, string, int) {
				name := l.AccessPlanName
				if name == "" {
					name = l.AccessPlanSlug
				}
				return strconv.Itoa(l.AccessPlanID), "", name, 0
			}}, nil
	case StatsByUser:
		return statsAggSpec{k1: "COALESCE(user_id,0)", k2: "''", name: "user_name", owner: "0",
			goKey: func(l model.RelayLog) (string, string, string, int) {
				return strconv.Itoa(l.UserID), "", l.UserName, 0
			}}, nil
	case StatsByAPIKey:
		return statsAggSpec{k1: "COALESCE(api_key_id,0)", k2: "''", name: "request_api_key_name", owner: "COALESCE(user_id,0)",
			goKey: func(l model.RelayLog) (string, string, string, int) {
				return strconv.Itoa(l.APIKeyID), "", l.RequestAPIKeyName, l.UserID
			}}, nil
	case StatsByHour:
		if rng.Days != 1 {
			return statsAggSpec{}, statsRangeErr("按小时只能看一天")
		}
		bounds := rng.hourBounds()
		expr, args := statsBucketSQL(bounds)
		return statsAggSpec{k1: expr, k1Args: args, k2: "''", name: "''", owner: "0",
			goKey: func(l model.RelayLog) (string, string, string, int) {
				return strconv.Itoa(statsBucketIndex(l.Time, bounds)), "", "", 0
			}}, nil
	default:
		return statsAggSpec{}, statsRangeErr("by 只能是 model / channel / plan / user / apikey / hour")
	}
}

// statsUserDaySpec 按（用户, 天）分组，给「前 N 名每天花多少」用。
func statsUserDaySpec(rng StatsRange) statsAggSpec {
	bounds := rng.dayBounds()
	expr, args := statsBucketSQL(bounds)
	return statsAggSpec{k1: "COALESCE(user_id,0)", k2: expr, k2Args: args, name: "user_name", owner: "0",
		goKey: func(l model.RelayLog) (string, string, string, int) {
			return strconv.Itoa(l.UserID), strconv.Itoa(statsBucketIndex(l.Time, bounds)), l.UserName, 0
		}}
}

// statsFailedSQL 与 statsLogFailed 同一规则：优先用写入时算好的 severity；
// 还没回填的旧行（severity=0）退回原来的判据。
var statsFailedSQL = fmt.Sprintf("(severity = %d OR (severity = %d AND %s))",
	model.RelayLogSeverityError, model.RelayLogSeverityUnset, relayLogErrorSQLCond)

const statsCostSQL = "CASE WHEN cost <> 0 THEN cost ELSE COALESCE(final_input_cost,0)+COALESCE(final_output_cost,0)+COALESCE(final_cache_read_cost,0)+COALESCE(final_cache_write_cost,0) END"

func statsLogFailed(l model.RelayLog) bool {
	if l.Severity == model.RelayLogSeverityUnset {
		return relayLogIsError(l)
	}
	return l.Severity == model.RelayLogSeverityError
}

func statsLogCounted(l model.RelayLog) bool {
	return !strings.Contains(strings.ToLower(l.ErrorStrategy), statsNotCountedMarker)
}

func statsUsageOfLog(l model.RelayLog) model.StatsUsage {
	u := model.StatsUsage{
		InputToken:      int64(l.InputTokens),
		OutputToken:     int64(l.OutputTokens),
		CacheHitToken:   int64(l.CacheHitTokens),
		CacheWriteToken: int64(l.CacheWriteTokens),
		CacheInputToken: int64(l.CacheInputTokens),
		TotalCost:       modelRankLogCost(l),
		WaitTime:        int64(l.UseTime),
	}
	if statsLogFailed(l) {
		u.RequestFailed = 1
	} else {
		u.RequestSuccess = 1
	}
	return u
}

// statsTimeIndexHint 只在 SQLite 上、且索引确实存在时返回 INDEXED BY。
// MySQL / Postgres 的规划器对 time 范围本来就会选它，不加提示，免得索引名不同时直接报错。
func statsTimeIndexHint() string {
	conn := db.GetDB()
	if conn == nil || conn.Dialector == nil || conn.Dialector.Name() != "sqlite" {
		return ""
	}
	if !conn.Migrator().HasIndex(&model.RelayLog{}, statsTimeIndexName) {
		return ""
	}
	return " INDEXED BY " + statsTimeIndexName
}

// statsAggSQL 拼出聚合 SQL。单独拆出来，测试可以对它跑 EXPLAIN QUERY PLAN。
func statsAggSQL(spec statsAggSpec, rng StatsRange, hint string) (string, []any) {
	query := "SELECT " + spec.k1 + " AS k1, " + spec.k2 + " AS k2" +
		", COALESCE(MAX(" + spec.name + "),'') AS name" +
		", COALESCE(MAX(" + spec.owner + "),0) AS owner" +
		", COUNT(*) AS requests" +
		", COALESCE(SUM(CASE WHEN " + statsFailedSQL + " THEN 1 ELSE 0 END),0) AS failed" +
		", COALESCE(SUM(input_tokens),0) AS input_token" +
		", COALESCE(SUM(output_tokens),0) AS output_token" +
		", COALESCE(SUM(cache_hit_tokens),0) AS cache_hit_token" +
		", COALESCE(SUM(cache_write_tokens),0) AS cache_write_token" +
		", COALESCE(SUM(cache_input_tokens),0) AS cache_input_token" +
		", COALESCE(SUM(" + statsCostSQL + "),0) AS cost" +
		", COALESCE(SUM(use_time),0) AS wait_time" +
		" FROM relay_logs" + hint +
		" WHERE time >= ? AND time < ? AND COALESCE(error_strategy,'') NOT LIKE ?" + spec.extraWhere +
		" GROUP BY k1, k2"
	args := make([]any, 0, len(spec.k1Args)+len(spec.k2Args)+3+len(spec.extraArgs))
	args = append(args, spec.k1Args...)
	args = append(args, spec.k2Args...)
	args = append(args, rng.Start, rng.End, "%"+statsNotCountedMarker+"%")
	args = append(args, spec.extraArgs...)
	return query, args
}

type statsAggRow struct {
	K1              string  `gorm:"column:k1"`
	K2              string  `gorm:"column:k2"`
	Name            string  `gorm:"column:name"`
	Owner           int64   `gorm:"column:owner"`
	Requests        int64   `gorm:"column:requests"`
	Failed          int64   `gorm:"column:failed"`
	InputToken      int64   `gorm:"column:input_token"`
	OutputToken     int64   `gorm:"column:output_token"`
	CacheHitToken   int64   `gorm:"column:cache_hit_token"`
	CacheWriteToken int64   `gorm:"column:cache_write_token"`
	CacheInputToken int64   `gorm:"column:cache_input_token"`
	Cost            float64 `gorm:"column:cost"`
	WaitTime        int64   `gorm:"column:wait_time"`
}

type statsAggGroup struct {
	k1, k2 string
	name   string
	owner  int
	usage  model.StatsUsage
}

// statsBreakdownSem 限制同时跑的聚合查询：仪表盘一屏会发好几个，别让它们一起压在 SQLite 上。
var statsBreakdownSem = make(chan struct{}, 2)

func statsAggregate(ctx context.Context, spec statsAggSpec, rng StatsRange) (map[string]*statsAggGroup, error) {
	select {
	case statsBreakdownSem <- struct{}{}:
		defer func() { <-statsBreakdownSem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	groups := make(map[string]*statsAggGroup)
	get := func(k1, k2 string) *statsAggGroup {
		key := k1 + "\x00" + k2
		g := groups[key]
		if g == nil {
			g = &statsAggGroup{k1: k1, k2: k2}
			groups[key] = g
		}
		return g
	}

	query, args := statsAggSQL(spec, rng, statsTimeIndexHint())
	var rows []statsAggRow
	if err := db.GetDB().WithContext(ctx).Raw(query, args...).Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		g := get(row.K1, row.K2)
		if g.name == "" {
			g.name = row.Name
		}
		if g.owner == 0 {
			g.owner = int(row.Owner)
		}
		g.usage.Add(model.StatsUsage{
			RequestSuccess:  row.Requests - row.Failed,
			RequestFailed:   row.Failed,
			InputToken:      row.InputToken,
			OutputToken:     row.OutputToken,
			CacheHitToken:   row.CacheHitToken,
			CacheWriteToken: row.CacheWriteToken,
			CacheInputToken: row.CacheInputToken,
			TotalCost:       row.Cost,
			WaitTime:        row.WaitTime,
		})
	}

	pending, err := statsPendingLogs(ctx, rng, spec.pendingAllow)
	if err != nil {
		return nil, err
	}
	for _, l := range pending {
		k1, k2, name, owner := spec.goKey(l)
		g := get(k1, k2)
		if g.name == "" {
			g.name = name
		}
		if g.owner == 0 {
			g.owner = owner
		}
		g.usage.Add(statsUsageOfLog(l))
	}
	return groups, nil
}

// statsPendingLogs 取内存队列里还没落库、落在时间段内、计入统计的日志。
// 已经落库的（flush 进行中两边都有）按 id 去掉，和 model_rank 一样不重复算。
func statsPendingLogs(ctx context.Context, rng StatsRange, allow func(model.RelayLog) bool) ([]model.RelayLog, error) {
	relayLogCacheLock.Lock()
	candidates := make([]model.RelayLog, 0, len(relayLogCache))
	for _, l := range relayLogCache {
		if l.Time < rng.Start || l.Time >= rng.End || !statsLogCounted(l) {
			continue
		}
		if allow != nil && !allow(l) {
			continue
		}
		candidates = append(candidates, l)
	}
	relayLogCacheLock.Unlock()
	if len(candidates) == 0 {
		return nil, nil
	}

	inDB := make(map[int64]struct{}, len(candidates))
	for start := 0; start < len(candidates); start += statsPendingIDChunk {
		end := start + statsPendingIDChunk
		if end > len(candidates) {
			end = len(candidates)
		}
		ids := make([]int64, 0, end-start)
		for _, l := range candidates[start:end] {
			ids = append(ids, l.ID)
		}
		var found []int64
		if err := db.GetDB().WithContext(ctx).Model(&model.RelayLog{}).Where("id IN ?", ids).Pluck("id", &found).Error; err != nil {
			return nil, err
		}
		for _, id := range found {
			inDB[id] = struct{}{}
		}
	}
	out := candidates[:0]
	for _, l := range candidates {
		if _, ok := inDB[l.ID]; !ok {
			out = append(out, l)
		}
	}
	return out, nil
}

// statsOldestLogTime 是库里 + 内存队列里最早一条日志的时间；MIN(time) 由时间索引直接给出。
func statsOldestLogTime(ctx context.Context) (int64, error) {
	var oldest sql.NullInt64
	if err := db.GetDB().WithContext(ctx).Raw("SELECT MIN(time) FROM relay_logs").Scan(&oldest).Error; err != nil {
		return 0, err
	}
	result := int64(0)
	if oldest.Valid {
		result = oldest.Int64
	}
	relayLogCacheLock.Lock()
	for _, l := range relayLogCache {
		if l.Time > 0 && (result == 0 || l.Time < result) {
			result = l.Time
		}
	}
	relayLogCacheLock.Unlock()
	return result, nil
}

func statsSortItems(items []model.StatsBreakdownItem) {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].TotalCost != items[j].TotalCost {
			return items[i].TotalCost > items[j].TotalCost
		}
		if items[i].Requests() != items[j].Requests() {
			return items[i].Requests() > items[j].Requests()
		}
		return items[i].Key < items[j].Key
	})
}

// StatsBreakdownGet 按 by 拆开一段时间的用量。limit <= 0 用默认值；by=hour 不截断。
func StatsBreakdownGet(ctx context.Context, rng StatsRange, by string, limit int) (model.StatsBreakdown, error) {
	spec, err := statsAggSpecFor(by, rng)
	if err != nil {
		return model.StatsBreakdown{}, err
	}
	groups, err := statsAggregate(ctx, spec, rng)
	if err != nil {
		return model.StatsBreakdown{}, err
	}
	oldest, err := statsOldestLogTime(ctx)
	if err != nil {
		return model.StatsBreakdown{}, err
	}

	result := model.StatsBreakdown{From: rng.FromDate, To: rng.ToDate, By: by, Days: rng.Days, Start: rng.Start, End: rng.End, OldestLogTime: oldest}
	for _, g := range groups {
		result.Total.Add(g.usage)
	}

	if by == StatsByHour {
		bounds := rng.hourBounds()
		items := make([]model.StatsBreakdownItem, len(bounds)-1)
		for i := range items {
			items[i] = model.StatsBreakdownItem{
				Key:   strconv.Itoa(i),
				Name:  time.Unix(bounds[i], 0).In(rng.loc).Format("15:04"),
				Start: bounds[i],
			}
		}
		for _, g := range groups {
			if i, err := strconv.Atoi(g.k1); err == nil && i >= 0 && i < len(items) {
				items[i].StatsUsage.Add(g.usage)
			}
		}
		result.Items = items
		result.ItemCount = len(items)
		return result, nil
	}

	items := make([]model.StatsBreakdownItem, 0, len(groups))
	for _, g := range groups {
		item := model.StatsBreakdownItem{Key: g.k1, Name: g.name, StatsUsage: g.usage}
		if by == StatsByModel {
			item.Name = g.k1
		} else {
			item.ID, _ = strconv.Atoi(g.k1)
		}
		if by == StatsByAPIKey {
			item.UserID = g.owner
		}
		items = append(items, item)
	}
	statsSortItems(items)
	result.ItemCount = len(items)
	if by == StatsByUser {
		active := 0
		for _, item := range items {
			if item.ID > 0 {
				active++
			}
		}
		result.ActiveUsers = &active
	}

	if limit <= 0 {
		limit = statsBreakdownDefaultLimit
	}
	if limit > statsBreakdownMaxLimit {
		limit = statsBreakdownMaxLimit
	}
	if len(items) > limit {
		others := model.StatsUsage{}
		for _, item := range items[limit:] {
			others.Add(item.StatsUsage)
		}
		result.Others = &others
		result.OthersCount = len(items) - limit
		items = items[:limit]
	}
	if err := statsResolveNames(ctx, by, items); err != nil {
		return model.StatsBreakdown{}, err
	}
	result.Items = items
	return result, nil
}

// StatsUserTrendGet 返回这段时间花费最多的前 top 个用户，以及他们每天的用量。
// 一次扫描按（用户, 天）分组，再在内存里挑前几名。
func StatsUserTrendGet(ctx context.Context, rng StatsRange, top int) (model.StatsUserTrend, error) {
	if top <= 0 {
		top = statsUserTrendDefaultTop
	}
	if top > statsUserTrendMaxTop {
		top = statsUserTrendMaxTop
	}
	groups, err := statsAggregate(ctx, statsUserDaySpec(rng), rng)
	if err != nil {
		return model.StatsUserTrend{}, err
	}
	oldest, err := statsOldestLogTime(ctx)
	if err != nil {
		return model.StatsUserTrend{}, err
	}

	dates := rng.dates()
	byUser := make(map[int]*model.StatsUserTrendUser)
	for _, g := range groups {
		id, err := strconv.Atoi(g.k1)
		if err != nil || id <= 0 {
			// user_id=0 不是一个人（没归属的 Key / 管理令牌），不进「前几名」。
			continue
		}
		day, err := strconv.Atoi(g.k2)
		if err != nil || day < 0 || day >= len(dates) {
			continue
		}
		u := byUser[id]
		if u == nil {
			u = &model.StatsUserTrendUser{UserID: id, Name: g.name, Points: make([]model.StatsUsage, len(dates))}
			byUser[id] = u
		}
		if u.Name == "" {
			u.Name = g.name
		}
		u.Points[day].Add(g.usage)
		u.Total.Add(g.usage)
	}

	users := make([]model.StatsUserTrendUser, 0, len(byUser))
	for _, u := range byUser {
		users = append(users, *u)
	}
	sort.SliceStable(users, func(i, j int) bool {
		if users[i].Total.TotalCost != users[j].Total.TotalCost {
			return users[i].Total.TotalCost > users[j].Total.TotalCost
		}
		if users[i].Total.Requests() != users[j].Total.Requests() {
			return users[i].Total.Requests() > users[j].Total.Requests()
		}
		return users[i].UserID < users[j].UserID
	})
	if len(users) > top {
		users = users[:top]
	}

	items := make([]model.StatsBreakdownItem, len(users))
	for i, u := range users {
		items[i] = model.StatsBreakdownItem{ID: u.UserID, Name: u.Name}
	}
	if err := statsResolveNames(ctx, StatsByUser, items); err != nil {
		return model.StatsUserTrend{}, err
	}
	for i := range users {
		users[i].Name = items[i].Name
		users[i].Missing = items[i].Missing
	}
	return model.StatsUserTrend{From: rng.FromDate, To: rng.ToDate, Dates: dates, Users: users, OldestLogTime: oldest}, nil
}

// statsResolveNames 用当前的名字（用户名 / Key 名 / 渠道名 / 方案名）替换日志里记的旧名字；
// 已经删掉的标 Missing，名字留日志里的。只查本页这几行的 id。
func statsResolveNames(ctx context.Context, by string, items []model.StatsBreakdownItem) error {
	ids := make([]int, 0, len(items))
	for _, item := range items {
		if item.ID > 0 {
			ids = append(ids, item.ID)
		}
	}
	conn := db.GetDB().WithContext(ctx)
	names := make(map[int]string, len(ids))
	owners := make(map[int]int)
	switch by {
	case StatsByUser:
		if len(ids) > 0 {
			var users []model.User
			if err := conn.Select("id", "username").Where("id IN ?", ids).Find(&users).Error; err != nil {
				return err
			}
			for _, u := range users {
				names[u.ID] = u.Username
			}
		}
	case StatsByAPIKey:
		if len(ids) > 0 {
			var keys []model.APIKey
			if err := conn.Select("id", "name", "user_id").Where("id IN ?", ids).Find(&keys).Error; err != nil {
				return err
			}
			for _, k := range keys {
				names[k.ID] = k.Name
				owners[k.ID] = k.UserID
			}
		}
	case StatsByChannel:
		if len(ids) > 0 {
			var channels []model.Channel
			if err := conn.Select("id", "name").Where("id IN ?", ids).Find(&channels).Error; err != nil {
				return err
			}
			for _, ch := range channels {
				names[ch.ID] = ch.Name
			}
		}
	case StatsByPlan:
		if len(ids) > 0 {
			var plans []model.AccessPlan
			if err := conn.Select("id", "slug", "display_name").Where("id IN ?", ids).Find(&plans).Error; err != nil {
				return err
			}
			for _, p := range plans {
				name := strings.TrimSpace(p.DisplayName)
				if name == "" {
					name = p.Slug
				}
				names[p.ID] = name
			}
		}
	default:
		return nil
	}

	for i := range items {
		if items[i].ID <= 0 {
			continue
		}
		name, ok := names[items[i].ID]
		if !ok {
			items[i].Missing = true
			continue
		}
		items[i].Name = name
		if owner, ok := owners[items[i].ID]; ok {
			items[i].UserID = owner
		}
	}

	if by == StatsByAPIKey {
		ownerIDs := make([]int, 0, len(items))
		for _, item := range items {
			if item.UserID > 0 {
				ownerIDs = append(ownerIDs, item.UserID)
			}
		}
		if len(ownerIDs) > 0 {
			var users []model.User
			if err := conn.Select("id", "username").Where("id IN ?", ownerIDs).Find(&users).Error; err != nil {
				return err
			}
			userNames := make(map[int]string, len(users))
			for _, u := range users {
				userNames[u.ID] = u.Username
			}
			for i := range items {
				items[i].UserName = userNames[items[i].UserID]
			}
		}
	}
	return nil
}
