package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
)

// 通过 API Key 鉴权的请求会记一条客户端指纹（管理员名下的 Key 也一样，可按 Key 查）；
// Key 本身（Authorization 的值）不进任何字段。
func TestAPIKeyAuthRecordsClientFingerprintWithoutSecrets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupMiddlewareAuthDB(t)
	admin := createMiddlewareAuthUser(t, ctx)
	key := createMiddlewareAuthAPIKey(t, ctx, admin, "guest", "sk-octopus-guest-secret-123", nil)
	router := newEndpointFamilyAuthRouter()

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("x-api-key", key.APIKey)
	req.Header.Set("User-Agent", "claude-cli/2.0.14 (external, cli)")
	req.Header.Set("X-App", "cli")
	req.Header.Set("Cookie", "sid=cookie-secret")
	req.RemoteAddr = "203.0.113.5:1234"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("auth failed %d %s", rec.Code, rec.Body.String())
	}

	d, err := op.UserClientDetailGet(ctx, 0, key.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 内存队列是进程级的，同包别的用例的请求也可能在里面（同样的 user / key id），只看这次的那条。
	fp, ok := findClaude(d.Fingerprints)
	if !ok {
		t.Fatalf("no claude code fingerprint in %#v", d.Fingerprints)
	}
	if fp.UserID != admin.ID || fp.APIKeyID != key.ID || fp.ClientName != "Claude Code" || fp.LastIP != "203.0.113.5" || fp.Requests != 1 {
		t.Fatalf("bad fingerprint %#v", fp)
	}
	raw, _ := json.Marshal(fp)
	for _, secret := range []string{"sk-octopus-guest-secret-123", "cookie-secret"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("secret %q stored: %s", secret, raw)
		}
	}
	if !strings.Contains(fp.HeaderNames, "X-Api-Key") || !strings.Contains(fp.HeaderNames, "Cookie") {
		t.Fatalf("header names should list secret headers by name: %s", fp.HeaderNames)
	}

	// 鉴权失败的不记。
	bad := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	bad.Header.Set("x-api-key", "sk-wrong")
	router.ServeHTTP(httptest.NewRecorder(), bad)
	d, _ = op.UserClientDetailGet(ctx, admin.ID, 0)
	if fp, _ := findClaude(d.Fingerprints); fp.Requests != 1 {
		t.Fatalf("failed auth must not record: %#v", d.Fingerprints)
	}
}

func findClaude(fps []model.UserClientFingerprint) (model.UserClientFingerprint, bool) {
	for _, fp := range fps {
		if fp.ClientName == "Claude Code" {
			return fp, true
		}
	}
	return model.UserClientFingerprint{}, false
}
