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

func TestJavaParserGoldenFixture(t *testing.T) {
	mgr, err := treesitter.NewManager()
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	defer mgr.Close()

	reg := parser.NewRegistry()
	jp, err := RegisterJava(reg, mgr)
	if err != nil {
		t.Fatalf("RegisterJava failed: %v", err)
	}
	if jp.Language() != "java" || !slices.Equal(jp.Extensions(), []string{".java"}) {
		t.Fatalf("JavaParser metadata mismatch: %s %v", jp.Language(), jp.Extensions())
	}

	src, err := os.ReadFile(filepath.Join("testdata", "OrderService.java"))
	if err != nil {
		t.Fatalf("ReadFile OrderService.java failed: %v", err)
	}
	pf, err := reg.ParseFile(context.Background(), "src/main/java/com/example/orders/OrderService.java", src)
	if err != nil {
		t.Fatalf("ParseFile Java failed: %v", err)
	}
	if pf.HasErrors() || pf.Language != treesitter.LangJava || pf.Package != "com.example.orders" {
		t.Fatalf("Java header/errors mismatch: lang=%q pkg=%q errs=%v", pf.Language, pf.Package, pf.ParseErrors)
	}

	// Verify regular, static, and wildcard imports.
	var hasListImp, hasStaticReq, hasWildcardIO bool
	for _, imp := range pf.Imports {
		switch {
		case imp.Path == "java.util" && slices.Equal(imp.Names, []string{"List"}) && !imp.IsWildcard:
			hasListImp = true
		case imp.Path == "java.util.Objects" && slices.Equal(imp.Names, []string{"requireNonNull"}):
			hasStaticReq = true
		case imp.Path == "java.io" && imp.IsWildcard:
			hasWildcardIO = true
		}
	}
	if !hasListImp || !hasStaticReq || !hasWildcardIO {
		t.Fatalf("Java imports mismatch: %+v", pf.Imports)
	}

	// Verify interface, annotation type, enum, record, outer class, and inner class.
	clsByQName := make(map[string]parser.ParsedClass, len(pf.Classes))
	for _, cls := range pf.Classes {
		clsByQName[cls.QName] = cls
	}

	repo := clsByQName["com.example.orders::OrderRepository"]
	if repo.Kind != graph.KindInterface || !repo.Exported || !slices.Equal(repo.Bases, []string{"BaseRepository"}) || len(repo.Fields) != 1 {
		t.Fatalf("OrderRepository interface mismatch: %+v", repo)
	}
	audited := clsByQName["com.example.orders::Audited"]
	if audited.Kind != graph.KindInterface || !slices.Contains(audited.Decorators, "java:annotation") {
		t.Fatalf("Audited annotation type mismatch: %+v", audited)
	}
	status := clsByQName["com.example.orders::OrderStatus"]
	if status.Kind != graph.KindEnum || len(status.Fields) < 3 {
		t.Fatalf("OrderStatus enum mismatch: %+v", status)
	}
	rec := clsByQName["com.example.orders::OrderSummary"]
	if rec.Kind != graph.KindClass || !slices.Contains(rec.Decorators, "java:record") || len(rec.Fields) != 2 {
		t.Fatalf("OrderSummary record mismatch: %+v", rec)
	}
	svc := clsByQName["com.example.orders::OrderService"]
	if svc.Kind != graph.KindClass || !svc.Exported ||
		!slices.Equal(svc.Bases, []string{"AbstractService"}) ||
		!slices.Equal(svc.Implements, []string{"OrderRepository", "Serializable"}) ||
		!slices.Contains(svc.Decorators, `Service("orderService")`) ||
		!strings.Contains(svc.Docstring, "Coordinates order lookups") {
		t.Fatalf("OrderService class mismatch: %+v", svc)
	}
	if len(svc.Fields) != 4 { // entries, MAX_RETRIES, region, zone
		t.Fatalf("OrderService fields count = %d, want 4: %+v", len(svc.Fields), svc.Fields)
	}
	builder, ok := clsByQName["com.example.orders::OrderService::Builder"]
	if !ok || !builder.Exported {
		t.Fatalf("missing inner Builder class: %+v", clsByQName)
	}

	// Verify methods, constructors, varargs, annotations, interface methods, and complexity.
	fnByQName := make(map[string]parser.ParsedFunction, len(pf.Functions))
	for _, fn := range pf.Functions {
		fnByQName[fn.QName] = fn
	}
	ctor := fnByQName["com.example.orders::OrderService::OrderService"]
	if !ctor.IsConstructor || !ctor.IsMethod || !ctor.Exported {
		t.Fatalf("OrderService constructor mismatch: %+v", ctor)
	}
	findFn := fnByQName["com.example.orders::OrderService::findById"]
	if !findFn.IsMethod || findFn.Complexity < 8 ||
		!slices.Contains(findFn.Decorators, "Override") ||
		!slices.Contains(findFn.Decorators, `Audited("lookup")`) {
		t.Fatalf("OrderService.findById mismatch: %+v", findFn)
	}
	tagFn := fnByQName["com.example.orders::OrderService::tagAll"]
	if len(tagFn.Params) != 1 || tagFn.Params[0].Type != "String..." {
		t.Fatalf("varargs parameter mismatch: %+v", tagFn.Params)
	}

	// Verify call sites: constructor super(), requireNonNull, new OrderService<>, System.out::println, chained stream/map/forEach, anonymous Runnable body.
	hasCall := func(caller, receiver, callee string, isCtor bool) bool {
		for _, c := range pf.Calls {
			if c.CallerQName == caller && c.ReceiverName == receiver && c.CalleeName == callee && c.IsConstructor == isCtor {
				return true
			}
		}
		return false
	}
	if !hasCall("com.example.orders::OrderService::OrderService", "", "super", true) ||
		!hasCall("com.example.orders::OrderService::OrderService", "", "requireNonNull", false) {
		t.Fatalf("missing constructor calls: %+v", pf.Calls)
	}
	if !hasCall("com.example.orders::OrderService::Builder::build", "", "OrderService", true) {
		t.Fatalf("missing new OrderService<> call in Builder.build: %+v", pf.Calls)
	}
	findCaller := "com.example.orders::OrderService::findById"
	if !hasCall(findCaller, "", "Runnable", true) ||
		!hasCall(findCaller, "System.out", "println", false) ||
		!hasCall(findCaller, "entries", "stream", false) ||
		!hasCall(findCaller, "entries.stream().map(item -> item.toString())", "forEach", false) ||
		!hasCall(findCaller, "notifier", "run", false) {
		t.Fatalf("missing expected calls in findById: %+v", pf.Calls)
	}
}

func TestJavaParserErrorsGuardsAndConcurrency(t *testing.T) {
	if _, err := NewJavaParser(nil); !errors.Is(err, ErrNilTreeSitterManager) {
		t.Fatalf("NewJavaParser(nil) = %v", err)
	}
	var nilParser *JavaParser
	if _, err := nilParser.Parse(context.Background(), "A.java", nil); !errors.Is(err, ErrNilTreeSitterManager) {
		t.Fatalf("nil JavaParser.Parse = %v", err)
	}

	mgr, err := treesitter.NewManager()
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	defer mgr.Close()

	jp, err := NewJavaParser(mgr)
	if err != nil {
		t.Fatalf("NewJavaParser failed: %v", err)
	}

	brokenPF, err := jp.Parse(context.Background(), "pkg/Broken.java", []byte("public class Valid { void ok() {} }\npublic class Bad { void broken( }\n"))
	if err != nil {
		t.Fatalf("broken Java parse failed fatally: %v", err)
	}
	if !brokenPF.HasErrors() || len(brokenPF.Classes) == 0 || brokenPF.Package != "" {
		t.Fatalf("broken Java parse mismatch: %+v", brokenPF)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := jp.Parse(canceled, "A.java", []byte("class A {}\n")); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Parse = %v", err)
	}

	src, err := os.ReadFile(filepath.Join("testdata", "OrderService.java"))
	if err != nil {
		t.Fatalf("ReadFile fixture failed: %v", err)
	}
	var wg sync.WaitGroup
	errCh := make(chan error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := jp.Parse(context.Background(), "OrderService.java", src)
			if err != nil || res.HasErrors() || len(res.Functions) == 0 || len(res.Classes) == 0 {
				errCh <- errors.New("parallel Java parse failed")
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
}

func TestJavaParserMethodRefsStaticWildcardAndCommaGenerics(t *testing.T) {
	mgr, err := treesitter.NewManager()
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	defer mgr.Close()

	jp, err := NewJavaParser(mgr)
	if err != nil {
		t.Fatalf("NewJavaParser failed: %v", err)
	}

	src := []byte(`package demo;

import static java.lang.Math.*;

public class Handler implements Map<String, Integer>, Serializable {
    void process(java.util.List<String> xs) {
        var r1 = System.out::println;
        var r2 = String::valueOf;
        var r3 = String::new;
        xs.forEach(System.out::println);
    }
}
`)

	pf, err := jp.Parse(context.Background(), "demo/Handler.java", src)
	if err != nil || pf.HasErrors() {
		t.Fatalf("Parse failed: err=%v errs=%v", err, pf.ParseErrors)
	}

	if len(pf.Imports) != 1 || pf.Imports[0].Path != "java.lang.Math" || !pf.Imports[0].IsWildcard {
		t.Fatalf("static wildcard import mismatch: %+v", pf.Imports)
	}
	if len(pf.Classes) != 1 || !slices.Equal(pf.Classes[0].Implements, []string{"Map", "Serializable"}) {
		t.Fatalf("comma-separated generic implements mismatch: %+v", pf.Classes)
	}

	hasRef := func(receiver, callee string, isCtor, isMethod bool) bool {
		for _, c := range pf.Calls {
			if c.CallerQName == "demo::Handler::process" &&
				c.ReceiverName == receiver &&
				c.CalleeName == callee &&
				c.IsConstructor == isCtor &&
				c.IsMethodCall == isMethod {
				return true
			}
		}
		return false
	}
	if !hasRef("System.out", "println", false, true) ||
		!hasRef("String", "valueOf", false, true) ||
		!hasRef("String", "new", true, true) {
		t.Fatalf("method reference calls mismatch: %+v", pf.Calls)
	}
}
