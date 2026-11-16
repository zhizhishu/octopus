package handlers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/redteamlab"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

func TestRedTeamV2RevisionAuthorizationAndBoundedTwoTurnRun(t *testing.T) {
	ctx := setupSettingAuthTestDB(t)
	admin := seedSettingAuthSiteSecrets(t, ctx)
	token := jwtBearer(t, admin)
	route := newRedTeamLabRouter()
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := requests.Add(1)
		// Never use any actual model/provider secret in these fixtures.
		if r.Header.Get("Authorization") != "Bearer sk-v2-fake-upstream" {
			t.Errorf("wrong outgoing auth fixture")
		}
		w.Header().Set("Content-Type", "application/json")
		answer := "acknowledged"
		if count == 2 {
			answer = "OCT_V2_MARKER"
		}
		_, _ = fmt.Fprintf(w, `{"id":"chatcmpl-v2","object":"chat.completion","model":"model-v2","choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":4,"total_tokens":14}}`, answer)
	}))
	defer upstream.Close()
	channel := model.Channel{Name: "v2-fake-target", Type: outbound.OutboundTypeOpenAIChat, Enabled: true, Model: "model-v2", BaseUrls: []model.BaseUrl{{URL: upstream.URL}}, Keys: []model.ChannelKey{{Enabled: true, ChannelKey: "sk-v2-fake-upstream"}}}
	if err := op.ChannelCreate(&channel, ctx); err != nil {
		t.Fatal(err)
	}

	spec := redteamlab.V2CaseSpec{Schema: redteamlab.V2Schema, ID: "v2-sample", Name: "边界测试", Category: "prompt_injection.direct", Prompts: []string{"Describe the policy boundary clearly.", "Now summarize the boundary in one sentence."}, Expected: "OCT_V2_MARKER", MaxTokens: 64, SourceKind: "manual", SourceLicense: "original"}
	if got := redTeamRequest(route, http.MethodPost, "/api/v1/redteam-lab/v2/cases", adminTokenBearer(), spec); got.Code != http.StatusForbidden {
		t.Fatalf("automation accessed v2: %d", got.Code)
	}
	if got := redTeamRequest(route, http.MethodPost, "/api/v1/redteam-lab/v2/cases", token, spec); got.Code != http.StatusOK {
		t.Fatalf("save case: %d %s", got.Code, got.Body)
	}
	if got := redTeamRequest(route, http.MethodPost, "/api/v1/redteam-lab/v2/cases", token, spec); got.Code != http.StatusOK {
		t.Fatalf("idempotent identical case: %d", got.Code)
	}
	var count int64
	if err := db.GetDB().Model(&model.RedTeamV2Revision{}).Where("case_id = ?", spec.ID).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("duplicate revision: %d %v", count, err)
	}
	spec.Name = "边界测试改版"
	if got := redTeamRequest(route, http.MethodPost, "/api/v1/redteam-lab/v2/cases", token, spec); got.Code != http.StatusOK {
		t.Fatalf("save v2 case revision: %d %s", got.Code, got.Body)
	}
	if got := redTeamRequest(route, http.MethodGet, "/api/v1/redteam-lab/v2/cases/v2-sample?revision=1", token, nil); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), "边界测试") {
		t.Fatalf("revision retrieval: %d %s", got.Code, got.Body)
	}
	if got := redTeamRequest(route, http.MethodGet, "/api/v1/redteam-lab/v2/cases", token, nil); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"revision":2`) {
		t.Fatalf("list newest case: %d %s", got.Code, got.Body)
	}
	if got := redTeamRequest(route, http.MethodGet, "/api/v1/redteam-lab/v2/cases/v2-sample/revisions", token, nil); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"revision":1`) || !strings.Contains(got.Body.String(), `"revision":2`) {
		t.Fatalf("version history: %d %s", got.Code, got.Body)
	}
	if got := redTeamRequest(route, http.MethodGet, "/api/v1/redteam-lab/v2/cases/v2-sample/revisions", token, nil); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"revision":1`) || !strings.Contains(got.Body.String(), `"revision":2`) {
		t.Fatalf("revision history: %d %s", got.Code, got.Body)
	}

	run := map[string]any{"case_id": spec.ID, "revision": 2, "channel_id": channel.ID, "model": "model-v2", "confirm_cost": true, "idempotency_key": "v2-test-unique-idempotent-key-001"}
	if got := redTeamRequest(route, http.MethodPost, "/api/v1/redteam-lab/v2/runs", token, run); got.Code != http.StatusForbidden {
		t.Fatalf("ran before enable: %d", got.Code)
	}
	if requests.Load() != 0 {
		t.Fatal("disabled V2 made a paid send")
	}
	invalidImport := map[string]any{"confirmed_license": true, "cases": []redteamlab.V2CaseSpec{{Schema: redteamlab.V2Schema, ID: "bad-import", Name: "bad", Category: "tests", Prompts: []string{"sample import content"}, MaxTokens: 64, SourceKind: "imported", SourceLicense: "AGPL", SourceURL: "https://example.com"}}}
	if got := redTeamRequest(route, http.MethodPost, "/api/v1/redteam-lab/v2/cases/import", token, invalidImport); got.Code != http.StatusBadRequest {
		t.Fatalf("unsafe license accepted: %d", got.Code)
	}

	cfg := redteamlab.Config{Enabled: true, Cases: []redteamlab.Case{}}
	if got := redTeamRequest(route, http.MethodPut, "/api/v1/redteam-lab/config", token, cfg); got.Code != http.StatusOK {
		t.Fatalf("enable: %d %s", got.Code, got.Body)
	}
	if got := redTeamRequest(route, http.MethodPost, "/api/v1/redteam-lab/v2/runs", adminTokenBearer(), run); got.Code != http.StatusForbidden {
		t.Fatalf("automation ran V2: %d", got.Code)
	}
	if got := redTeamRequest(route, http.MethodPost, "/api/v1/redteam-lab/v2/runs", token, run); got.Code != http.StatusOK {
		t.Fatalf("start V2 run: %d %s", got.Code, got.Body)
	}
	// Same key must not enqueue, even while first run is still pending.
	if got := redTeamRequest(route, http.MethodPost, "/api/v1/redteam-lab/v2/runs", token, run); got.Code != http.StatusOK {
		t.Fatalf("idempotent replay: %d %s", got.Code, got.Body)
	}
	conflicting := map[string]any{"case_id": spec.ID, "revision": 1, "channel_id": channel.ID, "model": "model-v2", "confirm_cost": true, "idempotency_key": run["idempotency_key"]}
	if got := redTeamRequest(route, http.MethodPost, "/api/v1/redteam-lab/v2/runs", token, conflicting); got.Code != http.StatusConflict {
		t.Fatalf("same key with a different version allowed: %d", got.Code)
	}
	var completed model.RedTeamV2Run
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if err := db.GetDB().Where("idempotency_key = ?", run["idempotency_key"]).First(&completed).Error; err != nil {
			t.Fatal(err)
		}
		if completed.Status == "completed" {
			break
		}
		if completed.Status != "queued" && completed.Status != "running" {
			t.Fatalf("unexpected state %s: %s", completed.Status, completed.ErrorMessage)
		}
		time.Sleep(25 * time.Millisecond)
	}
	if completed.Status != "completed" || completed.Verdict != "matched" || completed.TargetRequests != 2 || requests.Load() != 2 {
		t.Fatalf("run %+v outbound calls=%d", completed, requests.Load())
	}
	var turns []model.RedTeamV2Turn
	if err := db.GetDB().Where("run_id = ?", completed.ID).Order("sequence").Find(&turns).Error; err != nil || len(turns) != 2 {
		t.Fatalf("recorded turns %d %v", len(turns), err)
	}
	if got := redTeamRequest(route, http.MethodGet, fmt.Sprintf("/api/v1/redteam-lab/v2/runs/%d", completed.ID), token, nil); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), "OCT_V2_MARKER") {
		t.Fatalf("details %d %s", got.Code, got.Body)
	}
	if got := redTeamRequest(route, http.MethodPost, fmt.Sprintf("/api/v1/redteam-lab/v2/runs/%d/cancel", completed.ID), token, map[string]any{}); got.Code != http.StatusConflict {
		t.Fatalf("finished run cancelled: %d", got.Code)
	}
	var lane model.RedTeamV2Lane
	if err := db.GetDB().First(&lane, 1).Error; err != nil || lane.ActiveRunID != 0 {
		t.Fatalf("lane not released %+v %v", lane, err)
	}
	reviewPath := fmt.Sprintf("/api/v1/redteam-lab/v2/runs/%d/reviews", completed.ID)
	if got := redTeamRequest(route, http.MethodPost, reviewPath, adminTokenBearer(), map[string]string{"decision": "confirmed", "reason": "人工证据说明"}); got.Code != http.StatusForbidden {
		t.Fatalf("automation reviewed run: %d", got.Code)
	}
	if got := redTeamRequest(route, http.MethodPost, reviewPath, token, map[string]string{"decision": "pwned", "reason": "无效状态不能写"}); got.Code != http.StatusBadRequest {
		t.Fatalf("invalid review accepted: %d", got.Code)
	}
	if got := redTeamRequest(route, http.MethodPost, reviewPath, token, map[string]string{"decision": "uncertain", "reason": "人类无法根据字面标记认定绕过真实安全机制"}); got.Code != http.StatusOK {
		t.Fatalf("human review not saved: %d %s", got.Code, got.Body)
	}
	if got := redTeamRequest(route, http.MethodGet, fmt.Sprintf("/api/v1/redteam-lab/v2/runs/%d", completed.ID), token, nil); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"reviews":[{"id":`) {
		t.Fatalf("review not in detail: %d %s", got.Code, got.Body)
	}
	var sameRun model.RedTeamV2Run
	if err := db.GetDB().First(&sameRun, completed.ID).Error; err != nil || sameRun.Verdict != "matched" || requests.Load() != 2 {
		t.Fatalf("human review mutated machine verdict or upstream requests: %+v %v", sameRun, err)
	}
}

func TestRedTeamV2RunInvalidConfigAndIdentityIsFailClosed(t *testing.T) {
	ctx := setupSettingAuthTestDB(t)
	admin := seedSettingAuthSiteSecrets(t, ctx)
	user, err := op.UserCreate(model.UserCreateRequest{Username: "v2-normal-user", Password: "password", Role: model.UserRoleUser, Status: model.UserStatusActive}, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	route := newRedTeamLabRouter()
	adminJWT := jwtBearer(t, admin)
	userJWT := jwtBearer(t, user)
	req := map[string]any{"case_id": "unknown", "revision": 1, "channel_id": 10000, "model": "model", "confirm_cost": true, "idempotency_key": "v2-unknown-case-0123456789"}
	for _, token := range []string{userJWT, adminTokenBearer()} {
		for _, path := range []string{"/api/v1/redteam-lab/v2/cases", "/api/v1/redteam-lab/v2/runs"} {
			if got := redTeamRequest(route, http.MethodGet, path, token, nil); got.Code != http.StatusForbidden {
				t.Fatalf("unauthorized read %s: %d", path, got.Code)
			}
		}
		if got := redTeamRequest(route, http.MethodPost, "/api/v1/redteam-lab/v2/runs", token, req); got.Code != http.StatusForbidden {
			t.Fatalf("unauthorized send %d", got.Code)
		}
	}
	if got := redTeamRequest(route, http.MethodPost, "/api/v1/redteam-lab/v2/runs", adminJWT, req); got.Code != http.StatusForbidden {
		t.Fatalf("disabled lab allowed unknown case: %d", got.Code)
	}
}

func TestRedTeamV2CaseImportIsAtomic(t *testing.T) {
	ctx := setupSettingAuthTestDB(t)
	admin := seedSettingAuthSiteSecrets(t, ctx)
	route := newRedTeamLabRouter()
	spec := redteamlab.V2CaseSpec{Schema: redteamlab.V2Schema, ID: "imported_a", Name: "import A", Category: "test.seed", Prompts: []string{"Check the allowed boundary"}, MaxTokens: 64, SourceKind: "imported", SourceLicense: "MIT", SourceURL: "https://github.com/example/original/blob/abc/examples.json"}
	cases := []redteamlab.V2CaseSpec{spec, spec}
	cases[1].ID = "imported_b"
	cases[1].SourceLicense = "AGPL"
	got := redTeamRequest(route, http.MethodPost, "/api/v1/redteam-lab/v2/cases/import", jwtBearer(t, admin), map[string]any{"confirmed_license": true, "cases": cases})
	if got.Code != http.StatusBadRequest {
		t.Fatalf("accepted partly unlicensed batch: %d", got.Code)
	}
	var number int64
	if err := db.GetDB().Model(&model.RedTeamV2Case{}).Count(&number).Error; err != nil || number != 0 {
		t.Fatalf("partial import: %d %v", number, err)
	}
	cases[1].SourceLicense = "MIT"
	got = redTeamRequest(route, http.MethodPost, "/api/v1/redteam-lab/v2/cases/import", jwtBearer(t, admin), map[string]any{"confirmed_license": true, "cases": cases})
	if got.Code != http.StatusOK {
		t.Fatalf("valid import: %d %s", got.Code, got.Body)
	}
	if err := db.GetDB().Model(&model.RedTeamV2Case{}).Count(&number).Error; err != nil || number != 2 {
		t.Fatalf("atomic import %d %v", number, err)
	}
}
