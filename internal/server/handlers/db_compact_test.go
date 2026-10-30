package handlers

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/db"
)

func TestDBCompactHandlersOnSQLite(t *testing.T) {
	gin.SetMode(gin.TestMode)
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "compact.db"), false); err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/setting/database", nil)
	getDBCompactInfo(c)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"supported":true`) || !strings.Contains(w.Body.String(), `"file_bytes"`) {
		t.Fatalf("info: %d %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/setting/database/compact", nil)
	runDBCompact(c)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"before_bytes"`) || !strings.Contains(w.Body.String(), `"after_bytes"`) {
		t.Fatalf("compact: %d %s", w.Code, w.Body.String())
	}
}
