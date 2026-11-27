package pipeline_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/roshankumar0036singh/gographctx/internal/config"
	"github.com/roshankumar0036singh/gographctx/internal/graph"
	"github.com/roshankumar0036singh/gographctx/internal/pipeline"
	"github.com/roshankumar0036singh/gographctx/internal/storage"
)

func createMixedFixtureRepo(t testing.TB) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"go.mod": "module github.com/acme/demo\n\ngo 1.22\n",
		"pkg/store.go": `package pkg
type Repo struct{}
func NewRepo() *Repo { return &Repo{} }
func (r *Repo) Ping() string { return "ok" }
`,
		"cmd/main.go": `package main
import "github.com/acme/demo/pkg"
func main() {
	r := pkg.NewRepo()
	_ = r.Ping()
}
`,
		"py/helpers.py": `def clean(x: str) -> str:
    return x.strip()
`,
		"py/app.py": `from .helpers import clean
def handle(v: str) -> str:
    return clean(v)
`,
		"web/util.ts": `export function greet(name: string): string {
  return "hi " + name;
}
`,
		"web/index.ts": `import { greet } from "./util";
export function run(): string {
  return greet("world");
}
`,
	}
	for rel, content := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll(%q) failed: %v", full, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatalf("WriteFile(%q) failed: %v", full, err)
		}
	}
	return root
}

func TestIndexRepositoryEndToEndAndSmartMode(t *testing.T) {
	root := createMixedFixtureRepo(t)
	cfg := config.DefaultConfig()
	cfg.Workers = 2

	// 1. Initial full index producing .ggctx/index.ggctx and manifest.json
	res1, err := pipeline.IndexRepository(context.Background(), root, cfg, pipeline.IndexOptions{})
	if err != nil {
		t.Fatalf("IndexRepository initial failed: %v", err)
	}
	if res1.Stats.TotalFiles != 6 || res1.Stats.IndexedFiles != 6 || res1.Stats.SkippedFiles != 0 {
		t.Fatalf("unexpected initial stats: %+v", res1.Stats)
	}
	if res1.Stats.StrongCallEdges < 4 {
		t.Fatalf("expected >=4 resolved strong call edges across Go/Py/TS, got %+v", res1.Stats)
	}
	if _, err := os.Stat(res1.SnapshotPath); err != nil {
		t.Fatalf("expected snapshot on disk at %s: %v", res1.SnapshotPath, err)
	}
	if !strings.Contains(res1.FormatSummary(), "GoGraphCtx Index Summary") {
		t.Fatalf("unexpected summary format: %q", res1.FormatSummary())
	}

	// Verify persisted snapshot loads cleanly via storage.LoadStore.
	store, loadedG, loadStats, err := storage.LoadStore(res1.StoreDir, storage.WriteOptions{Compress: cfg.CompressSnapshots}, storage.WALOptions{}, storage.LoadOptions{})
	if err != nil {
		t.Fatalf("LoadStore failed: %v", err)
	}
	_ = store.Close()
	if !loadStats.HasSnapshot || loadedG.NodeCount() != res1.Stats.NodeCount || loadedG.EdgeCount() != res1.Stats.EdgeCount {
		t.Fatalf("persisted snapshot mismatch: loaded (%d nodes, %d edges) vs indexed (%d nodes, %d edges)",
			loadedG.NodeCount(), loadedG.EdgeCount(), res1.Stats.NodeCount, res1.Stats.EdgeCount)
	}

	// 2. Second run in smart mode with no changes -> all 6 files skipped!
	res2, err := pipeline.IndexRepository(context.Background(), root, cfg, pipeline.IndexOptions{Force: false})
	if err != nil {
		t.Fatalf("IndexRepository smart skip failed: %v", err)
	}
	if res2.Stats.SkippedFiles != 6 || res2.Stats.IndexedFiles != 0 {
		t.Fatalf("expected smart mode to skip 6/6 files, got %+v", res2.Stats)
	}

	// 3. Modify one Python file (changing size & mtime) -> incremental delta re-indexes changed file + dependents.
	helpersPath := filepath.Join(root, "py", "helpers.py")
	newHelpers := "def clean(x: str) -> str:\n    return x.strip().lower()\n"
	if err := os.WriteFile(helpersPath, []byte(newHelpers), 0o600); err != nil {
		t.Fatalf("WriteFile updated helpers.py failed: %v", err)
	}
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(helpersPath, future, future)

	res3, err := pipeline.IndexRepository(context.Background(), root, cfg, pipeline.IndexOptions{Force: false})
	if err != nil {
		t.Fatalf("IndexRepository smart delta failed: %v", err)
	}
	if res3.Stats.IndexedFiles != 1 || res3.Stats.SkippedFiles != 5 {
		t.Fatalf("expected 1 indexed and 5 skipped after single-file edit, got %+v", res3.Stats)
	}
	res3.Graph.View(func(v *graph.ReadView) {
		var sawCleanCall bool
		for _, e := range v.AllEdges() {
			if e.Kind == graph.EdgeCalls {
				if tgt, ok := v.GetNode(e.Target); ok && tgt.QName == "py.helpers::clean" {
					sawCleanCall = true
				}
			}
		}
		if !sawCleanCall {
			t.Fatalf("expected py.helpers::clean CALLS edge preserved after incremental delta")
		}
	})

	// 4. --force re-indexes every file regardless of manifest match.
	res4, err := pipeline.IndexRepository(context.Background(), root, cfg, pipeline.IndexOptions{Force: true})
	if err != nil {
		t.Fatalf("IndexRepository force failed: %v", err)
	}
	if res4.Stats.IndexedFiles != 6 || res4.Stats.SkippedFiles != 0 {
		t.Fatalf("expected --force to index all 6 files, got %+v", res4.Stats)
	}
}

func BenchmarkIndexRepository(b *testing.B) {
	root := createMixedFixtureRepo(b)
	cfg := config.DefaultConfig()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := pipeline.IndexRepository(context.Background(), root, cfg, pipeline.IndexOptions{Force: true})
		if err != nil {
			b.Fatalf("IndexRepository failed: %v", err)
		}
		if res.Stats.NodeCount == 0 {
			b.Fatalf("expected non-empty graph")
		}
	}
}
