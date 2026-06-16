package relay

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/bestruirui/octopus/internal/transformer/inbound"
	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/gin-gonic/gin"
)

func maybeHandleCursorEmptyOpenAIProbe(c *gin.Context, inboundType inbound.InboundType, req *transformerModel.InternalLLMRequest, body []byte, err error) bool {
	if c == nil || req == nil || !isEmptyClientRequestError(err) {
		return false
	}
	if inboundType != inbound.InboundTypeOpenAIChat && inboundType != inbound.InboundTypeOpenAIResponse {
		return false
	}
	if c.Request == nil || c.Request.URL == nil {
		return false
	}
	if !requestHasNoEffectiveInput(req) || !looksLikeCursorEmptyOpenAIProbe(c.Request, body) {
		return false
	}

	saveCursorEmptyProbeRelayLog(c.Request.Context(), c, req, body)
	writeCursorEmptyOpenAIProbeResponse(c, req, inboundType)
	return true
}

func looksLikeCursorEmptyOpenAIProbe(r *http.Request, body []byte) bool {
	bodyLooksLikeProbe := false
	var probe struct {
		Messages        []json.RawMessage `json:"messages"`
		Input           json.RawMessage   `json:"input"`
		MaxTokens       int64             `json:"max_tokens"`
		MaxOutputTokens int64             `json:"max_output_tokens"`
		Stream          *bool             `json:"stream"`
		Tools           []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(body, &probe); err == nil && probe.Stream != nil && *probe.Stream {
		bodyLooksLikeProbe = (probe.MaxTokens >= 100000 || probe.MaxOutputTokens >= 100000) &&
			probe.Tools != nil &&
			len(probe.Tools) == 0 &&
			len(probe.Messages) == 0
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

func writeCursorEmptyOpenAIProbeResponse(c *gin.Context, req *transformerModel.InternalLLMRequest, inboundType inbound.InboundType) {
	stream := req.Stream != nil && *req.Stream
	modelName := strings.TrimSpace(req.Model)
	if modelName == "" {
		modelName = "gpt-4o"
	}
	c.Header("X-Octopus-Local-Validation", cursorEmptyProbeCode)

	if inboundType == inbound.InboundTypeOpenAIResponse {
		if stream {
			c.Header("Content-Type", "text/event-stream")
			c.Header("Cache-Control", "no-cache")
			c.String(http.StatusOK, cursorEmptyProbeOpenAIResponsesStream(modelName))
			return
		}
		c.JSON(http.StatusOK, cursorEmptyProbeOpenAIResponsesJSON(modelName))
		return
	}

	if stream {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.String(http.StatusOK, cursorEmptyProbeOpenAIChatStream(modelName))
		return
	}
	c.JSON(http.StatusOK, cursorEmptyProbeOpenAIChatJSON(modelName))
}

func cursorEmptyProbeOpenAIChatJSON(modelName string) map[string]any {
	return map[string]any{
		"id":      "chatcmpl-octopus_cursor_empty_probe",
		"object":  "chat.completion",
		"created": 0,
		"model":   modelName,
		"choices": []map[string]any{{
			"index":         0,
			"finish_reason": "stop",
			"message": map[string]any{
				"role":    "assistant",
				"content": "",
			},
		}},
		"usage": map[string]int{
			"prompt_tokens":     0,
			"completion_tokens": 0,
			"total_tokens":      0,
		},
	}
}

func cursorEmptyProbeOpenAIChatStream(modelName string) string {
	chunk := map[string]any{
		"id":      "chatcmpl-octopus_cursor_empty_probe",
		"object":  "chat.completion.chunk",
		"created": 0,
		"model":   modelName,
		"choices": []map[string]any{{
			"index":         0,
			"finish_reason": nil,
			"delta": map[string]any{
				"role":    "assistant",
				"content": "",
			},
		}},
	}
	doneChunk := map[string]any{
		"id":      "chatcmpl-octopus_cursor_empty_probe",
		"object":  "chat.completion.chunk",
		"created": 0,
		"model":   modelName,
		"choices": []map[string]any{{
			"index":         0,
			"finish_reason": "stop",
		}},
	}
	data, _ := json.Marshal(chunk)
	doneData, _ := json.Marshal(doneChunk)
	return fmt.Sprintf("data: %s\n\ndata: %s\n\ndata: [DONE]\n\n", string(data), string(doneData))
}

func cursorEmptyProbeOpenAIResponsesJSON(modelName string) map[string]any {
	return map[string]any{
		"id":     "resp_octopus_cursor_empty_probe",
		"object": "response",
		"model":  modelName,
		"status": "completed",
		"output": []any{},
		"usage":  map[string]int{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0},
	}
}

func cursorEmptyProbeOpenAIResponsesStream(modelName string) string {
	created := map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id":     "resp_octopus_cursor_empty_probe",
			"object": "response",
			"model":  modelName,
			"status": "in_progress",
			"output": []any{},
		},
	}
	completed := map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id":     "resp_octopus_cursor_empty_probe",
			"object": "response",
			"model":  modelName,
			"status": "completed",
			"output": []any{},
			"usage":  map[string]int{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0},
		},
	}
	data1, _ := json.Marshal(created)
	data2, _ := json.Marshal(completed)
	return fmt.Sprintf("event: response.created\ndata: %s\n\nevent: response.completed\ndata: %s\n\ndata: [DONE]\n\n", string(data1), string(data2))
}
