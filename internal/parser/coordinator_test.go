package parser_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/roshankumar0036singh/gographctx/internal/parser"
	"github.com/roshankumar0036singh/gographctx/internal/parser/lang"
	"github.com/roshankumar0036singh/gographctx/internal/parser/treesitter"
)

func buildTestRegistry(t testing.TB) (*treesitter.Manager, *parser.Registry) {
	t.Helper()
	mgr, err := treesitter.NewManager()
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	reg := parser.NewRegistry()
	if _, err := lang.RegisterGo(reg, mgr); err != nil {
		t.Fatalf("RegisterGo: %v", err)
	}
	if _, err := lang.RegisterPython(reg, mgr); err != nil {
		t.Fatalf("RegisterPython: %v", err)
	}
	if _, err := lang.RegisterTypeScript(reg, mgr); err != nil {
		t.Fatalf("RegisterTypeScript: %v", err)
	}
	if _, err := lang.RegisterTSX(reg, mgr); err != nil {
		t.Fatalf("RegisterTSX: %v", err)
	}
	if _, err := lang.RegisterJavaScript(reg, mgr); err != nil {
		t.Fatalf("RegisterJavaScript: %v", err)
	}
	if _, err := lang.RegisterRust(reg, mgr); err != nil {
		t.Fatalf("RegisterRust: %v", err)
	}
	if _, err := lang.RegisterJava(reg, mgr); err != nil {
		t.Fatalf("RegisterJava: %v", err)
	}
	return mgr, reg
}

func TestCoordinatorMixedLanguageDeterminismAndNameIndex(t *testing.T) {
	mgr, reg := buildTestRegistry(t)
	defer mgr.Close()

	coord, err := parser.NewCoordinator(reg, parser.CoordinatorConfig{
		Workers:           4,
		DeterministicSort: true,
	})
	if err != nil {
		t.Fatalf("NewCoordinator failed: %v", err)
	}
	if coord.Workers() != 4 {
		t.Fatalf("Workers() = %d, want 4", coord.Workers())
	}

	grouped := map[string][]parser.FileInput{
		"python": {
			{
				Path:   "svc/api.py",
				Source: []byte("class OrderHandler:\n    def handle(self):\n        return 1\n"),
			},
		},
		"go": {
			{
				Path:   "cmd/server/main.go",
				Source: []byte("package main\nfunc Handle() {}\n"),
			},
		},
		"rust": {
			{
				Path:     "src/store/mod.rs",
				DiskPath: filepath.Join("lang", "testdata", "rust_crate_sample.rs"),
			},
		},
		"java": {
			{
				Path:     "com/example/orders/OrderService.java",
				DiskPath: filepath.Join("lang", "testdata", "OrderService.java"),
			},
		},
		"typescript": {
			{
				Path:     "src/orders/sample_service.ts",
				DiskPath: filepath.Join("lang", "testdata", "sample_service.ts"),
			},
		},
	}

	res1, err := coord.ParseGrouped(context.Background(), grouped)
	if err != nil {
		t.Fatalf("ParseGrouped run 1 failed: %v", err)
	}
	res2, err := coord.ParseGrouped(context.Background(), grouped)
	if err != nil {
		t.Fatalf("ParseGrouped run 2 failed: %v", err)
	}
	if res1.HasErrors() || res1.Err() != nil {
		t.Fatalf("unexpected batch errors: %v", res1.Err())
	}
	if len(res1.Files) != 5 || len(res2.Files) != 5 {
		t.Fatalf("parsed file count = (%d, %d), want 5", len(res1.Files), len(res2.Files))
	}

	// Deterministic canonical ordering across runs.
	var paths1, paths2 []string
	for i := range res1.Files {
		paths1 = append(paths1, res1.Files[i].Path)
		paths2 = append(paths2, res2.Files[i].Path)
	}
	if !slices.Equal(paths1, paths2) {
		t.Fatalf("non-deterministic file ordering: %v vs %v", paths1, paths2)
	}
	if !slices.IsSorted(paths1) {
		t.Fatalf("expected sorted paths with DeterministicSort=true, got %v", paths1)
	}

	// Verify NameIndex single-pass lookups.
	if files := res1.NameIndex.FilesForSymbol("OrderService"); !slices.Equal(files, []string{"com/example/orders/OrderService.java", "src/orders/sample_service.ts"}) {
		t.Fatalf("FilesForSymbol(OrderService) = %v", files)
	}
	if q := res1.NameIndex.LookupQName("src::store::OrderClient"); len(q) != 1 || q[0].Language != "rust" {
		t.Fatalf("LookupQName(src::store::OrderClient) = %+v", q)
	}
	if pkgFiles := res1.NameIndex.FilesForPackage("com.example.orders"); !slices.Equal(pkgFiles, []string{"com/example/orders/OrderService.java"}) {
		t.Fatalf("FilesForPackage(com.example.orders) = %v", pkgFiles)
	}
	if len(res1.NameIndex.LookupName("Handle")) != 1 {
		t.Fatalf("expected Handle symbol in NameIndex")
	}
}

func TestCoordinatorErrorsFailFastAndCancel(t *testing.T) {
	if _, err := parser.NewCoordinator(nil, parser.CoordinatorConfig{}); !errors.Is(err, parser.ErrNilCoordinator) {
		t.Fatalf("NewCoordinator(nil) = %v, want ErrNilCoordinator", err)
	}

	mgr, reg := buildTestRegistry(t)
	defer mgr.Close()

	// Default FailFast=false collects failures without aborting valid files.
	coord, err := parser.NewCoordinator(reg, parser.CoordinatorConfig{})
	if err != nil {
		t.Fatalf("NewCoordinator failed: %v", err)
	}
	if coord.Workers() < 1 {
		t.Fatalf("expected Workers() >= 1, got %d", coord.Workers())
	}

	res, err := coord.ParseFiles(context.Background(), []parser.FileInput{
		{Path: "good.go", Source: []byte("package good\nfunc Ok() {}\n")},
		{Path: "unknown.xyz", Source: []byte("whatever")},
		{Path: "missing.py", DiskPath: filepath.Join(t.TempDir(), "does_not_exist.py")},
	})
	if err != nil {
		t.Fatalf("ContinueOnError returned fatal err: %v", err)
	}
	if !res.HasErrors() || len(res.Errors) != 2 || res.Err() == nil {
		t.Fatalf("expected 2 FileErrors collected, got %+v", res.Errors)
	}
	var fe parser.FileError
	if !errors.As(res.Err(), &fe) || fe.Path != "unknown.xyz" {
		t.Fatalf("errors.As(res.Err(), &fe) mismatch: fe=%+v err=%v", fe, res.Err())
	}
	if len(res.Files) != 1 || res.Files[0].Path != "good.go" {
		t.Fatalf("expected healthy good.go parsed, got %+v", res.Files)
	}

	// FailFast=true aborts and returns error matching FileError.
	fastCoord, err := parser.NewCoordinator(reg, parser.CoordinatorConfig{Workers: 2, FailFast: true})
	if err != nil {
		t.Fatalf("NewCoordinator FailFast failed: %v", err)
	}
	_, ffErr := fastCoord.ParseFiles(context.Background(), []parser.FileInput{
		{Path: "bad.unknown", Source: []byte("x")},
	})
	var ffFileErr parser.FileError
	if ffErr == nil || !errors.As(ffErr, &ffFileErr) || ffFileErr.Path != "bad.unknown" {
		t.Fatalf("FailFast errors.As mismatch: err=%v fe=%+v", ffErr, ffFileErr)
	}

	// Mixed ./ and backslash paths sort consistently across Files and NameIndex.
	mixedCoord, err := parser.NewCoordinator(reg, parser.CoordinatorConfig{Workers: 2, DeterministicSort: true})
	if err != nil {
		t.Fatalf("NewCoordinator mixed failed: %v", err)
	}
	mixedRes, err := mixedCoord.ParseFiles(context.Background(), []parser.FileInput{
		{Path: `pkg\zeta.go`, Source: []byte("package pkg\nfunc Shared() {}\n")},
		{Path: "./pkg/alpha.go", Source: []byte("package pkg\nfunc Shared() {}\n")},
	})
	if err != nil || mixedRes.HasErrors() || len(mixedRes.Files) != 2 {
		t.Fatalf("mixed paths parse failed: err=%v errs=%v", err, mixedRes.Errors)
	}
	if mixedRes.Files[0].Path != "./pkg/alpha.go" || mixedRes.Files[1].Path != `pkg\zeta.go` {
		t.Fatalf("mixed path Files order = [%q, %q]", mixedRes.Files[0].Path, mixedRes.Files[1].Path)
	}
	if got := mixedRes.NameIndex.FilesForSymbol("Shared"); !slices.Equal(got, []string{"./pkg/alpha.go", `pkg\zeta.go`}) {
		t.Fatalf("mixed path NameIndex.FilesForSymbol order = %v", got)
	}

	// Canceled context fails promptly.
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := coord.ParseFiles(canceled, []parser.FileInput{{Path: "a.go"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled ParseFiles = %v, want context.Canceled", err)
	}
}

func TestCoordinator100FilesMixedLanguagesDeterministic(t *testing.T) {
	mgr, reg := buildTestRegistry(t)
	defer mgr.Close()

	coord, err := parser.NewCoordinator(reg, parser.CoordinatorConfig{
		Workers:           6,
		DeterministicSort: true,
	})
	if err != nil {
		t.Fatalf("NewCoordinator failed: %v", err)
	}

	inputs := make([]parser.FileInput, 0, 100)
	for i := 0; i < 25; i++ {
		inputs = append(inputs,
			parser.FileInput{Path: fmt.Sprintf("svc_%02d/handler.go", i), Source: []byte("package svc\nfunc Handle() {}\n")},
			parser.FileInput{Path: fmt.Sprintf("svc_%02d/worker.py", i), Source: []byte("def work():\n    return 1\n")},
			parser.FileInput{Path: fmt.Sprintf("svc_%02d/client.ts", i), Source: []byte("export function ping(): number { return 1; }\n")},
			parser.FileInput{Path: fmt.Sprintf("svc_%02d/store.rs", i), Source: []byte("pub fn save() -> bool { true }\n")},
		)
	}

	res1, err := coord.ParseFiles(context.Background(), inputs)
	if err != nil {
		t.Fatalf("ParseFiles failed: %v", err)
	}
	res2, err := coord.ParseFiles(context.Background(), inputs)
	if err != nil {
		t.Fatalf("ParseFiles run 2 failed: %v", err)
	}
	if res1.HasErrors() || len(res1.Files) != 100 || len(res2.Files) != 100 {
		t.Fatalf("unexpected 100-file parse result: errs=%v counts=(%d, %d)", res1.Errors, len(res1.Files), len(res2.Files))
	}
	for i := range res1.Files {
		if res1.Files[i].Path != res2.Files[i].Path {
			t.Fatalf("non-deterministic order at index %d: %q vs %q", i, res1.Files[i].Path, res2.Files[i].Path)
		}
	}
	if got := len(res1.NameIndex.FilesForSymbol("Handle")); got != 25 {
		t.Fatalf("FilesForSymbol(Handle) count = %d, want 25", got)
	}
}

func BenchmarkCoordinatorParallelVsSequential(b *testing.B) {
	mgr, reg := buildTestRegistry(b)
	defer mgr.Close()

	goSrc, err := os.ReadFile(filepath.Join("lang", "testdata", "sample_service.go"))
	if err != nil {
		b.Fatalf("ReadFile go fixture: %v", err)
	}
	pySrc, err := os.ReadFile(filepath.Join("lang", "testdata", "flask_service.py"))
	if err != nil {
		b.Fatalf("ReadFile py fixture: %v", err)
	}
	tsSrc, err := os.ReadFile(filepath.Join("lang", "testdata", "sample_service.ts"))
	if err != nil {
		b.Fatalf("ReadFile ts fixture: %v", err)
	}
	rsSrc, err := os.ReadFile(filepath.Join("lang", "testdata", "rust_crate_sample.rs"))
	if err != nil {
		b.Fatalf("ReadFile rs fixture: %v", err)
	}

	inputs := make([]parser.FileInput, 0, 100)
	for i := 0; i < 25; i++ {
		inputs = append(inputs,
			parser.FileInput{Path: "pkg/go_" + string(rune('a'+i%26)) + ".go", Source: goSrc},
			parser.FileInput{Path: "pkg/py_" + string(rune('a'+i%26)) + ".py", Source: pySrc},
			parser.FileInput{Path: "pkg/ts_" + string(rune('a'+i%26)) + ".ts", Source: tsSrc},
			parser.FileInput{Path: "pkg/rs_" + string(rune('a'+i%26)) + ".rs", Source: rsSrc},
		)
	}

	seqCoord, err := parser.NewCoordinator(reg, parser.CoordinatorConfig{Workers: 1})
	if err != nil {
		b.Fatalf("NewCoordinator seq: %v", err)
	}
	parCoord, err := parser.NewCoordinator(reg, parser.CoordinatorConfig{})
	if err != nil {
		b.Fatalf("NewCoordinator par: %v", err)
	}

	b.Run("Sequential", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			res, err := seqCoord.ParseFiles(context.Background(), inputs)
			if err != nil || res.HasErrors() || len(res.Files) != len(inputs) {
				b.Fatalf("seq failed: err=%v errs=%v len=%d", err, res.Errors, len(res.Files))
			}
		}
	})

	b.Run("Parallel", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			res, err := parCoord.ParseFiles(context.Background(), inputs)
			if err != nil || res.HasErrors() || len(res.Files) != len(inputs) {
				b.Fatalf("par failed: err=%v errs=%v len=%d", err, res.Errors, len(res.Files))
			}
		}
	})
}
