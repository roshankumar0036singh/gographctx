package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// DefaultIgnorePatterns lists standard repository paths excluded from indexing.
var DefaultIgnorePatterns = []string{
	".git/",
	".hg/",
	".svn/",
	".ggctx/",
	"node_modules/",
	"vendor/",
	"__pycache__/",
	".pytest_cache/",
	".mypy_cache/",
	".ruff_cache/",
	"dist/",
	"build/",
	"target/",
	".venv/",
	"venv/",
	".tox/",
}

type ignoreRule struct {
	baseDir  string // Normalized relative dir where the ignore file lives ("" for repo root)
	patSegs  []string
	dirOnly  bool
	negated  bool
	anchored bool
}

// IgnoreMatcher evaluates gitignore-compatible rules (including defaults, .gitignore,
// .ggctxignore, negation, anchored/unanchored patterns, ** globs, and directory scoping).
type IgnoreMatcher struct {
	rules []ignoreRule
}

// NewIgnoreMatcher compiles a slice of gitignore-format pattern lines relative to repo root.
func NewIgnoreMatcher(patterns []string) (*IgnoreMatcher, error) {
	m := &IgnoreMatcher{}
	if err := m.addPatterns("", patterns); err != nil {
		return nil, err
	}
	return m, nil
}

// DefaultIgnoreMatcher returns an IgnoreMatcher loaded with DefaultIgnorePatterns.
func DefaultIgnoreMatcher() *IgnoreMatcher {
	m, _ := NewIgnoreMatcher(DefaultIgnorePatterns)
	return m
}

// ParseIgnoreRules compiles rules from gitignore file text scoped at baseDir ("" for root).
func ParseIgnoreRules(baseDir, content string) (*IgnoreMatcher, error) {
	m := &IgnoreMatcher{}
	var lines []string
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if err := m.addPatterns(normalizeRelDir(baseDir), lines); err != nil {
		return nil, err
	}
	return m, nil
}

// LoadIgnoreFile reads and compiles patterns from path scoped at repo root.
func LoadIgnoreFile(path string) (*IgnoreMatcher, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseIgnoreRules("", string(raw))
}

// LoadRepoIgnoreMatcher builds the effective repository IgnoreMatcher from:
// DefaultIgnorePatterns -> cfg.DataDir -> <repoRoot>/.gitignore (if cfg.UseGitignore) -> <repoRoot>/<cfg.IgnoreFile>.
func LoadRepoIgnoreMatcher(repoRoot string, cfg Config) (*IgnoreMatcher, error) {
	m := &IgnoreMatcher{}
	defaults := append([]string{}, DefaultIgnorePatterns...)
	if dataDir := normalizeRelDir(cfg.DataDir); dataDir != "" && dataDir != ".ggctx" {
		defaults = append(defaults, "/"+dataDir+"/")
	}
	if err := m.addPatterns("", defaults); err != nil {
		return nil, err
	}

	if strings.TrimSpace(repoRoot) == "" {
		return m, nil
	}

	if cfg.UseGitignore {
		gitPath := filepath.Join(repoRoot, ".gitignore")
		if err := m.loadFileInto("", gitPath); err != nil {
			return nil, err
		}
	}

	ignoreName := strings.TrimSpace(cfg.IgnoreFile)
	if ignoreName == "" {
		ignoreName = DefaultIgnoreFileName
	}
	ggPath := filepath.Join(repoRoot, ignoreName)
	if err := m.loadFileInto("", ggPath); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *IgnoreMatcher) loadFileInto(baseDir, filePath string) error {
	raw, err := os.ReadFile(filePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read ignore file %q: %w", filePath, err)
	}
	return m.addFileContent(baseDir, string(raw))
}

// Child returns a new IgnoreMatcher inheriting m's rules plus additional patterns scoped to relDir.
func (m *IgnoreMatcher) Child(relDir string, patterns []string) (*IgnoreMatcher, error) {
	child := &IgnoreMatcher{
		rules: make([]ignoreRule, len(m.rules)),
	}
	copy(child.rules, m.rules)
	if err := child.addPatterns(normalizeRelDir(relDir), patterns); err != nil {
		return nil, err
	}
	return child, nil
}

// ChildFromFile returns a new IgnoreMatcher inheriting m's rules plus rules parsed from filePath under relDir.
func (m *IgnoreMatcher) ChildFromFile(relDir, filePath string) (*IgnoreMatcher, error) {
	raw, err := os.ReadFile(filePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return m, nil
		}
		return nil, fmt.Errorf("read ignore file %q: %w", filePath, err)
	}
	child := &IgnoreMatcher{
		rules: make([]ignoreRule, len(m.rules)),
	}
	copy(child.rules, m.rules)
	if err := child.addFileContent(normalizeRelDir(relDir), string(raw)); err != nil {
		return nil, err
	}
	return child, nil
}

func (m *IgnoreMatcher) addFileContent(baseDir, content string) error {
	var lines []string
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return m.addPatterns(baseDir, lines)
}

func (m *IgnoreMatcher) addPatterns(baseDir string, lines []string) error {
	for _, rawLine := range lines {
		rule, ok, err := parseIgnoreLine(baseDir, rawLine)
		if err != nil {
			return err
		}
		if ok {
			m.rules = append(m.rules, rule)
		}
	}
	return nil
}

func parseIgnoreLine(baseDir, raw string) (ignoreRule, bool, error) {
	line := strings.TrimSuffix(raw, "\r")
	line = stripTrailingUnescapedSpaces(line)
	if line == "" {
		return ignoreRule{}, false, nil
	}
	if strings.HasPrefix(line, "#") {
		return ignoreRule{}, false, nil
	}

	var rule ignoreRule
	rule.baseDir = baseDir

	if strings.HasPrefix(line, "!") {
		rule.negated = true
		line = line[1:]
	} else if strings.HasPrefix(line, `\#`) || strings.HasPrefix(line, `\!`) {
		line = line[1:]
	}

	if line == "" {
		return ignoreRule{}, false, nil
	}

	line = filepath.ToSlash(line)
	if strings.HasSuffix(line, "/") {
		rule.dirOnly = true
		line = strings.TrimRight(line, "/")
	}
	if line == "" {
		return ignoreRule{}, false, nil
	}

	if strings.HasPrefix(line, "/") || strings.Contains(line, "/") {
		rule.anchored = true
		line = strings.TrimPrefix(line, "/")
	}
	if line == "" {
		return ignoreRule{}, false, nil
	}

	segs := strings.Split(line, "/")
	for i := range segs {
		segs[i] = normalizeSegmentPattern(segs[i])
		if _, err := path.Match(segs[i], "probe"); err != nil {
			return ignoreRule{}, false, fmt.Errorf("%w: invalid ignore pattern %q: %w", ErrInvalidConfig, raw, err)
		}
	}
	rule.patSegs = segs
	return rule, true, nil
}

func stripTrailingUnescapedSpaces(s string) string {
	end := len(s)
	for end > 0 && (s[end-1] == ' ' || s[end-1] == '\t') {
		if end >= 2 && s[end-2] == '\\' {
			// Replace escaped space "\ " with literal " " and stop stripping.
			return s[:end-2] + s[end-1:end]
		}
		end--
	}
	return s[:end]
}

// normalizeSegmentPattern converts gitignore's '[!...]' class negation to path.Match's '[^...]'.
func normalizeSegmentPattern(seg string) string {
	var b strings.Builder
	b.Grow(len(seg))
	for i := 0; i < len(seg); i++ {
		if seg[i] == '[' && i+1 < len(seg) && seg[i+1] == '!' {
			b.WriteString("[^")
			i++
			continue
		}
		b.WriteByte(seg[i])
	}
	return b.String()
}

// ShouldIgnore reports whether relPath should be ignored.
// Paths ending with '/' or '\' are treated as directories; ancestor path components
// are automatically checked as directories so directory rules exclude descendants.
func (m *IgnoreMatcher) ShouldIgnore(relPath string) bool {
	if m == nil {
		return false
	}
	slash := filepath.ToSlash(relPath)
	isDir := strings.HasSuffix(slash, "/")
	return m.ShouldIgnorePath(slash, isDir)
}

// ShouldIgnorePath reports whether relPath (with explicit isDir flag) should be ignored.
func (m *IgnoreMatcher) ShouldIgnorePath(relPath string, isDir bool) bool {
	if m == nil || len(m.rules) == 0 {
		return false
	}

	norm := normalizeRelPath(relPath)
	if norm == "" || norm == "." {
		return false
	}

	// Gitignore semantics: if any ancestor directory of relPath is ignored,
	// relPath cannot be re-included and is ignored immediately.
	parts := strings.Split(norm, "/")
	for i := 1; i < len(parts); i++ {
		ancestor := strings.Join(parts[:i], "/")
		if m.evalExact(ancestor, parts[:i], true) {
			return true
		}
	}

	return m.evalExact(norm, parts, isDir)
}

func (m *IgnoreMatcher) evalExact(fullPath string, parts []string, isDir bool) bool {
	ignored := false
	for i := range m.rules {
		r := &m.rules[i]
		if r.dirOnly && !isDir {
			continue
		}

		subPath := fullPath
		subParts := parts
		if r.baseDir != "" {
			prefix := r.baseDir + "/"
			if !strings.HasPrefix(fullPath, prefix) {
				continue
			}
			subPath = strings.TrimPrefix(fullPath, prefix)
			if subPath == "" {
				continue
			}
			subParts = strings.Split(subPath, "/")
		}

		if r.match(subParts) {
			ignored = !r.negated
		}
	}
	return ignored
}

func (r *ignoreRule) match(pathSegs []string) bool {
	if len(pathSegs) == 0 {
		return false
	}
	if !r.anchored && len(r.patSegs) == 1 {
		if r.patSegs[0] == "**" {
			return true
		}
		ok, _ := path.Match(r.patSegs[0], pathSegs[len(pathSegs)-1])
		return ok
	}
	return matchSegments(r.patSegs, pathSegs)
}

func matchSegments(patSegs, pathSegs []string) bool {
	pi, ti := 0, 0
	starPat, starText := -1, 0

	for ti < len(pathSegs) {
		if pi < len(patSegs) && patSegs[pi] == "**" {
			starPat = pi
			starText = ti
			pi++
			continue
		}
		if pi < len(patSegs) {
			if ok, _ := path.Match(patSegs[pi], pathSegs[ti]); ok {
				pi++
				ti++
				continue
			}
		}
		if starPat != -1 {
			pi = starPat + 1
			starText++
			ti = starText
			continue
		}
		return false
	}
	for pi < len(patSegs) && patSegs[pi] == "**" {
		pi++
	}
	return pi == len(patSegs)
}

func normalizeRelDir(dir string) string {
	norm := normalizeRelPath(dir)
	if norm == "." {
		return ""
	}
	return norm
}

func normalizeRelPath(p string) string {
	s := filepath.ToSlash(p)
	for strings.HasPrefix(s, "./") {
		s = s[2:]
	}
	s = strings.Trim(s, "/")
	if s == "" {
		return ""
	}
	return path.Clean(s)
}
