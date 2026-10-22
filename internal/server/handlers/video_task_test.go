package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
)

func setupVideoTaskHandlerTestDB(t *testing.T) context.Context {
	t.Helper()
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "video_task_handler_test.db"), false); err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
	})
	return context.Background()
}

// videoTaskTestRouter 把被测 handler 挂在测试路由上，用一个最小的身份注入替代
// middleware.Auth()：这里要验的是「handler 如何用会话身份做归属过滤」，
// JWT 解析本身由 middleware/auth_test.go 覆盖。?as_user= 模拟登录态里的 user_id
// ——真实接口上客户端**不能**选身份（见下面的 ?user_id= 被忽略的测试）。
func videoTaskTestRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	grp := r.Group("/api/v1/video-task")
	grp.Use(func(c *gin.Context) {
		if raw := c.Query("as_user"); raw != "" {
			userID, _ := strconv.Atoi(raw)
			c.Set("user_id", userID)
		}
		c.Next()
	})
	grp.GET("/list", listVideoTasks)
	return r
}

func seedVideoTaskHandlerRows(t *testing.T, ctx context.Context) {
	t.Helper()
	rows := []model.VideoTask{
		{
			UpstreamTaskID: "mine-success",
			Model:          "doubao-seedance-1-0",
			OwnerTokenID:   5,
			OwnerUserID:    7,
			Status:         model.VideoTaskSuccess,
			Progress:       "100%",
			ResultURL:      "https://example.com/mine.mp4",
			SubmitTime:     1_700_000_000,
			FinishTime:     1_700_000_600,
		},
		{
			UpstreamTaskID: "mine-failure",
			OwnerTokenID:   5,
			OwnerUserID:    7,
			Status:         model.VideoTaskFailure,
			FailReason:     "upstream timeout",
			SubmitTime:     1_700_000_100,
		},
		{
			UpstreamTaskID: "someone-else",
			OwnerTokenID:   6,
			OwnerUserID:    9,
			Status:         model.VideoTaskSuccess,
			ResultURL:      "https://example.com/other.mp4",
			SubmitTime:     1_700_000_200,
		},
		{
			// 历史行：没有归属。列表接口不许把它当成「公共任务」。
			UpstreamTaskID: "legacy-no-owner",
			Status:         model.VideoTaskFailure,
			SubmitTime:     1_700_000_300,
		},
	}
	for i := range rows {
		if err := op.VideoTaskCreate(ctx, &rows[i]); err != nil {
			t.Fatalf("seed %s: %v", rows[i].UpstreamTaskID, err)
		}
	}
}

type videoTaskListEnvelope struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		Items []struct {
			TaskID     string `json:"task_id"`
			Model      string `json:"model"`
			Status     string `json:"status"`
			Progress   string `json:"progress"`
			FailReason string `json:"fail_reason"`
			ResultURL  string `json:"result_url"`
			SubmitTime int64  `json:"submit_time"`
			StartTime  int64  `json:"start_time"`
			FinishTime int64  `json:"finish_time"`
			CreatedAt  int64  `json:"created_at"`
		} `json:"items"`
		Total    int64 `json:"total"`
		Page     int   `json:"page"`
		PageSize int   `json:"page_size"`
	} `json:"data"`
}

func getVideoTasks(t *testing.T, router *gin.Engine, query string) (*httptest.ResponseRecorder, videoTaskListEnvelope) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/video-task/list"+query, nil)
	router.ServeHTTP(rec, req)
	var envelope videoTaskListEnvelope
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("decode %q: %v", rec.Body.String(), err)
		}
	}
	return rec, envelope
}

// 【租户隔离】列表接口只返回调用者自己的任务：别人的行、以及没有归属的历史行
// 都不许出现在响应体里（结构化字段与裸 JSON 文本两处都断言）。
func TestListVideoTasksOnlyReturnsOwnTasks(t *testing.T) {
	ctx := setupVideoTaskHandlerTestDB(t)
	seedVideoTaskHandlerRows(t, ctx)
	router := videoTaskTestRouter()

	rec, envelope := getVideoTasks(t, router, "?as_user=7")
	if rec.Code != http.StatusOK {
		t.Fatalf("as_user=7: %d %s, want 200", rec.Code, rec.Body.String())
	}
	if envelope.Data.Total != 2 || len(envelope.Data.Items) != 2 {
		t.Fatalf("as_user=7 saw total=%d items=%d, want its own 2", envelope.Data.Total, len(envelope.Data.Items))
	}
	gotIDs := map[string]bool{}
	for _, item := range envelope.Data.Items {
		gotIDs[item.TaskID] = true
	}
	if !gotIDs["mine-success"] || !gotIDs["mine-failure"] {
		t.Fatalf("as_user=7 items = %v, want mine-success + mine-failure", gotIDs)
	}
	// 裸文本对照：别人那条的 id / 结果地址都不该在响应里留下任何痕迹。
	for _, foreign := range []string{"someone-else", "legacy-no-owner", "other.mp4"} {
		if body := rec.Body.String(); strings.Contains(body, foreign) {
			t.Fatalf("response leaked %q: %s", foreign, body)
		}
	}

	// 反向对照：库里那条行本身是可读的，用户 9 查得到 → 上面 7 看不到只可能是归属过滤。
	rec9, envelope9 := getVideoTasks(t, router, "?as_user=9")
	if rec9.Code != http.StatusOK || envelope9.Data.Total != 1 || len(envelope9.Data.Items) != 1 {
		t.Fatalf("as_user=9: %d total=%d items=%d, want its own single row", rec9.Code, envelope9.Data.Total, len(envelope9.Data.Items))
	}
	if envelope9.Data.Items[0].TaskID != "someone-else" {
		t.Fatalf("as_user=9 saw %q, want someone-else", envelope9.Data.Items[0].TaskID)
	}

	// 客户端传来的 user_id 不起作用：身份只认会话。
	recOverride, override := getVideoTasks(t, router, "?as_user=7&user_id=9")
	if recOverride.Code != http.StatusOK {
		t.Fatalf("user_id override: %d, want 200 (query is ignored)", recOverride.Code)
	}
	if override.Data.Total != 2 {
		t.Fatalf("user_id query changed the scope: total=%d, want 2 (session identity wins)", override.Data.Total)
	}
}

// 没有身份（user_id=0）时必须 403，而不是「不过滤 = 全表」。
func TestListVideoTasksRejectsMissingIdentity(t *testing.T) {
	ctx := setupVideoTaskHandlerTestDB(t)
	seedVideoTaskHandlerRows(t, ctx)
	router := videoTaskTestRouter()

	for _, query := range []string{"", "?as_user=0"} {
		rec, _ := getVideoTasks(t, router, query)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("query %q: %d %s, want 403", query, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "mine-success") || strings.Contains(rec.Body.String(), "someone-else") {
			t.Fatalf("query %q leaked task data: %s", query, rec.Body.String())
		}
	}
}

// 字段名照 new-api dto.TaskDto 的语义，且刻意不含渠道/密钥/归属/计费字段。
func TestListVideoTasksFieldContract(t *testing.T) {
	ctx := setupVideoTaskHandlerTestDB(t)
	seedVideoTaskHandlerRows(t, ctx)
	router := videoTaskTestRouter()

	rec, envelope := getVideoTasks(t, router, "?as_user=7&status=success")
	if rec.Code != http.StatusOK {
		t.Fatalf("status filter: %d %s", rec.Code, rec.Body.String())
	}
	if envelope.Data.Total != 1 || len(envelope.Data.Items) != 1 {
		t.Fatalf("status=success saw total=%d items=%d, want 1", envelope.Data.Total, len(envelope.Data.Items))
	}
	item := envelope.Data.Items[0]
	if item.TaskID != "mine-success" || item.Status != string(model.VideoTaskSuccess) ||
		item.Progress != "100%" || item.ResultURL != "https://example.com/mine.mp4" ||
		item.SubmitTime != 1_700_000_000 || item.FinishTime != 1_700_000_600 {
		t.Fatalf("item round-trip mismatch: %+v", item)
	}

	// 未声明的字段（渠道 id、key 引用、归属、计费）一个都不许出现在 JSON 里。
	var raw struct {
		Data struct {
			Items []map[string]any `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	allowed := map[string]bool{
		"task_id": true, "model": true, "status": true, "progress": true,
		"fail_reason": true, "result_url": true, "submit_time": true,
		"start_time": true, "finish_time": true, "created_at": true,
	}
	for key := range raw.Data.Items[0] {
		if !allowed[key] {
			t.Fatalf("unexpected field %q in response item (channel/key/owner/billing fields must not leak)", key)
		}
	}
	for _, key := range []string{"task_id", "status", "progress", "fail_reason", "result_url", "submit_time", "finish_time"} {
		if _, ok := raw.Data.Items[0][key]; !ok {
			t.Fatalf("missing field %q required by the dto.TaskDto naming contract", key)
		}
	}
}

func TestListVideoTasksRejectsUnknownStatus(t *testing.T) {
	ctx := setupVideoTaskHandlerTestDB(t)
	seedVideoTaskHandlerRows(t, ctx)
	router := videoTaskTestRouter()

	rec, _ := getVideoTasks(t, router, "?as_user=7&status=done")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown status: %d %s, want 400", rec.Code, rec.Body.String())
	}
}

func TestListVideoTasksPagination(t *testing.T) {
	ctx := setupVideoTaskHandlerTestDB(t)
	for i := 0; i < 3; i++ {
		row := model.VideoTask{
			UpstreamTaskID: fmt.Sprintf("page-%d", i),
			OwnerUserID:    7,
			Status:         model.VideoTaskSuccess,
		}
		if err := op.VideoTaskCreate(ctx, &row); err != nil {
			t.Fatalf("seed page-%d: %v", i, err)
		}
	}
	router := videoTaskTestRouter()

	rec, envelope := getVideoTasks(t, router, "?as_user=7&page=2&page_size=2")
	if rec.Code != http.StatusOK {
		t.Fatalf("page 2: %d %s", rec.Code, rec.Body.String())
	}
	if envelope.Data.Total != 3 || envelope.Data.Page != 2 || envelope.Data.PageSize != 2 {
		t.Fatalf("pagination meta = total %d page %d size %d, want 3/2/2", envelope.Data.Total, envelope.Data.Page, envelope.Data.PageSize)
	}
	if len(envelope.Data.Items) != 1 {
		t.Fatalf("page 2 rows = %d, want 1", len(envelope.Data.Items))
	}
}
