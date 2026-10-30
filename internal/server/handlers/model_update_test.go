package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
)

func postUpdateLLM(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/model/update", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	updateLLM(c)
	return w
}

func loadLLMRow(t *testing.T, name string) model.LLMInfo {
	t.Helper()
	var row model.LLMInfo
	if err := db.GetDB().Where("name = ?", name).First(&row).Error; err != nil {
		t.Fatalf("load row %q: %v", name, err)
	}
	return row
}

// An older frontend never sends per_call. Editing the token prices from it must
// not switch off per-call billing for a video model.
func TestUpdateLLMKeepsPerCallWhenOmitted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupModelGetDB(t)
	if err := op.LLMCreate(model.LLMInfo{Name: "video-x", LLMPrice: model.LLMPrice{Input: 1, Output: 2, CacheRead: 0.5, CacheWrite: 0.7, PerCall: 0.25}}, ctx); err != nil {
		t.Fatalf("create: %v", err)
	}

	w := postUpdateLLM(t, `{"name":"video-x","input":3,"output":4}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body=%s", w.Code, w.Body.String())
	}

	want := model.LLMPrice{Input: 3, Output: 4, CacheRead: 0.5, CacheWrite: 0.7, PerCall: 0.25}
	if row := loadLLMRow(t, "video-x"); row.LLMPrice != want {
		t.Fatalf("db row: got %+v want %+v", row.LLMPrice, want)
	}
	if cached, err := op.LLMGet("video-x"); err != nil || cached != want {
		t.Fatalf("cache: got %+v err=%v want %+v", cached, err, want)
	}
}

// A client that does send per_call (including an explicit 0) must still be able
// to change or clear it.
func TestUpdateLLMAppliesSentPerCall(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupModelGetDB(t)
	if err := op.LLMCreate(model.LLMInfo{Name: "video-y", LLMPrice: model.LLMPrice{PerCall: 0.25}}, ctx); err != nil {
		t.Fatalf("create: %v", err)
	}

	if w := postUpdateLLM(t, `{"name":"video-y","per_call":0.5}`); w.Code != http.StatusOK {
		t.Fatalf("status %d body=%s", w.Code, w.Body.String())
	}
	if got := loadLLMRow(t, "video-y").PerCall; got != 0.5 {
		t.Fatalf("per_call after set: got %v want 0.5", got)
	}
	if w := postUpdateLLM(t, `{"name":"video-y","per_call":0}`); w.Code != http.StatusOK {
		t.Fatalf("status %d body=%s", w.Code, w.Body.String())
	}
	if got := loadLLMRow(t, "video-y").PerCall; got != 0 {
		t.Fatalf("per_call after clear: got %v want 0", got)
	}
}

func TestUpdateLLMUnknownModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupModelGetDB(t)
	if w := postUpdateLLM(t, `{"name":"nope","input":1}`); w.Code == http.StatusOK {
		t.Fatalf("expected failure for unknown model, got 200 body=%s", w.Body.String())
	}
}
