package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/relay/intervention"
	"github.com/gin-gonic/gin"
)

func setupInterventionTestDB(t *testing.T) context.Context {
	t.Helper()
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "intervention_test.db"), false); err != nil {
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
	return context.Background()
}

func setupTestRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	grp := r.Group("/api/v1/intervention")
	{
		grp.GET("/list", listInterventions)
		grp.GET("/:id", getIntervention)
		grp.POST("/:id/retry", retryIntervention)
		grp.POST("/:id/abort", abortIntervention)
	}
	return r
}

func TestRetryInterventionValidation(t *testing.T) {
	ctx := setupInterventionTestDB(t)
	router := setupTestRouter()

	ch := model.Channel{
		Name:    "test-chan",
		Enabled: true,
		Keys: []model.ChannelKey{
			{Enabled: true, ChannelKey: "sk-valid-key"},
			{Enabled: false, ChannelKey: "sk-disabled-key"},
		},
	}
	if err := op.ChannelCreate(&ch, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	validKeyID := ch.Keys[0].ID
	disabledKeyID := ch.Keys[1].ID
	disabledKey := ch.Keys[1]
	disabledKey.Enabled = false
	if err := op.ChannelKeyUpdate(disabledKey); err != nil {
		t.Fatalf("disable channel key: %v", err)
	}

	disabledCh := model.Channel{
		Name:    "disabled-chan",
		Enabled: false,
		Keys: []model.ChannelKey{
			{Enabled: true, ChannelKey: "sk-chan-disabled"},
		},
	}
	if err := op.ChannelCreate(&disabledCh, ctx); err != nil {
		t.Fatalf("create disabled channel: %v", err)
	}
	disabledChanKeyID := disabledCh.Keys[0].ID
	disabled := false
	if _, err := op.ChannelUpdate(&model.ChannelUpdateRequest{ID: disabledCh.ID, Enabled: &disabled}, ctx); err != nil {
		t.Fatalf("disable channel: %v", err)
	}

	tests := []struct {
		name       string
		body       map[string]any
		wantStatus int
		wantSubstr string
	}{
		{
			name:       "missing channel_id",
			body:       map[string]any{"key_id": validKeyID},
			wantStatus: http.StatusBadRequest,
			wantSubstr: "channel_id is required",
		},
		{
			name:       "missing key_id (0 forbidden)",
			body:       map[string]any{"channel_id": ch.ID, "key_id": 0},
			wantStatus: http.StatusBadRequest,
			wantSubstr: "key_id is required",
		},
		{
			name:       "channel not found",
			body:       map[string]any{"channel_id": 99999, "key_id": validKeyID},
			wantStatus: http.StatusBadRequest,
			wantSubstr: "channel not found",
		},
		{
			name:       "channel disabled",
			body:       map[string]any{"channel_id": disabledCh.ID, "key_id": disabledChanKeyID},
			wantStatus: http.StatusBadRequest,
			wantSubstr: "channel is disabled",
		},
		{
			name:       "key not in channel",
			body:       map[string]any{"channel_id": ch.ID, "key_id": 99999},
			wantStatus: http.StatusBadRequest,
			wantSubstr: "key not found or disabled in channel",
		},
		{
			name:       "key disabled in channel",
			body:       map[string]any{"channel_id": ch.ID, "key_id": disabledKeyID},
			wantStatus: http.StatusBadRequest,
			wantSubstr: "key not found or disabled in channel",
		},
		{
			name:       "valid channel and key",
			body:       map[string]any{"channel_id": ch.ID, "key_id": validKeyID, "model_name": "gpt-4o-custom"},
			wantStatus: http.StatusOK,
			wantSubstr: "success",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, err := intervention.Register(&intervention.Pending{RequestModel: "gpt-4o"})
			if err != nil {
				t.Fatalf("failed to register intervention: %v", err)
			}
			defer intervention.Cancel(id)

			jsonBytes, _ := json.Marshal(tt.body)
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/intervention/"+id+"/retry", bytes.NewReader(jsonBytes))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			if w.Code != tt.wantStatus {
				t.Fatalf("expected status %d, got %d. body=%s", tt.wantStatus, w.Code, w.Body.String())
			}
			if !bytes.Contains(w.Body.Bytes(), []byte(tt.wantSubstr)) {
				t.Fatalf("expected body to contain %q, got %s", tt.wantSubstr, w.Body.String())
			}
		})
	}
}
