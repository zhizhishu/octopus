package op

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
)

func setupGroupAutoTest(t *testing.T) context.Context {
	t.Helper()

	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "octopus.db"), false); err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
	})

	ctx := context.Background()
	if err := channelRefreshCache(ctx); err != nil {
		t.Fatalf("refresh channel cache: %v", err)
	}
	if err := groupRefreshCache(ctx); err != nil {
		t.Fatalf("refresh group cache: %v", err)
	}
	if err := accessPlanRefreshCache(ctx); err != nil {
		t.Fatalf("refresh access plan cache: %v", err)
	}
	return ctx
}

func TestGroupEnsureChannelModelsCreatesPassthroughGroups(t *testing.T) {
	ctx := setupGroupAutoTest(t)

	channel := model.Channel{
		Name:    "openai",
		Enabled: true,
		Model:   "gpt-4o,gpt-4o-mini",
	}
	if err := ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	if err := GroupEnsureChannelModels(channel.ID, []string{"gpt-4o", "gpt-4o-mini", "gpt-4o", " "}, ctx); err != nil {
		t.Fatalf("ensure groups: %v", err)
	}
	if err := GroupEnsureChannelModels(channel.ID, []string{"gpt-4o"}, ctx); err != nil {
		t.Fatalf("ensure duplicate groups: %v", err)
	}

	group, err := GroupGetEnabledMap("gpt-4o", ctx)
	if err != nil {
		t.Fatalf("get auto group: %v", err)
	}
	if !group.AutoCreated {
		t.Fatalf("expected group to be marked auto-created")
	}
	if len(group.Items) != 1 {
		t.Fatalf("expected one group item, got %d", len(group.Items))
	}
	if group.Items[0].ChannelID != channel.ID || group.Items[0].ModelName != "gpt-4o" {
		t.Fatalf("unexpected group item: %+v", group.Items[0])
	}

	models, err := GroupListModel(ctx)
	if err != nil {
		t.Fatalf("list group models: %v", err)
	}
	if !containsString(models, "gpt-4o") || !containsString(models, "gpt-4o-mini") {
		t.Fatalf("expected model groups in /v1/models list, got %v", models)
	}

	if err := GroupItemBatchDelByChannelAndModels([]model.GroupIDAndLLMName{{
		ChannelID: channel.ID,
		ModelName: "gpt-4o",
	}}, ctx); err != nil {
		t.Fatalf("delete group item: %v", err)
	}
	if _, err := GroupGetEnabledMap("gpt-4o", ctx); err == nil {
		t.Fatalf("expected empty auto-created group to be removed")
	}
}

func TestGroupListModelHidesGroupsWithoutEnabledChannel(t *testing.T) {
	ctx := setupGroupAutoTest(t)

	disabledChannel := model.Channel{
		Name:    "disabled-channel",
		Enabled: true,
		Model:   "hidden-model",
	}
	if err := ChannelCreate(&disabledChannel, ctx); err != nil {
		t.Fatalf("create disabled channel: %v", err)
	}
	if err := ChannelEnabled(disabledChannel.ID, false, ctx); err != nil {
		t.Fatalf("disable channel: %v", err)
	}
	hiddenGroup := model.Group{
		Name: "hidden-model",
		Mode: model.GroupModeRoundRobin,
	}
	if err := GroupCreate(&hiddenGroup, ctx); err != nil {
		t.Fatalf("create hidden group: %v", err)
	}
	if err := GroupItemAdd(&model.GroupItem{
		GroupID:   hiddenGroup.ID,
		ChannelID: disabledChannel.ID,
		ModelName: "hidden-model",
	}, ctx); err != nil {
		t.Fatalf("add disabled group item: %v", err)
	}

	enabledChannel := model.Channel{
		Name:    "enabled-channel",
		Enabled: true,
		Model:   "visible-model",
	}
	if err := ChannelCreate(&enabledChannel, ctx); err != nil {
		t.Fatalf("create enabled channel: %v", err)
	}
	visibleGroup := model.Group{
		Name: "visible-model",
		Mode: model.GroupModeRoundRobin,
	}
	if err := GroupCreate(&visibleGroup, ctx); err != nil {
		t.Fatalf("create visible group: %v", err)
	}
	if err := GroupItemAdd(&model.GroupItem{
		GroupID:   visibleGroup.ID,
		ChannelID: enabledChannel.ID,
		ModelName: "visible-model",
	}, ctx); err != nil {
		t.Fatalf("add enabled group item: %v", err)
	}

	models, err := GroupListModel(ctx)
	if err != nil {
		t.Fatalf("list group models: %v", err)
	}
	if containsString(models, "hidden-model") {
		t.Fatalf("disabled-only group should be hidden, got %v", models)
	}
	if !containsString(models, "visible-model") {
		t.Fatalf("enabled group should be visible, got %v", models)
	}
}

func TestGroupEnsureChannelModelsPreservesManualGroup(t *testing.T) {
	ctx := setupGroupAutoTest(t)

	manual := model.Group{
		Name: "manual-model",
		Mode: model.GroupModeRoundRobin,
	}
	if err := GroupCreate(&manual, ctx); err != nil {
		t.Fatalf("create manual group: %v", err)
	}

	channel := model.Channel{
		Name:    "manual-channel",
		Enabled: true,
		Model:   "manual-model",
	}
	if err := ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	if err := GroupEnsureChannelModels(channel.ID, []string{"manual-model"}, ctx); err != nil {
		t.Fatalf("ensure group item: %v", err)
	}

	group, err := GroupGetEnabledMap("manual-model", ctx)
	if err != nil {
		t.Fatalf("get manual group: %v", err)
	}
	if group.AutoCreated {
		t.Fatalf("manual group should not be marked auto-created")
	}
	if len(group.Items) != 1 {
		t.Fatalf("expected one group item, got %d", len(group.Items))
	}

	if err := GroupItemBatchDelByChannelAndModels([]model.GroupIDAndLLMName{{
		ChannelID: channel.ID,
		ModelName: "manual-model",
	}}, ctx); err != nil {
		t.Fatalf("delete group item: %v", err)
	}
	group, err = GroupGetEnabledMap("manual-model", ctx)
	if err != nil {
		t.Fatalf("manual group should remain after item removal: %v", err)
	}
	if len(group.Items) != 0 {
		t.Fatalf("expected manual group to remain empty, got %d items", len(group.Items))
	}
}

func TestChannelUpdateRemovesEmptyAutoCreatedGroupsForRemovedModels(t *testing.T) {
	ctx := setupGroupAutoTest(t)

	channel := model.Channel{
		Name:    "sync-channel",
		Enabled: true,
		Model:   "model-a,model-b",
	}
	if err := ChannelCreate(&channel, ctx); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	if err := GroupEnsureChannelModels(channel.ID, []string{"model-a", "model-b"}, ctx); err != nil {
		t.Fatalf("ensure groups: %v", err)
	}

	nextModel := "model-b"
	if _, err := ChannelUpdate(&model.ChannelUpdateRequest{
		ID:    channel.ID,
		Model: &nextModel,
	}, ctx); err != nil {
		t.Fatalf("update channel: %v", err)
	}

	if _, err := GroupGetEnabledMap("model-a", ctx); err == nil {
		t.Fatalf("expected removed model group to be deleted")
	}
	if _, err := GroupGetEnabledMap("model-b", ctx); err != nil {
		t.Fatalf("expected remaining model group to exist: %v", err)
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
