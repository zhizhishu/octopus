package intervention

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/bestruirui/octopus/internal/db"
	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
)

func setupConfigTest(t *testing.T) {
	t.Helper()
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "octopus.db"), false); err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := op.InitCache(); err != nil {
		t.Fatalf("init cache: %v", err)
	}
}

func TestInterventionEnabledDefaultsOnForNewInstall(t *testing.T) {
	setupConfigTest(t)
	if !Enabled() {
		t.Fatal("new installs should default relay_intervention_enabled on")
	}
}

func TestNoBreakerRetryBudgetDefaultAndBounds(t *testing.T) {
	setupConfigTest(t)

	if got := NoBreakerRetryBudget(); got != 300*time.Second {
		t.Fatalf("default budget = %v, want 300s", got)
	}
	// A stored value above the ceiling (an older build's 600/900) must be clamped down to
	// 300 at read time, never honoured.
	for _, stored := range []string{"600", "900"} {
		if err := op.SettingSetString(dbmodel.SettingKeyRelayNoBreakerRetryBudgetSec, stored); err != nil {
			t.Fatalf("set budget %s: %v", stored, err)
		}
		if got := NoBreakerRetryBudget(); got != 300*time.Second {
			t.Fatalf("stored %s clamped budget = %v, want 300s", stored, got)
		}
	}
	if err := op.SettingSetString(dbmodel.SettingKeyRelayNoBreakerRetryBudgetSec, "120"); err != nil {
		t.Fatalf("set budget: %v", err)
	}
	if got := NoBreakerRetryBudget(); got != 120*time.Second {
		t.Fatalf("tightened budget = %v, want 120s", got)
	}
	if err := op.SettingSetString(dbmodel.SettingKeyRelayNoBreakerRetryBudgetSec, "0"); err != nil {
		t.Fatalf("disable budget: %v", err)
	}
	if got := NoBreakerRetryBudget(); got != 0 {
		t.Fatalf("disabled budget = %v, want 0", got)
	}
}
