package handlers

import (
	"bytes"
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
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

func setupV6HandlerTest(t *testing.T) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "octopus.db"), false); err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
	})
	if err := op.InitCache(); err != nil {
		t.Fatalf("init cache: %v", err)
	}
}

func doJSON(t *testing.T, router *gin.Engine, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// 删除接口：单条删、批量删跳过在跑的、清空已完成，都走真实 handler。
func TestModelAuditDeleteEndpoints(t *testing.T) {
	setupV6HandlerTest(t)
	ch := model.Channel{Name: "del", Type: outbound.OutboundTypeOpenAIChat, Enabled: true}
	if err := op.ChannelCreate(&ch, t.Context()); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	mk := func(name, jobStatus, resStatus string) int64 {
		id, err := op.ModelAuditEnqueue(t.Context(), op.ModelAuditEnqueueInput{ChannelID: ch.ID, Models: []string{name}})
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		db.GetDB().Model(&model.ModelAuditJob{}).Where("id = ?", id).Update("status", jobStatus)
		db.GetDB().Model(&model.ModelAuditResult{}).Where("job_id = ?", id).Update("status", resStatus)
		return id
	}
	done := mk("a", model.AuditJobDone, model.AuditResultDone)
	done2 := mk("b", model.AuditJobDone, model.AuditResultFailed)
	done3 := mk("c", model.AuditJobCanceled, model.AuditResultSkipped)
	running := mk("d", model.AuditJobRunning, model.AuditResultRunning)

	r := gin.New()
	r.DELETE("/jobs/:id", deleteModelAuditJob)
	r.POST("/jobs/delete", deleteModelAuditJobs)
	r.POST("/jobs/clear-finished", clearFinishedModelAuditJobs)

	if rec := doJSON(t, r, http.MethodDelete, "/jobs/"+itoa64(running), nil); rec.Code != http.StatusConflict {
		t.Fatalf("deleting a running job = %d %s, want 409", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, r, http.MethodDelete, "/jobs/"+itoa64(done), nil); rec.Code != http.StatusOK {
		t.Fatalf("delete one = %d %s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, r, http.MethodDelete, "/jobs/"+itoa64(done), nil); rec.Code != http.StatusNotFound {
		t.Fatalf("delete twice = %d, want 404", rec.Code)
	}
	rec := doJSON(t, r, http.MethodPost, "/jobs/delete", map[string]any{"ids": []int64{done2, running}})
	var batch struct {
		Data op.ModelAuditDeleteResult `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &batch); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("batch = %d %s", rec.Code, rec.Body.String())
	}
	if batch.Data.Deleted != 1 || len(batch.Data.SkippedBusy) != 1 || batch.Data.SkippedBusy[0] != running {
		t.Fatalf("batch result = %+v", batch.Data)
	}
	rec = doJSON(t, r, http.MethodPost, "/jobs/clear-finished", map[string]any{})
	if err := json.Unmarshal(rec.Body.Bytes(), &batch); err != nil || batch.Data.Deleted != 1 {
		t.Fatalf("clear finished = %d %s", rec.Code, rec.Body.String())
	}
	var left []int64
	db.GetDB().Model(&model.ModelAuditJob{}).Order("id").Pluck("id", &left)
	if len(left) != 1 || left[0] != running {
		t.Fatalf("remaining jobs = %v, want only the running one (%d); done3=%d", left, running, done3)
	}
}

func itoa64(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// 出图执行体：走渠道检测的出站链路发 1 次，从回复里抠出 SVG。
func TestCreativeWorkerExtractsSVGFromReply(t *testing.T) {
	setupV6HandlerTest(t)
	svg := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><circle r="4"/></svg>`
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-1", "object": "chat.completion", "model": "pelican-model",
			"choices": []map[string]any{{"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "Here:\n```svg\n" + svg + "\n```"}}},
			"usage": map[string]any{"prompt_tokens": 12, "completion_tokens": 34, "total_tokens": 46},
		})
	}))
	t.Cleanup(upstream.Close)
	ch := model.Channel{
		Name: "pelican", Type: outbound.OutboundTypeOpenAIChat, Enabled: true, Model: "pelican-model",
		BaseUrls: []model.BaseUrl{{URL: upstream.URL}},
		Keys:     []model.ChannelKey{{Enabled: true, ChannelKey: "k"}},
	}
	if err := op.ChannelCreate(&ch, t.Context()); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	_, ids, err := op.ModelAuditCreativeEnqueue(t.Context(), op.CreativeKindPelican, "",
		[]op.CreativeTarget{{ChannelID: ch.ID, Model: "pelican-model"}}, time.Now())
	if err != nil || len(ids) != 1 {
		t.Fatalf("enqueue: %v %v", ids, err)
	}
	row, err := op.ModelAuditCreativeClaimNext(t.Context())
	if err != nil || row == nil {
		t.Fatalf("claim: %v %v", row, err)
	}
	runOneCreative(t.Context(), row)

	var got model.ModelAuditCreative
	if err := db.GetDB().First(&got, ids[0]).Error; err != nil {
		t.Fatalf("load: %v", err)
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want exactly 1", calls)
	}
	if got.Status != model.CreativeDone || got.Format != "svg" || got.Content != svg {
		t.Fatalf("creative = status %q format %q content %q err %q", got.Status, got.Format, got.Content, got.Error)
	}
	if got.OutputTokens != 34 || got.FinishedAt == nil {
		t.Fatalf("tokens/finish not recorded: %+v", got)
	}
	if next, _ := op.ModelAuditCreativeClaimNext(t.Context()); next != nil {
		t.Fatalf("nothing should be left to run")
	}
}
