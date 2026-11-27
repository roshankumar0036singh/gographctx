package resolver_test

import (
	"context"
	"errors"
	"testing"

	"github.com/roshankumar0036singh/gographctx/internal/graph"
	"github.com/roshankumar0036singh/gographctx/internal/parser/lang"
	"github.com/roshankumar0036singh/gographctx/internal/parser/treesitter"
	"github.com/roshankumar0036singh/gographctx/internal/resolver"
)

func TestInferTypesGoReceiverMultiReturnConstructorAndScopeIsolation(t *testing.T) {
	mgr, err := treesitter.NewManager()
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	defer mgr.Close()

	gp, err := lang.NewGoParser(mgr)
	if err != nil {
		t.Fatalf("NewGoParser failed: %v", err)
	}

	src := []byte(`package billing

type Repo interface {
	Load(id string) (*Order, error)
}

type Order struct {
	ID string
}

type Cache struct{}

type Service struct {
	repo  Repo
	cache *Cache
}

func makeOrder(id string) (*Order, error) {
	return &Order{ID: id}, nil
}

func (s *Service) Handle(req *Order) {
	client := NewClient("api")
	copyClient := client
	ord, err := makeOrder(req.ID)
	local := &Service{}
	_ = client
	_ = copyClient
	_ = ord
	_ = err
	_ = local
	_ = s.cache
}

func SiblingFunc(client *Cache) {
	_ = client
}
`)

	pf, err := gp.Parse(context.Background(), "billing/service.go", src)
	if err != nil {
		t.Fatalf("Go Parse failed: %v", err)
	}

	tm, err := resolver.InferTypes(context.Background(), pf, nil)
	if err != nil {
		t.Fatalf("InferTypes failed: %v", err)
	}

	handleScope := "billing::Service::Handle"
	siblingScope := "billing::SiblingFunc"

	cases := []struct {
		scope string
		expr  string
		want  string
	}{
		{handleScope, "s", "Service"},
		{handleScope, "s.cache", "Cache"},
		{handleScope, "s.repo", "Repo"},
		{handleScope, "req", "Order"},
		{handleScope, "client", "Client"},
		{handleScope, "copyClient", "Client"},
		{handleScope, "ord", "Order"},
		{handleScope, "local", "Service"},
		// Sibling function has its own parameter type and never inherits Handle's "ord" or "client: Client".
		{siblingScope, "client", "Cache"},
	}

	for _, tc := range cases {
		got, ok := tm.LookupReceiver(tc.scope, tc.expr)
		if !ok || got.Name != tc.want {
			t.Errorf("LookupReceiver(%q, %q) = (%+v, %v), want %q", tc.scope, tc.expr, got, ok, tc.want)
		}
	}

	if _, ok := tm.Lookup(siblingScope, "ord"); ok {
		t.Fatal("scope leak: SiblingFunc unexpectedly resolved variable ord from Handle")
	}

	flat := tm.ToFlatMap()
	if got, ok := resolver.LookupFlat(flat, handleScope, "client"); !ok || got.Name != "Client" {
		t.Fatalf("LookupFlat(Handle, client) = (%+v, %v), want Client", got, ok)
	}
	if got, ok := resolver.LookupFlat(flat, siblingScope, "client"); !ok || got.Name != "Cache" {
		t.Fatalf("LookupFlat(SiblingFunc, client) = (%+v, %v), want Cache", got, ok)
	}
}

func TestInferTypesPythonTSRustJavaAndCrossFileContext(t *testing.T) {
	mgr, err := treesitter.NewManager()
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	defer mgr.Close()

	// Build a backing Graph with a remote function return type for cross-file lookup.
	g := graph.New()
	if _, err := g.AddNode(graph.Node{
		Kind:       graph.KindFunction,
		Name:       "buildRemoteClient",
		QName:      "remote::buildRemoteClient",
		ReturnType: "RemoteClient",
	}); err != nil {
		t.Fatalf("AddNode failed: %v", err)
	}

	g.View(func(view *graph.ReadView) {
		rc := &resolver.ResolutionContext{View: view}

		// 1. Python: param types, self attribute promotion, constructor, return type, nested closure scope.
		pp, err := lang.NewPythonParser(mgr)
		if err != nil {
			t.Fatalf("NewPythonParser: %v", err)
		}
		pySrc := []byte(`class Cache:
    pass

def get_cache() -> Cache:
    return Cache()

class Worker:
    def __init__(self, store: Storage):
        self.cache = get_cache()

    def process(self, req: Request):
        local = Cache()
        remote = buildRemoteClient()
        def inner():
            return local
        return inner()
`)
		pyPF, err := pp.Parse(context.Background(), "worker.py", pySrc)
		if err != nil {
			t.Fatalf("Python Parse: %v", err)
		}
		pyTM, err := resolver.InferTypes(context.Background(), pyPF, rc)
		if err != nil {
			t.Fatalf("Python InferTypes: %v", err)
		}
		if got, ok := pyTM.LookupReceiver("worker::Worker::process", "self"); !ok || got.Name != "Worker" {
			t.Fatalf("Python self = (%+v, %v)", got, ok)
		}
		if got, ok := pyTM.LookupReceiver("worker::Worker::process", "self.cache"); !ok || got.Name != "Cache" {
			t.Fatalf("Python self.cache = (%+v, %v)", got, ok)
		}
		if got, ok := pyTM.LookupReceiver("worker::Worker::process", "req"); !ok || got.Name != "Request" {
			t.Fatalf("Python req = (%+v, %v)", got, ok)
		}
		if got, ok := pyTM.LookupReceiver("worker::Worker::process::inner", "local"); !ok || got.Name != "Cache" {
			t.Fatalf("Python inner lexical capture = (%+v, %v)", got, ok)
		}
		if got, ok := pyTM.LookupReceiver("worker::Worker::process", "remote"); !ok || got.Name != "RemoteClient" {
			t.Fatalf("Python cross-file remote = (%+v, %v)", got, ok)
		}

		// 2. TypeScript: constructor param property `this.cfg`, `new`, `await` return.
		tp, err := lang.NewTypeScriptParser(mgr)
		if err != nil {
			t.Fatalf("NewTypeScriptParser: %v", err)
		}
		tsSrc := []byte(`export class Service {
  constructor(private readonly cfg: Config) {}
  async run(arg: Payload): Promise<void> {
    const cli = new ApiClient();
    const remote = await buildRemoteClient();
    const c = this.cfg;
  }
}
`)
		tsPF, err := tp.Parse(context.Background(), "svc.ts", tsSrc)
		if err != nil {
			t.Fatalf("TS Parse: %v", err)
		}
		tsTM, err := resolver.InferTypes(context.Background(), tsPF, rc)
		if err != nil {
			t.Fatalf("TS InferTypes: %v", err)
		}
		if got, ok := tsTM.LookupReceiver("svc::Service::run", "this.cfg"); !ok || got.Name != "Config" {
			t.Fatalf("TS this.cfg = (%+v, %v)", got, ok)
		}
		if got, ok := tsTM.LookupReceiver("svc::Service::run", "cli"); !ok || got.Name != "ApiClient" {
			t.Fatalf("TS new ApiClient = (%+v, %v)", got, ok)
		}
		if got, ok := tsTM.LookupReceiver("svc::Service::run", "remote"); !ok || got.Name != "RemoteClient" {
			t.Fatalf("TS await remote = (%+v, %v)", got, ok)
		}
		if got, ok := tsTM.LookupReceiver("svc::Service::run", "c"); !ok || got.Name != "Config" {
			t.Fatalf("TS c = this.cfg = (%+v, %v)", got, ok)
		}

		// 3. Rust: associated `::new()`, struct expression, `self`.
		rp, err := lang.NewRustParser(mgr)
		if err != nil {
			t.Fatalf("NewRustParser: %v", err)
		}
		rsSrc := []byte(`pub struct Client { id: u32 }
impl Client {
    pub fn work(&self) {
        let c = OrderClient::new();
        let lit = Client { id: 1 };
    }
}
`)
		rsPF, err := rp.Parse(context.Background(), "lib.rs", rsSrc)
		if err != nil {
			t.Fatalf("Rust Parse: %v", err)
		}
		rsTM, err := resolver.InferTypes(context.Background(), rsPF, rc)
		if err != nil {
			t.Fatalf("Rust InferTypes: %v", err)
		}
		if got, ok := rsTM.LookupReceiver("lib::Client::work", "c"); !ok || got.Name != "OrderClient" {
			t.Fatalf("Rust OrderClient::new = (%+v, %v)", got, ok)
		}
		if got, ok := rsTM.LookupReceiver("lib::Client::work", "lit"); !ok || got.Name != "Client" {
			t.Fatalf("Rust struct literal = (%+v, %v)", got, ok)
		}

		// 4. Java: `var x = new Foo<>()` and typed local vars.
		jp, err := lang.NewJavaParser(mgr)
		if err != nil {
			t.Fatalf("NewJavaParser: %v", err)
		}
		javaSrc := []byte(`package demo;
public class Runner {
    void exec() {
        var repo = new OrderRepo<String>();
        Client cli = buildRemoteClient();
    }
}
`)
		javaPF, err := jp.Parse(context.Background(), "Runner.java", javaSrc)
		if err != nil {
			t.Fatalf("Java Parse: %v", err)
		}
		javaTM, err := resolver.InferTypes(context.Background(), javaPF, rc)
		if err != nil {
			t.Fatalf("Java InferTypes: %v", err)
		}
		if got, ok := javaTM.LookupReceiver("demo::Runner::exec", "repo"); !ok || got.Name != "OrderRepo" {
			t.Fatalf("Java var new OrderRepo = (%+v, %v)", got, ok)
		}
		if got, ok := javaTM.LookupReceiver("demo::Runner::exec", "cli"); !ok || got.Name != "Client" {
			t.Fatalf("Java explicit Client = (%+v, %v)", got, ok)
		}
	})

	// Canceled context & nil file guards.
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := resolver.InferTypes(canceled, nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled InferTypes = %v, want context.Canceled", err)
	}
	var nilCtx context.Context
	emptyTM, err := resolver.InferTypes(nilCtx, nil, nil)
	if err != nil || emptyTM == nil || len(emptyTM.ToFlatMap()) != 0 {
		t.Fatalf("nil file InferTypes mismatch: (%+v, %v)", emptyTM, err)
	}
}
