package lang

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/roshankumar0036singh/gographctx/internal/graph"
	"github.com/roshankumar0036singh/gographctx/internal/parser"
	"github.com/roshankumar0036singh/gographctx/internal/parser/treesitter"
)

func TestPythonParserFlaskGoldenFixture(t *testing.T) {
	mgr, err := treesitter.NewManager()
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	defer mgr.Close()

	pp, err := NewPythonParser(mgr)
	if err != nil {
		t.Fatalf("NewPythonParser failed: %v", err)
	}

	src, err := os.ReadFile(filepath.Join("testdata", "flask_service.py"))
	if err != nil {
		t.Fatalf("ReadFile fixture failed: %v", err)
	}

	pf, err := pp.Parse(context.Background(), "app/api/flask_service.py", src)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if pf.HasErrors() {
		t.Fatalf("expected error-free Python parse, got errors: %v", pf.ParseErrors)
	}
	if pf.Language != treesitter.LangPython || pf.Package != "app.api.flask_service" {
		t.Fatalf("metadata mismatch: lang=%q pkg=%q", pf.Language, pf.Package)
	}

	// Imports: __future__, standard, aliased dotted, relative, multi-item + wildcard.
	var foundFuture, foundSysAlias, foundCacheAlias, foundCacheUnaliased, foundRelErrors, foundWildcard bool
	for _, imp := range pf.Imports {
		switch {
		case imp.Path == "__future__" && slices.Contains(imp.Names, "annotations"):
			foundFuture = true
		case imp.Path == "os" && imp.Alias == "operating_system":
			foundSysAlias = true
		case imp.Path == "..storage.cache" && imp.Alias == "Cache" && slices.Equal(imp.Names, []string{"CacheClient"}) && imp.IsRelative:
			foundCacheAlias = true
		case imp.Path == "..storage.cache" && imp.Alias == "" && slices.Equal(imp.Names, []string{"StoreError"}) && imp.IsRelative:
			foundCacheUnaliased = true
		case imp.Path == "." && slices.Equal(imp.Names, []string{"errors"}) && imp.IsRelative:
			foundRelErrors = true
		case imp.Path == ".helpers" && imp.IsWildcard && imp.IsRelative:
			foundWildcard = true
		}
	}
	if !foundFuture || !foundSysAlias || !foundCacheAlias || !foundCacheUnaliased || !foundRelErrors || !foundWildcard {
		t.Fatalf("missing expected imports in: %+v", pf.Imports)
	}

	// Variables: typed, multi-target, annotation-only, ALL_CAPS const heuristic.
	varByName := make(map[string]parser.ParsedVariable, len(pf.Variables))
	for _, v := range pf.Variables {
		varByName[v.Name] = v
	}
	if v := varByName["API_PREFIX"]; v.Type != "str" || !v.IsConst || !v.Exported || v.QName != "app.api.flask_service::API_PREFIX" {
		t.Fatalf("API_PREFIX variable mismatch: %+v", v)
	}
	if v := varByName["retry_budget"]; v.IsConst || !v.Exported {
		t.Fatalf("retry_budget variable mismatch: %+v", v)
	}
	if _, ok := varByName["dry_run_mode"]; !ok {
		t.Fatal("missing tuple-unpacked variable dry_run_mode")
	}
	if v := varByName["uninitialized_hint"]; v.Type != "str" {
		t.Fatalf("uninitialized_hint variable mismatch: %+v", v)
	}

	// PEP 695 type alias.
	if len(pf.TypeAliases) != 1 || pf.TypeAliases[0].Name != "OrderFilter" ||
		pf.TypeAliases[0].UnderlyingType != "dict[str, T]" || !slices.Equal(pf.TypeAliases[0].TypeParams, []string{"T"}) {
		t.Fatalf("TypeAliases mismatch: %+v", pf.TypeAliases)
	}

	// Classes: bases, metaclass, dataclass decorator, nested class, class attributes.
	clsByQName := make(map[string]parser.ParsedClass, len(pf.Classes))
	for _, cls := range pf.Classes {
		clsByQName[cls.QName] = cls
	}
	ctrl := clsByQName["app.api.flask_service::OrderController"]
	if ctrl.Kind != graph.KindClass || !slices.Equal(ctrl.Bases, []string{"BaseController"}) ||
		!slices.Contains(ctrl.Decorators, "metaclass:RouteMeta") {
		t.Fatalf("OrderController class mismatch: %+v", ctrl)
	}
	if !strings.Contains(ctrl.Docstring, "Coordinates HTTP handlers for orders.") {
		t.Fatalf("OrderController docstring = %q", ctrl.Docstring)
	}
	fieldByName := make(map[string]parser.ParsedField, len(ctrl.Fields))
	for _, f := range ctrl.Fields {
		fieldByName[f.Name] = f
	}
	if f := fieldByName["version"]; f.Type != "int" || !f.IsStatic {
		t.Fatalf("OrderController.version field mismatch: %+v", f)
	}
	if _, ok := fieldByName["default_currency"]; !ok {
		t.Fatal("missing OrderController.default_currency class attribute")
	}
	nestedCfg, ok := clsByQName["app.api.flask_service::OrderController::Config"]
	if !ok || len(nestedCfg.Fields) != 2 {
		t.Fatalf("nested Config class mismatch: ok=%v cls=%+v", ok, nestedCfg)
	}
	payloadCls := clsByQName["app.api.flask_service::OrderPayload"]
	if !slices.Contains(payloadCls.Decorators, "dataclass") {
		t.Fatalf("OrderPayload missing dataclass decorator: %+v", payloadCls.Decorators)
	}

	// Functions & methods: async def, decorators, constructors, private/dunder, nested closure, complexity.
	fnByQName := make(map[string]parser.ParsedFunction, len(pf.Functions))
	for _, fn := range pf.Functions {
		fnByQName[fn.QName] = fn
	}

	initFn := fnByQName["app.api.flask_service::OrderController::__init__"]
	if !initFn.IsMethod || !initFn.IsConstructor || !initFn.Exported || initFn.Receiver != "self" || initFn.ReceiverType != "OrderController" {
		t.Fatalf("OrderController.__init__ mismatch: %+v", initFn)
	}
	if len(initFn.Params) != 3 || initFn.Params[1].Type != "Cache" || initFn.Params[2].Type != "float" || initFn.ReturnType != "None" {
		t.Fatalf("OrderController.__init__ params/return mismatch: params=%+v ret=%q", initFn.Params, initFn.ReturnType)
	}

	fromEnv := fnByQName["app.api.flask_service::OrderController::from_env"]
	if !fromEnv.IsMethod || fromEnv.Receiver != "cls" || fromEnv.ReturnType != "OrderController" || !slices.Contains(fromEnv.Decorators, "classmethod") {
		t.Fatalf("OrderController.from_env mismatch: %+v", fromEnv)
	}

	buildKey := fnByQName["app.api.flask_service::OrderController::build_key"]
	if !buildKey.IsMethod || buildKey.Receiver != "" || !slices.Contains(buildKey.Decorators, "staticmethod") {
		t.Fatalf("OrderController.build_key staticmethod mismatch: %+v", buildKey)
	}

	auditFn := fnByQName["app.api.flask_service::OrderController::_audit"]
	if auditFn.Exported || len(auditFn.Params) != 4 || auditFn.Params[2].Name != "*tags" || auditFn.Params[3].Name != "**meta" {
		t.Fatalf("OrderController._audit mismatch: %+v", auditFn)
	}

	handleFn := fnByQName["app.api.flask_service::OrderController::handle_order"]
	if handleFn.Complexity < 12 {
		t.Fatalf("handle_order complexity = %d, want >= 12 (if/elif/bool/try/match/comprehensions)", handleFn.Complexity)
	}
	if !strings.Contains(handleFn.Docstring, "Load an order, enrich items") {
		t.Fatalf("handle_order docstring = %q", handleFn.Docstring)
	}
	if _, ok := fnByQName["app.api.flask_service::OrderController::handle_order::normalize_tag"]; !ok {
		t.Fatal("missing nested function normalize_tag inside handle_order")
	}

	syncFn := fnByQName["app.api.flask_service::sync_orders"]
	if !syncFn.IsAsync || syncFn.IsMethod || len(syncFn.Decorators) != 2 || !strings.HasPrefix(syncFn.Decorators[0], "app.route(") {
		t.Fatalf("sync_orders async/decorators mismatch: %+v", syncFn)
	}

	// Verify call sites: <module> scope calls, decorator calls, chained method calls, generator call.
	hasCall := func(caller, receiver, callee string) bool {
		for _, c := range pf.Calls {
			if c.CallerQName == caller && c.ReceiverName == receiver && c.CalleeName == callee {
				return true
			}
		}
		return false
	}

	modCaller := "app.api.flask_service::<module>"
	if !hasCall(modCaller, "", "Blueprint") || !hasCall(modCaller, "app", "route") || !hasCall(modCaller, "", "main") {
		t.Fatalf("missing <module> scope calls: %+v", pf.Calls)
	}
	handleCaller := "app.api.flask_service::OrderController::handle_order"
	if !hasCall(handleCaller, "self.cache", "namespace") ||
		!hasCall(handleCaller, `self.cache.namespace("orders")`, "get") ||
		!hasCall(handleCaller, `self.cache.namespace("orders").get(key)`, "unwrap") ||
		!hasCall(handleCaller, "", "sum") {
		t.Fatalf("missing chained/generator calls in handle_order: %+v", pf.Calls)
	}
}

func TestPythonParserInitModuleRegistryAndErrorRecovery(t *testing.T) {
	if _, err := NewPythonParser(nil); !errors.Is(err, ErrNilTreeSitterManager) {
		t.Fatalf("NewPythonParser(nil) = %v, want ErrNilTreeSitterManager", err)
	}
	var nilParser *PythonParser
	if _, err := nilParser.Parse(context.Background(), "a.py", nil); !errors.Is(err, ErrNilTreeSitterManager) {
		t.Fatalf("nil PythonParser.Parse = %v, want ErrNilTreeSitterManager", err)
	}

	mgr, err := treesitter.NewManager()
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	defer mgr.Close()

	reg := parser.NewRegistry()
	if _, err := RegisterPython(nil, mgr); !errors.Is(err, parser.ErrInvalidParser) {
		t.Fatalf("RegisterPython(nil) = %v, want ErrInvalidParser", err)
	}
	pp, err := RegisterPython(reg, mgr)
	if err != nil {
		t.Fatalf("RegisterPython failed: %v", err)
	}
	if pp.Language() != "python" || !slices.Equal(pp.Extensions(), []string{".py", ".pyi"}) {
		t.Fatalf("PythonParser metadata mismatch: lang=%q exts=%v", pp.Language(), pp.Extensions())
	}

	// __init__.py package marker derives dotted directory as Package/scope.
	initSrc, err := os.ReadFile(filepath.Join("testdata", "pkg_init_fixture", "__init__.py"))
	if err != nil {
		t.Fatalf("ReadFile __init__.py failed: %v", err)
	}
	initPF, err := reg.ParseFile(context.Background(), "services/orders/__init__.py", initSrc)
	if err != nil {
		t.Fatalf("ParseFile __init__.py failed: %v", err)
	}
	if initPF.Package != "services.orders" || initPF.HasErrors() {
		t.Fatalf("__init__.py package mismatch: pkg=%q errs=%v", initPF.Package, initPF.ParseErrors)
	}
	if len(initPF.Functions) != 1 || initPF.Functions[0].QName != "services.orders::create_app" {
		t.Fatalf("__init__.py function QName mismatch: %+v", initPF.Functions)
	}

	// .pyi stub support + CRLF raw docstring cleanup.
	stubSrc := []byte("def ping(host: str) -> bool:\r\n    r\"\"\"Ping host.\r\n\r\n    Returns true.\r\n    \"\"\"\r\n    ...\r\n")
	stubPF, err := reg.ParseFile(context.Background(), "stubs/net.pyi", stubSrc)
	if err != nil {
		t.Fatalf("ParseFile .pyi failed: %v", err)
	}
	if len(stubPF.Functions) != 1 || strings.Contains(stubPF.Functions[0].Docstring, "\r") {
		t.Fatalf(".pyi docstring/function mismatch: %+v", stubPF.Functions)
	}
	if stubPF.Functions[0].Docstring != "Ping host.\n\nReturns true." {
		t.Fatalf(".pyi Docstring = %q", stubPF.Functions[0].Docstring)
	}

	// Syntax error resilience.
	brokenSrc := []byte("def ok_before() -> int:\n    return 1\n\ndef broken(\n")
	brokenPF, err := reg.ParseFile(context.Background(), "broken.py", brokenSrc)
	if err != nil {
		t.Fatalf("broken ParseFile returned fatal err: %v", err)
	}
	if !brokenPF.HasErrors() || len(brokenPF.ParseErrors) == 0 {
		t.Fatalf("expected syntax errors recorded for broken python: %+v", brokenPF)
	}
	if len(brokenPF.Functions) == 0 || brokenPF.Functions[0].Name != "ok_before" {
		t.Fatalf("expected ok_before extracted before broken def: %+v", brokenPF.Functions)
	}

	// Empty file & canceled context.
	var nilCtx context.Context
	emptyPF, err := pp.Parse(nilCtx, "", nil)
	if err != nil || emptyPF.LineCount != 0 {
		t.Fatalf("empty python parse mismatch: (%+v, %v)", emptyPF, err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := pp.Parse(canceled, "x.py", []byte("x = 1\n")); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Parse = %v, want context.Canceled", err)
	}

	// Concurrent parsing across goroutines.
	flaskSrc, err := os.ReadFile(filepath.Join("testdata", "flask_service.py"))
	if err != nil {
		t.Fatalf("ReadFile flask_service.py failed: %v", err)
	}
	var wg sync.WaitGroup
	errCh := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := reg.ParseFile(context.Background(), "app/api/flask_service.py", flaskSrc)
			if err != nil {
				errCh <- err
				return
			}
			if res.HasErrors() || len(res.Functions) == 0 || len(res.Classes) == 0 {
				errCh <- errors.New("unexpected parallel python parse result")
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
}
