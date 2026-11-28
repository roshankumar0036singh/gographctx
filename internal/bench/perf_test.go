package bench

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/roshankumar0036singh/gographctx/internal/config"
	"github.com/roshankumar0036singh/gographctx/internal/pipeline"
)

func TestPhase30PerformanceSLAs(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping heavy 1000-file Go + incremental SLA suite under -short")
	}

	workDir := t.TempDir()
	binName := "gographctx"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	cliBin := filepath.Join(workDir, binName)
	buildCmd := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w", "-o", cliBin, "../../cmd/gographctx")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build gographctx CLI binary failed: %v\n%s", err, out)
	}

	res, err := RunPerfSuite(context.Background(), workDir, cliBin, "")
	if err != nil {
		t.Fatalf("RunPerfSuite failed: %v", err)
	}
	t.Logf("Binary size: %.2fMB, CLI cold startup: %.2fms", res.BinarySizeMB, res.CLIStartupMs)
	t.Logf("Flask (83 .py files): %.2fms, RSS %.2fMB, nodes=%d, edges=%d, strong=%.1f%%",
		res.Flask.DurationMs, res.Flask.PeakRSSMB, res.Flask.Nodes, res.Flask.Edges, res.Flask.StrongTierPct)
	t.Logf("Go-1k (1000 .go files): %.2fms, RSS %.2fMB, nodes=%d, edges=%d",
		res.Go1k.DurationMs, res.Go1k.PeakRSSMB, res.Go1k.Nodes, res.Go1k.Edges)
	t.Logf("Incremental body p50=%.2fms (p95=%.2fms); export p50=%.2fms (p95=%.2fms)",
		res.IncrementalBody.P50Ms, res.IncrementalBody.P95Ms, res.IncrementalExport.P50Ms, res.IncrementalExport.P95Ms)
	t.Logf("MCP queries p50=%.2fms, p95=%.2fms, p99=%.2fms",
		res.MCPQueries.P50Ms, res.MCPQueries.P95Ms, res.MCPQueries.P99Ms)

	targets := DefaultSLATargets()
	if breaches := CheckSLAs(res, targets); len(breaches) > 0 {
		t.Fatalf("Phase 30 SLA breaches: %v", breaches)
	}
}

func BenchmarkFlaskScale83PythonFiles(b *testing.B) {
	dir := b.TempDir()
	if err := GenerateFlaskScalePythonCorpus(dir, 83); err != nil {
		b.Fatalf("GenerateFlaskScalePythonCorpus: %v", err)
	}
	cfg := config.DefaultConfig()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := pipeline.IndexRepository(context.Background(), dir, cfg, pipeline.IndexOptions{Force: true})
		if err != nil {
			b.Fatalf("IndexRepository: %v", err)
		}
		if res.Stats.IndexedFiles != 83 {
			b.Fatalf("expected 83 indexed files, got %d", res.Stats.IndexedFiles)
		}
	}
}

func BenchmarkGo1000Files(b *testing.B) {
	dir := b.TempDir()
	if err := GenerateGoMonorepoCorpus(dir, 1000); err != nil {
		b.Fatalf("GenerateGoMonorepoCorpus: %v", err)
	}
	cfg := config.DefaultConfig()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := pipeline.IndexRepository(context.Background(), dir, cfg, pipeline.IndexOptions{Force: true})
		if err != nil {
			b.Fatalf("IndexRepository: %v", err)
		}
		if res.Stats.IndexedFiles < 1000 {
			b.Fatalf("expected >=1000 indexed files, got %d", res.Stats.IndexedFiles)
		}
	}
}
