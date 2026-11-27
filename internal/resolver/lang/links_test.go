package lang_test

import (
	"context"
	"slices"
	"testing"

	"github.com/roshankumar0036singh/gographctx/internal/graph"
	"github.com/roshankumar0036singh/gographctx/internal/parser"
	parserlang "github.com/roshankumar0036singh/gographctx/internal/parser/lang"
	"github.com/roshankumar0036singh/gographctx/internal/parser/treesitter"
	"github.com/roshankumar0036singh/gographctx/internal/resolver"
	resolverlang "github.com/roshankumar0036singh/gographctx/internal/resolver/lang"
)

func resolveAndMaterializeLinks(
	t *testing.T,
	inputs []parser.FileInput,
	registerParsers func(*parser.Registry, *treesitter.Manager),
	registerResolvers func(*resolver.Registry),
) (*graph.Graph, map[string]*resolver.FileResolution) {
	t.Helper()
	mgr, err := treesitter.NewManager()
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	defer mgr.Close()

	preg := parser.NewRegistry()
	registerParsers(preg, mgr)
	coord, err := parser.NewCoordinator(preg, parser.CoordinatorConfig{DeterministicSort: true})
	if err != nil {
		t.Fatalf("NewCoordinator failed: %v", err)
	}

	batch, err := coord.ParseFiles(context.Background(), inputs)
	if err != nil || len(batch.Errors) > 0 {
		t.Fatalf("ParseFiles failed: err=%v errs=%v", err, batch.Errors)
	}

	g := graph.New()
	for _, pf := range batch.Files {
		if err := resolverlang.AddParsedFileToGraph(g, pf); err != nil {
			t.Fatalf("AddParsedFileToGraph(%q) failed: %v", pf.Path, err)
		}
	}

	rreg := resolver.NewRegistry()
	registerResolvers(rreg)

	resByFile := make(map[string]*resolver.FileResolution, len(batch.Files))
	g.View(func(view *graph.ReadView) {
		rc := &resolver.ResolutionContext{
			View:      view,
			NameIndex: batch.NameIndex,
		}
		rc.IndexFiles(batch.Files)

		for _, pf := range batch.Files {
			fr, err := rreg.ResolveFile(context.Background(), pf, rc)
			if err != nil {
				t.Fatalf("ResolveFile(%q) failed: %v", pf.Path, err)
			}
			for _, l := range fr.Links {
				if err := l.Validate(); err != nil {
					t.Errorf("invalid ResolvedLink in %s: %v (%+v)", pf.Path, err, l)
				}
			}
			resByFile[pf.Path] = fr
		}
	})

	// Materialize INHERITS/IMPLEMENTS/DECORATED_BY edges into g so ResolveTransitiveChain can traverse them.
	for _, fr := range resByFile {
		for _, l := range fr.Links {
			if err := g.AddEdge(graph.Edge{
				Source: l.SourceUID,
				Target: l.TargetUID,
				Kind:   l.Kind,
				Tier:   l.Tier,
				Label:  l.Method,
			}); err != nil {
				t.Fatalf("AddEdge(%s -> %s) failed: %v", l.SourceQName, l.TargetQName, err)
			}
		}
	}

	return g, resByFile
}

func assertHasLink(t *testing.T, links []resolver.ResolvedLink, srcQName, tgtQName string, kind graph.EdgeKind) {
	t.Helper()
	for _, l := range links {
		if l.SourceQName == srcQName && l.TargetQName == tgtQName && l.Kind == kind {
			return
		}
	}
	t.Fatalf("missing %s link %s -> %s in %+v", kind, srcQName, tgtQName, links)
}

func TestPythonCrossFileDiamondInheritanceAndDecorators(t *testing.T) {
	g, res := resolveAndMaterializeLinks(t, []parser.FileInput{
		{
			Path: "pkg/decorators.py",
			Source: []byte(`def audited(fn):
    return fn
`),
		},
		{
			Path: "pkg/base.py",
			Source: []byte(`from typing import Protocol

class StoreProto(Protocol):
    def save(self) -> None: ...

class Entity:
    pass
`),
		},
		{
			Path: "pkg/mixins.py",
			Source: []byte(`from .base import Entity

class TimestampMixin(Entity):
    pass

class AuditMixin(Entity):
    pass
`),
		},
		{
			Path: "pkg/order.py",
			Source: []byte(`from .base import StoreProto
from .decorators import audited
from .mixins import AuditMixin, TimestampMixin

@audited
class OrderModel(TimestampMixin, AuditMixin, StoreProto):
    @audited
    def commit(self) -> None:
        pass
`),
		},
	}, func(r *parser.Registry, m *treesitter.Manager) {
		if _, err := parserlang.RegisterPython(r, m); err != nil {
			t.Fatal(err)
		}
	}, func(r *resolver.Registry) {
		r.MustRegister(resolverlang.NewPythonResolver())
	})

	orderLinks := res["pkg/order.py"].Links
	assertHasLink(t, orderLinks, "pkg.order::OrderModel", "pkg.mixins::TimestampMixin", graph.EdgeInherits)
	assertHasLink(t, orderLinks, "pkg.order::OrderModel", "pkg.mixins::AuditMixin", graph.EdgeInherits)
	assertHasLink(t, orderLinks, "pkg.order::OrderModel", "pkg.base::StoreProto", graph.EdgeImplements)
	assertHasLink(t, orderLinks, "pkg.order::OrderModel", "pkg.decorators::audited", graph.EdgeDecoratedBy)
	assertHasLink(t, orderLinks, "pkg.order::OrderModel::commit", "pkg.decorators::audited", graph.EdgeDecoratedBy)

	// Verify 3+ level transitive diamond chain: OrderModel -> TimestampMixin / AuditMixin / StoreProto -> Entity.
	var chainQNames []string
	g.View(func(v *graph.ReadView) {
		rc := &resolver.ResolutionContext{View: v}
		for _, n := range resolverlang.ResolveTransitiveChain(rc, "pkg.order::OrderModel") {
			chainQNames = append(chainQNames, n.QName)
		}
	})
	for _, want := range []string{"pkg.mixins::TimestampMixin", "pkg.mixins::AuditMixin", "pkg.base::StoreProto", "pkg.base::Entity"} {
		if !slices.Contains(chainQNames, want) {
			t.Fatalf("transitive chain missing %q: %v", want, chainQNames)
		}
	}
}

func TestTypeScriptGoRustJavaLinksEndToEnd(t *testing.T) {
	// 1. Go embedded multi-level structs + interface satisfaction
	goGraph, goRes := resolveAndMaterializeLinks(t, []parser.FileInput{
		{Path: "core/base.go", Source: []byte("package core\ntype Root struct{}\nfunc (r *Root) ID() string { return \"1\" }\n")},
		{Path: "mid/mid.go", Source: []byte("package mid\nimport \"core\"\ntype Middle struct { core.Root }\n")},
		{Path: "app/leaf.go", Source: []byte("package app\nimport \"mid\"\ntype Identifier interface { ID() string }\ntype Leaf struct { mid.Middle }\n")},
	}, func(r *parser.Registry, m *treesitter.Manager) {
		if _, err := parserlang.RegisterGo(r, m); err != nil {
			t.Fatal(err)
		}
	}, func(r *resolver.Registry) {
		r.MustRegister(resolverlang.NewGoResolver())
	})
	assertHasLink(t, goRes["app/leaf.go"].Links, "app::Leaf", "mid::Middle", graph.EdgeInherits)
	assertHasLink(t, goRes["app/leaf.go"].Links, "app::Leaf", "app::Identifier", graph.EdgeImplements)
	goGraph.View(func(v *graph.ReadView) {
		chain := resolverlang.ResolveTransitiveChain(&resolver.ResolutionContext{View: v}, "app::Leaf")
		var qnames []string
		for _, n := range chain {
			qnames = append(qnames, n.QName)
		}
		if !slices.Contains(qnames, "core::Root") {
			t.Fatalf("Go transitive chain missing core::Root: %v", qnames)
		}
	})

	// 2. TypeScript cross-file extends + implements + decorator
	_, tsRes := resolveAndMaterializeLinks(t, []parser.FileInput{
		{Path: "src/meta.ts", Source: []byte("export function Entity(target: any) { return target; }\n")},
		{Path: "src/base.ts", Source: []byte("export interface Serializable { serialize(): string; }\nexport class BaseEntity {}\n")},
		{Path: "src/user.ts", Source: []byte("import { Entity } from \"./meta\";\nimport { BaseEntity, Serializable } from \"./base\";\n@Entity\nexport class User extends BaseEntity implements Serializable { serialize(): string { return \"u\"; } }\n")},
	}, func(r *parser.Registry, m *treesitter.Manager) {
		if _, err := parserlang.RegisterTypeScript(r, m); err != nil {
			t.Fatal(err)
		}
	}, func(r *resolver.Registry) {
		r.MustRegister(resolverlang.NewTypeScriptResolver())
	})
	tsLinks := tsRes["src/user.ts"].Links
	assertHasLink(t, tsLinks, "src/user::User", "src/base::BaseEntity", graph.EdgeInherits)
	assertHasLink(t, tsLinks, "src/user::User", "src/base::Serializable", graph.EdgeImplements)
	assertHasLink(t, tsLinks, "src/user::User", "src/meta::Entity", graph.EdgeDecoratedBy)

	// 3. Rust cross-module trait super-bounds + impl Trait for Struct (including separate-file impl)
	_, rsRes := resolveAndMaterializeLinks(t, []parser.FileInput{
		{Path: "src/traits.rs", Source: []byte("pub trait Base {}\npub trait Sub: Base { fn ping(&self); }\n")},
		{Path: "src/model.rs", Source: []byte("pub struct Client;\n")},
		{Path: "src/impls.rs", Source: []byte("use crate::model::Client;\nuse crate::traits::Sub;\nimpl Sub for Client { fn ping(&self) {} }\n")},
	}, func(r *parser.Registry, m *treesitter.Manager) {
		if _, err := parserlang.RegisterRust(r, m); err != nil {
			t.Fatal(err)
		}
	}, func(r *resolver.Registry) {
		r.MustRegister(resolverlang.NewRustResolver())
	})
	assertHasLink(t, rsRes["src/traits.rs"].Links, "src::traits::Sub", "src::traits::Base", graph.EdgeInherits)
	assertHasLink(t, rsRes["src/impls.rs"].Links, "src::model::Client", "src::traits::Sub", graph.EdgeImplements)

	// 4. Java multi-level cross-package extends + implements + annotation
	jGraph, jRes := resolveAndMaterializeLinks(t, []parser.FileInput{
		{Path: "com/acme/ann/Tracked.java", Source: []byte("package com.acme.ann;\npublic @interface Tracked {}\n")},
		{Path: "com/acme/core/Root.java", Source: []byte("package com.acme.core;\npublic interface Identifiable {}\npublic class Root implements Identifiable {}\n")},
		{Path: "com/acme/mid/Mid.java", Source: []byte("package com.acme.mid;\nimport com.acme.core.Root;\npublic class Mid extends Root {}\n")},
		{Path: "com/acme/app/Leaf.java", Source: []byte("package com.acme.app;\nimport com.acme.ann.Tracked;\nimport com.acme.mid.Mid;\n@Tracked\npublic class Leaf extends Mid {}\n")},
	}, func(r *parser.Registry, m *treesitter.Manager) {
		if _, err := parserlang.RegisterJava(r, m); err != nil {
			t.Fatal(err)
		}
	}, func(r *resolver.Registry) {
		r.MustRegister(resolverlang.NewJavaResolver())
	})
	assertHasLink(t, jRes["com/acme/app/Leaf.java"].Links, "com.acme.app::Leaf", "com.acme.mid::Mid", graph.EdgeInherits)
	assertHasLink(t, jRes["com/acme/app/Leaf.java"].Links, "com.acme.app::Leaf", "com.acme.ann::Tracked", graph.EdgeDecoratedBy)
	assertHasLink(t, jRes["com/acme/core/Root.java"].Links, "com.acme.core::Root", "com.acme.core::Identifiable", graph.EdgeImplements)
	jGraph.View(func(v *graph.ReadView) {
		chain := resolverlang.ResolveTransitiveChain(&resolver.ResolutionContext{View: v}, "com.acme.app::Leaf")
		var qnames []string
		for _, n := range chain {
			qnames = append(qnames, n.QName)
		}
		for _, want := range []string{"com.acme.mid::Mid", "com.acme.core::Root", "com.acme.core::Identifiable"} {
			if !slices.Contains(qnames, want) {
				t.Fatalf("Java transitive chain missing %q: %v", want, qnames)
			}
		}
	})
}
