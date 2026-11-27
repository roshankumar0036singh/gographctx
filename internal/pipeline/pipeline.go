package pipeline

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/roshankumar0036singh/gographctx/internal/config"
	"github.com/roshankumar0036singh/gographctx/internal/graph"
	"github.com/roshankumar0036singh/gographctx/internal/parser"
	parserlang "github.com/roshankumar0036singh/gographctx/internal/parser/lang"
	"github.com/roshankumar0036singh/gographctx/internal/parser/treesitter"
	"github.com/roshankumar0036singh/gographctx/internal/resolver"
	resolverlang "github.com/roshankumar0036singh/gographctx/internal/resolver/lang"
	"github.com/roshankumar0036singh/gographctx/internal/storage"
)

const manifestFileName = "manifest.json"

// IndexOptions controls full vs smart incremental indexing and storage location.
type IndexOptions struct {
	Force    bool   // Force full re-index even when mtime+size match previous manifest
	StoreDir string // Override storage directory (defaults to <repo>/<cfg.DataDir>)
}

// StageTimings captures wall-clock duration for each pipeline stage.
type StageTimings struct {
	Discover time.Duration `json:"discover"`
	Load     time.Duration `json:"load,omitempty"`
	Parse    time.Duration `json:"parse"`
	Build    time.Duration `json:"build"`
	Resolve  time.Duration `json:"resolve"`
	Finalize time.Duration `json:"finalize"`
	Persist  time.Duration `json:"persist"`
	Total    time.Duration `json:"total"`
}

// FormatSummary returns a clean human-readable stage timing and graph summary table.
func (r *IndexResult) FormatSummary() string {
	if r == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "GoGraphCtx Index Summary (%s)\n", r.RepoRoot)
	fmt.Fprintf(&b, "  Discover : %v (%d files found, %d ignored)\n",
		r.Timings.Discover.Round(time.Millisecond), r.Stats.TotalFiles, r.Discovery.Stats.IgnoredFiles)
	fmt.Fprintf(&b, "  Parse    : %v (%d parsed, %d skipped unchanged)\n",
		r.Timings.Parse.Round(time.Millisecond), r.Stats.IndexedFiles, r.Stats.SkippedFiles)
	fmt.Fprintf(&b, "  Build    : %v (%d nodes)\n",
		r.Timings.Build.Round(time.Millisecond), r.Stats.NodeCount)
	fmt.Fprintf(&b, "  Resolve  : %v (%d edges, %d call sites)\n",
		r.Timings.Resolve.Round(time.Millisecond), r.Stats.EdgeCount, r.Stats.CallSiteCount)
	fmt.Fprintf(&b, "  Persist  : %v -> %s\n",
		r.Timings.Persist.Round(time.Millisecond), r.SnapshotPath)
	fmt.Fprintf(&b, "  Total    : %v (errors: %d)\n",
		r.Timings.Total.Round(time.Millisecond), len(r.Errors))
	return b.String()
}

// IndexStats summarizes files, graph counts, and resolution coverage.
type IndexStats struct {
	TotalFiles      int `json:"total_files"`
	IndexedFiles    int `json:"indexed_files"`
	SkippedFiles    int `json:"skipped_files"`
	RemovedFiles    int `json:"removed_files"`
	NodeCount       int `json:"node_count"`
	EdgeCount       int `json:"edge_count"`
	CallSiteCount   int `json:"call_site_count"`
	StrongCallEdges int `json:"strong_call_edges"`
	LikelyCallEdges int `json:"likely_call_edges"`
}

// IndexResult captures the indexed Graph, stage timings, counts, and non-fatal file errors.
type IndexResult struct {
	RepoRoot     string
	StoreDir     string
	SnapshotPath string
	ManifestPath string
	Graph        *graph.Graph
	Discovery    *DiscoveryResult
	Timings      StageTimings
	Stats        IndexStats
	Errors       []error
}

// FileManifestRecord records cached metadata used by smart-mode change detection.
type FileManifestRecord struct {
	Size            int64  `json:"size"`
	ModTimeUnixNano int64  `json:"mod_time_unix_nano"`
	Language        string `json:"language"`
}

// FileManifest tracks indexed file metadata persisted next to index.ggctx.
type FileManifest struct {
	Version int                           `json:"version"`
	Files   map[string]FileManifestRecord `json:"files"`
}

// DefaultParserRegistry constructs a parser Registry supporting all core languages.
func DefaultParserRegistry(mgr *treesitter.Manager) (*parser.Registry, error) {
	if mgr == nil {
		return nil, fmt.Errorf("pipeline: nil tree-sitter manager")
	}
	reg := parser.NewRegistry()
	if _, err := parserlang.RegisterGo(reg, mgr); err != nil {
		return nil, err
	}
	if _, err := parserlang.RegisterPython(reg, mgr); err != nil {
		return nil, err
	}
	if _, err := parserlang.RegisterTypeScript(reg, mgr); err != nil {
		return nil, err
	}
	if _, err := parserlang.RegisterTSX(reg, mgr); err != nil {
		return nil, err
	}
	if _, err := parserlang.RegisterJavaScript(reg, mgr); err != nil {
		return nil, err
	}
	if _, err := parserlang.RegisterRust(reg, mgr); err != nil {
		return nil, err
	}
	if _, err := parserlang.RegisterJava(reg, mgr); err != nil {
		return nil, err
	}
	return reg, nil
}

// DefaultResolverRegistry constructs a resolver Registry supporting all core languages.
func DefaultResolverRegistry() (*resolver.Registry, error) {
	reg := resolver.NewRegistry()
	for _, lr := range []resolver.LanguageResolver{
		resolverlang.NewGoResolver(),
		resolverlang.NewPythonResolver(),
		resolverlang.NewTypeScriptResolver(),
		resolverlang.NewTSXResolver(),
		resolverlang.NewJavaScriptResolver(),
		resolverlang.NewRustResolver(),
		resolverlang.NewJavaResolver(),
	} {
		if err := reg.Register(lr); err != nil {
			return nil, err
		}
	}
	return reg, nil
}

// IndexRepository orchestrates full or smart-skip repository indexing:
// discover -> load/diff manifest -> parse -> skeleton build -> resolve -> finalize -> persist.
func IndexRepository(ctx context.Context, repoPath string, cfg config.Config, opts IndexOptions) (*IndexResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(repoPath) == "" {
		return nil, fmt.Errorf("pipeline: empty repository path")
	}

	totalStart := time.Now()
	absRoot, err := filepath.Abs(repoPath)
	if err != nil {
		return nil, fmt.Errorf("resolve repository path: %w", err)
	}

	if cfg.Workers <= 0 {
		cfg = config.DefaultConfig()
	}

	storeDir := opts.StoreDir
	if strings.TrimSpace(storeDir) == "" {
		dataDir := strings.TrimSpace(cfg.DataDir)
		if dataDir == "" {
			dataDir = config.DefaultDataDirName
		}
		if filepath.IsAbs(dataDir) {
			storeDir = dataDir
		} else {
			storeDir = filepath.Join(absRoot, dataDir)
		}
	}
	manifestPath := filepath.Join(storeDir, manifestFileName)

	mgr, err := treesitter.NewManager()
	if err != nil {
		return nil, fmt.Errorf("init tree-sitter manager: %w", err)
	}
	defer mgr.Close()

	preg, err := DefaultParserRegistry(mgr)
	if err != nil {
		return nil, fmt.Errorf("init parser registry: %w", err)
	}
	rreg, err := DefaultResolverRegistry()
	if err != nil {
		return nil, fmt.Errorf("init resolver registry: %w", err)
	}

	// Stage 1: File Discovery
	discStart := time.Now()
	disc, err := DiscoverFiles(ctx, absRoot, cfg, preg)
	if err != nil {
		return nil, err
	}
	timings := StageTimings{Discover: time.Since(discStart)}

	res := &IndexResult{
		RepoRoot:     absRoot,
		StoreDir:     storeDir,
		SnapshotPath: filepath.Join(storeDir, storage.SnapshotFileName),
		ManifestPath: manifestPath,
		Discovery:    disc,
		Stats: IndexStats{
			TotalFiles: len(disc.Files),
		},
	}

	snapOpts := storage.WriteOptions{Compress: cfg.CompressSnapshots, Sync: cfg.SyncWrites}
	walOpts := storage.WALOptions{Sync: cfg.SyncWrites, CompactThresholdBytes: cfg.WALCompactThresholdBytes}

	// Stage 2: Check smart-mode skip using prior snapshot + manifest.
	var prevGraph *graph.Graph
	var prevManifest FileManifest
	hasPrev := false
	if !opts.Force {
		loadStart := time.Now()
		if _, statErr := os.Stat(res.SnapshotPath); statErr == nil {
			store, loadedG, loadStats, loadErr := storage.LoadStore(storeDir, snapOpts, walOpts, storage.LoadOptions{
				AllowTrailingPartialWAL: true,
			})
			if loadErr == nil && loadStats.HasSnapshot {
				_ = store.Close()
				if mf, mfErr := loadManifest(manifestPath); mfErr == nil {
					prevGraph = loadedG
					prevManifest = mf
					hasPrev = true
				}
			} else if store != nil {
				_ = store.Close()
			}
		}
		timings.Load = time.Since(loadStart)
	}

	toIndex := disc.Files
	var unchanged []DiscoveredFile
	var deletedPaths []string

	if hasPrev {
		toIndex = make([]DiscoveredFile, 0, len(disc.Files))
		seenNow := make(map[string]struct{}, len(disc.Files))
		for _, df := range disc.Files {
			seenNow[df.RelPath] = struct{}{}
			if rec, ok := prevManifest.Files[df.RelPath]; ok &&
				rec.Size == df.Size &&
				rec.ModTimeUnixNano == df.ModTimeUnixNano &&
				rec.Language == df.Language {
				unchanged = append(unchanged, df)
				continue
			}
			toIndex = append(toIndex, df)
		}
		for prevPath := range prevManifest.Files {
			if _, ok := seenNow[prevPath]; !ok {
				deletedPaths = append(deletedPaths, prevPath)
			}
		}
		slices.Sort(deletedPaths)

		// Fast path: 100% unchanged repository.
		if len(toIndex) == 0 && len(deletedPaths) == 0 {
			res.Graph = prevGraph
			res.Stats.SkippedFiles = len(unchanged)
			populateGraphStats(&res.Stats, prevGraph)
			timings.Total = time.Since(totalStart)
			res.Timings = timings
			return res, nil
		}
	}

	res.Stats.SkippedFiles = len(unchanged)
	res.Stats.RemovedFiles = len(deletedPaths)

	// Collect affected files in smart incremental mode: unchanged files that depend on
	// changed or deleted files are re-parsed and re-resolved cleanly along with toIndex.
	smartIncremental := hasPrev && len(unchanged) > 0 && len(toIndex)+len(deletedPaths) < len(disc.Files)/2
	if smartIncremental {
		g, fileErrs, err := indexIncrementalDelta(ctx, absRoot, cfg, preg, rreg, prevGraph, toIndex, unchanged, deletedPaths, &timings)
		if err != nil {
			return nil, err
		}
		res.Graph = g
		res.Errors = append(res.Errors, fileErrs...)
		res.Stats.IndexedFiles = len(toIndex)
	} else {
		// Full repository parse -> skeleton -> resolve -> finalize.
		toIndex = disc.Files
		res.Stats.SkippedFiles = 0
		g, fileErrs, err := indexFullBatch(ctx, absRoot, cfg, preg, rreg, toIndex, &timings)
		if err != nil {
			return nil, err
		}
		res.Graph = g
		res.Errors = append(res.Errors, fileErrs...)
		res.Stats.IndexedFiles = len(toIndex)
	}

	// Stage 6: Persist atomic snapshot (.ggctx/index.ggctx) + manifest.json.
	persistStart := time.Now()
	store, err := storage.OpenStore(storeDir, snapOpts, walOpts)
	if err != nil {
		return nil, fmt.Errorf("open store %q: %w", storeDir, err)
	}
	if _, err := store.Compact(res.Graph); err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("compact snapshot: %w", err)
	}
	if err := store.Close(); err != nil {
		return nil, fmt.Errorf("close store: %w", err)
	}

	nextManifest := buildManifest(disc.Files)
	if err := saveManifest(manifestPath, nextManifest); err != nil {
		return nil, fmt.Errorf("save manifest: %w", err)
	}
	timings.Persist = time.Since(persistStart)

	populateGraphStats(&res.Stats, res.Graph)
	timings.Total = time.Since(totalStart)
	res.Timings = timings
	return res, nil
}

func indexFullBatch(
	ctx context.Context,
	repoRoot string,
	cfg config.Config,
	preg *parser.Registry,
	rreg *resolver.Registry,
	files []DiscoveredFile,
	timings *StageTimings,
) (*graph.Graph, []error, error) {
	// Parse discovered files in parallel via Coordinator.
	parseStart := time.Now()
	inputs, readErrs := readDiscoveredFileInputs(files)
	coord, err := parser.NewCoordinator(preg, parser.CoordinatorConfig{
		Workers:           cfg.Workers,
		FailFast:          false,
		DeterministicSort: true,
	})
	if err != nil {
		return nil, nil, err
	}
	batch, err := coord.ParseFiles(ctx, inputs)
	if err != nil {
		return nil, nil, err
	}
	timings.Parse = time.Since(parseStart)

	var errs []error
	errs = append(errs, readErrs...)
	for _, fe := range batch.Errors {
		errs = append(errs, fe)
	}

	// Build structural skeleton graph.
	buildStart := time.Now()
	skelGraph, err := BuildGraph(ctx, batch.Files, nil)
	if err != nil {
		return nil, nil, err
	}
	timings.Build = time.Since(buildStart)

	// Resolve imports, types, calls, and links concurrently under read view.
	resolveStart := time.Now()
	resolvedMap, resErrs, err := resolveFilesParallel(ctx, rreg, repoRoot, batch.Files, batch.NameIndex, skelGraph, cfg.Workers)
	if err != nil {
		return nil, nil, err
	}
	errs = append(errs, resErrs...)
	timings.Resolve = time.Since(resolveStart)

	// Finalize fragments into complete graph.
	finStart := time.Now()
	finalGraph, err := BuildGraph(ctx, batch.Files, resolvedMap)
	if err != nil {
		return nil, nil, err
	}
	timings.Finalize = time.Since(finStart)

	return finalGraph, errs, nil
}

func indexIncrementalDelta(
	ctx context.Context,
	repoRoot string,
	cfg config.Config,
	preg *parser.Registry,
	rreg *resolver.Registry,
	g *graph.Graph,
	changed []DiscoveredFile,
	unchanged []DiscoveredFile,
	deletedPaths []string,
	timings *StageTimings,
) (*graph.Graph, []error, error) {
	// Identify unchanged dependents BEFORE removing/updating changed/deleted files.
	depSet := make(map[string]struct{})
	unchangedByRel := make(map[string]DiscoveredFile, len(unchanged))
	for _, u := range unchanged {
		unchangedByRel[u.RelPath] = u
	}

	g.View(func(v *graph.ReadView) {
		collectDependents := func(rel string) {
			for _, n := range v.GetByFile(rel) {
				for _, depFile := range v.GetDependentFiles(n.UID) {
					if _, ok := unchangedByRel[depFile]; ok {
						depSet[depFile] = struct{}{}
					}
				}
			}
			// Check negative call sites matching basename or short symbol names from updated files.
			for _, n := range v.GetByFile(rel) {
				if n.Name != "" {
					for _, cs := range v.GetCallSitesByLookupKey(n.Name) {
						if _, ok := unchangedByRel[cs.File]; ok {
							depSet[cs.File] = struct{}{}
						}
					}
				}
			}
		}
		for _, c := range changed {
			collectDependents(c.RelPath)
		}
		for _, d := range deletedPaths {
			collectDependents(d)
		}
	})

	for _, d := range deletedPaths {
		g.RemoveFile(d)
	}

	reparseList := slices.Clone(changed)
	for depRel := range depSet {
		reparseList = append(reparseList, unchangedByRel[depRel])
	}
	slices.SortFunc(reparseList, func(a, b DiscoveredFile) int {
		return cmp.Compare(a.RelPath, b.RelPath)
	})

	parseStart := time.Now()
	inputs, readErrs := readDiscoveredFileInputs(reparseList)
	coord, err := parser.NewCoordinator(preg, parser.CoordinatorConfig{
		Workers:           cfg.Workers,
		FailFast:          false,
		DeterministicSort: true,
	})
	if err != nil {
		return nil, nil, err
	}
	batch, err := coord.ParseFiles(ctx, inputs)
	if err != nil {
		return nil, nil, err
	}
	timings.Parse = time.Since(parseStart)

	var errs []error
	errs = append(errs, readErrs...)
	for _, fe := range batch.Errors {
		errs = append(errs, fe)
	}

	// Update structural nodes in g for all re-parsed files first so ReadView sees new symbols.
	buildStart := time.Now()
	for _, pf := range batch.Files {
		symFrag, err := BuildSymbolFragment(pf)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := g.ReplaceFile(symFrag.File, symFrag.Nodes, symFrag.Edges, nil); err != nil {
			return nil, nil, err
		}
	}
	timings.Build = time.Since(buildStart)

	// Reconstruct full NameIndex across g + parsed files for cross-file resolution.
	var allParsed []*parser.ParsedFile
	g.View(func(v *graph.ReadView) {
		allParsed = synthesizeIndexInputsFromView(v, batch.Files)
	})
	fullNameIndex := parser.BuildNameIndex(allParsed)

	resolveStart := time.Now()
	resolvedMap, resErrs, err := resolveFilesParallel(ctx, rreg, repoRoot, batch.Files, fullNameIndex, g, cfg.Workers)
	if err != nil {
		return nil, nil, err
	}
	errs = append(errs, resErrs...)
	timings.Resolve = time.Since(resolveStart)

	finStart := time.Now()
	var frags []graph.FileFragment
	var buildErr error
	g.View(func(v *graph.ReadView) {
		for _, pf := range batch.Files {
			frag, err := BuildResolvedFragment(pf, resolvedMap[pf.Path], v)
			if err != nil {
				buildErr = err
				return
			}
			frags = append(frags, frag)
		}
	})
	if buildErr != nil {
		return nil, nil, buildErr
	}
	for _, frag := range frags {
		if _, err := g.ReplaceFile(frag.File, frag.Nodes, frag.Edges, frag.CallSites); err != nil {
			return nil, nil, err
		}
	}
	timings.Finalize = time.Since(finStart)
	return g, errs, nil
}

func resolveFilesParallel(
	ctx context.Context,
	rreg *resolver.Registry,
	repoRoot string,
	files []*parser.ParsedFile,
	nameIdx *parser.NameIndex,
	skelGraph *graph.Graph,
	workers int,
) (map[string]*resolver.FileResolution, []error, error) {
	if workers <= 0 {
		workers = runtime.NumCPU()
		if workers < 1 {
			workers = 1
		}
	}
	out := make(map[string]*resolver.FileResolution, len(files))
	if len(files) == 0 {
		return out, nil, nil
	}

	var errs []error
	var mu sync.Mutex
	var outerErr error

	skelGraph.View(func(v *graph.ReadView) {
		rc := &resolver.ResolutionContext{
			View:      v,
			NameIndex: nameIdx,
			RepoRoot:  repoRoot,
		}
		rc.IndexFiles(files)

		g, gctx := errgroup.WithContext(ctx)
		g.SetLimit(workers)

		for _, pf := range files {
			if pf == nil {
				continue
			}
			pf := pf
			g.Go(func() error {
				if err := gctx.Err(); err != nil {
					return err
				}
				fr, err := rreg.ResolveFile(gctx, pf, rc)
				if err != nil {
					if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
						return err
					}
					mu.Lock()
					errs = append(errs, fmt.Errorf("resolve %q: %w", pf.Path, err))
					mu.Unlock()
					return nil
				}
				mu.Lock()
				out[pf.Path] = fr
				mu.Unlock()
				return nil
			})
		}
		outerErr = g.Wait()
	})
	if outerErr != nil {
		return nil, errs, outerErr
	}
	return out, errs, nil
}

func synthesizeIndexInputsFromView(v *graph.ReadView, reParsed []*parser.ParsedFile) []*parser.ParsedFile {
	byPath := make(map[string]*parser.ParsedFile)
	for _, pf := range reParsed {
		if pf != nil {
			byPath[pf.Path] = pf
		}
	}
	if v == nil {
		out := make([]*parser.ParsedFile, 0, len(byPath))
		for _, pf := range byPath {
			out = append(out, pf)
		}
		return out
	}
	for _, n := range v.AllNodes() {
		if n.File == "" {
			continue
		}
		if _, exists := byPath[n.File]; exists {
			continue
		}
		pf := &parser.ParsedFile{
			Path:     n.File,
			Language: n.Language,
		}
		byPath[n.File] = pf
	}
	// Populate minimal symbol/package info on unchanged synthesized files so NameIndex finds them.
	for pathKey, pf := range byPath {
		isParsed := false
		for _, rp := range reParsed {
			if rp != nil && rp.Path == pathKey {
				isParsed = true
				break
			}
		}
		if isParsed {
			continue
		}
		for _, n := range v.GetByFile(pathKey) {
			switch n.Kind {
			case graph.KindModule:
				pf.Package = n.Name
			case graph.KindFunction:
				pf.Functions = append(pf.Functions, parser.ParsedFunction{
					Name:       n.Name,
					QName:      n.QName,
					StartLine:  n.StartLine,
					Exported:   n.Exported,
					ReturnType: n.ReturnType,
				})
			case graph.KindClass, graph.KindStruct, graph.KindInterface, graph.KindEnum:
				pf.Classes = append(pf.Classes, parser.ParsedClass{
					Name:      n.Name,
					QName:     n.QName,
					Kind:      n.Kind,
					StartLine: n.StartLine,
					Exported:  n.Exported,
					Bases:     n.Bases,
				})
			case graph.KindVariable:
				pf.Variables = append(pf.Variables, parser.ParsedVariable{
					Name:      n.Name,
					QName:     n.QName,
					Type:      n.Type,
					StartLine: n.StartLine,
					Exported:  n.Exported,
				})
			case graph.KindTypeAlias:
				pf.TypeAliases = append(pf.TypeAliases, parser.ParsedTypeAlias{
					Name:           n.Name,
					QName:          n.QName,
					UnderlyingType: n.Type,
					StartLine:      n.StartLine,
					Exported:       n.Exported,
				})
			}
		}
	}
	out := make([]*parser.ParsedFile, 0, len(byPath))
	for _, pf := range byPath {
		out = append(out, pf)
	}
	slices.SortFunc(out, func(a, b *parser.ParsedFile) int {
		return cmp.Compare(a.Path, b.Path)
	})
	return out
}

func readDiscoveredFileInputs(files []DiscoveredFile) ([]parser.FileInput, []error) {
	inputs := make([]parser.FileInput, 0, len(files))
	var errs []error
	for _, df := range files {
		raw, err := os.ReadFile(df.AbsPath)
		if err != nil {
			errs = append(errs, fmt.Errorf("read %q: %w", df.RelPath, err))
			continue
		}
		inputs = append(inputs, parser.FileInput{
			Path:     df.RelPath,
			Language: df.Language,
			Source:   raw,
		})
	}
	return inputs, errs
}

func populateGraphStats(stats *IndexStats, g *graph.Graph) {
	if stats == nil || g == nil {
		return
	}
	g.View(func(v *graph.ReadView) {
		stats.NodeCount = v.NodeCount()
		stats.EdgeCount = v.EdgeCount()
		stats.CallSiteCount = v.CallSiteCount()
		var strong, likely int
		for _, e := range v.AllEdges() {
			switch e.Kind {
			case graph.EdgeCalls:
				strong++
			case graph.EdgeLikelyCalls:
				likely++
			}
		}
		stats.StrongCallEdges = strong
		stats.LikelyCallEdges = likely
	})
}

func buildManifest(files []DiscoveredFile) FileManifest {
	m := FileManifest{
		Version: 1,
		Files:   make(map[string]FileManifestRecord, len(files)),
	}
	for _, df := range files {
		m.Files[df.RelPath] = FileManifestRecord{
			Size:            df.Size,
			ModTimeUnixNano: df.ModTimeUnixNano,
			Language:        df.Language,
		}
	}
	return m
}

func loadManifest(path string) (FileManifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return FileManifest{}, err
	}
	var m FileManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return FileManifest{}, err
	}
	if m.Files == nil {
		m.Files = make(map[string]FileManifestRecord)
	}
	return m, nil
}

func saveManifest(path string, m FileManifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
