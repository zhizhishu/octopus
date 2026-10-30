package handlers

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/logimage"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/server/router"
)

func newLogImageEngine(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	// 走真实注册的路由与中间件链（Auth + AdminOnly），不绕过鉴权。
	if err := router.RegisterAll(engine); err != nil {
		t.Fatalf("register routes: %v", err)
	}
	return engine
}

func putLogImage(t *testing.T, dir string, data []byte, ext string) string {
	t.Helper()
	sum := sha256.Sum256(data)
	h := hex.EncodeToString(sum[:])
	p := filepath.Join(dir, h[:2], h+"."+ext)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return h
}

func getWithBearer(engine *gin.Engine, target, bearer string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec
}

func TestLogImageEndpoint(t *testing.T) {
	ctx := setupSettingAuthTestDB(t)
	admin := seedSettingAuthSiteSecrets(t, ctx)
	dir := t.TempDir()
	t.Cleanup(logimage.SetDir(dir))
	// 目录外放一个「机密」文件，验证路径穿越拿不到它。
	secretPath := filepath.Join(filepath.Dir(dir), "octopus-secret.txt")
	_ = os.WriteFile(secretPath, []byte("TOP-SECRET"), 0o600)
	t.Cleanup(func() { _ = os.Remove(secretPath) })

	png := append([]byte("\x89PNG\r\n\x1a\n"), []byte("pixels")...)
	jpg := append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, []byte("jpeg")...)
	pngHash := putLogImage(t, dir, png, "png")
	jpgHash := putLogImage(t, dir, jpg, "jpg")

	user, err := op.UserCreate(model.UserCreateRequest{
		Username: "viewer", Password: "password", Role: model.UserRoleUser, Status: model.UserStatusActive,
	}, ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	engine := newLogImageEngine(t)
	adminJWT := jwtBearer(t, admin)
	userJWT := jwtBearer(t, user)

	t.Run("admin gets the image with safe headers", func(t *testing.T) {
		for hash, want := range map[string]struct {
			body []byte
			ct   string
		}{pngHash: {png, "image/png"}, jpgHash: {jpg, "image/jpeg"}} {
			rec := getWithBearer(engine, "/api/v1/log/image/"+hash, adminJWT)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d body=%s", rec.Code, rec.Body.String())
			}
			if !bytes.Equal(rec.Body.Bytes(), want.body) {
				t.Fatal("body mismatch")
			}
			if got := rec.Header().Get("Content-Type"); got != want.ct {
				t.Fatalf("content-type %q want %q", got, want.ct)
			}
			if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatal("nosniff header missing")
			}
			if !strings.HasPrefix(rec.Header().Get("Cache-Control"), "private") {
				t.Fatalf("cache-control must be private: %q", rec.Header().Get("Cache-Control"))
			}
		}
	})

	t.Run("admin access token also works", func(t *testing.T) {
		rec := getWithBearer(engine, "/api/v1/log/image/"+pngHash, adminTokenBearer())
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d", rec.Code)
		}
	})

	t.Run("no auth is refused", func(t *testing.T) {
		rec := getWithBearer(engine, "/api/v1/log/image/"+pngHash, "")
		if rec.Code == http.StatusOK || bytes.Contains(rec.Body.Bytes(), []byte("pixels")) {
			t.Fatalf("unauthenticated request got the image: %d", rec.Code)
		}
	})

	t.Run("non-admin is forbidden", func(t *testing.T) {
		rec := getWithBearer(engine, "/api/v1/log/image/"+pngHash, userJWT)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status %d, want 403", rec.Code)
		}
	})

	t.Run("bad hashes and traversal are rejected", func(t *testing.T) {
		for _, target := range []string{
			"/api/v1/log/image/abc",
			"/api/v1/log/image/" + strings.ToUpper(pngHash),
			"/api/v1/log/image/" + pngHash + "0",
			"/api/v1/log/image/..%2Foctopus-secret.txt",
			"/api/v1/log/image/..%2F..%2F..%2F..%2Fetc%2Fpasswd",
			"/api/v1/log/image/" + pngHash[:2] + "%2F" + pngHash + ".png",
			"/api/v1/log/image/../../octopus-secret.txt",
		} {
			rec := getWithBearer(engine, target, adminJWT)
			if rec.Code == http.StatusOK {
				t.Fatalf("%s returned 200", target)
			}
			if bytes.Contains(rec.Body.Bytes(), []byte("TOP-SECRET")) || bytes.Contains(rec.Body.Bytes(), []byte("root:")) {
				t.Fatalf("%s leaked a file outside the image dir", target)
			}
		}
		rec := getWithBearer(engine, "/api/v1/log/image/"+strings.ToUpper(pngHash), adminJWT)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid hash should be 400, got %d", rec.Code)
		}
	})

	t.Run("missing image is 404", func(t *testing.T) {
		missing := strings.Repeat("ab", 32)
		rec := getWithBearer(engine, "/api/v1/log/image/"+missing, adminJWT)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status %d, want 404", rec.Code)
		}
	})

	t.Run("log detail route still works next to /image/:hash", func(t *testing.T) {
		rec := getWithBearer(engine, "/api/v1/log/123456", adminJWT)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("GET /api/v1/log/:id should still reach getLog (404 for a missing id), got %d %s", rec.Code, rec.Body.String())
		}
	})
}
