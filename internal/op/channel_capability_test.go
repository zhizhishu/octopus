package op

import (
	"testing"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// 契约第一节：渠道创建/更新的 handler 必须真的把能力位与嵌入自定义路径写进
// channels 表。这两个字段走的是 ChannelUpdate 的 selectFields 白名单，漏了不会报错，
// 只会静默不生效 —— 所以必须由用例盯着。
func TestChannelCreateAndUpdatePersistCapabilities(t *testing.T) {
	ctx := setupChannelImportTest(t)

	channel := model.Channel{
		Name:                 "volc-capability",
		Type:                 outbound.OutboundTypeVolcengine,
		Enabled:              true,
		BaseUrls:             []model.BaseUrl{{URL: "https://ark.example.com/api/v3"}},
		Keys:                 []model.ChannelKey{{ChannelKey: "sk-key-1", Enabled: true}},
		Capabilities:         []string{model.CapabilityEmbeddings},
		OpenAIEmbeddingsPath: "/embeddings",
	}
	if err := ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("ChannelCreate: %v", err)
	}

	created, err := ChannelGet(channel.ID, ctx)
	if err != nil {
		t.Fatalf("ChannelGet: %v", err)
	}
	if len(created.Capabilities) != 1 || created.Capabilities[0] != model.CapabilityEmbeddings {
		t.Fatalf("创建后 capabilities = %v, want [embeddings]", created.Capabilities)
	}
	if created.OpenAIEmbeddingsPath != "/embeddings" {
		t.Fatalf("创建后 openai_embeddings_path = %q, want /embeddings", created.OpenAIEmbeddingsPath)
	}
	if !created.SupportsCapability(model.CapabilityEmbeddings) {
		t.Fatalf("创建后渠道必须能承接嵌入流量")
	}

	// 更新：显式清空能力位（[ ] = 回到按渠道类型判定的存量语义）。
	empty := []string{}
	updated, err := ChannelUpdate(&model.ChannelUpdateRequest{
		ID:           channel.ID,
		Capabilities: &empty,
	}, ctx)
	if err != nil {
		t.Fatalf("ChannelUpdate: %v", err)
	}
	if len(updated.Capabilities) != 0 {
		t.Fatalf("更新后 capabilities = %v, want empty", updated.Capabilities)
	}
	if updated.SupportsCapability(model.CapabilityEmbeddings) {
		t.Fatalf("清空能力位后，火山渠道必须回到「不能承接嵌入」")
	}

	// 更新：重新勾上 + 改嵌入路径。
	enabled := []string{model.CapabilityEmbeddings}
	path := "/api/v3/embeddings"
	updated, err = ChannelUpdate(&model.ChannelUpdateRequest{
		ID:                   channel.ID,
		Capabilities:         &enabled,
		OpenAIEmbeddingsPath: &path,
	}, ctx)
	if err != nil {
		t.Fatalf("ChannelUpdate: %v", err)
	}
	if len(updated.Capabilities) != 1 || updated.Capabilities[0] != model.CapabilityEmbeddings {
		t.Fatalf("更新后 capabilities = %v, want [embeddings]", updated.Capabilities)
	}
	if updated.OpenAIEmbeddingsPath != "/api/v3/embeddings" {
		t.Fatalf("更新后 openai_embeddings_path = %q, want /api/v3/embeddings", updated.OpenAIEmbeddingsPath)
	}

	// 存量请求形状：没提到能力位（nil）时不得动库里已有的值。
	storeFrontPath := "/embeddings"
	updated, err = ChannelUpdate(&model.ChannelUpdateRequest{
		ID:                   channel.ID,
		OpenAIEmbeddingsPath: &storeFrontPath,
	}, ctx)
	if err != nil {
		t.Fatalf("ChannelUpdate: %v", err)
	}
	if len(updated.Capabilities) != 1 || updated.Capabilities[0] != model.CapabilityEmbeddings {
		t.Fatalf("没提到 capabilities 的 PATCH 不得改动已存的能力位, got %v", updated.Capabilities)
	}
}
