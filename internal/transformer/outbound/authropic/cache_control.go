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

	// Prefix breakpoint (system -> tools -> first user message), gated by min-chars.
	// This anchors the cache at the front of the request so the stable prefix is reused.
	// markFirstUserMessageCacheControl reports the message index it marked (or -1) so the
	// sliding breakpoints below can avoid double-marking the same block.
	prefixHit := false
	prefixUserIdx := -1
	if markSystemCacheControl(req, minChars) || markToolsCacheControl(req, minChars) {
		prefixHit = true
	} else if prefixUserIdx = markFirstUserMessageCacheControl(req, minChars); prefixUserIdx >= 0 {
		prefixHit = true
	}

	// Sliding breakpoints only make sense once a cacheable prefix exists: their value is
	// that the long prefix in front of them gets cached for the next turn (leapfrog). If
	// no prefix breakpoint was placed (nothing met min-chars), the request is too small to
	// be worth caching at all, so skip them — adding a lone breakpoint to a tiny request
	// just pays the write premium for nothing.
	if !prefixHit {
		return
	}

	// Mark up to the last two user messages. They don't need a min-chars gate of their
	// own — what matters is the cached prefix ahead of them.
	// Anthropic allows at most 4 cache_control breakpoints; prefix(1) + sliding(<=2) = <=3.
	markSlidingUserCacheControls(req, prefixUserIdx)
}

// markSlidingUserCacheControls marks the last user message, and (when there are at least
// 4 messages) the second-to-last user message. The message index already claimed by the
// prefix breakpoint (prefixUserIdx, -1 if none) is skipped to avoid re-marking the same block.
func markSlidingUserCacheControls(req *anthropicModel.MessageRequest, prefixUserIdx int) {
	lastUserIdx := lastUserMessageIndex(req, len(req.Messages))
	if lastUserIdx >= 0 && lastUserIdx != prefixUserIdx {
		markMessageContentCacheControl(&req.Messages[lastUserIdx].Content)
	}

	// Only add the second sliding breakpoint once the conversation has enough turns,
	// so short single-turn requests stay minimal.
	if len(req.Messages) < 4 {
		return
	}

	upperBound := lastUserIdx
	if upperBound < 0 {
		upperBound = len(req.Messages)
	}
	prevUserIdx := lastUserMessageIndex(req, upperBound)
	if prevUserIdx >= 0 && prevUserIdx != prefixUserIdx {
		markMessageContentCacheControl(&req.Messages[prevUserIdx].Content)
	}
}

// lastUserMessageIndex returns the index of the last user message strictly before `before`,
// or -1 if none exists.
func lastUserMessageIndex(req *anthropicModel.MessageRequest, before int) int {
	if before > len(req.Messages) {
		before = len(req.Messages)
	}
	for i := before - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			return i
		}
	}
	return -1
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

// markFirstUserMessageCacheControl marks the first user message whose text meets the
// min-chars threshold. It returns the index of the marked message, or -1 if none qualified
// (or the chosen block had no cacheable text). The index lets the sliding breakpoints skip
// a block that's already been marked as the prefix breakpoint.
func markFirstUserMessageCacheControl(req *anthropicModel.MessageRequest, minChars int) int {
	for i := range req.Messages {
		if req.Messages[i].Role != "user" || messageContentTextLen(req.Messages[i].Content) < minChars {
			continue
		}
		if markMessageContentCacheControl(&req.Messages[i].Content) {
			return i
		}
		return -1
	}
	return -1
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
