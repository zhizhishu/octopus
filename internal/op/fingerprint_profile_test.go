package op

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
)

func setupFingerprintProfileTest(t *testing.T) context.Context {
	t.Helper()

	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "octopus.db"), false); err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
	})
	return context.Background()
}

// An earlier build seeded a redundant all-empty "默认(Windows)" profile that
// duplicates the dropdown's ProfileID=0 option (so it showed THREE entries). The
// refresh must drop that exact auto-seed on upgrade and keep the real "Linux 真机".
func TestFingerprintProfileRefreshDropsRedundantDefault(t *testing.T) {
	ctx := setupFingerprintProfileTest(t)

	redundant := &model.FingerprintProfile{Name: "默认(Windows)", Seed: "stale-instance-seed"}
	if err := db.GetDB().WithContext(ctx).Create(redundant).Error; err != nil {
		t.Fatalf("seed redundant default: %v", err)
	}
	linux := &model.FingerprintProfile{
		Name:            "Linux 真机",
		Seed:            "linux-seed",
		ClaudeUserAgent: "claude-cli/2.1.186 (external, sdk-cli)",
		ClaudeOS:        "Linux",
	}
	if err := db.GetDB().WithContext(ctx).Create(linux).Error; err != nil {
		t.Fatalf("seed linux profile: %v", err)
	}

	if err := fingerprintProfileRefreshCache(ctx); err != nil {
		t.Fatalf("refresh fingerprint cache: %v", err)
	}

	var remaining []model.FingerprintProfile
	if err := db.GetDB().WithContext(ctx).Order("id").Find(&remaining).Error; err != nil {
		t.Fatalf("reload profiles: %v", err)
	}
	// After cleanup the redundant all-empty 默认(Windows) is dropped and "Linux 真机" is
	// kept; because the 2nd built-in ("Linux 真机 2 (Ubuntu)") is missing it is backfilled,
	// so exactly the two built-in Linux identities remain.
	names := make(map[string]bool, len(remaining))
	for _, p := range remaining {
		names[p.Name] = true
	}
	if names["默认(Windows)"] {
		t.Fatalf("redundant all-empty 默认(Windows) must be dropped, got %+v", remaining)
	}
	if len(remaining) != 2 || !names["Linux 真机"] || !names["Linux 真机 2 (Ubuntu)"] {
		t.Fatalf("expected 默认(Windows) dropped and both built-in Linux profiles present, got %d: %+v", len(remaining), remaining)
	}
}

// A user-customised profile that merely happens to be named "默认(Windows)" but has
// a real header field set must NOT be removed — only the all-empty auto-seed is.
func TestFingerprintProfileRefreshKeepsCustomizedProfileNamedDefault(t *testing.T) {
	ctx := setupFingerprintProfileTest(t)

	custom := &model.FingerprintProfile{Name: "默认(Windows)", Seed: "x", ClaudeOS: "Windows"}
	if err := db.GetDB().WithContext(ctx).Create(custom).Error; err != nil {
		t.Fatalf("seed customized default-named profile: %v", err)
	}

	if err := fingerprintProfileRefreshCache(ctx); err != nil {
		t.Fatalf("refresh fingerprint cache: %v", err)
	}

	var remaining []model.FingerprintProfile
	if err := db.GetDB().WithContext(ctx).Find(&remaining).Error; err != nil {
		t.Fatalf("reload profiles: %v", err)
	}
	if len(remaining) != 1 || remaining[0].Name != "默认(Windows)" || remaining[0].ClaudeOS != "Windows" {
		t.Fatalf("customised default-named profile must be preserved, got %+v", remaining)
	}
}
