package op

import (
	"strings"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/telemetry"
)

func resetUserClientPending(t *testing.T) {
	t.Helper()
	reset := func() {
		userClientPendingMu.Lock()
		userClientPending = make(map[string]*model.UserClientFingerprint)
		userClientPendingDropped = 0
		userClientPendingMu.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

func snap(hash, ua, session string) telemetry.Snapshot {
	return telemetry.Snapshot{
		Hash: hash, UserAgent: ua, ClientName: "Claude Code", ClientVersion: "2.0.14",
		HeaderNames: []string{"host", "user-agent", "authorization"}, OrderKnown: true,
		Notable:     map[string]string{"x-stainless-os": "MacOS", "x-claude-code-session-id": session},
		HTTPVersion: "HTTP/1.1", Path: "/v1/messages",
	}
}

// 记录 → 还没落库也看得到 → 落库按指纹累加、「最近」列取最新 → 按 Key 看 → 删用户一并清掉。
func TestUserClientRecordPersistAndDetail(t *testing.T) {
	ctx := setupUserTest(t)
	resetUserClientPending(t)
	u := newAdminTestUser(t, ctx, "fp-user", model.UserRoleUser, 0)
	now := time.Now().Unix()

	UserClientRecord(u.ID, 11, "203.0.113.1", snap("aaaa", "claude-cli/2.0.14", "s1"), now-100)
	UserClientRecord(u.ID, 11, "203.0.113.2", snap("aaaa", "claude-cli/2.0.14", "s2"), now-50)
	UserClientRecord(u.ID, 12, "198.51.100.7", snap("bbbb", "codex_cli_rs/0.50.0", ""), now-10)
	UserClientRecord(0, 12, "198.51.100.7", snap("cccc", "x", ""), now)   // 没有用户不记
	UserClientRecord(u.ID, 12, "198.51.100.7", telemetry.Snapshot{}, now) // 没有指纹不记

	d, err := UserClientDetailGet(ctx, u.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Fingerprints) != 2 || d.Fingerprints[0].FPHash != "bbbb" || d.Fingerprints[1].Requests != 2 ||
		d.Fingerprints[1].LastIP != "203.0.113.2" || len(d.Keys) != 2 || d.TLSTerminated {
		t.Fatalf("pending detail %#v", d)
	}
	if err := UserClientSaveDB(ctx); err != nil {
		t.Fatal(err)
	}
	UserClientRecord(u.ID, 11, "203.0.113.9", snap("aaaa", "claude-cli/2.0.14", "s3"), now-5)
	if err := UserClientSaveDB(ctx); err != nil {
		t.Fatal(err)
	}
	var rows []model.UserClientFingerprint
	db.GetDB().Where("user_id = ?", u.ID).Order("fp_hash").Find(&rows)
	if len(rows) != 2 || rows[0].Requests != 3 || rows[0].LastIP != "203.0.113.9" || rows[0].FirstSeen != now-100 ||
		rows[0].LastSeen != now-5 || !strings.Contains(rows[0].Notable, `"s3"`) || rows[0].HeaderNames != "host,user-agent,authorization" {
		t.Fatalf("stored %#v", rows)
	}
	// 迟到的旧记录不覆盖「最近」列。
	UserClientRecord(u.ID, 11, "192.0.2.200", snap("aaaa", "claude-cli/2.0.14", "old"), now-1000)
	_ = UserClientSaveDB(ctx)
	db.GetDB().Where("user_id = ?", u.ID).Order("fp_hash").Find(&rows)
	if rows[0].LastIP != "203.0.113.9" || rows[0].FirstSeen != now-1000 || rows[0].Requests != 4 {
		t.Fatalf("late row %#v", rows[0])
	}

	// 只按 Key 看（访客 Key 在管理员名下，只给 key 也能查）。
	byKey, err := UserClientDetailGet(ctx, 0, 12)
	if err != nil || len(byKey.Fingerprints) != 1 || byKey.Fingerprints[0].FPHash != "bbbb" {
		t.Fatalf("by key %#v %v", byKey, err)
	}
	filtered, _ := UserClientDetailGet(ctx, u.ID, 11)
	if len(filtered.Fingerprints) != 1 || len(filtered.Keys) != 2 {
		t.Fatalf("user+key filter keeps key list: %#v", filtered)
	}
	if _, err := UserClientDetailGet(ctx, 0, 0); err == nil {
		t.Fatal("need user or key")
	}

	UserClientRecord(u.ID, 11, "192.0.2.50", snap("dddd", "curl/8", ""), now)
	if err := UserDelete(u.ID, 0, ctx); err != nil {
		t.Fatal(err)
	}
	var left int64
	db.GetDB().Model(&model.UserClientFingerprint{}).Where("user_id = ?", u.ID).Count(&left)
	userClientPendingMu.Lock()
	pendingLeft := len(userClientPending)
	userClientPendingMu.Unlock()
	if left != 0 || pendingLeft != 0 {
		t.Fatalf("delete user left %d rows / %d pending", left, pendingLeft)
	}
}

// 体量有上限：90 天没出现的删；每个 (用户, Key) 只留最新的 UserClientPerKeyMax 个；内存队列有上限。
func TestUserClientBounded(t *testing.T) {
	ctx := setupUserTest(t)
	resetUserClientPending(t)
	now := time.Now()
	old := now.Add(-(UserAccessKeepDays + 1) * 24 * time.Hour).Unix()
	db.GetDB().Create(&model.UserClientFingerprint{SeenKey: "old", UserID: 1, APIKeyID: 1, FPHash: "old", FirstSeen: old, LastSeen: old, Requests: 1})
	extra := 5
	for i := 0; i < model.UserClientPerKeyMax+extra; i++ {
		h := "h" + strings.Repeat("x", i%3) + string(rune('a'+i%26)) + string(rune('a'+i/26))
		db.GetDB().Create(&model.UserClientFingerprint{SeenKey: model.UserClientSeenKey(2, 7, h), UserID: 2, APIKeyID: 7, FPHash: h,
			FirstSeen: now.Unix() - 1000 + int64(i), LastSeen: now.Unix() - 1000 + int64(i), Requests: 1})
	}
	userClientLastPrune = time.Time{}
	if err := UserClientSaveDB(ctx); err != nil {
		t.Fatal(err)
	}
	var n, oldLeft, minLast int64
	db.GetDB().Model(&model.UserClientFingerprint{}).Where("user_id = 2").Count(&n)
	db.GetDB().Model(&model.UserClientFingerprint{}).Where("seen_key = 'old'").Count(&oldLeft)
	db.GetDB().Model(&model.UserClientFingerprint{}).Where("user_id = 2").Select("MIN(last_seen)").Scan(&minLast)
	if n != model.UserClientPerKeyMax || oldLeft != 0 || minLast != now.Unix()-1000+int64(extra) {
		t.Fatalf("n=%d old=%d min=%d", n, oldLeft, minLast)
	}

	for i := 0; i < userClientPendingMax+10; i++ {
		UserClientRecord(3, 1, "192.0.2.1", snap("p"+strings.Repeat("0", 4)+string(rune(0x4e00+i)), "ua", ""), now.Unix())
	}
	userClientPendingMu.Lock()
	size, dropped := len(userClientPending), userClientPendingDropped
	userClientPendingMu.Unlock()
	if size != userClientPendingMax || dropped != 10 {
		t.Fatalf("pending %d dropped %d", size, dropped)
	}
}
