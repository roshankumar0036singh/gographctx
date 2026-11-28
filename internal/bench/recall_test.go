package bench

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/roshankumar0036singh/gographctx/internal/config"
	"github.com/roshankumar0036singh/gographctx/internal/graph"
	"github.com/roshankumar0036singh/gographctx/internal/pipeline"
)

func writeRecallFixtureFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", full, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatalf("WriteFile(%s): %v", full, err)
		}
	}
}

func assertRecallAccuracy(t *testing.T, corpus string, repoDir string, truth []CallEdgeTruth, minPrecision, minRecall float64) AccuracyReport {
	t.Helper()
	res, err := pipeline.IndexRepository(context.Background(), repoDir, config.DefaultConfig(), pipeline.IndexOptions{Force: true})
	if err != nil {
		t.Fatalf("%s IndexRepository failed: %v", corpus, err)
	}
	rep := EvaluateGraphAccuracy(corpus, res.Graph, truth)
	t.Logf("%s accuracy: ground_truth=%d matched_certain=%d matched_total=%d precision=%.3f recall=%.3f strong=%.3f unresolved=%d",
		corpus, rep.GroundTruthEdges, rep.MatchedCertain, rep.MatchedWithLikely,
		rep.PrecisionOverall, rep.Recall, rep.StrongTierRatio, rep.UnresolvedCallSites)
	if len(rep.FalsePositives) > 0 {
		t.Logf("%s false positives: %v", corpus, rep.FalsePositives)
	}
	if len(rep.FalseNegatives) > 0 {
		t.Logf("%s false negatives: %v", corpus, rep.FalseNegatives)
	}
	if rep.Recall < minRecall {
		res.Graph.View(func(v *graph.ReadView) {
			for _, cs := range v.GetCallSites() {
				t.Logf("%s unresolved site: %s:%d expr=%q key=%q status=%s reason=%q", corpus, cs.File, cs.Line, cs.CalleeExpr, cs.LookupKey, cs.Status, cs.Reason)
			}
		})
	}
	if rep.PrecisionOverall < minPrecision {
		t.Fatalf("%s precision %.3f < %.3f (FPs: %v)", corpus, rep.PrecisionOverall, minPrecision, rep.FalsePositives)
	}
	if rep.Recall < minRecall {
		t.Fatalf("%s recall %.3f < %.3f (FNs: %v)", corpus, rep.Recall, minRecall, rep.FalseNegatives)
	}
	return rep
}

func TestCrossLanguageRecallAndPrecision(t *testing.T) {
	t.Run("Python_FlaskLayout_Modules_Decorators_Constructors_ForLoops_With_CallableAlias", func(t *testing.T) {
		dir := t.TempDir()
		writeRecallFixtureFiles(t, dir, map[string]string{
			"src/miniapp/__init__.py": `from .app import Flask, render_template
from .blueprints import Blueprint
`,
			"src/miniapp/blueprints.py": `class Blueprint:
    def __init__(self, name: str):
        self.name = name

    def before_request(self, fn):
        return fn
`,
			"src/miniapp/helpers.py": `def clean_input(v: str) -> str:
    return v

def login_required(fn):
    return fn

class InputValidator:
    def __call__(self, v: str) -> str:
        return clean_input(v)
`,
			"src/miniapp/app.py": `from .helpers import clean_input

def render_template(name: str) -> str:
    return clean_input(name)

class OrderItem:
    def process(self) -> str:
        return clean_input("item")

class TestClient:
    def get(self, path: str) -> str:
        return clean_input(path)

class Flask:
    def __init__(self, import_name: str):
        self.import_name = import_name

    def errorhandler(self, code: int):
        def dec(fn):
            return fn
        return dec

    def test_client(self) -> TestClient:
        return TestClient()

    def iter_orders(self) -> list[OrderItem]:
        return [OrderItem()]
`,
			"src/miniapp/views.py": `import miniapp
from miniapp import Flask, Blueprint
from .helpers import clean_input, login_required, InputValidator

app = Flask("miniapp")
bp = Blueprint("main")

@app.errorhandler(400)
@login_required
def handle_bad_request(err: str) -> str:
    fn = clean_input
    validator = InputValidator()
    for order in app.iter_orders():
        order.process()
    rendered = miniapp.render_template(fn(err))
    return validator(rendered)
`,
			"tests/test_views.py": `from miniapp.views import app

def test_home():
    with app.test_client() as client:
        return client.get("/")
`,
		})

		truth := []CallEdgeTruth{
			{CallerFile: "src/miniapp/helpers.py", CallerName: "__call__", CalleeFile: "src/miniapp/helpers.py", CalleeName: "clean_input", Category: "plain"},
			{CallerFile: "src/miniapp/app.py", CallerName: "render_template", CalleeFile: "src/miniapp/helpers.py", CalleeName: "clean_input", Category: "plain"},
			{CallerFile: "src/miniapp/app.py", CallerName: "process", CalleeFile: "src/miniapp/helpers.py", CalleeName: "clean_input", Category: "plain"},
			{CallerFile: "src/miniapp/app.py", CallerName: "get", CalleeFile: "src/miniapp/helpers.py", CalleeName: "clean_input", Category: "plain"},
			{CallerFile: "src/miniapp/app.py", CallerName: "test_client", CalleeFile: "src/miniapp/app.py", CalleeName: "TestClient", Category: "constructor"},
			{CallerFile: "src/miniapp/app.py", CallerName: "iter_orders", CalleeFile: "src/miniapp/app.py", CalleeName: "OrderItem", Category: "constructor"},
			{CallerFile: "src/miniapp/views.py", CallerName: "<module>", CalleeFile: "src/miniapp/app.py", CalleeName: "__init__", Category: "constructor"},
			{CallerFile: "src/miniapp/views.py", CallerName: "<module>", CalleeFile: "src/miniapp/blueprints.py", CalleeName: "__init__", Category: "constructor"},
			{CallerFile: "src/miniapp/views.py", CallerName: "<module>", CalleeFile: "src/miniapp/app.py", CalleeName: "errorhandler", Category: "decorator"},
			{CallerFile: "src/miniapp/views.py", CallerName: "<module>", CalleeFile: "src/miniapp/helpers.py", CalleeName: "login_required", Category: "decorator"},
			{CallerFile: "src/miniapp/views.py", CallerName: "handle_bad_request", CalleeFile: "src/miniapp/helpers.py", CalleeName: "clean_input", Category: "plain"},
			{CallerFile: "src/miniapp/views.py", CallerName: "handle_bad_request", CalleeFile: "src/miniapp/app.py", CalleeName: "iter_orders", Category: "var_or_module"},
			{CallerFile: "src/miniapp/views.py", CallerName: "handle_bad_request", CalleeFile: "src/miniapp/app.py", CalleeName: "process", Category: "var_or_module"},
			{CallerFile: "src/miniapp/views.py", CallerName: "handle_bad_request", CalleeFile: "src/miniapp/app.py", CalleeName: "render_template", Category: "var_or_module"},
			{CallerFile: "src/miniapp/views.py", CallerName: "handle_bad_request", CalleeFile: "src/miniapp/helpers.py", CalleeName: "InputValidator", Category: "constructor"},
			{CallerFile: "src/miniapp/views.py", CallerName: "handle_bad_request", CalleeFile: "src/miniapp/helpers.py", CalleeName: "__call__", Category: "var_or_module"},
			{CallerFile: "tests/test_views.py", CallerName: "test_home", CalleeFile: "src/miniapp/app.py", CalleeName: "test_client", Category: "var_or_module"},
			{CallerFile: "tests/test_views.py", CallerName: "test_home", CalleeFile: "src/miniapp/app.py", CalleeName: "get", Category: "var_or_module"},
		}
		rep := assertRecallAccuracy(t, "python-flask-recall", dir, truth, 0.95, 1.00)
		if rep.LibraryTruthEdges == 0 || rep.TestTruthEdges == 0 {
			t.Fatalf("expected library/test split populated, got %+v", rep)
		}
	})

	t.Run("Go_PackageVars_MultiReturn_Range_Chained_CallableAlias", func(t *testing.T) {
		dir := t.TempDir()
		writeRecallFixtureFiles(t, dir, map[string]string{
			"go.mod": "module example.com/demo\n\ngo 1.22\n",
			"store/store.go": `package store

type Repo struct{}

func (r *Repo) Save() error { return nil }

type Client struct {
	repo *Repo
}

func (c *Client) Repo() *Repo { return c.repo }

func MakeClient() *Client { return &Client{repo: &Repo{}} }

func NewClient() (*Client, error) { return &Client{repo: &Repo{}}, nil }

func ListClients() []*Client { return []*Client{MakeClient()} }
`,
			"app/main.go": `package app

import "example.com/demo/store"

var defaultClient = store.MakeClient()

func Helper() error { return nil }

func Run() {
	fn := Helper
	_ = fn()
	_ = defaultClient.Repo().Save()
	cli, err := store.NewClient()
	if err == nil {
		_ = cli.Repo().Save()
	}
	for _, item := range store.ListClients() {
		_ = item.Repo().Save()
	}
}
`,
		})

		truth := []CallEdgeTruth{
			{CallerFile: "store/store.go", CallerName: "ListClients", CalleeFile: "store/store.go", CalleeName: "MakeClient", Category: "plain"},
			{CallerFile: "app/main.go", CallerName: "<module>", CalleeFile: "store/store.go", CalleeName: "MakeClient", Category: "var_or_module"},
			{CallerFile: "app/main.go", CallerName: "Run", CalleeFile: "app/main.go", CalleeName: "Helper", Category: "plain"},
			{CallerFile: "app/main.go", CallerName: "Run", CalleeFile: "store/store.go", CalleeName: "NewClient", Category: "var_or_module"},
			{CallerFile: "app/main.go", CallerName: "Run", CalleeFile: "store/store.go", CalleeName: "ListClients", Category: "var_or_module"},
			{CallerFile: "app/main.go", CallerName: "Run", CalleeFile: "store/store.go", CalleeName: "Repo", Category: "var_or_module"},
			{CallerFile: "app/main.go", CallerName: "Run", CalleeFile: "store/store.go", CalleeName: "Save", Category: "var_or_module"},
		}
		assertRecallAccuracy(t, "go-recall", dir, truth, 0.95, 1.00)
	})

	t.Run("TypeScript_ModuleCalls_Decorators_DestructuredFactory_JSX_CallableAlias", func(t *testing.T) {
		dir := t.TempDir()
		writeRecallFixtureFiles(t, dir, map[string]string{
			"repo.ts": `export class Repo {
  save(): void {}
}

export class Service {
  repo: Repo = new Repo();
  getRepo(): Repo {
    return this.repo;
  }
}
`,
			"di.tsx": `import { Service } from "./repo";

export function Injectable(_target: unknown): void {}

export function sanitize(v: string): string {
  return v;
}

export function createService(): Service {
  return new Service();
}

export function Badge(): null {
  return null;
}

export function Panel(): null {
  return <Badge />;
}
`,
			"main.ts": `import { Injectable, createService, sanitize } from "./di";

@Injectable
export class Controller {}

const svc = createService();
svc.getRepo().save();

export function handle(v: string): string {
  const { getRepo, repo } = createService();
  getRepo();
  repo.save();
  const fn = sanitize;
  return fn(v);
}
`,
		})

		truth := []CallEdgeTruth{
			{CallerFile: "repo.ts", CallerName: "repo", CalleeFile: "repo.ts", CalleeName: "Repo", Category: "constructor"},
			{CallerFile: "di.tsx", CallerName: "createService", CalleeFile: "repo.ts", CalleeName: "Service", Category: "constructor"},
			{CallerFile: "di.tsx", CallerName: "Panel", CalleeFile: "di.tsx", CalleeName: "Badge", Category: "constructor"},
			{CallerFile: "main.ts", CallerName: "<module>", CalleeFile: "di.tsx", CalleeName: "Injectable", Category: "decorator"},
			{CallerFile: "main.ts", CallerName: "<module>", CalleeFile: "di.tsx", CalleeName: "createService", Category: "plain"},
			{CallerFile: "main.ts", CallerName: "<module>", CalleeFile: "repo.ts", CalleeName: "getRepo", Category: "var_or_module"},
			{CallerFile: "main.ts", CallerName: "<module>", CalleeFile: "repo.ts", CalleeName: "save", Category: "var_or_module"},
			{CallerFile: "main.ts", CallerName: "handle", CalleeFile: "di.tsx", CalleeName: "createService", Category: "plain"},
			{CallerFile: "main.ts", CallerName: "handle", CalleeFile: "repo.ts", CalleeName: "getRepo", Category: "plain"},
			{CallerFile: "main.ts", CallerName: "handle", CalleeFile: "repo.ts", CalleeName: "save", Category: "var_or_module"},
			{CallerFile: "main.ts", CallerName: "handle", CalleeFile: "di.tsx", CalleeName: "sanitize", Category: "plain"},
		}
		assertRecallAccuracy(t, "typescript-recall", dir, truth, 0.95, 0.95)
	})

	t.Run("Rust_IfLet_ForLoop_MatchArms_UFCS_BuilderChain_Unwrap_CallableAlias", func(t *testing.T) {
		dir := t.TempDir()
		writeRecallFixtureFiles(t, dir, map[string]string{
			"Cargo.toml": "[package]\nname = \"demo\"\nversion = \"0.1.0\"\nedition = \"2021\"\n",
			"src/lib.rs": `pub struct Client;
pub struct Builder;

pub trait Connectable {
    fn connect(&self);
}

impl Builder {
    pub fn build(self) -> Client {
        Client
    }
}

impl Connectable for Client {
    fn connect(&self) {}
}

impl Client {
    pub fn builder() -> Builder {
        Builder
    }
}

pub fn make_client() -> Option<Client> {
    Some(Client)
}

pub fn list_clients() -> Vec<Client> {
    vec![]
}

pub fn helper() {}

pub fn run() {
    if let Some(c) = make_client() {
        c.connect();
    }
    for c in list_clients() {
        c.connect();
    }
    match make_client() {
        Some(m) => m.connect(),
        None => {}
    }
    <Client as Connectable>::connect(&Client);
    let built = Client::builder().build();
    built.connect();
    let unwrapped = make_client().unwrap();
    unwrapped.connect();
    let fn_ref = helper;
    fn_ref();
}
`,
		})

		truth := []CallEdgeTruth{
			{CallerFile: "src/lib.rs", CallerName: "run", CalleeFile: "src/lib.rs", CalleeName: "make_client", Category: "plain"},
			{CallerFile: "src/lib.rs", CallerName: "run", CalleeFile: "src/lib.rs", CalleeName: "list_clients", Category: "plain"},
			{CallerFile: "src/lib.rs", CallerName: "run", CalleeFile: "src/lib.rs", CalleeName: "builder", Category: "constructor"},
			{CallerFile: "src/lib.rs", CallerName: "run", CalleeFile: "src/lib.rs", CalleeName: "build", Category: "var_or_module"},
			{CallerFile: "src/lib.rs", CallerName: "run", CalleeFile: "src/lib.rs", CalleeName: "connect", Category: "var_or_module"},
			{CallerFile: "src/lib.rs", CallerName: "run", CalleeFile: "src/lib.rs", CalleeName: "helper", Category: "plain"},
		}
		assertRecallAccuracy(t, "rust-recall", dir, truth, 0.95, 1.00)
	})

	t.Run("Java_TryWithResources_EnhancedFor_BuilderChain_DI_MethodRefs", func(t *testing.T) {
		dir := t.TempDir()
		writeRecallFixtureFiles(t, dir, map[string]string{
			"src/main/java/demo/Order.java": `package demo;

public class Order {
    public void validate() {}
}
`,
			"src/main/java/demo/Builder.java": `package demo;

public class Builder {
    public Client build() {
        return new Client();
    }
}
`,
			"src/main/java/demo/Client.java": `package demo;

public class Client {
    public static Builder builder() {
        return new Builder();
    }
    public void send() {}
}
`,
			"src/main/java/demo/OrderService.java": `package demo;

public class OrderService {
    public void process(Order o) {
        o.validate();
    }
}
`,
			"src/main/java/demo/App.java": `package demo;

import java.util.List;
import java.util.function.Consumer;
import java.util.function.Supplier;

public class App {
    @Autowired
    private OrderService svc;

    public void run(List<Order> orders) {
        try (Client c = Client.builder().build()) {
            c.send();
        }
        var built = Client.builder().build();
        built.send();
        for (Order o : orders) {
            svc.process(o);
        }
        Supplier<Order> ctor = Order::new;
        Consumer<Order> proc = svc::process;
    }
}
`,
		})

		truth := []CallEdgeTruth{
			{CallerFile: "src/main/java/demo/Builder.java", CallerName: "build", CalleeFile: "src/main/java/demo/Client.java", CalleeName: "Client", Category: "constructor"},
			{CallerFile: "src/main/java/demo/Client.java", CallerName: "builder", CalleeFile: "src/main/java/demo/Builder.java", CalleeName: "Builder", Category: "constructor"},
			{CallerFile: "src/main/java/demo/OrderService.java", CallerName: "process", CalleeFile: "src/main/java/demo/Order.java", CalleeName: "validate", Category: "var_or_module"},
			{CallerFile: "src/main/java/demo/App.java", CallerName: "run", CalleeFile: "src/main/java/demo/Client.java", CalleeName: "builder", Category: "var_or_module"},
			{CallerFile: "src/main/java/demo/App.java", CallerName: "run", CalleeFile: "src/main/java/demo/Builder.java", CalleeName: "build", Category: "var_or_module"},
			{CallerFile: "src/main/java/demo/App.java", CallerName: "run", CalleeFile: "src/main/java/demo/Client.java", CalleeName: "send", Category: "var_or_module"},
			{CallerFile: "src/main/java/demo/App.java", CallerName: "run", CalleeFile: "src/main/java/demo/OrderService.java", CalleeName: "process", Category: "var_or_module"},
			{CallerFile: "src/main/java/demo/App.java", CallerName: "run", CalleeFile: "src/main/java/demo/Order.java", CalleeName: "Order", Category: "constructor"},
		}
		assertRecallAccuracy(t, "java-recall", dir, truth, 0.95, 1.00)
	})
}
