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

func TestTypeScriptAndTSXParsersGolden(t *testing.T) {
	mgr, err := treesitter.NewManager()
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	defer mgr.Close()

	reg := parser.NewRegistry()
	tp, err := RegisterTypeScript(reg, mgr)
	if err != nil {
		t.Fatalf("RegisterTypeScript failed: %v", err)
	}
	tsxp, err := RegisterTSX(reg, mgr)
	if err != nil {
		t.Fatalf("RegisterTSX failed: %v", err)
	}
	if tp.Language() != "typescript" || !slices.Equal(tp.Extensions(), []string{".ts", ".mts", ".cts"}) {
		t.Fatalf("TypeScriptParser metadata mismatch: %s %v", tp.Language(), tp.Extensions())
	}
	if tsxp.Language() != "tsx" || !slices.Equal(tsxp.Extensions(), []string{".tsx"}) {
		t.Fatalf("TSXParser metadata mismatch: %s %v", tsxp.Language(), tsxp.Extensions())
	}

	tsSrc, err := os.ReadFile(filepath.Join("testdata", "sample_service.ts"))
	if err != nil {
		t.Fatalf("ReadFile sample_service.ts failed: %v", err)
	}
	pf, err := reg.ParseFile(context.Background(), "src/orders/sample_service.ts", tsSrc)
	if err != nil {
		t.Fatalf("ParseFile TS failed: %v", err)
	}
	if pf.HasErrors() || pf.Language != treesitter.LangTypeScript || pf.Package != "src/orders/sample_service" {
		t.Fatalf("TS header/errors mismatch: lang=%q pkg=%q errs=%v", pf.Language, pf.Package, pf.ParseErrors)
	}

	// Verify ES module, type-only, side-effect, CommonJS, dynamic, and re-export imports.
	var hasTypeImport, hasNamespaceImp, hasInlineTypeSpec, hasSideEffect, hasDynamicImport, hasReExportStar, hasReExportAlias, hasRequire bool
	for _, imp := range pf.Imports {
		switch {
		case imp.Path == "./config" && imp.IsTypeOnly && slices.Contains(imp.Names, "Logger"):
			hasTypeImport = true
		case imp.Path == "path" && imp.Alias == "path" && imp.IsWildcard:
			hasNamespaceImp = true
		case imp.Path == "./client" && imp.IsTypeOnly && slices.Equal(imp.Names, []string{"RetryPolicy"}):
			hasInlineTypeSpec = true
		case imp.Path == "./bootstrap" && len(imp.Names) == 0 && !imp.IsTypeOnly:
			hasSideEffect = true
		case imp.Path == "./lazy_plugin" && imp.IsRelative:
			hasDynamicImport = true
		case imp.Path == "./shared" && imp.IsWildcard:
			hasReExportStar = true
		case imp.Path == "./client" && imp.Alias == "publicNormalize" && slices.Equal(imp.Names, []string{"normalizeId"}):
			hasReExportAlias = true
		case imp.Path == "./legacy" && imp.Alias == "legacyLoader":
			hasRequire = true
		}
	}
	if !hasTypeImport || !hasNamespaceImp || !hasInlineTypeSpec || !hasSideEffect || !hasDynamicImport || !hasReExportStar || !hasReExportAlias || !hasRequire {
		t.Fatalf("missing expected TS imports: %+v", pf.Imports)
	}

	// Type aliases & exports.
	taByName := make(map[string]parser.ParsedTypeAlias, len(pf.TypeAliases))
	for _, ta := range pf.TypeAliases {
		taByName[ta.Name] = ta
	}
	if ta := taByName["OrderID"]; !ta.Exported || len(ta.TypeParams) != 1 || ta.UnderlyingType != "T | number" {
		t.Fatalf("OrderID type alias mismatch: %+v", ta)
	}
	if ta := taByName["InternalToken"]; ta.Exported || ta.UnderlyingType != "string" {
		t.Fatalf("InternalToken type alias mismatch: %+v", ta)
	}

	// Classes, interfaces, enums, decorators, constructor parameter properties, private fields.
	clsByQName := make(map[string]parser.ParsedClass, len(pf.Classes))
	for _, cls := range pf.Classes {
		clsByQName[cls.QName] = cls
	}
	enumCls := clsByQName["src/orders/sample_service::OrderState"]
	if enumCls.Kind != graph.KindEnum || !enumCls.Exported || len(enumCls.Fields) != 2 {
		t.Fatalf("OrderState enum mismatch: %+v", enumCls)
	}
	repoCls := clsByQName["src/orders/sample_service::Repository"]
	if repoCls.Kind != graph.KindInterface || !slices.Equal(repoCls.Bases, []string{"BaseStore"}) || len(repoCls.Fields) != 1 {
		t.Fatalf("Repository interface mismatch: %+v", repoCls)
	}
	svcCls := clsByQName["src/orders/sample_service::OrderService"]
	if svcCls.Kind != graph.KindClass || !svcCls.Exported ||
		!slices.Equal(svcCls.Bases, []string{"BaseService"}) ||
		!slices.Equal(svcCls.Implements, []string{"Repo"}) && !slices.Equal(svcCls.Implements, []string{"Repository"}) {
		t.Fatalf("OrderService class heritage mismatch: %+v", svcCls)
	}
	if !slices.Contains(svcCls.Decorators, "sealed") || !strings.Contains(svcCls.Docstring, "Coordinates order lookups") {
		t.Fatalf("OrderService decorator/doc mismatch: decs=%v doc=%q", svcCls.Decorators, svcCls.Docstring)
	}
	baseCls := clsByQName["src/orders/sample_service::BaseService"]
	if !baseCls.Exported {
		t.Fatalf("BaseService should be Exported=true via deferred `export default BaseService`: %+v", baseCls)
	}

	// Functions, namespace functions, methods, private methods, arrows, constructors, complexity.
	fnByQName := make(map[string]parser.ParsedFunction, len(pf.Functions))
	for _, fn := range pf.Functions {
		fnByQName[fn.QName] = fn
	}
	if nsFn := fnByQName["src/orders/sample_service::Helpers::formatKey"]; !nsFn.Exported || len(nsFn.Params) != 2 {
		t.Fatalf("Helpers.formatKey namespace function mismatch: %+v", nsFn)
	}
	if privMethod := fnByQName["src/orders/sample_service::OrderService::#signPayload"]; !privMethod.IsMethod || privMethod.Exported {
		t.Fatalf("#signPayload method mismatch: %+v", privMethod)
	}
	findFn := fnByQName["src/orders/sample_service::OrderService::findById"]
	if !findFn.IsAsync || !findFn.IsMethod || findFn.ReturnType != "Promise<T | null>" {
		t.Fatalf("OrderService.findById mismatch: %+v", findFn)
	}
	evalFn := fnByQName["src/orders/sample_service::evaluateBatch"]
	if evalFn.Complexity < 5 {
		t.Fatalf("evaluateBatch complexity = %d, want >= 5", evalFn.Complexity)
	}

	// Calls: super, private method, constructor new_expression, dynamic import.
	hasCall := func(caller, receiver, callee string, isCtor bool) bool {
		for _, c := range pf.Calls {
			if c.CallerQName == caller && c.ReceiverName == receiver && c.CalleeName == callee && c.IsConstructor == isCtor {
				return true
			}
		}
		return false
	}
	if !hasCall("src/orders/sample_service::OrderService::findById", "", "DefaultClient", true) {
		t.Fatalf("missing new DefaultClient constructor call: %+v", pf.Calls)
	}
	if !hasCall("src/orders/sample_service::OrderService::findById", "this", "#signPayload", false) ||
		!hasCall("src/orders/sample_service::OrderService::findById", "dyn", "load", false) {
		t.Fatalf("missing findById method calls: %+v", pf.Calls)
	}

	// Verify TSX component rendering calls (Panel and UI.Header, skipping <div>).
	tsxSrc, err := os.ReadFile(filepath.Join("testdata", "sample_component.tsx"))
	if err != nil {
		t.Fatalf("ReadFile sample_component.tsx failed: %v", err)
	}
	tsxPF, err := reg.ParseFile(context.Background(), "src/ui/sample_component.tsx", tsxSrc)
	if err != nil {
		t.Fatalf("ParseFile TSX failed: %v", err)
	}
	if tsxPF.HasErrors() || tsxPF.Language != treesitter.LangTSX {
		t.Fatalf("TSX parse mismatch: lang=%q errs=%v", tsxPF.Language, tsxPF.ParseErrors)
	}
	var sawPanel, sawUIHeader, sawDiv bool
	for _, c := range tsxPF.Calls {
		if c.CallerQName == "src/ui/sample_component::OrderCard" {
			if c.CalleeName == "Panel" {
				sawPanel = true
			}
			if c.ReceiverName == "UI" && c.CalleeName == "Header" && c.IsMethodCall {
				sawUIHeader = true
			}
			if c.CalleeName == "div" {
				sawDiv = true
			}
		}
	}
	if !sawPanel || !sawUIHeader || sawDiv {
		t.Fatalf("TSX component calls mismatch (panel=%v header=%v div=%v): %+v", sawPanel, sawUIHeader, sawDiv, tsxPF.Calls)
	}
}

func TestJavaScriptCommonJSPrivateMembersAndErrors(t *testing.T) {
	mgr, err := treesitter.NewManager()
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	defer mgr.Close()

	reg := parser.NewRegistry()
	jp, err := RegisterJavaScript(reg, mgr)
	if err != nil {
		t.Fatalf("RegisterJavaScript failed: %v", err)
	}
	if jp.Language() != "javascript" || !slices.Equal(jp.Extensions(), []string{".js", ".mjs", ".cjs", ".jsx"}) {
		t.Fatalf("JavaScriptParser metadata mismatch: %s %v", jp.Language(), jp.Extensions())
	}

	jsSrc, err := os.ReadFile(filepath.Join("testdata", "sample_worker.js"))
	if err != nil {
		t.Fatalf("ReadFile sample_worker.js failed: %v", err)
	}
	pf, err := reg.ParseFile(context.Background(), "lib/sample_worker.js", jsSrc)
	if err != nil {
		t.Fatalf("ParseFile JS failed: %v", err)
	}
	if pf.HasErrors() || pf.Language != treesitter.LangJavaScript {
		t.Fatalf("JS parse mismatch: lang=%q errs=%v", pf.Language, pf.ParseErrors)
	}

	// Verify destructured CommonJS require + aliased destructure + default require.
	var hasFSRead, hasFSWriteAlias, hasHelperReq bool
	for _, imp := range pf.Imports {
		switch {
		case imp.Path == "fs" && slices.Contains(imp.Names, "readFile"):
			hasFSRead = true
		case imp.Path == "fs" && imp.Alias == "writeDisk" && slices.Equal(imp.Names, []string{"writeFile"}):
			hasFSWriteAlias = true
		case imp.Path == "./helpers" && imp.Alias == "helperMod" && imp.IsRelative:
			hasHelperReq = true
		}
	}
	if !hasFSRead || !hasFSWriteAlias || !hasHelperReq {
		t.Fatalf("JS CommonJS require imports mismatch: %+v", pf.Imports)
	}

	// Verify WorkerEngine exported via module.exports, #token private field, static version, #hash private method.
	if len(pf.Classes) != 1 || pf.Classes[0].Name != "WorkerEngine" || !pf.Classes[0].Exported {
		t.Fatalf("WorkerEngine class mismatch: %+v", pf.Classes)
	}
	workerFields := make(map[string]parser.ParsedField)
	for _, f := range pf.Classes[0].Fields {
		workerFields[f.Name] = f
	}
	if f := workerFields["#token"]; f.Exported || !slices.Contains(f.Decorators, "js:private") {
		t.Fatalf("#token field mismatch: %+v", f)
	}
	if f := workerFields["version"]; !f.Exported || !f.IsStatic {
		t.Fatalf("static version field mismatch: %+v", f)
	}

	// Verify exports.doubleScore, module.exports.formatTask, and JSX WidgetShell inside .jsx arrow function.
	fnByName := make(map[string]parser.ParsedFunction)
	for _, fn := range pf.Functions {
		fnByName[fn.Name] = fn
	}
	if !fnByName["formatTask"].Exported || !fnByName["doubleScore"].Exported {
		t.Fatalf("CommonJS exported functions mismatch: %+v", fnByName)
	}
	if fnByName["#hash"].Exported || !fnByName["#hash"].IsMethod {
		t.Fatalf("#hash private JS method mismatch: %+v", fnByName["#hash"])
	}

	var sawWidgetJSX bool
	for _, c := range pf.Calls {
		if c.CallerQName == "lib/sample_worker::renderWidget" && c.CalleeName == "WidgetShell" {
			sawWidgetJSX = true
		}
	}
	if !sawWidgetJSX {
		t.Fatalf("expected JSX <WidgetShell /> call recorded in JS arrow function: %+v", pf.Calls)
	}

	// Error recovery + guards + concurrency checks.
	if _, err := NewTypeScriptParser(nil); !errors.Is(err, ErrNilTreeSitterManager) {
		t.Fatalf("NewTypeScriptParser(nil) = %v", err)
	}
	if _, err := NewTSXParser(nil); !errors.Is(err, ErrNilTreeSitterManager) {
		t.Fatalf("NewTSXParser(nil) = %v", err)
	}
	if _, err := NewJavaScriptParser(nil); !errors.Is(err, ErrNilTreeSitterManager) {
		t.Fatalf("NewJavaScriptParser(nil) = %v", err)
	}

	brokenPF, err := reg.ParseFile(context.Background(), "broken.mjs", []byte("export function ok() { return 1; }\nexport function bad( {"))
	if err != nil {
		t.Fatalf("broken JS parse failed fatally: %v", err)
	}
	if !brokenPF.HasErrors() || len(brokenPF.Functions) == 0 {
		t.Fatalf("expected syntax errors + partial extraction: %+v", brokenPF)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := reg.ParseFile(context.Background(), "worker.jsx", jsSrc)
			if err != nil || res.HasErrors() || len(res.Functions) == 0 {
				errCh <- errors.New("parallel JS parse failed")
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
}

func TestJSTSReviewEdgeCasesCommonJSObjectsAndNestedJSX(t *testing.T) {
	mgr, err := treesitter.NewManager()
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	defer mgr.Close()

	jp, err := NewJavaScriptParser(mgr)
	if err != nil {
		t.Fatalf("NewJavaScriptParser failed: %v", err)
	}
	tsxp, err := NewTSXParser(mgr)
	if err != nil {
		t.Fatalf("NewTSXParser failed: %v", err)
	}

	jsSrc := []byte(`const fnShorthand = () => 1;
function helperFn() { return 2; }
module.exports = {
  fnShorthand,
  exportedAlias: helperFn,
  inlineMethod: (x) => x + 1,
};
`)
	jsPF, err := jp.Parse(context.Background(), "cjs_obj.js", jsSrc)
	if err != nil || jsPF.HasErrors() {
		t.Fatalf("Parse CommonJS object export failed: err=%v errs=%v", err, jsPF.ParseErrors)
	}
	fnByName := make(map[string]parser.ParsedFunction, len(jsPF.Functions))
	for _, fn := range jsPF.Functions {
		fnByName[fn.Name] = fn
	}
	if !fnByName["fnShorthand"].Exported || !fnByName["helperFn"].Exported || !fnByName["inlineMethod"].Exported {
		t.Fatalf("expected all CommonJS object exports marked Exported=true: %+v", fnByName)
	}

	tsxSrc := []byte(`export const View = () => <NS.Sub.Comp />;
export class CtorTest {
  constructor(private readonly cfg: string, public label: number) {}
}
`)
	tsxPF, err := tsxp.Parse(context.Background(), "deep.tsx", tsxSrc)
	if err != nil || tsxPF.HasErrors() {
		t.Fatalf("Parse deep TSX failed: err=%v errs=%v", err, tsxPF.ParseErrors)
	}
	var sawDeepJSX bool
	for _, c := range tsxPF.Calls {
		if c.CallerQName == "deep::View" && c.ReceiverName == "NS.Sub" && c.CalleeName == "Comp" && c.IsMethodCall {
			sawDeepJSX = true
		}
	}
	if !sawDeepJSX {
		t.Fatalf("missing deep nested JSX call <NS.Sub.Comp />: %+v", tsxPF.Calls)
	}
	if len(tsxPF.Classes) != 1 || len(tsxPF.Classes[0].Fields) != 2 || tsxPF.Classes[0].Fields[0].Exported || !tsxPF.Classes[0].Fields[1].Exported {
		t.Fatalf("constructor parameter properties mismatch: %+v", tsxPF.Classes)
	}
}
