package parser

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/roshankumar0036singh/gographctx/internal/graph"
	"golang.org/x/sync/errgroup"
)

// ErrNilCoordinator indicates a nil Coordinator or missing Registry.
var ErrNilCoordinator = errors.New("parser: nil coordinator or registry")

// FileInput describes one source file queued for parallel parsing.
type FileInput struct {
	Path     string
	DiskPath string
	Language string
	Source   []byte
}

// CoordinatorConfig controls concurrency, ordering, and failure behavior for
// parallel multi-language parsing.
type CoordinatorConfig struct {
	// Workers sets max concurrent parse goroutines (defaults to runtime.NumCPU()).
	Workers int
	// FailFast cancels sibling tasks on the first parse/IO error when true.
	// By default (false), individual failures are recorded in BatchResult.Errors
	// while healthy files continue parsing.
	FailFast bool
	// DeterministicSort sorts files canonically by normalized Path before
	// assigning output indices. When false, the caller's input slice order is preserved.
	DeterministicSort bool
}

// FileError captures a per-file failure observed during batch parsing.
// Both BatchResult.Err() and FailFast error returns wrap FileError values so
// callers can consistently inspect failures via errors.As(err, &fileErr).
type FileError struct {
	Path     string
	Language string
	Err      error
}

// Error implements error.
func (e FileError) Error() string {
	if e.Language != "" {
		return fmt.Sprintf("parse %s (%s): %v", e.Path, e.Language, e.Err)
	}
	return fmt.Sprintf("parse %s: %v", e.Path, e.Err)
}

// Unwrap returns the underlying error.
func (e FileError) Unwrap() error {
	return e.Err
}

// SymbolEntry links a parsed symbol name/QName back to its defining file and kind.
type SymbolEntry struct {
	File      string
	Language  string
	Kind      graph.NodeKind
	Name      string
	QName     string
	Package   string
	StartLine uint32
	Exported  bool
}

// NameIndex maps symbol names, qualified names, and packages to defining files
// and symbol metadata from a single parse pass (eliminating two-pass pre-scans).
// Both exported and unexported fields/symbols are indexed with Exported preserved
// so intra-package receiver resolution can match private members while
// cross-package resolvers filter on Exported.
type NameIndex struct {
	ByName    map[string][]SymbolEntry
	ByQName   map[string][]SymbolEntry
	ByPackage map[string][]string
	ByFile    map[string][]SymbolEntry
}

// LookupName returns all symbol entries matching short name (or nil if absent).
func (idx *NameIndex) LookupName(name string) []SymbolEntry {
	if idx == nil {
		return nil
	}
	return slices.Clone(idx.ByName[name])
}

// LookupQName returns all symbol entries matching qualified name (or nil if absent).
func (idx *NameIndex) LookupQName(qname string) []SymbolEntry {
	if idx == nil {
		return nil
	}
	return slices.Clone(idx.ByQName[qname])
}

// FilesForSymbol returns the sorted list of unique file paths defining name.
func (idx *NameIndex) FilesForSymbol(name string) []string {
	if idx == nil {
		return nil
	}
	entries := idx.ByName[name]
	if len(entries) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(entries))
	files := make([]string, 0, len(entries))
	for _, entry := range entries {
		if _, ok := seen[entry.File]; !ok {
			seen[entry.File] = struct{}{}
			files = append(files, entry.File)
		}
	}
	slices.SortFunc(files, func(a, b string) int {
		return cmp.Compare(canonicalPathKey(a), canonicalPathKey(b))
	})
	return files
}

// FilesForPackage returns sorted unique file paths belonging to pkg.
func (idx *NameIndex) FilesForPackage(pkg string) []string {
	if idx == nil {
		return nil
	}
	return slices.Clone(idx.ByPackage[pkg])
}

// BuildNameIndex builds a deterministic NameIndex from a slice of ParsedFiles.
func BuildNameIndex(files []*ParsedFile) *NameIndex {
	idx := &NameIndex{
		ByName:    make(map[string][]SymbolEntry),
		ByQName:   make(map[string][]SymbolEntry),
		ByPackage: make(map[string][]string),
		ByFile:    make(map[string][]SymbolEntry, len(files)),
	}
	pkgFiles := make(map[string]map[string]struct{})

	addEntry := func(entry SymbolEntry) {
		if entry.Name != "" {
			idx.ByName[entry.Name] = append(idx.ByName[entry.Name], entry)
		}
		if entry.QName != "" {
			idx.ByQName[entry.QName] = append(idx.ByQName[entry.QName], entry)
		}
		idx.ByFile[entry.File] = append(idx.ByFile[entry.File], entry)
	}

	for _, pf := range files {
		if pf == nil {
			continue
		}
		if pf.Package != "" {
			set, ok := pkgFiles[pf.Package]
			if !ok {
				set = make(map[string]struct{})
				pkgFiles[pf.Package] = set
			}
			set[pf.Path] = struct{}{}
		}

		for _, fn := range pf.Functions {
			addEntry(SymbolEntry{
				File:      pf.Path,
				Language:  pf.Language,
				Kind:      fn.EffectiveKind(),
				Name:      fn.Name,
				QName:     fn.QName,
				Package:   pf.Package,
				StartLine: fn.StartLine,
				Exported:  fn.Exported,
			})
		}
		for _, cls := range pf.Classes {
			addEntry(SymbolEntry{
				File:      pf.Path,
				Language:  pf.Language,
				Kind:      cls.EffectiveKind(),
				Name:      cls.Name,
				QName:     cls.QName,
				Package:   pf.Package,
				StartLine: cls.StartLine,
				Exported:  cls.Exported,
			})
			for _, field := range cls.Fields {
				fq := field.QName
				if fq == "" {
					fq = cls.QName + "::" + field.Name
				}
				addEntry(SymbolEntry{
					File:      pf.Path,
					Language:  pf.Language,
					Kind:      graph.KindProperty,
					Name:      field.Name,
					QName:     fq,
					Package:   pf.Package,
					StartLine: field.StartLine,
					Exported:  field.Exported,
				})
			}
		}
		for _, v := range pf.Variables {
			addEntry(SymbolEntry{
				File:      pf.Path,
				Language:  pf.Language,
				Kind:      graph.KindVariable,
				Name:      v.Name,
				QName:     v.QName,
				Package:   pf.Package,
				StartLine: v.StartLine,
				Exported:  v.Exported,
			})
		}
		for _, ta := range pf.TypeAliases {
			addEntry(SymbolEntry{
				File:      pf.Path,
				Language:  pf.Language,
				Kind:      graph.KindTypeAlias,
				Name:      ta.Name,
				QName:     ta.QName,
				Package:   pf.Package,
				StartLine: ta.StartLine,
				Exported:  ta.Exported,
			})
		}
	}

	sortEntries := func(entries []SymbolEntry) {
		slices.SortFunc(entries, func(a, b SymbolEntry) int {
			if c := cmp.Compare(canonicalPathKey(a.File), canonicalPathKey(b.File)); c != 0 {
				return c
			}
			if c := cmp.Compare(a.StartLine, b.StartLine); c != 0 {
				return c
			}
			if c := cmp.Compare(a.QName, b.QName); c != 0 {
				return c
			}
			return cmp.Compare(a.Kind, b.Kind)
		})
	}

	for name, entries := range idx.ByName {
		sortEntries(entries)
		idx.ByName[name] = entries
	}
	for qname, entries := range idx.ByQName {
		sortEntries(entries)
		idx.ByQName[qname] = entries
	}
	for file, entries := range idx.ByFile {
		sortEntries(entries)
		idx.ByFile[file] = entries
	}
	for pkg, set := range pkgFiles {
		paths := make([]string, 0, len(set))
		for p := range set {
			paths = append(paths, p)
		}
		slices.SortFunc(paths, func(a, b string) int {
			return cmp.Compare(canonicalPathKey(a), canonicalPathKey(b))
		})
		idx.ByPackage[pkg] = paths
	}

	return idx
}

func canonicalPathKey(p string) string {
	trimmed := strings.TrimSpace(p)
	if trimmed == "" {
		return ""
	}
	return path.Clean(strings.ReplaceAll(trimmed, "\\", "/"))
}

// BatchResult aggregates deterministic parse outputs, per-file errors, language
// breakdown, and the single-pass NameIndex built from all parsed files.
type BatchResult struct {
	Files      []*ParsedFile
	ByLanguage map[string]int
	Errors     []FileError
	NameIndex  *NameIndex
	Duration   time.Duration
}

// HasErrors reports whether any file encountered a fatal parse or read failure.
func (r *BatchResult) HasErrors() bool {
	return r != nil && len(r.Errors) > 0
}

// Err returns a joined error across all recorded FileErrors, or nil if none occurred.
func (r *BatchResult) Err() error {
	if r == nil || len(r.Errors) == 0 {
		return nil
	}
	errs := make([]error, len(r.Errors))
	for i := range r.Errors {
		errs[i] = r.Errors[i]
	}
	return errors.Join(errs...)
}

// Coordinator orchestrates parallel single-pass parsing across all registered languages.
type Coordinator struct {
	reg *Registry
	cfg CoordinatorConfig
}

// NewCoordinator constructs a parallel parse Coordinator backed by reg.
func NewCoordinator(reg *Registry, cfg CoordinatorConfig) (*Coordinator, error) {
	if reg == nil {
		return nil, ErrNilCoordinator
	}
	if cfg.Workers <= 0 {
		cfg.Workers = runtime.NumCPU()
		if cfg.Workers < 1 {
			cfg.Workers = 1
		}
	}
	return &Coordinator{
		reg: reg,
		cfg: cfg,
	}, nil
}

// Workers returns the configured concurrency limit.
func (c *Coordinator) Workers() int {
	if c == nil {
		return 0
	}
	return c.cfg.Workers
}

// ParseGrouped accepts file lists grouped by language, flattens them
// deterministically, and parses all files concurrently via ParseFiles.
func (c *Coordinator) ParseGrouped(ctx context.Context, grouped map[string][]FileInput) (*BatchResult, error) {
	if c == nil || c.reg == nil {
		return nil, ErrNilCoordinator
	}
	if len(grouped) == 0 {
		return &BatchResult{
			ByLanguage: make(map[string]int),
			NameIndex:  BuildNameIndex(nil),
		}, nil
	}

	langs := make([]string, 0, len(grouped))
	for lang := range grouped {
		langs = append(langs, lang)
	}
	slices.Sort(langs)

	var flat []FileInput
	for _, lang := range langs {
		normLang := NormalizeLanguage(lang)
		for _, item := range grouped[lang] {
			if item.Language == "" {
				item.Language = normLang
			}
			flat = append(flat, item)
		}
	}
	return c.ParseFiles(ctx, flat)
}

// ParseFiles parses all inputs in parallel using an errgroup worker pool,
// returning deterministically ordered ParsedFiles, a single-pass NameIndex, and
// aggregated per-file errors.
func (c *Coordinator) ParseFiles(ctx context.Context, inputs []FileInput) (*BatchResult, error) {
	if c == nil || c.reg == nil {
		return nil, ErrNilCoordinator
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	start := time.Now()
	ordered := slices.Clone(inputs)
	if c.cfg.DeterministicSort {
		slices.SortStableFunc(ordered, func(a, b FileInput) int {
			return cmp.Compare(canonicalPathKey(a.Path), canonicalPathKey(b.Path))
		})
	}

	slots := make([]*ParsedFile, len(ordered))
	errSlots := make([]*FileError, len(ordered))

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(c.cfg.Workers)

	for i, input := range ordered {
		idx := i
		item := input
		g.Go(func() error {
			pf, fileErr := c.parseOne(gctx, item)
			if fileErr != nil {
				errSlots[idx] = fileErr
				if c.cfg.FailFast {
					return *fileErr
				}
				return nil
			}
			slots[idx] = pf
			return nil
		})
	}

	groupErr := g.Wait()

	res := &BatchResult{
		Files:      make([]*ParsedFile, 0, len(slots)),
		ByLanguage: make(map[string]int),
		Duration:   time.Since(start),
	}
	for i := range slots {
		if slots[i] != nil {
			res.Files = append(res.Files, slots[i])
			res.ByLanguage[slots[i].Language]++
		}
		if errSlots[i] != nil {
			res.Errors = append(res.Errors, *errSlots[i])
		}
	}
	res.NameIndex = BuildNameIndex(res.Files)

	if groupErr != nil && c.cfg.FailFast {
		return res, groupErr
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return res, ctxErr
	}
	return res, nil
}

func (c *Coordinator) parseOne(ctx context.Context, item FileInput) (*ParsedFile, *FileError) {
	if err := ctx.Err(); err != nil {
		return nil, &FileError{Path: item.Path, Language: item.Language, Err: err}
	}

	var lp LanguageParser
	var ok bool
	langHint := NormalizeLanguage(item.Language)
	if langHint != "" {
		lp, ok = c.reg.Get(langHint)
	} else {
		lp, ok = c.reg.ForFile(item.Path)
	}
	if !ok {
		return nil, &FileError{
			Path:     item.Path,
			Language: langHint,
			Err:      fmt.Errorf("%w: no parser for %q", ErrUnsupportedLanguage, item.Path),
		}
	}

	src := item.Source
	if src == nil {
		readTarget := item.DiskPath
		if readTarget == "" {
			readTarget = item.Path
		}
		data, err := os.ReadFile(readTarget)
		if err != nil {
			return nil, &FileError{Path: item.Path, Language: lp.Language(), Err: err}
		}
		src = data
	}

	pf, err := lp.Parse(ctx, item.Path, src)
	if err != nil {
		return nil, &FileError{Path: item.Path, Language: lp.Language(), Err: err}
	}
	return pf, nil
}
