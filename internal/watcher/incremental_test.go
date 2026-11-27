package watcher_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/roshankumar0036singh/gographctx/internal/config"
	"github.com/roshankumar0036singh/gographctx/internal/graph"
	"github.com/roshankumar0036singh/gographctx/internal/pipeline"
	"github.com/roshankumar0036singh/gographctx/internal/storage"
	"github.com/roshankumar0036singh/gographctx/internal/watcher"
)

func createIncrementalRepoFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"pkg/helpers.py": `def clean(raw: str) -> str:
    return raw.strip()
`,
		"pkg/app.py": `from .helpers import *

def process(text: str) -> str:
    val = clean(text)
    return emit_metric(val)
`,
		"pkg/standalone.py": `def independent() -> int:
    return 42
`,
	}
	for rel, content := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll failed: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatalf("WriteFile failed: %v", err)
		}
	}
	return root
}

func TestIncrementalEngineBodyExportAndDeleteUpdates(t *testing.T) {
	root := createIncrementalRepoFixture(t)
	cfg := config.DefaultConfig()
	cfg.Workers = 2

	eng, err := watcher.NewIncrementalEngine(context.Background(), root, cfg, pipeline.IndexOptions{})
	if err != nil {
		t.Fatalf("NewIncrementalEngine failed: %v", err)
	}
	defer func() { _ = eng.Close() }()

	if deps := eng.Dependents("pkg/helpers.py"); !slices.Equal(deps, []string{"pkg/app.py"}) {
		t.Fatalf("initial Dependents(pkg/helpers.py) = %v, want [pkg/app.py]", deps)
	}

	// Verify emit_metric in pkg/app.py starts as unresolved CallSite.
	eng.Graph().View(func(v *graph.ReadView) {
		var sawUnresolved bool
		for _, cs := range v.GetCallSitesByFile("pkg/app.py") {
			if cs.CalleeExpr == "emit_metric" && cs.Status == graph.CallStatusUnresolved {
				sawUnresolved = true
			}
		}
		if !sawUnresolved {
			t.Fatalf("expected initial unresolved CallSite emit_metric in pkg/app.py")
		}
	})

	// 1. Body-only edit in pkg/helpers.py -> no dependents re-resolved.
	helpersAbs := filepath.Join(root, "pkg", "helpers.py")
	if err := os.WriteFile(helpersAbs, []byte("def clean(raw: str) -> str:\n    return raw.strip().lower()\n"), 0o600); err != nil {
		t.Fatalf("WriteFile body edit failed: %v", err)
	}
	csBody, err := eng.UpdateFile(context.Background(), "pkg/helpers.py")
	if err != nil {
		t.Fatalf("UpdateFile(body edit) failed: %v", err)
	}
	if !csBody.BodyOnly || csBody.DependentsUpdated != 0 || !slices.Equal(csBody.ReResolvedFiles, []string{"pkg/helpers.py"}) {
		t.Fatalf("expected BodyOnly=true with 0 dependents updated, got %+v", csBody)
	}

	// 2. Add new export `emit_metric` to pkg/helpers.py -> pkg/app.py re-resolved via deps + CallSite lookup!
	if err := os.WriteFile(helpersAbs, []byte(`def clean(raw: str) -> str:
    return raw.strip().lower()

def emit_metric(val: str) -> str:
    return "metric:" + val
`), 0o600); err != nil {
		t.Fatalf("WriteFile add export failed: %v", err)
	}
	csAdd, err := eng.UpdateFile(context.Background(), "pkg/helpers.py")
	if err != nil {
		t.Fatalf("UpdateFile(add export) failed: %v", err)
	}
	if csAdd.BodyOnly || csAdd.DependentsUpdated != 1 || !slices.Contains(csAdd.AddedSymbols, "pkg.helpers::emit_metric") {
		t.Fatalf("expected new export emit_metric with 1 dependent updated, got %+v", csAdd)
	}
	eng.Graph().View(func(v *graph.ReadView) {
		var sawEdge bool
		for _, e := range v.AllEdges() {
			if e.Kind == graph.EdgeCalls {
				if tgt, ok := v.GetNode(e.Target); ok && tgt.QName == "pkg.helpers::emit_metric" {
					sawEdge = true
				}
			}
		}
		if !sawEdge {
			t.Fatalf("expected CALLS edge from pkg/app.py to newly added pkg.helpers::emit_metric")
		}
		for _, cs := range v.GetCallSitesByFile("pkg/app.py") {
			if cs.CalleeExpr == "emit_metric" {
				t.Fatalf("expected unresolved emit_metric CallSite cleared after resolution, got %+v", cs)
			}
		}
	})

	// 3. Delete `emit_metric` function from pkg/helpers.py -> pkg/app.py re-resolved back to unresolved CallSite.
	if err := os.WriteFile(helpersAbs, []byte(`def clean(raw: str) -> str:
    return raw.strip()
`), 0o600); err != nil {
		t.Fatalf("WriteFile delete func failed: %v", err)
	}
	csDelFn, err := eng.UpdateFile(context.Background(), "pkg/helpers.py")
	if err != nil {
		t.Fatalf("UpdateFile(delete func) failed: %v", err)
	}
	if !slices.Contains(csDelFn.RemovedSymbols, "pkg.helpers::emit_metric") || csDelFn.DependentsUpdated != 1 {
		t.Fatalf("expected RemovedSymbols=[pkg.helpers::emit_metric] and 1 dependent, got %+v", csDelFn)
	}
	eng.Graph().View(func(v *graph.ReadView) {
		if nodes := v.GetByQName("pkg.helpers::emit_metric"); len(nodes) != 0 {
			t.Fatalf("expected emit_metric node removed, got %+v", nodes)
		}
		var sawUnresolvedBack bool
		for _, cs := range v.GetCallSitesByFile("pkg/app.py") {
			if cs.CalleeExpr == "emit_metric" && cs.Status == graph.CallStatusUnresolved {
				sawUnresolvedBack = true
			}
		}
		if !sawUnresolvedBack {
			t.Fatalf("expected emit_metric back in unresolved CallSites after function deletion")
		}
	})

	// 4. Delete pkg/standalone.py on disk and invoke RemoveFile -> verify WAL replay persistence.
	if err := os.Remove(filepath.Join(root, "pkg", "standalone.py")); err != nil {
		t.Fatalf("Remove standalone.py failed: %v", err)
	}
	csDelFile, err := eng.RemoveFile(context.Background(), "pkg/standalone.py")
	if err != nil {
		t.Fatalf("RemoveFile failed: %v", err)
	}
	if csDelFile.Action != watcher.ChangeDeleted {
		t.Fatalf("expected ChangeDeleted, got %+v", csDelFile)
	}

	wantNodes := eng.Graph().NodeCount()
	wantEdges := eng.Graph().EdgeCount()
	if err := eng.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Verify WAL replay produces identical Graph state.
	storeDir := filepath.Join(root, cfg.DataDir)
	reStore, reG, lstats, err := storage.LoadStore(storeDir, storage.WriteOptions{Compress: cfg.CompressSnapshots}, storage.WALOptions{}, storage.LoadOptions{})
	if err != nil {
		t.Fatalf("LoadStore after incremental WAL appends failed: %v", err)
	}
	_ = reStore.Close()
	if lstats.WALFramesApplied == 0 || reG.NodeCount() != wantNodes || reG.EdgeCount() != wantEdges {
		t.Fatalf("WAL replay mismatch: stats=%+v reG(%d,%d) != live(%d,%d)",
			lstats, reG.NodeCount(), reG.EdgeCount(), wantNodes, wantEdges)
	}
}
