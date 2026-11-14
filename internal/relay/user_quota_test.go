package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
)

func relayQuotaTestUser(t *testing.T, ctx context.Context) model.User {
	t.Helper()
	user, err := op.UserCreate(model.UserCreateRequest{Username: "relay-quota-user", Password: "test-password", Role: model.UserRoleUser, Status: model.UserStatusActive, Balance: 100}, ctx)
	if err != nil {
		t.Fatal(err)
	}
	return user
}

func TestUserQuotaRawHandlerKeepsPayloadAndRejectsBeforeUpstream(t *testing.T) {
	ctx := setupRelayErrorDB(t)
	user := relayQuotaTestUser(t, ctx)
	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"index":0,"relevance_score":0.8}],"usage":{"prompt_tokens":0,"completion_tokens":0}}`))
	}))
	t.Cleanup(upstream.Close)
	createRawProtocolGroup(t, ctx, upstream.URL, "quota-request-model", "quota-upstream-model", "test-key")
	run := func(body, models string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/rerank", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Set("user_id", user.ID)
		c.Set("api_key_id", 0)
		c.Set("request_ip", "127.0.0.1")
		c.Set("supported_models", models)
		RawProtocolHandler(RawProtocolOptions{Endpoint: "/rerank", Name: "rerank"}, c)
		return rec
	}
	body := `{"model":"quota-request-model","query":"hello","documents":["world"]}`
	baseline := run(body, "")
	if baseline.Code != http.StatusOK {
		t.Fatalf("baseline=%d %s", baseline.Code, baseline.Body.String())
	}
	_, err := op.UserQuotaPolicySave(ctx, model.UserQuotaPolicy{UserID: user.ID, Enabled: true, Timezone: "UTC", FiveHourRequests: 1, AllowedModels: "quota-request-model"})
	if err != nil {
		t.Fatal(err)
	}
	if bad := run("{", ""); bad.Code != http.StatusBadRequest {
		t.Fatalf("malformed=%d", bad.Code)
	}
	if denied := run(body, "some-other-model"); denied.Code != http.StatusBadRequest {
		t.Fatalf("key restriction=%d", denied.Code)
	}
	accepted := run(body, "quota-request-model")
	if accepted.Code != http.StatusOK {
		t.Fatalf("accepted=%d %s", accepted.Code, accepted.Body.String())
	}
	if baseline.Body.String() != accepted.Body.String() {
		t.Fatalf("policy changed payload\nbaseline=%s\npolicy=%s", baseline.Body.String(), accepted.Body.String())
	}
	denied := run(body, "quota-request-model")
	if denied.Code != http.StatusPaymentRequired {
		t.Fatalf("cap=%d %s", denied.Code, denied.Body.String())
	}
	if upstreamCalls.Load() != 2 {
		t.Fatalf("invalid/rejected calls reached fake upstream: %d", upstreamCalls.Load())
	}
	state, err := op.UserQuotaStatusGet(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.PendingReceipts != 0 {
		t.Fatalf("completed request leaked reservation: %+v", state)
	}
	for _, w := range state.Windows {
		if w.Accepted != 1 || w.FailedAccepted != 0 || w.CostReserved != 0 {
			t.Fatalf("handler window=%+v", w)
		}
	}
}

func TestUserQuotaRealtimeIsExplicitlyRefused(t *testing.T) {
	ctx := setupRelayErrorDB(t)
	user := relayQuotaTestUser(t, ctx)
	if _, err := op.UserQuotaPolicySave(ctx, model.UserQuotaPolicy{UserID: user.ID, Enabled: true, Timezone: "UTC", FiveHourRequests: 1}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/realtime?model=test", nil)
	c.Request.Header.Set("Connection", "Upgrade")
	c.Request.Header.Set("Upgrade", "websocket")
	c.Set("user_id", user.ID)
	GrokRealtimeHandler(c)
	if rec.Code != http.StatusPaymentRequired || !strings.Contains(rec.Body.String(), "user_quota_realtime_unavailable") {
		t.Fatalf("realtime=%d %s", rec.Code, rec.Body.String())
	}
	state, err := op.UserQuotaStatusGet(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range state.Windows {
		if w.Accepted != 0 {
			t.Fatal("refused realtime consumed a unit")
		}
	}
}

func TestUserQuotaSettlementFailureStaysVisibleAndDoesNotSplitDebit(t *testing.T) {
	ctx := setupRelayErrorDB(t)
	user := relayQuotaTestUser(t, ctx)
	if _, err := op.UserQuotaPolicySave(ctx, model.UserQuotaPolicy{UserID: user.ID, Enabled: true, Timezone: "UTC", FiveHourCost: 10}); err != nil {
		t.Fatal(err)
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/rerank", nil)
	c.Set("user_id", user.ID)
	quota, err := admitRelayUserQuota(c, "m")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.GetDB().Exec("CREATE TRIGGER quota_test_fail_settlement BEFORE UPDATE ON user_quota_receipts WHEN NEW.settled_at > 0 BEGIN SELECT RAISE(ABORT, 'injected quota settlement failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	quota.finish(ctx, 3, true)
	quota.close()
	got, _ := op.UserGet(user.ID)
	if got.Balance != 100 {
		t.Fatalf("failed receipt debit escaped transaction: %v", got.Balance)
	}
	var stored model.User
	if err := db.GetDB().First(&stored, user.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Balance != 100 {
		t.Fatalf("DB split debit: %v", stored.Balance)
	}
	state, err := op.UserQuotaStatusGet(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.PendingReceipts != 1 {
		t.Fatalf("failed receipt is not visible: %+v", state)
	}
	for _, w := range state.Windows {
		if w.CostUsed != 0 || w.CostReserved <= 0 || w.Accepted != 1 {
			t.Fatalf("pending reservation lost: %+v", w)
		}
	}
	if err := db.GetDB().Exec("DROP TRIGGER quota_test_fail_settlement").Error; err != nil {
		t.Fatal(err)
	}
	quota.close()
	got, _ = op.UserGet(user.ID)
	if got.Balance != 97 {
		t.Fatalf("retry forgot target actual cost: %v", got.Balance)
	}
}
