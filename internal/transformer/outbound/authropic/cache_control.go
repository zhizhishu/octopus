package authropic

import (
	"strings"

	anthropicModel "github.com/bestruirui/octopus/internal/transformer/inbound/anthropic"
)

const (
	anthropicAutoCacheControlMinChars      = 4096
	anthropicAutoCacheControlHaikuMinChars = 8192
)

func applyAutomaticCacheControl(req *anthropicModel.MessageRequest) {
	if req == nil || countCacheControls(req) > 0 {
		return
	}

	minChars := autoCacheControlMinChars(req.Model)
	if markSystemCacheControl(req, minChars) {
		return
	}
	if markToolsCacheControl(req, minChars) {
		return
	}
	markFirstUserMessageCacheControl(req, minChars)
}

func autoCacheControlMinChars(model string) int {
	if strings.Contains(strings.ToLower(model), "haiku") {
		return anthropicAutoCacheControlHaikuMinChars
	}
	return anthropicAutoCacheControlMinChars
}

func autoCacheControl() *anthropicModel.CacheControl {
	return &anthropicModel.CacheControl{Type: "ephemeral"}
}

func countCacheControls(req *anthropicModel.MessageRequest) int {
	count := 0
	if req.System != nil {
		for _, part := range req.System.MultiplePrompts {
			if part.CacheControl != nil {
				count++
			}
		}
	}
	for _, tool := range req.Tools {
		if tool.CacheControl != nil {
			count++
		}
	}
	for _, msg := range req.Messages {
		for _, block := range messageContentBlocks(msg.Content) {
			if block.CacheControl != nil {
				count++
			}
		}
	}
	return count
}

func markSystemCacheControl(req *anthropicModel.MessageRequest, minChars int) bool {
	if req.System == nil {
		return false
	}

	if req.System.Prompt != nil {
		text := *req.System.Prompt
		if len(text) < minChars {
			return false
		}
		req.System = &anthropicModel.SystemPrompt{
			MultiplePrompts: []anthropicModel.SystemPromptPart{{
				Type:         "text",
				Text:         text,
				CacheControl: autoCacheControl(),
			}},
		}
		return true
	}

	if len(req.System.MultiplePrompts) == 0 || systemPromptTextLen(req.System) < minChars {
		return false
	}
	for i := len(req.System.MultiplePrompts) - 1; i >= 0; i-- {
		if req.System.MultiplePrompts[i].Text == "" {
			continue
		}
		req.System.MultiplePrompts[i].CacheControl = autoCacheControl()
		return true
	}
	return false
}

func systemPromptTextLen(system *anthropicModel.SystemPrompt) int {
	if system == nil {
		return 0
	}
	if system.Prompt != nil {
		return len(*system.Prompt)
	}
	total := 0
	for _, part := range system.MultiplePrompts {
		total += len(part.Text)
	}
	return total
}

func markToolsCacheControl(req *anthropicModel.MessageRequest, minChars int) bool {
	if len(req.Tools) == 0 || toolsTextLen(req.Tools) < minChars {
		return false
	}
	req.Tools[len(req.Tools)-1].CacheControl = autoCacheControl()
	return true
}

func toolsTextLen(tools []anthropicModel.Tool) int {
	total := 0
	for _, tool := range tools {
		total += len(tool.Name)
		total += len(tool.Description)
		total += len(tool.InputSchema)
	}
	return total
}

func markFirstUserMessageCacheControl(req *anthropicModel.MessageRequest, minChars int) bool {
	for i := range req.Messages {
		if req.Messages[i].Role != "user" || messageContentTextLen(req.Messages[i].Content) < minChars {
			continue
		}
		return markMessageContentCacheControl(&req.Messages[i].Content)
	}
	return false
}

func markMessageContentCacheControl(content *anthropicModel.MessageContent) bool {
	if content == nil {
		return false
	}
	if content.Content != nil {
		text := *content.Content
		content.Content = nil
		content.MultipleContent = []anthropicModel.MessageContentBlock{{
			Type:         "text",
			Text:         &text,
			CacheControl: autoCacheControl(),
		}}
		return true
	}
	for i := len(content.MultipleContent) - 1; i >= 0; i-- {
		block := &content.MultipleContent[i]
		if block.Type != "text" || block.Text == nil || *block.Text == "" {
			continue
		}
		block.CacheControl = autoCacheControl()
		return true
	}
	return false
}

func messageContentTextLen(content anthropicModel.MessageContent) int {
	total := 0
	if content.Content != nil {
		total += len(*content.Content)
	}
	for _, block := range content.MultipleContent {
		if block.Type == "text" && block.Text != nil {
			total += len(*block.Text)
		}
	}
	return total
}

func messageContentBlocks(content anthropicModel.MessageContent) []anthropicModel.MessageContentBlock {
	if len(content.MultipleContent) > 0 {
		return content.MultipleContent
	}
	if content.Content == nil {
		return nil
	}
	return []anthropicModel.MessageContentBlock{{Type: "text", Text: content.Content}}
}
