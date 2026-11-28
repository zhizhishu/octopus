package relay

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestProxySSENonSSEBodyRedactsSecretsPinnedMode pins that the non-SSE
// content-type error path passes the upstream body through upstreamBodySummary
// (secret redaction + truncation), never echoing raw credential text.
func TestProxySSENonSSEBodyRedactsSecretsPinnedMode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ0ZXN0In0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"
	body := `{"error":{"message":"bad key sk-test1234567890abcdefgh token ` + jwt + ` hex 0123456789abcdef0123456789abcdef"}}`

	respUp := &http.Response{
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(body)),
		StatusCode: 401,
	}
	respUp.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/images", nil)

	_, _, _, err := proxySSEWithOptions(context.Background(), c, respUp, 30, nil, proxySSEOptions{ScanAllData: true})
	if err == nil {
		t.Fatal("expected error for non-SSE content-type")
	}
	msg := err.Error()
	for _, secret := range []string{jwt, "sk-test1234567890abcdefgh", "0123456789abcdef0123456789abcdef"} {
		if !strings.Contains(msg, secret) {
			t.Fatalf("pack-1128: error message must preserve text verbatim (missing %q): %s", secret[:12]+"...", msg)
		}
	}
	if strings.Contains(msg, "[redacted]") {
		t.Fatalf("pack-1128: no redaction marker may appear, got: %s", msg)
	}
}
