package op

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
)

func accessRow(ip, ua string, key int, first, last int64) model.UserAccessSeen {
	return model.UserAccessSeen{UserID: 1, APIKeyID: key, IP: ip, UserAgent: ua, FirstSeen: first, LastSeen: last, Requests: 1}
}

func alertKinds(alerts []model.UserAccessAlert) []string {
	out := make([]string, 0, len(alerts))
	for _, a := range alerts {
		out = append(out, a.Kind)
	}
	return out
}

func resetUserAccessPending(t *testing.T) {
	t.Helper()
	userAccessPendingMu.Lock()
	userAccessPending = make(map[string]*model.UserAccessSeen)
	userAccessPendingDropped = 0
	userAccessPendingMu.Unlock()
	t.Cleanup(func() {
		userAccessPendingMu.Lock()
		userAccessPending = make(map[string]*model.UserAccessSeen)
		userAccessPendingMu.Unlock()
	})
}

func accessPreset(t *testing.T, level string) model.UserAccessAlertConfig {
	t.Helper()
	cfg, ok := model.UserAccessAlertPreset(level)
	if !ok {
		t.Fatalf("no preset %q", level)
	}
	return cfg
}

// 异常切换规则（等级「标准」：窗口 1 小时、3 个 IP 算多）。逐条钉住「报 / 不报」。
func TestUserAccessAlertRule(t *testing.T) {
	const now = int64(1_800_000_000)
	day := int64(86400)
	cc := "claude-cli/2.0.1 (external, cli)"
	w := accessPreset(t, model.UserAccessAlertLevelStandard)
	cases := []struct {
		name string
		rows []model.UserAccessSeen
		want []string
	}{
		{"one ip one client", []model.UserAccessSeen{accessRow("203.0.113.1", cc, 1, now-day, now)}, []string{}},
		{"two known ips used in the same hour", []model.UserAccessSeen{
			accessRow("203.0.113.1", cc, 1, now-3*day, now-60),
			accessRow("198.51.100.2", cc, 1, now-2*day, now),
		}, []string{}},
		{"new ip while old ip still active", []model.UserAccessSeen{
			accessRow("203.0.113.1", cc, 1, now-3*day, now-120),
			accessRow("198.51.100.2", cc, 1, now-300, now),
		}, []string{model.UserAccessAlertNewIP}},
		{"clean move: old ip quiet for over an hour", []model.UserAccessSeen{
			accessRow("203.0.113.1", cc, 1, now-3*day, now-2*3600),
			accessRow("198.51.100.2", cc, 1, now-300, now),
		}, []string{}},
		{"three ips in an hour reports many_ips only", []model.UserAccessSeen{
			accessRow("203.0.113.1", cc, 1, now-3*day, now-100),
			accessRow("198.51.100.2", cc, 1, now-200, now-50),
			accessRow("192.0.2.3", cc, 2, now-100, now),
		}, []string{model.UserAccessAlertManyIPs}},
		{"ipv6 privacy suffix change in same /64 is one place", []model.UserAccessSeen{
			accessRow("2001:db8:1:2::aaaa", cc, 1, now-3*day, now-60),
			accessRow("2001:db8:1:2:1111:2222:3333:4444", cc, 1, now-300, now),
		}, []string{}},
		{"client version upgrade is not a switch", []model.UserAccessSeen{
			accessRow("203.0.113.1", "claude-cli/2.0.1", 1, now-3*day, now-60),
			accessRow("203.0.113.1", "claude-cli/2.0.5", 1, now-300, now),
		}, []string{}},
		{"new client while old client active", []model.UserAccessSeen{
			accessRow("203.0.113.1", cc, 1, now-3*day, now-60),
			accessRow("203.0.113.1", "codex_cli_rs/0.50.0", 1, now-300, now),
		}, []string{model.UserAccessAlertNewClient}},
		{"same ip seen before with another ua is not a new ip", []model.UserAccessSeen{
			accessRow("203.0.113.1", "curl/8", 1, now-10*day, now-9*day),
			accessRow("203.0.113.1", cc, 1, now-300, now),
			accessRow("198.51.100.2", cc, 1, now-3*day, now-30),
		}, []string{}},
		{"ip and client switch together", []model.UserAccessSeen{
			accessRow("203.0.113.1", cc, 1, now-3*day, now-60),
			accessRow("198.51.100.2", "CherryStudio/1.5", 2, now-120, now),
		}, []string{model.UserAccessAlertNewIP, model.UserAccessAlertNewClient}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := alertKinds(userAccessAlerts(tc.rows, now, w))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("alerts = %v, want %v", got, tc.want)
			}
		})
	}

	rows := []model.UserAccessSeen{
		accessRow("203.0.113.1", cc, 1, now-3*day, now-120),
		accessRow("198.51.100.2", cc, 1, now-300, now),
	}
	a := userAccessAlerts(rows, now, w)[0]
	if a.Value != "198.51.100.2" || !reflect.DeepEqual(a.Others, []string{"203.0.113.1"}) || a.At != now-300 || a.WindowSec != 3600 || a.Count != 2 {
		t.Fatalf("bad new_ip detail %#v", a)
	}
	// 过了窗口没有新请求，提示自然消失。
	if got := userAccessAlerts(rows, now+2*3600, w); len(got) != 0 {
		t.Fatalf("alert must expire after the window, got %v", got)
	}
}

// 每个风控等级在同一组场景下报什么（预设阈值见 model.UserAccessAlertPreset）。
func TestUserAccessAlertLevels(t *testing.T) {
	const now = int64(1_800_000_000)
	day := int64(86400)
	cc := "claude-cli/2.0.1"
	manyIPs := func(prefix string, n int) []model.UserAccessSeen {
		rows := make([]model.UserAccessSeen, 0, n)
		for i := 0; i < n; i++ {
			rows = append(rows, accessRow(fmt.Sprintf(prefix, i+1), cc, 1, now-3*day, now-int64(i*60)))
		}
		return rows
	}
	scenarios := map[string][]model.UserAccessSeen{
		// 冒出一个从没见过的 IP，旧 IP 2 分钟前还在用。
		"new ip": {accessRow("203.0.113.1", cc, 1, now-3*day, now-120), accessRow("198.51.100.2", cc, 1, now-300, now)},
		// 两个都是老 IP，一小时内来回换（家里 / 公司）。
		"two known ips": {accessRow("203.0.113.1", cc, 1, now-3*day, now-60), accessRow("198.51.100.2", cc, 1, now-2*day, now)},
		// 一小时内 3 个老 IP。
		"3 ips": manyIPs("192.0.%d.10", 3),
		// 一小时内 11 个不同网段的老 IP。
		"11 ips": manyIPs("192.0.%d.10", 11),
		// 一小时内 11 个 IP，都在同一个 /24（手机 / CGNAT 换地址）。
		"11 ips same /24": manyIPs("203.0.113.%d", 11),
		// 同一个 IP 换了个从没见过的客户端，旧客户端 1 分钟前还在用。
		"new client": {accessRow("203.0.113.1", cc, 1, now-3*day, now-60), accessRow("203.0.113.1", "codex_cli_rs/0.50.0", 1, now-300, now)},
	}
	many, newIP, newClient := model.UserAccessAlertManyIPs, model.UserAccessAlertNewIP, model.UserAccessAlertNewClient
	want := map[string]map[string][]string{
		model.UserAccessAlertLevelOff: {
			"new ip": {}, "two known ips": {}, "3 ips": {}, "11 ips": {}, "11 ips same /24": {}, "new client": {},
		},
		model.UserAccessAlertLevelLoose: {
			"new ip": {}, "two known ips": {}, "3 ips": {}, "11 ips": {many}, "11 ips same /24": {}, "new client": {},
		},
		model.UserAccessAlertLevelStandard: {
			"new ip": {newIP}, "two known ips": {}, "3 ips": {many}, "11 ips": {many}, "11 ips same /24": {many}, "new client": {newClient},
		},
		model.UserAccessAlertLevelStrict: {
			"new ip": {many}, "two known ips": {many}, "3 ips": {many}, "11 ips": {many}, "11 ips same /24": {many}, "new client": {newClient},
		},
	}
	for _, level := range model.UserAccessAlertPresetLevels {
		cfg := accessPreset(t, level)
		for name, rows := range scenarios {
			got := alertKinds(userAccessAlerts(rows, now, cfg))
			if !reflect.DeepEqual(got, want[level][name]) {
				t.Errorf("%s / %s: alerts = %v, want %v", level, name, got, want[level][name])
			}
		}
	}
	// 宽松的「10 个以内不报」：10 个不同网段的 IP 不报，第 11 个才报。
	if got := alertKinds(userAccessAlerts(manyIPs("192.0.%d.10", 10), now, accessPreset(t, model.UserAccessAlertLevelLoose))); len(got) != 0 {
		t.Fatalf("loose must not flag 10 ips, got %v", got)
	}

	// 自定义：10 分钟窗口、不看 IP 数、只报新 IP、IPv4 同 /24 算一个。
	custom := model.UserAccessAlertConfig{Level: model.UserAccessAlertLevelCustom, WindowSec: 600, ManyIPs: 0, NewIP: true, NewClient: false, IPv4Prefix: 24}
	for _, tc := range []struct {
		name string
		rows []model.UserAccessSeen
		want []string
	}{
		{"new ip in another /24", []model.UserAccessSeen{accessRow("203.0.113.1", cc, 1, now-3*day, now-120), accessRow("198.51.100.2", cc, 1, now-300, now)}, []string{newIP}},
		{"new ip in the same /24", []model.UserAccessSeen{accessRow("203.0.113.1", cc, 1, now-3*day, now-120), accessRow("203.0.113.77", cc, 1, now-300, now)}, []string{}},
		{"old ip quiet for 20 minutes", []model.UserAccessSeen{accessRow("203.0.113.1", cc, 1, now-3*day, now-1200), accessRow("198.51.100.2", cc, 1, now-300, now)}, []string{}},
		{"new client is off", scenarios["new client"], []string{}},
		{"11 ips not counted", scenarios["11 ips"], []string{}},
	} {
		if got := alertKinds(userAccessAlerts(tc.rows, now, custom)); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("custom / %s: alerts = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// 设置 → 阈值：预设直接用；自定义读五个数，坏值退回「标准」；认不出的等级按「标准」。
func TestUserAccessAlertConfigFromSettings(t *testing.T) {
	std := accessPreset(t, model.UserAccessAlertLevelStandard)
	values := map[model.SettingKey]string{
		model.SettingKeyUserAccessAlertWindowMin:  "30",
		model.SettingKeyUserAccessAlertManyIPs:    "5",
		model.SettingKeyUserAccessAlertNewIP:      "false",
		model.SettingKeyUserAccessAlertNewClient:  "true",
		model.SettingKeyUserAccessAlertIPv4Prefix: "24",
	}
	get := func(k model.SettingKey) (string, bool) { v, ok := values[k]; return v, ok }
	for _, level := range model.UserAccessAlertPresetLevels {
		if got := userAccessAlertConfigFrom(level, get); got != accessPreset(t, level) {
			t.Fatalf("%s = %#v", level, got)
		}
	}
	if got := userAccessAlertConfigFrom("", get); got != std {
		t.Fatalf("empty level = %#v, want standard", got)
	}
	if got := userAccessAlertConfigFrom("paranoid", get); got != std {
		t.Fatalf("unknown level = %#v, want standard", got)
	}
	want := model.UserAccessAlertConfig{Level: model.UserAccessAlertLevelCustom, WindowSec: 1800, ManyIPs: 5, NewIP: false, NewClient: true, IPv4Prefix: 24}
	if got := userAccessAlertConfigFrom(model.UserAccessAlertLevelCustom, get); got != want {
		t.Fatalf("custom = %#v, want %#v", got, want)
	}
	values[model.SettingKeyUserAccessAlertWindowMin] = "1"
	values[model.SettingKeyUserAccessAlertManyIPs] = "1"
	values[model.SettingKeyUserAccessAlertIPv4Prefix] = "16"
	delete(values, model.SettingKeyUserAccessAlertNewIP)
	want = model.UserAccessAlertConfig{Level: model.UserAccessAlertLevelCustom, WindowSec: std.WindowSec, ManyIPs: std.ManyIPs, NewIP: std.NewIP, NewClient: true, IPv4Prefix: std.IPv4Prefix}
	if got := userAccessAlertConfigFrom(model.UserAccessAlertLevelCustom, get); got != want {
		t.Fatalf("custom with bad values = %#v, want %#v", got, want)
	}
}

func TestUserAccessNormalizers(t *testing.T) {
	for ua, want := range map[string]string{
		"claude-cli/2.0.1 (external, cli)": "claude-cli",
		"codex_cli_rs/0.50.0 (Mac OS)":     "codex_cli_rs",
		"Mozilla/5.0 (X11)":                "mozilla",
		"CherryStudio":                     "cherrystudio",
		"  ":                               "",
		"OpenAI/Python 1.40.0":             "openai",
	} {
		if got := userAccessClientFamily(ua); got != want {
			t.Fatalf("client family %q = %q, want %q", ua, got, want)
		}
	}
	for _, tc := range []struct {
		ip     string
		prefix int
		want   string
	}{
		{"203.0.113.9", 32, "203.0.113.9"},
		{"::ffff:203.0.113.9", 32, "203.0.113.9"},
		{"203.0.113.9", 24, "203.0.113.0/24"},
		{"::ffff:203.0.113.9", 24, "203.0.113.0/24"},
		{"2001:db8:1:2:3:4:5:6", 32, "2001:db8:1:2::/64"},
		{"2001:db8:1:2:3:4:5:6", 24, "2001:db8:1:2::/64"},
		{"not-an-ip", 24, "not-an-ip"},
	} {
		if got := userAccessIPBucket(tc.ip, tc.prefix); got != tc.want {
			t.Fatalf("ip bucket %q /%d = %q, want %q", tc.ip, tc.prefix, got, tc.want)
		}
	}
}

// RelayLogAdd 每条日志都把入站 IP / UA 记进来源表（关了日志保存也记）；落库按组合累加；
// 抽屉按 IP、按客户端分组；删用户一并清掉。
func TestUserAccessRecordFromRelayLogAndPersist(t *testing.T) {
	ctx := setupUserTest(t)
	resetUserAccessPending(t)
	u := newAdminTestUser(t, ctx, "ip-user", model.UserRoleUser, 0)
	if err := SettingSetString(model.SettingKeyRelayLogKeepEnabled, "false"); err != nil {
		t.Fatal(err)
	}
	// 关了日志保存时 RelayLogAdd 会把日志留在进程级的 relayLogCache 里；不清掉会漏进同包别的用例
	// （它们按 user_id 统计内存日志，新库里的用户 ID 会和这里的重复）。
	t.Cleanup(func() {
		relayLogCacheLock.Lock()
		relayLogCache = nil
		relayLogCacheLock.Unlock()
	})
	now := time.Now().Unix()
	add := func(ip, ua string, key int, at int64) {
		t.Helper()
		if err := RelayLogAdd(ctx, model.RelayLog{UserID: u.ID, APIKeyID: key, RequestIP: ip, RequestUserAgent: ua, Time: at}); err != nil {
			t.Fatalf("relay log add: %v", err)
		}
	}
	add("203.0.113.1", "claude-cli/2.0.1", 11, now-3*86400)
	add("203.0.113.1", "claude-cli/2.0.1", 11, now-120)
	// 没有用户 / 没有 IP 的不记（如管理员的渠道测试）。
	if err := RelayLogAdd(ctx, model.RelayLog{UserID: 0, RequestIP: "192.0.2.1", Time: now}); err != nil {
		t.Fatal(err)
	}
	if err := RelayLogAdd(ctx, model.RelayLog{UserID: u.ID, RequestIP: "", Time: now}); err != nil {
		t.Fatal(err)
	}

	// 还没落库也能看到。
	sums, err := UserAccessSummaries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sums) != 1 || sums[0].UserID != u.ID || sums[0].Last.IP != "203.0.113.1" || sums[0].Last.UserAgent != "claude-cli/2.0.1" || len(sums[0].Alerts) != 0 {
		t.Fatalf("bad pending summary %#v", sums)
	}
	if err := UserAccessSaveDB(ctx); err != nil {
		t.Fatalf("save: %v", err)
	}
	var stored []model.UserAccessSeen
	if err := db.GetDB().Where("user_id = ?", u.ID).Find(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0].Requests != 2 || stored[0].FirstSeen != now-3*86400 || stored[0].LastSeen != now-120 {
		t.Fatalf("bad stored rows %#v", stored)
	}

	// 第二轮：同一组合走 UPDATE 累加；再从一个新 IP + 新客户端来 → 两条提示。
	add("203.0.113.1", "claude-cli/2.0.1", 11, now-60)
	add("198.51.100.7", "codex_cli_rs/0.50.0", 12, now-10)
	sums, err = UserAccessSummaries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sums) != 1 || sums[0].Last.IP != "198.51.100.7" || sums[0].Last.APIKeyID != 12 ||
		!reflect.DeepEqual(alertKinds(sums[0].Alerts), []string{model.UserAccessAlertNewIP, model.UserAccessAlertNewClient}) {
		t.Fatalf("bad summary after switch %#v", sums)
	}
	if err := UserAccessSaveDB(ctx); err != nil {
		t.Fatalf("save 2: %v", err)
	}
	stored = nil
	_ = db.GetDB().Where("user_id = ?", u.ID).Order("last_seen").Find(&stored).Error
	if len(stored) != 2 || stored[0].Requests != 3 || stored[0].LastSeen != now-60 || stored[0].FirstSeen != now-3*86400 {
		t.Fatalf("bad stored rows after update %#v", stored)
	}
	// 落库后（内存已清空）结果一样。
	sums, _ = UserAccessSummaries(ctx)
	if len(sums) != 1 || len(sums[0].Alerts) != 2 {
		t.Fatalf("summary from db only %#v", sums)
	}
	// 设置「风控等级」立刻生效：关闭 → 不提示；严格 → 两个 IP 就报 IP 过多。照记不误。
	setLevel := func(level string) {
		t.Helper()
		if err := SettingSetString(model.SettingKeyUserAccessAlertLevel, level); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = SettingSetString(model.SettingKeyUserAccessAlertLevel, model.UserAccessAlertLevelStandard) })
	setLevel(model.UserAccessAlertLevelOff)
	sums, _ = UserAccessSummaries(ctx)
	if len(sums) != 1 || len(sums[0].Alerts) != 0 || sums[0].Last.IP != "198.51.100.7" {
		t.Fatalf("level off must keep the last source but no alerts %#v", sums)
	}
	setLevel(model.UserAccessAlertLevelStrict)
	sums, _ = UserAccessSummaries(ctx)
	if len(sums) != 1 || !reflect.DeepEqual(alertKinds(sums[0].Alerts), []string{model.UserAccessAlertManyIPs, model.UserAccessAlertNewClient}) {
		t.Fatalf("level strict %#v", sums)
	}
	if d, _ := UserAccessDetailGet(u.ID, ctx); d.Config.Level != model.UserAccessAlertLevelStrict || d.ManyIPsLimit != 2 {
		t.Fatalf("detail must carry the active config %#v", d.Config)
	}
	setLevel(model.UserAccessAlertLevelStandard)

	detail, err := UserAccessDetailGet(u.ID, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if detail.TotalRequests != 4 || len(detail.IPs) != 2 || len(detail.Clients) != 2 || detail.WindowSec != 3600 || detail.ManyIPsLimit != 3 ||
		detail.Config.Level != model.UserAccessAlertLevelStandard {
		t.Fatalf("bad detail %#v", detail)
	}
	if detail.IPs[0].Value != "198.51.100.7" || detail.IPs[1].Requests != 3 || detail.IPs[1].Keys[0].APIKeyID != 11 ||
		!reflect.DeepEqual(detail.IPs[1].Values, []string{"claude-cli/2.0.1"}) {
		t.Fatalf("bad ip groups %#v", detail.IPs)
	}
	if detail.Clients[0].Value != "codex_cli_rs/0.50.0" || !reflect.DeepEqual(detail.Clients[0].Values, []string{"198.51.100.7"}) {
		t.Fatalf("bad client groups %#v", detail.Clients)
	}

	add("192.0.2.50", "curl/8", 11, now)
	if err := UserDelete(u.ID, 0, ctx); err != nil {
		t.Fatalf("delete: %v", err)
	}
	var left int64
	db.GetDB().Model(&model.UserAccessSeen{}).Where("user_id = ?", u.ID).Count(&left)
	userAccessPendingMu.Lock()
	pendingLeft := len(userAccessPending)
	userAccessPendingMu.Unlock()
	if left != 0 || pendingLeft != 0 {
		t.Fatalf("delete user left %d rows / %d pending", left, pendingLeft)
	}
}

// 超过保留天数没再出现的组合会被清掉。
func TestUserAccessPruneOldRows(t *testing.T) {
	ctx := setupUserTest(t)
	resetUserAccessPending(t)
	old := time.Now().Add(-(UserAccessKeepDays + 1) * 24 * time.Hour).Unix()
	if err := db.GetDB().Create(&model.UserAccessSeen{SeenKey: "old", UserID: 1, IP: "192.0.2.1", FirstSeen: old, LastSeen: old, Requests: 1}).Error; err != nil {
		t.Fatal(err)
	}
	userAccessLastPrune = time.Time{}
	if err := UserAccessSaveDB(ctx); err != nil {
		t.Fatal(err)
	}
	var n int64
	db.GetDB().Model(&model.UserAccessSeen{}).Count(&n)
	if n != 0 {
		t.Fatalf("old row not pruned, %d left", n)
	}
}
