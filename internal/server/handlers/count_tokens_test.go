package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bestruirui/octopus/internal/server/middleware"
	anthropic "github.com/bestruirui/octopus/internal/transformer/inbound/anthropic"
	"github.com/gin-gonic/gin"
)

func strPtr(s string) *string { return &s }

func TestCountAnthropicInputTokens(t *testing.T) {
	empty := anthropic.MessageRequest{Model: "claude-3-5-sonnet"}
	if got := countAnthropicInputTokens(empty); got != 0 {
		t.Fatalf("empty request: got %d want 0", got)
	}

	withMsg := anthropic.MessageRequest{
		Model: "claude-3-5-sonnet",
		Messages: []anthropic.MessageParam{
			{Role: "user", Content: anthropic.MessageContent{Content: strPtr("Hello, how are you today?")}},
		},
	}
	base := countAnthropicInputTokens(withMsg)
	if base <= 0 {
		t.Fatalf("single message: got %d want > 0", base)
	}

	// More content must produce a larger estimate.
	withMore := anthropic.MessageRequest{
		Model: "claude-3-5-sonnet",
		Messages: []anthropic.MessageParam{
			{Role: "user", Content: anthropic.MessageContent{Content: strPtr("Hello, how are you today?")}},
			{Role: "assistant", Content: anthropic.MessageContent{Content: strPtr("I am doing great, thanks for asking. How can I help you with your code today?")}},
		},
	}
	more := countAnthropicInputTokens(withMore)
	if more <= base {
		t.Fatalf("more content: got %d want > %d", more, base)
	}

	// System prompt must be counted.
	withSystem := withMsg
	withSystem.System = &anthropic.SystemPrompt{Prompt: strPtr("You are a helpful assistant that writes Go.")}
	sys := countAnthropicInputTokens(withSystem)
	if sys <= base {
		t.Fatalf("system prompt: got %d want > %d", sys, base)
	}

	// Tools must be counted (name + description + schema + per-tool overhead).
	withTools := withMsg
	withTools.Tools = []anthropic.Tool{
		{
			Name:        "get_weather",
			Description: "Get the current weather for a given location.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"location":{"type":"string"}},"required":["location"]}`),
		},
	}
	tools := countAnthropicInputTokens(withTools)
	if tools <= base {
		t.Fatalf("tools: got %d want > %d", tools, base)
	}

	// Array content blocks (text) must be counted too.
	withBlocks := anthropic.MessageRequest{
		Model: "claude-3-5-sonnet",
		Messages: []anthropic.MessageParam{
			{Role: "user", Content: anthropic.MessageContent{MultipleContent: []anthropic.MessageContentBlock{
				{Type: "text", Text: strPtr("Explain goroutines and channels in detail please.")},
			}}},
		},
	}
	if got := countAnthropicInputTokens(withBlocks); got <= 0 {
		t.Fatalf("array content: got %d want > 0", got)
	}
}

// TestMessagesCountTokensHandler exercises the handler directly (no auth
// middleware) to validate body parsing and the 200 application/json response
// shape with a positive input_tokens count.
func TestMessagesCountTokensHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/v1/messages/count_tokens", messagesCountTokens)

	body := []byte(`{
		"model": "claude-3-5-sonnet",
		"max_tokens": 1024,
		"messages": [{"role": "user", "content": "Count the tokens in this sentence please."}]
	}`)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200, body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct == "" || ct[:16] != "application/json" {
		t.Fatalf("content-type: got %q want application/json", ct)
	}
	var out struct {
		InputTokens int64 `json:"input_tokens"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v body=%s", err, w.Body.String())
	}
	if out.InputTokens <= 0 {
		t.Fatalf("input_tokens: got %d want > 0", out.InputTokens)
	}
}

func TestMessagesCountTokensBadJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/v1/messages/count_tokens", messagesCountTokens)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", bytes.NewReader([]byte(`{not json`)))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400, body=%s", w.Code, w.Body.String())
	}
}

// TestMessagesCountTokensRequiresAuth checks that the production middleware
// chain rejects an unauthenticated request before reaching the handler. No DB
// access happens because the missing key fails fast in APIKeyAuth.
func TestMessagesCountTokensRequiresAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	grp := engine.Group("/v1", middleware.APIKeyAuth(), middleware.RequireJSON())
	grp.POST("/messages/count_tokens", messagesCountTokens)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", bytes.NewReader([]byte(`{"model":"x","max_tokens":1,"messages":[]}`)))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401, body=%s", w.Code, w.Body.String())
	}
}
