package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"
)

func TestDefaultConfigAndLoadWithoutRCFile(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load without .ggctxrc failed: %v", err)
	}

	wantWorkers := runtime.NumCPU()
	if wantWorkers < 1 {
		wantWorkers = 1
	}
	if cfg.Workers != wantWorkers {
		t.Fatalf("Workers = %d, want %d", cfg.Workers, wantWorkers)
	}
	if cfg.TokenBudget != DefaultTokenBudget {
		t.Fatalf("TokenBudget = %d, want %d", cfg.TokenBudget, DefaultTokenBudget)
	}
	if cfg.DebounceDuration() != 100*time.Millisecond {
		t.Fatalf("DebounceDuration() = %v, want 100ms", cfg.DebounceDuration())
	}
	if !cfg.CompressSnapshots || cfg.SyncWrites || !cfg.UseGitignore || cfg.FollowSymlinks {
		t.Fatalf("unexpected boolean defaults: %+v", cfg)
	}
	if cfg.DataDir != ".ggctx" || cfg.IgnoreFile != ".ggctxignore" {
		t.Fatalf("unexpected path defaults: %+v", cfg)
	}
	if !slices.Equal(cfg.Languages, DefaultLanguages) {
		t.Fatalf("Languages = %v, want %v", cfg.Languages, DefaultLanguages)
	}
}

func TestLoadJSONAndKeyValueRCWithEnvPrecedence(t *testing.T) {
	// 1. JSON .ggctxrc
	jsonDir := t.TempDir()
	jsonRC := `{
		"workers": 6,
		"token_budget": 8000,
		"debounce_ms": 250,
		"compress_snapshots": false,
		"sync_writes": true,
		"wal_compact_threshold_bytes": 1048576,
		"data_dir": ".custom_ggctx",
		"ignore_file": ".customignore",
		"use_gitignore": false,
		"follow_symlinks": true,
		"max_call_chain_depth": 5,
		"max_search_results": 50,
		"languages": ["Go", "Python"]
	}`
	if err := os.WriteFile(filepath.Join(jsonDir, DefaultRCFileName), []byte(jsonRC), 0o644); err != nil {
		t.Fatal(err)
	}

	// Env overrides specific fields on top of .ggctxrc
	t.Setenv("GGCTX_WORKERS", "12")
	t.Setenv("GGCTX_COMPRESS_SNAPSHOTS", "true")
	t.Setenv("GGCTX_LANGUAGES", "rust, java")

	cfg, err := Load(jsonDir)
	if err != nil {
		t.Fatalf("Load JSON .ggctxrc failed: %v", err)
	}
	if cfg.Workers != 12 {
		t.Fatalf("env GGCTX_WORKERS did not override JSON rc: got %d", cfg.Workers)
	}
	if cfg.TokenBudget != 8000 || cfg.DebounceMs != 250 || !cfg.SyncWrites || cfg.UseGitignore || !cfg.FollowSymlinks {
		t.Fatalf("JSON rc values not applied: %+v", cfg)
	}
	if !cfg.CompressSnapshots {
		t.Fatal("env GGCTX_COMPRESS_SNAPSHOTS did not override JSON rc")
	}
	if cfg.DataDir != ".custom_ggctx" || cfg.IgnoreFile != ".customignore" || cfg.WALCompactThresholdBytes != 1048576 {
		t.Fatalf("JSON path/threshold mismatch: %+v", cfg)
	}
	if !slices.Equal(cfg.Languages, []string{"rust", "java"}) {
		t.Fatalf("env GGCTX_LANGUAGES mismatch: %v", cfg.Languages)
	}

	// 2. Key = value .ggctxrc with inline comments and quotes
	kvDir := t.TempDir()
	kvRC := `
# GoGraphCtx settings
workers = 4 # worker threads
token_budget = 6000 // MCP token limit
debounce_ms = 0
data_dir = ".kv#data" # keeps '#' inside quotes
languages = "go, typescript"
`
	kvPath := filepath.Join(kvDir, DefaultRCFileName)
	if err := os.WriteFile(kvPath, []byte(kvRC), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GGCTX_WORKERS", "9")
	cfgKV, err := LoadFromFile(kvPath)
	if err != nil {
		t.Fatalf("LoadFromFile key=value failed: %v", err)
	}
	if cfgKV.Workers != 9 || cfgKV.TokenBudget != 6000 || cfgKV.DebounceDuration() != 0 || cfgKV.DataDir != ".kv#data" {
		t.Fatalf("key=value rc mismatch: %+v", cfgKV)
	}
}

func TestConfigValidationAndMalformedErrors(t *testing.T) {
	dir := t.TempDir()

	// Empty .ggctxrc file succeeds with defaults
	emptyRC := filepath.Join(dir, DefaultRCFileName)
	if err := os.WriteFile(emptyRC, []byte("   \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFromFile(emptyRC); err != nil {
		t.Fatalf("empty .ggctxrc should succeed: %v", err)
	}

	// Unknown JSON field & trailing JSON content rejected cleanly
	badJSON := filepath.Join(dir, "bad.json.rc")
	if err := os.WriteFile(badJSON, []byte(`{"unknown_field": 1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFromFile(badJSON); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig for unknown JSON field, got %v", err)
	}
	trailingJSON := filepath.Join(dir, "trailing.json.rc")
	if err := os.WriteFile(trailingJSON, []byte(`{"workers": 2}{"workers": 4}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFromFile(trailingJSON); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig for trailing JSON object, got %v", err)
	}
	garbageJSON := filepath.Join(dir, "garbage.json.rc")
	if err := os.WriteFile(garbageJSON, []byte(`{"workers": 2} trailing`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFromFile(garbageJSON); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig for trailing JSON garbage, got %v", err)
	}

	// Unknown key=value key or syntax error rejected
	badKV := filepath.Join(dir, "bad.kv.rc")
	if err := os.WriteFile(badKV, []byte("not_a_pair\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFromFile(badKV); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig for malformed kv line, got %v", err)
	}

	// Missing path for LoadFromFile
	if _, err := LoadFromFile(""); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig for empty path, got %v", err)
	}
	if _, err := LoadFromFile(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("expected error for missing file in LoadFromFile")
	}

	// Invalid env var value
	t.Run("bad env", func(t *testing.T) {
		t.Setenv("GGCTX_WORKERS", "abc")
		if _, err := Load(""); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("expected ErrInvalidConfig for non-int GGCTX_WORKERS, got %v", err)
		}
	})

	// Validation edge bounds
	invalidConfigs := []struct {
		name string
		mod  func(*Config)
	}{
		{"workers zero", func(c *Config) { c.Workers = 0 }},
		{"token budget too small", func(c *Config) { c.TokenBudget = 50 }},
		{"negative debounce", func(c *Config) { c.DebounceMs = -1 }},
		{"negative wal threshold", func(c *Config) { c.WALCompactThresholdBytes = -1 }},
		{"empty data dir", func(c *Config) { c.DataDir = "  " }},
		{"empty ignore file", func(c *Config) { c.IgnoreFile = "" }},
		{"depth zero", func(c *Config) { c.MaxCallChainDepth = 0 }},
		{"search limit zero", func(c *Config) { c.MaxSearchResults = 0 }},
		{"empty languages", func(c *Config) { c.Languages = nil }},
		{"blank language entry", func(c *Config) { c.Languages = []string{"go", "  "} }},
	}
	for _, tc := range invalidConfigs {
		c := DefaultConfig()
		tc.mod(&c)
		if err := c.Validate(); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("%s: Validate() = %v, want ErrInvalidConfig", tc.name, err)
		}
	}
}
