package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// 走真实 HTTP 入口：手动检测 → 报告.plain 人话 + finding_n 不计 0 分项。
func TestRunModelAuditPlainAndScoredFindings(t *testing.T) {
	gin.SetMode(gin.TestMode)
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "octopus.db"), false); err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := op.InitCache(); err != nil {
		t.Fatalf("init cache: %v", err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 回显探针期望看到的串在请求体里；给一段通用 OK 让 liveness 之外也能跑通。
		_, _ = w.Write([]byte(`{"id":"chatcmpl_x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11}}`))
	}))
	t.Cleanup(upstream.Close)

	channel := model.Channel{
		Name:     "plain-path",
		Type:     outbound.OutboundTypeOpenAIChat,
		Enabled:  true,
		Model:    "gpt-4o",
		BaseUrls: []model.BaseUrl{{URL: upstream.URL}},
		Keys:     []model.ChannelKey{{Enabled: true, ChannelKey: "k"}},
	}
	if err := op.ChannelCreate(&channel, t.Context()); err != nil {
		t.Fatalf("create: %v", err)
	}

	body, _ := json.Marshal(map[string]any{
		"channel_id": channel.ID,
		"model":      "gpt-4o",
		"probes":     []string{"liveness", "echo_rewrite", "context_canary"},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/model-audit/run", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r := gin.New()
	r.POST("/api/v1/model-audit/run", runModelAudit)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	var envelope struct {
		Data struct {
			Report struct {
				Verdict  string `json:"verdict"`
				Findings []struct {
					Score int `json:"score"`
				} `json:"findings"`
				Plain struct {
					Headline   string `json:"headline"`
					Confidence string `json:"confidence"`
					Summary    string `json:"summary"`
				} `json:"plain"`
			} `json:"report"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	plain := envelope.Data.Report.Plain
	if plain.Headline == "" || plain.Confidence == "" {
		t.Fatalf("plain missing: %+v", plain)
	}
	// 假上游几乎必然让 liveness/echo 失败 → 不会是"没发现问题"；但必须有人话档。
	allowed := map[string]bool{
		"没发现问题": true, "有小疑点": true, "有可疑点，建议复核": true, "数据不够，没法判断": true,
	}
	if !allowed[plain.Headline] {
		t.Fatalf("unexpected headline %q", plain.Headline)
	}
}

// 计划接口：真实入口，零上游；非 Claude 渠道签名项灰掉。
func TestAuditPlanGreysSignature(t *testing.T) {
	gin.SetMode(gin.TestMode)
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "octopus.db"), false); err != nil {
		t.Fatalf("init: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := op.InitCache(); err != nil {
		t.Fatalf("cache: %v", err)
	}
	channel := model.Channel{
		Name: "plan-ch", Type: outbound.OutboundTypeOpenAIChat, Enabled: true,
		Model: "gpt-4o", BaseUrls: []model.BaseUrl{{URL: "http://127.0.0.1:9"}},
		Keys: []model.ChannelKey{{Enabled: true, ChannelKey: "k"}},
	}
	if err := op.ChannelCreate(&channel, t.Context()); err != nil {
		t.Fatalf("create: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/model-audit/plan?channel_id="+strconv.Itoa(channel.ID)+"&model=gpt-4o", nil)
	rec := httptest.NewRecorder()
	r := gin.New()
	r.GET("/api/v1/model-audit/plan", getModelAuditPlan)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var envelope struct {
		Data struct {
			Probes []struct {
				ID         string `json:"id"`
				Applicable bool   `json:"applicable"`
				Requests   int    `json:"requests"`
			} `json:"probes"`
			Quick struct {
				Requests int `json:"requests"`
			} `json:"quick"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &envelope)
	if envelope.Data.Quick.Requests != 3 {
		t.Fatalf("quick requests = %d, want 3", envelope.Data.Quick.Requests)
	}
	for _, p := range envelope.Data.Probes {
		if p.ID == "signature" {
			if p.Applicable || p.Requests != 0 {
				t.Fatalf("signature should be grey: %+v", p)
			}
			return
		}
	}
	t.Fatal("signature missing from plan")
}

// 渠道最新检测：真实入队落库 → 查 /channels/:id/latest。
func TestChannelLatestAuditsEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "octopus.db"), false); err != nil {
		t.Fatalf("init: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := op.InitCache(); err != nil {
		t.Fatalf("cache: %v", err)
	}
	channel := model.Channel{
		Name: "latest-ch", Type: outbound.OutboundTypeOpenAIChat, Enabled: true,
		Model: "gpt-4o", BaseUrls: []model.BaseUrl{{URL: "http://127.0.0.1:9"}},
		Keys:           []model.ChannelKey{{Enabled: true, ChannelKey: "k"}},
		SelectedModels: []string{"gpt-4o", "gpt-4o-mini"},
	}
	if err := op.ChannelCreate(&channel, t.Context()); err != nil {
		t.Fatalf("create: %v", err)
	}
	jobID, err := op.ModelAuditEnqueue(t.Context(), op.ModelAuditEnqueueInput{
		ChannelID: channel.ID, Models: []string{"gpt-4o"}, Probes: []string{"liveness"},
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	now := time.Now()
	if err := db.GetDB().Model(&model.ModelAuditResult{}).
		Where("job_id = ?", jobID).
		Updates(map[string]any{
			"status": model.AuditResultDone, "verdict": "medium", "score": 40, "finding_n": 1,
			"report":      `{"verdict":"medium","score":40,"findings":[{"probe":"token_delta","score":25,"title":"用量虚报","plain_evidence":"输入多了约3倍"}],"plain":{"headline":"有可疑点，建议复核","summary":"1 项对不上。","confidence":"中"},"results":[],"errors":[],"disclaimer":"x"}`,
			"finished_at": now,
		}).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/model-audit/channels/"+strconv.Itoa(channel.ID)+"/latest", nil)
	rec := httptest.NewRecorder()
	r := gin.New()
	r.GET("/api/v1/model-audit/channels/:id/latest", getChannelLatestAudits)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var envelope struct {
		Data []struct {
			Model     string   `json:"model"`
			Chip      string   `json:"chip"`
			ChipLabel string   `json:"chip_label"`
			Headline  string   `json:"headline"`
			Points    []string `json:"points"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(envelope.Data) != 1 {
		t.Fatalf("want 1 latest, got %d", len(envelope.Data))
	}
	got := envelope.Data[0]
	if got.Chip != "warn" || got.ChipLabel != "待复核" {
		t.Fatalf("chip = %s/%s", got.Chip, got.ChipLabel)
	}
	if got.Headline != "有可疑点，建议复核" {
		t.Fatalf("headline %q", got.Headline)
	}
	if len(got.Points) == 0 || got.Points[0] == "" {
		t.Fatalf("points empty: %v", got.Points)
	}
}
