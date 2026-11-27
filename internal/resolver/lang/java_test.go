package lang_test

import (
	"context"
	"testing"

	"github.com/roshankumar0036singh/gographctx/internal/graph"
	"github.com/roshankumar0036singh/gographctx/internal/parser"
	parserlang "github.com/roshankumar0036singh/gographctx/internal/parser/lang"
	"github.com/roshankumar0036singh/gographctx/internal/parser/treesitter"
	"github.com/roshankumar0036singh/gographctx/internal/resolver"
	resolverlang "github.com/roshankumar0036singh/gographctx/internal/resolver/lang"
)

func TestJavaResolverPackagesHierarchyAndOverloads(t *testing.T) {
	mgr, err := treesitter.NewManager()
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	defer mgr.Close()

	reg := parser.NewRegistry()
	if _, err := parserlang.RegisterJava(reg, mgr); err != nil {
		t.Fatalf("RegisterJava failed: %v", err)
	}
	coord, err := parser.NewCoordinator(reg, parser.CoordinatorConfig{DeterministicSort: true})
	if err != nil {
		t.Fatalf("NewCoordinator failed: %v", err)
	}

	batch, err := coord.ParseFiles(context.Background(), []parser.FileInput{
		{
			Path: "src/main/java/com/example/util/Ids.java",
			Source: []byte(`package com.example.util;

public class Ids {
    public static String nextId() {
        return "ord-1";
    }
}
`),
		},
		{
			Path: "src/main/java/com/example/store/BaseStore.java",
			Source: []byte(`package com.example.store;

public class BaseStore {
    public String connect() {
        return "ok";
    }
}
`),
		},
		{
			Path: "src/main/java/com/example/store/MemoryStore.java",
			Source: []byte(`package com.example.store;

public class MemoryStore extends BaseStore {
    public MemoryStore() {}

    public String save(String id) {
        return super.connect();
    }

    public void notifyItem(String id) {}
    public void notifyItem(int code) {}
    public void notifyItem(String id, boolean urgent) {}
}
`),
		},
		{
			Path: "src/main/java/com/example/orders/Order.java",
			Source: []byte(`package com.example.orders;

public class Order {
    public static class Builder {
        public Order build() {
            return new Order();
        }
    }
}
`),
		},
		{
			Path: "src/main/java/com/example/orders/OrderService.java",
			Source: []byte(`package com.example.orders;

import static com.example.util.Ids.nextId;
import com.example.store.*;
import java.util.Objects;

public class OrderService {
    private final MemoryStore store;

    public OrderService(MemoryStore store) {
        this.store = store;
    }

    public Order placeOrder() {
        String id = Objects.requireNonNull(nextId());
        this.store.save(id);
        this.store.notifyItem(id, true);
        this.store.notifyItem(id);
        Order.Builder b = new Order.Builder();
        return b.build();
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
	resReg.MustRegister(resolverlang.NewJavaResolver())

	resByFile := make(map[string]*resolver.FileResolution, len(batch.Files))
	g.View(func(view *graph.ReadView) {
		rc := &resolver.ResolutionContext{
			View:      view,
			NameIndex: batch.NameIndex,
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

	svcRes := resByFile["src/main/java/com/example/orders/OrderService.java"]
	storeRes := resByFile["src/main/java/com/example/store/MemoryStore.java"]
	if svcRes == nil || storeRes == nil {
		t.Fatalf("missing Java resolutions")
	}

	hasResolved := func(fr *resolver.FileResolution, targetQName string, tier graph.ResolutionTier) bool {
		for _, rcCall := range fr.Resolved {
			if rcCall.TargetQName == targetQName && rcCall.Tier == tier {
				return true
			}
		}
		return false
	}

	// Verify static import, wildcard package field method, arity-disambiguated 2-arg overload,
	// inner class constructor & inner class method (`Order.Builder.build()`):
	checks := []struct {
		target string
		tier   graph.ResolutionTier
		desc   string
	}{
		{"com.example.util::Ids::nextId", resolver.TierQualifiedImport, "static import nextId()"},
		{"com.example.store::MemoryStore::save", resolver.TierReceiverType, "this.store.save(id)"},
		{"com.example.orders::Order::Builder", resolver.TierSamePackage, "new Order.Builder()"},
		{"com.example.orders::Order::Builder::build", resolver.TierReceiverType, "b.build() on inner class"},
	}
	for _, chk := range checks {
		if !hasResolved(svcRes, chk.target, chk.tier) {
			t.Errorf("OrderService.java missing %s (%s @ tier %s); resolved=%+v unresolved=%+v",
				chk.desc, chk.target, chk.tier, svcRes.Resolved, svcRes.Unresolved)
		}
	}

	// Verify 2-arg overload `this.store.notifyItem(id, true)` resolved cleanly at TierReceiverType.
	if !hasResolved(svcRes, "com.example.store::MemoryStore::notifyItem", resolver.TierReceiverType) {
		t.Errorf("expected 2-arg notifyItem(id, true) overload to resolve uniquely; resolved=%+v", svcRes.Resolved)
	}

	// Verify ambiguous 1-arg overload `this.store.notifyItem(id)` emits TierTypeNarrowed CallStatusAmbiguous
	// with 2 candidate UIDs, rather than guessing CALLS.
	var sawAmbiguousOverload, sawExternalObjects bool
	for _, uc := range svcRes.Unresolved {
		if uc.CalleeExpr == "this.store.notifyItem" && uc.Status == graph.CallStatusAmbiguous && uc.Tier == resolver.TierTypeNarrowed && len(uc.Candidates) == 2 {
			sawAmbiguousOverload = true
		}
		if uc.CalleeExpr == "Objects.requireNonNull" && uc.Status == graph.CallStatusExternal {
			sawExternalObjects = true
		}
	}
	if !sawAmbiguousOverload {
		t.Errorf("expected ambiguous 1-arg overload for notifyItem(id); unresolved=%+v", svcRes.Unresolved)
	}
	if !sawExternalObjects {
		t.Errorf("expected java.util.Objects.requireNonNull classified external; unresolved=%+v", svcRes.Unresolved)
	}

	// Verify `super.connect()` inheritance in MemoryStore.java -> BaseStore::connect.
	if !hasResolved(storeRes, "com.example.store::BaseStore::connect", resolver.TierSelf) {
		t.Fatalf("MemoryStore.java expected TierSelf super.connect() -> BaseStore::connect; resolved=%+v", storeRes.Resolved)
	}
}
