package graph

import (
	"fmt"
	"slices"
	"testing"
)

func buildQueryTestGraph(t *testing.T) (*Graph, map[string]UID) {
	t.Helper()
	g := New()
	uids := make(map[string]UID)

	addNode := func(key string, n Node) {
		uid, err := g.AddNode(n)
		if err != nil {
			t.Fatalf("AddNode(%s) failed: %v", key, err)
		}
		uids[key] = uid
	}

	// Files
	addNode("fileMain", Node{Kind: KindFile, Name: "main.go", File: "cmd/main.go", Language: "go", StartLine: 1})
	addNode("fileSvc", Node{Kind: KindFile, Name: "service.go", File: "pkg/service/service.go", Language: "go", StartLine: 1})
	addNode("fileRepo", Node{Kind: KindFile, Name: "repo.go", File: "pkg/repo/repo.go", Language: "go", StartLine: 1})
	addNode("fileTest", Node{Kind: KindFile, Name: "service_test.go", File: "pkg/service/service_test.go", Language: "go", StartLine: 1})
	addNode("filePy", Node{Kind: KindFile, Name: "worker.py", File: "scripts/worker.py", Language: "python", StartLine: 1})

	// Functions forming diamond + cycle call graph:
	// main -> HandleRequest -> SaveUser (TierLocal) -> ExecSQL (TierQualifiedImport)
	// main -> AuditLog (TierSamePackage) -> ExecSQL (TierTypeNarrowed / LIKELY_CALLS)
	// main -> HandleAdmin -> AuditLog (TierLocal)
	// RecursiveCycle: CycleA -> CycleB -> CycleA, CycleA -> CycleA
	addNode("main", Node{Kind: KindFunction, Name: "main", QName: "cmd::main", File: "cmd/main.go", StartLine: 10, Complexity: 3})
	addNode("handleReq", Node{Kind: KindFunction, Name: "HandleRequest", QName: "pkg/service::HandleRequest", File: "pkg/service/service.go", StartLine: 15, Language: "go", Exported: true, Complexity: 12})
	addNode("handleAdm", Node{Kind: KindFunction, Name: "HandleAdmin", QName: "pkg/service::HandleAdmin", File: "pkg/service/service.go", StartLine: 40, Language: "go", Exported: true, Complexity: 18})
	addNode("audit", Node{Kind: KindFunction, Name: "AuditLog", QName: "pkg/service::AuditLog", File: "pkg/service/service.go", StartLine: 65, Language: "go", Exported: false, Complexity: 6})
	addNode("saveUser", Node{Kind: KindFunction, Name: "SaveUser", QName: "pkg/repo::SaveUser", File: "pkg/repo/repo.go", StartLine: 20, Language: "go", Exported: true, Complexity: 9})
	addNode("execSQL", Node{Kind: KindFunction, Name: "ExecSQL", QName: "pkg/repo::ExecSQL", File: "pkg/repo/repo.go", StartLine: 50, Language: "go", Exported: true, Complexity: 16})
	addNode("testSvc", Node{Kind: KindFunction, Name: "TestService", QName: "pkg/service::TestService", File: "pkg/service/service_test.go", StartLine: 10, Language: "go", Complexity: 4})
	addNode("pyTask", Node{Kind: KindFunction, Name: "run_worker", QName: "scripts.worker::run_worker", File: "scripts/worker.py", Language: "python", Exported: true, Complexity: 22})
	addNode("cycleA", Node{Kind: KindFunction, Name: "CycleA", QName: "pkg/repo::CycleA", File: "pkg/repo/repo.go", StartLine: 80, Language: "go"})
	addNode("cycleB", Node{Kind: KindFunction, Name: "CycleB", QName: "pkg/repo::CycleB", File: "pkg/repo/repo.go", StartLine: 90, Language: "go"})

	// Inheritance / Interface hierarchy:
	// Iface -> BaseStore (IMPLEMENTS) -> FileStore (INHERITS) -> CachedFileStore (INHERITS)
	// AuditDecorator (DECORATED_BY) on HandleAdmin
	addNode("iface", Node{Kind: KindInterface, Name: "Store", QName: "pkg/repo::Store", File: "pkg/repo/repo.go", StartLine: 5, Language: "go", Exported: true})
	addNode("baseStore", Node{Kind: KindStruct, Name: "BaseStore", QName: "pkg/repo::BaseStore", File: "pkg/repo/repo.go", StartLine: 100, Language: "go", Exported: true})
	addNode("fileStore", Node{Kind: KindStruct, Name: "FileStore", QName: "pkg/repo::FileStore", File: "pkg/repo/repo.go", StartLine: 115, Language: "go", Exported: true})
	addNode("cachedStore", Node{Kind: KindStruct, Name: "CachedFileStore", QName: "pkg/repo::CachedFileStore", File: "pkg/repo/repo.go", StartLine: 130, Language: "go", Exported: true})
	addNode("decorator", Node{Kind: KindDecorator, Name: "RequireAdmin", QName: "pkg/service::RequireAdmin", File: "pkg/service/service.go", StartLine: 5, Language: "go"})

	mustEdge := func(e Edge) {
		if err := g.AddEdge(e); err != nil {
			t.Fatalf("AddEdge(%+v) failed: %v", e, err)
		}
	}

	// Containment edges (must NOT count as dependency impact)
	mustEdge(Edge{Source: uids["fileMain"], Target: uids["main"], Kind: EdgeContains, Tier: TierStructural})
	mustEdge(Edge{Source: uids["fileSvc"], Target: uids["handleReq"], Kind: EdgeContains, Tier: TierStructural})
	mustEdge(Edge{Source: uids["fileSvc"], Target: uids["handleAdm"], Kind: EdgeContains, Tier: TierStructural})
	mustEdge(Edge{Source: uids["fileSvc"], Target: uids["audit"], Kind: EdgeContains, Tier: TierStructural})
	mustEdge(Edge{Source: uids["fileRepo"], Target: uids["saveUser"], Kind: EdgeContains, Tier: TierStructural})
	mustEdge(Edge{Source: uids["fileRepo"], Target: uids["execSQL"], Kind: EdgeContains, Tier: TierStructural})

	// Call edges
	mustEdge(Edge{Source: uids["main"], Target: uids["handleReq"], Kind: EdgeCalls, Tier: TierQualifiedImport, Sites: []CallSiteRef{{Line: 12}}})
	mustEdge(Edge{Source: uids["main"], Target: uids["handleAdm"], Kind: EdgeCalls, Tier: TierQualifiedImport, Sites: []CallSiteRef{{Line: 14}}})
	mustEdge(Edge{Source: uids["main"], Target: uids["audit"], Kind: EdgeCalls, Tier: TierSamePackage, Sites: []CallSiteRef{{Line: 16}}})
	mustEdge(Edge{Source: uids["handleReq"], Target: uids["saveUser"], Kind: EdgeCalls, Tier: TierQualifiedImport, Sites: []CallSiteRef{{Line: 22}}})
	mustEdge(Edge{Source: uids["saveUser"], Target: uids["execSQL"], Kind: EdgeCalls, Tier: TierLocal, Sites: []CallSiteRef{{Line: 28}}})
	mustEdge(Edge{Source: uids["handleAdm"], Target: uids["audit"], Kind: EdgeCalls, Tier: TierLocal, Sites: []CallSiteRef{{Line: 45}}})
	mustEdge(Edge{Source: uids["audit"], Target: uids["execSQL"], Kind: EdgeLikelyCalls, Tier: TierTypeNarrowed, Sites: []CallSiteRef{{Line: 70}}})

	// Imports & Decorators (non-call dependency edges)
	mustEdge(Edge{Source: uids["testSvc"], Target: uids["handleReq"], Kind: EdgeImports, Tier: TierStructural})
	mustEdge(Edge{Source: uids["handleAdm"], Target: uids["decorator"], Kind: EdgeDecoratedBy, Tier: TierStructural})

	// Cycles
	mustEdge(Edge{Source: uids["cycleA"], Target: uids["cycleB"], Kind: EdgeCalls, Tier: TierLocal, Sites: []CallSiteRef{{Line: 82}}})
	mustEdge(Edge{Source: uids["cycleB"], Target: uids["cycleA"], Kind: EdgeCalls, Tier: TierLocal, Sites: []CallSiteRef{{Line: 92}}})
	mustEdge(Edge{Source: uids["cycleA"], Target: uids["cycleA"], Kind: EdgeCalls, Tier: TierLocal, Sites: []CallSiteRef{{Line: 85}}})

	// Inheritance / Interface chain
	mustEdge(Edge{Source: uids["baseStore"], Target: uids["iface"], Kind: EdgeImplements, Tier: TierStructural})
	mustEdge(Edge{Source: uids["fileStore"], Target: uids["baseStore"], Kind: EdgeInherits, Tier: TierStructural})
	mustEdge(Edge{Source: uids["cachedStore"], Target: uids["fileStore"], Kind: EdgeInherits, Tier: TierStructural})
	// Direct diamond shortcut CachedFileStore -> Store
	mustEdge(Edge{Source: uids["cachedStore"], Target: uids["iface"], Kind: EdgeImplements, Tier: TierStructural})

	// CallSites for coverage metric
	_ = g.AddCallSite(CallSite{Caller: uids["main"], File: "cmd/main.go", Line: 20, LookupKey: "unknownFn", Status: CallStatusUnresolved, Tier: TierUnresolved})
	_ = g.AddCallSite(CallSite{Caller: uids["handleReq"], File: "pkg/service/service.go", Line: 30, LookupKey: "fmt.Println", Status: CallStatusExternal, Tier: TierUnresolved})

	return g, uids
}

func TestQueryCallersCalleesAndFileNodes(t *testing.T) {
	var empty Graph
	if empty.Callers(ZeroUID) != nil || empty.Callees(ZeroUID) != nil || empty.FileNodes("") != nil {
		t.Fatal("empty graph queries should return nil")
	}

	g, uids := buildQueryTestGraph(t)

	callers := g.Callers(uids["execSQL"])
	if len(callers) != 2 || callers[0].Source != uids["saveUser"] || callers[1].Source != uids["audit"] {
		t.Fatalf("Callers(execSQL) unexpected: %+v", callers)
	}
	callees := g.Callees(uids["main"])
	if len(callees) != 3 {
		t.Fatalf("Callees(main) count = %d, want 3", len(callees))
	}

	svcNodes := g.FileNodes("pkg/service/service.go")
	if len(svcNodes) != 5 || svcNodes[0].StartLine != 1 || svcNodes[1].StartLine != 5 {
		t.Fatalf("FileNodes(service.go) unexpected: %+v", svcNodes)
	}
}

func TestCallChainDirectionsCyclesAndOptions(t *testing.T) {
	var empty Graph
	if chains := empty.CallChain(GenerateNodeUID(KindFunction, "x", 0), 3, DirectionCallees); len(chains) != 0 {
		t.Fatal("empty graph CallChain should be empty")
	}

	g, uids := buildQueryTestGraph(t)

	// Depth <= 0 or missing node returns nil
	if chains := g.CallChain(uids["main"], 0, DirectionCallees); chains != nil {
		t.Fatalf("CallChain depth 0 should be nil, got %+v", chains)
	}
	if chains := g.CallChain(GenerateNodeUID(KindFunction, "missing", 0), 3, DirectionCallees); chains != nil {
		t.Fatalf("CallChain missing node should be nil, got %+v", chains)
	}

	// DirectionCallees from main (depth 3):
	// 1) main -> handleReq -> saveUser -> execSQL
	// 2) main -> audit -> execSQL
	// 3) main -> handleAdm -> audit -> execSQL
	chains := g.CallChain(uids["main"], 3, DirectionCallees)
	if len(chains) != 3 {
		t.Fatalf("CallChain(main, 3, callees) count = %d, want 3: %+v", len(chains), chains)
	}
	// Deterministic repeat check
	chains2 := g.CallChain(uids["main"], 3, DirectionCallees)
	if fmt.Sprintf("%+v", chains) != fmt.Sprintf("%+v", chains2) {
		t.Fatal("CallChain output is non-deterministic")
	}

	// CertainOnly filters out LIKELY_CALLS (audit -> execSQL), leaving audit as a leaf at depth 1/2
	certainRes := g.CallChainWithOptions(uids["main"], CallChainOptions{
		Direction:   DirectionCallees,
		MaxDepth:    3,
		CertainOnly: true,
	})
	for _, ch := range certainRes.Chains {
		for _, e := range ch {
			if !e.Tier.IsCertain() {
				t.Fatalf("CertainOnly returned non-certain edge: %+v", e)
			}
		}
	}

	// DirectionCallers from execSQL (depth 3)
	callerChains := g.CallChain(uids["execSQL"], 3, DirectionCallers)
	if len(callerChains) != 3 {
		t.Fatalf("CallChain(execSQL, 3, callers) count = %d, want 3", len(callerChains))
	}

	// Cycles & direct recursion terminate safely without infinite loops
	cycleChains := g.CallChain(uids["cycleA"], 10, DirectionCallees)
	if len(cycleChains) != 2 { // [cycleA->cycleA] and [cycleA->cycleB]
		t.Fatalf("cycle CallChain unexpected count=%d: %+v", len(cycleChains), cycleChains)
	}

	// MaxPaths truncation
	trunc := g.CallChainWithOptions(uids["main"], CallChainOptions{
		Direction: DirectionCallees,
		MaxDepth:  3,
		MaxPaths:  2,
	})
	if !trunc.Truncated || len(trunc.Chains) != 2 {
		t.Fatalf("expected truncated result with 2 chains, got truncated=%v len=%d", trunc.Truncated, len(trunc.Chains))
	}
}

func TestImpactAnalysisPropagationAndCycleSafety(t *testing.T) {
	var empty Graph
	if res := empty.ImpactAnalysis(ZeroUID, 3); res.Found {
		t.Fatal("empty graph ImpactAnalysis should report Found=false")
	}

	g, uids := buildQueryTestGraph(t)

	if res := g.ImpactAnalysis(GenerateNodeUID(KindFunction, "missing", 0), 3); res.Found {
		t.Fatal("missing UID ImpactAnalysis should report Found=false")
	}

	// Impact of changing execSQL up to depth 3:
	// Depth 1: saveUser (certain: TierLocal), audit (likely: TierTypeNarrowed)
	// Depth 2: handleReq (certain via saveUser), main (certain via audit? NO! audit->execSQL was likely, so at depth 2 main via audit is likely;
	//          Wait: main also calls audit at depth 2 and handleAdm calls audit at depth 2, then main is depth 3 via handleReq->saveUser!)
	// Let's trace exact shortest depth:
	// Depth 1: saveUser (TierLocal, certain), audit (TierTypeNarrowed, likely)
	// Depth 2: handleReq (via saveUser, TierQualifiedImport, certain), main (via audit -> execSQL, effective TierTypeNarrowed = likely!), handleAdm (via audit -> execSQL, effective TierTypeNarrowed = likely!)
	// Depth 3: testSvc (imports handleReq -> saveUser -> execSQL, TierQualifiedImport, certain!)
	res := g.ImpactAnalysis(uids["execSQL"], 3)
	if !res.Found || res.Target.UID != uids["execSQL"] {
		t.Fatal("ImpactAnalysis target mismatch")
	}
	if res.DirectCallers != 2 {
		t.Fatalf("DirectCallers = %d, want 2 (saveUser, audit)", res.DirectCallers)
	}
	if res.TotalImpacted != 6 {
		t.Fatalf("TotalImpacted = %d, want 6; got %+v", res.TotalImpacted, res.Impacted)
	}

	byKey := make(map[UID]ImpactNode, len(res.Impacted))
	for _, imp := range res.Impacted {
		byKey[imp.Node.UID] = imp
	}
	if !byKey[uids["saveUser"]].Certain || byKey[uids["saveUser"]].Depth != 1 {
		t.Fatalf("saveUser impact unexpected: %+v", byKey[uids["saveUser"]])
	}
	if byKey[uids["audit"]].Certain || byKey[uids["audit"]].EffectiveTier != TierTypeNarrowed {
		t.Fatalf("audit impact should be likely/TierTypeNarrowed: %+v", byKey[uids["audit"]])
	}
	// Honesty check (P2): main reaches execSQL in 2 hops via likely audit->execSQL, so EffectiveTier must be TierTypeNarrowed (likely), NOT certain!
	if byKey[uids["main"]].Depth != 2 || byKey[uids["main"]].Certain || byKey[uids["main"]].EffectiveTier != TierTypeNarrowed {
		t.Fatalf("main transitive certainty leaked across likely hop: %+v", byKey[uids["main"]])
	}
	// Decorator edge makes handleAdm impacted when decorator changes (let's verify below),
	// and IMPORTS edge makes testSvc impacted at depth 3 when execSQL changes!
	if byKey[uids["testSvc"]].Depth != 3 || !byKey[uids["testSvc"]].Certain {
		t.Fatalf("testSvc impact via IMPORTS unexpected: %+v", byKey[uids["testSvc"]])
	}

	expectedFiles := []string{"cmd/main.go", "pkg/repo/repo.go", "pkg/service/service.go", "pkg/service/service_test.go"}
	if !slices.Equal(res.AffectedFiles, expectedFiles) {
		t.Fatalf("AffectedFiles = %v, want %v", res.AffectedFiles, expectedFiles)
	}

	// Verify CONTAINS edges do NOT propagate impact (e.g. changing main must not mark fileMain impacted!)
	mainImpact := g.ImpactAnalysis(uids["main"], 3)
	if mainImpact.TotalImpacted != 0 {
		t.Fatalf("main has only CONTAINS incoming edge, expected 0 impacted, got %+v", mainImpact.Impacted)
	}

	// Verify DECORATED_BY edge propagates impact to decorated target
	decImpact := g.ImpactAnalysis(uids["decorator"], 2)
	if decImpact.TotalImpacted != 2 { // handleAdm (depth 1) + main (depth 2)
		t.Fatalf("decorator impact count = %d, want 2: %+v", decImpact.TotalImpacted, decImpact.Impacted)
	}

	// Verify cycle impact terminates cleanly
	cycImpact := g.ImpactAnalysis(uids["cycleA"], 5)
	if cycImpact.TotalImpacted != 1 || cycImpact.Impacted[0].Node.UID != uids["cycleB"] {
		t.Fatalf("cycleA impact unexpected: %+v", cycImpact.Impacted)
	}
}

func TestAncestorsAndDescendantsHierarchy(t *testing.T) {
	var empty Graph
	if empty.Ancestors(ZeroUID) != nil || empty.Descendants(ZeroUID) != nil {
		t.Fatal("empty hierarchy queries should return nil")
	}

	g, uids := buildQueryTestGraph(t)

	// Ancestors of CachedFileStore:
	// distance 1: FileStore (INHERITS), Store (direct IMPLEMENTS)
	// distance 2: BaseStore (via FileStore)
	anc := g.Ancestors(uids["cachedStore"])
	if len(anc) != 3 {
		t.Fatalf("Ancestors(CachedFileStore) len = %d, want 3: %+v", len(anc), anc)
	}
	if anc[0].Name != "Store" || anc[1].Name != "FileStore" || anc[2].Name != "BaseStore" {
		t.Fatalf("Ancestors ordering unexpected: %+v", anc)
	}

	// Descendants of Store interface:
	// distance 1: BaseStore, CachedFileStore (direct IMPLEMENTS)
	// distance 2: FileStore (inherits BaseStore)
	desc := g.Descendants(uids["iface"])
	if len(desc) != 3 {
		t.Fatalf("Descendants(Store) len = %d, want 3: %+v", len(desc), desc)
	}
	if desc[0].Name != "BaseStore" || desc[1].Name != "CachedFileStore" || desc[2].Name != "FileStore" {
		t.Fatalf("Descendants ordering unexpected: %+v", desc)
	}

	// Cyclic inheritance safety
	_ = g.AddEdge(Edge{Source: uids["iface"], Target: uids["cachedStore"], Kind: EdgeInherits, Tier: TierStructural})
	if got := g.Ancestors(uids["iface"]); len(got) != 3 {
		t.Fatalf("cyclic Ancestors failed to terminate cleanly: %+v", got)
	}
}

func TestSearchFiltersGlobsRankingAndPagination(t *testing.T) {
	var empty Graph
	if len(empty.Search(SearchQuery{NamePattern: "foo"})) != 0 {
		t.Fatal("empty graph Search should return nil/empty")
	}

	g, _ := buildQueryTestGraph(t)

	// Prefix glob: Handle* -> HandleRequest, HandleAdmin
	handles := g.Search(SearchQuery{NamePattern: "handle*"})
	if len(handles) != 2 || handles[0].Name != "HandleRequest" || handles[1].Name != "HandleAdmin" {
		t.Fatalf("Search(handle*) unexpected: %+v", handles)
	}

	// Single-char '?' glob + suffix wildcard: *Store
	stores := g.Search(SearchQuery{NamePattern: "*store"})
	if len(stores) != 4 { // Store, BaseStore, FileStore, CachedFileStore
		t.Fatalf("Search(*store) count = %d, want 4", len(stores))
	}
	oneChar := g.Search(SearchQuery{NamePattern: "Cycle?"})
	if len(oneChar) != 2 {
		t.Fatalf("Search(Cycle?) count = %d, want 2", len(oneChar))
	}

	// Ranking: exact match "store" before suffix/substring matches
	if stores[0].Name != "Store" {
		t.Fatalf("expected exact match Store ranked first, got %q", stores[0].Name)
	}

	// ExactName toggle
	exactStore := g.Search(SearchQuery{NamePattern: "store", ExactName: true})
	if len(exactStore) != 1 || exactStore[0].Name != "Store" {
		t.Fatalf("ExactName search mismatch: %+v", exactStore)
	}

	// Multi-criteria filters: Language, ExportedOnly, Min/MaxComplexity, FilePattern, QNamePattern, Kinds
	complexPy := g.Search(SearchQuery{
		Kinds:         []NodeKind{KindFunction},
		Languages:     []string{"Python"},
		ExportedOnly:  true,
		MinComplexity: 20,
		MaxComplexity: 30,
		FilePattern:   "scripts/*.py",
		QNamePattern:  "*worker*",
	})
	if len(complexPy) != 1 || complexPy[0].Name != "run_worker" {
		t.Fatalf("multi-criteria search mismatch: %+v", complexPy)
	}

	// Pagination & Truncation
	page1 := g.SearchWithStats(SearchQuery{
		Kinds:  []NodeKind{KindFunction},
		Offset: 1,
		Limit:  3,
	})
	if page1.Total != 10 || page1.Returned != 3 || !page1.Truncated {
		t.Fatalf("SearchWithStats pagination mismatch: total=%d returned=%d truncated=%v", page1.Total, page1.Returned, page1.Truncated)
	}
	outOfRange := g.SearchWithStats(SearchQuery{Offset: 999, Limit: 5})
	if outOfRange.Total == 0 || outOfRange.Returned != 0 || len(outOfRange.Nodes) != 0 {
		t.Fatalf("out-of-range offset unexpected: %+v", outOfRange)
	}
}

func TestRepoStatsAndReadViewParity(t *testing.T) {
	var empty Graph
	if st := empty.Stats(); st.NodeCount != 0 || st.ResolutionCoverage != 0 {
		t.Fatalf("empty graph Stats mismatch: %+v", st)
	}

	g, uids := buildQueryTestGraph(t)
	st := g.Stats()
	if st.NodeCount != 20 || st.FileCount != 5 || st.CallSiteCount != 2 {
		t.Fatalf("unexpected Stats counts: %+v", st)
	}
	if st.NodesByKind[KindFunction] != 10 || st.NodesByLanguage["python"] != 2 {
		t.Fatalf("unexpected kind/language breakdown: %+v", st)
	}
	if st.EdgesByKind[EdgeCalls] != 9 || st.EdgesByKind[EdgeLikelyCalls] != 1 {
		t.Fatalf("unexpected EdgesByKind: %+v", st.EdgesByKind)
	}
	// 10 resolved calls (9 CALLS + 1 LIKELY_CALLS) + 1 CallStatusUnresolved (External excluded) => 10/11
	wantCov := 10.0 / 11.0
	if st.ResolutionCoverage < wantCov-0.001 || st.ResolutionCoverage > wantCov+0.001 {
		t.Fatalf("ResolutionCoverage = %.4f, want %.4f", st.ResolutionCoverage, wantCov)
	}

	// ReadView parity under single RLock
	g.View(func(v *ReadView) {
		if len(v.Callers(uids["execSQL"])) != 2 || len(v.Callees(uids["main"])) != 3 {
			t.Fatal("ReadView Callers/Callees mismatch")
		}
		if len(v.CallChain(uids["main"], 3, DirectionCallees)) != 3 {
			t.Fatal("ReadView CallChain mismatch")
		}
		if ch := v.CallChainWithOptions(uids["main"], CallChainOptions{Direction: DirectionCallees, MaxDepth: 3}); len(ch.Chains) != 3 {
			t.Fatal("ReadView CallChainWithOptions mismatch")
		}
		if imp := v.ImpactAnalysis(uids["execSQL"], 3); imp.TotalImpacted != 6 {
			t.Fatal("ReadView ImpactAnalysis mismatch")
		}
		if len(v.Ancestors(uids["cachedStore"])) != 3 || len(v.Descendants(uids["iface"])) != 3 {
			t.Fatal("ReadView Ancestors/Descendants mismatch")
		}
		if len(v.FileNodes("pkg/service/service.go")) != 5 {
			t.Fatal("ReadView FileNodes mismatch")
		}
		if len(v.Search(SearchQuery{NamePattern: "handle*"})) != 2 {
			t.Fatal("ReadView Search mismatch")
		}
		if sr := v.SearchWithStats(SearchQuery{NamePattern: "handle*"}); sr.Total != 2 {
			t.Fatal("ReadView SearchWithStats mismatch")
		}
		if vst := v.Stats(); vst.NodeCount != st.NodeCount {
			t.Fatal("ReadView Stats mismatch")
		}
	})
}
