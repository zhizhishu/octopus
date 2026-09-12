package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/server/middleware"
	"github.com/gin-gonic/gin"
)

func TestAbortRunningRequestHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name, id, body string
		want           int
	}{
		{"zero id", "0", `{}`, http.StatusBadRequest},
		{"invalid id", "abc", `{}`, http.StatusBadRequest},
		{"missing time", "1", `{}`, http.StatusBadRequest},
		{"invalid time", "1", `{"started_at":"no"}`, http.StatusBadRequest},
		{"missing request", "18446744073709551615", `{"started_at":"2026-09-10T00:00:00Z"}`, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Params = gin.Params{{Key: "id", Value: tc.id}}
			c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))
			c.Request.Header.Set("Content-Type", "application/json")
			abortRunningRequest(c)
			if rec.Code != tc.want {
				t.Fatalf("status=%d want=%d", rec.Code, tc.want)
			}
		})
	}
}

func TestAbortRunningRequestRequiresAdmin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/abort", middleware.AdminOnly(), abortRunningRequest)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/abort", strings.NewReader(`{}`)))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d want=403", rec.Code)
	}
}
