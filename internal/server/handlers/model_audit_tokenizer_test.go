package handlers

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/server/router"
)

// 基线会改变之后每一次分词器识别的结论（存错了会把冒充当成正品），所以三个接口
// 只给管理员；普通用户与未登录一律拒绝。走真的 RegisterAll，确认路由与中间件链都在。
func TestTokenizerBaselineRoutesAreAdminOnly(t *testing.T) {
	ctx := setupSettingAuthTestDB(t)
	admin := seedSettingAuthSiteSecrets(t, ctx)
	user, err := op.UserCreate(model.UserCreateRequest{
		Username: "plain", Password: "password", Role: model.UserRoleUser, Status: model.UserStatusActive,
	}, ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	if err := router.RegisterAll(engine); err != nil {
		t.Fatalf("register routes: %v", err)
	}
	targets := []struct {
		method, path string
		body         []byte
	}{
		{http.MethodGet, "/api/v1/model-audit/tokenizer-baselines", nil},
		{http.MethodPost, "/api/v1/model-audit/tokenizer-baselines", []byte(`{"result_id":1}`)},
		{http.MethodDelete, "/api/v1/model-audit/tokenizer-baselines/1", nil},
	}
	for _, tc := range targets {
		if rec := doSettingAuthRequest(engine, tc.method, tc.path, "", tc.body); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s without auth = %d", tc.method, tc.path, rec.Code)
		}
		if rec := doSettingAuthRequest(engine, tc.method, tc.path, jwtBearer(t, user), tc.body); rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s as non-admin = %d", tc.method, tc.path, rec.Code)
		}
	}
	bearer := jwtBearer(t, admin)
	if rec := doSettingAuthRequest(engine, http.MethodGet, targets[0].path, bearer, nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"baselines":[]`) {
		t.Fatalf("admin list = %d %s", rec.Code, rec.Body.String())
	}
	if rec := doSettingAuthRequest(engine, http.MethodPost, targets[1].path, bearer, []byte(`{"result_id":0}`)); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing result_id = %d", rec.Code)
	}
	if rec := doSettingAuthRequest(engine, http.MethodPost, targets[1].path, bearer, []byte(`{"result_id":424242}`)); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown result = %d", rec.Code)
	}
	if rec := doSettingAuthRequest(engine, http.MethodDelete, targets[2].path, bearer, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("delete unknown = %d", rec.Code)
	}
}
