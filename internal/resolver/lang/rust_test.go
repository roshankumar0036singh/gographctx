package lang_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/roshankumar0036singh/gographctx/internal/graph"
	"github.com/roshankumar0036singh/gographctx/internal/parser"
	parserlang "github.com/roshankumar0036singh/gographctx/internal/parser/lang"
	"github.com/roshankumar0036singh/gographctx/internal/parser/treesitter"
	"github.com/roshankumar0036singh/gographctx/internal/resolver"
	resolverlang "github.com/roshankumar0036singh/gographctx/internal/resolver/lang"
)

func TestParseCargoTOML(t *testing.T) {
	dir := t.TempDir()
	cargo := `[package]
name = "acme-orders"
version = "0.1.0"
`
	if err := os.WriteFile(filepath.Join(dir, "Cargo.toml"), []byte(cargo), 0o600); err != nil {
		t.Fatalf("WriteFile Cargo.toml failed: %v", err)
	}
	info, err := resolverlang.ParseCargoTOML(dir)
	if err != nil {
		t.Fatalf("ParseCargoTOML failed: %v", err)
	}
	if info.PackageName != "acme-orders" || info.CrateIdent != "acme_orders" {
		t.Fatalf("unexpected CargoInfo: %+v", info)
	}
}

func TestRustResolverCrateModulesAndTraits(t *testing.T) {
	repoDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoDir, "Cargo.toml"), []byte("[package]\nname = \"acme-orders\"\nversion = \"0.1.0\"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile Cargo.toml failed: %v", err)
	}

	mgr, err := treesitter.NewManager()
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	defer mgr.Close()

	reg := parser.NewRegistry()
	if _, err := parserlang.RegisterRust(reg, mgr); err != nil {
		t.Fatalf("RegisterRust failed: %v", err)
	}
	coord, err := parser.NewCoordinator(reg, parser.CoordinatorConfig{DeterministicSort: true})
	if err != nil {
		t.Fatalf("NewCoordinator failed: %v", err)
	}

	batch, err := coord.ParseFiles(context.Background(), []parser.FileInput{
		{
			Path: "src/lib.rs",
			Source: []byte(`pub mod store;
pub mod service;

pub use crate::store::MemoryRepo;
`),
		},
		{
			Path: "src/store/mod.rs",
			Source: []byte(`pub mod repo;
pub use self::repo::MemoryRepo;
`),
		},
		{
			Path: "src/store/repo.rs",
			Source: []byte(`pub trait Repository {
    fn save(&self, id: &str);
}

pub struct MemoryRepo;

impl MemoryRepo {
    pub fn new() -> Self {
        MemoryRepo
    }
}

impl Repository for MemoryRepo {
    fn save(&self, id: &str) {
        std::mem::drop(id);
    }
}
`),
		},
		{
			Path: "src/service.rs",
			Source: []byte(`use acme_orders::MemoryRepo;
use crate::store::repo::Repository;

pub fn audit_log(msg: &str) {
    println!("{}", msg);
}

pub struct OrderService {
    repo: MemoryRepo,
}

impl OrderService {
    pub fn new(repo: MemoryRepo) -> Self {
        OrderService { repo }
    }

    pub fn process(&self, id: &str) {
        self.repo.save(id);
        self.notify(id);
    }

    fn notify(&self, id: &str) {
        audit_log(id);
    }
}

pub fn build_repo() -> Result<Box<MemoryRepo>, &'static str> {
    Ok(Box::new(MemoryRepo::new()))
}

pub fn run_once() {
    let repo = MemoryRepo::new();
    let svc = OrderService::new(repo);
    svc.process("ord-1");
    let boxed = build_repo();
    boxed.save("ord-2");
}
`),
		},
	})
	if err != nil || len(batch.Errors) > 0 {
		t.Fatalf("ParseFiles failed: err=%v errs=%v", err, batch.Errors)
	}

	g := graph.New()
	for _, pf := range batch.Files {
		if err := resolverlang.AddParsedFileToGraph(g, pf); err != nil {
			t.Fatalf("AddParsedFileToGraph(%q) failed: %v", pf.Path, err)
		}
	}

	resReg := resolver.NewRegistry()
	resReg.MustRegister(resolverlang.NewRustResolver())

	var serviceRes *resolver.FileResolution
	g.View(func(view *graph.ReadView) {
		rc := &resolver.ResolutionContext{
			View:      view,
			NameIndex: batch.NameIndex,
			RepoRoot:  repoDir,
		}
		rc.IndexFiles(batch.Files)

		for _, pf := range batch.Files {
			fr, err := resReg.ResolveFile(context.Background(), pf, rc)
			if err != nil {
				t.Fatalf("ResolveFile(%q) failed: %v", pf.Path, err)
			}
			for _, rcCall := range fr.Resolved {
				if err := rcCall.Validate(); err != nil {
					t.Errorf("invalid ResolvedCall in %s: %v", pf.Path, err)
				}
			}
			for _, ucCall := range fr.Unresolved {
				if err := ucCall.Validate(); err != nil {
					t.Errorf("invalid UnresolvedCall in %s: %v", pf.Path, err)
				}
			}
			if pf.Path == "src/service.rs" {
				serviceRes = fr
			}
		}
	})

	if serviceRes == nil {
		t.Fatalf("missing resolution for src/service.rs")
	}

	hasResolved := func(fr *resolver.FileResolution, targetQName string, tier graph.ResolutionTier) bool {
		for _, rcCall := range fr.Resolved {
			if rcCall.TargetQName == targetQName && rcCall.Tier == tier {
				return true
			}
		}
		return false
	}

	checks := []struct {
		target string
		tier   graph.ResolutionTier
		desc   string
	}{
		{"src::store::repo::MemoryRepo::save", resolver.TierReceiverType, "trait impl method self.repo.save()"},
		{"src::service::OrderService::notify", resolver.TierSelf, "self.notify()"},
		{"src::service::audit_log", resolver.TierLocal, "same-file audit_log()"},
		{"src::store::repo::MemoryRepo::new", resolver.TierQualifiedImport, "re-exported MemoryRepo::new()"},
		{"src::service::OrderService::new", resolver.TierLocal, "OrderService::new()"},
		{"src::service::OrderService::process", resolver.TierReceiverType, "svc.process()"},
		{"src::service::build_repo", resolver.TierLocal, "local build_repo()"},
	}
	for _, chk := range checks {
		if !hasResolved(serviceRes, chk.target, chk.tier) {
			t.Errorf("service.rs missing %s (%s @ tier %s); resolved=%+v unresolved=%+v",
				chk.desc, chk.target, chk.tier, serviceRes.Resolved, serviceRes.Unresolved)
		}
	}

	// Verify println! classified as CallStatusExternal macro.
	var sawMacroExt bool
	for _, uc := range serviceRes.Unresolved {
		if uc.CalleeExpr == "println!" && uc.Status == graph.CallStatusExternal {
			sawMacroExt = true
		}
	}
	if !sawMacroExt {
		t.Fatalf("expected println! macro classified as CallStatusExternal; unresolved=%+v", serviceRes.Unresolved)
	}
}
