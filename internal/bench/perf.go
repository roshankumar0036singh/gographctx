package bench

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/roshankumar0036singh/gographctx/internal/config"
	"github.com/roshankumar0036singh/gographctx/internal/graph"
	"github.com/roshankumar0036singh/gographctx/internal/mcp"
	"github.com/roshankumar0036singh/gographctx/internal/pipeline"
	"github.com/roshankumar0036singh/gographctx/internal/storage"
	"github.com/roshankumar0036singh/gographctx/internal/watcher"
)

// SLATargets defines GoGraphCtx's Phase 30 performance and accuracy ceilings.
type SLATargets struct {
	MaxFlaskIndexMs        float64 `json:"max_flask_index_ms"`
	MaxFlaskRSSMB          float64 `json:"max_flask_rss_mb"`
	MaxGo1kIndexMs         float64 `json:"max_go1k_index_ms"`
	MaxGo1kRSSMB           float64 `json:"max_go1k_rss_mb"`
	MaxIncrementalBodyMs   float64 `json:"max_incremental_body_ms"`
	MaxIncrementalExportMs float64 `json:"max_incremental_export_ms"`
	MaxMCPQueryP95Ms       float64 `json:"max_mcp_query_p95_ms"`
	MaxCLIStartupMs        float64 `json:"max_cli_startup_ms"`
	MaxBinarySizeMB        float64 `json:"max_binary_size_mb"`
}

// DefaultSLATargets returns the targets documented in CONTEXT.md Phase 30.
func DefaultSLATargets() SLATargets {
	return SLATargets{
		MaxFlaskIndexMs:        1000,
		MaxFlaskRSSMB:          80,
		MaxGo1kIndexMs:         3000,
		MaxGo1kRSSMB:           150,
		MaxIncrementalBodyMs:   50,
		MaxIncrementalExportMs: 100,
		MaxMCPQueryP95Ms:       10,
		MaxCLIStartupMs:        50,
		MaxBinarySizeMB:        30,
	}
}

// RepoIndexMetric captures performance and graph quality telemetry for one full index run.
type RepoIndexMetric struct {
	Corpus        string  `json:"corpus"`
	Files         int     `json:"files"`
	DurationMs    float64 `json:"duration_ms"`
	PeakRSSMB     float64 `json:"peak_rss_mb"`
	Nodes         int     `json:"nodes"`
	Edges         int     `json:"edges"`
	CallSites     int     `json:"call_sites"`
	StrongTierPct float64 `json:"strong_tier_pct"`
	SnapshotBytes int64   `json:"snapshot_bytes"`
}

// LatencySummary summarizes median/p95/p99 latencies in milliseconds.
type LatencySummary struct {
	Iterations int     `json:"iterations"`
	P50Ms      float64 `json:"p50_ms"`
	P95Ms      float64 `json:"p95_ms"`
	P99Ms      float64 `json:"p99_ms"`
	MaxMs      float64 `json:"max_ms"`
}

// PerfSuiteResult collects all GoGraphCtx internal benchmark results.
type PerfSuiteResult struct {
	Timestamp         string          `json:"timestamp"`
	Flask             RepoIndexMetric `json:"flask"`
	Go1k              RepoIndexMetric `json:"go_1k"`
	IncrementalBody   LatencySummary  `json:"incremental_body"`
	IncrementalExport LatencySummary  `json:"incremental_export"`
	MCPQueries        LatencySummary  `json:"mcp_queries"`
	CLIStartupMs      float64         `json:"cli_startup_ms,omitempty"`
	BinarySizeMB      float64         `json:"binary_size_mb,omitempty"`
}

// CheckSLAs verifies PerfSuiteResult against targets and returns any breach descriptions.
func CheckSLAs(res PerfSuiteResult, targets SLATargets) []string {
	var breaches []string
	if res.Flask.DurationMs > targets.MaxFlaskIndexMs {
		breaches = append(breaches, fmt.Sprintf("Flask index %.2fms > %.2fms", res.Flask.DurationMs, targets.MaxFlaskIndexMs))
	}
	if res.Flask.PeakRSSMB > targets.MaxFlaskRSSMB {
		breaches = append(breaches, fmt.Sprintf("Flask peak RSS %.2fMB > %.2fMB", res.Flask.PeakRSSMB, targets.MaxFlaskRSSMB))
	}
	if res.Go1k.DurationMs > targets.MaxGo1kIndexMs {
		breaches = append(breaches, fmt.Sprintf("Go-1k index %.2fms > %.2fms", res.Go1k.DurationMs, targets.MaxGo1kIndexMs))
	}
	if res.Go1k.PeakRSSMB > targets.MaxGo1kRSSMB {
		breaches = append(breaches, fmt.Sprintf("Go-1k peak RSS %.2fMB > %.2fMB", res.Go1k.PeakRSSMB, targets.MaxGo1kRSSMB))
	}
	if res.IncrementalBody.P50Ms > targets.MaxIncrementalBodyMs {
		breaches = append(breaches, fmt.Sprintf("Incremental body p50 %.2fms > %.2fms", res.IncrementalBody.P50Ms, targets.MaxIncrementalBodyMs))
	}
	if res.IncrementalExport.P50Ms > targets.MaxIncrementalExportMs {
		breaches = append(breaches, fmt.Sprintf("Incremental export p50 %.2fms > %.2fms", res.IncrementalExport.P50Ms, targets.MaxIncrementalExportMs))
	}
	if res.MCPQueries.P95Ms > targets.MaxMCPQueryP95Ms {
		breaches = append(breaches, fmt.Sprintf("MCP query p95 %.2fms > %.2fms", res.MCPQueries.P95Ms, targets.MaxMCPQueryP95Ms))
	}
	if res.CLIStartupMs > 0 && res.CLIStartupMs > targets.MaxCLIStartupMs {
		breaches = append(breaches, fmt.Sprintf("CLI startup %.2fms > %.2fms", res.CLIStartupMs, targets.MaxCLIStartupMs))
	}
	if res.BinarySizeMB > 0 && res.BinarySizeMB > targets.MaxBinarySizeMB {
		breaches = append(breaches, fmt.Sprintf("Binary size %.2fMB > %.2fMB", res.BinarySizeMB, targets.MaxBinarySizeMB))
	}
	return breaches
}

// RunPerfSuite executes the Phase 30 GoGraphCtx benchmark suite inside workDir.
// If cliBinary is non-empty, CLI cold-startup time and binary size are also measured.
// If flaskRepoDir is non-empty, it indexes that directory; otherwise it generates an 83-file Flask corpus.
func RunPerfSuite(ctx context.Context, workDir, cliBinary, flaskRepoDir string) (PerfSuiteResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	res := PerfSuiteResult{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	// 1. Flask benchmark (83 .py files)
	flaskDir := flaskRepoDir
	if flaskDir == "" {
		flaskDir = filepath.Join(workDir, "flask-83")
		if err := GenerateFlaskScalePythonCorpus(flaskDir, 83); err != nil {
			return res, fmt.Errorf("generate flask corpus: %w", err)
		}
	}
	flaskGraph, flaskMetric, err := measureRepoIndex(ctx, "flask-83", flaskDir, cliBinary)
	if err != nil {
		return res, fmt.Errorf("benchmark flask index: %w", err)
	}
	res.Flask = flaskMetric

	// 2. Incremental body + export benchmarks on the indexed Flask repo
	bodyLat, exportLat, err := measureIncrementalLatencies(ctx, flaskDir)
	if err != nil {
		return res, fmt.Errorf("benchmark incremental updates: %w", err)
	}
	res.IncrementalBody = bodyLat
	res.IncrementalExport = exportLat

	// 3. MCP tool latency benchmark against the live Flask graph
	res.MCPQueries = measureMCPLatency(ctx, flaskDir, flaskGraph)

	// 4. 1000-file Go monorepo benchmark
	go1kDir := filepath.Join(workDir, "go-1k")
	if err := GenerateGoMonorepoCorpus(go1kDir, 1000); err != nil {
		return res, fmt.Errorf("generate go-1k corpus: %w", err)
	}
	_, go1kMetric, err := measureRepoIndex(ctx, "go-1000", go1kDir, cliBinary)
	if err != nil {
		return res, fmt.Errorf("benchmark go-1k index: %w", err)
	}
	res.Go1k = go1kMetric

	// 5. CLI startup + binary size
	if strings.TrimSpace(cliBinary) != "" {
		if st, err := os.Stat(cliBinary); err == nil {
			res.BinarySizeMB = float64(st.Size()) / (1024 * 1024)
		}
		res.CLIStartupMs = measureCLIStartupMs(ctx, cliBinary, 15)
	}

	return res, nil
}

func measureRepoIndex(ctx context.Context, corpusName, repoDir, cliBinary string) (*graph.Graph, RepoIndexMetric, error) {
	cfg := config.DefaultConfig()
	var peakBytes uint64
	var dur time.Duration
	var g *graph.Graph
	storeDir := filepath.Join(repoDir, config.DefaultDataDirName)
	fileCount := 0

	if strings.TrimSpace(cliBinary) != "" {
		start := time.Now()
		childPeak, out, err := RunMeasuredCapture(ctx, repoDir, 120*time.Second, cliBinary, "index", "--force", ".")
		dur = time.Since(start)
		if err != nil {
			return nil, RepoIndexMetric{}, fmt.Errorf("cli index failed: %w (%s)", err, out)
		}
		peakBytes = childPeak
		st, loadedG, _, loadErr := storage.LoadStore(storeDir, storage.WriteOptions{Compress: cfg.CompressSnapshots}, storage.WALOptions{}, storage.LoadOptions{AllowTrailingPartialWAL: true})
		if loadErr != nil {
			return nil, RepoIndexMetric{}, loadErr
		}
		_ = st.Close()
		g = loadedG
		g.View(func(v *graph.ReadView) {
			fileCount = v.Stats().FileCount
		})
	} else {
		startInProc := time.Now()
		idxRes, err := pipeline.IndexRepository(ctx, repoDir, cfg, pipeline.IndexOptions{Force: true})
		dur = time.Since(startInProc)
		if err != nil {
			return nil, RepoIndexMetric{}, err
		}
		peakBytes = CurrentPeakRSSBytes()
		g = idxRes.Graph
		storeDir = idxRes.StoreDir
		fileCount = idxRes.Stats.IndexedFiles
	}

	var snapSize int64
	if st, statErr := os.Stat(filepath.Join(storeDir, storage.SnapshotFileName)); statErr == nil {
		snapSize = st.Size()
	}

	var nodeCount, edgeCount, callSiteCount, certainCalls, likelyCalls int
	g.View(func(v *graph.ReadView) {
		nodeCount = v.NodeCount()
		edgeCount = v.EdgeCount()
		callSiteCount = v.CallSiteCount()
		for _, e := range v.AllEdges() {
			if e.Kind == graph.EdgeCalls {
				certainCalls++
			} else if e.Kind == graph.EdgeLikelyCalls {
				likelyCalls++
			}
		}
	})
	strongPct := 0.0
	if total := certainCalls + likelyCalls; total > 0 {
		strongPct = float64(certainCalls) * 100.0 / float64(total)
	}

	return g, RepoIndexMetric{
		Corpus:        corpusName,
		Files:         fileCount,
		DurationMs:    float64(dur.Microseconds()) / 1000.0,
		PeakRSSMB:     float64(peakBytes) / (1024 * 1024),
		Nodes:         nodeCount,
		Edges:         edgeCount,
		CallSites:     callSiteCount,
		StrongTierPct: strongPct,
		SnapshotBytes: snapSize,
	}, nil
}

func measureIncrementalLatencies(ctx context.Context, flaskDir string) (LatencySummary, LatencySummary, error) {
	cfg := config.DefaultConfig()
	eng, err := watcher.NewIncrementalEngine(ctx, flaskDir, cfg, pipeline.IndexOptions{})
	if err != nil {
		return LatencySummary{}, LatencySummary{}, err
	}
	defer func() { _ = eng.Close() }()

	helpersAbs := filepath.Join(flaskDir, "app", "helpers.py")
	origBytes, err := os.ReadFile(helpersAbs)
	if err != nil {
		return LatencySummary{}, LatencySummary{}, err
	}
	defer func() { _ = os.WriteFile(helpersAbs, origBytes, 0o600) }()

	const iters = 7
	bodySamples := make([]float64, 0, iters)
	exportSamples := make([]float64, 0, iters)

	for i := 0; i < iters; i++ {
		bodySrc := fmt.Sprintf(`def clean_input(raw: str) -> str:
    return raw.strip() + "%d"

def sign_token(payload: str) -> str:
    return f"sig:{payload}"

def audit_event(name: str, detail: str) -> str:
    cleaned = clean_input(detail)
    return sign_token(f"{name}:{cleaned}")
`, i)
		if err := os.WriteFile(helpersAbs, []byte(bodySrc), 0o600); err != nil {
			return LatencySummary{}, LatencySummary{}, err
		}
		cs, err := eng.UpdateFile(ctx, "app/helpers.py")
		if err != nil {
			return LatencySummary{}, LatencySummary{}, err
		}
		bodySamples = append(bodySamples, float64(cs.Duration.Microseconds())/1000.0)

		exportSrc := fmt.Sprintf(`def clean_input(raw: str) -> str:
    return raw.strip()

def sign_token(payload: str) -> str:
    return f"sig:{payload}"

def audit_event(name: str, detail: str) -> str:
    cleaned = clean_input(detail)
    return sign_token(f"{name}:{cleaned}")

def extra_helper_%d(v: str) -> str:
    return v
`, i)
		if err := os.WriteFile(helpersAbs, []byte(exportSrc), 0o600); err != nil {
			return LatencySummary{}, LatencySummary{}, err
		}
		csExp, err := eng.UpdateFile(ctx, "app/helpers.py")
		if err != nil {
			return LatencySummary{}, LatencySummary{}, err
		}
		exportSamples = append(exportSamples, float64(csExp.Duration.Microseconds())/1000.0)
	}

	return summarizeLatencies(bodySamples), summarizeLatencies(exportSamples), nil
}

func measureMCPLatency(ctx context.Context, repoRoot string, g *graph.Graph) LatencySummary {
	h := mcp.NewHandler(g, mcp.FormatterConfig{
		RepoRoot: repoRoot,
		RepoName: filepath.Base(repoRoot),
	})
	reqs := []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"find_symbol","arguments":{"name":"audit_event"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_callers","arguments":{"symbol":"app.helpers::clean_input"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_callees","arguments":{"symbol":"app.helpers::audit_event"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"get_impact","arguments":{"symbol":"app.helpers::clean_input","max_depth":3}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"file_outline","arguments":{"file":"app/helpers.py"}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"search_code","arguments":{"kind":"Function","limit":25}}}`,
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"repo_overview","arguments":{}}}`,
	}

	// Warm up one call first so JIT/string-intern caches are realistic warm query latencies.
	var warmOut bytes.Buffer
	_ = mcp.NewServer(h, strings.NewReader(reqs[0]+"\n"), &warmOut, nil).Serve(ctx)

	samples := make([]float64, 0, len(reqs)*8)
	var out bytes.Buffer
	for round := 0; round < 8; round++ {
		for _, r := range reqs {
			out.Reset()
			start := time.Now()
			srv := mcp.NewServer(h, strings.NewReader(r+"\n"), &out, nil)
			_ = srv.Serve(ctx)
			samples = append(samples, float64(time.Since(start).Microseconds())/1000.0)
		}
	}
	return summarizeLatencies(samples)
}

func measureCLIStartupMs(ctx context.Context, cliBinary string, runs int) float64 {
	if runs <= 0 {
		runs = 10
	}
	samples := make([]float64, 0, runs)
	dir := filepath.Dir(cliBinary)
	for i := 0; i < runs; i++ {
		start := time.Now()
		_, err := runMeasuredCommand(ctx, dir, cliBinary, "version")
		if err == nil {
			samples = append(samples, float64(time.Since(start).Microseconds())/1000.0)
		}
	}
	return summarizeLatencies(samples).P50Ms
}

func summarizeLatencies(samples []float64) LatencySummary {
	if len(samples) == 0 {
		return LatencySummary{}
	}
	sorted := slices.Clone(samples)
	slices.SortFunc(sorted, cmp.Compare[float64])
	pct := func(p float64) float64 {
		idx := int(float64(len(sorted)-1) * p)
		if idx < 0 {
			idx = 0
		}
		if idx >= len(sorted) {
			idx = len(sorted) - 1
		}
		return sorted[idx]
	}
	return LatencySummary{
		Iterations: len(sorted),
		P50Ms:      pct(0.50),
		P95Ms:      pct(0.95),
		P99Ms:      pct(0.99),
		MaxMs:      sorted[len(sorted)-1],
	}
}
