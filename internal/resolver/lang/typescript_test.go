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

func TestParseTSConfigAndPackageJSON(t *testing.T) {
	dir := t.TempDir()
	tsconfig := `{
  // Base TypeScript config with trailing commas
  "compilerOptions": {
    "baseUrl": "./src",
    "paths": {
      "@core/*": ["core/*"],
      "@ui": ["components/index.ts"],
    },
  },
}`
	if err := os.WriteFile(filepath.Join(dir, "tsconfig.json"), []byte(tsconfig), 0o600); err != nil {
		t.Fatalf("WriteFile tsconfig.json failed: %v", err)
	}
	cfg, err := resolverlang.ParseTSConfig(dir)
	if err != nil {
		t.Fatalf("ParseTSConfig failed: %v", err)
	}
	if cfg.BaseURL != "src" {
		t.Fatalf("expected BaseURL=src, got %q", cfg.BaseURL)
	}
	if len(cfg.Paths["@core/*"]) != 1 || cfg.Paths["@core/*"][0] != "core/*" {
		t.Fatalf("unexpected paths: %+v", cfg.Paths)
	}

	pkgJSON := `{
  "name": "acme-kit",
  "main": "./dist/index.cjs",
  "exports": {
    ".": {
      "import": "./dist/index.mjs",
      "default": "./dist/index.cjs"
    },
    "./format": "./dist/format.js"
  }
}`
	pkgPath := filepath.Join(dir, "package.json")
	if err := os.WriteFile(pkgPath, []byte(pkgJSON), 0o600); err != nil {
		t.Fatalf("WriteFile package.json failed: %v", err)
	}
	pkg, err := resolverlang.ParsePackageJSON(pkgPath)
	if err != nil {
		t.Fatalf("ParsePackageJSON failed: %v", err)
	}
	if pkg.Exports["."] != "./dist/index.mjs" || pkg.Exports["./format"] != "./dist/format.js" {
		t.Fatalf("unexpected package exports: %+v", pkg.Exports)
	}

	// Ensure unclosed /* comment at EOF does not fail or panic.
	unclosedDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(unclosedDir, "tsconfig.json"), []byte(`{"compilerOptions":{"baseUrl":"src"}} /* unclosed`), 0o600); err != nil {
		t.Fatalf("WriteFile unclosed tsconfig failed: %v", err)
	}
	uCfg, err := resolverlang.ParseTSConfig(unclosedDir)
	if err != nil || uCfg.BaseURL != "src" {
		t.Fatalf("ParseTSConfig unclosed comment = %+v, err=%v", uCfg, err)
	}
}

func TestTypeScriptAndJSXResolverEndToEnd(t *testing.T) {
	repoDir := t.TempDir()
	tsconfig := `{
  "compilerOptions": {
    "baseUrl": ".",
    "paths": {
      "@core/*": ["src/core/*"],
      "@components": ["src/components/index.ts"]
    }
  }
}`
	if err := os.WriteFile(filepath.Join(repoDir, "tsconfig.json"), []byte(tsconfig), 0o600); err != nil {
		t.Fatalf("WriteFile tsconfig.json failed: %v", err)
	}
	nmDir := filepath.Join(repoDir, "node_modules", "acme-logger")
	if err := os.MkdirAll(nmDir, 0o755); err != nil {
		t.Fatalf("MkdirAll node_modules failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nmDir, "package.json"), []byte(`{"name":"acme-logger","main":"./index.js"}`), 0o600); err != nil {
		t.Fatalf("WriteFile node_modules package.json failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nmDir, "index.js"), []byte(`export function info(msg) { return msg; }`), 0o600); err != nil {
		t.Fatalf("WriteFile node_modules index.js failed: %v", err)
	}

	mgr, err := treesitter.NewManager()
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	defer mgr.Close()

	reg := parser.NewRegistry()
	if _, err := parserlang.RegisterTypeScript(reg, mgr); err != nil {
		t.Fatalf("RegisterTypeScript failed: %v", err)
	}
	if _, err := parserlang.RegisterTSX(reg, mgr); err != nil {
		t.Fatalf("RegisterTSX failed: %v", err)
	}
	if _, err := parserlang.RegisterJavaScript(reg, mgr); err != nil {
		t.Fatalf("RegisterJavaScript failed: %v", err)
	}
	coord, err := parser.NewCoordinator(reg, parser.CoordinatorConfig{DeterministicSort: true})
	if err != nil {
		t.Fatalf("NewCoordinator failed: %v", err)
	}

	batch, err := coord.ParseFiles(context.Background(), []parser.FileInput{
		{
			Path: "src/core/base.ts",
			Source: []byte(`export class BaseRepo {
  connect(): string {
    return "connected";
  }
}
`),
		},
		{
			Path: "src/core/store.ts",
			Source: []byte(`import { BaseRepo } from "./base";

export class OrderStore extends BaseRepo {
  save(id: string): string {
    return this.connect();
  }
}
`),
		},
		{
			Path: "src/core/index.ts",
			Source: []byte(`export { OrderStore as RepoStore } from "./store";
`),
		},
		{
			Path: "src/utils/format.js",
			Source: []byte(`function cleanId(raw) {
  return raw.trim();
}
module.exports = { cleanId };
`),
		},
		{
			Path: "src/components/Banner.tsx",
			Source: []byte(`export function Banner() {
  return null;
}
`),
		},
		{
			Path: "src/components/index.ts",
			Source: []byte(`export * from "./Banner";
`),
		},
		{
			Path: "src/app.tsx",
			Source: []byte(`import { info } from "acme-logger";
import { RepoStore } from "@core/index";
import * as UI from "@components";
import { Banner } from "@components";
const fmt = require("./utils/format");

function localAudit(id: string): void {
  info(id);
}

export class OrderPage {
  private readonly store: RepoStore;

  constructor(store: RepoStore) {
    this.store = store;
  }

  renderOrder(id: string): string {
    const clean = fmt.cleanId(id);
    localAudit(clean);
    <Banner />;
    <UI.Banner />;
    const adHoc = new RepoStore();
    return adHoc.save(clean) + this.store.save(clean);
  }
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
	resReg.MustRegister(resolverlang.NewTypeScriptResolver())
	resReg.MustRegister(resolverlang.NewTSXResolver())
	resReg.MustRegister(resolverlang.NewJavaScriptResolver())

	resByFile := make(map[string]*resolver.FileResolution, len(batch.Files))
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
			resByFile[pf.Path] = fr
		}
	})

	appRes := resByFile["src/app.tsx"]
	if appRes == nil {
		t.Fatalf("missing resolution for src/app.tsx")
	}

	// Verify node_modules resolution in Imports (TargetFile set & IsExternal=true).
	var sawNodeMod bool
	for _, imp := range appRes.Imports {
		if imp.ImportPath == "acme-logger" {
			if !imp.IsExternal || imp.TargetFile != "node_modules/acme-logger/index.js" {
				t.Fatalf("expected node_modules/acme-logger/index.js external import, got %+v", imp)
			}
			sawNodeMod = true
		}
	}
	if !sawNodeMod {
		t.Fatalf("missing acme-logger import in app.tsx: %+v", appRes.Imports)
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
		{"src/utils/format::cleanId", resolver.TierQualifiedImport, "CommonJS require fmt.cleanId()"},
		{"src/app::localAudit", resolver.TierLocal, "same-file localAudit()"},
		{"src/components/Banner::Banner", resolver.TierQualifiedImport, "JSX <Banner /> via barrel export *"},
		{"src/core/store::OrderStore", resolver.TierQualifiedImport, "new RepoStore() via @core/index barrel re-export"},
		{"src/core/store::OrderStore::save", resolver.TierReceiverType, "adHoc.save() / this.store.save()"},
	}
	for _, chk := range checks {
		if !hasResolved(appRes, chk.target, chk.tier) {
			t.Errorf("app.tsx missing %s (%s @ tier %s); resolved=%+v unresolved=%+v",
				chk.desc, chk.target, chk.tier, appRes.Resolved, appRes.Unresolved)
		}
	}

	// Verify inherited `this.connect()` in src/core/store.ts -> BaseRepo::connect.
	storeRes := resByFile["src/core/store.ts"]
	if !hasResolved(storeRes, "src/core/base::BaseRepo::connect", resolver.TierSelf) {
		t.Fatalf("store.ts expected TierSelf inherited BaseRepo::connect, got resolved=%+v", storeRes.Resolved)
	}

	// Verify node_modules `info(id)` classified as CallStatusExternal.
	var sawExtInfo bool
	for _, uc := range appRes.Unresolved {
		if uc.CalleeExpr == "info" && uc.Status == graph.CallStatusExternal {
			sawExtInfo = true
		}
	}
	if !sawExtInfo {
		t.Fatalf("expected external call classification for info(id); unresolved=%+v", appRes.Unresolved)
	}
}
