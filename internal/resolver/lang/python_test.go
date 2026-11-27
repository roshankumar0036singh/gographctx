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

func TestPythonResolverFlaskStylePackages(t *testing.T) {
	mgr, err := treesitter.NewManager()
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	defer mgr.Close()

	reg := parser.NewRegistry()
	if _, err := parserlang.RegisterPython(reg, mgr); err != nil {
		t.Fatalf("RegisterPython failed: %v", err)
	}
	coord, err := parser.NewCoordinator(reg, parser.CoordinatorConfig{DeterministicSort: true})
	if err != nil {
		t.Fatalf("NewCoordinator failed: %v", err)
	}

	batch, err := coord.ParseFiles(context.Background(), []parser.FileInput{
		{
			Path: "app/__init__.py",
			Source: []byte(`from .models import BaseModel, Order
from .storage import make_store
`),
		},
		{
			Path: "app/models.py",
			Source: []byte(`class BaseModel:
    def validate(self) -> bool:
        return True

class Order(BaseModel):
    def __init__(self, order_id: str):
        self.order_id = order_id

    def save(self) -> bool:
        return self.validate()
`),
		},
		{
			Path: "app/storage/__init__.py",
			Source: []byte(`from .cache import MemoryCache

def make_store() -> MemoryCache:
    return MemoryCache()
`),
		},
		{
			Path: "app/storage/cache.py",
			Source: []byte(`from ..models import Order

class MemoryCache:
    def put(self, order: Order) -> None:
        _ = order.save()

    def get(self, key: str) -> str:
        return key
`),
		},
		{
			Path: "app/api/__init__.py",
			Source: []byte(`def sanitize(raw: str) -> str:
    return raw.strip()
`),
		},
		{
			Path: "app/api/helpers.py",
			Source: []byte(`def format_tag(name: str) -> str:
    return name.lower()

def log_metric(metric: str) -> None:
    _ = format_tag(metric)
`),
		},
		{
			Path: "app/api/routes.py",
			Source: []byte(`import json
from . import sanitize
from .helpers import *
from .. import Order
from ..storage.cache import MemoryCache as Cache
from ..storage import make_store

try:
    from .helpers import log_metric as record_metric
except ImportError:
    from external_metrics import record_metric

class BaseHandler:
    def audit(self, msg: str) -> None:
        record_metric(msg)

class OrderHandler(BaseHandler):
    def __init__(self, cache: Cache):
        self.cache = cache

    @classmethod
    def create(cls):
        store = make_store()
        return cls(store)

    def audit(self, msg: str) -> None:
        super().audit(msg)
        self.persist(msg)

    def persist(self, key: str) -> str:
        return self.cache.get(key)

    def handle(self, order_id: str) -> Order:
        clean = sanitize(order_id)
        order = Order(clean)
        self.cache.put(order)
        self.audit(format_tag(clean))
        _ = json.dumps({"id": clean})
        return order
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
	resReg.MustRegister(resolverlang.NewPythonResolver())

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

	routesRes := resByFile["app/api/routes.py"]
	if routesRes == nil {
		t.Fatalf("missing resolution for app/api/routes.py")
	}

	hasResolved := func(fr *resolver.FileResolution, targetQName string, tier graph.ResolutionTier) bool {
		for _, rcCall := range fr.Resolved {
			if rcCall.TargetQName == targetQName && rcCall.Tier == tier {
				return true
			}
		}
		return false
	}

	// Verify relative imports, __init__.py re-exports, wildcard (*), conditional import,
	// super(), self.x, cls(), and inferred cache attribute receiver methods:
	checks := []struct {
		target string
		tier   graph.ResolutionTier
		desc   string
	}{
		{"app.api.helpers::log_metric", resolver.TierQualifiedImport, "conditional import alias record_metric"},
		{"app.storage::make_store", resolver.TierQualifiedImport, "relative __init__ import make_store()"},
		{"app.api.routes::BaseHandler::audit", resolver.TierSelf, "super().audit()"},
		{"app.api.routes::OrderHandler::persist", resolver.TierSelf, "self.persist()"},
		{"app.storage.cache::MemoryCache::get", resolver.TierReceiverType, "self.cache.get()"},
		{"app.api::sanitize", resolver.TierQualifiedImport, "from . import sanitize (__init__.py)"},
		{"app.models::Order::__init__", resolver.TierQualifiedImport, "re-exported Order from .. (__init__.py)"},
		{"app.storage.cache::MemoryCache::put", resolver.TierReceiverType, "self.cache.put()"},
		{"app.api.routes::OrderHandler::audit", resolver.TierSelf, "self.audit()"},
		{"app.api.helpers::format_tag", resolver.TierQualifiedImport, "wildcard from .helpers import *"},
	}
	for _, chk := range checks {
		if !hasResolved(routesRes, chk.target, chk.tier) {
			t.Errorf("routes.py missing %s (%s @ tier %s); resolved=%+v unresolved=%+v",
				chk.desc, chk.target, chk.tier, routesRes.Resolved, routesRes.Unresolved)
		}
	}

	// Verify inherited method on self in app/models.py (`Order.save()` -> `self.validate()`).
	modelsRes := resByFile["app/models.py"]
	if !hasResolved(modelsRes, "app.models::BaseModel::validate", resolver.TierSelf) {
		t.Fatalf("models.py expected TierSelf inherited validate(), got resolved=%+v", modelsRes.Resolved)
	}

	// Verify cross-file parameter type inference in app/storage/cache.py (`order.save()` -> `Order::save`).
	cacheRes := resByFile["app/storage/cache.py"]
	if !hasResolved(cacheRes, "app.models::Order::save", resolver.TierReceiverType) {
		t.Fatalf("cache.py expected TierReceiverType Order::save, got resolved=%+v unresolved=%+v", cacheRes.Resolved, cacheRes.Unresolved)
	}

	// Verify external json.dumps classification.
	var sawJSONExternal bool
	for _, uc := range routesRes.Unresolved {
		if uc.CalleeExpr == "json.dumps" && uc.Status == graph.CallStatusExternal {
			sawJSONExternal = true
		}
	}
	if !sawJSONExternal {
		t.Fatalf("expected json.dumps classified as CallStatusExternal; unresolved=%+v", routesRes.Unresolved)
	}

	// Check strong-tier internal resolution rate across all files (>=85% exit criterion).
	totalInternal := 0
	strongInternal := 0
	for _, fr := range resByFile {
		for _, rcCall := range fr.Resolved {
			totalInternal++
			if rcCall.Tier <= resolver.TierSamePackage {
				strongInternal++
			}
		}
		for _, ucCall := range fr.Unresolved {
			if ucCall.Status == graph.CallStatusExternal {
				continue
			}
			totalInternal++
		}
	}
	if totalInternal == 0 || float64(strongInternal)/float64(totalInternal) < 0.85 {
		t.Fatalf("expected >=85%% internal call resolution rate, got %d/%d", strongInternal, totalInternal)
	}
}
