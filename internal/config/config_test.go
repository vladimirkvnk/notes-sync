package config

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestDecodeDefaultsAndNormalization verifies defaults and normalization.
func TestDecodeDefaultsAndNormalization(t *testing.T) {
	dir := t.TempDir()
	input := fmt.Sprintf(`{
		"repositories": [{"path": %q, "extensions": ["*.txt", ".md"]}]
	}`, dir)

	cfg, err := Decode(strings.NewReader(input))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if cfg.ScanInterval != 2*time.Second || cfg.Debounce != 2*time.Second {
		t.Fatalf("unexpected scan defaults: scan=%v debounce=%v", cfg.ScanInterval, cfg.Debounce)
	}
	if cfg.GitTimeout != time.Minute || cfg.Repositories[0].SyncInterval != 5*time.Minute {
		t.Fatalf("unexpected Git defaults: timeout=%v sync=%v", cfg.GitTimeout, cfg.Repositories[0].SyncInterval)
	}
	if !cfg.Notifications || cfg.LogLevel != "info" {
		t.Fatalf("unexpected operational defaults: notifications=%v log=%q", cfg.Notifications, cfg.LogLevel)
	}
	if got, want := cfg.Repositories[0].Extensions, []string{".md", ".txt"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("extensions = %v, want %v", got, want)
	}
}

// TestDecodeOverrides verifies explicit configuration values.
func TestDecodeOverrides(t *testing.T) {
	dir := t.TempDir()
	input := fmt.Sprintf(`{
		"scan_interval": "750ms",
		"debounce": "3s",
		"git_timeout": "15s",
		"log_level": "WARN",
		"notifications": false,
		"repositories": [{
			"path": %q,
			"include_hidden": true,
			"sync_interval": "30s"
		}]
	}`, dir)

	cfg, err := Decode(strings.NewReader(input))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if cfg.ScanInterval != 750*time.Millisecond || cfg.Debounce != 3*time.Second || cfg.GitTimeout != 15*time.Second {
		t.Fatalf("unexpected durations: %+v", cfg)
	}
	if cfg.Notifications || cfg.LogLevel != "warn" || !cfg.Repositories[0].IncludeHidden {
		t.Fatalf("unexpected overrides: %+v", cfg)
	}
}

// TestDecodeRejectsInvalidInput verifies strict JSON validation.
func TestDecodeRejectsInvalidInput(t *testing.T) {
	dir := t.TempDir()
	other := t.TempDir()
	tests := map[string]string{
		"unknown field":                          fmt.Sprintf(`{"unknown": true, "repositories": [{"path": %q}]}`, dir),
		"trailing value":                         fmt.Sprintf(`{"repositories": [{"path": %q}]} {}`, dir),
		"no repositories":                        `{}`,
		"relative path":                          `{"repositories": [{"path": "notes"}]}`,
		"bad duration":                           fmt.Sprintf(`{"scan_interval": "soon", "repositories": [{"path": %q}]}`, dir),
		"zero duration":                          fmt.Sprintf(`{"debounce": "0s", "repositories": [{"path": %q}]}`, dir),
		"bad log level":                          fmt.Sprintf(`{"log_level": "trace", "repositories": [{"path": %q}]}`, dir),
		"bad extension":                          fmt.Sprintf(`{"repositories": [{"path": %q, "extensions": ["**/*.md"]}]}`, dir),
		"duplicate extension":                    fmt.Sprintf(`{"repositories": [{"path": %q, "extensions": [".md", "*.md"]}]}`, dir),
		"multiple independent valid JSON values": fmt.Sprintf(`{"repositories": [{"path": %q}]} true`, other),
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(strings.NewReader(input)); err == nil {
				t.Fatal("Decode() unexpectedly succeeded")
			}
		})
	}
}

// TestDecodeRejectsOverlappingRepositories verifies nested root rejection.
func TestDecodeRejectsOverlappingRepositories(t *testing.T) {
	parent := t.TempDir()
	child := parent + "/child"
	if err := os.Mkdir(child, 0o755); err != nil {
		t.Fatal(err)
	}
	input := fmt.Sprintf(`{"repositories": [{"path": %q}, {"path": %q}]}`, parent, child)
	if _, err := Decode(strings.NewReader(input)); err == nil {
		t.Fatal("Decode() unexpectedly accepted overlapping repositories")
	}
}
