package op

import (
	"errors"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
)

// 过期时间存的是绝对时刻（Unix 秒），前端按本地时区换算；后端原样存、原样读，不做任何时区偏移。
// 新设的过去时间被拒；已过期的 Key 不改过期时间时，其他字段仍可保存。
func TestAPIKeyExpireStoredAsIsAndPastRejected(t *testing.T) {
	ctx, owner := setupCreateDisabledTest(t)

	// 洛杉矶 2026-11-01 00:00 (PDT) = 2026-11-01T07:00Z
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skipf("no tzdata: %v", err)
	}
	future := time.Now().AddDate(1, 0, 0)
	localMidnight := time.Date(future.Year(), 11, 1, 0, 0, 0, 0, la)
	key := model.APIKey{UserID: owner.ID, Name: "exp", APIKey: "sk-octopus-exp", Enabled: true, ExpireAt: localMidnight.Unix()}
	if err := APIKeyCreate(&key, ctx); err != nil {
		t.Fatalf("create: %v", err)
	}
	var row model.APIKey
	if err := db.GetDB().First(&row, key.ID).Error; err != nil {
		t.Fatalf("load: %v", err)
	}
	if row.ExpireAt != localMidnight.Unix() {
		t.Fatalf("stored %d want %d", row.ExpireAt, localMidnight.Unix())
	}
	if got := time.Unix(row.ExpireAt, 0).In(la); got.Hour() != 0 || got.Day() != 1 || got.Month() != 11 {
		t.Fatalf("read back in LA = %v, want Nov 1 00:00", got)
	}
	if err := apiKeyRefreshCache(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got, _ := APIKeyGetByAPIKey("sk-octopus-exp", ctx); got.ExpireAt != localMidnight.Unix() {
		t.Fatalf("after reload expire=%d", got.ExpireAt)
	}

	past := time.Now().Add(-time.Hour).Unix()
	bad := model.APIKey{UserID: owner.ID, Name: "past", APIKey: "sk-octopus-past", Enabled: true, ExpireAt: past}
	if err := APIKeyCreate(&bad, ctx); !errors.Is(err, errAPIKeyExpirePast) {
		t.Fatalf("create with past expiry err=%v, want errAPIKeyExpirePast", err)
	}

	upd := key
	upd.ExpireAt = past
	if err := APIKeyUpdate(&upd, owner, ctx); !errors.Is(err, errAPIKeyExpirePast) {
		t.Fatalf("update to past expiry err=%v", err)
	}

	// 已经过期的老 Key（直接写库模拟）：只改名字、过期时间不动，允许保存。
	if err := db.GetDB().Model(&model.APIKey{}).Where("id = ?", key.ID).Update("expire_at", past).Error; err != nil {
		t.Fatalf("age key: %v", err)
	}
	if err := apiKeyRefreshCache(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	rename, _ := APIKeyGetByAPIKey("sk-octopus-exp", ctx)
	rename.Name = "renamed"
	if err := APIKeyUpdate(&rename, owner, ctx); err != nil {
		t.Fatalf("rename expired key: %v", err)
	}
	// 永不过期 (0) 永远允许。
	rename.ExpireAt = 0
	if err := APIKeyUpdate(&rename, owner, ctx); err != nil {
		t.Fatalf("clear expiry: %v", err)
	}
}
