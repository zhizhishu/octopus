package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
)

// 访客 Key 过了 APIKeyAuth 之后：计费 / 日志口径不变（user_id = 管理员、api_key_id = 访客 Key、
// request_ip 照常解析），但上下文里的「当前用户」不是管理员。
func TestAPIKeyAuthGuestKeyContextIsNotAdmin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupMiddlewareAuthDB(t)
	admin := createMiddlewareAuthUser(t, ctx)
	guest, err := op.GuestKeyCreate(model.GuestKeyRequest{Name: "g", Models: []string{"m1"}, ExpireAt: time.Now().Add(time.Hour).Unix()}, "sk-octopus-guest-ctx-test", admin, ctx)
	if err != nil {
		t.Fatal(err)
	}
	plain := createMiddlewareAuthAPIKey(t, ctx, admin, "plain", "sk-octopus-plain-ctx-test", nil)

	router := gin.New()
	var sawAdmin, sawGuest bool
	var userID, keyID int
	var supported, ip string
	router.POST("/v1/chat/completions", APIKeyAuth(), func(c *gin.Context) {
		sawAdmin = CurrentUserIsAdmin(c)
		sawGuest = IsGuestKeyRequest(c)
		userID = c.GetInt("user_id")
		keyID = c.GetInt("api_key_id")
		supported = c.GetString("supported_models")
		ip = c.GetString("request_ip")
		c.Status(http.StatusNoContent)
	})
	do := func(key string) int {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		req.RemoteAddr = "198.51.100.7:4567"
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := do(guest.APIKey); code != http.StatusNoContent {
		t.Fatalf("guest auth: %d", code)
	}
	if sawAdmin || !sawGuest || userID != admin.ID || keyID != guest.ID || supported != "m1" || ip == "" {
		t.Fatalf("guest ctx: admin=%v guest=%v user=%d key=%d models=%q ip=%q", sawAdmin, sawGuest, userID, keyID, supported, ip)
	}
	// 普通 Key 不受影响：管理员的普通 Key 照旧是管理员。
	if code := do(plain.APIKey); code != http.StatusNoContent {
		t.Fatalf("plain auth: %d", code)
	}
	if !sawAdmin || sawGuest {
		t.Fatalf("plain admin key ctx changed: admin=%v guest=%v", sawAdmin, sawGuest)
	}
}
