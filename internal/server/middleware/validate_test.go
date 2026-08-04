package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestRequireJSON locks in the relaxed Content-Type policy: a JSON body must be
// accepted regardless of whether the client labels it application/json,
// text/plain, or sends no Content-Type at all (browser translation add-ons and
// minimal HTTP clients do this to dodge a CORS preflight). Only form/multipart
// submissions are rejected. Regression guard for the silent-415 bug where
// translation requests were bounced before the relay handler and left no log.
func TestRequireJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name        string
		method      string
		contentType string
		wantPass    bool
	}{
		{"post application/json", http.MethodPost, "application/json", true},
		{"post application/json with charset", http.MethodPost, "application/json; charset=utf-8", true},
		{"post text/plain from translation add-on", http.MethodPost, "text/plain;charset=UTF-8", true},
		{"post empty content-type", http.MethodPost, "", true},
		{"post uppercase JSON", http.MethodPost, "Application/JSON", true},
		{"post multipart form rejected", http.MethodPost, "multipart/form-data; boundary=x", false},
		{"post urlencoded form rejected", http.MethodPost, "application/x-www-form-urlencoded", false},
		{"get short-circuited", http.MethodGet, "application/x-www-form-urlencoded", true},
		{"delete short-circuited", http.MethodDelete, "text/plain", true},
		{"options short-circuited", http.MethodOptions, "multipart/form-data", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(tc.method, "/v1/chat/completions", strings.NewReader("{}"))
			if tc.contentType != "" {
				c.Request.Header.Set("Content-Type", tc.contentType)
			}

			RequireJSON()(c)

			passed := !c.IsAborted()
			if passed != tc.wantPass {
				t.Fatalf("method=%s content-type=%q: passed=%v want=%v (status=%d)",
					tc.method, tc.contentType, passed, tc.wantPass, w.Code)
			}
			if !tc.wantPass && w.Code != http.StatusUnsupportedMediaType {
				t.Fatalf("rejected request should be 415, got %d", w.Code)
			}
		})
	}
}
