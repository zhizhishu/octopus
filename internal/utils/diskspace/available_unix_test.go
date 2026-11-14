//go:build !windows

package diskspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAvailableProbesExistingAncestorWithoutCreatingDirectories(t *testing.T) {
	dir := t.TempDir()
	if free, ok := Available(dir); !ok || free < 0 {
		t.Fatalf("filesystem measurement unavailable: %d %v", free, ok)
	}
	missing := filepath.Join(dir, "missing", "nested")
	if free, ok := Available(missing); !ok || free < 0 {
		t.Fatalf("existing ancestor not measured: %d %v", free, ok)
	}
	if _, err := os.Stat(filepath.Join(dir, "missing")); !os.IsNotExist(err) {
		t.Fatalf("probe created a directory: %v", err)
	}
}
