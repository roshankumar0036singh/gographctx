package watcher

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/roshankumar0036singh/gographctx/internal/config"
	"github.com/roshankumar0036singh/gographctx/internal/graph"
	"github.com/roshankumar0036singh/gographctx/internal/parser"
	"github.com/roshankumar0036singh/gographctx/internal/parser/treesitter"
	"github.com/roshankumar0036singh/gographctx/internal/pipeline"
	"github.com/roshankumar0036singh/gographctx/internal/resolver"
	"github.com/roshankumar0036singh/gographctx/internal/storage"
)

// ChangeAction describes the high-level mutation that triggered an incremental update.
type ChangeAction string

const (
	ChangeModified ChangeAction = "modified"
	ChangeAdded    ChangeAction = "added"
	ChangeDeleted  ChangeAction = "deleted"
)

// ChangeSet reports the symbol diffs and blast radius processed during an incremental update.
type ChangeSet struct {
	File              string        `json:"file"`
	Action            ChangeAction  `json:"action"`
	BodyOnly          bool          `json:"body_only"`
	AddedSymbols      []string      `json:"added_symbols,omitempty"`
	RemovedSymbols    []string      `json:"removed_symbols,omitempty"`
	ModifiedSymbols   []string      `json:"modified_symbols,omitempty"`
	ReResolvedFiles   []string      `json:"re_resolved_files,omitempty"`
	DependentsUpdated int           `json:"dependents_updated"`
	Duration          time.Duration `json:"duration"`
}

// IncrementalEngine executes change-proportional re-parsing, blast-radius
// resolution, atomic Graph replacement, and WAL persistence.
type IncrementalEngine struct {
	mu          sync.Mutex
	repoRoot    string
	storeDir    string
	cfg         config.Config
	g           *graph.Graph
	store       *storage.Store
	mgr         *treesitter.Manager
	preg        *parser.Registry
	rreg        *resolver.Registry
	depTracker  *DepTracker
	parsedCache map[string]*parser.ParsedFile
}

// NewIncrementalEngine creates (or loads) the repository index and returns a
// ready-to-use IncrementalEngine backed by *graph.Graph and *storage.Store.
func NewIncrementalEngine(ctx context.Context, repoRoot string, cfg config.Config, opts pipeline.IndexOptions) (*IncrementalEngine, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cfg.Workers <= 0 {
		cfg = config.DefaultConfig()
	}

	absRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve repository root: %w", err)
	}
	canonRoot := absRoot
	if resolved, err := filepath.EvalSymlinks(absRoot); err == nil {
		canonRoot = resolved
	}

	res, err := pipeline.IndexRepository(ctx, canonRoot, cfg, opts)
	if err != nil {
		return nil, err
	}

	snapOpts := storage.WriteOptions{Compress: cfg.CompressSnapshots, Sync: cfg.SyncWrites}
	walOpts := storage.WALOptions{Sync: cfg.SyncWrites, CompactThresholdBytes: cfg.WALCompactThresholdBytes}
	store, loadedG, _, err := storage.LoadStore(res.StoreDir, snapOpts, walOpts, storage.LoadOptions{
		AllowTrailingPartialWAL: true,
	})
	if err != nil {
		return nil, fmt.Errorf("load incremental store: %w", err)
	}

	mgr, err := treesitter.NewManager()
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	preg, err := pipeline.DefaultParserRegistry(mgr)
	if err != nil {
		_ = store.Close()
		mgr.Close()
		return nil, err
	}
	rreg, err := pipeline.DefaultResolverRegistry()
	if err != nil {
		_ = store.Close()
		mgr.Close()
		return nil, err
	}

	dt := NewDepTracker()
	loadedG.View(func(v *graph.ReadView) {
		dt.RefreshFromView(v)
	})

	return &IncrementalEngine{
		repoRoot:    canonRoot,
		storeDir:    res.StoreDir,
		cfg:         cfg,
		g:           loadedG,
		store:       store,
		mgr:         mgr,
		preg:        preg,
		rreg:        rreg,
		depTracker:  dt,
		parsedCache: make(map[string]*parser.ParsedFile),
	}, nil
}

// Graph returns the live in-memory graph maintained by the engine.
func (e *IncrementalEngine) Graph() *graph.Graph {
	return e.g
}

// Dependents returns all files currently tracked as depending on relPath.
func (e *IncrementalEngine) Dependents(relPath string) []string {
	rel, err := e.normalizeRepoPath(relPath)
	if err != nil {
		return nil
	}
	return e.depTracker.Dependents(rel)
}

// Close flushes/closes WAL handles and tree-sitter parsers cleanly.
func (e *IncrementalEngine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	var errs []error
	if e.store != nil {
		if err := e.store.Close(); err != nil {
			errs = append(errs, err)
		}
		e.store = nil
	}
	if e.mgr != nil {
		e.mgr.Close()
		e.mgr = nil
	}
	return errors.Join(errs...)
}

// UpdateFile incrementally re-parses pathArg, diffs symbols vs the existing graph,
// determines whether dependents must be re-resolved, updates Graph in-place, and
// appends delta frames to the WAL.
func (e *IncrementalEngine) UpdateFile(ctx context.Context, pathArg string) (ChangeSet, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return ChangeSet{}, err
	}

	start := time.Now()
	relPath, err := e.normalizeRepoPath(pathArg)
	if err != nil {
		return ChangeSet{}, err
	}

	absPath := filepath.Join(e.repoRoot, filepath.FromSlash(relPath))
	src, err := os.ReadFile(absPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return e.RemoveFile(ctx, relPath)
		}
		return ChangeSet{}, fmt.Errorf("read %q: %w", relPath, err)
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	lp, ok := e.preg.ForFile(relPath)
	if !ok {
		return ChangeSet{}, fmt.Errorf("%w: no parser for %q", parser.ErrUnsupportedLanguage, relPath)
	}
	newPF, err := lp.Parse(ctx, relPath, src)
	if err != nil {
		return ChangeSet{}, fmt.Errorf("parse %q: %w", relPath, err)
	}

	newSymFrag, err := pipeline.BuildSymbolFragment(newPF)
	if err != nil {
		return ChangeSet{}, err
	}

	// Snapshot existing symbols for relPath and reverse dependents BEFORE replacing nodes.
	var oldNodes []graph.Node
	e.g.View(func(v *graph.ReadView) {
		oldNodes = v.GetByFile(relPath)
	})

	diff := diffFileSymbols(oldNodes, newSymFrag.Nodes)
	isNewFile := len(oldNodes) == 0
	action := ChangeModified
	if isNewFile {
		action = ChangeAdded
	}

	cs := ChangeSet{
		File:            relPath,
		Action:          action,
		AddedSymbols:    diff.addedNames,
		RemovedSymbols:  diff.removedNames,
		ModifiedSymbols: diff.modifiedNames,
	}

	bodyOnly := !isNewFile &&
		len(diff.addedNames) == 0 &&
		len(diff.removedNames) == 0 &&
		len(diff.modifiedNames) == 0
	cs.BodyOnly = bodyOnly

	// Compute blast radius before structural replacement removes stale outgoing/incoming hints.
	depSet := make(map[string]struct{})
	if !bodyOnly {
		for _, dep := range e.depTracker.Dependents(relPath) {
			depSet[dep] = struct{}{}
		}
		if len(diff.addedSimpleNames) > 0 {
			e.g.View(func(v *graph.ReadView) {
				for _, f := range e.depTracker.CallSiteDependents(v, relPath, diff.addedSimpleNames) {
					depSet[f] = struct{}{}
				}
			})
		}
	}

	// Stage 1: Replace structural symbols for relPath so dependents can resolve against new exports.
	e.parsedCache[relPath] = newPF
	if _, err := e.g.ReplaceFile(relPath, newSymFrag.Nodes, newSymFrag.Edges, nil); err != nil {
		return ChangeSet{}, err
	}

	// Gather re-resolution targets: relPath first, then sorted dependents.
	targets := []string{relPath}
	depList := make([]string, 0, len(depSet))
	for d := range depSet {
		if d != relPath {
			depList = append(depList, d)
		}
	}
	slices.SortFunc(depList, cmp.Compare[string])
	targets = append(targets, depList...)

	// Ensure all affected dependent files are parsed and cached.
	targetParsed := make([]*parser.ParsedFile, 0, len(targets))
	targetParsed = append(targetParsed, newPF)
	for _, depRel := range depList {
		dpf, err := e.ensureParsedLocked(ctx, depRel)
		if err != nil {
			continue
		}
		targetParsed = append(targetParsed, dpf)
	}

	if err := e.reResolveAndPersistLocked(ctx, targetParsed); err != nil {
		return ChangeSet{}, err
	}

	cs.ReResolvedFiles = targets
	cs.DependentsUpdated = len(depList)
	cs.Duration = time.Since(start)
	return cs, nil
}

// RemoveFile deletes relPath from Graph and DepTracker, appends a WAL tombstone,
// and re-resolves all files that previously depended on relPath.
func (e *IncrementalEngine) RemoveFile(ctx context.Context, pathArg string) (ChangeSet, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return ChangeSet{}, err
	}

	start := time.Now()
	relPath, err := e.normalizeRepoPath(pathArg)
	if err != nil {
		return ChangeSet{}, err
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	var removedNames []string
	e.g.View(func(v *graph.ReadView) {
		for _, n := range v.GetByFile(relPath) {
			if n.Kind != graph.KindFile && n.Kind != graph.KindModule && n.Kind != graph.KindExternal {
				removedNames = append(removedNames, n.QName)
			}
		}
	})
	slices.Sort(removedNames)

	deps := e.depTracker.Dependents(relPath)
	e.g.RemoveFile(relPath)
	e.depTracker.RemoveFile(relPath)
	delete(e.parsedCache, relPath)

	if e.store != nil {
		if _, err := e.store.AppendRemoveFile(relPath); err != nil {
			return ChangeSet{}, fmt.Errorf("append WAL remove %q: %w", relPath, err)
		}
	}

	var depParsed []*parser.ParsedFile
	var reResolved []string
	for _, depRel := range deps {
		if dpf, err := e.ensureParsedLocked(ctx, depRel); err == nil {
			depParsed = append(depParsed, dpf)
			reResolved = append(reResolved, depRel)
		}
	}

	if len(depParsed) > 0 {
		if err := e.reResolveAndPersistLocked(ctx, depParsed); err != nil {
			return ChangeSet{}, err
		}
	} else if e.store != nil && e.store.NeedsCompaction() {
		if _, err := e.store.Compact(e.g); err != nil {
			return ChangeSet{}, err
		}
	}

	return ChangeSet{
		File:              relPath,
		Action:            ChangeDeleted,
		RemovedSymbols:    removedNames,
		ReResolvedFiles:   reResolved,
		DependentsUpdated: len(reResolved),
		Duration:          time.Since(start),
	}, nil
}

func (e *IncrementalEngine) ensureParsedLocked(ctx context.Context, relPath string) (*parser.ParsedFile, error) {
	if pf, ok := e.parsedCache[relPath]; ok && pf != nil {
		return pf, nil
	}
	absPath := filepath.Join(e.repoRoot, filepath.FromSlash(relPath))
	src, err := os.ReadFile(absPath)
	if err != nil {
		return nil, err
	}
	lp, ok := e.preg.ForFile(relPath)
	if !ok {
		return nil, fmt.Errorf("%w: no parser for %q", parser.ErrUnsupportedLanguage, relPath)
	}
	pf, err := lp.Parse(ctx, relPath, src)
	if err != nil {
		return nil, err
	}
	e.parsedCache[relPath] = pf
	return pf, nil
}

func (e *IncrementalEngine) reResolveAndPersistLocked(ctx context.Context, files []*parser.ParsedFile) error {
	if len(files) == 0 {
		return nil
	}

	// Lazily hydrate parsedCache for direct import targets of files being re-resolved
	// so transitive re-export chasing (__init__.py / barrel index.ts / pub use) remains
	// accurate even after loading a cold snapshot on restart.
	var importTargets []string
	e.g.View(func(v *graph.ReadView) {
		seen := make(map[string]struct{})
		for _, pf := range files {
			for _, fnNode := range v.GetByQName(pf.Path) {
				if fnNode.Kind != graph.KindFile {
					continue
				}
				for _, edge := range v.GetOutgoingEdges(fnNode.UID, graph.EdgeImports) {
					if tgt, ok := v.GetNode(edge.Target); ok && tgt.Kind == graph.KindFile && tgt.File != "" {
						if _, dup := seen[tgt.File]; !dup {
							seen[tgt.File] = struct{}{}
							importTargets = append(importTargets, tgt.File)
						}
					}
				}
			}
		}
	})
	for _, impFile := range importTargets {
		_, _ = e.ensureParsedLocked(ctx, impFile)
	}

	var nameIdx *parser.NameIndex
	allOverlay := make([]*parser.ParsedFile, 0, len(e.parsedCache))
	for _, pf := range e.parsedCache {
		if pf != nil {
			allOverlay = append(allOverlay, pf)
		}
	}
	e.g.View(func(v *graph.ReadView) {
		nameIdx = pipeline.BuildViewNameIndex(v, allOverlay)
	})

	frags := make([]graph.FileFragment, 0, len(files))
	var outerErr error
	e.g.View(func(v *graph.ReadView) {
		rc := &resolver.ResolutionContext{
			View:      v,
			NameIndex: nameIdx,
			RepoRoot:  e.repoRoot,
		}
		rc.IndexFiles(allOverlay)

		for _, pf := range files {
			fr, err := e.rreg.ResolveFile(ctx, pf, rc)
			if err != nil {
				outerErr = err
				return
			}
			frag, err := pipeline.BuildResolvedFragment(pf, fr, v)
			if err != nil {
				outerErr = err
				return
			}
			frags = append(frags, frag)
		}
	})
	if outerErr != nil {
		return outerErr
	}

	for _, frag := range frags {
		if _, err := e.g.ReplaceFile(frag.File, frag.Nodes, frag.Edges, frag.CallSites); err != nil {
			return err
		}
		if e.store != nil {
			if _, err := e.store.AppendReplaceFile(frag.File, frag.Nodes, frag.Edges, frag.CallSites); err != nil {
				return fmt.Errorf("append WAL replace %q: %w", frag.File, err)
			}
		}
	}

	e.g.View(func(v *graph.ReadView) {
		for _, frag := range frags {
			e.depTracker.UpdateFileOutgoing(frag.File, v)
		}
	})

	if e.store != nil && e.store.NeedsCompaction() {
		if _, err := e.store.Compact(e.g); err != nil {
			return fmt.Errorf("compact WAL: %w", err)
		}
	}
	return nil
}

func (e *IncrementalEngine) normalizeRepoPath(pathArg string) (string, error) {
	trimmed := strings.TrimSpace(pathArg)
	if trimmed == "" {
		return "", fmt.Errorf("watcher: empty path")
	}
	if filepath.IsAbs(trimmed) {
		canonArg := trimmed
		if resolved, err := filepath.EvalSymlinks(trimmed); err == nil {
			canonArg = resolved
		}
		rel, err := filepath.Rel(e.repoRoot, canonArg)
		if err != nil {
			return "", fmt.Errorf("path %q outside repo root %q: %w", trimmed, e.repoRoot, err)
		}
		rel = filepath.ToSlash(filepath.Clean(rel))
		if strings.HasPrefix(rel, "../") || rel == ".." {
			return "", fmt.Errorf("path %q escapes repo root %q", trimmed, e.repoRoot)
		}
		return rel, nil
	}
	cleaned := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(trimmed)), "./")
	if strings.HasPrefix(cleaned, "../") || cleaned == ".." {
		return "", fmt.Errorf("path %q escapes repo root", trimmed)
	}
	return cleaned, nil
}

type symbolDiff struct {
	addedNames       []string
	addedSimpleNames []string
	removedNames     []string
	modifiedNames    []string
}

type symbolFingerprint struct {
	kind    graph.NodeKind
	sigHash graph.SigHash
	exp     bool
	typ     string
	bases   string
}

func diffFileSymbols(oldNodes, newNodes []graph.Node) symbolDiff {
	indexMap := func(nodes []graph.Node) map[string]symbolFingerprint {
		m := make(map[string]symbolFingerprint)
		for _, n := range nodes {
			if n.Kind == graph.KindFile || n.Kind == graph.KindModule || n.Kind == graph.KindExternal {
				continue
			}
			key := fmt.Sprintf("%s|%s#%d", n.Kind.String(), n.QName, n.Occurrence)
			m[key] = symbolFingerprint{
				kind:    n.Kind,
				sigHash: n.SignatureHash,
				exp:     n.Exported,
				typ:     n.Type,
				bases:   strings.Join(n.Bases, ","),
			}
		}
		return m
	}

	oldMap := indexMap(oldNodes)
	newMap := indexMap(newNodes)
	newSimpleByKey := make(map[string]string, len(newNodes))
	for _, n := range newNodes {
		if n.Kind == graph.KindFile || n.Kind == graph.KindModule || n.Kind == graph.KindExternal {
			continue
		}
		key := fmt.Sprintf("%s|%s#%d", n.Kind.String(), n.QName, n.Occurrence)
		newSimpleByKey[key] = n.Name
	}

	var out symbolDiff
	for key, nf := range newMap {
		qname := extractQNameFromDiffKey(key)
		of, exists := oldMap[key]
		if !exists {
			out.addedNames = append(out.addedNames, qname)
			if s := newSimpleByKey[key]; s != "" {
				out.addedSimpleNames = append(out.addedSimpleNames, s)
			}
			continue
		}
		if of.sigHash != nf.sigHash || of.exp != nf.exp || of.typ != nf.typ || of.bases != nf.bases {
			out.modifiedNames = append(out.modifiedNames, qname)
			if !of.exp && nf.exp {
				if s := newSimpleByKey[key]; s != "" {
					out.addedSimpleNames = append(out.addedSimpleNames, s)
				}
			}
		}
	}
	for key := range oldMap {
		if _, exists := newMap[key]; !exists {
			out.removedNames = append(out.removedNames, extractQNameFromDiffKey(key))
		}
	}
	slices.Sort(out.addedNames)
	slices.Sort(out.addedSimpleNames)
	slices.Sort(out.removedNames)
	slices.Sort(out.modifiedNames)
	return out
}

func extractQNameFromDiffKey(key string) string {
	bar := strings.IndexByte(key, '|')
	hash := strings.LastIndexByte(key, '#')
	if bar >= 0 && hash > bar {
		return key[bar+1 : hash]
	}
	return key
}
