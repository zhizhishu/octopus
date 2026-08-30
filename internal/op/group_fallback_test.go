package op

import (
	"testing"

	"github.com/bestruirui/octopus/internal/model"
)

func TestChannelServesModel(t *testing.T) {
	tests := []struct {
		name    string
		channel model.Channel
		model   string
		want    bool
	}{
		{
			name:    "selected model identity match",
			channel: model.Channel{SelectedModels: []string{"deepseek-v4-pro"}},
			model:   "deepseek-v4-pro",
			want:    true,
		},
		{
			name:    "selected model case-insensitive",
			channel: model.Channel{SelectedModels: []string{"DeepSeek-V4-Pro"}},
			model:   "deepseek-v4-pro",
			want:    true,
		},
		{
			name: "mapping alias with upstream selected",
			channel: model.Channel{
				SelectedModels: []string{"deepseek-ai/deepseek-v4-pro"},
				ModelMapping:   map[string]string{"deepseek-v4-pro": "deepseek-ai/deepseek-v4-pro"},
			},
			model: "deepseek-v4-pro",
			want:  true,
		},
		{
			name: "mapping alias whose upstream is not selected",
			channel: model.Channel{
				SelectedModels: []string{"deepseek-ai/other"},
				ModelMapping:   map[string]string{"deepseek-v4-pro": "deepseek-ai/deepseek-v4-pro"},
			},
			model: "deepseek-v4-pro",
			want:  false,
		},
		{
			name:    "model not served",
			channel: model.Channel{SelectedModels: []string{"deepseek-v4-pro"}},
			model:   "glm-5.2",
			want:    false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := channelServesModel(tc.channel, tc.model); got != tc.want {
				t.Fatalf("channelServesModel(%q) = %v, want %v", tc.model, got, tc.want)
			}
		})
	}
}

func TestGroupFallbackFromChannels(t *testing.T) {
	const clean = "deepseek-v4-pro"

	// Seed the in-package channel cache. The op package has no other tests, so a
	// little leakage from this table is moot; it also exercises the real cache the
	// fallback reads at runtime.
	channelCache.Set(1, model.Channel{ID: 1, Enabled: true, Priority: 5, SelectedModels: []string{"deepseek-v4-pro"}})
	channelCache.Set(2, model.Channel{ID: 2, Enabled: true, Priority: 0, SelectedModels: []string{"deepseek-v4-pro"}})
	channelCache.Set(3, model.Channel{ID: 3, Enabled: false, Priority: 1, SelectedModels: []string{"deepseek-v4-pro"}})
	channelCache.Set(4, model.Channel{
		ID:            4,
		Enabled:       true,
		Priority:      0,
		SelectedModels: []string{"deepseek-ai/deepseek-v4-pro"},
		ModelMapping:  map[string]string{"deepseek-v4-pro": "deepseek-ai/deepseek-v4-pro"},
	})

	group, err := groupFallbackFromChannels(clean)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if group.Mode != 0 {
		t.Errorf("fallback Mode = %v, want 0 (global default governs routing)", group.Mode)
	}

	// Priority asc then channel ID asc: 2 (prio 0) < 4 (prio 0) < 1 (prio 5).
	// Disabled channel 3 is excluded entirely.
	wantIDs := []int{2, 4, 1}
	if len(group.Items) != len(wantIDs) {
		t.Fatalf("len(items) = %d, want %d", len(group.Items), len(wantIDs))
	}
	for i, id := range wantIDs {
		if group.Items[i].ChannelID != id {
			t.Errorf("items[%d].ChannelID = %d, want %d", i, group.Items[i].ChannelID, id)
		}
		if group.Items[i].ModelName != clean {
			t.Errorf("items[%d].ModelName = %q, want %q (client alias kept for applyModelMapping)", i, group.Items[i].ModelName, clean)
		}
	}
}
