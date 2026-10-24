package migrate

import (
	"testing"

	"github.com/zhizhishu/octopus/internal/model"
)

// 019 加三列：图片/视频/任务查询的路径逃生口。
//
// 断言的是**数据库列名**而不是字段名：op.ChannelUpdate 的 selectFields 直接写这三个
// 列名（"open_ai_images_path" / "open_ai_videos_path" / "open_ai_tasks_path"），列名
// 一旦被 GORM 的命名策略改掉，更新就静默失效——只有按列名断言才抓得住。
func TestMigration019AddsChannelPathOverrideColumns(t *testing.T) {
	gdb := new016RawTestDB(t)

	// 复用 016 的 legacy 表定义（capabilities + 三个路径列都没有）。
	if err := gdb.AutoMigrate(&legacyChannelWithoutCapabilities{}); err != nil {
		t.Fatalf("auto-migrate legacy channels: %v", err)
	}
	// type 4 是退役的渠道类型：迁移不许替用户改这一行，它读出来必须还是 4。
	if err := gdb.Create(&legacyChannelWithoutCapabilities{ID: 11, Name: "retired-type-4", Type: 4}).Error; err != nil {
		t.Fatalf("seed legacy channel: %v", err)
	}
	legacy := channelsColumnNames(t, gdb)
	for _, name := range []string{"open_ai_images_path", "open_ai_videos_path", "open_ai_tasks_path"} {
		if legacy[name] {
			t.Fatalf("precondition failed: channels.%s already exists", name)
		}
	}

	if err := migrateAddChannelPathOverrideColumns(gdb); err != nil {
		t.Fatalf("019: %v", err)
	}
	after := channelsColumnNames(t, gdb)
	for _, name := range []string{"open_ai_images_path", "open_ai_videos_path", "open_ai_tasks_path"} {
		if !after[name] {
			t.Fatalf("019 did not add channels.%s (columns: %v)", name, after)
		}
	}

	// 存量行读出来必须是空值，且渠道类型原封不动：
	// 空 = 沿用标准路径，行为与改动前逐字节一致；类型不许被"顺便整理"。
	var stored model.Channel
	if err := gdb.Where("id = ?", 11).First(&stored).Error; err != nil {
		t.Fatalf("read legacy channel: %v", err)
	}
	if int(stored.Type) != 4 {
		t.Fatalf("019 rewrote channels.type: got %d, want 4 (a retired type must be left for the operator to fix)", int(stored.Type))
	}
	if stored.OpenAIImagesPath != "" || stored.OpenAIVideosPath != "" || stored.OpenAITasksPath != "" {
		t.Fatalf("legacy channel gained path overrides: images=%q videos=%q tasks=%q, want all empty",
			stored.OpenAIImagesPath, stored.OpenAIVideosPath, stored.OpenAITasksPath)
	}

	// 幂等：重复跑不报错、结果不变。
	if err := migrateAddChannelPathOverrideColumns(gdb); err != nil {
		t.Fatalf("019 second run: %v", err)
	}
}

func TestMigration019NilDB(t *testing.T) {
	if err := migrateAddChannelPathOverrideColumns(nil); err == nil {
		t.Fatalf("019 must reject a nil db")
	}
}
