package parser

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

var (
	// ErrInvalidParser indicates a nil or malformed LanguageParser registration.
	ErrInvalidParser = errors.New("parser: invalid language parser")
	// ErrDuplicateParser indicates a language or extension is already registered.
	ErrDuplicateParser = errors.New("parser: duplicate registration")
	// ErrUnsupportedLanguage indicates no parser is registered for a language or file.
	ErrUnsupportedLanguage = errors.New("parser: unsupported language")
)

// LanguageParser extracts structural graph entities and syntactic calls from a
// single source file in its target language.
type LanguageParser interface {
	Language() string
	Extensions() []string
	Parse(ctx context.Context, path string, source []byte) (*ParsedFile, error)
}

var languageAliases = map[string]string{
	"golang":     "go",
	"py":         "python",
	"python3":    "python",
	"ts":         "typescript",
	"js":         "javascript",
	"jsx":        "javascript",
	"ecmascript": "javascript",
	"rs":         "rust",
}

// NormalizeLanguage canonicalizes a language name or common alias.
func NormalizeLanguage(lang string) string {
	norm := strings.ToLower(strings.TrimSpace(lang))
	if canonical, ok := languageAliases[norm]; ok {
		return canonical
	}
	return norm
}

// NormalizeExtension canonicalizes a file extension as lowercase with a leading dot.
func NormalizeExtension(ext string) string {
	norm := strings.ToLower(strings.TrimSpace(ext))
	if norm == "" {
		return ""
	}
	if !strings.HasPrefix(norm, ".") {
		norm = "." + norm
	}
	return norm
}

// Registry maps canonical language identifiers and file extensions to LanguageParser
// implementations. It is safe for concurrent use.
type Registry struct {
	mu      sync.RWMutex
	byLang  map[string]LanguageParser
	byExt   map[string]LanguageParser
	extLang map[string]string
}

// NewRegistry returns an empty parser Registry.
func NewRegistry() *Registry {
	return &Registry{
		byLang:  make(map[string]LanguageParser),
		byExt:   make(map[string]LanguageParser),
		extLang: make(map[string]string),
	}
}

// Register adds p to the registry for its Language() and Extensions().
func (r *Registry) Register(p LanguageParser) error {
	if r == nil || p == nil {
		return fmt.Errorf("%w: nil parser or registry", ErrInvalidParser)
	}
	lang := NormalizeLanguage(p.Language())
	if lang == "" {
		return fmt.Errorf("%w: empty language name", ErrInvalidParser)
	}

	rawExts := p.Extensions()
	exts := make([]string, 0, len(rawExts))
	seenExt := make(map[string]struct{}, len(rawExts))
	for _, raw := range rawExts {
		ext := NormalizeExtension(raw)
		if ext == "" || ext == "." {
			return fmt.Errorf("%w: invalid extension %q for language %q", ErrInvalidParser, raw, lang)
		}
		if _, dup := seenExt[ext]; dup {
			continue
		}
		seenExt[ext] = struct{}{}
		exts = append(exts, ext)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.byLang[lang]; exists {
		return fmt.Errorf("%w: language %q already registered", ErrDuplicateParser, lang)
	}
	for _, ext := range exts {
		if existingLang, exists := r.extLang[ext]; exists {
			return fmt.Errorf("%w: extension %q already registered to %q", ErrDuplicateParser, ext, existingLang)
		}
	}

	r.byLang[lang] = p
	for _, ext := range exts {
		r.byExt[ext] = p
		r.extLang[ext] = lang
	}
	return nil
}

// MustRegister registers p and panics if registration fails.
func (r *Registry) MustRegister(p LanguageParser) {
	if err := r.Register(p); err != nil {
		panic(err)
	}
}

// Get returns the parser registered for lang (after alias normalization).
func (r *Registry) Get(lang string) (LanguageParser, bool) {
	if r == nil {
		return nil, false
	}
	norm := NormalizeLanguage(lang)
	if norm == "" {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.byLang[norm]
	return p, ok
}

// ForFile resolves a parser from the file path's extension.
func (r *Registry) ForFile(path string) (LanguageParser, bool) {
	if r == nil {
		return nil, false
	}
	ext := NormalizeExtension(filepath.Ext(strings.TrimSpace(path)))
	if ext == "" || ext == "." {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.byExt[ext]
	return p, ok
}

// SupportsLanguage reports whether lang has a registered parser.
func (r *Registry) SupportsLanguage(lang string) bool {
	_, ok := r.Get(lang)
	return ok
}

// SupportsFile reports whether path maps to a registered parser by extension.
func (r *Registry) SupportsFile(path string) bool {
	_, ok := r.ForFile(path)
	return ok
}

// Languages returns the sorted list of registered canonical language names.
func (r *Registry) Languages() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	langs := make([]string, 0, len(r.byLang))
	for lang := range r.byLang {
		langs = append(langs, lang)
	}
	sort.Strings(langs)
	return langs
}

// Extensions returns the sorted list of registered lowercase extensions.
func (r *Registry) Extensions() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	exts := make([]string, 0, len(r.byExt))
	for ext := range r.byExt {
		exts = append(exts, ext)
	}
	sort.Strings(exts)
	return exts
}

// ParseFile detects the parser for path and executes Parse.
func (r *Registry) ParseFile(ctx context.Context, path string, source []byte) (*ParsedFile, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p, ok := r.ForFile(path)
	if !ok {
		return nil, fmt.Errorf("%w: no parser registered for %q", ErrUnsupportedLanguage, path)
	}
	return p.Parse(ctx, path, source)
}
