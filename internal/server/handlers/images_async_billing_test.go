package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/price"
)

// 异步生图提交时的计费口径（走真实路由 + 真 APIKeyAuth）。

func imageAsyncTaskCount(t *testing.T) int64 {
	t.Helper()
	var n int64
	if err := db.GetDB().Model(&model.ImageTask{}).Count(&n).Error; err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	return n
}

func submitImageAsyncBody(env *imageAsyncEnv, apiKey, body string) (int, map[string]any, string) {
	rec := doImageAsyncRequest(env.engine, http.MethodPost, "/v1/images/generations/async", apiKey, body)
	var payload map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &payload)
	return rec.Code, payload, rec.Body.String()
}

// 没价的模型：普通用户提交直接 400，说清是哪个模型、找管理员配价；不扣钱、不建任务。
// （照 new-api 的 "model price not configured"。）
func TestImageAsyncSubmitRejectsUnpricedModelForUsers(t *testing.T) {
	env := setupImageAsyncEnv(t, true)
	price.SetLLMPrice(imageAsyncTestModel, model.LLMPrice{})

	user, key := createImageAsyncUserKey(t, env.ctx, "async-unpriced-user", "sk-octopus-async-unpriced", false, 5.0)
	before := imageAsyncTaskCount(t)
	code, _, body := submitImageAsyncBody(env, key.APIKey, `{"model":"`+imageAsyncTestModel+`","prompt":"x"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("submit = %d %s, want 400", code, body)
	}
	if !strings.Contains(body, imageAsyncTestModel) || !strings.Contains(body, "还没有配置价格") || !strings.Contains(body, "no price configured") {
		t.Fatalf("error must name the model and say the price is missing: %s", body)
	}
	if got := mustHandlerUser(t, user.ID).Balance; got != 5.0 {
		t.Fatalf("balance = %v, want 5.0", got)
	}
	if after := imageAsyncTaskCount(t); after != before {
		t.Fatalf("no task may be created, count %d -> %d", before, after)
	}
	if *env.hits != 0 {
		t.Fatalf("upstream must not be called, got %d", *env.hits)
	}
}

// 管理员从不扣费（等同 new-api 的自用模式）：没价模型对管理员放行。
func TestImageAsyncSubmitAllowsUnpricedModelForAdmin(t *testing.T) {
	env := setupImageAsyncEnv(t, true)
	price.SetLLMPrice(imageAsyncTestModel, model.LLMPrice{})

	_, key := createImageAsyncUserKey(t, env.ctx, "async-unpriced-admin", "sk-octopus-async-unpriced-admin", true, 0)
	code, _, body := submitImageAsyncBody(env, key.APIKey, `{"model":"`+imageAsyncTestModel+`","prompt":"x"}`)
	if code != http.StatusAccepted {
		t.Fatalf("admin submit = %d %s, want 202", code, body)
	}
}

// 按张计费：预扣「单价 × n × 倍率」，口径冻结到任务行上。
func TestImageAsyncSubmitPerImagePrechargesUnitTimesCount(t *testing.T) {
	env := setupImageAsyncEnv(t, true)
	price.SetLLMPrice(imageAsyncTestModel, model.LLMPrice{PerCall: 0.5, Output: 40})
	t.Cleanup(func() { price.SetLLMPrice(imageAsyncTestModel, model.LLMPrice{}) })

	user, key := createImageAsyncUserKey(t, env.ctx, "async-perimage-user", "sk-octopus-async-perimage", false, 5.0)
	code, payload, body := submitImageAsyncBody(env, key.APIKey, `{"model":"`+imageAsyncTestModel+`","prompt":"x","n":3}`)
	if code != http.StatusAccepted {
		t.Fatalf("submit = %d %s", code, body)
	}
	if got := mustHandlerUser(t, user.ID).Balance; got != 3.5 {
		t.Fatalf("balance = %v, want 3.5 (0.5 × 3 pre-charged)", got)
	}
	taskID, _ := payload["id"].(string)
	task, err := op.ImageTaskGetForOwner(env.ctx, taskID, key.ID, key.UserID)
	if err != nil || task == nil {
		t.Fatalf("read task: %v", err)
	}
	if task.Cost != 1.5 || task.BillingMode != "per_image" || task.ImageCount != 3 {
		t.Fatalf("task billing = cost %v mode %q count %d", task.Cost, task.BillingMode, task.ImageCount)
	}
}

// 按 token 计费：提交不预扣（只占在途预留），token 单价冻结到任务行上，完成时结算。
func TestImageAsyncSubmitTokenPricedFreezesPriceWithoutPrecharge(t *testing.T) {
	env := setupImageAsyncEnv(t, true)
	price.SetLLMPrice(imageAsyncTestModel, model.LLMPrice{Input: 5, Output: 40})
	t.Cleanup(func() { price.SetLLMPrice(imageAsyncTestModel, model.LLMPrice{}) })

	user, key := createImageAsyncUserKey(t, env.ctx, "async-token-user", "sk-octopus-async-token", false, 5.0)
	code, payload, body := submitImageAsyncBody(env, key.APIKey, `{"model":"`+imageAsyncTestModel+`","prompt":"x"}`)
	if code != http.StatusAccepted {
		t.Fatalf("submit = %d %s", code, body)
	}
	if got := mustHandlerUser(t, user.ID).Balance; got != 5.0 {
		t.Fatalf("balance = %v, want 5.0 (token-priced tasks settle at completion)", got)
	}
	taskID, _ := payload["id"].(string)
	task, err := op.ImageTaskGetForOwner(env.ctx, taskID, key.ID, key.UserID)
	if err != nil || task == nil {
		t.Fatalf("read task: %v", err)
	}
	var snap model.AccessPlanBillingSnapshot
	if err := json.Unmarshal([]byte(task.BillingSnapshot), &snap); err != nil {
		t.Fatalf("snapshot %q: %v", task.BillingSnapshot, err)
	}
	if task.Cost != 0 || task.BillingMode != "token" || snap.BaseInputPrice != 5 || snap.BaseOutputPrice != 40 || snap.BillingModelName != imageAsyncTestModel {
		t.Fatalf("task billing = cost %v mode %q snapshot %+v", task.Cost, task.BillingMode, snap)
	}
}
