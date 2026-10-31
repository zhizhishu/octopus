package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
)

// 端到端走 HTTP，用前端真实发的请求体：方案页保存不清提示词，提示词页保存不改路由。

func promptSepRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/update", updateAccessPlan)
	r.POST("/prompt/update", updateAccessPlanPrompt)
	r.POST("/route-prompt/update", updateAccessPlanRoutePrompt)
	r.POST("/route-target/update", updateAccessRouteTarget)
	return r
}

func promptSepPost(t *testing.T, r *gin.Engine, path string, body any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s -> %d %s", path, rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out
}

// waitAccessPlanSync waits for the background channel sync that /update schedules, so it
// can't hold the sqlite write lock while the next request writes.
func waitAccessPlanSync() {
	time.Sleep(150 * time.Millisecond)
	accessPlanSyncMu.Lock()
	accessPlanSyncMu.Unlock()
}

func TestPlanPageAndPromptPageSaveSeparately(t *testing.T) {
	waitAccessPlanSync() // a sync left over from an earlier test in this package
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "octopus.db"), false); err != nil {
		t.Fatalf("init db: %v", err)
	}
	if err := op.InitCache(); err != nil {
		t.Fatalf("init cache: %v", err)
	}
	t.Cleanup(func() {
		// /update 会在后台跑一次方案渠道同步；等它跑完再关库。
		waitAccessPlanSync()
		_ = db.Close()
	})
	ctx := context.Background()
	ch := model.Channel{Name: "sep-ch", Enabled: true, Model: "gpt-x"}
	if err := op.ChannelCreate(&ch, ctx); err != nil {
		t.Fatal(err)
	}
	plans, _ := op.AccessPlanList(ctx)
	var vip model.AccessPlan
	for _, p := range plans {
		if p.Slug == "vip" {
			vip = p
		}
	}
	r := promptSepRouter()

	routeTable := map[string]any{"access_plan_id": vip.ID, "targets": []map[string]any{{
		"request_model": "gpt-x", "channel_id": ch.ID, "upstream_model": "gpt-x", "priority": 1, "weight": 1,
		"enabled": true, "billing_model_source": "request_model", "mode": 3, "fallback_mode": "return_group",
	}}}
	promptSepPost(t, r, "/route-target/update", routeTable)

	// 提示词页：方案层 + 路由层
	promptSepPost(t, r, "/prompt/update", map[string]any{"id": vip.ID, "system_prompt_override": "PLAN", "prompt_override_mode": "after", "prompt_override_enabled": true})
	promptSepPost(t, r, "/route-prompt/update", map[string]any{"access_plan_id": vip.ID, "request_model": "gpt-x", "system_prompt_override": "ROUTE", "prompt_override_mode": "before", "prompt_override_enabled": true})

	// 方案页：方案设置保存（和 PlanBasicsEditor 发的完全一样，不带提示词字段）+ 路由整表保存
	promptSepPost(t, r, "/update", map[string]any{"id": vip.ID, "display_name": "VIP2", "slug": "vip", "enabled": true, "is_default": vip.IsDefault})
	waitAccessPlanSync()
	routeTable["targets"].([]map[string]any)[0]["fallback_mode"] = "none"
	promptSepPost(t, r, "/route-target/update", routeTable)

	plans, _ = op.AccessPlanList(ctx)
	for _, p := range plans {
		if p.ID != vip.ID {
			continue
		}
		if p.DisplayName != "VIP2" {
			t.Fatalf("plan save not applied: %q", p.DisplayName)
		}
		if p.SystemPromptOverride != "PLAN" || p.PromptOverrideMode != model.PromptOverrideModeAfter || !p.PromptOverrideEnabled {
			t.Fatalf("plan page save wiped the plan prompt: %+v", p)
		}
		found := false
		for _, target := range p.RouteTargets {
			if target.RequestModel != "gpt-x" {
				continue
			}
			found = true
			if target.FallbackMode != model.AccessRouteFallbackNone {
				t.Fatalf("route save not applied: %v", target.FallbackMode)
			}
			if target.SystemPromptOverride != "ROUTE" || target.PromptOverrideMode != model.PromptOverrideModeBefore || !target.PromptOverrideEnabled {
				t.Fatalf("plan page route save wiped the route prompt: %+v", target)
			}
		}
		if !found {
			t.Fatalf("route missing")
		}
	}

	// 老客户端：/update 里显式带了提示词字段，照样能写（只写那几列）。
	out := promptSepPost(t, r, "/update", map[string]any{"id": vip.ID, "display_name": "VIP2", "slug": "vip", "enabled": true, "is_default": vip.IsDefault, "system_prompt_override": "OLD", "prompt_override_mode": "append_system"})
	data := out["data"].(map[string]any)
	if data["system_prompt_override"] != "OLD" || data["prompt_override_mode"] != "before" || data["prompt_override_enabled"] != true {
		t.Fatalf("old client prompt write: %+v", data)
	}
}
