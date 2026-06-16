package relay

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestWriteResponsesFailedSSE(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/responses", nil)

	if !writeResponsesFailedSSE(c, "gpt-5.3-codex", "upstream_error", "upstream stream failed") {
		t.Fatal("expected response.failed event to be written")
	}

	got := rec.Body.String()
	for _, want := range []string{
		"event: response.failed",
		`"type":"response.failed"`,
		`"model":"gpt-5.3-codex"`,
		`"status":"failed"`,
		`"output":[]`,
		`"code":"upstream_error"`,
		"data: [DONE]",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("response.failed SSE missing %s in %s", want, got)
		}
	}
}

func TestWriteAnthropicErrorSSE(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	if !writeAnthropicErrorSSE(c, "api_error", "upstream stream failed before terminal event") {
		t.Fatal("expected Anthropic error event to be written")
	}

	got := rec.Body.String()
	for _, want := range []string{
		"event: error",
		`"type":"error"`,
		`"type":"api_error"`,
		`"message":"upstream stream failed before terminal event"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("Anthropic error SSE missing %s in %s", want, got)
		}
	}
}

func TestIsResponsesInboundPathCoversCodexAliases(t *testing.T) {
	tests := map[string]bool{
		"/v1/responses":                        true,
		"/v1/responses/compact":                true,
		"/responses":                           true,
		"/responses/compact":                   true,
		"/backend-api/codex/responses":         true,
		"/backend-api/codex/responses/compact": true,
		"/v1/chat/completions":                 false,
		"/backend-api/codex/not-responses":     false,
		"/backend-api/codex/responses-fake":    false,
	}
	for path, want := range tests {
		if got := isResponsesInboundPath(path); got != want {
			t.Fatalf("isResponsesInboundPath(%q): got %t want %t", path, got, want)
		}
	}
}
