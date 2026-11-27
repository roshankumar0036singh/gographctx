package resolver

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/roshankumar0036singh/gographctx/internal/graph"
	"github.com/roshankumar0036singh/gographctx/internal/parser"
)

// TypeOrigin describes where a variable or expression type annotation came from.
type TypeOrigin uint8

const (
	OriginUnknown TypeOrigin = iota
	OriginAssignment
	OriginParameter
	OriginReturnType
	OriginConstructor
	OriginField
	OriginLiteral
)

// String returns a concise name for TypeOrigin.
func (o TypeOrigin) String() string {
	switch o {
	case OriginAssignment:
		return "assignment"
	case OriginParameter:
		return "parameter"
	case OriginReturnType:
		return "return_type"
	case OriginConstructor:
		return "constructor"
	case OriginField:
		return "field"
	case OriginLiteral:
		return "literal"
	default:
		return "unknown"
	}
}

// TypeInfo captures an inferred type binding for a local variable, parameter,
// or expression within a parsed file.
type TypeInfo struct {
	Name       string     `json:"name"`
	QName      string     `json:"qualified_name,omitempty"`
	Package    string     `json:"package,omitempty"`
	Origin     TypeOrigin `json:"origin"`
	Confidence float32    `json:"confidence"`
	Line       uint32     `json:"line,omitempty"`
}

// ResolvedImport represents an import edge resolved against workspace modules/files.
type ResolvedImport struct {
	SourceFile    string   `json:"source_file"`
	ImportPath    string   `json:"import_path"`
	TargetPackage string   `json:"target_package,omitempty"`
	TargetFile    string   `json:"target_file,omitempty"`
	Alias         string   `json:"alias,omitempty"`
	ImportedNames []string `json:"imported_names,omitempty"`
	Line          uint32   `json:"line,omitempty"`
	IsWildcard    bool     `json:"is_wildcard,omitempty"`
	IsTypeOnly    bool     `json:"is_type_only,omitempty"`
	IsRelative    bool     `json:"is_relative,omitempty"`
	IsExternal    bool     `json:"is_external,omitempty"`
}

// FileResolution aggregates imports, inferred types, and call outcomes for one file.
type FileResolution struct {
	Path       string              `json:"path"`
	Language   string              `json:"language"`
	Imports    []ResolvedImport    `json:"imports,omitempty"`
	Types      map[string]TypeInfo `json:"types,omitempty"`
	Resolved   []ResolvedCall      `json:"resolved,omitempty"`
	Unresolved []UnresolvedCall    `json:"unresolved,omitempty"`
}

// ResolutionContext supplies read-only access to the indexed symbol graph and
// single-pass NameIndex for cross-file resolution.
type ResolutionContext struct {
	View      *graph.ReadView
	NameIndex *parser.NameIndex
	RepoRoot  string
	Module    string
}

// LookupByQName returns matching graph nodes for qname from View, if present.
func (rc *ResolutionContext) LookupByQName(qname string) []graph.Node {
	if rc == nil || rc.View == nil || qname == "" {
		return nil
	}
	return rc.View.GetByQName(qname)
}

// LookupByName returns matching graph nodes for short name from View, if present.
func (rc *ResolutionContext) LookupByName(name string) []graph.Node {
	if rc == nil || rc.View == nil || name == "" {
		return nil
	}
	return rc.View.GetByName(name)
}

// LookupSymbolEntries returns NameIndex entries for short name, if available.
func (rc *ResolutionContext) LookupSymbolEntries(name string) []parser.SymbolEntry {
	if rc == nil || rc.NameIndex == nil || name == "" {
		return nil
	}
	return rc.NameIndex.LookupName(name)
}

// LanguageResolver defines per-language import resolution, local type inference,
// and call-site resolution behavior.
type LanguageResolver interface {
	Language() string
	ResolveImports(ctx context.Context, file *parser.ParsedFile, rc *ResolutionContext) ([]ResolvedImport, error)
	InferTypes(ctx context.Context, file *parser.ParsedFile, rc *ResolutionContext) (map[string]TypeInfo, error)
	ResolveCalls(ctx context.Context, file *parser.ParsedFile, rc *ResolutionContext, imports []ResolvedImport, types map[string]TypeInfo) ([]ResolvedCall, []UnresolvedCall, error)
}

// Registry stores registered LanguageResolver implementations keyed by canonical language.
// Safe for concurrent use.
type Registry struct {
	mu        sync.RWMutex
	resolvers map[string]LanguageResolver
}

// NewRegistry constructs an empty resolver Registry.
func NewRegistry() *Registry {
	return &Registry{
		resolvers: make(map[string]LanguageResolver),
	}
}

// Register adds lr to the registry under its canonical normalized language name.
func (r *Registry) Register(lr LanguageResolver) error {
	if r == nil || lr == nil {
		return fmt.Errorf("%w: nil resolver or registry", ErrInvalidResolver)
	}
	lang := parser.NormalizeLanguage(lr.Language())
	if lang == "" {
		return fmt.Errorf("%w: empty language name", ErrInvalidResolver)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.resolvers[lang]; exists {
		return fmt.Errorf("%w: language %q already registered", ErrDuplicateResolver, lang)
	}
	r.resolvers[lang] = lr
	return nil
}

// MustRegister registers lr and panics if registration fails.
func (r *Registry) MustRegister(lr LanguageResolver) {
	if err := r.Register(lr); err != nil {
		panic(err)
	}
}

// Get looks up the LanguageResolver registered for lang.
func (r *Registry) Get(lang string) (LanguageResolver, bool) {
	if r == nil {
		return nil, false
	}
	norm := parser.NormalizeLanguage(lang)
	if norm == "" {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	lr, ok := r.resolvers[norm]
	return lr, ok
}

// Supports reports whether lang has a registered resolver.
func (r *Registry) Supports(lang string) bool {
	_, ok := r.Get(lang)
	return ok
}

// Languages returns sorted canonical names of registered resolvers.
func (r *Registry) Languages() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	langs := make([]string, 0, len(r.resolvers))
	for lang := range r.resolvers {
		langs = append(langs, lang)
	}
	sort.Strings(langs)
	return langs
}

// ResolveFile executes the 3-stage LanguageResolver pipeline (ResolveImports ->
// InferTypes -> ResolveCalls) for file.
func (r *Registry) ResolveFile(ctx context.Context, file *parser.ParsedFile, rc *ResolutionContext) (*FileResolution, error) {
	if r == nil || file == nil {
		return nil, fmt.Errorf("%w: nil registry or parsed file", ErrInvalidResolver)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if rc == nil {
		rc = &ResolutionContext{}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	lr, ok := r.Get(file.Language)
	if !ok {
		return nil, fmt.Errorf("%w: no resolver registered for %q", ErrUnsupportedLanguage, file.Language)
	}

	imports, err := lr.ResolveImports(ctx, file, rc)
	if err != nil {
		return nil, err
	}
	types, err := lr.InferTypes(ctx, file, rc)
	if err != nil {
		return nil, err
	}
	resolved, unresolved, err := lr.ResolveCalls(ctx, file, rc, imports, types)
	if err != nil {
		return nil, err
	}

	return &FileResolution{
		Path:       file.Path,
		Language:   parser.NormalizeLanguage(file.Language),
		Imports:    imports,
		Types:      types,
		Resolved:   resolved,
		Unresolved: unresolved,
	}, nil
}
