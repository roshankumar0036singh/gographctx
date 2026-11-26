package treesitter

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/odvcencio/gotreesitter"
	golanggrammar "github.com/odvcencio/gotreesitter/grammars/go"
	javagrammar "github.com/odvcencio/gotreesitter/grammars/java"
	jsgrammar "github.com/odvcencio/gotreesitter/grammars/javascript"
	pythongrammar "github.com/odvcencio/gotreesitter/grammars/python"
	rustgrammar "github.com/odvcencio/gotreesitter/grammars/rust"
	tsxgrammar "github.com/odvcencio/gotreesitter/grammars/tsx"
	tsgrammar "github.com/odvcencio/gotreesitter/grammars/typescript"
	"github.com/roshankumar0036singh/gographctx/internal/parser"
)

// Canonical language identifiers supported by the compiled-in tree-sitter grammars.
const (
	LangGo         = "go"
	LangPython     = "python"
	LangTypeScript = "typescript"
	LangTSX        = "tsx"
	LangJavaScript = "javascript"
	LangRust       = "rust"
	LangJava       = "java"
)

var (
	// ErrManagerClosed indicates an operation was attempted after Close was called.
	ErrManagerClosed = errors.New("treesitter: manager is closed")
	// ErrNilManager indicates a nil Manager receiver.
	ErrNilManager = errors.New("treesitter: nil manager")
	// ErrUnsupportedLanguage indicates no compiled-in grammar matches the language or file.
	ErrUnsupportedLanguage = errors.New("treesitter: unsupported language")
	// ErrEmptyQueryPattern indicates an empty S-expression query string.
	ErrEmptyQueryPattern = errors.New("treesitter: empty query pattern")
	// ErrInvalidOption indicates invalid Manager configuration options.
	ErrInvalidOption = errors.New("treesitter: invalid manager option")
)

type languageSpec struct {
	name       string
	extensions []string
	load       func() *gotreesitter.Language
}

var builtinSpecs = []languageSpec{
	{
		name:       LangGo,
		extensions: []string{".go"},
		load:       golanggrammar.Language,
	},
	{
		name:       LangPython,
		extensions: []string{".py", ".pyi"},
		load:       pythongrammar.Language,
	},
	{
		name:       LangTypeScript,
		extensions: []string{".ts", ".mts", ".cts"},
		load:       tsgrammar.Language,
	},
	{
		name:       LangTSX,
		extensions: []string{".tsx"},
		load:       tsxgrammar.Language,
	},
	{
		name:       LangJavaScript,
		extensions: []string{".js", ".mjs", ".cjs", ".jsx"},
		load:       jsgrammar.Language,
	},
	{
		name:       LangRust,
		extensions: []string{".rs"},
		load:       rustgrammar.Language,
	},
	{
		name:       LangJava,
		extensions: []string{".java"},
		load:       javagrammar.Language,
	},
}

var (
	extToLang     map[string]string
	langToExts    map[string][]string
	canonicalList []string
	extensionList []string
)

func init() {
	extToLang = make(map[string]string)
	langToExts = make(map[string][]string, len(builtinSpecs))
	canonicalList = make([]string, 0, len(builtinSpecs))

	for _, spec := range builtinSpecs {
		canonicalList = append(canonicalList, spec.name)
		exts := make([]string, len(spec.extensions))
		for i, ext := range spec.extensions {
			norm := parser.NormalizeExtension(ext)
			exts[i] = norm
			extToLang[norm] = spec.name
			extensionList = append(extensionList, norm)
		}
		langToExts[spec.name] = exts
	}
	sort.Strings(canonicalList)
	sort.Strings(extensionList)
}

// Option configures a treesitter Manager.
type Option func(*managerConfig) error

type managerConfig struct {
	parseTimeout      time.Duration
	memoryBudgetBytes int64
}

// WithParseTimeout sets a per-parse timeout on pooled parsers. Zero disables timeout.
func WithParseTimeout(d time.Duration) Option {
	return func(cfg *managerConfig) error {
		if d < 0 {
			return fmt.Errorf("%w: negative parse timeout %v", ErrInvalidOption, d)
		}
		cfg.parseTimeout = d
		return nil
	}
}

// WithMemoryBudgetBytes sets a per-parse memory budget on pooled parsers. Zero disables the limit.
func WithMemoryBudgetBytes(bytes int64) Option {
	return func(cfg *managerConfig) error {
		if bytes < 0 {
			return fmt.Errorf("%w: negative memory budget %d", ErrInvalidOption, bytes)
		}
		cfg.memoryBudgetBytes = bytes
		return nil
	}
}

type langEntry struct {
	name    string
	grammar *gotreesitter.Language
	pool    *gotreesitter.ParserPool
}

// ParsedTree owns a parsed tree-sitter syntax tree along with the language grammar
// needed to inspect node types and text slices. Call Release when finished.
type ParsedTree struct {
	Language string
	Tree     *gotreesitter.Tree
	Grammar  *gotreesitter.Language
	Source   []byte

	released atomic.Bool
}

// RootNode returns the root syntax node, or nil if the tree is nil or released.
func (pt *ParsedTree) RootNode() *gotreesitter.Node {
	if pt == nil || pt.released.Load() || pt.Tree == nil {
		return nil
	}
	return pt.Tree.RootNode()
}

// RootType returns the root node symbol type (e.g., "source_file", "module", "program").
func (pt *ParsedTree) RootType() string {
	root := pt.RootNode()
	if root == nil || pt.Grammar == nil {
		return ""
	}
	return root.Type(pt.Grammar)
}

// HasError reports whether the parsed syntax tree contains any syntax errors or missing nodes.
func (pt *ParsedTree) HasError() bool {
	root := pt.RootNode()
	if root == nil {
		return false
	}
	return root.HasError()
}

// NodeText extracts the source slice covered by node n.
func (pt *ParsedTree) NodeText(n *gotreesitter.Node) string {
	if pt == nil || n == nil || pt.released.Load() {
		return ""
	}
	return n.Text(pt.Source)
}

// Release releases underlying arena memory held by the syntax tree. Safe to call multiple times.
func (pt *ParsedTree) Release() {
	if pt == nil || pt.released.Swap(true) {
		return
	}
	if pt.Tree != nil {
		pt.Tree.Release()
	}
}

// Manager initializes compiled-in grammars and manages concurrency-safe ParserPool
// instances for all supported languages.
type Manager struct {
	mu      sync.RWMutex
	closed  bool
	entries map[string]*langEntry
}

// NewManager initializes compiled-in grammars and parser pools for all supported languages.
func NewManager(opts ...Option) (*Manager, error) {
	var cfg managerConfig
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		if err := opt(&cfg); err != nil {
			return nil, err
		}
	}

	var poolOpts []gotreesitter.ParserPoolOption
	if cfg.parseTimeout > 0 {
		micros := uint64(cfg.parseTimeout.Microseconds())
		if micros == 0 {
			micros = 1
		}
		poolOpts = append(poolOpts, gotreesitter.WithParserPoolTimeoutMicros(micros))
	}
	if cfg.memoryBudgetBytes > 0 {
		poolOpts = append(poolOpts, gotreesitter.WithParserPoolMemoryBudgetBytes(cfg.memoryBudgetBytes))
	}

	entries := make(map[string]*langEntry, len(builtinSpecs))
	for _, spec := range builtinSpecs {
		lang := spec.load()
		if lang == nil {
			return nil, fmt.Errorf("treesitter: compiled grammar for %q returned nil", spec.name)
		}
		entries[spec.name] = &langEntry{
			name:    spec.name,
			grammar: lang,
			pool:    gotreesitter.NewParserPool(lang, poolOpts...),
		}
	}

	return &Manager{
		entries: entries,
	}, nil
}

// Close releases references to parser pools and marks the Manager as closed.
// Already-returned *ParsedTree values can still be safely released via ParsedTree.Release().
func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	m.closed = true
	clear(m.entries)
	return nil
}

func (m *Manager) lookupEntry(lang string) (*langEntry, string, error) {
	if m == nil {
		return nil, "", ErrNilManager
	}
	norm := normalizeLanguage(lang)
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return nil, "", ErrManagerClosed
	}
	entry, ok := m.entries[norm]
	if !ok {
		return nil, "", fmt.Errorf("%w: %q", ErrUnsupportedLanguage, lang)
	}
	return entry, norm, nil
}

func normalizeLanguage(lang string) string {
	norm := parser.NormalizeLanguage(lang)
	if norm == "jsx" {
		return LangJavaScript
	}
	return norm
}

// DetectLanguage resolves a canonical language name from a file path extension.
func DetectLanguage(path string) (string, bool) {
	ext := parser.NormalizeExtension(filepath.Ext(strings.TrimSpace(path)))
	if ext == "" || ext == "." {
		return "", false
	}
	lang, ok := extToLang[ext]
	return lang, ok
}

// SupportsLanguage reports whether lang maps to a compiled-in grammar.
func SupportsLanguage(lang string) bool {
	_, ok := langToExts[normalizeLanguage(lang)]
	return ok
}

// SupportsFile reports whether path maps to a supported grammar by file extension.
func SupportsFile(path string) bool {
	_, ok := DetectLanguage(path)
	return ok
}

// SupportedLanguages returns the sorted list of canonical supported language names.
func SupportedLanguages() []string {
	return slices.Clone(canonicalList)
}

// SupportedExtensions returns the sorted list of recognized file extensions.
func SupportedExtensions() []string {
	return slices.Clone(extensionList)
}

// ExtensionsForLanguage returns the file extensions associated with lang, or nil if unsupported.
func ExtensionsForLanguage(lang string) []string {
	exts, ok := langToExts[normalizeLanguage(lang)]
	if !ok {
		return nil
	}
	return slices.Clone(exts)
}

// Language returns the underlying *gotreesitter.Language for lang.
func (m *Manager) Language(lang string) (*gotreesitter.Language, error) {
	entry, _, err := m.lookupEntry(lang)
	if err != nil {
		return nil, err
	}
	return entry.grammar, nil
}

// Parse parses source using best-effort error-tolerant parsing for lang.
// Callers must call Release on the returned *ParsedTree when done.
func (m *Manager) Parse(lang string, source []byte) (*ParsedTree, error) {
	return m.ParseContext(context.Background(), lang, source)
}

// ParseContext parses source if ctx has not already been canceled or expired.
func (m *Manager) ParseContext(ctx context.Context, lang string, source []byte) (*ParsedTree, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entry, canonical, err := m.lookupEntry(lang)
	if err != nil {
		return nil, err
	}
	tree, err := entry.pool.Parse(source)
	if err != nil {
		return nil, fmt.Errorf("treesitter: parse %s: %w", canonical, err)
	}
	return &ParsedTree{
		Language: canonical,
		Tree:     tree,
		Grammar:  entry.grammar,
		Source:   source,
	}, nil
}

// ParseStrict parses source and propagates strict parse errors and early stops.
// Callers must call Release on the returned *ParsedTree when done.
func (m *Manager) ParseStrict(lang string, source []byte) (*ParsedTree, error) {
	entry, canonical, err := m.lookupEntry(lang)
	if err != nil {
		return nil, err
	}
	tree, err := entry.pool.ParseStrict(source)
	if err != nil {
		return nil, fmt.Errorf("treesitter: strict parse %s: %w", canonical, err)
	}
	return &ParsedTree{
		Language: canonical,
		Tree:     tree,
		Grammar:  entry.grammar,
		Source:   source,
	}, nil
}

// ParseFile detects the language from path and parses source.
func (m *Manager) ParseFile(path string, source []byte) (*ParsedTree, error) {
	lang, ok := DetectLanguage(path)
	if !ok {
		return nil, fmt.Errorf("%w: no grammar for %q", ErrUnsupportedLanguage, path)
	}
	return m.Parse(lang, source)
}

// CompileQuery compiles a tree-sitter S-expression query for lang.
func (m *Manager) CompileQuery(lang string, pattern string) (*gotreesitter.Query, error) {
	if strings.TrimSpace(pattern) == "" {
		return nil, ErrEmptyQueryPattern
	}
	entry, canonical, err := m.lookupEntry(lang)
	if err != nil {
		return nil, err
	}
	q, err := gotreesitter.NewQuery(pattern, entry.grammar)
	if err != nil {
		return nil, fmt.Errorf("treesitter: compile query for %s: %w", canonical, err)
	}
	return q, nil
}
