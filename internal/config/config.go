package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	// defaultScanInterval controls repository polling when omitted.
	defaultScanInterval = 2 * time.Second
	// defaultDebounce controls change settling when omitted.
	defaultDebounce = 2 * time.Second
	// defaultGitTimeout bounds each Git subprocess when omitted.
	defaultGitTimeout = 60 * time.Second
	// defaultSyncInterval controls scheduled synchronization when omitted.
	defaultSyncInterval = 5 * time.Minute
)

// Config is the fully validated runtime configuration.
type Config struct {
	ScanInterval  time.Duration
	Debounce      time.Duration
	GitTimeout    time.Duration
	LogLevel      string
	Notifications bool
	Repositories  []Repository
}

// Repository describes one Git worktree managed by the daemon.
type Repository struct {
	Path          string
	Extensions    []string
	IncludeHidden bool
	SyncInterval  time.Duration
}

// rawConfig mirrors the JSON document before validation.
type rawConfig struct {
	ScanInterval  string          `json:"scan_interval"`
	Debounce      string          `json:"debounce"`
	GitTimeout    string          `json:"git_timeout"`
	LogLevel      string          `json:"log_level"`
	Notifications *bool           `json:"notifications"`
	Repositories  []rawRepository `json:"repositories"`
}

// rawRepository mirrors one JSON repository entry.
type rawRepository struct {
	Path          string   `json:"path"`
	Extensions    []string `json:"extensions"`
	IncludeHidden bool     `json:"include_hidden"`
	SyncInterval  string   `json:"sync_interval"`
}

// DefaultPath returns the platform-specific user configuration path.
func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("find user config directory: %w", err)
	}
	return filepath.Join(dir, "notes-sync", "config.json"), nil
}

// Load reads and strictly validates a JSON configuration file.
func Load(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open config %q: %w", path, err)
	}
	defer f.Close()

	return Decode(f)
}

// Decode strictly decodes and validates a configuration.
func Decode(r io.Reader) (Config, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()

	var raw rawConfig
	if err := dec.Decode(&raw); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	if err := ensureEOF(dec); err != nil {
		return Config{}, err
	}

	cfg := Config{
		ScanInterval:  defaultScanInterval,
		Debounce:      defaultDebounce,
		GitTimeout:    defaultGitTimeout,
		LogLevel:      "info",
		Notifications: true,
	}
	if raw.Notifications != nil {
		cfg.Notifications = *raw.Notifications
	}

	var err error
	if cfg.ScanInterval, err = parseDuration("scan_interval", raw.ScanInterval, defaultScanInterval); err != nil {
		return Config{}, err
	}
	if cfg.Debounce, err = parseDuration("debounce", raw.Debounce, defaultDebounce); err != nil {
		return Config{}, err
	}
	if cfg.GitTimeout, err = parseDuration("git_timeout", raw.GitTimeout, defaultGitTimeout); err != nil {
		return Config{}, err
	}
	if raw.LogLevel != "" {
		cfg.LogLevel = strings.ToLower(raw.LogLevel)
	}
	if !slices.Contains([]string{"debug", "info", "warn", "error"}, cfg.LogLevel) {
		return Config{}, errors.New("log_level must be one of debug, info, warn, error")
	}
	if len(raw.Repositories) == 0 {
		return Config{}, errors.New("repositories must contain at least one repository")
	}

	for i, rawRepo := range raw.Repositories {
		repo, err := validateRepository(rawRepo)
		if err != nil {
			return Config{}, fmt.Errorf("repositories[%d]: %w", i, err)
		}
		cfg.Repositories = append(cfg.Repositories, repo)
	}
	if err := validateNoOverlaps(cfg.Repositories); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// ensureEOF rejects data after the configuration object.
func ensureEOF(dec *json.Decoder) error {
	var extra any
	err := dec.Decode(&extra)
	switch {
	case errors.Is(err, io.EOF):
		return nil
	case err != nil:
		return fmt.Errorf("decode trailing config data: %w", err)
	default:
		return errors.New("config contains more than one JSON value")
	}
}

// parseDuration parses a positive duration or returns its default.
func parseDuration(field, value string, fallback time.Duration) (time.Duration, error) {
	if value == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", field, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be positive", field)
	}
	return d, nil
}

// validateRepository normalizes one repository configuration.
func validateRepository(raw rawRepository) (Repository, error) {
	if raw.Path == "" {
		return Repository{}, errors.New("path is required")
	}
	if !filepath.IsAbs(raw.Path) {
		return Repository{}, errors.New("path must be absolute")
	}

	path, err := filepath.EvalSymlinks(filepath.Clean(raw.Path))
	if err != nil {
		return Repository{}, fmt.Errorf("resolve path: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return Repository{}, fmt.Errorf("stat path: %w", err)
	}
	if !info.IsDir() {
		return Repository{}, errors.New("path must be a directory")
	}

	extensions := raw.Extensions
	if len(extensions) == 0 {
		extensions = []string{".md", ".txt"}
	}
	normalized := make([]string, 0, len(extensions))
	seen := make(map[string]struct{}, len(extensions))
	for _, value := range extensions {
		ext, err := normalizeExtension(value)
		if err != nil {
			return Repository{}, err
		}
		if _, ok := seen[ext]; ok {
			return Repository{}, fmt.Errorf("duplicate extension %q", ext)
		}
		seen[ext] = struct{}{}
		normalized = append(normalized, ext)
	}
	slices.Sort(normalized)

	syncInterval, err := parseDuration("sync_interval", raw.SyncInterval, defaultSyncInterval)
	if err != nil {
		return Repository{}, err
	}

	return Repository{
		Path:          path,
		Extensions:    normalized,
		IncludeHidden: raw.IncludeHidden,
		SyncInterval:  syncInterval,
	}, nil
}

// normalizeExtension accepts .ext and *.ext forms.
func normalizeExtension(value string) (string, error) {
	ext := strings.TrimSpace(value)
	if strings.HasPrefix(ext, "*.") {
		ext = ext[1:]
	}
	if len(ext) < 2 || ext[0] != '.' {
		return "", fmt.Errorf("extension %q must look like .md or *.md", value)
	}
	if strings.ContainsAny(ext, `/\\*?[]`) || strings.Contains(ext[1:], ".") {
		return "", fmt.Errorf("extension %q is not a simple file extension", value)
	}
	return ext, nil
}

// validateNoOverlaps rejects duplicate and nested roots.
func validateNoOverlaps(repositories []Repository) error {
	for i := range repositories {
		for j := i + 1; j < len(repositories); j++ {
			left, err := os.Stat(repositories[i].Path)
			if err != nil {
				return fmt.Errorf("stat repository %q: %w", repositories[i].Path, err)
			}
			right, err := os.Stat(repositories[j].Path)
			if err != nil {
				return fmt.Errorf("stat repository %q: %w", repositories[j].Path, err)
			}
			if os.SameFile(left, right) {
				return fmt.Errorf("repository path is configured more than once: %q", repositories[i].Path)
			}
			if pathsOverlap(repositories[i].Path, repositories[j].Path) {
				return fmt.Errorf("repository paths overlap: %q and %q", repositories[i].Path, repositories[j].Path)
			}
		}
	}
	return nil
}

// pathsOverlap reports whether either path contains the other.
func pathsOverlap(a, b string) bool {
	rel, err := filepath.Rel(a, b)
	if err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))) {
		return true
	}
	rel, err = filepath.Rel(b, a)
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
}
