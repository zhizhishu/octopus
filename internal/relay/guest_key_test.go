package relay

import (
	"net/http/httptest"
	"testing"

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
	if cut := b.guestKeyCapCutoff(0.5); cut != nil {
		t.Fatalf("under cap must not cut: %+v", cut)
	}
	cut := b.guestKeyCapCutoff(1.2)
	if cut == nil || cut.Code != grokRealtimeCutoffCodeKeyCap {
		t.Fatalf("over cap must cut with key-cap code, got %+v", cut)
	}
	adminPlain := &grokRealtimeBiller{admin: true, apiKeyID: 99, keyMaxCost: 1}
	if cut := adminPlain.guestKeyCapCutoff(5); cut != nil {
		t.Fatal("non-guest admin key behaviour must stay unchanged")
	}
	noCap := &grokRealtimeBiller{admin: true, guest: true, apiKeyID: 99}
	if cut := noCap.guestKeyCapCutoff(5); cut != nil {
		t.Fatal("guest key without max_cost has no cap")
	}
}
