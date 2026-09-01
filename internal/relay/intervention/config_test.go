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

func TestNoBreakerRetryBudgetDefaultAndBounds(t *testing.T) {
	setupConfigTest(t)

	if got := NoBreakerRetryBudget(); got != 300*time.Second {
		t.Fatalf("default budget = %v, want 300s", got)
	}
	if err := op.SettingSetString(dbmodel.SettingKeyRelayNoBreakerRetryBudgetSec, "900"); err != nil {
		t.Fatalf("set budget: %v", err)
	}
	if got := NoBreakerRetryBudget(); got != 600*time.Second {
		t.Fatalf("capped budget = %v, want 600s", got)
	}
	if err := op.SettingSetString(dbmodel.SettingKeyRelayNoBreakerRetryBudgetSec, "0"); err != nil {
		t.Fatalf("disable budget: %v", err)
	}
	if got := NoBreakerRetryBudget(); got != 0 {
		t.Fatalf("disabled budget = %v, want 0", got)
	}
}
