package relay

import (
	"context"
	"math"
	"testing"

	"github.com/zhizhishu/octopus/internal/transformer/model"
	anthropicOutbound "github.com/zhizhishu/octopus/internal/transformer/outbound/authropic"
	openaiOutbound "github.com/zhizhishu/octopus/internal/transformer/outbound/openai"
)

// End-to-end check of the number the logs/dashboard show as 缓存命中率: run the real
// upstream stream shapes through the outbound transformer, then through the same
// usageCacheStats/cacheHitRate the relay log uses.

func lastStreamUsage(t *testing.T, feed func([]byte) (*model.InternalLLMResponse, error), events ...string) *model.Usage {
	t.Helper()
	var usage *model.Usage
	for _, ev := range events {
		resp, err := feed([]byte(ev))
		if err != nil {
			t.Fatalf("stream event %s: %v", ev, err)
		}
		if resp != nil && resp.Usage != nil {
			usage = resp.Usage
		}
	}
	if usage == nil {
		t.Fatalf("stream produced no usage")
	}
	return usage
}

func TestCacheHitRateFromClaudeStream(t *testing.T) {
	o := &anthropicOutbound.MessageOutbound{}
	ctx := context.Background()
	usage := lastStreamUsage(t, func(b []byte) (*model.InternalLLMResponse, error) { return o.TransformStream(ctx, b) },
		`{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"claude","content":[],"usage":{"input_tokens":100,"cache_read_input_tokens":9000,"cache_creation_input_tokens":900,"output_tokens":1}}}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}`,
		`{"type":"message_stop"}`,
	)
	hit, write, input := usageCacheStats(usage)
	if hit != 9000 || write != 900 || input != 10000 {
		t.Fatalf("claude stream cache stats hit=%d write=%d input=%d, want 9000/900/10000", hit, write, input)
	}
	if rate := cacheHitRate(hit, input); math.Abs(rate-0.9) > 1e-9 {
		t.Fatalf("claude hit rate = %v, want 0.9", rate)
	}
}

func TestCacheHitRateFromCodexResponsesStream(t *testing.T) {
	o := &openaiOutbound.ResponseOutbound{}
	ctx := context.Background()
	usage := lastStreamUsage(t, func(b []byte) (*model.InternalLLMResponse, error) { return o.TransformStream(ctx, b) },
		`{"type":"response.created","sequence_number":0,"response":{"id":"resp_1","object":"response","created_at":1,"model":"gpt-5","status":"in_progress","output":[]}}`,
		`{"type":"response.completed","sequence_number":1,"response":{"id":"resp_1","object":"response","created_at":1,"model":"gpt-5","status":"completed","output":[],"usage":{"input_tokens":10000,"input_tokens_details":{"cached_tokens":8000},"output_tokens":20,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":10020}}}`,
	)
	hit, write, input := usageCacheStats(usage)
	// OpenAI input_tokens already include cached_tokens: the base must not double count.
	if hit != 8000 || write != 0 || input != 10000 {
		t.Fatalf("codex stream cache stats hit=%d write=%d input=%d, want 8000/0/10000", hit, write, input)
	}
	if rate := cacheHitRate(hit, input); math.Abs(rate-0.8) > 1e-9 {
		t.Fatalf("codex hit rate = %v, want 0.8", rate)
	}
}
