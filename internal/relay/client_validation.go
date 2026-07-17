package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/utils/log"
	"github.com/gin-gonic/gin"
)

const (
	clientEmptyRequestCode   = dbmodel.RelayLogErrorCodeClientEmptyRequest
	cursorEmptyProbeCode     = dbmodel.RelayLogErrorCodeCursorEmptyProbe
	clientInvalidRequestCode = "client_invalid_request"
)

func clientValidationErrorCode(err error) string {
	if isEmptyClientRequestError(err) {
		return clientEmptyRequestCode
	}
	return clientInvalidRequestCode
}

func clientValidationErrorMessage(err error) string {
	if isEmptyClientRequestError(err) {
		return "empty request: messages or input is required"
	}
	if err == nil {
		return "invalid request"
	}
	return err.Error()
}

func isEmptyClientRequestError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	switch msg {
	case "either messages or input is required",
		"messages are required",
		"input cannot be empty":
		return true
	default:
		return strings.Contains(msg, "messages") && strings.Contains(msg, "required") ||
			strings.Contains(msg, "input") && strings.Contains(msg, "empty")
	}
}

func saveClientValidationRelayLog(ctx context.Context, c *gin.Context, inboundType inbound.InboundType, req *transformerModel.InternalLLMRequest, body []byte, err error) {
	if c == nil || req == nil {
		return
	}

	code := clientValidationErrorCode(err)
	if code != clientEmptyRequestCode {
		return
	}

	requestPath := ""
	if c.Request != nil && c.Request.URL != nil {
		requestPath = c.Request.URL.Path
	}
	clientSession := deriveClientSessionInfo(c.Request.Header, req)

	relayLog := dbmodel.RelayLog{
		UserID:             c.GetInt("user_id"),
		APIKeyID:           c.GetInt("api_key_id"),
		RequestIP:          c.GetString("request_ip"),
		Time:               time.Now().Unix(),
		RequestEndpoint:    endpointNameForInbound(inboundType, requestPath),
		RequestPath:        requestPath,
		RequestModelName:   strings.TrimSpace(req.Model),
		ActualModelName:    strings.TrimSpace(req.Model),
		UseTime:            0,
		TotalAttempts:      0,
		RequestContent:     truncateString(strings.TrimSpace(string(body)), 8*1024),
		ResponseContent:    clientValidationResponseContent(code),
		Error:              clientValidationErrorMessage(err),
		ErrorCode:          code,
		ErrorStatus:        http.StatusBadRequest,
		ErrorStrategy:      dbmodel.RelayLogErrorStrategyLocalValidation,
		SessionKey:         clientSession.Key,
		SessionSource:      clientSession.Source,
		UsageSource:        dbmodel.RelayLogUsageSourceLocalValidation,
		UsageMissingReason: dbmodel.RelayLogUsageMissingReasonLocalValidation,
	}
	if apiKey, getErr := op.APIKeyGet(relayLog.APIKeyID, ctx); getErr == nil {
		relayLog.RequestAPIKeyName = apiKey.Name
	}
	if user, getErr := op.UserGet(relayLog.UserID); getErr == nil {
		relayLog.UserName = user.Username
	}

	if logErr := op.RelayLogAdd(ctx, relayLog); logErr != nil {
		log.Warnf("failed to save client validation relay log: %v", logErr)
	}
}

func maybeHandleCursorEmptyAnthropicProbe(c *gin.Context, inboundType inbound.InboundType, req *transformerModel.InternalLLMRequest, body []byte, err error) bool {
	if c == nil || req == nil || inboundType != inbound.InboundTypeAnthropic || !isEmptyClientRequestError(err) {
		return false
	}
	if c.Request == nil || c.Request.URL == nil || !strings.HasSuffix(c.Request.URL.Path, "/messages") {
		return false
	}
	if !requestHasNoEffectiveInput(req) || !looksLikeCursorEmptyAnthropicProbe(c.Request, body) {
		return false
	}

	saveCursorEmptyProbeRelayLog(c.Request.Context(), c, req, body)
	writeCursorEmptyProbeResponse(c, req)
	return true
}

func requestHasNoEffectiveInput(req *transformerModel.InternalLLMRequest) bool {
	if req == nil || len(req.Messages) == 0 {
		return true
	}
	for _, msg := range req.Messages {
		if messageHasEffectiveInput(msg) {
			return false
		}
	}
	return true
}

func messageHasEffectiveInput(msg transformerModel.Message) bool {
	if strings.TrimSpace(msg.Refusal) != "" || strings.TrimSpace(msg.GetReasoningContent()) != "" {
		return true
	}
	if msg.FunctionCall != nil || len(msg.ToolCalls) > 0 || msg.ToolCallID != nil && strings.TrimSpace(*msg.ToolCallID) != "" {
		return true
	}
	if msg.Audio != nil || len(msg.Images) > 0 {
		return true
	}
	if msg.Content.Content != nil && strings.TrimSpace(*msg.Content.Content) != "" {
		return true
	}
	for _, part := range msg.Content.MultipleContent {
		if messagePartHasEffectiveInput(part) {
			return true
		}
	}
	return false
}

func messagePartHasEffectiveInput(part transformerModel.MessageContentPart) bool {
	switch strings.ToLower(strings.TrimSpace(part.Type)) {
	case "", "text":
		return part.Text != nil && strings.TrimSpace(*part.Text) != ""
	case "image_url", "input_audio", "file":
		return part.ImageURL != nil || part.Audio != nil || part.File != nil
	default:
		return true
	}
}

func looksLikeCursorEmptyAnthropicProbe(r *http.Request, body []byte) bool {
	bodyLooksLikeProbe := false
	var probe struct {
		Messages        []json.RawMessage `json:"messages"`
		MaxTokens       int64             `json:"max_tokens"`
		ReasoningEffort string            `json:"reasoning_effort"`
		Stream          *bool             `json:"stream"`
		Tools           []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(body, &probe); err == nil && probe.Stream != nil && *probe.Stream {
		bodyLooksLikeProbe = probe.MaxTokens >= 100000 &&
			strings.TrimSpace(probe.ReasoningEffort) != "" &&
			probe.Tools != nil &&
			len(probe.Tools) == 0
	}

	if r != nil {
		for _, key := range []string{
			"User-Agent",
			"X-Client-Name",
			"X-Client-App",
			"X-Title",
			"HTTP-Referer",
		} {
			if bodyLooksLikeProbe && strings.Contains(strings.ToLower(r.Header.Get(key)), "cursor") {
				return true
			}
		}
	}
	return bodyLooksLikeProbe
}

func saveCursorEmptyProbeRelayLog(ctx context.Context, c *gin.Context, req *transformerModel.InternalLLMRequest, body []byte) {
	requestPath := ""
	if c.Request != nil && c.Request.URL != nil {
		requestPath = c.Request.URL.Path
	}
	clientSession := deriveClientSessionInfo(c.Request.Header, req)

	relayLog := dbmodel.RelayLog{
		UserID:             c.GetInt("user_id"),
		APIKeyID:           c.GetInt("api_key_id"),
		RequestIP:          c.GetString("request_ip"),
		Time:               time.Now().Unix(),
		RequestEndpoint:    endpointNameForInbound(inbound.InboundTypeAnthropic, requestPath),
		RequestPath:        requestPath,
		RequestModelName:   strings.TrimSpace(req.Model),
		ActualModelName:    strings.TrimSpace(req.Model),
		UseTime:            0,
		TotalAttempts:      0,
		RequestContent:     truncateString(strings.TrimSpace(string(body)), 8*1024),
		ResponseContent:    cursorEmptyProbeResponseContent(),
		ErrorCode:          cursorEmptyProbeCode,
		ErrorStatus:        http.StatusOK,
		ErrorStrategy:      dbmodel.RelayLogErrorStrategyLocalCursorProbe,
		SessionKey:         clientSession.Key,
		SessionSource:      clientSession.Source,
		UsageSource:        dbmodel.RelayLogUsageSourceLocalValidation,
		UsageMissingReason: dbmodel.RelayLogUsageMissingReasonLocalValidation,
	}
	if apiKey, getErr := op.APIKeyGet(relayLog.APIKeyID, ctx); getErr == nil {
		relayLog.RequestAPIKeyName = apiKey.Name
	}
	if user, getErr := op.UserGet(relayLog.UserID); getErr == nil {
		relayLog.UserName = user.Username
	}

	if logErr := op.RelayLogAdd(ctx, relayLog); logErr != nil {
		log.Warnf("failed to save cursor empty probe relay log: %v", logErr)
	}
}

func writeCursorEmptyProbeResponse(c *gin.Context, req *transformerModel.InternalLLMRequest) {
	stream := req.Stream != nil && *req.Stream
	modelName := strings.TrimSpace(req.Model)
	if modelName == "" {
		modelName = "claude"
	}
	if stream {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("X-Octopus-Local-Validation", cursorEmptyProbeCode)
		c.String(http.StatusOK, cursorEmptyProbeStream(modelName))
		return
	}

	c.Header("X-Octopus-Local-Validation", cursorEmptyProbeCode)
	c.JSON(http.StatusOK, map[string]any{
		"id":            "msg_octopus_cursor_empty_probe",
		"type":          "message",
		"role":          "assistant",
		"model":         modelName,
		"content":       []map[string]string{{"type": "text", "text": ""}},
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"usage": map[string]int{
			"input_tokens":  0,
			"output_tokens": 0,
		},
	})
}

func cursorEmptyProbeStream(modelName string) string {
	message := map[string]any{
		"id":      "msg_octopus_cursor_empty_probe",
		"type":    "message",
		"role":    "assistant",
		"content": []any{},
		"model":   modelName,
		"usage": map[string]int{
			"input_tokens":  0,
			"output_tokens": 0,
		},
	}
	stopReason := "end_turn"
	events := []struct {
		name string
		data map[string]any
	}{
		{name: "message_start", data: map[string]any{"type": "message_start", "message": message}},
		{name: "message_delta", data: map[string]any{
			"type": "message_delta",
			"delta": map[string]any{
				"stop_reason":   stopReason,
				"stop_sequence": nil,
			},
			"usage": map[string]int{"output_tokens": 0},
		}},
		{name: "message_stop", data: map[string]any{"type": "message_stop"}},
	}

	var builder strings.Builder
	for _, event := range events {
		data, err := json.Marshal(event.data)
		if err != nil {
			continue
		}
		builder.WriteString(fmt.Sprintf("event: %s\n", event.name))
		builder.WriteString("data: ")
		builder.Write(data)
		builder.WriteString("\n\n")
	}
	return builder.String()
}

func cursorEmptyProbeResponseContent() string {
	payload := map[string]any{
		"status":             "satisfied",
		"stage":              "local_validation",
		"compatibility":      "cursor_empty_anthropic_probe",
		"error_code":         cursorEmptyProbeCode,
		"upstream_forwarded": false,
		"billable":           false,
		"stats_counted":      false,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return string(b)
}

func clientValidationResponseContent(code string) string {
	payload := map[string]any{
		"status":             "intercepted",
		"stage":              "local_validation",
		"error_code":         code,
		"upstream_forwarded": false,
		"billable":           false,
		"stats_counted":      false,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return string(b)
}
