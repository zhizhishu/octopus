package op

import (
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

// 异常切换规则：窗口 1 小时、3 个 IP 算多。逐条钉住「报 / 不报」。
func TestUserAccessAlertRule(t *testing.T) {
	const now = int64(1_800_000_000)
	day := int64(86400)
	cc := "claude-cli/2.0.1 (external, cli)"
	w := UserAccessAlertWindow
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
			got := alertKinds(userAccessAlerts(tc.rows, now, w, UserAccessManyIPsLimit))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("alerts = %v, want %v", got, tc.want)
			}
		})
	}

	rows := []model.UserAccessSeen{
		accessRow("203.0.113.1", cc, 1, now-3*day, now-120),
		accessRow("198.51.100.2", cc, 1, now-300, now),
	}
	a := userAccessAlerts(rows, now, w, UserAccessManyIPsLimit)[0]
	if a.Value != "198.51.100.2" || !reflect.DeepEqual(a.Others, []string{"203.0.113.1"}) || a.At != now-300 || a.WindowSec != 3600 || a.Count != 2 {
		t.Fatalf("bad new_ip detail %#v", a)
	}
	// 过了窗口没有新请求，提示自然消失。
	if got := userAccessAlerts(rows, now+2*3600, w, UserAccessManyIPsLimit); len(got) != 0 {
		t.Fatalf("alert must expire after the window, got %v", got)
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
	for ip, want := range map[string]string{
		"203.0.113.9":          "203.0.113.9",
		"::ffff:203.0.113.9":   "203.0.113.9",
		"2001:db8:1:2:3:4:5:6": "2001:db8:1:2::/64",
		"not-an-ip":            "not-an-ip",
	} {
		if got := userAccessIPBucket(ip); got != want {
			t.Fatalf("ip bucket %q = %q, want %q", ip, got, want)
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

	detail, err := UserAccessDetailGet(u.ID, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if detail.TotalRequests != 4 || len(detail.IPs) != 2 || len(detail.Clients) != 2 || detail.WindowSec != 3600 || detail.ManyIPsLimit != 3 {
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
