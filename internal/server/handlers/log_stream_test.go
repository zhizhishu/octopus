package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/gin-gonic/gin"
)

func TestStreamLogWritesInitialHeartbeat(t *testing.T) {
	gin.SetMode(gin.TestMode)

	token, err := op.RelayLogStreamTokenCreate(model.RelayLogScope{}, true)
	if err != nil {
		t.Fatalf("create stream token: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/log/stream?token="+token, nil).WithContext(ctx)

	done := make(chan struct{})
	go func() {
		streamLog(c)
		close(done)
	}()

	time.Sleep(25 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("streamLog did not stop after request context cancellation")
	}

	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
	if body := rec.Body.String(); !strings.Contains(body, ": connected\n\n") {
		t.Fatalf("expected initial SSE comment heartbeat, got %q", body)
	}
}

func TestRelayLogExportSummaryDoesNotLeakContent(t *testing.T) {
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

	if err := db.GetDB().Create(&model.RelayLog{
		ID:               6001,
		Time:             6001,
		RequestEndpoint:  "responses",
		RequestPath:      "/v1/responses",
		RequestModelName: "gpt-5.5",
		ChannelName:      "any",
		ActualModelName:  "gpt-5.5",
		RequestContent:   `{"input":"secret prompt"}`,
		ResponseContent:  `{"output":"secret answer"}`,
		Error:            "secret upstream detail",
	}).Error; err != nil {
		t.Fatalf("create relay log: %v", err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/log/export", nil)

	if err := streamRelayLogExport(c, nil, nil, 100, nil, true); err != nil {
		t.Fatalf("export relay logs: %v", err)
	}

	body := rec.Body.String()
	for _, leak := range []string{"request_content", "response_content", "secret prompt", "secret answer", "secret upstream detail"} {
		if strings.Contains(body, leak) {
			t.Fatalf("export leaked %q in body %s", leak, body)
		}
	}
	if !strings.Contains(body, `"request_model_name":"gpt-5.5"`) {
		t.Fatalf("export summary lost safe model metadata: %s", body)
	}
}
