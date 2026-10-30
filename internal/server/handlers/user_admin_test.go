package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/server/router"
)

// 这一组测试都走真实路由表（router.RegisterAll）+ 真实 Auth / AdminOnly 中间件 + 真实 handler。

type userAdminEnv struct {
	engine *gin.Engine
	ctx    context.Context
	admin  model.User
	token  string
}

func setupUserAdminEnv(t *testing.T) userAdminEnv {
	t.Helper()
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "user_admin_test.db"), false); err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := op.InitCache(); err != nil {
		t.Fatalf("init cache: %v", err)
	}
	ctx := context.Background()
	admin, err := op.UserCreate(model.UserCreateRequest{Username: "root", Password: "password", Role: model.UserRoleAdmin}, ctx)
	if err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	if err := router.RegisterAll(engine); err != nil {
		t.Fatal(err)
	}
	return userAdminEnv{engine: engine, ctx: ctx, admin: admin, token: jwtBearer(t, admin)}
}

func (e userAdminEnv) do(t *testing.T, method, target, token string, body any) (*httptest.ResponseRecorder, json.RawMessage) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, target, reader)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.engine.ServeHTTP(rec, req)
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &envelope)
	return rec, envelope.Data
}

func (e userAdminEnv) newUser(t *testing.T, name string, balance float64) model.User {
	t.Helper()
	u, err := op.UserCreate(model.UserCreateRequest{Username: name, Password: "password", Role: model.UserRoleUser, Balance: balance, Note: "keep-me"}, e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestUserAdminRoutesRejectNonAdmin(t *testing.T) {
	env := setupUserAdminEnv(t)
	plain := env.newUser(t, "plain", 1)
	userToken := jwtBearer(t, plain)
	cases := []struct{ method, target string }{
		{http.MethodPost, "/api/v1/user/balance"},
		{http.MethodGet, fmt.Sprintf("/api/v1/user/balance-log?id=%d", plain.ID)},
		{http.MethodGet, fmt.Sprintf("/api/v1/user/usage?id=%d", plain.ID)},
		{http.MethodPost, "/api/v1/user/batch"},
	}
	for _, c := range cases {
		for _, token := range []string{"", userToken} {
			rec, _ := env.do(t, c.method, c.target, token, map[string]any{"id": plain.ID, "mode": "add", "amount": 5, "ids": []int{plain.ID}, "action": "disable"})
			if rec.Code == http.StatusOK {
				t.Fatalf("%s %s with token=%v must be refused, got 200 %s", c.method, c.target, token != "", rec.Body.String())
			}
		}
	}
	if got, _ := op.UserGet(plain.ID); got.Balance != 1 || got.Status != model.UserStatusActive {
		t.Fatalf("refused request changed the user: %#v", got)
	}
}

func TestUserBalanceEndpointAddSubtractSetAndLog(t *testing.T) {
	env := setupUserAdminEnv(t)
	u := env.newUser(t, "wallet", 10)

	rec, data := env.do(t, http.MethodPost, "/api/v1/user/balance", env.token, map[string]any{"id": u.ID, "mode": "add", "amount": 2.5, "reason": "线下转账"})
	if rec.Code != http.StatusOK {
		t.Fatalf("add: %d %s", rec.Code, rec.Body.String())
	}
	var res model.UserBalanceAdjustResponse
	if err := json.Unmarshal(data, &res); err != nil {
		t.Fatal(err)
	}
	if res.User.Balance != 12.5 || res.Log.Delta != 2.5 || res.Log.ActorName != "root" || res.Log.Reason != "线下转账" {
		t.Fatalf("bad add response %#v", res)
	}
	if rec, _ := env.do(t, http.MethodPost, "/api/v1/user/balance", env.token, map[string]any{"id": u.ID, "mode": "subtract", "amount": 100}); rec.Code != http.StatusBadRequest {
		t.Fatalf("over-subtract must be 400, got %d", rec.Code)
	}
	if rec, _ := env.do(t, http.MethodPost, "/api/v1/user/balance", env.token, map[string]any{"id": u.ID, "mode": "subtract", "amount": 0.5}); rec.Code != http.StatusOK {
		t.Fatalf("subtract: %d %s", rec.Code, rec.Body.String())
	}
	if rec, _ := env.do(t, http.MethodPost, "/api/v1/user/balance", env.token, map[string]any{"id": u.ID, "mode": "set", "amount": 1}); rec.Code != http.StatusOK {
		t.Fatalf("set: %d %s", rec.Code, rec.Body.String())
	}
	if got, _ := op.UserGet(u.ID); got.Balance != 1 {
		t.Fatalf("balance after set = %v", got.Balance)
	}

	rec, data = env.do(t, http.MethodGet, fmt.Sprintf("/api/v1/user/balance-log?id=%d", u.ID), env.token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("log: %d %s", rec.Code, rec.Body.String())
	}
	var logs []model.UserBalanceLog
	_ = json.Unmarshal(data, &logs)
	if len(logs) != 3 || logs[0].Mode != "set" || logs[0].Delta != -11 || logs[2].Mode != "add" {
		t.Fatalf("bad log list %#v", logs)
	}
}

// /user/update 现在是部分更新：只发 note 不能清掉余额、身份、月卡；旧的整条发照样能用。
func TestUserUpdateEndpointIsPartial(t *testing.T) {
	env := setupUserAdminEnv(t)
	u := env.newUser(t, "partial", 9)
	limit, expire := 4.0, time.Now().Add(48*time.Hour).Unix()
	if _, err := op.UserPatch(model.UserPatchRequest{ID: u.ID, MonthlyLimit: &limit, MonthlyExpireAt: &expire}, 0, env.ctx); err != nil {
		t.Fatal(err)
	}
	if err := op.UserRecordUsage(u.ID, 1.5, env.ctx); err != nil {
		t.Fatal(err)
	}

	rec, _ := env.do(t, http.MethodPost, "/api/v1/user/update", env.token, map[string]any{"id": u.ID, "note": "new note"})
	if rec.Code != http.StatusOK {
		t.Fatalf("partial update: %d %s", rec.Code, rec.Body.String())
	}
	got, _ := op.UserGet(u.ID)
	if got.Note != "new note" || got.Balance != 9 || got.MonthlyUsed != 1.5 || got.MonthlyLimit != 4 || got.Role != model.UserRoleUser || got.Username != "partial" {
		t.Fatalf("partial update wiped fields: %#v", got)
	}

	// 旧客户端整条发：仍然按请求里的值写。
	rec, _ = env.do(t, http.MethodPost, "/api/v1/user/update", env.token, map[string]any{
		"id": u.ID, "username": "partial2", "role": "user", "status": "disabled", "balance": 3,
		"monthly_limit": 0, "monthly_used": 0, "monthly_expire_at": 0, "monthly_reset_at": 0, "note": "",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("full update: %d %s", rec.Code, rec.Body.String())
	}
	got, _ = op.UserGet(u.ID)
	if got.Username != "partial2" || got.Status != model.UserStatusDisabled || got.Balance != 3 || got.Note != "" || got.MonthlyLimit != 0 {
		t.Fatalf("full update not applied: %#v", got)
	}

	// 最后一个管理员不能把自己降级。
	if rec, _ := env.do(t, http.MethodPost, "/api/v1/user/update", env.token, map[string]any{"id": env.admin.ID, "role": "user"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("demoting the last admin must fail, got %d", rec.Code)
	}

	// 旧的「直接改成」接口照样能用，只动余额 / 月卡。
	rec, _ = env.do(t, http.MethodPost, "/api/v1/user/quota", env.token, map[string]any{"id": u.ID, "balance": 7})
	if rec.Code != http.StatusOK {
		t.Fatalf("quota: %d %s", rec.Code, rec.Body.String())
	}
	got, _ = op.UserGet(u.ID)
	if got.Balance != 7 || got.Username != "partial2" || got.Status != model.UserStatusDisabled {
		t.Fatalf("quota path changed other fields: %#v", got)
	}
}

func TestUserBatchEndpoint(t *testing.T) {
	env := setupUserAdminEnv(t)
	a := env.newUser(t, "batch-a", 0)
	b := env.newUser(t, "batch-b", 1)

	rec, data := env.do(t, http.MethodPost, "/api/v1/user/batch", env.token, map[string]any{"ids": []int{a.ID, b.ID, env.admin.ID}, "action": "disable"})
	if rec.Code != http.StatusOK {
		t.Fatalf("batch: %d %s", rec.Code, rec.Body.String())
	}
	var res model.UserBatchResult
	_ = json.Unmarshal(data, &res)
	if len(res.Done) != 2 || len(res.Failed) != 1 || res.Failed[0].ID != env.admin.ID {
		t.Fatalf("admin must not disable himself in a batch: %#v", res)
	}
	for _, id := range []int{a.ID, b.ID} {
		if got, _ := op.UserGet(id); got.Status != model.UserStatusDisabled || got.Note != "keep-me" {
			t.Fatalf("user %d not disabled or note wiped: %#v", id, got)
		}
	}
	if got, _ := op.UserGet(env.admin.ID); got.Status != model.UserStatusActive {
		t.Fatal("admin was disabled")
	}

	rec, _ = env.do(t, http.MethodPost, "/api/v1/user/batch", env.token, map[string]any{"ids": []int{a.ID, b.ID}, "action": "add_balance", "amount": 2, "reason": "活动"})
	if rec.Code != http.StatusOK {
		t.Fatalf("batch add: %d %s", rec.Code, rec.Body.String())
	}
	if got, _ := op.UserGet(b.ID); got.Balance != 3 {
		t.Fatalf("batch add balance = %v", got.Balance)
	}
	if rec, _ := env.do(t, http.MethodPost, "/api/v1/user/batch", env.token, map[string]any{"ids": []int{a.ID}, "action": "nope"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad action must be 400, got %d", rec.Code)
	}
}

func TestUserUsageEndpoint(t *testing.T) {
	env := setupUserAdminEnv(t)
	u := env.newUser(t, "usage", 0)
	now := time.Now().Unix()
	logs := []model.RelayLog{
		{ID: 101, UserID: u.ID, Time: now, RequestModelName: "m1", Cost: 2, InputTokens: 10, OutputTokens: 5},
		{ID: 102, UserID: u.ID, Time: now, RequestModelName: "m2", Cost: 1, Error: "x"},
		{ID: 103, UserID: env.admin.ID, Time: now, RequestModelName: "m1", Cost: 50},
	}
	if err := db.GetDB().Create(&logs).Error; err != nil {
		t.Fatal(err)
	}
	rec, data := env.do(t, http.MethodGet, fmt.Sprintf("/api/v1/user/usage?id=%d&range=today", u.ID), env.token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("usage: %d %s", rec.Code, rec.Body.String())
	}
	var usage model.UserUsageResponse
	_ = json.Unmarshal(data, &usage)
	if usage.RequestSuccess != 1 || usage.RequestFailed != 1 || usage.TotalCost != 3 || len(usage.TopModels) != 2 || usage.TopModels[0].Model != "m1" || len(usage.Daily) != 1 {
		t.Fatalf("bad usage %#v", usage)
	}
	if rec, _ := env.do(t, http.MethodGet, fmt.Sprintf("/api/v1/user/usage?id=%d&range=forever", u.ID), env.token, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad range must be 400, got %d", rec.Code)
	}
}
