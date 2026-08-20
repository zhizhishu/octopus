package relay

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/bestruirui/octopus/internal/server/resp"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
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

type chatStreamErrorBody struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code,omitempty"`
	} `json:"error"`
}

// writeChatErrorSSE emits an OpenAI chat-completions-style error onto an already
// committed SSE stream (used when comment heartbeats committed HTTP 200 during
// failover and every channel then failed). It closes with the [DONE] sentinel.
func writeChatErrorSSE(c *gin.Context, code string, message string) bool {
	if c == nil || c.Writer == nil {
		return false
	}
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		return false
	}
	message = strings.TrimSpace(message)
	if message == "" {
		message = "upstream stream failed"
	}
	body := chatStreamErrorBody{}
	body.Error.Message = message
	body.Error.Type = "upstream_error"
	body.Error.Code = strings.TrimSpace(code)
	payload, err := json.Marshal(body)
	if err != nil {
		_ = c.Error(err)
		return true
	}
	if _, err := fmt.Fprintf(c.Writer, "data: %s\n\ndata: [DONE]\n\n", payload); err != nil {
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

// writeRelayErrorPreStream writes a pre-stream error in the inbound-aware envelope.
//
// On the /v1/responses path, octopus's internal ResponseStruct {code,error_code,message}
// is NOT what cursor's responses parser expects — it sees an unknown shape and surfaces
// "OpenAI Responses API failed: unknown error" to the user. Mirror new-api's types.NewAPIError
// contract: ALWAYS emit the OpenAI shape {"error":{"message":..,"type":..,"code":..}} to
// OpenAI-protocol inbound. Chat / Anthropic inbound keep their existing octopus-internal
// / Anthropic error shapes; only the responses inbound changes here.
//
// This is the pre-stream (no SSE prelude committed) branch counterpart of the post-stream
// switch in relay.go (writeResponsesFailedSSE / writeChatErrorSSE / writeAnthropicErrorSSE):
// here c.Writer.Written() is false and we deliver the error as a normal JSON HTTP response.
// Once any meaningful bytes are flushed (heartbeats or prelude), the post-stream switch
// takes over and writes the error in-band on the SSE stream.
func writeRelayErrorPreStream(c *gin.Context, inboundType inbound.InboundType, httpStatus int, errType string, errCode string, message string) {
	switch inboundType {
	case inbound.InboundTypeOpenAIResponse:
		resp.OpenAIError(c, httpStatus, errType, errCode, message)
	default:
		if errCode == "" {
			resp.Error(c, httpStatus, message)
		} else {
			resp.ErrorWithCode(c, httpStatus, errCode, message)
		}
	}
}
