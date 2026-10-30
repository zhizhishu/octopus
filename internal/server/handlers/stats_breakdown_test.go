package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/conf"
	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/server/auth"
	"github.com/zhizhishu/octopus/internal/server/router"
)

// 仪表盘的两个新接口走真实路由（router.RegisterAll，带生产上同一套 Auth + AdminOnly），
// 用一份手算过的日志去打，逐维度、逐时间段核对合计。
//
// 统计时区设成 Asia/Shanghai（和机器时区无关），日界线按它切。

type dashFixture struct {
	engine     *gin.Engine
	adminJWT   string
	userJWT    string
	loc        *time.Location
	today      time.Time
	alice      model.User
	bob        model.User
	carol      model.User
	keyA1      model.APIKey
	keyA2      model.APIKey
	keyB       model.APIKey
	chanA      model.Channel
	chanB      model.Channel
	planVIP    model.AccessPlan
	deletedKey int
}

func (f dashFixture) day(offset int) string {
	return f.today.AddDate(0, 0, offset).Format("2006-01-02")
}

func (f dashFixture) at(dayOffset int, d time.Duration) int64 {
	return f.today.AddDate(0, 0, dayOffset).Add(d).Unix()
}

func setupDashFixture(t *testing.T) dashFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	t.Setenv(strings.ToUpper(conf.APP_NAME)+"_ADMIN_TOKEN", "")
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "dash_test.db"), false); err != nil {
		t.Fatalf("init db: %v", err)
	}
	ctx := context.Background()
	t.Cleanup(func() {
		// 内存日志队列是进程级的：清掉，别漏给同包下一个测试。
		_ = op.RelayLogClear(ctx, nil)
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
	})
	if err := op.InitCache(); err != nil {
		t.Fatalf("init cache: %v", err)
	}
	if err := op.RelayLogClear(ctx, nil); err != nil {
		t.Fatalf("clear logs: %v", err)
	}
	if err := op.SettingSetString(model.SettingKeyStatsTimezone, "Asia/Shanghai"); err != nil {
		t.Fatalf("set tz: %v", err)
	}
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load tz: %v", err)
	}
	now := time.Now().In(loc)

	f := dashFixture{loc: loc, today: time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)}
	mkUser := func(name string, role model.UserRole) model.User {
		u, err := op.UserCreate(model.UserCreateRequest{Username: name, Password: "password-" + name, Role: role, Status: model.UserStatusActive}, ctx)
		if err != nil {
			t.Fatalf("create user %s: %v", name, err)
		}
		return u
	}
	admin := mkUser("dash-admin", model.UserRoleAdmin)
	f.alice = mkUser("alice", model.UserRoleUser)
	f.bob = mkUser("bob", model.UserRoleUser)
	f.carol = mkUser("carol", model.UserRoleUser)

	mkKey := func(owner model.User, name string) model.APIKey {
		k := model.APIKey{UserID: owner.ID, Name: name, APIKey: "sk-octopus-test-" + name, Enabled: true}
		if err := op.APIKeyCreate(&k, ctx); err != nil {
			t.Fatalf("create key %s: %v", name, err)
		}
		return k
	}
	f.keyA1 = mkKey(f.alice, "alice-laptop")
	f.keyA2 = mkKey(f.alice, "alice-ci")
	f.keyB = mkKey(f.bob, "bob-main")
	f.deletedKey = 9090

	mkChan := func(name string) model.Channel {
		ch := model.Channel{Name: name, Enabled: true, Keys: []model.ChannelKey{{Enabled: true, ChannelKey: "sk-upstream-test-" + name}}}
		if err := op.ChannelCreate(&ch, ctx); err != nil {
			t.Fatalf("create channel %s: %v", name, err)
		}
		return ch
	}
	f.chanA = mkChan("chan-a")
	f.chanB = mkChan("chan-b")

	f.planVIP = model.AccessPlan{Slug: "dash-team", DisplayName: "Team Plan", Enabled: true}
	if err := op.AccessPlanCreate(&f.planVIP, ctx); err != nil {
		t.Fatalf("create plan: %v", err)
	}

	f.engine = gin.New()
	if err := router.RegisterAll(f.engine); err != nil {
		t.Fatalf("register routes: %v", err)
	}
	f.adminJWT = dashJWT(t, admin)
	f.userJWT = dashJWT(t, f.alice)

	seedDashLogs(t, ctx, f)
	return f
}

func dashJWT(t *testing.T, u model.User) string {
	t.Helper()
	token, _, err := auth.GenerateJWTToken(u, 0)
	if err != nil {
		t.Fatalf("jwt: %v", err)
	}
	return token
}

// seedDashLogs 写入下面这张表。金额都用二进制能精确表示的数，合计可以直接 == 比。
//
//	#   时间              用户   Key       渠道  方案        模型          费用    备注
//	1   今天 01:00        alice  A1        A     Team(真)    gpt-x         1.5
//	2   今天 02:00        alice  A2        B     Team(真)    claude-y      0.5    cost=0，按 final_* 合计；失败；severity=0 走旧判据
//	3   今天 03:00        bob    B         A     svip(已删)  " gpt-x "     2.0    还在内存队列里，没落库
//	4   今天 04:00        bob    B         A     -           gpt-x         9.0    模型测试 stats_counted=false，不算
//	5   昨天 05:00        carol  -         B     -           (actual)gem   0.25
//	6   3 天前 10:00      alice  A1        A     Team        gpt-x         4.0
//	7   100 天前          alice  A1        A     -           gpt-x         50     永远在 90 天外
//	8   今天 00:00:00     carol  已删 Key  B     -           edge          0.125  正好在今天的起点
//	9   昨天 23:59:59     carol  已删 Key  B     -           edge          0.0625 正好在今天之前一秒
//	10  今天 05:00        alice  A1        A     Team        gpt-x         0      成功，severity=0 走旧判据
func seedDashLogs(t *testing.T, ctx context.Context, f dashFixture) {
	t.Helper()
	conn := db.GetDB().WithContext(ctx)
	dbLogs := []model.RelayLog{
		{ID: 1, Time: f.at(0, time.Hour), UserID: f.alice.ID, UserName: "alice", APIKeyID: f.keyA1.ID, RequestAPIKeyName: "alice-laptop", ChannelId: f.chanA.ID, ChannelName: "chan-a", AccessPlanID: f.planVIP.ID, AccessPlanName: "old-team-name", RequestModelName: "gpt-x", Cost: 1.5, InputTokens: 100, OutputTokens: 10, CacheHitTokens: 40, CacheInputTokens: 100, UseTime: 1000},
		{ID: 2, Time: f.at(0, 2*time.Hour), UserID: f.alice.ID, UserName: "alice", APIKeyID: f.keyA2.ID, RequestAPIKeyName: "alice-ci", ChannelId: f.chanB.ID, ChannelName: "chan-b", AccessPlanID: f.planVIP.ID, AccessPlanName: "old-team-name", RequestModelName: "claude-y", FinalInputCost: 0.25, FinalOutputCost: 0.25, InputTokens: 50, OutputTokens: 5, UseTime: 3000, Error: "boom", ErrorStatus: 502},
		{ID: 4, Time: f.at(0, 4*time.Hour), UserID: f.bob.ID, UserName: "bob", APIKeyID: f.keyB.ID, ChannelId: f.chanA.ID, RequestModelName: "gpt-x", Cost: 9, InputTokens: 999, ErrorStrategy: "model_test;billable=false;stats_counted=false"},
		{ID: 5, Time: f.at(-1, 5*time.Hour), UserID: f.carol.ID, UserName: "carol", ChannelId: f.chanB.ID, ChannelName: "chan-b", ActualModelName: "gemini-z", Cost: 0.25, InputTokens: 20, OutputTokens: 2, UseTime: 500},
		{ID: 6, Time: f.at(-3, 10*time.Hour), UserID: f.alice.ID, UserName: "alice", APIKeyID: f.keyA1.ID, ChannelId: f.chanA.ID, AccessPlanID: f.planVIP.ID, RequestModelName: "gpt-x", Cost: 4, InputTokens: 400, OutputTokens: 40, UseTime: 2000},
		{ID: 7, Time: f.at(-100, time.Hour), UserID: f.alice.ID, UserName: "alice", APIKeyID: f.keyA1.ID, ChannelId: f.chanA.ID, RequestModelName: "gpt-x", Cost: 50, InputTokens: 5000},
		{ID: 8, Time: f.at(0, 0), UserID: f.carol.ID, UserName: "carol", APIKeyID: f.deletedKey, RequestAPIKeyName: "gone-key", ChannelId: f.chanB.ID, RequestModelName: "edge", Cost: 0.125, InputTokens: 8, UseTime: 100},
		{ID: 9, Time: f.at(0, 0) - 1, UserID: f.carol.ID, UserName: "carol", APIKeyID: f.deletedKey, RequestAPIKeyName: "gone-key", ChannelId: f.chanB.ID, RequestModelName: "edge", Cost: 0.0625, InputTokens: 4, UseTime: 100},
		{ID: 10, Time: f.at(0, 5*time.Hour), UserID: f.alice.ID, UserName: "alice", APIKeyID: f.keyA1.ID, ChannelId: f.chanA.ID, AccessPlanID: f.planVIP.ID, RequestModelName: "gpt-x", InputTokens: 10, OutputTokens: 1, UseTime: 400},
	}
	if err := conn.Create(&dbLogs).Error; err != nil {
		t.Fatalf("seed logs: %v", err)
	}
	// 2 和 10 模拟回填前的旧行：severity=0，成功/失败要靠 error 列判。
	if err := conn.Exec("UPDATE relay_logs SET severity = 0 WHERE id IN (2, 10)").Error; err != nil {
		t.Fatalf("unset severity: %v", err)
	}
	if err := op.SettingSetString(model.SettingKeyRelayLogKeepEnabled, "true"); err != nil {
		t.Fatalf("enable log keep: %v", err)
	}
	// 3 只进内存队列（测试里没有起后台 flusher），接口必须把它也算进去。
	if err := op.RelayLogAdd(ctx, model.RelayLog{Time: f.at(0, 3*time.Hour), UserID: f.bob.ID, UserName: "bob", APIKeyID: f.keyB.ID, RequestAPIKeyName: "bob-main", ChannelId: f.chanA.ID, ChannelName: "chan-a", AccessPlanID: 777, AccessPlanName: "svip", RequestModelName: " gpt-x ", Cost: 2, InputTokens: 200, OutputTokens: 20, UseTime: 600}); err != nil {
		t.Fatalf("add pending log: %v", err)
	}
	var stored int64
	conn.Model(&model.RelayLog{}).Count(&stored)
	if stored != int64(len(dbLogs)) {
		t.Fatalf("pending log leaked into DB before the test: %d rows", stored)
	}
}

type dashEnvelope[T any] struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    T      `json:"data"`
}

func dashGet(f dashFixture, bearer, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	f.engine.ServeHTTP(rec, req)
	return rec
}

func dashBreakdown(t *testing.T, f dashFixture, from, to, by string, extra string) model.StatsBreakdown {
	t.Helper()
	rec := dashGet(f, f.adminJWT, fmt.Sprintf("/api/v1/stats/breakdown?from=%s&to=%s&by=%s%s", from, to, by, extra))
	if rec.Code != http.StatusOK {
		t.Fatalf("breakdown %s %s..%s: status %d body %s", by, from, to, rec.Code, rec.Body.String())
	}
	var env dashEnvelope[model.StatsBreakdown]
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return env.Data
}

type dashWant struct {
	name    string
	cost    float64
	success int64
	failed  int64
	input   int64
}

func dashCheck(t *testing.T, label string, got model.StatsBreakdown, want []dashWant, total float64) {
	t.Helper()
	if got.Total.TotalCost != total {
		t.Errorf("%s: total cost = %v, want %v", label, got.Total.TotalCost, total)
	}
	var itemSum float64
	for _, item := range got.Items {
		itemSum += item.TotalCost
	}
	if got.Others != nil {
		itemSum += got.Others.TotalCost
	}
	if math.Abs(itemSum-got.Total.TotalCost) > 1e-9 {
		t.Errorf("%s: items+others = %v but total = %v", label, itemSum, got.Total.TotalCost)
	}
	if len(got.Items) != len(want) {
		t.Fatalf("%s: %d items, want %d: %+v", label, len(got.Items), len(want), got.Items)
	}
	for i, w := range want {
		item := got.Items[i]
		if item.Name != w.name || item.TotalCost != w.cost || item.RequestSuccess != w.success || item.RequestFailed != w.failed || (w.input >= 0 && item.InputToken != w.input) {
			t.Errorf("%s[%d]: got {%s cost=%v ok=%d fail=%d in=%d}, want {%s cost=%v ok=%d fail=%d in=%d}",
				label, i, item.Name, item.TotalCost, item.RequestSuccess, item.RequestFailed, item.InputToken,
				w.name, w.cost, w.success, w.failed, w.input)
		}
	}
}

func TestStatsBreakdownSumsPerDimensionToday(t *testing.T) {
	f := setupDashFixture(t)
	today := f.day(0)

	dashCheck(t, "model/today", dashBreakdown(t, f, today, today, "model", ""), []dashWant{
		{name: "gpt-x", cost: 3.5, success: 3, input: 310},
		{name: "claude-y", cost: 0.5, failed: 1, input: 50},
		{name: "edge", cost: 0.125, success: 1, input: 8},
	}, 4.125)

	users := dashBreakdown(t, f, today, today, "user", "")
	dashCheck(t, "user/today", users, []dashWant{
		{name: "alice", cost: 2.0, success: 2, failed: 1, input: 160},
		{name: "bob", cost: 2.0, success: 1, input: 200},
		{name: "carol", cost: 0.125, success: 1, input: 8},
	}, 4.125)
	if users.ActiveUsers == nil || *users.ActiveUsers != 3 {
		t.Errorf("active users today = %v, want 3", users.ActiveUsers)
	}

	keys := dashBreakdown(t, f, today, today, "apikey", "")
	dashCheck(t, "apikey/today", keys, []dashWant{
		{name: "bob-main", cost: 2.0, success: 1, input: 200},
		{name: "alice-laptop", cost: 1.5, success: 2, input: 110},
		{name: "alice-ci", cost: 0.5, failed: 1, input: 50},
		{name: "gone-key", cost: 0.125, success: 1, input: 8},
	}, 4.125)
	if keys.Items[0].UserName != "bob" || keys.Items[1].UserName != "alice" || keys.Items[1].UserID != f.alice.ID {
		t.Errorf("api key owners wrong: %+v", keys.Items)
	}
	if !keys.Items[3].Missing || keys.Items[3].ID != f.deletedKey || keys.Items[3].UserID != f.carol.ID {
		t.Errorf("deleted key must keep its log name and be marked missing: %+v", keys.Items[3])
	}

	dashCheck(t, "channel/today", dashBreakdown(t, f, today, today, "channel", ""), []dashWant{
		{name: "chan-a", cost: 3.5, success: 3, input: 310},
		{name: "chan-b", cost: 0.625, success: 1, failed: 1, input: 58},
	}, 4.125)

	plans := dashBreakdown(t, f, today, today, "plan", "")
	dashCheck(t, "plan/today", plans, []dashWant{
		// Team：现名覆盖日志里的旧名；svip 已删，留日志名；0 = 没方案。
		{name: "Team Plan", cost: 2.0, success: 2, failed: 1, input: 160},
		{name: "svip", cost: 2.0, success: 1, input: 200},
		{name: "", cost: 0.125, success: 1, input: 8},
	}, 4.125)
	if !plans.Items[1].Missing || plans.Items[0].Missing || plans.Items[2].ID != 0 {
		t.Errorf("plan missing flags wrong: %+v", plans.Items)
	}
	if plans.Start != f.at(0, 0) || plans.End != f.at(1, 0) || plans.Days != 1 {
		t.Errorf("range echo: start=%d end=%d days=%d", plans.Start, plans.End, plans.Days)
	}
	if plans.OldestLogTime != f.at(-100, time.Hour) {
		t.Errorf("oldest log time = %d, want %d", plans.OldestLogTime, f.at(-100, time.Hour))
	}
}

func TestStatsBreakdownSumsPerRange(t *testing.T) {
	f := setupDashFixture(t)

	// 只看昨天：卡在今天起点前一秒的 9 号算昨天，00:00:00 的 8 号不算。
	yesterday := f.day(-1)
	dashCheck(t, "model/yesterday", dashBreakdown(t, f, yesterday, yesterday, "model", ""), []dashWant{
		{name: "gemini-z", cost: 0.25, success: 1, input: 20},
		{name: "edge", cost: 0.0625, success: 1, input: 4},
	}, 0.3125)

	// 近 7 天。
	week := dashBreakdown(t, f, f.day(-6), f.day(0), "user", "")
	dashCheck(t, "user/7d", week, []dashWant{
		{name: "alice", cost: 6.0, success: 3, failed: 1, input: 560},
		{name: "bob", cost: 2.0, success: 1, input: 200},
		{name: "carol", cost: 0.4375, success: 3, input: 32},
	}, 8.4375)
	if *week.ActiveUsers != 3 {
		t.Errorf("active users 7d = %d", *week.ActiveUsers)
	}

	// limit 只截表格，合计和「其余」照算。
	limited := dashBreakdown(t, f, f.day(-6), f.day(0), "user", "&limit=1")
	if len(limited.Items) != 1 || limited.ItemCount != 3 || limited.OthersCount != 2 || limited.Others == nil || limited.Others.TotalCost != 2.4375 || *limited.ActiveUsers != 3 {
		t.Errorf("limit=1: items=%d count=%d others=%+v n=%d active=%d", len(limited.Items), limited.ItemCount, limited.Others, limited.OthersCount, *limited.ActiveUsers)
	}

	// 90 天（上限）也取不到 100 天前那条。
	dashCheck(t, "user/90d", dashBreakdown(t, f, f.day(-89), f.day(0), "user", ""), []dashWant{
		{name: "alice", cost: 6.0, success: 3, failed: 1, input: 560},
		{name: "bob", cost: 2.0, success: 1, input: 200},
		{name: "carol", cost: 0.4375, success: 3, input: 32},
	}, 8.4375)

	// 3 天前那一天单看。
	dashCheck(t, "channel/-3d", dashBreakdown(t, f, f.day(-3), f.day(-3), "channel", ""), []dashWant{
		{name: "chan-a", cost: 4, success: 1, input: 400},
	}, 4)

	// 空的一天：合计 0，没有行。
	empty := dashBreakdown(t, f, f.day(-20), f.day(-20), "model", "")
	if len(empty.Items) != 0 || empty.Total.TotalCost != 0 || empty.Total.Requests() != 0 {
		t.Errorf("empty day: %+v", empty)
	}
}

func TestStatsBreakdownHourly(t *testing.T) {
	f := setupDashFixture(t)
	today := dashBreakdown(t, f, f.day(0), f.day(0), "hour", "")
	if len(today.Items) != 24 {
		t.Fatalf("today hourly: %d buckets", len(today.Items))
	}
	wantCost := map[int]float64{0: 0.125, 1: 1.5, 2: 0.5, 3: 2.0, 5: 0}
	wantReq := map[int]int64{0: 1, 1: 1, 2: 1, 3: 1, 5: 1}
	var sum float64
	for i, item := range today.Items {
		if item.Start != f.at(0, time.Duration(i)*time.Hour) || item.Name != fmt.Sprintf("%02d:00", i) {
			t.Errorf("bucket %d: start=%d name=%s", i, item.Start, item.Name)
		}
		if item.TotalCost != wantCost[i] || item.Requests() != wantReq[i] {
			t.Errorf("bucket %d: cost=%v req=%d, want cost=%v req=%d", i, item.TotalCost, item.Requests(), wantCost[i], wantReq[i])
		}
		sum += item.TotalCost
	}
	if sum != 4.125 || today.Total.TotalCost != 4.125 {
		t.Errorf("hourly sum %v total %v", sum, today.Total.TotalCost)
	}

	yesterday := dashBreakdown(t, f, f.day(-1), f.day(-1), "hour", "")
	if yesterday.Items[5].TotalCost != 0.25 || yesterday.Items[23].TotalCost != 0.0625 || yesterday.Total.TotalCost != 0.3125 {
		t.Errorf("yesterday hourly: h5=%v h23=%v total=%v", yesterday.Items[5].TotalCost, yesterday.Items[23].TotalCost, yesterday.Total.TotalCost)
	}
}

func TestStatsUserTrendDailyPoints(t *testing.T) {
	f := setupDashFixture(t)
	rec := dashGet(f, f.adminJWT, fmt.Sprintf("/api/v1/stats/user-trend?from=%s&to=%s&top=2", f.day(-6), f.day(0)))
	if rec.Code != http.StatusOK {
		t.Fatalf("user-trend: %d %s", rec.Code, rec.Body.String())
	}
	var env dashEnvelope[model.StatsUserTrend]
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	trend := env.Data
	if len(trend.Dates) != 7 || trend.Dates[0] != f.day(-6) || trend.Dates[6] != f.day(0) {
		t.Fatalf("dates: %v", trend.Dates)
	}
	if len(trend.Users) != 2 || trend.Users[0].Name != "alice" || trend.Users[1].Name != "bob" {
		t.Fatalf("top users: %+v", trend.Users)
	}
	alice := trend.Users[0]
	want := []float64{0, 0, 0, 4, 0, 0, 2}
	for i, p := range alice.Points {
		if p.TotalCost != want[i] {
			t.Errorf("alice day %s cost=%v want %v", trend.Dates[i], p.TotalCost, want[i])
		}
	}
	if alice.Total.TotalCost != 6 || alice.Total.RequestFailed != 1 {
		t.Errorf("alice total: %+v", alice.Total)
	}
	bob := trend.Users[1]
	if bob.Points[6].TotalCost != 2 || bob.Total.TotalCost != 2 {
		t.Errorf("bob: %+v", bob)
	}
}

func TestStatsBreakdownRangeBounds(t *testing.T) {
	f := setupDashFixture(t)
	bad := []struct {
		name, query string
	}{
		{"missing from", "to=" + f.day(0) + "&by=model"},
		{"missing to", "from=" + f.day(0) + "&by=model"},
		{"bad format", "from=2026/10/01&to=" + f.day(0) + "&by=model"},
		{"unix seconds rejected", fmt.Sprintf("from=%d&to=%d&by=model", f.at(-1, 0), f.at(0, 0))},
		{"to after today", "from=" + f.day(0) + "&to=" + f.day(1) + "&by=model"},
		{"from after to", "from=" + f.day(0) + "&to=" + f.day(-1) + "&by=model"},
		{"91 days", "from=" + f.day(-90) + "&to=" + f.day(0) + "&by=model"},
		{"hour over two days", "from=" + f.day(-1) + "&to=" + f.day(0) + "&by=hour"},
		{"unknown dimension", "from=" + f.day(0) + "&to=" + f.day(0) + "&by=request_content"},
		{"empty dimension", "from=" + f.day(0) + "&to=" + f.day(0)},
		{"negative limit", "from=" + f.day(0) + "&to=" + f.day(0) + "&by=model&limit=-1"},
		{"too early", "from=1999-12-31&to=2000-01-01&by=model"},
	}
	for _, tc := range bad {
		rec := dashGet(f, f.adminJWT, "/api/v1/stats/breakdown?"+tc.query)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400 (body %s)", tc.name, rec.Code, rec.Body.String())
		}
	}
	for _, tc := range []string{
		"from=" + f.day(-90) + "&to=" + f.day(0),
		"from=" + f.day(0) + "&to=" + f.day(1),
	} {
		if rec := dashGet(f, f.adminJWT, "/api/v1/stats/user-trend?"+tc); rec.Code != http.StatusBadRequest {
			t.Errorf("user-trend %s: status %d, want 400", tc, rec.Code)
		}
	}
	// 正好 90 天、以及 20060102 写法都放行。
	if rec := dashGet(f, f.adminJWT, "/api/v1/stats/breakdown?from="+f.day(-89)+"&to="+f.day(0)+"&by=model"); rec.Code != http.StatusOK {
		t.Errorf("90 days: %d %s", rec.Code, rec.Body.String())
	}
	compact := f.today.Format("20060102")
	if rec := dashGet(f, f.adminJWT, "/api/v1/stats/breakdown?from="+compact+"&to="+compact+"&by=hour"); rec.Code != http.StatusOK {
		t.Errorf("compact date: %d %s", rec.Code, rec.Body.String())
	}
}

func TestStatsBreakdownIsAdminOnly(t *testing.T) {
	f := setupDashFixture(t)
	targets := []string{
		"/api/v1/stats/breakdown?from=" + f.day(0) + "&to=" + f.day(0) + "&by=user",
		"/api/v1/stats/user-trend?from=" + f.day(-6) + "&to=" + f.day(0),
	}
	for _, target := range targets {
		if rec := dashGet(f, "", target); rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "alice") {
			t.Errorf("%s without login: %d %s", target, rec.Code, rec.Body.String())
		}
		if rec := dashGet(f, "not-a-real-token", target); rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "alice") {
			t.Errorf("%s with junk token: %d %s", target, rec.Code, rec.Body.String())
		}
		rec := dashGet(f, f.userJWT, target)
		if rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), "bob") {
			t.Errorf("%s as normal user: %d %s, want 403 with no data", target, rec.Code, rec.Body.String())
		}
		if rec := dashGet(f, f.adminJWT, target); rec.Code != http.StatusOK {
			t.Errorf("%s as admin: %d %s", target, rec.Code, rec.Body.String())
		}
	}
}
