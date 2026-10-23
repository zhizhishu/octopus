package authropic

import (
	"context"
	"testing"
)

// 这组测试盯的是**缓存费**：Anthropic 流式请求的 message_delta 只带 output_tokens，
// 缓存读写量只在 message_start 里出现一次。
//
// 旧实现用 convertAnthropicUsage(delta) 重建整个 usage，只把 PromptTokens 补回来，
// cache_read / cache_creation 被写成 0，再由 `o.streamUsage = usage` 覆盖 message_start
// 的那份真值；message_stop 把它当最终 usage 交出去。后果是三重的：
//   - 计费：cacheRead×BaseCacheReadPrice、cacheWrite×BaseCacheWritePrice 两项恒为 0，
//     走 Anthropic 渠道的流式请求系统性少收缓存费（Claude Code 默认就是流式+带缓存）；
//   - 观测：relay_logs 的 cache_hit_token / cache_write_token 恒 0，看着像没命中缓存；
//   - 客户端：total_tokens 被重算成 prompt+output，丢掉 cache 部分。
//
// 修法是合并（delta 里为 0 的字段保留 prev），不是重建。
func TestStreamMessageDeltaKeepsCacheTokensFromMessageStart(t *testing.T) {
	o := &MessageOutbound{}
	ctx := context.Background()

	// message_start：真实的 Anthropic 形态 —— input 不含缓存，缓存单列。
	start := []byte(`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[],"usage":{"input_tokens":12,"output_tokens":1,"cache_read_input_tokens":40000,"cache_creation_input_tokens":3000}}}`)
	if _, err := o.TransformStream(ctx, start); err != nil {
		t.Fatalf("message_start: %v", err)
	}

	// message_delta：只带 output_tokens（这是 Anthropic 的真实行为）。
	delta := []byte(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":250}}`)
	if _, err := o.TransformStream(ctx, delta); err != nil {
		t.Fatalf("message_delta: %v", err)
	}

	stop, err := o.TransformStream(ctx, []byte(`{"type":"message_stop"}`))
	if err != nil {
		t.Fatalf("message_stop: %v", err)
	}
	if stop == nil || stop.Usage == nil {
		t.Fatal("message_stop 没有带出 usage")
	}

	u := stop.Usage
	if u.CacheCreationInputTokens != 3000 {
		t.Errorf("cache_creation_input_tokens = %d, want 3000（被 message_delta 抹成 0 → 缓存写入费漏收）",
			u.CacheCreationInputTokens)
	}
	if u.PromptTokensDetails == nil || u.PromptTokensDetails.CachedTokens != 40000 {
		got := int64(0)
		if u.PromptTokensDetails != nil {
			got = u.PromptTokensDetails.CachedTokens
		}
		t.Errorf("cache_read_input_tokens = %d, want 40000（被抹成 0 → 缓存命中费漏收）", got)
	}
	if u.CompletionTokens != 250 {
		t.Errorf("output_tokens = %d, want 250（delta 的累计值应以 delta 为准）", u.CompletionTokens)
	}
	if u.PromptTokens != 12 {
		t.Errorf("input_tokens = %d, want 12", u.PromptTokens)
	}
	// Anthropic 语义：总量含缓存。
	if want := int64(12 + 250 + 40000 + 3000); u.TotalTokens != want {
		t.Errorf("total_tokens = %d, want %d（total 丢了缓存部分）", u.TotalTokens, want)
	}
	if !u.AnthropicUsage {
		t.Error("AnthropicUsage 标记被 delta 的零值翻掉了：计费侧会按 OpenAI 语义重复折叠缓存")
	}
}

// delta 自己带了缓存值时，以 delta 为准（不覆盖成 prev 的旧值）。
func TestStreamMessageDeltaAppliesItsOwnCacheValues(t *testing.T) {
	o := &MessageOutbound{}
	ctx := context.Background()

	start := []byte(`{"type":"message_start","message":{"id":"msg_2","type":"message","role":"assistant","model":"m","content":[],"usage":{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":1000}}}`)
	if _, err := o.TransformStream(ctx, start); err != nil {
		t.Fatalf("message_start: %v", err)
	}
	delta := []byte(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":50,"cache_read_input_tokens":2000}}`)
	if _, err := o.TransformStream(ctx, delta); err != nil {
		t.Fatalf("message_delta: %v", err)
	}
	stop, err := o.TransformStream(ctx, []byte(`{"type":"message_stop"}`))
	if err != nil || stop == nil || stop.Usage == nil {
		t.Fatalf("message_stop: %v", err)
	}
	if stop.Usage.PromptTokensDetails == nil || stop.Usage.PromptTokensDetails.CachedTokens != 2000 {
		t.Fatalf("delta 明确带了 2000 却没用上：合并方向错了")
	}
}
