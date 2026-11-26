package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
)

// 非法的排队配置是调用方的参数错：更新的跨字段校验（QueueSize>0 需要 MaxConcurrent>0）
// 发生在 op 的合并校验里，handler 必须把它映射成 400，而不是当成服务端故障回 500。
func TestChannelUpdateInvalidQueueSettingsIsBadRequest(t *testing.T) {
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "octopus.db"), false); err != nil {
		t.Fatalf("init db: %v", err)
	}
	// sqlite 文件在包级连接池里，TempDir 清理前先关掉，否则 Windows 删不掉会报错。
	t.Cleanup(func() { _ = db.Close() })

	channel := model.Channel{
		Name:          "queue-400-channel",
		Type:          0,
		Enabled:       true,
		MaxConcurrent: 2,
		QueueSize:     3,
		BaseUrls:      []model.BaseUrl{{URL: "http://127.0.0.1:9"}},
		Keys:          []model.ChannelKey{{ChannelKey: "sk-queue-400", Enabled: true}},
		Model:         "queue-400-model",
	}
	if err := op.ChannelCreate(&channel, context.Background()); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	// 只把 max_concurrent 改成 0：库里 queue_size=3 还在，合并后非法 → 400。
	rec := postChannelUpdate(t, map[string]any{"id": channel.ID, "max_concurrent": 0})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("an invalid queue configuration is a client parameter error (400), got %d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Code == http.StatusInternalServerError {
		t.Fatalf("an invalid queue configuration must never be reported as a server error")
	}
	stored, err := op.ChannelGet(channel.ID, context.Background())
	if err != nil {
		t.Fatalf("get channel: %v", err)
	}
	if stored.QueueSize != 3 || stored.MaxConcurrent != 2 {
		t.Fatalf("a rejected update must not be persisted, got max_concurrent=%d queue_size=%d", stored.MaxConcurrent, stored.QueueSize)
	}

	// 负数排队时长同样是参数错 → 400。
	rec = postChannelUpdate(t, map[string]any{"id": channel.ID, "queue_timeout_ms": -1})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a negative queue_timeout_ms is a client parameter error (400), got %d body=%s", rec.Code, rec.Body.String())
	}

	// 合法更新照常 200，证明 400 只针对排队参数错、没有泛化。
	rec = postChannelUpdate(t, map[string]any{"id": channel.ID, "queue_size": 5})
	if rec.Code != http.StatusOK {
		t.Fatalf("a valid queue update must still succeed, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func postChannelUpdate(t *testing.T, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/api/v1/channel/update", updateChannel)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/channel/update", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec
}
