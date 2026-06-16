package relay

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

type responsesFailedError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type responsesFailedBody struct {
	ID     string               `json:"id"`
	Object string               `json:"object"`
	Model  string               `json:"model,omitempty"`
	Status string               `json:"status"`
	Output []any                `json:"output"`
	Error  responsesFailedError `json:"error"`
}

type responsesFailedEvent struct {
	Type     string              `json:"type"`
	Response responsesFailedBody `json:"response"`
}

type anthropicStreamErrorBody struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeResponsesFailedSSE(c *gin.Context, requestModel string, code string, message string) bool {
	if c == nil || c.Writer == nil {
		return false
	}
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		return false
	}
	code = strings.TrimSpace(code)
	if code == "" {
		code = "upstream_error"
	}
	message = strings.TrimSpace(message)
	if message == "" {
		message = "upstream stream failed"
	}

	payload, err := json.Marshal(responsesFailedEvent{
		Type: "response.failed",
		Response: responsesFailedBody{
			ID:     fmt.Sprintf("resp_%d", time.Now().UnixNano()),
			Object: "response",
			Model:  strings.TrimSpace(requestModel),
			Status: "failed",
			Output: []any{},
			Error: responsesFailedError{
				Code:    code,
				Message: message,
			},
		},
	})
	if err != nil {
		_ = c.Error(err)
		return true
	}

	if _, err := fmt.Fprintf(c.Writer, "event: response.failed\ndata: %s\n\ndata: [DONE]\n\n", payload); err != nil {
		_ = c.Error(err)
		return true
	}
	flusher.Flush()
	return true
}

func writeAnthropicErrorSSE(c *gin.Context, errorType string, message string) bool {
	if c == nil || c.Writer == nil {
		return false
	}
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		return false
	}
	errorType = strings.TrimSpace(errorType)
	if errorType == "" {
		errorType = "api_error"
	}
	message = strings.TrimSpace(message)
	if message == "" {
		message = "upstream stream failed before terminal event"
	}

	body := anthropicStreamErrorBody{Type: "error"}
	body.Error.Type = errorType
	body.Error.Message = message
	payload, err := json.Marshal(body)
	if err != nil {
		_ = c.Error(err)
		return true
	}

	if _, err := fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", payload); err != nil {
		_ = c.Error(err)
		return true
	}
	flusher.Flush()
	return true
}

func isResponsesInboundPath(path string) bool {
	path = strings.TrimRight(strings.TrimSpace(path), "/")
	if path == "" {
		return false
	}
	return strings.HasSuffix(path, "/responses") || strings.Contains(path, "/responses/")
}

func responsesStreamFailureMessage(err error) string {
	if err == nil {
		return "upstream stream failed"
	}
	if isClientAbortError(err) {
		return ""
	}
	msg := strings.TrimSpace(err.Error())
	if msg == "" {
		return "upstream stream failed"
	}
	if strings.Contains(strings.ToLower(msg), "client disconnected") {
		return ""
	}
	return "upstream stream failed"
}

func anthropicStreamFailureMessage(err error) string {
	if err == nil {
		return "upstream stream failed before terminal event"
	}
	if isClientAbortError(err) {
		return ""
	}
	return "upstream stream failed before terminal event"
}
