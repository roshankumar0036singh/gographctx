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

func TestGoParserGoldenFixture(t *testing.T) {
	mgr, err := treesitter.NewManager()
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	defer mgr.Close()

	gp, err := NewGoParser(mgr)
	if err != nil {
		t.Fatalf("NewGoParser failed: %v", err)
	}

	fixturePath := filepath.Join("testdata", "sample_service.go")
	src, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("ReadFile(%q) failed: %v", fixturePath, err)
	}

	pf, err := gp.Parse(context.Background(), "orderservice/service.go", src)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	if pf.LineCount < 500 {
		t.Fatalf("fixture LineCount = %d, want >= 500", pf.LineCount)
	}
	if pf.HasErrors() {
		t.Fatalf("expected error-free parse, got errors: %v", pf.ParseErrors)
	}
	if pf.Language != treesitter.LangGo || pf.Package != "orderservice" || len(pf.ContentHash) != 64 {
		t.Fatalf("header metadata mismatch: lang=%q pkg=%q hashLen=%d", pf.Language, pf.Package, len(pf.ContentHash))
	}

	// Verify imports (standard, aliased, dot-wildcard, blank, relative).
	impByPath := make(map[string]parser.ParsedImport, len(pf.Imports))
	for _, imp := range pf.Imports {
		impByPath[imp.Path] = imp
	}
	if imp := impByPath["sync"]; imp.Alias != "syncalias" || imp.IsWildcard || imp.IsRelative {
		t.Fatalf("sync import mismatch: %+v", imp)
	}
	if imp := impByPath["unicode"]; imp.Alias != "." || !imp.IsWildcard {
		t.Fatalf("unicode dot import mismatch: %+v", imp)
	}
	if imp := impByPath["embed"]; imp.Alias != "_" || imp.IsWildcard {
		t.Fatalf("embed blank import mismatch: %+v", imp)
	}
	if imp := impByPath["./internal/cfg"]; imp.Alias != "localcfg" || !imp.IsRelative {
		t.Fatalf("relative import mismatch: %+v", imp)
	}

	// Verify package-level constants and variables (including multi-identifier var spec).
	varByName := make(map[string]parser.ParsedVariable, len(pf.Variables))
	for _, v := range pf.Variables {
		varByName[v.Name] = v
	}
	if v := varByName["ServiceVersion"]; !v.IsConst || !v.Exported || !strings.Contains(v.Docstring, "ServiceVersion identifies") {
		t.Fatalf("ServiceVersion var mismatch: %+v", v)
	}
	if v := varByName["internalRetryLimit"]; !v.IsConst || v.Exported {
		t.Fatalf("internalRetryLimit var mismatch: %+v", v)
	}
	if v := varByName["DefaultTimeout"]; v.IsConst || v.Type != "time.Duration" || !v.Exported {
		t.Fatalf("DefaultTimeout var mismatch: %+v", v)
	}
	if v := varByName["fallbackRegion"]; v.Type != "string" || v.Exported {
		t.Fatalf("fallbackRegion var mismatch: %+v", v)
	}
	if v := varByName["zone"]; v.Type != "string" || v.Exported {
		t.Fatalf("zone var mismatch: %+v", v)
	}
	if v := varByName["globalRegistry"]; v.QName != "orderservice::globalRegistry" {
		t.Fatalf("globalRegistry QName mismatch: %+v", v)
	}

	// Verify type aliases and defined non-struct/interface types.
	aliasByName := make(map[string]parser.ParsedTypeAlias, len(pf.TypeAliases))
	for _, ta := range pf.TypeAliases {
		aliasByName[ta.Name] = ta
	}
	if ta := aliasByName["OrderID"]; ta.UnderlyingType != "string" || !ta.Exported {
		t.Fatalf("OrderID alias mismatch: %+v", ta)
	}
	if ta := aliasByName["GenericIndex"]; len(ta.TypeParams) != 1 || ta.UnderlyingType != "map[OrderID]V" {
		t.Fatalf("GenericIndex alias mismatch: %+v", ta)
	}
	if ta := aliasByName["StatusCode"]; ta.UnderlyingType != "int" {
		t.Fatalf("StatusCode defined type mismatch: %+v", ta)
	}
	if _, ok := aliasByName["HandlerFunc"]; !ok {
		t.Fatal("missing HandlerFunc defined function type")
	}

	// Verify structs, embedded types, multi-name fields, tags, and interfaces.
	classByName := make(map[string]parser.ParsedClass, len(pf.Classes))
	for _, cls := range pf.Classes {
		classByName[cls.Name] = cls
	}

	svcCls := classByName["Service"]
	if svcCls.Kind != graph.KindStruct || !svcCls.Exported || len(svcCls.TypeParams) != 1 {
		t.Fatalf("Service struct mismatch: %+v", svcCls)
	}
	if !slices.Equal(svcCls.Bases, []string{"BaseEntity", "syncalias.RWMutex"}) {
		t.Fatalf("Service embedded Bases = %v, want [BaseEntity syncalias.RWMutex]", svcCls.Bases)
	}

	baseCls := classByName["BaseEntity"]
	baseFields := make(map[string]parser.ParsedField, len(baseCls.Fields))
	for _, f := range baseCls.Fields {
		baseFields[f.Name] = f
	}
	if f := baseFields["ID"]; !f.Exported || f.Type != "string" || !slices.Contains(f.Decorators, "`json:\"id\"`") {
		t.Fatalf("BaseEntity.ID field mismatch: %+v", f)
	}
	if baseFields["CreatedAt"].Type != "int64" || baseFields["UpdatedAt"].Type != "int64" || baseFields["deleted"].Exported {
		t.Fatalf("BaseEntity multi/unexported fields mismatch: %+v", baseFields)
	}

	repoIface := classByName["Repository"]
	if repoIface.Kind != graph.KindInterface || !slices.Equal(repoIface.Bases, []string{"ReaderStore", "WriterStore"}) {
		t.Fatalf("Repository interface mismatch: %+v", repoIface)
	}
	idIface := classByName["Identifier"]
	if idIface.Kind != graph.KindInterface || len(idIface.Bases) != 0 || !slices.Contains(idIface.Decorators, "go:constraint:~string | ~int64") {
		t.Fatalf("Identifier constraint interface mismatch: %+v", idIface)
	}

	// Verify functions, methods, interface methods, multiple init()s, variadic params, complexity.
	var initCount int
	var ifaceMethods int
	fnByQName := make(map[string]parser.ParsedFunction, len(pf.Functions))
	for _, fn := range pf.Functions {
		if fn.Name == "init" {
			initCount++
			if !slices.Contains(fn.Decorators, "go:init") {
				t.Fatalf("init function missing go:init tag: %+v", fn)
			}
		}
		if slices.Contains(fn.Decorators, "go:interface_method") {
			ifaceMethods++
		}
		fnByQName[fn.QName] = fn
	}
	if initCount != 2 {
		t.Fatalf("initCount = %d, want 2", initCount)
	}
	if ifaceMethods < 6 {
		t.Fatalf("ifaceMethods = %d, want >= 6 across ReaderStore/WriterStore/Repository/AuditLogger", ifaceMethods)
	}

	newSvc := fnByQName["orderservice::NewService"]
	if !newSvc.Exported || newSvc.IsMethod || len(newSvc.Params) != 4 || newSvc.Params[3].Type != "...Middleware" {
		t.Fatalf("NewService function mismatch: %+v", newSvc)
	}
	if !strings.Contains(newSvc.Docstring, "NewService constructs a configured Service instance.") {
		t.Fatalf("NewService docstring = %q", newSvc.Docstring)
	}

	fetchFn := fnByQName["orderservice::Service::FetchOrder"]
	if !fetchFn.IsMethod || fetchFn.Receiver != "*Service[T]" || fetchFn.ReceiverType != "Service" {
		t.Fatalf("Service.FetchOrder mismatch: %+v", fetchFn)
	}
	if fetchFn.ReturnType != "(*Order, error)" || fetchFn.Complexity < 6 {
		t.Fatalf("Service.FetchOrder return/complexity mismatch: ret=%q complexity=%d", fetchFn.ReturnType, fetchFn.Complexity)
	}

	describeFn := fnByQName["orderservice::DescribeValue"]
	if describeFn.Complexity < 9 {
		t.Fatalf("DescribeValue complexity = %d, want >= 9 for nested type/value switches", describeFn.Complexity)
	}

	// Verify call sites across package init, init(), methods, closures, generics, and parens.
	hasCall := func(caller, receiver, callee string) bool {
		for _, c := range pf.Calls {
			if c.CallerQName == caller && c.ReceiverName == receiver && c.CalleeName == callee {
				return true
			}
		}
		return false
	}

	if !hasCall("orderservice::<package_init>", "", "NewRegistry") {
		t.Fatal("missing package-level initializer call to NewRegistry")
	}
	if !hasCall("orderservice::init", "", "registerBuiltinCodecs") || !hasCall("orderservice::init", "localcfg", "EnsureDefaults") {
		t.Fatal("missing calls inside init() functions")
	}
	if !hasCall("orderservice::Service::FetchOrder", "s.cache", "Get") || !hasCall("orderservice::Service::FetchOrder", "cached", "Clone") {
		t.Fatal("missing method/chained calls in FetchOrder")
	}
	if !hasCall("orderservice::Service::handleCreate", "localcfg", "Decode") {
		t.Fatal("missing generic call localcfg.Decode[Order] in handleCreate")
	}
	if !hasCall("orderservice::Service::RunBatch", "wg", "Done") || !hasCall("orderservice::Service::RunBatch", "s", "ProcessCommand") {
		t.Fatal("missing goroutine/defer calls inside RunBatch")
	}
	if !hasCall("orderservice::AuditHook", "", "fn") {
		t.Fatal("missing parenthesized call (fn) in AuditHook")
	}
}

func TestGoParserTestFilePatterns(t *testing.T) {
	mgr, err := treesitter.NewManager()
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	defer mgr.Close()

	gp, err := NewGoParser(mgr)
	if err != nil {
		t.Fatalf("NewGoParser failed: %v", err)
	}

	fixturePath := filepath.Join("testdata", "sample_service_test.go")
	src, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("ReadFile(%q) failed: %v", fixturePath, err)
	}

	pf, err := gp.Parse(context.Background(), "orderservice/sample_service_test.go", src)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if pf.HasErrors() {
		t.Fatalf("unexpected syntax errors: %v", pf.ParseErrors)
	}

	wantDecorators := map[string]string{
		"TestOrderWorkflow":         "go:test",
		"Test_edgeCase":             "go:test",
		"BenchmarkOrderProcessing":  "go:benchmark",
		"FuzzOrderPayload":          "go:fuzz",
		"ExampleService_FetchOrder": "go:example",
	}

	seen := make(map[string]parser.ParsedFunction, len(pf.Functions))
	for _, fn := range pf.Functions {
		seen[fn.Name] = fn
	}

	for name, wantTag := range wantDecorators {
		fn, ok := seen[name]
		if !ok {
			t.Fatalf("missing test function %q", name)
		}
		if !slices.Contains(fn.Decorators, wantTag) {
			t.Fatalf("%s decorators = %v, want %q", name, fn.Decorators, wantTag)
		}
		if fn.QName != "orderservice_test::"+name {
			t.Fatalf("%s QName = %q, want orderservice_test::%s", name, fn.QName, name)
		}
	}

	for _, nonTest := range []string{"TestnotExportedPattern", "testHelper"} {
		fn, ok := seen[nonTest]
		if !ok {
			t.Fatalf("missing helper %q", nonTest)
		}
		if len(fn.Decorators) != 0 {
			t.Fatalf("%s should not receive test decorators, got %v", nonTest, fn.Decorators)
		}
	}
}

func TestGoParserErrorRecoveryRegistryAndConcurrency(t *testing.T) {
	if _, err := NewGoParser(nil); !errors.Is(err, ErrNilTreeSitterManager) {
		t.Fatalf("NewGoParser(nil) = %v, want ErrNilTreeSitterManager", err)
	}
	var nilParser *GoParser
	if _, err := nilParser.Parse(context.Background(), "a.go", nil); !errors.Is(err, ErrNilTreeSitterManager) {
		t.Fatalf("nil GoParser.Parse = %v, want ErrNilTreeSitterManager", err)
	}

	mgr, err := treesitter.NewManager()
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	defer mgr.Close()

	reg := parser.NewRegistry()
	if _, err := RegisterGo(nil, mgr); !errors.Is(err, parser.ErrInvalidParser) {
		t.Fatalf("RegisterGo(nil) = %v, want ErrInvalidParser", err)
	}
	gp, err := RegisterGo(reg, mgr)
	if err != nil {
		t.Fatalf("RegisterGo failed: %v", err)
	}
	if gp.Language() != "go" || !slices.Equal(gp.Extensions(), []string{".go"}) {
		t.Fatalf("GoParser metadata mismatch: lang=%q exts=%v", gp.Language(), gp.Extensions())
	}

	// Empty file parses cleanly.
	var nilCtx context.Context
	emptyPF, err := reg.ParseFile(nilCtx, "empty.go", nil)
	if err != nil || emptyPF.LineCount != 0 || emptyPF.HasErrors() {
		t.Fatalf("empty parse mismatch: (%+v, %v)", emptyPF, err)
	}

	// Broken file records syntax errors while still extracting valid sibling declarations.
	brokenSrc := []byte(`package broken

func ValidBefore() int {
	return Helper()
}

func BrokenMid( {

func ValidAfter() {
	ValidBefore()
}
`)
	brokenPF, err := reg.ParseFile(context.Background(), "broken.go", brokenSrc)
	if err != nil {
		t.Fatalf("broken ParseFile returned fatal err: %v", err)
	}
	if !brokenPF.HasErrors() || len(brokenPF.ParseErrors) == 0 {
		t.Fatalf("expected syntax errors recorded for broken source, got %+v", brokenPF)
	}
	foundValidBefore := false
	for _, fn := range brokenPF.Functions {
		if fn.Name == "ValidBefore" {
			foundValidBefore = true
		}
	}
	if !foundValidBefore {
		t.Fatalf("expected ValidBefore extracted despite later syntax error: %+v", brokenPF.Functions)
	}

	// Canceled context fails promptly.
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := gp.Parse(canceled, "a.go", []byte("package a\n")); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Parse = %v, want context.Canceled", err)
	}

	// Verify DeriveModuleScope edge cases.
	if got := DeriveModuleScope("cmd/api/main.go", "main"); got != "cmd/api" {
		t.Fatalf("DeriveModuleScope(cmd/api/main.go, main) = %q, want cmd/api", got)
	}
	if got := DeriveModuleScope("/tmp/abs/main.go", "sample"); got != "sample" {
		t.Fatalf("DeriveModuleScope abs path = %q, want sample", got)
	}
	if got := DeriveModuleScope("", "pkg"); got != "pkg" {
		t.Fatalf("DeriveModuleScope empty path = %q, want pkg", got)
	}

	// Concurrent parsing stress test across multiple goroutines.
	fixtureSrc, err := os.ReadFile(filepath.Join("testdata", "sample_service.go"))
	if err != nil {
		t.Fatalf("ReadFile fixture failed: %v", err)
	}
	var wg sync.WaitGroup
	errCh := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := reg.ParseFile(context.Background(), "orderservice/service.go", fixtureSrc)
			if err != nil {
				errCh <- err
				return
			}
			if res.HasErrors() || len(res.Functions) == 0 || len(res.Classes) == 0 {
				errCh <- errors.New("unexpected incomplete parallel parse result")
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
}

func TestGoParserCRLFBlockDocstringsGenericsAndRangeComplexity(t *testing.T) {
	mgr, err := treesitter.NewManager()
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	defer mgr.Close()

	gp, err := NewGoParser(mgr)
	if err != nil {
		t.Fatalf("NewGoParser failed: %v", err)
	}

	src := "package edge\r\n\r\n" +
		"/*\r\n * Process handles generic calls.\r\n * Second line.\r\n */\r\n" +
		"func Process(xs []int) {\r\n" +
		"\tfor _, x := range xs {\r\n" +
		"\t\t_ = Decode[int](x)\r\n" +
		"\t\t_ = pkg.Decode[int](x)\r\n" +
		"\t\t_ = a.b.Decode[int](x)\r\n" +
		"\t}\r\n" +
		"}\r\n"

	pf, err := gp.Parse(context.Background(), "edge/process.go", []byte(src))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if pf.HasErrors() || len(pf.Functions) != 1 {
		t.Fatalf("unexpected parse result: errs=%v funcs=%d", pf.ParseErrors, len(pf.Functions))
	}

	fn := pf.Functions[0]
	if strings.Contains(fn.Docstring, "\r") {
		t.Fatalf("Docstring retained carriage return: %q", fn.Docstring)
	}
	if fn.Docstring != "Process handles generic calls.\nSecond line." {
		t.Fatalf("Docstring = %q, want %q", fn.Docstring, "Process handles generic calls.\nSecond line.")
	}
	if fn.Complexity != 2 {
		t.Fatalf("Complexity with single for..range = %d, want 2", fn.Complexity)
	}

	hasCall := func(receiver, callee string) bool {
		for _, c := range pf.Calls {
			if c.ReceiverName == receiver && c.CalleeName == callee {
				return true
			}
		}
		return false
	}
	if !hasCall("", "Decode") || !hasCall("pkg", "Decode") || !hasCall("a.b", "Decode") {
		t.Fatalf("generic selector calls mismatch: %+v", pf.Calls)
	}
}
