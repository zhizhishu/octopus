package relay

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/model"
)

func guestTestContext(guest bool) *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("api_key_id", 7)
	c.Set("user_id", 1) // 管理员
	if guest {
		c.Set(model.APIKeyGuestContextKey, true)
	}
	return c
}

// 访客 Key 只认自己 Key 提交的任务：管理员名下只记了 user 的旧任务它看不到；普通 Key 不变。
func TestGuestKeyTaskOwnershipIsTokenOnly(t *testing.T) {
	adminLegacy := videoTaskEntry{ownerTokenID: 0, ownerUserID: 1}
	own := videoTaskEntry{ownerTokenID: 7, ownerUserID: 1}
	otherKey := videoTaskEntry{ownerTokenID: 8, ownerUserID: 1}

	g := guestTestContext(true)
	if videoTaskOwnerMatches(adminLegacy, g.GetInt("api_key_id"), taskOwnerUserID(g)) {
		t.Fatal("guest key must not see admin's legacy user-owned task")
	}
	if !videoTaskOwnerMatches(own, g.GetInt("api_key_id"), taskOwnerUserID(g)) {
		t.Fatal("guest key must see its own task")
	}
	if videoTaskOwnerMatches(otherKey, g.GetInt("api_key_id"), taskOwnerUserID(g)) {
		t.Fatal("guest key must not see another key's task")
	}
	if s := taskScope(g); s.TokenID != 7 || s.UserID != 0 {
		t.Fatalf("guest task scope must be token-only, got %+v", s)
	}

	p := guestTestContext(false)
	if !videoTaskOwnerMatches(adminLegacy, p.GetInt("api_key_id"), taskOwnerUserID(p)) {
		t.Fatal("plain key behaviour changed: legacy user-owned task should still match")
	}
	if s := taskScope(p); s.TokenID != 7 || s.UserID != 1 {
		t.Fatalf("plain task scope changed: %+v", s)
	}
}

// 实时语音：访客 Key 归属管理员（不看余额），但 Key 上限照样会断；普通管理员 Key 仍然不断。
func TestGuestKeyRealtimeCapCutoff(t *testing.T) {
	b := &grokRealtimeBiller{admin: true, guest: true, apiKeyID: 99, keyMaxCost: 1}
	if cut := b.guestKeyCapCutoff(0.5, 0); cut != nil {
		t.Fatalf("under cap must not cut: %+v", cut)
	}
	cut := b.guestKeyCapCutoff(1.2, 0)
	if cut == nil || cut.Code != grokRealtimeCutoffCodeKeyCap {
		t.Fatalf("over cap must cut with key-cap code, got %+v", cut)
	}
	adminPlain := &grokRealtimeBiller{admin: true, apiKeyID: 99, keyMaxCost: 1}
	if cut := adminPlain.guestKeyCapCutoff(5, 0); cut != nil {
		t.Fatal("non-guest admin key behaviour must stay unchanged")
	}
	noCap := &grokRealtimeBiller{admin: true, guest: true, apiKeyID: 99}
	if cut := noCap.guestKeyCapCutoff(5, 0); cut != nil {
		t.Fatal("guest key without max_cost has no cap")
	}
}

// 实时语音（按分钟计价）：访客 Key 的下次结算挪到「刚好花到上限」的时刻，不再固定等 1 分钟。
// 上限 1、每分钟 0.6（每秒 0.01）：已花 0.5 → 还剩 0.5 = 50 秒；已花 0.995 → 不够 1 秒，当场断。
func TestGuestKeyRealtimeNextSettleStopsAtCap(t *testing.T) {
	pricing := grokRealtimePricing{perMinute: true, perMinuteCost: 0.6}
	b := &grokRealtimeBiller{admin: true, guest: true, apiKeyID: 98, keyMaxCost: 1}

	cut, next := b.guestKeyCapSettle(0.5, pricing, true)
	if cut != nil {
		t.Fatalf("under cap must not cut: %+v", cut)
	}
	if next < 49*time.Second || next > 50*time.Second {
		t.Fatalf("next settle should be ~50s (until cap), got %v", next)
	}

	if cut, _ := b.guestKeyCapSettle(0.995, pricing, true); cut == nil || cut.Code != grokRealtimeCutoffCodeKeyCap {
		t.Fatalf("less than one second left must cut now, got %+v", cut)
	}

	// 普通管理员 Key：不断，仍然 1 分钟复查。
	plain := &grokRealtimeBiller{admin: true, apiKeyID: 98, keyMaxCost: 1}
	if cut, next := plain.guestKeyCapSettle(5, pricing, true); cut != nil || next != grokRealtimeBillingInterval {
		t.Fatalf("plain admin session unchanged: cut=%+v next=%v", cut, next)
	}
}

// 无归属（0/0）的旧视频 / MJ 任务：任何 API Key（普通、管理员自己的、访客）都碰不到；
// 只有管理员控制台登录会话（没有 api_key_id、角色 admin）能看。有归属的任务规则不变。
func TestOwnerlessLegacyTaskOnlyForAdminSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	newCtx := func(apiKeyID int, role string, guest bool) *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		if apiKeyID > 0 {
			c.Set("api_key_id", apiKeyID)
		}
		c.Set("user_id", 1)
		c.Set("user_role", role)
		if guest {
			c.Set(model.APIKeyGuestContextKey, true)
		}
		return c
	}
	cases := []struct {
		name string
		c    *gin.Context
		want bool
	}{
		{"admin console session", newCtx(0, string(model.UserRoleAdmin), false), true},
		{"admin's own API key", newCtx(7, string(model.UserRoleAdmin), false), false},
		{"plain user key", newCtx(8, string(model.UserRoleUser), false), false},
		{"guest key", newCtx(9, "guest_key", true), false},
		{"plain user session", newCtx(0, string(model.UserRoleUser), false), false},
	}
	for _, tc := range cases {
		if got := taskOwnerMatchesRequest(tc.c, 0, 0); got != tc.want {
			t.Fatalf("%s: ownerless task visible=%v, want %v", tc.name, got, tc.want)
		}
	}
	// 有归属的任务：同一把 Key 能看，管理员会话也不能越权看别人的 Key 的任务。
	if !taskOwnerMatchesRequest(newCtx(7, string(model.UserRoleAdmin), false), 7, 1) {
		t.Fatal("owner key must still see its own task")
	}
	if taskOwnerMatchesRequest(newCtx(0, string(model.UserRoleAdmin), false), 7, 1) {
		t.Fatal("admin session must not bypass token-owned task scope")
	}
}
