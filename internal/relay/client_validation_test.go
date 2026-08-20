package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/db"
	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	geminiInbound "github.com/bestruirui/octopus/internal/transformer/inbound/gemini"
	"github.com/gin-gonic/gin"
)

func TestHandlerInterceptsEmptyClientRequestsWithoutStats(t *testing.T) {
	tests := []struct {
		name           string
		inboundType    inbound.InboundType
		path           string
		body           string
		wantEndpoint   string
		contextModel   string
		contextStream  bool
		wantBodySignal string
	}{
		{
			name:         "openai chat empty messages",
			inboundType:  inbound.InboundTypeOpenAIChat,
			path:         "/v1/chat/completions",
			body:         `{"model":"gpt-5.5","messages":[],"stream":true}`,
			wantEndpoint: "chat",
		},
		{
			name:           "openai chat whitespace-only content",
			inboundType:    inbound.InboundTypeOpenAIChat,
			path:           "/v1/chat/completions",
			body:           `{"model":"gpt-5.5","messages":[{"role":"user","content":"   "}],"stream":true}`,
			wantEndpoint:   "chat",
			wantBodySignal: `"content":"   "`,
		},
		{
			name:         "openai responses empty input",
			inboundType:  inbound.InboundTypeOpenAIResponse,
			path:         "/v1/responses",
			body:         `{"model":"gpt-5.5","stream":true,"tools":[]}`,
			wantEndpoint: "responses",
		},
		{
			name:           "openai responses whitespace-only input",
			inboundType:    inbound.InboundTypeOpenAIResponse,
			path:           "/v1/responses",
			body:           `{"model":"gpt-5.5","input":"   ","stream":true,"tools":[]}`,
			wantEndpoint:   "responses",
			wantBodySignal: `"input":"   "`,
		},
		{
			name:           "anthropic messages empty messages",
			inboundType:    inbound.InboundTypeAnthropic,
			path:           "/v1/messages",
			body:           `{"model":"claude-opus-4-7[1m]","max_tokens":256,"stream":true,"messages":[]}`,
			wantEndpoint:   "messages",
			wantBodySignal: `"messages":[]`,
		},
		{
			name:           "anthropic messages empty string content",
			inboundType:    inbound.InboundTypeAnthropic,
			path:           "/v1/messages",
			body:           `{"model":"claude-opus-4-7[1m]","max_tokens":256,"stream":true,"messages":[{"role":"user","content":""}]}`,
			wantEndpoint:   "messages",
			wantBodySignal: `"content":""`,
		},
		{
			name:           "anthropic messages blank text block content",
			inboundType:    inbound.InboundTypeAnthropic,
			path:           "/v1/messages",
			body:           `{"model":"claude-opus-4-7[1m]","max_tokens":256,"stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"   "}]}]}`,
			wantEndpoint:   "messages",
			wantBodySignal: `"text":"   "`,
		},
		{
			name:          "gemini empty contents",
			inboundType:   inbound.InboundTypeGemini,
			path:          "/v1beta/models/gemini-pro:generateContent",
			body:          `{"contents":[]}`,
			wantEndpoint:  "gemini_generate_content",
			contextModel:  "gemini-pro",
			contextStream: false,
		},
		{
			name:           "gemini whitespace-only text part",
			inboundType:    inbound.InboundTypeGemini,
			path:           "/v1beta/models/gemini-pro:generateContent",
			body:           `{"contents":[{"role":"user","parts":[{"text":"   "}]}]}`,
			wantEndpoint:   "gemini_generate_content",
			contextModel:   "gemini-pro",
			contextStream:  false,
			wantBodySignal: `"text":"   "`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			ctx := setupRelayErrorDB(t)
			if err := op.RelayLogClear(ctx, nil); err != nil {
				t.Fatalf("clear relay logs: %v", err)
			}

			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			req := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			if tt.contextModel != "" {
				req = req.WithContext(geminiInbound.WithRequestOptions(req.Context(), tt.contextModel, tt.contextStream))
			}
			c.Request = req
			c.Set("api_key_id", 0)
			c.Set("user_id", 0)
			c.Set("request_ip", "127.0.0.1")

			Handler(tt.inboundType, c)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected local 400, got %d body %s", rec.Code, rec.Body.String())
			}
			var rawBody map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &rawBody); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			var gotCode, gotMsg string
			if errObj, ok := rawBody["error"].(map[string]any); ok {
				// OpenAI error envelope {"error":{"message":..,"code":..}} (used for responses inbound)
				if c, ok := errObj["code"].(string); ok {
					gotCode = c
				}
				if m, ok := errObj["message"].(string); ok {
					gotMsg = m
				}
			} else {
				// Octopus-internal ResponseStruct {code, error_code, message} (used for chat/anthropic/gemini)
				if c, ok := rawBody["error_code"].(string); ok {
					gotCode = c
				}
				if m, ok := rawBody["message"].(string); ok {
					gotMsg = m
				}
			}
			if gotCode != clientEmptyRequestCode {
				t.Fatalf("expected %s, got %q body %s", clientEmptyRequestCode, gotCode, rec.Body.String())
			}
			if !strings.Contains(strings.ToLower(gotMsg), "empty request") {
				t.Fatalf("expected empty request message, got %q", gotMsg)
			}

			logs, err := op.RelayLogList(ctx, nil, nil, 1, 10, nil)
			if err != nil {
				t.Fatalf("list relay logs: %v", err)
			}
			if len(logs) != 1 {
				t.Fatalf("expected one validation log, got %d", len(logs))
			}
			got := logs[0]
			if got.RequestEndpoint != tt.wantEndpoint || got.RequestPath != tt.path {
				t.Fatalf("unexpected endpoint/path: endpoint=%q path=%q", got.RequestEndpoint, got.RequestPath)
			}
			if got.ErrorCode != clientEmptyRequestCode || got.ErrorStatus != http.StatusBadRequest {
				t.Fatalf("unexpected log error: status=%d code=%q", got.ErrorStatus, got.ErrorCode)
			}
			if got.ErrorStrategy != dbmodel.RelayLogErrorStrategyLocalValidation {
				t.Fatalf("unexpected error strategy: %q", got.ErrorStrategy)
			}
			if got.TotalAttempts != 0 || len(got.Attempts) != 0 || got.ChannelId != 0 {
				t.Fatalf("validation log must not have upstream attempts: channel=%d attempts=%d total=%d", got.ChannelId, len(got.Attempts), got.TotalAttempts)
			}
			if got.Cost != 0 || got.InputTokens != 0 || got.OutputTokens != 0 {
				t.Fatalf("validation log must not be billed or token-counted: cost=%f input=%d output=%d", got.Cost, got.InputTokens, got.OutputTokens)
			}
			if got.UsageSource != dbmodel.RelayLogUsageSourceLocalValidation || got.UsageMissingReason != dbmodel.RelayLogUsageMissingReasonLocalValidation {
				t.Fatalf("unexpected validation usage audit: source=%q reason=%q", got.UsageSource, got.UsageMissingReason)
			}
			detail, detailErr := op.RelayLogGetByID(ctx, got.ID, nil)
			if detailErr != nil {
				t.Fatalf("get validation log detail: %v", detailErr)
			}
			if tt.wantBodySignal != "" && !strings.Contains(detail.RequestContent, tt.wantBodySignal) {
				t.Fatalf("expected request content to preserve %s, got %s", tt.wantBodySignal, detail.RequestContent)
			}
			if !strings.Contains(detail.ResponseContent, `"upstream_forwarded":false`) ||
				!strings.Contains(detail.ResponseContent, `"stats_counted":false`) {
				t.Fatalf("expected local validation response content, got %s", detail.ResponseContent)
			}

			var totalCount int64
			if err := db.GetDB().Model(&dbmodel.StatsTotal{}).Count(&totalCount).Error; err != nil {
				t.Fatalf("count total stats: %v", err)
			}
			if totalCount != 0 {
				t.Fatalf("empty validation request must not increment usage stats, got rows=%d", totalCount)
			}
		})
	}
}

func TestHandlerSatisfiesCursorAnthropicEmptyProbeLocally(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	if err := op.RelayLogClear(ctx, nil); err != nil {
		t.Fatalf("clear relay logs: %v", err)
	}

	body := `{"model":"claude-opus-4-7[1m]","max_tokens":128000,"reasoning_effort":"high","stream":true,"tools":[],"messages":[]}`
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Cursor/1.0")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	Handler(inbound.InboundTypeAnthropic, c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected local cursor probe 200, got %d body %s", rec.Code, rec.Body.String())
	}
	gotBody := rec.Body.String()
	if !strings.Contains(gotBody, "event: message_start") ||
		!strings.Contains(gotBody, "event: message_stop") ||
		!strings.Contains(gotBody, "msg_octopus_cursor_empty_probe") {
		t.Fatalf("expected anthropic stream probe response, got %s", gotBody)
	}

	logs, err := op.RelayLogList(ctx, nil, nil, 1, 10, nil)
	if err != nil {
		t.Fatalf("list relay logs: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("expected one cursor probe log, got %d", len(logs))
	}
	got := logs[0]
	if got.RequestEndpoint != "messages" || got.RequestPath != "/v1/messages" {
		t.Fatalf("unexpected endpoint/path: endpoint=%q path=%q", got.RequestEndpoint, got.RequestPath)
	}
	if got.Error != "" || got.ErrorCode != dbmodel.RelayLogErrorCodeCursorEmptyProbe || got.ErrorStatus != http.StatusOK {
		t.Fatalf("unexpected cursor probe audit fields: error=%q status=%d code=%q", got.Error, got.ErrorStatus, got.ErrorCode)
	}
	if got.ErrorStrategy != dbmodel.RelayLogErrorStrategyLocalCursorProbe {
		t.Fatalf("unexpected error strategy: %q", got.ErrorStrategy)
	}
	if got.TotalAttempts != 0 || len(got.Attempts) != 0 || got.ChannelId != 0 {
		t.Fatalf("cursor probe must not have upstream attempts: channel=%d attempts=%d total=%d", got.ChannelId, len(got.Attempts), got.TotalAttempts)
	}
	if got.Cost != 0 || got.InputTokens != 0 || got.OutputTokens != 0 {
		t.Fatalf("cursor probe must not be billed or token-counted: cost=%f input=%d output=%d", got.Cost, got.InputTokens, got.OutputTokens)
	}
	if got.UsageSource != dbmodel.RelayLogUsageSourceLocalValidation || got.UsageMissingReason != dbmodel.RelayLogUsageMissingReasonLocalValidation {
		t.Fatalf("unexpected cursor usage audit: source=%q reason=%q", got.UsageSource, got.UsageMissingReason)
	}
	detail, detailErr := op.RelayLogGetByID(ctx, got.ID, nil)
	if detailErr != nil {
		t.Fatalf("get cursor probe log detail: %v", detailErr)
	}
	if !strings.Contains(detail.ResponseContent, `"compatibility":"cursor_empty_anthropic_probe"`) ||
		!strings.Contains(detail.ResponseContent, `"upstream_forwarded":false`) ||
		!strings.Contains(detail.ResponseContent, `"stats_counted":false`) {
		t.Fatalf("expected cursor local validation response content, got %s", detail.ResponseContent)
	}

	var totalCount int64
	if err := db.GetDB().Model(&dbmodel.StatsTotal{}).Count(&totalCount).Error; err != nil {
		t.Fatalf("count total stats: %v", err)
	}
	if totalCount != 0 {
		t.Fatalf("cursor probe must not increment usage stats, got rows=%d", totalCount)
	}
}

func TestHandlerSatisfiesCursorOpenAIEmptyProbeLocally(t *testing.T) {
	tests := []struct {
		name        string
		inboundType inbound.InboundType
		path        string
		body        string
		wantSignal  string
	}{
		{
			name:        "chat completions",
			inboundType: inbound.InboundTypeOpenAIChat,
			path:        "/v1/chat/completions",
			body:        `{"model":"gpt-5.5","messages":[],"max_tokens":128000,"stream":true,"tools":[]}`,
			wantSignal:  "chatcmpl-octopus_cursor_empty_probe",
		},
		{
			name:        "responses",
			inboundType: inbound.InboundTypeOpenAIResponse,
			path:        "/v1/responses",
			body:        `{"model":"gpt-5.5","max_output_tokens":128000,"stream":true,"tools":[]}`,
			wantSignal:  "resp_octopus_cursor_empty_probe",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			ctx := setupRelayErrorDB(t)
			if err := op.RelayLogClear(ctx, nil); err != nil {
				t.Fatalf("clear relay logs: %v", err)
			}

			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			req := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("User-Agent", "Cursor/1.0")
			c.Request = req
			c.Set("api_key_id", 0)
			c.Set("user_id", 0)
			c.Set("request_ip", "127.0.0.1")

			Handler(tt.inboundType, c)

			if rec.Code != http.StatusOK {
				t.Fatalf("expected local cursor probe 200, got %d body %s", rec.Code, rec.Body.String())
			}
			if gotBody := rec.Body.String(); !strings.Contains(gotBody, tt.wantSignal) || !strings.Contains(gotBody, "data: [DONE]") {
				t.Fatalf("expected OpenAI stream probe response, got %s", gotBody)
			}

			logs, err := op.RelayLogList(ctx, nil, nil, 1, 10, nil)
			if err != nil {
				t.Fatalf("list relay logs: %v", err)
			}
			if len(logs) != 1 {
				t.Fatalf("expected one cursor probe log, got %d", len(logs))
			}
			got := logs[0]
			if got.ErrorCode != dbmodel.RelayLogErrorCodeCursorEmptyProbe || got.ErrorStatus != http.StatusOK {
				t.Fatalf("unexpected cursor probe audit fields: status=%d code=%q", got.ErrorStatus, got.ErrorCode)
			}
			if got.TotalAttempts != 0 || got.ChannelId != 0 {
				t.Fatalf("cursor probe must not forward upstream: channel=%d attempts=%d", got.ChannelId, got.TotalAttempts)
			}
			if got.Cost != 0 || got.InputTokens != 0 || got.OutputTokens != 0 {
				t.Fatalf("cursor probe must not be billed or token-counted: cost=%f input=%d output=%d", got.Cost, got.InputTokens, got.OutputTokens)
			}
		})
	}
}

func TestCursorLikeAnthropicEmptyProbeBodyWithoutHeaderIsLocal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	if err := op.RelayLogClear(ctx, nil); err != nil {
		t.Fatalf("clear relay logs: %v", err)
	}

	body := `{"model":"claude-opus-4-7[1m]","max_tokens":128000,"reasoning_effort":"high","stream":true,"tools":[],"messages":[]}`
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	Handler(inbound.InboundTypeAnthropic, c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected cursor-like local probe 200, got %d body %s", rec.Code, rec.Body.String())
	}
	logs, err := op.RelayLogList(ctx, nil, nil, 1, 10, nil)
	if err != nil {
		t.Fatalf("list relay logs: %v", err)
	}
	if len(logs) != 1 || logs[0].ErrorCode != dbmodel.RelayLogErrorCodeCursorEmptyProbe {
		t.Fatalf("expected cursor probe log, got %#v", logs)
	}
}

func TestCursorHeaderWithoutProbeBodyUsesEmptyRequestValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	if err := op.RelayLogClear(ctx, nil); err != nil {
		t.Fatalf("clear relay logs: %v", err)
	}

	body := `{"model":"claude-opus-4-7[1m]","max_tokens":256,"stream":true,"messages":[]}`
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Cursor/1.0")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	Handler(inbound.InboundTypeAnthropic, c)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected ordinary empty cursor request to stay 400, got %d body %s", rec.Code, rec.Body.String())
	}
	logs, err := op.RelayLogList(ctx, nil, nil, 1, 10, nil)
	if err != nil {
		t.Fatalf("list relay logs: %v", err)
	}
	if len(logs) != 1 || logs[0].ErrorCode != dbmodel.RelayLogErrorCodeClientEmptyRequest {
		t.Fatalf("expected client empty request log, got %#v", logs)
	}
}

func TestCursorAnthropicBlankMessageProbeIsLocal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := setupRelayErrorDB(t)
	if err := op.RelayLogClear(ctx, nil); err != nil {
		t.Fatalf("clear relay logs: %v", err)
	}

	body := `{"model":"claude-opus-4-7[1m]","max_tokens":128000,"reasoning_effort":"high","stream":true,"tools":[],"messages":[{"role":"user","content":""}]}`
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	c.Set("api_key_id", 0)
	c.Set("user_id", 0)
	c.Set("request_ip", "127.0.0.1")

	Handler(inbound.InboundTypeAnthropic, c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected cursor blank-message probe to stay local, got %d body %s", rec.Code, rec.Body.String())
	}
	if gotBody := rec.Body.String(); !strings.Contains(gotBody, "msg_octopus_cursor_empty_probe") {
		t.Fatalf("expected cursor local probe response, got %s", gotBody)
	}
	logs, err := op.RelayLogList(ctx, nil, nil, 1, 10, nil)
	if err != nil {
		t.Fatalf("list relay logs: %v", err)
	}
	if len(logs) != 1 || logs[0].ErrorCode != dbmodel.RelayLogErrorCodeCursorEmptyProbe {
		t.Fatalf("expected cursor probe log, got %#v", logs)
	}
	if logs[0].TotalAttempts != 0 || logs[0].ChannelId != 0 {
		t.Fatalf("cursor blank-message probe must not forward upstream: channel=%d attempts=%d", logs[0].ChannelId, logs[0].TotalAttempts)
	}
}
