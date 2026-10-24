package task

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// 定时检测入口（runScheduledAuditTarget）的两条运行时事实：
//
//  1. 它仍然真的把探针发到渠道的 base url 上（判据/发送器接线没断）；
//  2. 嵌入渠道（type=OpenAIEmbedding）在发请求之前就被探针准入拒掉。
//
// 这里**不断言具体是哪个端点名**，因为那个值在运行时不可观测：端点实参只影响
// model_test 的请求组装，而某渠道类型发到线上的报文形状由该类型的出站适配器决定
// （实测：Anthropic 渠道无论端点给 anthropic_messages 还是 openai_chat，发出去的
// body 都是 claude-cli 形状）——照"形状"断言端点等于给自己造一条永远绿的假绿灯。
// 「两个入口的端点取值必然相同」由 internal/op/audit_endpoint_single_source_test.go
// 静态证明：两处调用点的端点实参唯一来源就是 op.AuditEndpointForChannel。
func TestScheduledAuditTargetForwardsProbeToChannelBaseURL(t *testing.T) {
	setupScheduledAuditTestDB(t)

	var (
		mu    sync.Mutex
		paths []string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_audit","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	t.Cleanup(upstream.Close)

	channel := model.Channel{
		ID:       1,
		Name:     "scheduled-audit-anthropic",
		Type:     outbound.OutboundTypeAnthropic,
		Enabled:  true,
		Model:    "claude-sonnet-example",
		BaseUrls: []model.BaseUrl{{URL: upstream.URL}},
		Keys:     []model.ChannelKey{{ID: 1, Enabled: true, ChannelKey: "scheduled-audit-key"}},
	}

	item := runScheduledAuditTarget(context.Background(), scheduledAuditTarget{
		channel: channel,
		model:   channel.Model,
		trigger: "schedule",
	})
	if item.Skipped {
		t.Fatalf("定时检测被跳过：%s", item.SkipReason)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(paths) == 0 {
		t.Fatal("定时检测一次上游请求都没发出（判据→发送器的接线断了）")
	}
}

// 嵌入渠道不该被拿去做定时探活：它在探针准入处就被拒，一次上游请求都不许发。
func TestScheduledAuditTargetSkipsEmbeddingChannel(t *testing.T) {
	setupScheduledAuditTestDB(t)

	var (
		mu   sync.Mutex
		hits int
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(upstream.Close)

	channel := model.Channel{
		ID:       2,
		Name:     "scheduled-audit-embedding",
		Type:     outbound.OutboundTypeOpenAIEmbedding,
		Enabled:  true,
		Model:    "embedding-example",
		BaseUrls: []model.BaseUrl{{URL: upstream.URL}},
		Keys:     []model.ChannelKey{{ID: 2, Enabled: true, ChannelKey: "scheduled-audit-key"}},
	}

	item := runScheduledAuditTarget(context.Background(), scheduledAuditTarget{
		channel: channel,
		model:   channel.Model,
		trigger: "schedule",
	})
	if !item.Skipped {
		t.Fatalf("嵌入渠道居然被拿去做定时探活：%+v", item)
	}
	if item.SkipReason == "" {
		t.Fatal("跳过却没有说明原因")
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 0 {
		t.Fatalf("跳过时必须一次上游请求都不发，实际发了 %d 次", hits)
	}
}

// setupScheduledAuditTestDB 起一个临时库：渠道检测链路会读设置缓存与渠道/密钥状态，
// 没有库就会在初始化处 panic，那会掩盖这条用例真正要断言的东西。
func setupScheduledAuditTestDB(t *testing.T) {
	t.Helper()
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "octopus.db"), false); err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
	})
	if err := op.InitCache(); err != nil {
		t.Fatalf("init cache: %v", err)
	}
}
