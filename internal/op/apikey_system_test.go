package op

import (
	"context"
	"strings"
	"testing"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
)

// 系统 Key 的保护口径（迁移 032 建的那把）：删不得、停不得、改不得，明文不出
// 列表；批量路径逐条委派给单个实现，同受拦截。普通 Key 的同路径行为不变。
// 本文件所有测试为静态编写，未在 Windows 本机运行（环境约束禁止 go test）。

// seedSystemTestKey 直插一把 system=true 的 Key（绕过 op.APIKeyCreate——它会把
// system 强制擦掉，那正是要测的另一面）。刷新缓存后 op 层才能看见它。
func seedSystemTestKey(t *testing.T) (context.Context, model.APIKey, model.User) {
	t.Helper()
	ctx, admin, _, _ := setupAPIKeyAdminTest(t)
	key := model.APIKey{UserID: admin.ID, Name: "系统", APIKey: "sk-octopus-system-key", Enabled: true, System: true}
	if err := db.GetDB().WithContext(ctx).Create(&key).Error; err != nil {
		t.Fatalf("seed system key: %v", err)
	}
	if err := apiKeyRefreshCache(ctx); err != nil {
		t.Fatalf("refresh keys: %v", err)
	}
	return ctx, key, admin
}

func TestSystemKeyDeleteRejected(t *testing.T) {
	ctx, key, admin := seedSystemTestKey(t)
	err := APIKeyDelete(key.ID, admin, ctx)
	if err == nil || !strings.Contains(err.Error(), "系统 Key 不能删除") {
		t.Fatalf("delete system key: err=%v, want 系统 Key 不能删除", err)
	}
}

func TestSystemKeySetEnabledRejected(t *testing.T) {
	ctx, key, admin := seedSystemTestKey(t)
	if err := APIKeySetEnabled(key.ID, false, admin, ctx); err == nil || !strings.Contains(err.Error(), "系统 Key 不能停用") {
		t.Fatalf("disable system key: err=%v, want 系统 Key 不能停用", err)
	}
	if err := APIKeySetEnabled(key.ID, true, admin, ctx); err == nil {
		t.Fatal("enable on system key must also be rejected（统一口径）")
	}
}

func TestSystemKeyUpdateRejected(t *testing.T) {
	ctx, key, admin := seedSystemTestKey(t)
	patched := key
	patched.Name = "被人改名"
	patched.Enabled = false
	if err := APIKeyUpdate(&patched, admin, ctx); err == nil || !strings.Contains(err.Error(), "系统 Key 不能编辑") {
		t.Fatalf("update system key: err=%v, want 系统 Key 不能编辑", err)
	}
}

func TestSystemKeySecretNotListed(t *testing.T) {
	ctx, key, admin := seedSystemTestKey(t)
	keys, err := APIKeyList(ctx, admin)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, k := range keys {
		if k.ID == key.ID {
			found = true
			if k.APIKey == "sk-octopus-system-key" {
				t.Fatal("system key plaintext must not leave the op layer")
			}
			if k.APIKey != systemKeyRedactedSecret {
				t.Fatalf("system key secret = %q, want redacted placeholder", k.APIKey)
			}
		}
	}
	if !found {
		t.Fatal("system key should be visible in the admin key list")
	}
}

func TestCreateCannotForgeSystemFlag(t *testing.T) {
	ctx, _, admin := seedSystemTestKey(t)
	forged := model.APIKey{UserID: admin.ID, Name: "fake system", APIKey: "sk-octopus-fake", Enabled: true, System: true}
	if err := APIKeyCreate(&forged, ctx); err != nil {
		t.Fatalf("create: %v", err)
	}
	loaded, err := APIKeyGet(forged.ID, ctx)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if loaded.System {
		t.Fatal("APIKeyCreate must strip request-supplied system=true")
	}
}

func TestSystemAPIKeyGetReturnsSeededRow(t *testing.T) {
	ctx, key, _ := seedSystemTestKey(t)
	got, err := SystemAPIKeyGet(ctx)
	if err != nil {
		t.Fatalf("SystemAPIKeyGet: %v", err)
	}
	if got.ID != key.ID {
		t.Fatalf("got id=%d want %d", got.ID, key.ID)
	}
	// SystemAPIKeyGet 是**进程内部**取明文的唯一入口：明文必须有（内部发送器
	// 要用它拼 Authorization 头），这正是不许出列表的原因。
	if got.APIKey != "sk-octopus-system-key" {
		t.Fatalf("internal getter must return plaintext, got %q", got.APIKey)
	}
}
