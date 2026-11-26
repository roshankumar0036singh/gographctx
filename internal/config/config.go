package config

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultRCFileName is the repository config filename.
	DefaultRCFileName = ".ggctxrc"
	// DefaultIgnoreFileName is the repository ignore filename.
	DefaultIgnoreFileName = ".ggctxignore"
	// DefaultDataDirName is the default storage directory inside a repository.
	DefaultDataDirName = ".ggctx"

	DefaultTokenBudget      = 4000
	DefaultDebounceMs       = 100
	DefaultWALCompactBytes  = int64(4 << 20)
	DefaultMaxCallChain     = 10
	DefaultMaxSearchResults = 100
)

// ErrInvalidConfig indicates invalid syntax or out-of-range configuration values.
var ErrInvalidConfig = errors.New("config: invalid configuration")

// DefaultLanguages lists the six core v1 languages supported by GoGraphCtx.
var DefaultLanguages = []string{"go", "python", "typescript", "javascript", "rust", "java"}

// Config holds runtime settings for indexing, persistence, file watching, and MCP output.
type Config struct {
	Workers                  int      `json:"workers"`
	TokenBudget              int      `json:"token_budget"`
	DebounceMs               int      `json:"debounce_ms"`
	CompressSnapshots        bool     `json:"compress_snapshots"`
	SyncWrites               bool     `json:"sync_writes"`
	WALCompactThresholdBytes int64    `json:"wal_compact_threshold_bytes"`
	DataDir                  string   `json:"data_dir"`
	IgnoreFile               string   `json:"ignore_file"`
	UseGitignore             bool     `json:"use_gitignore"`
	FollowSymlinks           bool     `json:"follow_symlinks"`
	MaxCallChainDepth        int      `json:"max_call_chain_depth"`
	MaxSearchResults         int      `json:"max_search_results"`
	Languages                []string `json:"languages"`
}

// DefaultConfig returns sensible zero-config defaults.
func DefaultConfig() Config {
	workers := runtime.NumCPU()
	if workers < 1 {
		workers = 1
	}
	langs := make([]string, len(DefaultLanguages))
	copy(langs, DefaultLanguages)

	return Config{
		Workers:                  workers,
		TokenBudget:              DefaultTokenBudget,
		DebounceMs:               DefaultDebounceMs,
		CompressSnapshots:        true,
		SyncWrites:               false,
		WALCompactThresholdBytes: DefaultWALCompactBytes,
		DataDir:                  DefaultDataDirName,
		IgnoreFile:               DefaultIgnoreFileName,
		UseGitignore:             true,
		FollowSymlinks:           false,
		MaxCallChainDepth:        DefaultMaxCallChain,
		MaxSearchResults:         DefaultMaxSearchResults,
		Languages:                langs,
	}
}

// DebounceDuration returns the configured file-watcher debounce interval.
func (c Config) DebounceDuration() time.Duration {
	if c.DebounceMs <= 0 {
		return 0
	}
	return time.Duration(c.DebounceMs) * time.Millisecond
}

// Validate checks configuration invariants and normalizes language names.
func (c *Config) Validate() error {
	if c.Workers < 1 {
		return fmt.Errorf("%w: workers must be >= 1, got %d", ErrInvalidConfig, c.Workers)
	}
	if c.TokenBudget < 100 {
		return fmt.Errorf("%w: token_budget must be >= 100, got %d", ErrInvalidConfig, c.TokenBudget)
	}
	if c.DebounceMs < 0 {
		return fmt.Errorf("%w: debounce_ms must be >= 0, got %d", ErrInvalidConfig, c.DebounceMs)
	}
	if c.WALCompactThresholdBytes < 0 {
		return fmt.Errorf("%w: wal_compact_threshold_bytes must be >= 0, got %d", ErrInvalidConfig, c.WALCompactThresholdBytes)
	}
	if strings.TrimSpace(c.DataDir) == "" {
		return fmt.Errorf("%w: data_dir must not be empty", ErrInvalidConfig)
	}
	if strings.TrimSpace(c.IgnoreFile) == "" {
		return fmt.Errorf("%w: ignore_file must not be empty", ErrInvalidConfig)
	}
	if c.MaxCallChainDepth < 1 {
		return fmt.Errorf("%w: max_call_chain_depth must be >= 1, got %d", ErrInvalidConfig, c.MaxCallChainDepth)
	}
	if c.MaxSearchResults < 1 {
		return fmt.Errorf("%w: max_search_results must be >= 1, got %d", ErrInvalidConfig, c.MaxSearchResults)
	}
	if len(c.Languages) == 0 {
		return fmt.Errorf("%w: languages list must not be empty", ErrInvalidConfig)
	}
	cleaned := make([]string, 0, len(c.Languages))
	for _, lang := range c.Languages {
		l := strings.ToLower(strings.TrimSpace(lang))
		if l == "" {
			return fmt.Errorf("%w: empty language entry in languages", ErrInvalidConfig)
		}
		cleaned = append(cleaned, l)
	}
	c.Languages = cleaned
	return nil
}

// Load loads configuration for repoRoot with precedence:
// DefaultConfig -> <repoRoot>/.ggctxrc (if present) -> GGCTX_* environment variables.
func Load(repoRoot string) (Config, error) {
	cfg := DefaultConfig()

	if strings.TrimSpace(repoRoot) != "" {
		rcPath := filepath.Join(repoRoot, DefaultRCFileName)
		if _, err := os.Stat(rcPath); err == nil {
			if err := mergeFileInto(&cfg, rcPath); err != nil {
				return Config{}, err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return Config{}, fmt.Errorf("stat config %q: %w", rcPath, err)
		}
	}

	if err := applyEnvOverrides(&cfg); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// LoadFromFile loads defaults overlaid with the specified .ggctxrc path and GGCTX_* env overrides.
func LoadFromFile(path string) (Config, error) {
	cfg := DefaultConfig()
	if strings.TrimSpace(path) == "" {
		return Config{}, fmt.Errorf("%w: empty config file path", ErrInvalidConfig)
	}
	if err := mergeFileInto(&cfg, path); err != nil {
		return Config{}, err
	}
	if err := applyEnvOverrides(&cfg); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func mergeFileInto(cfg *Config, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config %q: %w", path, err)
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil
	}
	if trimmed[0] == '{' {
		dec := json.NewDecoder(bytes.NewReader(trimmed))
		dec.DisallowUnknownFields()
		if err := dec.Decode(cfg); err != nil {
			return fmt.Errorf("%w: parse JSON config %q: %w", ErrInvalidConfig, path, err)
		}
		return nil
	}
	return parseKeyValueConfig(cfg, path, trimmed)
}

func parseKeyValueConfig(cfg *Config, path string, data []byte) error {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			return fmt.Errorf("%w: %s:%d expected key = value, got %q", ErrInvalidConfig, path, lineNum, line)
		}
		key := strings.ToLower(strings.TrimSpace(line[:eq]))
		val := strings.TrimSpace(line[eq+1:])
		val = trimMatchingQuotes(val)

		if err := setConfigKey(cfg, key, val); err != nil {
			return fmt.Errorf("%w: %s:%d %w", ErrInvalidConfig, path, lineNum, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan config %q: %w", path, err)
	}
	return nil
}

func trimMatchingQuotes(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

func setConfigKey(cfg *Config, key, val string) error {
	switch key {
	case "workers":
		n, err := strconv.Atoi(val)
		if err != nil {
			return fmt.Errorf("invalid workers %q: %w", val, err)
		}
		cfg.Workers = n
	case "token_budget":
		n, err := strconv.Atoi(val)
		if err != nil {
			return fmt.Errorf("invalid token_budget %q: %w", val, err)
		}
		cfg.TokenBudget = n
	case "debounce_ms":
		n, err := strconv.Atoi(val)
		if err != nil {
			return fmt.Errorf("invalid debounce_ms %q: %w", val, err)
		}
		cfg.DebounceMs = n
	case "compress_snapshots":
		b, err := strconv.ParseBool(val)
		if err != nil {
			return fmt.Errorf("invalid compress_snapshots %q: %w", val, err)
		}
		cfg.CompressSnapshots = b
	case "sync_writes":
		b, err := strconv.ParseBool(val)
		if err != nil {
			return fmt.Errorf("invalid sync_writes %q: %w", val, err)
		}
		cfg.SyncWrites = b
	case "wal_compact_threshold_bytes", "wal_compact_bytes":
		n, err := strconv.ParseInt(val, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid %s %q: %w", key, val, err)
		}
		cfg.WALCompactThresholdBytes = n
	case "data_dir":
		cfg.DataDir = val
	case "ignore_file":
		cfg.IgnoreFile = val
	case "use_gitignore":
		b, err := strconv.ParseBool(val)
		if err != nil {
			return fmt.Errorf("invalid use_gitignore %q: %w", val, err)
		}
		cfg.UseGitignore = b
	case "follow_symlinks":
		b, err := strconv.ParseBool(val)
		if err != nil {
			return fmt.Errorf("invalid follow_symlinks %q: %w", val, err)
		}
		cfg.FollowSymlinks = b
	case "max_call_chain_depth":
		n, err := strconv.Atoi(val)
		if err != nil {
			return fmt.Errorf("invalid max_call_chain_depth %q: %w", val, err)
		}
		cfg.MaxCallChainDepth = n
	case "max_search_results":
		n, err := strconv.Atoi(val)
		if err != nil {
			return fmt.Errorf("invalid max_search_results %q: %w", val, err)
		}
		cfg.MaxSearchResults = n
	case "languages":
		parts := strings.Split(val, ",")
		langs := make([]string, 0, len(parts))
		for _, p := range parts {
			if t := strings.TrimSpace(p); t != "" {
				langs = append(langs, t)
			}
		}
		cfg.Languages = langs
	default:
		return fmt.Errorf("unknown config key %q", key)
	}
	return nil
}

func applyEnvOverrides(cfg *Config) error {
	overrides := [][2]string{
		{"GGCTX_WORKERS", "workers"},
		{"GGCTX_TOKEN_BUDGET", "token_budget"},
		{"GGCTX_DEBOUNCE_MS", "debounce_ms"},
		{"GGCTX_COMPRESS_SNAPSHOTS", "compress_snapshots"},
		{"GGCTX_SYNC_WRITES", "sync_writes"},
		{"GGCTX_WAL_COMPACT_BYTES", "wal_compact_threshold_bytes"},
		{"GGCTX_DATA_DIR", "data_dir"},
		{"GGCTX_IGNORE_FILE", "ignore_file"},
		{"GGCTX_USE_GITIGNORE", "use_gitignore"},
		{"GGCTX_FOLLOW_SYMLINKS", "follow_symlinks"},
		{"GGCTX_MAX_CALL_CHAIN_DEPTH", "max_call_chain_depth"},
		{"GGCTX_MAX_SEARCH_RESULTS", "max_search_results"},
		{"GGCTX_LANGUAGES", "languages"},
	}

	for _, ov := range overrides {
		if val, ok := os.LookupEnv(ov[0]); ok {
			if err := setConfigKey(cfg, ov[1], val); err != nil {
				return fmt.Errorf("%w: env %s: %w", ErrInvalidConfig, ov[0], err)
			}
		}
	}
	return nil
}
