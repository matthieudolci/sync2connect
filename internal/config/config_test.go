package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoad(t *testing.T) {
	t.Setenv("TEST_WITHINGS_SECRET", "s3cret")
	path := writeConfig(t, `
state_dir: /tmp/sync-state
sync:
  interval: 1h30m
  initial_lookback: 48h
  routes:
    - source: withings
      destination: garmin
providers:
  withings:
    client_id: cid
    client_secret: ${TEST_WITHINGS_SECRET}
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StateDir != "/tmp/sync-state" {
		t.Errorf("StateDir = %q", cfg.StateDir)
	}
	if time.Duration(cfg.Sync.Interval) != 90*time.Minute {
		t.Errorf("Interval = %v", time.Duration(cfg.Sync.Interval))
	}
	if time.Duration(cfg.Sync.InitialLookback) != 48*time.Hour {
		t.Errorf("InitialLookback = %v", time.Duration(cfg.Sync.InitialLookback))
	}
	if got := cfg.ProviderSettings("withings")["client_secret"]; got != "s3cret" {
		t.Errorf("env expansion failed: client_secret = %q", got)
	}
	if got := cfg.ProviderSettings("garmin"); got == nil {
		t.Error("ProviderSettings for unconfigured provider must not be nil")
	}
}

func TestLoadDefaults(t *testing.T) {
	path := writeConfig(t, `
sync:
  routes:
    - source: withings
      destination: garmin
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StateDir != "data" {
		t.Errorf("default StateDir = %q", cfg.StateDir)
	}
	if time.Duration(cfg.Sync.Interval) != 0 {
		t.Errorf("default Interval = %v, want 0 (one-shot)", time.Duration(cfg.Sync.Interval))
	}
	if time.Duration(cfg.Sync.InitialLookback) != 30*24*time.Hour {
		t.Errorf("default InitialLookback = %v", time.Duration(cfg.Sync.InitialLookback))
	}
}

func TestLoadStateDirFromEnv(t *testing.T) {
	t.Setenv("SYNC2CONNECT_STATE_DIR", "/var/lib/sync2connect")
	path := writeConfig(t, `
sync:
  routes:
    - source: withings
      destination: garmin
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StateDir != "/var/lib/sync2connect" {
		t.Errorf("StateDir = %q", cfg.StateDir)
	}
}

func TestLoadValidation(t *testing.T) {
	cases := map[string]string{
		"no routes": `
sync:
  routes: []
`,
		"missing destination": `
sync:
  routes:
    - source: withings
`,
		"same source and destination": `
sync:
  routes:
    - source: garmin
      destination: garmin
`,
		"bad duration": `
sync:
  interval: tomorrow
  routes:
    - source: withings
      destination: garmin
`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, content)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil || !strings.Contains(err.Error(), "reading config") {
		t.Fatalf("expected read error, got %v", err)
	}
}
