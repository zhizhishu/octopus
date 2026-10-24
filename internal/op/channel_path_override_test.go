package op

import (
	"testing"

	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/transformer/outbound"
)

// 契约第三节：三个路径逃生口（图片 / 视频 / 任务查询）必须真的写进 channels 表。
//
// 这三个字段走的是 ChannelUpdate 的 selectFields 白名单：漏一条不会报错，只会
// 静默不生效 —— 用户在表单里填了路径、保存成功、请求还是打到老地址，正是这类
// "存了个寂寞"的 bug。所以必须由用例盯着。
func TestChannelUpdatePersistsPathOverrides(t *testing.T) {
	ctx := setupChannelImportTest(t)

	channel := model.Channel{
		Name:     "path-override-channel",
		Type:     outbound.OutboundTypeOpenAIChat,
		Enabled:  true,
		BaseUrls: []model.BaseUrl{{URL: "https://upstream.example"}},
		Keys:     []model.ChannelKey{{ChannelKey: "sk-key-1", Enabled: true}},
	}
	if err := ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("ChannelCreate: %v", err)
	}
	// 新建的存量形态：三个字段都是空串 = 沿用标准路径。
	created, err := ChannelGet(channel.ID, ctx)
	if err != nil {
		t.Fatalf("ChannelGet: %v", err)
	}
	if created.OpenAIImagesPath != "" || created.OpenAIVideosPath != "" || created.OpenAITasksPath != "" {
		t.Fatalf("新建渠道的路径逃生口必须为空: images=%q videos=%q tasks=%q",
			created.OpenAIImagesPath, created.OpenAIVideosPath, created.OpenAITasksPath)
	}

	imagesPath, videosPath, tasksPath := "/openai/images", "/kling/v1/videos", "/kling/v1/tasks"
	updated, err := ChannelUpdate(&model.ChannelUpdateRequest{
		ID:               channel.ID,
		OpenAIImagesPath: &imagesPath,
		OpenAIVideosPath: &videosPath,
		OpenAITasksPath:  &tasksPath,
	}, ctx)
	if err != nil {
		t.Fatalf("ChannelUpdate: %v", err)
	}
	if updated.OpenAIImagesPath != imagesPath || updated.OpenAIVideosPath != videosPath || updated.OpenAITasksPath != tasksPath {
		t.Fatalf("更新后路径逃生口没落库: images=%q videos=%q tasks=%q",
			updated.OpenAIImagesPath, updated.OpenAIVideosPath, updated.OpenAITasksPath)
	}

	// 从库里重新读一遍（不信返回值，只信库）：走的是真实 UPDATE 而不是内存。
	reloaded, err := ChannelGet(channel.ID, ctx)
	if err != nil {
		t.Fatalf("ChannelGet after update: %v", err)
	}
	if reloaded.OpenAIImagesPath != imagesPath || reloaded.OpenAIVideosPath != videosPath || reloaded.OpenAITasksPath != tasksPath {
		t.Fatalf("库里读回的路径逃生口不对: images=%q videos=%q tasks=%q",
			reloaded.OpenAIImagesPath, reloaded.OpenAIVideosPath, reloaded.OpenAITasksPath)
	}

	// PATCH 里没提到这三个字段时，不得把已存的值清掉。
	renamed := "path-override-channel-renamed"
	updated, err = ChannelUpdate(&model.ChannelUpdateRequest{
		ID:   channel.ID,
		Name: &renamed,
	}, ctx)
	if err != nil {
		t.Fatalf("ChannelUpdate (rename only): %v", err)
	}
	if updated.OpenAIImagesPath != imagesPath || updated.OpenAIVideosPath != videosPath || updated.OpenAITasksPath != tasksPath {
		t.Fatalf("没提到路径字段的 PATCH 不得清掉已存的值: images=%q videos=%q tasks=%q",
			updated.OpenAIImagesPath, updated.OpenAIVideosPath, updated.OpenAITasksPath)
	}

	// 显式清空 = 回到标准路径（与 OpenAIChatPath 同款语义）。
	empty := ""
	cleared, err := ChannelUpdate(&model.ChannelUpdateRequest{
		ID:               channel.ID,
		OpenAIImagesPath: &empty,
		OpenAIVideosPath: &empty,
		OpenAITasksPath:  &empty,
	}, ctx)
	if err != nil {
		t.Fatalf("ChannelUpdate (clear): %v", err)
	}
	if cleared.OpenAIImagesPath != "" || cleared.OpenAIVideosPath != "" || cleared.OpenAITasksPath != "" {
		t.Fatalf("显式清空后仍留有值: images=%q videos=%q tasks=%q",
			cleared.OpenAIImagesPath, cleared.OpenAIVideosPath, cleared.OpenAITasksPath)
	}
}
