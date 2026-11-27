package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/server/router"
)

// 鹈鹕图自动存 PNG 的上传/下载接口：走真实注册的路由与中间件链（Auth + AdminOnly），
// 不绕过鉴权（套路同 log_image_test.go）。

func newCreativePNGEngine(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	if err := router.RegisterAll(engine); err != nil {
		t.Fatalf("register routes: %v", err)
	}
	return engine
}

// creativePNGUploadBytes 造一段带 PNG 文件头的字节（与 op 侧的 creativePNGBytes 同思路，
// 两个包各自持有一份，别为测试互相 import）。
func creativePNGUploadBytes(n int) []byte {
	data := make([]byte, 8+n)
	copy(data, []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A})
	for i := 8; i < len(data); i++ {
		data[i] = byte(i)
	}
	return data
}

func seedCreativePNGRow(t *testing.T, status string) int64 {
	t.Helper()
	row := model.ModelAuditCreative{
		RoundID: "r-png", ChannelID: 1, ChannelName: "c", Model: "m", Kind: op.CreativeKindPelican,
		Prompt: op.CreativePelicanPrompt, Status: status, CreatedAt: time.Now(),
	}
	if err := db.GetDB().Create(&row).Error; err != nil {
		t.Fatalf("seed creative: %v", err)
	}
	return row.ID
}

func doCreativePNG(engine *gin.Engine, method, target, bearer string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if body != nil {
		req.Header.Set("Content-Type", "image/png")
	}
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec
}

func creativePNGPath(id int64) string {
	return fmt.Sprintf("/api/v1/model-audit/creative/items/%d/png", id)
}

func storedFromResponse(t *testing.T, rec *httptest.ResponseRecorder) bool {
	t.Helper()
	var payload struct {
		Data struct {
			Stored bool `json:"stored"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v (%s)", err, rec.Body.String())
	}
	return payload.Data.Stored
}

func TestCreativePNGEndpoints(t *testing.T) {
	ctx := setupSettingAuthTestDB(t)
	admin := seedSettingAuthSiteSecrets(t, ctx)
	doneID := seedCreativePNGRow(t, model.CreativeDone)
	runningID := seedCreativePNGRow(t, model.CreativeRunning)
	noPNGID := seedCreativePNGRow(t, model.CreativeDone)
	user, err := op.UserCreate(model.UserCreateRequest{
		Username: "viewer", Password: "password", Role: model.UserRoleUser, Status: model.UserStatusActive,
	}, ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	engine := newCreativePNGEngine(t)
	adminJWT := jwtBearer(t, admin)
	userJWT := jwtBearer(t, user)
	png := creativePNGUploadBytes(128)

	t.Run("admin uploads once, re-upload is a no-op", func(t *testing.T) {
		rec := doCreativePNG(engine, http.MethodPost, creativePNGPath(doneID), adminJWT, png)
		if rec.Code != http.StatusOK {
			t.Fatalf("upload status %d body=%s", rec.Code, rec.Body.String())
		}
		if !storedFromResponse(t, rec) {
			t.Fatalf("first upload must store, got %s", rec.Body.String())
		}
		rec = doCreativePNG(engine, http.MethodPost, creativePNGPath(doneID), adminJWT, creativePNGUploadBytes(64))
		if rec.Code != http.StatusOK || storedFromResponse(t, rec) {
			t.Fatalf("second upload must be a no-op, got %d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("admin downloads with safe headers", func(t *testing.T) {
		rec := doCreativePNG(engine, http.MethodGet, creativePNGPath(doneID), adminJWT, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("download status %d body=%s", rec.Code, rec.Body.String())
		}
		if !bytes.Equal(rec.Body.Bytes(), png) {
			t.Fatal("downloaded bytes differ from the upload")
		}
		if got := rec.Header().Get("Content-Type"); got != "image/png" {
			t.Fatalf("content-type %q", got)
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatal("nosniff header missing")
		}
		if rec.Header().Get("Content-Disposition") == "" {
			t.Fatal("content-disposition missing")
		}
	})

	t.Run("non-admin is forbidden on both routes", func(t *testing.T) {
		for _, method := range []string{http.MethodPost, http.MethodGet} {
			rec := doCreativePNG(engine, method, creativePNGPath(doneID), userJWT, png)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s by non-admin = %d, want 403", method, rec.Code)
			}
		}
	})

	t.Run("no auth is refused as a bad request", func(t *testing.T) {
		for _, method := range []string{http.MethodPost, http.MethodGet} {
			rec := doCreativePNG(engine, method, creativePNGPath(doneID), "", nil)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s without auth = %d, want 400", method, rec.Code)
			}
		}
	})

	t.Run("running item refuses a PNG", func(t *testing.T) {
		rec := doCreativePNG(engine, http.MethodPost, creativePNGPath(runningID), adminJWT, png)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("running item upload = %d %s, want 400", rec.Code, rec.Body.String())
		}
	})

	t.Run("done item without png downloads 404", func(t *testing.T) {
		rec := doCreativePNG(engine, http.MethodGet, creativePNGPath(noPNGID), adminJWT, nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("no-png download = %d, want 404", rec.Code)
		}
	})

	t.Run("missing item is 404", func(t *testing.T) {
		rec := doCreativePNG(engine, http.MethodPost, creativePNGPath(99999999), adminJWT, png)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("upload to missing item = %d, want 404", rec.Code)
		}
		rec = doCreativePNG(engine, http.MethodGet, creativePNGPath(99999999), adminJWT, nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("download of missing item = %d, want 404", rec.Code)
		}
	})

	t.Run("oversized upload is 413", func(t *testing.T) {
		rec := doCreativePNG(engine, http.MethodPost, creativePNGPath(noPNGID), adminJWT, creativePNGUploadBytes(model.CreativeMaxPNGBytes+1))
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("oversized upload = %d %s, want 413", rec.Code, rec.Body.String())
		}
	})

	t.Run("non-png upload is 400", func(t *testing.T) {
		rec := doCreativePNG(engine, http.MethodPost, creativePNGPath(noPNGID), adminJWT, []byte("GIF89a not a png"))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("non-png upload = %d %s, want 400", rec.Code, rec.Body.String())
		}
	})

	t.Run("pin route next to the png route still binds JSON only", func(t *testing.T) {
		// 同组的 JSON 路由不受影响：拿 PNG 字节去 pin 还是 400（ShouldBindJSON 解不了）。
		rec := doCreativePNG(engine, http.MethodPost, fmt.Sprintf("/api/v1/model-audit/creative/items/%d/pin", noPNGID), adminJWT, png)
		if rec.Code == http.StatusOK {
			t.Fatalf("pin with non-JSON body should not succeed, got %d", rec.Code)
		}
	})
}
