package main

import (
	"fmt"
	"strings"

	"github.com/roshankumar0036singh/gographctx/internal/bench"
)

// ReleaseDelta compares one key benchmark metric against a baseline JSON report.
type ReleaseDelta struct {
	Metric    string  `json:"metric"`
	Baseline  float64 `json:"baseline"`
	Current   float64 `json:"current"`
	ChangePct float64 `json:"change_pct"`
	Regressed bool    `json:"regressed"`
}

// FullBenchReport is the structured artifact emitted by ggctx-bench for CI, accuracy evals, and releases.
type FullBenchReport struct {
	GeneratedAt        string                `json:"generated_at"`
	GoVersion          string                `json:"go_version"`
	OSArch             string                `json:"os_arch"`
	NumCPU             int                   `json:"num_cpu"`
	Corpus             string                `json:"corpus"`
	Performance        bench.PerfSuiteResult `json:"performance"`
	Targets            bench.SLATargets      `json:"targets"`
	SLABreaches        []string              `json:"sla_breaches,omitempty"`
	Accuracy           *bench.AccuracyReport `json:"accuracy,omitempty"`
	AccuracyBreaches   []string              `json:"accuracy_breaches,omitempty"`
	Comparators        []ToolBenchmarkResult `json:"comparators"`
	BaselineDeltas     []ReleaseDelta        `json:"baseline_deltas,omitempty"`
	RegressionBreaches []string              `json:"regression_breaches,omitempty"`
}

// CompareBaseline computes release-over-release latency/memory deltas against prev and flags regressions > maxRegressionRatio.
func CompareBaseline(cur, prev FullBenchReport, maxRegressionRatio float64) ([]ReleaseDelta, []string) {
	pairs := []struct {
		name string
		prev float64
		cur  float64
	}{
		{"flask_index_ms", prev.Performance.Flask.DurationMs, cur.Performance.Flask.DurationMs},
		{"flask_rss_mb", prev.Performance.Flask.PeakRSSMB, cur.Performance.Flask.PeakRSSMB},
		{"go_1k_index_ms", prev.Performance.Go1k.DurationMs, cur.Performance.Go1k.DurationMs},
		{"go_1k_rss_mb", prev.Performance.Go1k.PeakRSSMB, cur.Performance.Go1k.PeakRSSMB},
		{"incremental_body_p50_ms", prev.Performance.IncrementalBody.P50Ms, cur.Performance.IncrementalBody.P50Ms},
		{"incremental_export_p50_ms", prev.Performance.IncrementalExport.P50Ms, cur.Performance.IncrementalExport.P50Ms},
		{"mcp_query_p95_ms", prev.Performance.MCPQueries.P95Ms, cur.Performance.MCPQueries.P95Ms},
		{"cli_startup_ms", prev.Performance.CLIStartupMs, cur.Performance.CLIStartupMs},
		{"binary_size_mb", prev.Performance.BinarySizeMB, cur.Performance.BinarySizeMB},
	}

	var deltas []ReleaseDelta
	var breaches []string
	for _, p := range pairs {
		if p.prev <= 0 || p.cur <= 0 {
			continue
		}
		ratio := (p.cur - p.prev) / p.prev
		regressed := ratio > maxRegressionRatio
		deltas = append(deltas, ReleaseDelta{
			Metric:    p.name,
			Baseline:  p.prev,
			Current:   p.cur,
			ChangePct: ratio * 100.0,
			Regressed: regressed,
		})
		if regressed {
			breaches = append(breaches, fmt.Sprintf("%s regressed +%.1f%% (%.2f -> %.2f)", p.name, ratio*100.0, p.prev, p.cur))
		}
	}
	return deltas, breaches
}

// RenderMarkdownReport formats a comprehensive Markdown benchmark and comparison report.
func RenderMarkdownReport(rep FullBenchReport) string {
	var sb strings.Builder
	sb.WriteString("# GoGraphCtx Comprehensive Benchmark Report\n\n")
	fmt.Fprintf(&sb, "- **Generated:** `%s`\n", rep.GeneratedAt)
	fmt.Fprintf(&sb, "- **Environment:** `%s`, `%s`, `%d CPUs`\n", rep.GoVersion, rep.OSArch, rep.NumCPU)
	fmt.Fprintf(&sb, "- **Primary Corpus:** `%s`\n\n", rep.Corpus)

	sb.WriteString("## 1. GoGraphCtx Performance SLAs\n\n")
	sb.WriteString("| Metric | Measured | Target | Status |\n")
	sb.WriteString("|--------|----------|--------|--------|\n")
	writeSLARow(&sb, "Flask index (83 .py files)", fmt.Sprintf("%.2f ms", rep.Performance.Flask.DurationMs), fmt.Sprintf("< %.0f ms", rep.Targets.MaxFlaskIndexMs), rep.Performance.Flask.DurationMs <= rep.Targets.MaxFlaskIndexMs)
	writeSLARow(&sb, "Flask peak RSS", fmt.Sprintf("%.2f MB", rep.Performance.Flask.PeakRSSMB), fmt.Sprintf("< %.0f MB", rep.Targets.MaxFlaskRSSMB), rep.Performance.Flask.PeakRSSMB <= rep.Targets.MaxFlaskRSSMB)
	writeSLARow(&sb, "Go-1k index (1000 .go files)", fmt.Sprintf("%.2f ms", rep.Performance.Go1k.DurationMs), fmt.Sprintf("< %.0f ms", rep.Targets.MaxGo1kIndexMs), rep.Performance.Go1k.DurationMs <= rep.Targets.MaxGo1kIndexMs)
	writeSLARow(&sb, "Go-1k peak RSS", fmt.Sprintf("%.2f MB", rep.Performance.Go1k.PeakRSSMB), fmt.Sprintf("< %.0f MB", rep.Targets.MaxGo1kRSSMB), rep.Performance.Go1k.PeakRSSMB <= rep.Targets.MaxGo1kRSSMB)
	writeSLARow(&sb, "Incremental body edit (p50)", fmt.Sprintf("%.2f ms", rep.Performance.IncrementalBody.P50Ms), fmt.Sprintf("< %.0f ms", rep.Targets.MaxIncrementalBodyMs), rep.Performance.IncrementalBody.P50Ms <= rep.Targets.MaxIncrementalBodyMs)
	writeSLARow(&sb, "Incremental export edit (p50)", fmt.Sprintf("%.2f ms", rep.Performance.IncrementalExport.P50Ms), fmt.Sprintf("< %.0f ms", rep.Targets.MaxIncrementalExportMs), rep.Performance.IncrementalExport.P50Ms <= rep.Targets.MaxIncrementalExportMs)
	writeSLARow(&sb, "MCP tool query (p95)", fmt.Sprintf("%.2f ms", rep.Performance.MCPQueries.P95Ms), fmt.Sprintf("< %.0f ms", rep.Targets.MaxMCPQueryP95Ms), rep.Performance.MCPQueries.P95Ms <= rep.Targets.MaxMCPQueryP95Ms)
	if rep.Performance.CLIStartupMs > 0 {
		writeSLARow(&sb, "CLI cold startup (p50)", fmt.Sprintf("%.2f ms", rep.Performance.CLIStartupMs), fmt.Sprintf("< %.0f ms", rep.Targets.MaxCLIStartupMs), rep.Performance.CLIStartupMs <= rep.Targets.MaxCLIStartupMs)
	}
	if rep.Performance.BinarySizeMB > 0 {
		writeSLARow(&sb, "Static binary size", fmt.Sprintf("%.2f MB", rep.Performance.BinarySizeMB), fmt.Sprintf("< %.0f MB", rep.Targets.MaxBinarySizeMB), rep.Performance.BinarySizeMB <= rep.Targets.MaxBinarySizeMB)
	}

	if rep.Accuracy != nil {
		sb.WriteString("\n## 2. Accuracy Validation vs SCIP Compiler Oracle\n\n")
		sb.WriteString("| Corpus | Truth Edges | Certain Matched | Precision (Certain) | Recall | Strong-Tier % |\n")
		sb.WriteString("|--------|-------------|-----------------|---------------------|--------|---------------|\n")
		fmt.Fprintf(&sb, "| %s | %d | %d | %.1f%% | %.1f%% | %.1f%% |\n",
			rep.Accuracy.Corpus, rep.Accuracy.GroundTruthEdges, rep.Accuracy.MatchedCertain,
			rep.Accuracy.PrecisionCertain*100, rep.Accuracy.Recall*100, rep.Accuracy.StrongTierRatio*100)
	}

	sb.WriteString("\n## 3. Multi-Tool Comparison (GoGraphCtx vs CGC, SCIP, Ctags, ast-grep, Serena, Ripgrep)\n\n")
	sb.WriteString("| Tool | Category | Status | Index (ms) | Query p50/p95 (ms) | Peak RSS (MB) | Call Graph | Impact | Tiers | Tokens / Noise |\n")
	sb.WriteString("|------|----------|--------|------------|--------------------|---------------|------------|--------|-------|----------------|\n")
	for _, c := range rep.Comparators {
		idxCol := "-"
		if c.IndexDurationMs > 0 {
			idxCol = fmt.Sprintf("%.1f", c.IndexDurationMs)
		}
		qCol := "-"
		if c.QueryP50Ms > 0 || c.QueryP95Ms > 0 {
			qCol = fmt.Sprintf("%.2f / %.2f", c.QueryP50Ms, c.QueryP95Ms)
		}
		rssCol := "-"
		if c.PeakRSSMB > 0 {
			rssCol = fmt.Sprintf("%.1f", c.PeakRSSMB)
		}
		notes := "-"
		if c.Status == "skipped" || c.Status == "error" {
			notes = c.Reason
		} else if c.ResponseTokensTotal > 0 || c.NoiseOrAmbiguityPct > 0 {
			notes = fmt.Sprintf("%d tok / %.1f%% noise", c.ResponseTokensTotal, c.NoiseOrAmbiguityPct)
		}
		fmt.Fprintf(&sb, "| **%s** | %s | %s | %s | %s | %s | %v | %v | %v | %s |\n",
			c.Tool, c.Category, c.Status, idxCol, qCol, rssCol,
			yesNo(c.HasCallGraph), yesNo(c.HasImpactAnalysis), yesNo(c.HasConfidenceTiers), notes)
	}

	if len(rep.BaselineDeltas) > 0 {
		sb.WriteString("\n## 4. Release-over-Release Baseline Comparison\n\n")
		sb.WriteString("| Metric | Baseline | Current | Delta | Status |\n")
		sb.WriteString("|--------|----------|---------|-------|--------|\n")
		for _, d := range rep.BaselineDeltas {
			status := "OK"
			if d.Regressed {
				status = "REGRESSED"
			}
			fmt.Fprintf(&sb, "| %s | %.2f | %.2f | %+.1f%% | %s |\n", d.Metric, d.Baseline, d.Current, d.ChangePct, status)
		}
	}

	return sb.String()
}

func writeSLARow(sb *strings.Builder, name, measured, target string, ok bool) {
	status := "PASS"
	if !ok {
		status = "FAIL"
	}
	fmt.Fprintf(sb, "| %s | %s | %s | %s |\n", name, measured, target, status)
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
