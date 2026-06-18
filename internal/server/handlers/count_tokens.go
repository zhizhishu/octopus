package handlers

import (
	"net/http"

	"github.com/bestruirui/octopus/internal/server/middleware"
	"github.com/bestruirui/octopus/internal/server/resp"
	"github.com/bestruirui/octopus/internal/server/router"
	anthropic "github.com/bestruirui/octopus/internal/transformer/inbound/anthropic"
	"github.com/bestruirui/octopus/internal/utils/tokenizer"
	"github.com/gin-gonic/gin"
)

func init() {
	router.NewGroupRouter("/v1").
		Use(middleware.APIKeyAuth()).
		Use(middleware.RequireJSON()).
		AddRoute(
			router.NewRoute("/messages/count_tokens", http.MethodPost).
				Handle(messagesCountTokens),
		)
}

// messagesCountTokens implements POST /v1/messages/count_tokens.
//
// Claude Code / Anthropic IDE tooling calls this endpoint to pre-estimate the
// prompt size before sending a /v1/messages request. We do NOT forward this to
// an upstream provider; instead we compute a LOCAL estimate using the same
// tiktoken-based tokenizer and the same accumulation rules that the Anthropic
// inbound transformer uses when it derives inputToken. The returned value is an
// approximation (self-contained, low risk), not the upstream-precise count.
func messagesCountTokens(c *gin.Context) {
	var req anthropic.MessageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"input_tokens": countAnthropicInputTokens(req),
	})
}

// countAnthropicInputTokens is a pure local estimator that mirrors the token
// accounting in internal/transformer/inbound/anthropic (messages.go): it sums
// the system prompt, every textual message block, and each tool definition
// (name + description + input schema) plus a small per-tool overhead.
//
// It only reads the Anthropic request model types; it does not mutate them and
// does not touch the transformer pipeline.
func countAnthropicInputTokens(req anthropic.MessageRequest) int64 {
	model := req.Model
	var total int64

	// System prompt: either a single string or an array of text parts.
	if req.System != nil {
		if req.System.Prompt != nil && *req.System.Prompt != "" {
			total += int64(tokenizer.CountTokens(*req.System.Prompt, model))
		}
		for _, part := range req.System.MultiplePrompts {
			if part.Text != "" {
				total += int64(tokenizer.CountTokens(part.Text, model))
			}
		}
	}

	// Messages: string content, or text blocks within array content.
	for _, msg := range req.Messages {
		if msg.Content.Content != nil && *msg.Content.Content != "" {
			total += int64(tokenizer.CountTokens(*msg.Content.Content, model))
			continue
		}
		for _, block := range msg.Content.MultipleContent {
			total += countContentBlockTokens(block, model)
		}
	}

	// Tools: name + description + input schema, plus per-tool overhead to match
	// the inbound transformer (len(tools) * 3).
	for _, tool := range req.Tools {
		total += int64(tokenizer.CountTokens(tool.Name, model))
		total += int64(tokenizer.CountTokens(tool.Description, model))
		total += int64(tokenizer.CountTokens(string(tool.InputSchema), model))
	}
	if len(req.Tools) > 0 {
		total += int64(len(req.Tools) * 3)
	}

	return total
}

// imageBlockTokenEstimate is a conservative, flat per-image approximation. We
// cannot know the real pixel dimensions without decoding the image, so we use a
// constant in the ballpark of a single medium-sized Anthropic image (Anthropic
// bills vision tokens roughly as (width*height)/750, which for a ~1100x1100px
// image lands near ~1600 tokens). This intentionally over- rather than
// under-estimates so callers do not blow past upstream limits. It is an
// approximation, not the upstream-precise count.
const imageBlockTokenEstimate int64 = 1600

// countContentBlockTokens counts the textual payload of a single content block,
// recursing into tool_result content which may itself carry text blocks. Image
// blocks contribute a flat conservative estimate (see imageBlockTokenEstimate)
// since their true token cost depends on pixel dimensions we cannot read here.
func countContentBlockTokens(block anthropic.MessageContentBlock, model string) int64 {
	var total int64
	if block.Text != nil && *block.Text != "" {
		total += int64(tokenizer.CountTokens(*block.Text, model))
	}
	// Image blocks (base64 or url source) have no text we can tokenize; charge a
	// flat conservative per-image estimate. Approximate by design.
	if block.Type == "image" && block.Source != nil {
		total += imageBlockTokenEstimate
	}
	if block.Thinking != nil && *block.Thinking != "" {
		total += int64(tokenizer.CountTokens(*block.Thinking, model))
	}
	if len(block.Input) > 0 {
		total += int64(tokenizer.CountTokens(string(block.Input), model))
	}
	if block.Content != nil {
		if block.Content.Content != nil && *block.Content.Content != "" {
			total += int64(tokenizer.CountTokens(*block.Content.Content, model))
		}
		for _, inner := range block.Content.MultipleContent {
			total += countContentBlockTokens(inner, model)
		}
	}
	return total
}
