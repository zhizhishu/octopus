package task

import "testing"

func TestReconcileSyncedChannelModelsPreservesManualSelection(t *testing.T) {
	retained, deleted, changed := reconcileSyncedChannelModels(
		[]string{"gpt-4.1", "claude-sonnet-4.5"},
		[]string{"gpt-4.1", "gpt-4o", "claude-sonnet-4.5", "claude-opus-4.7"},
	)

	if changed {
		t.Fatalf("expected unchanged manual selection")
	}
	if len(deleted) != 0 {
		t.Fatalf("expected no deleted models, got %v", deleted)
	}
	assertStringSlice(t, retained, []string{"gpt-4.1", "claude-sonnet-4.5"})
}

func TestReconcileSyncedChannelModelsKeepsEmptySelection(t *testing.T) {
	retained, deleted, changed := reconcileSyncedChannelModels(
		nil,
		[]string{"gpt-4.1", "gpt-4.1", "claude-sonnet-4.5"},
	)

	if changed {
		t.Fatalf("expected empty selection to remain unchanged")
	}
	if len(retained) != 0 || len(deleted) != 0 {
		t.Fatalf("expected no retained/deleted models, got retained=%v deleted=%v", retained, deleted)
	}
}

func TestReconcileSyncedChannelModelsRemovesMissingSelectionOnly(t *testing.T) {
	retained, deleted, changed := reconcileSyncedChannelModels(
		[]string{"gpt-4.1", "missing-model", "claude-sonnet-4.5"},
		[]string{"gpt-4.1", "gpt-4o", "claude-sonnet-4.5"},
	)

	if !changed {
		t.Fatalf("expected selection to change when a selected model disappears")
	}
	assertStringSlice(t, retained, []string{"gpt-4.1", "claude-sonnet-4.5"})
	assertStringSlice(t, deleted, []string{"missing-model"})
}

func TestReconcileSyncedChannelModelsKeepsFetchedCasing(t *testing.T) {
	retained, deleted, changed := reconcileSyncedChannelModels(
		[]string{"GPT-4O"},
		[]string{"gpt-4o", "gpt-4o-mini"},
	)

	if !changed {
		t.Fatalf("expected selection to change when fetched casing differs")
	}
	if len(deleted) != 0 {
		t.Fatalf("expected no deleted models, got %v", deleted)
	}
	assertStringSlice(t, retained, []string{"gpt-4o"})
}

func assertStringSlice(t *testing.T, got []string, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
