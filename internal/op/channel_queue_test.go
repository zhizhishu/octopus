package op

import (
	"errors"
	"testing"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// 渠道「排队可调」在 op 层的两条口径：部分更新必须真的落库（包括显式 0），
// 且校验按「老行 + 本次 patch」的合并结果做，不能只看 patch 里出现了哪一半。

func TestChannelUpdatePersistsQueueSettings(t *testing.T) {
	ctx := setupChannelImportTest(t)

	channel := model.Channel{
		Name:           "queue-persist",
		Type:           outbound.OutboundTypeOpenAIChat,
		Enabled:        true,
		MaxConcurrent:  4,
		QueueSize:      2,
		QueueTimeoutMs: 800,
		BaseUrls:       []model.BaseUrl{{URL: "https://upstream.example"}},
		Keys:           []model.ChannelKey{{ChannelKey: "sk-queue-1", Enabled: true}},
	}
	// 创建侧校验是 handler 的职责（model.Channel.Validate），这里先证明这组值是合法的。
	if err := channel.Validate(); err != nil {
		t.Fatalf("queue settings must be valid on create: %v", err)
	}
	if err := ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("ChannelCreate: %v", err)
	}
	created, err := ChannelGet(channel.ID, ctx)
	if err != nil {
		t.Fatalf("ChannelGet: %v", err)
	}
	if created.QueueSize != 2 || created.QueueTimeoutMs != 800 {
		t.Fatalf("created queue settings = (%d,%d), want (2,800)", created.QueueSize, created.QueueTimeoutMs)
	}

	// 只改排队时长：另一个字段没提，库里的值必须原样保留。
	newTimeout := 1500
	updated, err := ChannelUpdate(&model.ChannelUpdateRequest{ID: channel.ID, QueueTimeoutMs: &newTimeout}, ctx)
	if err != nil {
		t.Fatalf("ChannelUpdate(queue_timeout_ms): %v", err)
	}
	if updated.QueueTimeoutMs != 1500 || updated.QueueSize != 2 {
		t.Fatalf("after timeout-only patch = (%d,%d), want (2,1500)", updated.QueueSize, updated.QueueTimeoutMs)
	}

	// 只改 queue_size（存量 max_concurrent=4 合法）：只看本次 patch 会误判成「没并发上限」。
	newQueue := 6
	updated, err = ChannelUpdate(&model.ChannelUpdateRequest{ID: channel.ID, QueueSize: &newQueue}, ctx)
	if err != nil {
		t.Fatalf("ChannelUpdate(queue_size) must be validated against the stored max_concurrent: %v", err)
	}
	if updated.QueueSize != 6 {
		t.Fatalf("queue_size after patch = %d, want 6", updated.QueueSize)
	}

	// 显式 0 必须真的落库（把排队关掉），不能被 Updates(struct) 当成零值跳过。
	zero := 0
	updated, err = ChannelUpdate(&model.ChannelUpdateRequest{ID: channel.ID, QueueSize: &zero}, ctx)
	if err != nil {
		t.Fatalf("ChannelUpdate(queue_size=0): %v", err)
	}
	if updated.QueueSize != 0 {
		t.Fatalf("explicit queue_size=0 must be persisted, got %d", updated.QueueSize)
	}
	stored, err := ChannelGet(channel.ID, ctx)
	if err != nil {
		t.Fatalf("ChannelGet after zeroing: %v", err)
	}
	if stored.QueueSize != 0 {
		t.Fatalf("explicit queue_size=0 must survive a cache refresh, got %d", stored.QueueSize)
	}
	if stored.QueueTimeoutMs != 1500 {
		t.Fatalf("untouched queue_timeout_ms must stay 1500, got %d", stored.QueueTimeoutMs)
	}
}

func TestChannelUpdateRejectsQueueWithoutMergedConcurrency(t *testing.T) {
	ctx := setupChannelImportTest(t)

	channel := model.Channel{
		Name:           "queue-merged-check",
		Type:           outbound.OutboundTypeOpenAIChat,
		Enabled:        true,
		MaxConcurrent:  2,
		QueueSize:      3,
		QueueTimeoutMs: 0,
		BaseUrls:       []model.BaseUrl{{URL: "https://upstream.example"}},
		Keys:           []model.ChannelKey{{ChannelKey: "sk-queue-2", Enabled: true}},
	}
	if err := ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("ChannelCreate: %v", err)
	}

	// 只把 max_concurrent 改成 0：库里 queue_size=3 还在，合并后的组合非法，必须拒
	// （并带上 ErrChannelQueueSettings，handler 才能回 400）。
	zeroConcurrency := 0
	if _, err := ChannelUpdate(&model.ChannelUpdateRequest{ID: channel.ID, MaxConcurrent: &zeroConcurrency}, ctx); !errors.Is(err, ErrChannelQueueSettings) {
		t.Fatalf("zeroing max_concurrent while queue_size>0 is stored must be rejected with ErrChannelQueueSettings, got %v", err)
	}
	after, err := ChannelGet(channel.ID, ctx)
	if err != nil {
		t.Fatalf("ChannelGet after rejected update: %v", err)
	}
	if after.MaxConcurrent != 2 || after.QueueSize != 3 {
		t.Fatalf("a rejected update must not be persisted, got max_concurrent=%d queue_size=%d", after.MaxConcurrent, after.QueueSize)
	}

	// 同一个请求里把两者一起收掉是合法的（先把并发上限降到 0、同时关掉排队）。
	zeroQueue := 0
	updated, err := ChannelUpdate(&model.ChannelUpdateRequest{
		ID:            channel.ID,
		MaxConcurrent: &zeroConcurrency,
		QueueSize:     &zeroQueue,
	}, ctx)
	if err != nil {
		t.Fatalf("dropping queue and concurrency together must be accepted: %v", err)
	}
	if updated.MaxConcurrent != 0 || updated.QueueSize != 0 {
		t.Fatalf("combined patch = (max_concurrent=%d, queue_size=%d), want (0,0)", updated.MaxConcurrent, updated.QueueSize)
	}

	// 负数的排队时长同样不允许落库，同样可被 handler 映射为 400。
	negative := -1
	if _, err := ChannelUpdate(&model.ChannelUpdateRequest{ID: channel.ID, QueueTimeoutMs: &negative}, ctx); !errors.Is(err, ErrChannelQueueSettings) {
		t.Fatalf("negative queue_timeout_ms must be rejected with ErrChannelQueueSettings, got %v", err)
	}
}
