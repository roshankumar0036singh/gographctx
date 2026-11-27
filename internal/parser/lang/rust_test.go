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

func TestRustParserCrateGoldenFixture(t *testing.T) {
	mgr, err := treesitter.NewManager()
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	defer mgr.Close()

	reg := parser.NewRegistry()
	rp, err := RegisterRust(reg, mgr)
	if err != nil {
		t.Fatalf("RegisterRust failed: %v", err)
	}
	if rp.Language() != "rust" || !slices.Equal(rp.Extensions(), []string{".rs"}) {
		t.Fatalf("RustParser metadata mismatch: %s %v", rp.Language(), rp.Extensions())
	}

	src, err := os.ReadFile(filepath.Join("testdata", "rust_crate_sample.rs"))
	if err != nil {
		t.Fatalf("ReadFile rust_crate_sample.rs failed: %v", err)
	}
	pf, err := reg.ParseFile(context.Background(), "src/store/mod.rs", src)
	if err != nil {
		t.Fatalf("ParseFile rust failed: %v", err)
	}
	if pf.HasErrors() || pf.Language != treesitter.LangRust || pf.Package != "src::store" {
		t.Fatalf("Rust header/errors mismatch: lang=%q pkg=%q errs=%v", pf.Language, pf.Package, pf.ParseErrors)
	}

	// Verify use statements, aliases, wildcards, inline mod imports, and external mod declaration.
	var hasHashMap, hasSetAlias, hasPubWildcard, hasExtMod, hasSuperSet bool
	for _, imp := range pf.Imports {
		switch {
		case imp.Path == "std::collections" && slices.Contains(imp.Names, "HashMap") && !imp.IsRelative:
			hasHashMap = true
		case imp.Path == "std::collections" && imp.Alias == "IdSet" && slices.Equal(imp.Names, []string{"HashSet"}):
			hasSetAlias = true
		case imp.Path == "self::storage" && imp.IsWildcard && imp.IsRelative:
			hasPubWildcard = true
		case imp.Path == "./internal_helpers" && slices.Equal(imp.Names, []string{"internal_helpers"}) && imp.IsRelative:
			hasExtMod = true
		case imp.Path == "super" && slices.Equal(imp.Names, []string{"IdSet"}):
			hasSuperSet = true
		}
	}
	if !hasHashMap || !hasSetAlias || !hasPubWildcard || !hasExtMod || !hasSuperSet {
		t.Fatalf("Rust use/mod imports mismatch: %+v", pf.Imports)
	}

	// Verify const, static, and type alias.
	varByName := make(map[string]parser.ParsedVariable, len(pf.Variables))
	for _, v := range pf.Variables {
		varByName[v.Name] = v
	}
	if v := varByName["MAX_BATCH"]; !v.IsConst || !v.Exported || v.Type != "usize" {
		t.Fatalf("MAX_BATCH const mismatch: %+v", v)
	}
	if v := varByName["GLOBAL_COUNTER"]; v.IsConst || v.Exported || v.Type != "u64" {
		t.Fatalf("GLOBAL_COUNTER static mismatch: %+v", v)
	}
	if len(pf.TypeAliases) != 1 || pf.TypeAliases[0].Name != "StoreResult" || len(pf.TypeAliases[0].TypeParams) != 1 {
		t.Fatalf("TypeAliases mismatch: %+v", pf.TypeAliases)
	}

	// Verify enum, trait (with supertraits), struct (with attributes, docstrings, fields), and impl Trait attachment.
	clsByQName := make(map[string]parser.ParsedClass, len(pf.Classes))
	for _, cls := range pf.Classes {
		clsByQName[cls.QName] = cls
	}

	enumCls := clsByQName["src::store::StoreError"]
	if enumCls.Kind != graph.KindEnum || !enumCls.Exported || len(enumCls.Fields) != 2 || !slices.Contains(enumCls.Decorators, "derive(Debug, Clone)") {
		t.Fatalf("StoreError enum mismatch: %+v", enumCls)
	}

	traitCls := clsByQName["src::store::Repository"]
	if traitCls.Kind != graph.KindInterface || !slices.Equal(traitCls.Bases, []string{"Send", "Sync"}) {
		t.Fatalf("Repository trait mismatch: %+v", traitCls)
	}

	clientCls := clsByQName["src::store::OrderClient"]
	if clientCls.Kind != graph.KindStruct || !clientCls.Exported || !slices.Equal(clientCls.Implements, []string{"Repository"}) {
		t.Fatalf("OrderClient struct/implements mismatch: %+v", clientCls)
	}
	if !slices.Equal(clientCls.TypeParams, []string{"T: Clone"}) {
		t.Fatalf("OrderClient TypeParams should ignore lifetimes, got %v", clientCls.TypeParams)
	}
	if !strings.Contains(clientCls.Docstring, "Thread-safe client backed by generic storage") || !slices.Contains(clientCls.Decorators, "derive(Debug)") {
		t.Fatalf("OrderClient docs/attrs mismatch: doc=%q decs=%v", clientCls.Docstring, clientCls.Decorators)
	}
	if len(clientCls.Fields) != 3 || !clientCls.Fields[0].Exported || clientCls.Fields[1].Exported {
		t.Fatalf("OrderClient fields mismatch: %+v", clientCls.Fields)
	}

	// Verify module fn, trait method signatures, associated constructor `new`, async method, trait impl method.
	fnByQName := make(map[string]parser.ParsedFunction, len(pf.Functions))
	for _, fn := range pf.Functions {
		fnByQName[fn.QName] = fn
	}
	if _, ok := fnByQName["src::store::storage::validate_key"]; !ok {
		t.Fatal("missing nested module function src::store::storage::validate_key")
	}
	newFn := fnByQName["src::store::OrderClient::new"]
	if newFn.IsMethod || !newFn.IsConstructor || newFn.ReceiverType != "OrderClient" {
		t.Fatalf("OrderClient::new constructor mismatch: %+v", newFn)
	}
	countFn := fnByQName["src::store::OrderClient::count_matching"]
	if !countFn.IsMethod || !countFn.IsAsync || countFn.Receiver != "&self" || countFn.Complexity < 6 || !slices.Contains(countFn.Decorators, "allow(dead_code)") {
		t.Fatalf("OrderClient::count_matching mismatch: %+v", countFn)
	}
	getFn := fnByQName["src::store::OrderClient::get"]
	if !getFn.IsMethod || !slices.Contains(getFn.Decorators, "rust:impl_trait:Repository") {
		t.Fatalf("OrderClient::get trait impl mismatch: %+v", getFn)
	}

	// Verify macro call, associated generic call (`OrderClient::<T>::record_hit`), scoped module call, and method calls.
	hasCall := func(caller, receiver, callee string) bool {
		for _, c := range pf.Calls {
			if c.CallerQName == caller && c.ReceiverName == receiver && c.CalleeName == callee {
				return true
			}
		}
		return false
	}
	if !hasCall("src::store::OrderClient::new", "", "println!") {
		t.Fatalf("missing println! macro call in new(): %+v", pf.Calls)
	}
	if !hasCall("src::store::OrderClient::get", "OrderClient", "record_hit") {
		t.Fatalf("missing OrderClient::<T>::record_hit call in get(): %+v", pf.Calls)
	}
	if !hasCall("src::store::OrderClient::save", "storage", "validate_key") {
		t.Fatalf("missing storage::validate_key scoped call in save(): %+v", pf.Calls)
	}
	if !hasCall("src::store::OrderClient::count_matching", "", "matches_prefix") ||
		!hasCall("src::store::OrderClient::count_matching", "self.service_name", "len") {
		t.Fatalf("missing closure/method calls in count_matching: %+v", pf.Calls)
	}
}

func TestRustParserErrorsGuardsAndConcurrency(t *testing.T) {
	if _, err := NewRustParser(nil); !errors.Is(err, ErrNilTreeSitterManager) {
		t.Fatalf("NewRustParser(nil) = %v", err)
	}
	var nilParser *RustParser
	if _, err := nilParser.Parse(context.Background(), "lib.rs", nil); !errors.Is(err, ErrNilTreeSitterManager) {
		t.Fatalf("nil RustParser.Parse = %v", err)
	}

	mgr, err := treesitter.NewManager()
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	defer mgr.Close()

	rp, err := NewRustParser(mgr)
	if err != nil {
		t.Fatalf("NewRustParser failed: %v", err)
	}

	brokenPF, err := rp.Parse(context.Background(), "broken.rs", []byte("pub fn valid() {}\npub fn broken( {\n"))
	if err != nil {
		t.Fatalf("broken rust parse returned fatal err: %v", err)
	}
	if !brokenPF.HasErrors() || len(brokenPF.Functions) == 0 {
		t.Fatalf("expected syntax errors + valid sibling fn: %+v", brokenPF)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := rp.Parse(canceled, "lib.rs", []byte("fn a() {}\n")); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Parse = %v", err)
	}

	src, err := os.ReadFile(filepath.Join("testdata", "rust_crate_sample.rs"))
	if err != nil {
		t.Fatalf("ReadFile fixture failed: %v", err)
	}
	var wg sync.WaitGroup
	errCh := make(chan error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := rp.Parse(context.Background(), "src/lib.rs", src)
			if err != nil || res.HasErrors() || len(res.Functions) == 0 || len(res.Classes) == 0 {
				errCh <- errors.New("parallel rust parse failed")
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
}
