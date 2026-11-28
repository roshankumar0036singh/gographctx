package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/roshankumar0036singh/gographctx/internal/bench"
	"github.com/roshankumar0036singh/gographctx/internal/mcp"
)

// ToolBenchmarkResult captures normalized performance, footprint, capability, and token-efficiency metrics
// for a single code-indexing or code-search tool on a target repository.
type ToolBenchmarkResult struct {
	Tool                string  `json:"tool"`
	Category            string  `json:"category"`
	Status              string  `json:"status"` // "ok", "skipped", "error"
	Reason              string  `json:"reason,omitempty"`
	Version             string  `json:"version,omitempty"`
	IndexDurationMs     float64 `json:"index_duration_ms,omitempty"`
	PeakRSSMB           float64 `json:"peak_rss_mb,omitempty"`
	IndexDiskBytes      int64   `json:"index_disk_bytes,omitempty"`
	WarmupStartupMs     float64 `json:"warmup_startup_ms,omitempty"`
	QueryP50Ms          float64 `json:"query_p50_ms,omitempty"`
	QueryP95Ms          float64 `json:"query_p95_ms,omitempty"`
	ResponseTokensTotal int     `json:"response_tokens_total,omitempty"`
	NoiseOrAmbiguityPct float64 `json:"noise_or_ambiguity_pct,omitempty"`
	StrongTierEdgePct   float64 `json:"strong_tier_edge_pct,omitempty"`
	MCPToolCount        int     `json:"mcp_tool_count,omitempty"`
	HasCallGraph        bool    `json:"has_call_graph"`
	HasImpactAnalysis   bool    `json:"has_impact_analysis"`
	HasConfidenceTiers  bool    `json:"has_confidence_tiers"`
	ZeroExternalDeps    bool    `json:"zero_external_deps"`
}

// RunAllComparators executes enabled comparator benchmarks against repoDir.
func RunAllComparators(ctx context.Context, repoDir, corpusKind, cliBinary string, enabled map[string]bool, perf bench.PerfSuiteResult) ([]ToolBenchmarkResult, string) {
	var results []ToolBenchmarkResult
	var scipPath string

	if enabled["gographctx"] {
		results = append(results, benchGoGraphCtxComparator(ctx, repoDir, corpusKind, cliBinary, perf))
	}
	if enabled["cgc"] {
		results = append(results, benchCGCComparator(ctx, repoDir))
	}
	if enabled["scip"] {
		scipRes, generatedSCIP := benchSCIPComparator(ctx, repoDir, corpusKind)
		results = append(results, scipRes)
		if generatedSCIP != "" {
			scipPath = generatedSCIP
		}
	}
	if enabled["ctags"] {
		results = append(results, benchCtagsComparator(ctx, repoDir))
	}
	if enabled["ast-grep"] {
		results = append(results, benchAstGrepComparator(ctx, repoDir, corpusKind))
	}
	if enabled["serena"] {
		results = append(results, benchSerenaComparator(ctx, repoDir))
	}
	if enabled["ripgrep"] {
		results = append(results, benchRipgrepComparator(ctx, repoDir, corpusKind))
	}
	return results, scipPath
}

func benchGoGraphCtxComparator(ctx context.Context, repoDir, corpusKind, cliBinary string, perf bench.PerfSuiteResult) ToolBenchmarkResult {
	res := ToolBenchmarkResult{
		Tool:               "gographctx",
		Category:           "code-graph-mcp",
		Status:             "ok",
		MCPToolCount:       len(mcp.AllToolDefinitions()),
		HasCallGraph:       true,
		HasImpactAnalysis:  true,
		HasConfidenceTiers: true,
		ZeroExternalDeps:   true,
	}

	metric := perf.Flask
	if corpusKind == "go-1k" {
		metric = perf.Go1k
	}
	res.IndexDurationMs = metric.DurationMs
	res.PeakRSSMB = metric.PeakRSSMB
	res.IndexDiskBytes = metric.SnapshotBytes
	res.StrongTierEdgePct = metric.StrongTierPct
	res.WarmupStartupMs = perf.CLIStartupMs
	res.QueryP50Ms = perf.MCPQueries.P50Ms
	res.QueryP95Ms = perf.MCPQueries.P95Ms

	if strings.TrimSpace(cliBinary) != "" {
		verOut, _ := runCapture(ctx, repoDir, 10*time.Second, cliBinary, "version")
		res.Version = strings.TrimSpace(verOut)

		// Measure total response tokens across 4 standard agent navigation commands.
		tokTotal := 0
		for _, qargs := range standardGoGraphCtxQueries(corpusKind) {
			out, err := runCapture(ctx, repoDir, 15*time.Second, cliBinary, qargs...)
			if err == nil {
				tokTotal += mcp.EstimateTokens(out)
			}
		}
		res.ResponseTokensTotal = tokTotal
	}
	return res
}

func standardGoGraphCtxQueries(corpusKind string) [][]string {
	sym := "audit_event"
	fileArg := "app/helpers.py"
	if corpusKind == "go-1k" {
		sym = "Normalize"
		fileArg = "internal/shared/shared.go"
	} else if corpusKind == "mixed" {
		sym = "validate_payload"
		fileArg = "py/service.py"
	}
	return [][]string{
		{"query", "--format", "json", "find_symbol", sym},
		{"query", "--format", "json", "get_callers", sym},
		{"query", "--format", "json", "get_impact", sym},
		{"query", "--format", "json", "file_outline", fileArg},
	}
}

func benchCGCComparator(ctx context.Context, repoDir string) ToolBenchmarkResult {
	res := ToolBenchmarkResult{
		Tool:               "cgc",
		Category:           "code-graph-mcp",
		HasCallGraph:       true,
		HasImpactAnalysis:  true,
		HasConfidenceTiers: true,
		ZeroExternalDeps:   false,
	}
	bin, err := exec.LookPath("cgc")
	if err != nil {
		res.Status = "skipped"
		res.Reason = "cgc binary not found in PATH"
		return res
	}
	if ver, verr := runCapture(ctx, repoDir, 10*time.Second, bin, "--version"); verr == nil {
		res.Version = strings.TrimSpace(ver)
	}

	// Copy repo into an isolated temp directory so CGC indexing artifacts don't affect others.
	copyDir, cerr := copyRepoTemp(repoDir)
	if cerr != nil {
		res.Status = "error"
		res.Reason = cerr.Error()
		return res
	}
	defer func() { _ = os.RemoveAll(copyDir) }()

	// Run cgc index with a bounded timeout (default CGC backends on Windows/unconfigured DB may error cleanly).
	start := time.Now()
	_, idxOut, idxErr := runMeasuredCapture(ctx, copyDir, 45*time.Second, bin, "index", ".")
	res.IndexDurationMs = float64(time.Since(start).Microseconds()) / 1000.0
	if idxErr != nil {
		res.Status = "skipped"
		msg := strings.TrimSpace(idxOut)
		if len(msg) > 200 {
			msg = msg[:200] + "..."
		}
		res.Reason = fmt.Sprintf("cgc index unavailable in current environment (%v: %s)", idxErr, msg)
		return res
	}
	res.Status = "ok"
	return res
}

func benchSCIPComparator(ctx context.Context, repoDir, corpusKind string) (ToolBenchmarkResult, string) {
	res := ToolBenchmarkResult{
		Tool:               "scip",
		Category:           "compiler-oracle",
		HasCallGraph:       true,
		HasImpactAnalysis:  false,
		HasConfidenceTiers: false,
		ZeroExternalDeps:   false,
	}

	var binName string
	var args []string
	switch corpusKind {
	case "go-1k":
		binName = "scip-go"
		args = []string{}
	default:
		binName = "scip-python"
		args = []string{"index", "--project-name", filepath.Base(repoDir), "."}
	}

	bin, err := exec.LookPath(binName)
	if err != nil {
		res.Status = "skipped"
		res.Reason = fmt.Sprintf("%s not found in PATH", binName)
		return res, ""
	}
	if ver, verr := runCapture(ctx, repoDir, 10*time.Second, bin, "--version"); verr == nil {
		res.Version = firstLine(ver)
	}

	start := time.Now()
	peak, out, idxErr := runMeasuredCapture(ctx, repoDir, 120*time.Second, bin, args...)
	res.IndexDurationMs = float64(time.Since(start).Microseconds()) / 1000.0
	res.PeakRSSMB = float64(peak) / (1024 * 1024)
	if idxErr != nil {
		res.Status = "error"
		res.Reason = fmt.Sprintf("%s failed: %v (%s)", binName, idxErr, firstLine(out))
		return res, ""
	}
	scipFile := filepath.Join(repoDir, "index.scip")
	if st, statErr := os.Stat(scipFile); statErr == nil {
		res.IndexDiskBytes = st.Size()
		res.Status = "ok"
		return res, scipFile
	}
	res.Status = "error"
	res.Reason = "index.scip not produced"
	return res, ""
}

func benchCtagsComparator(ctx context.Context, repoDir string) ToolBenchmarkResult {
	res := ToolBenchmarkResult{
		Tool:               "ctags",
		Category:           "tag-indexer",
		HasCallGraph:       false,
		HasImpactAnalysis:  false,
		HasConfidenceTiers: false,
		ZeroExternalDeps:   true,
	}
	bin, err := exec.LookPath("ctags")
	if err != nil {
		res.Status = "skipped"
		res.Reason = "ctags binary not found in PATH"
		return res
	}
	if ver, verr := runCapture(ctx, repoDir, 5*time.Second, bin, "--version"); verr == nil {
		res.Version = firstLine(ver)
	}

	outFile := filepath.Join(os.TempDir(), fmt.Sprintf("ggctx-ctags-%d.tags", time.Now().UnixNano()))
	defer func() { _ = os.Remove(outFile) }()

	start := time.Now()
	peak, out, runErr := runMeasuredCapture(ctx, repoDir, 30*time.Second, bin, "--output-format=json", "--fields=+nKS", "-R", "-f", outFile, ".")
	if runErr != nil {
		// Fallback to classic tag format if Universal Ctags JSON support wasn't compiled in.
		start = time.Now()
		peak, out, runErr = runMeasuredCapture(ctx, repoDir, 30*time.Second, bin, "-R", "-f", outFile, ".")
	}
	res.IndexDurationMs = float64(time.Since(start).Microseconds()) / 1000.0
	res.PeakRSSMB = float64(peak) / (1024 * 1024)
	if runErr != nil {
		res.Status = "error"
		res.Reason = fmt.Sprintf("ctags failed: %v (%s)", runErr, firstLine(out))
		return res
	}
	if st, statErr := os.Stat(outFile); statErr == nil {
		res.IndexDiskBytes = st.Size()
	}
	if data, rerr := os.ReadFile(outFile); rerr == nil {
		res.NoiseOrAmbiguityPct = measureCtagsNameCollisionPct(string(data))
	}
	res.Status = "ok"
	return res
}

func measureCtagsNameCollisionPct(raw string) float64 {
	counts := make(map[string]int)
	total := 0
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "!") {
			continue
		}
		var name string
		if strings.HasPrefix(line, "{") {
			var obj struct {
				Name string `json:"name"`
			}
			if json.Unmarshal([]byte(line), &obj) == nil {
				name = obj.Name
			}
		} else if tab := strings.IndexByte(line, '\t'); tab > 0 {
			name = line[:tab]
		}
		if name != "" {
			counts[name]++
			total++
		}
	}
	if total == 0 {
		return 0
	}
	ambig := 0
	for _, c := range counts {
		if c > 1 {
			ambig += c
		}
	}
	return float64(ambig) * 100.0 / float64(total)
}

func benchAstGrepComparator(ctx context.Context, repoDir, corpusKind string) ToolBenchmarkResult {
	res := ToolBenchmarkResult{
		Tool:               "ast-grep",
		Category:           "ast-search",
		HasCallGraph:       false,
		HasImpactAnalysis:  false,
		HasConfidenceTiers: false,
		ZeroExternalDeps:   true,
	}
	bin, err := exec.LookPath("sg")
	if err != nil {
		bin, err = exec.LookPath("ast-grep")
	}
	if err != nil {
		res.Status = "skipped"
		res.Reason = "sg / ast-grep binary not found in PATH"
		return res
	}
	if ver, verr := runCapture(ctx, repoDir, 5*time.Second, bin, "--version"); verr == nil {
		res.Version = firstLine(ver)
	}

	lang := "python"
	sym := "audit_event"
	if corpusKind == "go-1k" {
		lang = "go"
		sym = "Normalize"
	}
	patterns := [][]string{
		{"run", "-l", lang, "-p", fmt.Sprintf("def %s($$$)", sym), "--json", "."},
		{"run", "-l", lang, "-p", fmt.Sprintf("%s($$$)", sym), "--json", "."},
	}
	if lang == "go" {
		patterns[0] = []string{"run", "-l", "go", "-p", fmt.Sprintf("func %s($$$)", sym), "--json", "."}
	}

	var samples []float64
	var maxPeak uint64
	toks := 0
	for round := 0; round < 3; round++ {
		for _, p := range patterns {
			start := time.Now()
			peak, out, qerr := runMeasuredCapture(ctx, repoDir, 20*time.Second, bin, p...)
			dur := float64(time.Since(start).Microseconds()) / 1000.0
			if qerr != nil {
				continue
			}
			samples = append(samples, dur)
			if peak > maxPeak {
				maxPeak = peak
			}
			if round == 0 {
				toks += mcp.EstimateTokens(out)
			}
		}
	}
	if len(samples) == 0 {
		res.Status = "error"
		res.Reason = "ast-grep queries produced no valid output"
		return res
	}
	p50, p95 := quantiles(samples)
	res.QueryP50Ms = p50
	res.QueryP95Ms = p95
	res.PeakRSSMB = float64(maxPeak) / (1024 * 1024)
	res.ResponseTokensTotal = toks
	res.Status = "ok"
	return res
}

func benchSerenaComparator(ctx context.Context, repoDir string) ToolBenchmarkResult {
	res := ToolBenchmarkResult{
		Tool:               "serena",
		Category:           "lsp-mcp",
		HasCallGraph:       true,
		HasImpactAnalysis:  false,
		HasConfidenceTiers: false,
		ZeroExternalDeps:   false,
	}
	bin, err := exec.LookPath("serena")
	if err != nil {
		res.Status = "skipped"
		res.Reason = "serena binary not found in PATH"
		return res
	}
	if ver, verr := runCapture(ctx, repoDir, 10*time.Second, bin, "--version"); verr == nil {
		res.Version = firstLine(ver)
	}
	res.Status = "skipped"
	res.Reason = "serena LSP session benchmark requires configured project language server"
	return res
}

func benchRipgrepComparator(ctx context.Context, repoDir, corpusKind string) ToolBenchmarkResult {
	res := ToolBenchmarkResult{
		Tool:               "ripgrep",
		Category:           "lexical-search",
		HasCallGraph:       false,
		HasImpactAnalysis:  false,
		HasConfidenceTiers: false,
		ZeroExternalDeps:   true,
	}
	bin, err := exec.LookPath("rg")
	if err != nil {
		res.Status = "skipped"
		res.Reason = "rg (ripgrep) binary not found in PATH"
		return res
	}
	if ver, verr := runCapture(ctx, repoDir, 5*time.Second, bin, "--version"); verr == nil {
		res.Version = firstLine(ver)
	}

	queries := []string{"audit_event", "clean_input", "RequestContext", "dispatch"}
	if corpusKind == "go-1k" {
		queries = []string{"Normalize", "StdLogger", "Worker0001", "Process"}
	}

	var samples []float64
	var maxPeak uint64
	toks := 0
	totalHits := 0
	importOrCommentHits := 0

	for round := 0; round < 3; round++ {
		for _, q := range queries {
			start := time.Now()
			peak, out, qerr := runMeasuredCapture(ctx, repoDir, 15*time.Second, bin, "--json", "-w", q, ".")
			dur := float64(time.Since(start).Microseconds()) / 1000.0
			if qerr != nil {
				continue
			}
			samples = append(samples, dur)
			if peak > maxPeak {
				maxPeak = peak
			}
			if round == 0 {
				toks += mcp.EstimateTokens(out)
				h, noisy := countRipgrepNoise(out)
				totalHits += h
				importOrCommentHits += noisy
			}
		}
	}
	p50, p95 := quantiles(samples)
	res.QueryP50Ms = p50
	res.QueryP95Ms = p95
	res.PeakRSSMB = float64(maxPeak) / (1024 * 1024)
	res.ResponseTokensTotal = toks
	if totalHits > 0 {
		res.NoiseOrAmbiguityPct = float64(importOrCommentHits) * 100.0 / float64(totalHits)
	}
	res.Status = "ok"
	return res
}

func countRipgrepNoise(jsonLines string) (total int, noise int) {
	for _, line := range strings.Split(jsonLines, "\n") {
		if !strings.Contains(line, `"type":"match"`) {
			continue
		}
		total++
		if strings.Contains(line, `"text":"import `) || strings.Contains(line, `"text":"from `) || strings.Contains(line, `"text":"//`) || strings.Contains(line, `"text":"#`) {
			noise++
		}
	}
	return total, noise
}

func runCapture(ctx context.Context, dir string, timeout time.Duration, name string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, name, args...)
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

func runMeasuredCapture(ctx context.Context, dir string, timeout time.Duration, name string, args ...string) (uint64, string, error) {
	return bench.RunMeasuredCapture(ctx, dir, timeout, name, args...)
}

func copyRepoTemp(src string) (string, error) {
	dst, err := os.MkdirTemp("", "ggctx-cgc-bench-*")
	if err != nil {
		return "", err
	}
	err = filepath.WalkDir(src, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		rel, _ := filepath.Rel(src, p)
		if rel == ".ggctx" || strings.HasPrefix(rel, ".ggctx"+string(filepath.Separator)) || rel == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		return os.WriteFile(target, data, 0o600)
	})
	return dst, err
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if idx := strings.IndexAny(s, "\r\n"); idx >= 0 {
		return strings.TrimSpace(s[:idx])
	}
	return s
}

func quantiles(samples []float64) (p50, p95 float64) {
	if len(samples) == 0 {
		return 0, 0
	}
	cp := slices.Clone(samples)
	slices.SortFunc(cp, cmp.Compare[float64])
	p50 = cp[(len(cp)-1)*50/100]
	p95 = cp[(len(cp)-1)*95/100]
	return p50, p95
}
