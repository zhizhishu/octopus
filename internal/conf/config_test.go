package conf

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/viper"
)

func TestLoadEnvironmentOverridesConfigFile(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	config := []byte(`{
  "server": {
    "host": "0.0.0.0",
    "port": "3051"
  },
  "database": {
    "type": "sqlite",
    "path": "data/data.db"
  },
  "log": {
    "level": "info"
  }
}`)
	if err := os.WriteFile(configPath, config, 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	t.Setenv("OCTOPUS_SERVER_PORT", "5050")
	t.Setenv("OCTOPUS_DATABASE_PATH", "build/runtime-check-data/data.db")
	t.Setenv("OCTOPUS_LOG_LEVEL", "debug")

	if err := Load(configPath); err != nil {
		t.Fatalf("load config: %v", err)
	}

	if AppConfig.Server.Port != 5050 {
		t.Fatalf("expected env server port 5050, got %d", AppConfig.Server.Port)
	}
	if AppConfig.Database.Path != "build/runtime-check-data/data.db" {
		t.Fatalf("expected env database path override, got %q", AppConfig.Database.Path)
	}
	if AppConfig.Log.Level != "debug" {
		t.Fatalf("expected env log level debug, got %q", AppConfig.Log.Level)
	}
}

func TestLoadCoercesStringPortFromConfig(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	config := []byte(`{
  "server": {
    "host": "127.0.0.1",
    "port": "3051"
  },
  "database": {
    "type": "sqlite",
    "path": "data/data.db"
  },
  "log": {
    "level": "info"
  }
}`)
	if err := os.WriteFile(configPath, config, 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if err := Load(configPath); err != nil {
		t.Fatalf("load config: %v", err)
	}

	if AppConfig.Server.Port != 3051 {
		t.Fatalf("expected config string port 3051, got %d", AppConfig.Server.Port)
	}
}
