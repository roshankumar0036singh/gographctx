package main

import (
	"strings"
	"testing"

	"github.com/roshankumar0036singh/gographctx/internal/bench"
)

func TestCompareBaselineAndMarkdownReport(t *testing.T) {
	prev := FullBenchReport{
		Performance: bench.PerfSuiteResult{
			Flask:             bench.RepoIndexMetric{DurationMs: 400, PeakRSSMB: 40},
			Go1k:              bench.RepoIndexMetric{DurationMs: 1200, PeakRSSMB: 90},
			IncrementalBody:   bench.LatencySummary{P50Ms: 10},
			IncrementalExport: bench.LatencySummary{P50Ms: 20},
			MCPQueries:        bench.LatencySummary{P95Ms: 2.0},
			CLIStartupMs:      15,
			BinarySizeMB:      18,
		},
	}
	cur := prev
	cur.GeneratedAt = "2026-11-28T00:00:00Z"
	cur.GoVersion = "go1.25"
	cur.OSArch = "linux/amd64"
	cur.NumCPU = 8
	cur.Corpus = "flask-synth"
	cur.Targets = bench.DefaultSLATargets()
	cur.Performance.Flask.DurationMs = 500 // +25% regression!
	cur.Comparators = []ToolBenchmarkResult{
		{Tool: "gographctx", Category: "code-graph-mcp", Status: "ok", IndexDurationMs: 500, HasCallGraph: true, HasImpactAnalysis: true, HasConfidenceTiers: true},
		{Tool: "ctags", Category: "tag-indexer", Status: "skipped", Reason: "not installed"},
	}

	deltas, breaches := CompareBaseline(cur, prev, 0.20)
	if len(deltas) == 0 || len(breaches) != 1 || !strings.Contains(breaches[0], "flask_index_ms") {
		t.Fatalf("expected 1 regression breach for flask_index_ms, got deltas=%+v breaches=%v", deltas, breaches)
	}
	cur.BaselineDeltas = deltas
	cur.RegressionBreaches = breaches

	md := RenderMarkdownReport(cur)
	for _, want := range []string{"GoGraphCtx Comprehensive Benchmark Report", "Multi-Tool Comparison", "gographctx", "ctags", "REGRESSED"} {
		if !strings.Contains(md, want) {
			t.Fatalf("expected %q in rendered markdown report:\n%s", want, md)
		}
	}
}
