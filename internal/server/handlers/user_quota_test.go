package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
)

func TestUserLimitsRoutesAndOwnStatusScope(t *testing.T) {
	env := setupUserAdminEnv(t)
	first := env.newUser(t, "quota-first", 100)
	second := env.newUser(t, "quota-second", 100)
	endpoint := fmt.Sprintf("/api/v1/user-limits/%d", first.ID)
	policy := model.UserQuotaPolicy{Enabled: true, Timezone: "UTC", FiveHourRequests: 1}
	for _, token := range []string{"", jwtBearer(t, first)} {
		for _, method := range []string{http.MethodGet, http.MethodPut} {
			rec, _ := env.do(t, method, endpoint, token, policy)
			if rec.Code == http.StatusOK {
				t.Fatalf("ordinary/anonymous policy access allowed: %s %s", method, rec.Body.String())
			}
		}
	}
	oldStatus, oldStatusData := env.do(t, http.MethodGet, "/api/v1/user/status", jwtBearer(t, first), nil)
	if oldStatus.Code != http.StatusOK {
		t.Fatalf("old status=%d", oldStatus.Code)
	}
	beforeUser, _ := op.UserGet(first.ID)
	before, _ := json.Marshal(model.NewUserResponse(beforeUser))
	rec, data := env.do(t, http.MethodPut, endpoint, env.token, policy)
	if rec.Code != http.StatusOK {
		t.Fatalf("save=%d %s", rec.Code, rec.Body.String())
	}
	var saved model.UserQuotaStatus
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Policy == nil || saved.Policy.UserID != first.ID || saved.Policy.AnchorAt <= 0 {
		t.Fatalf("bad policy=%s", data)
	}
	newStatus, newStatusData := env.do(t, http.MethodGet, "/api/v1/user/status", jwtBearer(t, first), nil)
	if newStatus.Code != http.StatusOK || string(oldStatusData) != string(newStatusData) {
		t.Fatalf("old status payload changed: before=%s after=%s", oldStatusData, newStatusData)
	}
	afterUser, _ := op.UserGet(first.ID)
	after, _ := json.Marshal(model.NewUserResponse(afterUser))
	if string(before) != string(after) {
		t.Fatal("policy save changed existing user payload")
	}
	rec, data = env.do(t, http.MethodGet, fmt.Sprintf("/api/v1/user-limits/current?id=%d", first.ID), jwtBearer(t, second), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("own status=%d %s", rec.Code, rec.Body.String())
	}
	var own model.UserQuotaStatus
	if err := json.Unmarshal(data, &own); err != nil {
		t.Fatal(err)
	}
	if own.Policy != nil {
		t.Fatalf("current status leaked another user's policy: %s", data)
	}
	rec, _ = env.do(t, http.MethodPut, endpoint, env.token, policy)
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale policy overwrite=%d %s", rec.Code, rec.Body.String())
	}
}

func TestUserLimitsReadDoesNotConsumeAnAcceptedUnit(t *testing.T) {
	env := setupUserAdminEnv(t)
	user := env.newUser(t, "quota-reader", 100)
	if _, err := op.UserQuotaPolicySave(env.ctx, model.UserQuotaPolicy{UserID: user.ID, Enabled: true, Timezone: "UTC", FiveHourRequests: 1}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		rec, data := env.do(t, http.MethodGet, "/api/v1/user-limits/current", jwtBearer(t, user), nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d %s", rec.Code, rec.Body.String())
		}
		var state model.UserQuotaStatus
		if err := json.Unmarshal(data, &state); err != nil {
			t.Fatal(err)
		}
		for _, w := range state.Windows {
			if w.Accepted != 0 {
				t.Fatalf("read counted as generation: %s", data)
			}
		}
	}
}
