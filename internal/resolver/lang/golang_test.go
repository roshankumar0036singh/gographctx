package lang_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/roshankumar0036singh/gographctx/internal/graph"
	"github.com/roshankumar0036singh/gographctx/internal/parser"
	parserlang "github.com/roshankumar0036singh/gographctx/internal/parser/lang"
	"github.com/roshankumar0036singh/gographctx/internal/parser/treesitter"
	"github.com/roshankumar0036singh/gographctx/internal/resolver"
	resolverlang "github.com/roshankumar0036singh/gographctx/internal/resolver/lang"
)

func TestParseGoModAndReplace(t *testing.T) {
	dir := t.TempDir()
	modContent := `// sample module
module github.com/example/shop

go 1.22

replace github.com/example/shared => ./local/shared

replace (
	github.com/example/ext => github.com/fork/ext v1.2.0
)
`
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(modContent), 0o600); err != nil {
		t.Fatalf("WriteFile go.mod failed: %v", err)
	}
	info, err := resolverlang.ParseGoMod(dir)
	if err != nil {
		t.Fatalf("ParseGoMod failed: %v", err)
	}
	if info.ModulePath != "github.com/example/shop" || info.GoVersion != "1.22" {
		t.Fatalf("GoModInfo header mismatch: %+v", info)
	}
	if info.Replaces["github.com/example/shared"] != "local/shared" {
		t.Fatalf("GoModInfo Replaces = %+v", info.Replaces)
	}
	if _, hasExt := info.Replaces["github.com/example/ext"]; hasExt {
		t.Fatalf("non-local replace should not populate local replace map: %+v", info.Replaces)
	}
}

func TestGoResolverMultiPackageResolution(t *testing.T) {
	repoDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoDir, "go.mod"), []byte("module github.com/example/shop\n\ngo 1.22\n"), 0o600); err != nil {
		t.Fatalf("WriteFile go.mod failed: %v", err)
	}

	mgr, err := treesitter.NewManager()
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	defer mgr.Close()

	reg := parser.NewRegistry()
	if _, err := parserlang.RegisterGo(reg, mgr); err != nil {
		t.Fatalf("RegisterGo failed: %v", err)
	}
	coord, err := parser.NewCoordinator(reg, parser.CoordinatorConfig{DeterministicSort: true})
	if err != nil {
		t.Fatalf("NewCoordinator failed: %v", err)
	}

	batch, err := coord.ParseFiles(context.Background(), []parser.FileInput{
		{
			Path: "internal/store/store.go",
			Source: []byte(`package store

import "fmt"

type Order struct {
	ID string
}

type MemoryRepo struct{}

func NewMemoryRepo() *MemoryRepo {
	return &MemoryRepo{}
}

func (r *MemoryRepo) Load(id string) (*Order, error) {
	if id == "" {
		return nil, fmt.Errorf("empty")
	}
	return &Order{ID: id}, nil
}
`),
		},
		{
			Path: "internal/service/helpers.go",
			Source: []byte(`package service

func normalizeID(raw string) string {
	return raw
}
`),
		},
		{
			Path: "internal/service/service.go",
			Source: []byte(`package service

import (
	"fmt"
	"github.com/example/shop/internal/store"
)

type ReaderStore interface {
	Load(id string) (*store.Order, error)
}

type BaseAuditor struct{}

func (b *BaseAuditor) Audit(event string) {
	_ = fmt.Sprintf("audit:%s", event)
}

type OrderService struct {
	BaseAuditor
	repo ReaderStore
}

func NewOrderService(repo ReaderStore) *OrderService {
	return &OrderService{repo: repo}
}

func init() {
	_ = normalizeID("boot")
}

func init() {
	_ = localHelper()
}

func localHelper() string {
	return "ok"
}

func (s *OrderService) Fetch(id string) (*store.Order, error) {
	clean := normalizeID(id)
	s.Audit(clean)
	return s.repo.Load(clean)
}
`),
		},
		{
			Path: "cmd/app/main.go",
			Source: []byte(`package main

import (
	"github.com/example/shop/internal/service"
	"github.com/example/shop/internal/store"
)

func main() {
	repo := store.NewMemoryRepo()
	svc := service.NewOrderService(repo)
	_, _ = svc.Fetch("ord-1")
	svc.Audit("started")
}
`),
		},
	})
	if err != nil || batch.HasErrors() {
		t.Fatalf("ParseFiles failed: err=%v errs=%v", err, batch.Errors)
	}

	g := graph.New()
	for _, pf := range batch.Files {
		if err := resolverlang.AddParsedFileToGraph(g, pf); err != nil {
			t.Fatalf("AddParsedFileToGraph(%q) failed: %v", pf.Path, err)
		}
	}

	resReg := resolver.NewRegistry()
	goRes := resolverlang.NewGoResolver()
	resReg.MustRegister(goRes)

	var serviceRes, mainRes *resolver.FileResolution
	var sats []resolverlang.InterfaceSatisfaction

	g.View(func(view *graph.ReadView) {
		rc := &resolver.ResolutionContext{
			View:      view,
			NameIndex: batch.NameIndex,
			RepoRoot:  repoDir,
		}
		for _, pf := range batch.Files {
			fr, err := resReg.ResolveFile(context.Background(), pf, rc)
			if err != nil {
				t.Fatalf("ResolveFile(%q) failed: %v", pf.Path, err)
			}
			for _, rcCall := range fr.Resolved {
				if err := rcCall.Validate(); err != nil {
					t.Errorf("invalid ResolvedCall in %s: %v (%+v)", pf.Path, err, rcCall)
				}
			}
			for _, ucCall := range fr.Unresolved {
				if err := ucCall.Validate(); err != nil {
					t.Errorf("invalid UnresolvedCall in %s: %v (%+v)", pf.Path, err, ucCall)
				}
			}
			switch pf.Path {
			case "internal/service/service.go":
				serviceRes = fr
			case "cmd/app/main.go":
				mainRes = fr
			}
		}
		sats = resolverlang.DetectInterfaceSatisfaction(view)
	})

	// Check imports on cmd/app/main.go (project-local vs external).
	if len(mainRes.Imports) != 2 || mainRes.Imports[0].IsExternal || mainRes.Imports[1].IsExternal {
		t.Fatalf("main.go imports should both resolve locally: %+v", mainRes.Imports)
	}

	hasResolved := func(fr *resolver.FileResolution, targetQName string, tier graph.ResolutionTier) bool {
		for _, rcCall := range fr.Resolved {
			if rcCall.TargetQName == targetQName && rcCall.Tier == tier {
				return true
			}
		}
		return false
	}

	// Verify strong-tier resolutions in internal/service/service.go:
	// - TierLocal: localHelper()
	// - TierSamePackage: normalizeID() in helpers.go
	// - TierSelf via embedded struct: s.Audit()
	// - TierReceiverType: s.repo.Load() -> ReaderStore::Load
	if !hasResolved(serviceRes, "internal/service::localHelper", resolver.TierLocal) {
		t.Fatalf("expected TierLocal for localHelper(): %+v", serviceRes.Resolved)
	}
	if !hasResolved(serviceRes, "internal/service::normalizeID", resolver.TierSamePackage) {
		t.Fatalf("expected TierSamePackage for normalizeID(): %+v", serviceRes.Resolved)
	}
	if !hasResolved(serviceRes, "internal/service::BaseAuditor::Audit", resolver.TierSelf) {
		t.Fatalf("expected TierSelf for embedded s.Audit(): %+v", serviceRes.Resolved)
	}
	if !hasResolved(serviceRes, "internal/service::ReaderStore::Load", resolver.TierReceiverType) {
		t.Fatalf("expected TierReceiverType for s.repo.Load(): %+v", serviceRes.Resolved)
	}

	// Verify main.go cross-package calls & inferred receiver methods + embedded method:
	// - store.NewMemoryRepo() -> TierQualifiedImport
	// - service.NewOrderService() -> TierQualifiedImport
	// - svc.Fetch() -> TierReceiverType
	// - svc.Audit() -> TierReceiverType via embedded BaseAuditor
	if !hasResolved(mainRes, "internal/store::NewMemoryRepo", resolver.TierQualifiedImport) ||
		!hasResolved(mainRes, "internal/service::NewOrderService", resolver.TierQualifiedImport) ||
		!hasResolved(mainRes, "internal/service::OrderService::Fetch", resolver.TierReceiverType) ||
		!hasResolved(mainRes, "internal/service::BaseAuditor::Audit", resolver.TierReceiverType) {
		t.Fatalf("main.go expected cross-package/receiver resolutions, got: %+v (unresolved=%+v)", mainRes.Resolved, mainRes.Unresolved)
	}
	if len(mainRes.Resolved) != 4 || len(mainRes.Unresolved) != 0 {
		t.Fatalf("expected 100%% (4/4) internal calls resolved in main.go; got resolved=%d unresolved=%d", len(mainRes.Resolved), len(mainRes.Unresolved))
	}

	// Verify stdlib fmt.Sprintf in service.go is classified external (never emitted as CALLS).
	var sawFmtExternal bool
	for _, uc := range serviceRes.Unresolved {
		if uc.CalleeExpr == "fmt.Sprintf" && uc.Status == graph.CallStatusExternal && uc.Tier == resolver.TierUnresolved {
			sawFmtExternal = true
		}
	}
	if !sawFmtExternal {
		t.Fatalf("expected fmt.Sprintf recorded as CallStatusExternal in service.go: %+v", serviceRes.Unresolved)
	}

	// Verify DetectInterfaceSatisfaction recognizes MemoryRepo satisfies ReaderStore.
	var sawSatisfaction bool
	for _, sat := range sats {
		if sat.StructQName == "internal/store::MemoryRepo" &&
			sat.InterfaceQName == "internal/service::ReaderStore" &&
			slices.Equal(sat.MatchedMethods, []string{"Load"}) {
			sawSatisfaction = true
		}
	}
	if !sawSatisfaction {
		t.Fatalf("expected MemoryRepo -> ReaderStore interface satisfaction, got %+v", sats)
	}
}
