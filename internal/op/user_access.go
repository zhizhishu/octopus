package op

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/utils/log"
)

// 用户页「IP / 客户端」与「异常切换」提示。
//
// 记什么：每个请求（经 RelayLogAdd 的那一份日志）的 user_id / api_key_id / 入站 IP /
// 入站 User-Agent 聚合进 user_access_seen（见 model.UserAccessSeen）。只读入站数据——
// IP 来自 middleware.ResolveClientIP（与日志 request_ip 同一个值），UA 是调用端原样的
// User-Agent（与日志 request_user_agent 同一个值）。不碰出站请求头 / 指纹 / 请求体。
//
// 参考项目怎么做的（2026-10-30 最新版）：
//   - new-api 在 logs.ip 存每条请求的 IP（用户自己开 record_ip_log 才记），token 上有 allow_ips 白名单；
//   - sub2api 在 usage_logs 存 ip_address + user_agent，按条展示，api_key 上有 ip_whitelist / ip_blacklist；
//   - claude-code-hub 在用户上有 allowedClients（按 UA 限制客户端）。
// 三家都只「记录 / 白名单限制」，没有一家做「切换检测」。所以这里的异常规则（见 userAccessAlerts）
// 参照账号共享 / Key 泄露检测的通行做法自己定，灵敏度做成设置「风控等级」（预设阈值和出处见
// model.UserAccessAlertPreset）。只提示，不拦截。

const (
	// UserAccessKeepDays：多久没再出现的组合会被清掉；「以前见过没有」也只往回看这么久。
	UserAccessKeepDays = 90

	userAccessPendingMax   = 20000
	userAccessSaveTimeout  = 30 * time.Second
	userAccessPruneEvery   = time.Hour
	userAccessDetailMaxRow = 1000
	userAccessDetailGroups = 50
	userAccessGroupValues  = 10
	userAccessUAMaxRunes   = 512
)

var (
	userAccessPendingMu      sync.Mutex
	userAccessPending        = make(map[string]*model.UserAccessSeen)
	userAccessPendingDropped int64
	userAccessLastPrune      time.Time
)

func normalizeAccessUA(ua string) string {
	ua = strings.TrimSpace(ua)
	if r := []rune(ua); len(r) > userAccessUAMaxRunes {
		ua = string(r[:userAccessUAMaxRunes])
	}
	return ua
}

// UserAccessRecord 记一次入站请求的来源。只动内存，DB 由 UserAccessSaveDB 周期批量补齐
// （与 UserRecordRelayIP 同一个做法，转发热路径上不写库）。没有用户或解析不出 IP 时不记。
func UserAccessRecord(userID, apiKeyID int, ip, ua string, at int64) {
	ip = normalizeRequestIP(ip)
	if userID <= 0 || ip == "" {
		return
	}
	ua = normalizeAccessUA(ua)
	if at <= 0 {
		at = time.Now().Unix()
	}
	key := model.UserAccessSeenKey(userID, apiKeyID, ip, ua)
	userAccessPendingMu.Lock()
	defer userAccessPendingMu.Unlock()
	if row, ok := userAccessPending[key]; ok {
		row.Requests++
		if at > row.LastSeen {
			row.LastSeen = at
		}
		if at < row.FirstSeen {
			row.FirstSeen = at
		}
		return
	}
	if len(userAccessPending) >= userAccessPendingMax {
		userAccessPendingDropped++
		return
	}
	userAccessPending[key] = &model.UserAccessSeen{
		SeenKey: key, UserID: userID, APIKeyID: apiKeyID, IP: ip, UserAgent: ua,
		FirstSeen: at, LastSeen: at, Requests: 1,
	}
}

// UserAccessSaveDB 把内存里攒的来源记录合进 user_access_seen，并顺手清掉 UserAccessKeepDays
// 天没再出现的行（每小时最多清一次）。
func UserAccessSaveDB(ctx context.Context) error {
	userAccessPendingMu.Lock()
	pending := userAccessPending
	userAccessPending = make(map[string]*model.UserAccessSeen, len(pending))
	dropped := userAccessPendingDropped
	userAccessPendingDropped = 0
	userAccessPendingMu.Unlock()
	if dropped > 0 {
		log.Warnf("user access record: dropped %d new ip/client combos (pending cap %d)", dropped, userAccessPendingMax)
	}

	var firstErr error
	for _, row := range pending {
		if err := userAccessUpsert(ctx, *row); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("failed to save user access: %w", err)
		}
	}

	if time.Since(userAccessLastPrune) >= userAccessPruneEvery {
		userAccessLastPrune = time.Now()
		cutoff := time.Now().Add(-UserAccessKeepDays * 24 * time.Hour).Unix()
		if err := db.GetDB().WithContext(ctx).Where("last_seen < ?", cutoff).Delete(&model.UserAccessSeen{}).Error; err != nil && firstErr == nil {
			firstErr = fmt.Errorf("failed to prune user access: %w", err)
		}
	}
	return firstErr
}

// userAccessUpsert：先 UPDATE（累加次数、取更早的首次 / 更晚的最近），没命中再 INSERT；
// INSERT 撞唯一键（并发的另一个写者先插了）就再 UPDATE 一次。三种库通用，不依赖方言的 ON CONFLICT 语法。
func userAccessUpsert(ctx context.Context, row model.UserAccessSeen) error {
	update := func() (int64, error) {
		res := db.GetDB().WithContext(ctx).Exec(
			"UPDATE user_access_seen SET requests = requests + ?, "+
				"last_seen = CASE WHEN last_seen < ? THEN ? ELSE last_seen END, "+
				"first_seen = CASE WHEN first_seen > ? THEN ? ELSE first_seen END "+
				"WHERE seen_key = ?",
			row.Requests, row.LastSeen, row.LastSeen, row.FirstSeen, row.FirstSeen, row.SeenKey,
		)
		return res.RowsAffected, res.Error
	}
	n, err := update()
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	row.ID = 0
	if err := db.GetDB().WithContext(ctx).Create(&row).Error; err != nil {
		if n2, err2 := update(); err2 == nil && n2 > 0 {
			return nil
		}
		return err
	}
	return nil
}

// UserAccessSaveDBTask 是周期任务入口。
func UserAccessSaveDBTask() {
	ctx, cancel := context.WithTimeout(context.Background(), userAccessSaveTimeout)
	defer cancel()
	if err := UserAccessSaveDB(ctx); err != nil {
		log.Warnf("user access save db error: %v", err)
	}
}

// userAccessMergePending 把还没落库的记录合进查出来的行（同 seen_key 累加），
// 让刚发生的请求在用户页上立刻可见。keep 为 nil 时不筛用户。
func userAccessMergePending(rows []model.UserAccessSeen, keep func(model.UserAccessSeen) bool) []model.UserAccessSeen {
	idx := make(map[string]int, len(rows))
	for i, r := range rows {
		if r.SeenKey == "" {
			r.SeenKey = model.UserAccessSeenKey(r.UserID, r.APIKeyID, r.IP, r.UserAgent)
			rows[i] = r
		}
		idx[r.SeenKey] = i
	}
	userAccessPendingMu.Lock()
	defer userAccessPendingMu.Unlock()
	for key, p := range userAccessPending {
		if keep != nil && !keep(*p) {
			continue
		}
		if i, ok := idx[key]; ok {
			rows[i].Requests += p.Requests
			if p.LastSeen > rows[i].LastSeen {
				rows[i].LastSeen = p.LastSeen
			}
			if p.FirstSeen < rows[i].FirstSeen {
				rows[i].FirstSeen = p.FirstSeen
			}
			continue
		}
		idx[key] = len(rows)
		rows = append(rows, *p)
	}
	return rows
}

// userAccessIPBucket 是判「是不是同一个地方」用的 IP 归一：IPv4 按 v4Prefix（32 = 完整地址，
// 24 = 同一 /24 算一个），IPv6 固定按 /64。IPv6 的隐私地址会在同一个 /64 里频繁换后缀，
// 不归一的话同一台电脑也会被报成「换了 IP」。
func userAccessIPBucket(ip string, v4Prefix int) string {
	parsed := net.ParseIP(strings.TrimSpace(ip))
	if parsed == nil {
		return strings.TrimSpace(ip)
	}
	if v4 := parsed.To4(); v4 != nil {
		if v4Prefix > 0 && v4Prefix < 32 {
			return v4.Mask(net.CIDRMask(v4Prefix, 32)).String() + "/" + strconv.Itoa(v4Prefix)
		}
		return v4.String()
	}
	return parsed.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

// userAccessClientFamily 是判「是不是同一个客户端」用的 UA 归一：取第一个产品名（`/`、空格、
// `(` 之前那段，小写）。`claude-cli/2.0.1 (external, cli)` 和 `claude-cli/2.0.5` 是同一个客户端，
// 升级版本不算切换。不维护 UA→名称映射表（展示名在前端映射，同 relay_logs 的做法）。
func userAccessClientFamily(ua string) string {
	ua = strings.ToLower(strings.TrimSpace(ua))
	if i := strings.IndexAny(ua, "/ ("); i >= 0 {
		ua = ua[:i]
	}
	return ua
}

type userAccessBucket struct {
	first, last int64
	sample      string
	sampleLast  int64
}

func userAccessBuckets(rows []model.UserAccessSeen, keyOf func(model.UserAccessSeen) string, valueOf func(model.UserAccessSeen) string) map[string]*userAccessBucket {
	out := make(map[string]*userAccessBucket)
	for _, r := range rows {
		k := keyOf(r)
		b := out[k]
		if b == nil {
			b = &userAccessBucket{first: r.FirstSeen, last: r.LastSeen, sample: valueOf(r), sampleLast: r.LastSeen}
			out[k] = b
			continue
		}
		if r.FirstSeen < b.first {
			b.first = r.FirstSeen
		}
		if r.LastSeen > b.last {
			b.last = r.LastSeen
		}
		if r.LastSeen > b.sampleLast {
			b.sample, b.sampleLast = valueOf(r), r.LastSeen
		}
	}
	return out
}

// userAccessAlerts 是「异常切换」规则（rows 必须是这个用户的全部行，首次出现时间才可信），
// 阈值取自 cfg（设置「风控等级」）：
//
//  1. many_ips：最近 cfg.WindowSec 内用过的不同 IP（按 cfg.IPv4Prefix / IPv6 /64 归并）≥ cfg.ManyIPs 个。
//     cfg.ManyIPs = 0 不看。
//  2. new_ip（cfg.NewIP）：窗口内用过 ≥2 个 IP，且其中有一个是窗口内才第一次出现的
//     （= 冒出一个从没见过的 IP，同时另一个 IP 也还在用）。已经报了 many_ips 就不再报它。
//  3. new_client（cfg.NewClient）：同 2，只是把 IP 换成客户端（UA 的产品名，升级版本不算）。
//
// 两个都是「以前见过」的 IP 来回换（家里 / 公司）不报 new_ip；只有一个 IP 时永远不报。
// 过了窗口没有新请求，提示自然消失。cfg 全关（等级「关闭」）时什么都不报。
func userAccessAlerts(rows []model.UserAccessSeen, now int64, cfg model.UserAccessAlertConfig) []model.UserAccessAlert {
	alerts := make([]model.UserAccessAlert, 0)
	if len(rows) == 0 || !cfg.Enabled() {
		return alerts
	}
	windowSec := cfg.WindowSec
	since := now - windowSec

	check := func(kind string, buckets map[string]*userAccessBucket, many int, reportNew bool) {
		type item struct {
			key string
			b   *userAccessBucket
		}
		active := make([]item, 0)
		for k, b := range buckets {
			if b.last >= since {
				active = append(active, item{k, b})
			}
		}
		if len(active) < 2 {
			return
		}
		sort.Slice(active, func(i, j int) bool {
			if active[i].b.last != active[j].b.last {
				return active[i].b.last > active[j].b.last
			}
			return active[i].key < active[j].key
		})
		if many > 0 && len(active) >= many {
			others := make([]string, 0, len(active))
			for _, it := range active {
				others = append(others, it.b.sample)
			}
			alerts = append(alerts, model.UserAccessAlert{
				Kind: model.UserAccessAlertManyIPs, WindowSec: windowSec, Count: len(active),
				Others: others, At: active[0].b.last,
			})
			return
		}
		if !reportNew {
			return
		}
		var newest *item
		for i := range active {
			it := &active[i]
			if it.b.first < since {
				continue
			}
			if newest == nil || it.b.first > newest.b.first || (it.b.first == newest.b.first && it.key < newest.key) {
				newest = it
			}
		}
		if newest == nil {
			return
		}
		others := make([]string, 0, len(active)-1)
		for _, it := range active {
			if it.key != newest.key {
				others = append(others, it.b.sample)
			}
		}
		alerts = append(alerts, model.UserAccessAlert{
			Kind: kind, WindowSec: windowSec, Count: len(active),
			Value: newest.b.sample, Others: others, At: newest.b.first,
		})
	}

	if cfg.ManyIPs > 0 || cfg.NewIP {
		check(model.UserAccessAlertNewIP,
			userAccessBuckets(rows, func(r model.UserAccessSeen) string { return userAccessIPBucket(r.IP, cfg.IPv4Prefix) }, func(r model.UserAccessSeen) string { return r.IP }),
			cfg.ManyIPs, cfg.NewIP)
	}
	if cfg.NewClient {
		check(model.UserAccessAlertNewClient,
			userAccessBuckets(rows, func(r model.UserAccessSeen) string { return userAccessClientFamily(r.UserAgent) }, func(r model.UserAccessSeen) string { return r.UserAgent }),
			0, true)
	}
	return alerts
}

// UserAccessAlertConfigCurrent 读设置「风控等级」得出当前生效的阈值。预设等级直接用预设；
// 自定义读五个数（读不到 / 不合法的那一项退回「标准」的值）；等级本身认不出时按「标准」。
func UserAccessAlertConfigCurrent() model.UserAccessAlertConfig {
	level, _ := SettingGetString(model.SettingKeyUserAccessAlertLevel)
	return userAccessAlertConfigFrom(level, func(key model.SettingKey) (string, bool) {
		v, err := SettingGetString(key)
		return v, err == nil
	})
}

func userAccessAlertConfigFrom(level string, get func(model.SettingKey) (string, bool)) model.UserAccessAlertConfig {
	level = strings.TrimSpace(level)
	if cfg, ok := model.UserAccessAlertPreset(level); ok {
		return cfg
	}
	cfg, _ := model.UserAccessAlertPreset(model.UserAccessAlertLevelStandard)
	if level != model.UserAccessAlertLevelCustom {
		return cfg
	}
	cfg.Level = model.UserAccessAlertLevelCustom
	valid := func(key model.SettingKey) (string, bool) {
		v, ok := get(key)
		if !ok {
			return "", false
		}
		v = strings.TrimSpace(v)
		if (&model.Setting{Key: key, Value: v}).Validate() != nil {
			return "", false
		}
		return v, true
	}
	if v, ok := valid(model.SettingKeyUserAccessAlertWindowMin); ok {
		n, _ := strconv.Atoi(v)
		cfg.WindowSec = int64(n) * 60
	}
	if v, ok := valid(model.SettingKeyUserAccessAlertManyIPs); ok {
		cfg.ManyIPs, _ = strconv.Atoi(v)
	}
	if v, ok := valid(model.SettingKeyUserAccessAlertNewIP); ok {
		cfg.NewIP = v == "true"
	}
	if v, ok := valid(model.SettingKeyUserAccessAlertNewClient); ok {
		cfg.NewClient = v == "true"
	}
	if v, ok := valid(model.SettingKeyUserAccessAlertIPv4Prefix); ok {
		cfg.IPv4Prefix, _ = strconv.Atoi(v)
	}
	return cfg
}

// UserAccessAlertSettingsGet 给设置页：当前阈值 + 各预设阈值。
func UserAccessAlertSettingsGet() model.UserAccessAlertSettings {
	presets := make(map[string]model.UserAccessAlertConfig, len(model.UserAccessAlertPresetLevels))
	for _, level := range model.UserAccessAlertPresetLevels {
		presets[level], _ = model.UserAccessAlertPreset(level)
	}
	return model.UserAccessAlertSettings{Current: UserAccessAlertConfigCurrent(), Presets: presets}
}

func userAccessKeyName(id int) string {
	if id <= 0 {
		return ""
	}
	if k, ok := apiKeyCache.Get(id); ok {
		return k.Name
	}
	return ""
}

func userAccessLastOf(rows []model.UserAccessSeen) *model.UserAccessLast {
	var best *model.UserAccessSeen
	for i := range rows {
		r := &rows[i]
		if best == nil || r.LastSeen > best.LastSeen || (r.LastSeen == best.LastSeen && r.Requests > best.Requests) {
			best = r
		}
	}
	if best == nil {
		return nil
	}
	return &model.UserAccessLast{IP: best.IP, UserAgent: best.UserAgent, APIKeyID: best.APIKeyID, APIKeyName: userAccessKeyName(best.APIKeyID), At: best.LastSeen}
}

// UserAccessSummaries 给用户列表用：每个有记录的用户一条（最近一次来源 + 异常提示）。
func UserAccessSummaries(ctx context.Context) ([]model.UserAccessSummary, error) {
	return userAccessSummariesAt(ctx, time.Now())
}

func userAccessSummariesAt(ctx context.Context, now time.Time) ([]model.UserAccessSummary, error) {
	nowUnix := now.Unix()
	cfg := UserAccessAlertConfigCurrent()
	since := nowUnix - cfg.WindowSec

	// 每人最近的一行（相关子查询走 (user_id, last_seen) 索引）。
	var latest []model.UserAccessSeen
	if err := db.GetDB().WithContext(ctx).Raw(
		"SELECT * FROM user_access_seen t WHERE t.last_seen = (SELECT MAX(s.last_seen) FROM user_access_seen s WHERE s.user_id = t.user_id)",
	).Scan(&latest).Error; err != nil {
		return nil, err
	}
	// 窗口内活跃过的人：要拿他的全部行，「首次出现」才算得准。
	var activeIDs []int
	if err := db.GetDB().WithContext(ctx).Model(&model.UserAccessSeen{}).
		Where("last_seen >= ?", since).Distinct().Pluck("user_id", &activeIDs).Error; err != nil {
		return nil, err
	}
	userAccessPendingMu.Lock()
	for _, p := range userAccessPending {
		if p.LastSeen >= since {
			activeIDs = append(activeIDs, p.UserID)
		}
	}
	userAccessPendingMu.Unlock()
	activeSet := make(map[int]struct{}, len(activeIDs))
	ids := make([]int, 0, len(activeIDs))
	for _, id := range activeIDs {
		if _, ok := activeSet[id]; !ok {
			activeSet[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	var activeRows []model.UserAccessSeen
	if len(ids) > 0 {
		if err := db.GetDB().WithContext(ctx).Where("user_id IN ?", ids).Find(&activeRows).Error; err != nil {
			return nil, err
		}
	}
	activeRows = userAccessMergePending(activeRows, func(r model.UserAccessSeen) bool { _, ok := activeSet[r.UserID]; return ok })

	byUser := make(map[int][]model.UserAccessSeen)
	for _, r := range latest {
		if _, ok := activeSet[r.UserID]; ok {
			continue // 活跃的人用 activeRows（已含 pending）
		}
		byUser[r.UserID] = append(byUser[r.UserID], r)
	}
	for _, r := range activeRows {
		byUser[r.UserID] = append(byUser[r.UserID], r)
	}

	out := make([]model.UserAccessSummary, 0, len(byUser))
	for userID, rows := range byUser {
		last := userAccessLastOf(rows)
		if last == nil {
			continue
		}
		alerts := make([]model.UserAccessAlert, 0)
		if _, ok := activeSet[userID]; ok {
			alerts = userAccessAlerts(rows, nowUnix, cfg)
		}
		out = append(out, model.UserAccessSummary{UserID: userID, Last: *last, Alerts: alerts})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UserID < out[j].UserID })
	return out, nil
}

// UserAccessDetailGet 给抽屉用：这个人的 IP 列表、客户端列表（各自次数 / 最近时间 / 用过的 key）和异常提示。
func UserAccessDetailGet(userID int, ctx context.Context) (model.UserAccessDetail, error) {
	return userAccessDetailAt(userID, ctx, time.Now())
}

func userAccessDetailAt(userID int, ctx context.Context, now time.Time) (model.UserAccessDetail, error) {
	if userID <= 0 {
		return model.UserAccessDetail{}, fmt.Errorf("invalid user id")
	}
	if _, err := UserGet(userID); err != nil {
		return model.UserAccessDetail{}, err
	}
	var rows []model.UserAccessSeen
	if err := db.GetDB().WithContext(ctx).Where("user_id = ?", userID).
		Order("last_seen DESC").Limit(userAccessDetailMaxRow).Find(&rows).Error; err != nil {
		return model.UserAccessDetail{}, err
	}
	rows = userAccessMergePending(rows, func(r model.UserAccessSeen) bool { return r.UserID == userID })

	cfg := UserAccessAlertConfigCurrent()
	out := model.UserAccessDetail{
		UserID: userID, Config: cfg, WindowSec: cfg.WindowSec,
		ManyIPsLimit: cfg.ManyIPs, KeepDays: UserAccessKeepDays,
		Last:   userAccessLastOf(rows),
		Alerts: userAccessAlerts(rows, now.Unix(), cfg),
	}
	for _, r := range rows {
		out.TotalRequests += r.Requests
	}
	out.IPs = userAccessGroup(rows, func(r model.UserAccessSeen) string { return r.IP }, func(r model.UserAccessSeen) string { return r.UserAgent })
	out.Clients = userAccessGroup(rows, func(r model.UserAccessSeen) string { return r.UserAgent }, func(r model.UserAccessSeen) string { return r.IP })
	return out, nil
}

func userAccessGroup(rows []model.UserAccessSeen, keyOf, subOf func(model.UserAccessSeen) string) []model.UserAccessGroup {
	type acc struct {
		g    model.UserAccessGroup
		keys map[int]*model.UserAccessKeyUse
		subs map[string]int64
	}
	groups := make(map[string]*acc)
	for _, r := range rows {
		k := keyOf(r)
		a := groups[k]
		if a == nil {
			a = &acc{g: model.UserAccessGroup{Value: k, FirstSeen: r.FirstSeen, LastSeen: r.LastSeen}, keys: map[int]*model.UserAccessKeyUse{}, subs: map[string]int64{}}
			groups[k] = a
		}
		a.g.Requests += r.Requests
		if r.FirstSeen < a.g.FirstSeen {
			a.g.FirstSeen = r.FirstSeen
		}
		if r.LastSeen > a.g.LastSeen {
			a.g.LastSeen = r.LastSeen
		}
		ku := a.keys[r.APIKeyID]
		if ku == nil {
			ku = &model.UserAccessKeyUse{APIKeyID: r.APIKeyID, APIKeyName: userAccessKeyName(r.APIKeyID)}
			a.keys[r.APIKeyID] = ku
		}
		ku.Requests += r.Requests
		if r.LastSeen > ku.LastSeen {
			ku.LastSeen = r.LastSeen
		}
		if s := subOf(r); r.LastSeen > a.subs[s] {
			a.subs[s] = r.LastSeen
		}
	}
	out := make([]model.UserAccessGroup, 0, len(groups))
	for _, a := range groups {
		a.g.Keys = make([]model.UserAccessKeyUse, 0, len(a.keys))
		for _, ku := range a.keys {
			a.g.Keys = append(a.g.Keys, *ku)
		}
		sort.Slice(a.g.Keys, func(i, j int) bool {
			if a.g.Keys[i].LastSeen != a.g.Keys[j].LastSeen {
				return a.g.Keys[i].LastSeen > a.g.Keys[j].LastSeen
			}
			return a.g.Keys[i].APIKeyID < a.g.Keys[j].APIKeyID
		})
		subs := make([]string, 0, len(a.subs))
		for s := range a.subs {
			subs = append(subs, s)
		}
		sort.Slice(subs, func(i, j int) bool {
			if a.subs[subs[i]] != a.subs[subs[j]] {
				return a.subs[subs[i]] > a.subs[subs[j]]
			}
			return subs[i] < subs[j]
		})
		if len(subs) > userAccessGroupValues {
			subs = subs[:userAccessGroupValues]
		}
		a.g.Values = subs
		out = append(out, a.g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].LastSeen != out[j].LastSeen {
			return out[i].LastSeen > out[j].LastSeen
		}
		return out[i].Value < out[j].Value
	})
	if len(out) > userAccessDetailGroups {
		out = out[:userAccessDetailGroups]
	}
	return out
}

// userAccessDeleteUser 删用户时一并清掉他的来源记录（含还没落库的）。
func userAccessForgetPending(userID int) {
	userAccessPendingMu.Lock()
	defer userAccessPendingMu.Unlock()
	for k, p := range userAccessPending {
		if p.UserID == userID {
			delete(userAccessPending, k)
		}
	}
}
