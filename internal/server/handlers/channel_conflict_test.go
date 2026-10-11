package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/bestruirui/octopus/internal/db"
)

// The field evidence: creating a channel with a fresh name returns 200, and re-sending the SAME
// name returns a constraint failure. It was answered as HTTP 500 "constraint failed: UNIQUE
// constraint failed: channels.name" — a client-side conflict reported as a server fault, which
// tells the caller to retry something that can never succeed. The status must be 409.
//
// Measured on the real instance before this fix: #1 new name -> 200, #2 same name -> 500 in
// 0.001s (identical body), #3 same name -> byte-identical 500, new name -> 200 again.

func newChannelRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/v1/channel/create", createChannel)
	return r
}

func postChannelCreate(t *testing.T, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/channel/create", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	newChannelRouter().ServeHTTP(rec, req)
	return rec
}

func TestDuplicateChannelNameIsAConflictNotAServerError(t *testing.T) {
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "octopus.db"), false); err != nil {
		t.Fatalf("init db: %v", err)
	}
	// The sqlite file stays open through the package-level pool; release it before TempDir
	// cleanup or Windows refuses to unlink the file and marks the test failed for the wrong
	// reason.
	t.Cleanup(func() { _ = db.Close() })
	body := map[string]any{
		"name":      "duplicate-name-channel",
		"type":      0,
		"enabled":   true,
		"base_urls": []map[string]any{{"url": "http://127.0.0.1:9", "delay": 0}},
		"keys":      []map[string]any{{"channel_key": "sk-test", "enabled": true}},
		"model":     "duplicate-name-model",
	}

	first := postChannelCreate(t, body)
	if first.Code != http.StatusOK {
		t.Fatalf("first create with a fresh name must succeed, got %d body=%s", first.Code, first.Body.String())
	}

	second := postChannelCreate(t, body)
	if second.Code != http.StatusConflict {
		t.Fatalf("a duplicate channel name is a client conflict (409), got %d body=%s", second.Code, second.Body.String())
	}
	if second.Code == http.StatusInternalServerError {
		t.Fatalf("a duplicate channel name must never be reported as a server error")
	}

	// A different name still succeeds: the conflict is about the name, not about the API.
	body["name"] = "duplicate-name-channel-2"
	third := postChannelCreate(t, body)
	if third.Code != http.StatusOK {
		t.Fatalf("a fresh name must still succeed after a conflict, got %d body=%s", third.Code, third.Body.String())
	}
}

func TestUniqueConstraintClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"sqlite unique", errString("constraint failed: UNIQUE constraint failed: channels.name (2067)"), true},
		{"mysql duplicate", errString("Error 1062: Duplicate entry 'x' for key 'channels.name'"), true},
		{"postgres duplicate", errString(`duplicate key value violates unique constraint "channels_name_key"`), true},
		{"unrelated", errString("database is locked"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isUniqueConstraintViolation(tc.err); got != tc.want {
				t.Fatalf("isUniqueConstraintViolation(%v) = %t, want %t", tc.err, got, tc.want)
			}
		})
	}
}

type errString string

func (e errString) Error() string { return string(e) }
