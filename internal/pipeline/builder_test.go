package pipeline_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/roshankumar0036singh/gographctx/internal/graph"
	"github.com/roshankumar0036singh/gographctx/internal/parser"
	parserlang "github.com/roshankumar0036singh/gographctx/internal/parser/lang"
	"github.com/roshankumar0036singh/gographctx/internal/parser/treesitter"
	"github.com/roshankumar0036singh/gographctx/internal/pipeline"
	"github.com/roshankumar0036singh/gographctx/internal/resolver"
	resolverlang "github.com/roshankumar0036singh/gographctx/internal/resolver/lang"
	"github.com/roshankumar0036singh/gographctx/internal/storage"
)

func parseAndResolveFixture(t *testing.T, inputs []parser.FileInput) ([]*parser.ParsedFile, map[string]*resolver.FileResolution) {
	t.Helper()
	mgr, err := treesitter.NewManager()
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	defer mgr.Close()

	preg := parser.NewRegistry()
	if _, err := parserlang.RegisterPython(preg, mgr); err != nil {
		t.Fatal(err)
	}
	coord, err := parser.NewCoordinator(preg, parser.CoordinatorConfig{})
	if err != nil {
		t.Fatal(err)
	}
	batch, err := coord.ParseFiles(context.Background(), inputs)
	if err != nil || len(batch.Errors) > 0 {
		t.Fatalf("ParseFiles failed: err=%v errs=%v", err, batch.Errors)
	}

	// Pass 1 skeleton graph so resolver ReadView can resolve cross-file symbols.
	skelGraph, err := pipeline.BuildGraph(context.Background(), batch.Files, nil)
	if err != nil {
		t.Fatalf("BuildGraph(skeleton) failed: %v", err)
	}

	rreg := resolver.NewRegistry()
	rreg.MustRegister(resolverlang.NewPythonResolver())

	resolved := make(map[string]*resolver.FileResolution, len(batch.Files))
	skelGraph.View(func(v *graph.ReadView) {
		rc := &resolver.ResolutionContext{
			View:      v,
			NameIndex: batch.NameIndex,
		}
		rc.IndexFiles(batch.Files)
		for _, pf := range batch.Files {
			fr, err := rreg.ResolveFile(context.Background(), pf, rc)
			if err != nil {
				t.Fatalf("ResolveFile(%q) failed: %v", pf.Path, err)
			}
			resolved[pf.Path] = fr
		}
	})

	return batch.Files, resolved
}

func TestBuildGraphNodesEdgesIndexesAndDeterminism(t *testing.T) {
	inputs := []parser.FileInput{
		{
			Path: "app/base.py",
			Source: []byte(`def tag(fn):
    return fn

class BaseService:
    def ping(self) -> str:
        return "pong"
`),
		},
		{
			Path: "app/service.py",
			Source: []byte(`import json
from .base import BaseService, tag

@tag
class OrderService(BaseService):
    def handle(self) -> str:
        _ = json.dumps({"ok": True})
        return self.ping()
`),
		},
	}

	parsedA, resA := parseAndResolveFixture(t, inputs)
	gA, err := pipeline.BuildGraph(context.Background(), parsedA, resA)
	if err != nil {
		t.Fatalf("BuildGraph(A) failed: %v", err)
	}

	// Build second graph from reversed input slice order and verify snapshot byte identity.
	revInputs := []parser.FileInput{inputs[1], inputs[0]}
	parsedB, resB := parseAndResolveFixture(t, revInputs)
	gB, err := pipeline.BuildGraph(context.Background(), parsedB, resB)
	if err != nil {
		t.Fatalf("BuildGraph(B) failed: %v", err)
	}

	gA.View(func(v *graph.ReadView) {
		// Verify File, Module, Class, Function nodes present.
		var sawFile, sawMod, sawClass, sawMethod bool
		for _, n := range v.GetByFile("app/service.py") {
			switch n.Kind {
			case graph.KindFile:
				sawFile = true
			case graph.KindModule:
				sawMod = true
			case graph.KindClass:
				sawClass = true
			case graph.KindFunction:
				sawMethod = true
			}
		}
		if !sawFile || !sawMod || !sawClass || !sawMethod {
			t.Fatalf("missing expected node kinds in app/service.py: file=%v mod=%v cls=%v fn=%v", sawFile, sawMod, sawClass, sawMethod)
		}

		// Verify File --CONTAINS--> Class & Module, File --IMPORTS--> app/base.py,
		// OrderService --INHERITS--> BaseService, OrderService --DECORATED_BY--> tag,
		// handle --CALLS--> ping, and external CallSite for json.dumps.
		edges := v.AllEdges()
		hasEdgeKind := func(kind graph.EdgeKind, targetSubstr string) bool {
			for _, e := range edges {
				if e.Kind != kind {
					continue
				}
				if targetSubstr == "" {
					return true
				}
				if tgt, ok := v.GetNode(e.Target); ok && tgt.QName == targetSubstr {
					return true
				}
			}
			return false
		}
		for _, chk := range []struct {
			kind   graph.EdgeKind
			target string
		}{
			{graph.EdgeContains, "mod:app/service.py"},
			{graph.EdgeImports, "app/base.py"},
			{graph.EdgeInherits, "app.base::BaseService"},
			{graph.EdgeDecoratedBy, "app.base::tag"},
			{graph.EdgeCalls, "app.base::BaseService::ping"},
		} {
			if !hasEdgeKind(chk.kind, chk.target) {
				t.Fatalf("missing %s -> %s edge in graph", chk.kind, chk.target)
			}
		}

		sites := v.GetCallSitesByFile("app/service.py")
		if len(sites) == 0 || sites[0].Status != graph.CallStatusExternal {
			t.Fatalf("expected external CallSite in app/service.py, got %+v", sites)
		}

		if deps := v.GetDependentFiles(v.GetByQName("app.base::BaseService")[0].UID); !slices.Contains(deps, "app/service.py") {
			t.Fatalf("expected app/service.py in rev-deps of BaseService, got %v", deps)
		}
	})

	dir := t.TempDir()
	snapA := filepath.Join(dir, "a.ggctx")
	snapB := filepath.Join(dir, "b.ggctx")
	if _, err := storage.WriteSnapshot(snapA, gA, storage.WriteOptions{Compress: false}); err != nil {
		t.Fatalf("WriteSnapshot(A) failed: %v", err)
	}
	if _, err := storage.WriteSnapshot(snapB, gB, storage.WriteOptions{Compress: false}); err != nil {
		t.Fatalf("WriteSnapshot(B) failed: %v", err)
	}
	bytesA, err := os.ReadFile(snapA)
	bytesB, err2 := os.ReadFile(snapB)
	if err != nil || err2 != nil {
		t.Fatalf("read snapshots failed: %v %v", err, err2)
	}
	if !bytes.Equal(bytesA, bytesB) {
		t.Fatalf("expected deterministic byte-identical snapshots across input orderings (%d vs %d bytes)", len(bytesA), len(bytesB))
	}
}
