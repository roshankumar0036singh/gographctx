package graph

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func mustAssertConsistent(t *testing.T, g *Graph) {
	t.Helper()
	if err := g.VerifyIndexConsistency(); err != nil {
		t.Fatalf("index consistency check failed: %v", err)
	}
}

func TestZeroValueGraphSafety(t *testing.T) {
	var g Graph
	if g.NodeCount() != 0 || g.EdgeCount() != 0 || g.CallSiteCount() != 0 {
		t.Fatal("zero-value graph should report zero counts")
	}
	if _, ok := g.GetNode(GenerateNodeUID(KindFunction, "x", 0)); ok {
		t.Fatal("zero-value GetNode should return false")
	}
	if g.AllNodes() != nil || g.AllEdges() != nil || g.GetCallSites() != nil {
		t.Fatal("zero-value bulk queries should return nil")
	}
	g.View(func(v *ReadView) {
		if v.NodeCount() != 0 || v.EdgeCount() != 0 {
			t.Fatal("View on zero-value graph should see zero counts")
		}
	})
	mustAssertConsistent(t, &g)

	uid, err := g.AddNode(Node{Kind: KindFunction, Name: "Foo", QName: "pkg::Foo", File: "a.go", StartLine: 5})
	if err != nil || uid.IsZero() {
		t.Fatalf("AddNode on zero-value graph failed: (%v, %v)", uid, err)
	}
	if g.NodeCount() != 1 {
		t.Fatalf("NodeCount() = %d, want 1", g.NodeCount())
	}
	mustAssertConsistent(t, &g)
}

func TestGraphNodeCRUDImmutabilityAndOverloads(t *testing.T) {
	g := New()
	mustAssertConsistent(t, g)

	fileNode := Node{
		Kind:      KindFile,
		Name:      "server.go",
		File:      "cmd/server.go",
		StartLine: 1,
		Language:  "go",
	}
	fn1 := Node{
		Kind:          KindFunction,
		Name:          "Start",
		QName:         "cmd::Server::Start",
		File:          "cmd/server.go",
		StartLine:     25,
		EndLine:       40,
		Params:        []Param{{Name: "addr", Type: "string"}},
		ReturnType:    "error",
		SignatureHash: ComputeSignatureHash([]Param{{Name: "addr", Type: "string"}}, "error"),
	}
	fn2 := Node{
		Kind:      KindFunction,
		Name:      "Start", // same short name, different QName
		QName:     "cmd::Worker::Start",
		File:      "cmd/worker.go",
		StartLine: 10,
		EndLine:   20,
	}
	// Legal Go duplicate init() functions or overloaded methods: same QName, distinct Occurrence
	init0 := Node{
		Kind:       KindFunction,
		Name:       "init",
		QName:      "cmd::init",
		Occurrence: 0,
		File:       "cmd/server.go",
		StartLine:  5,
		Params:     []Param{{Name: "s", Type: "string"}},
	}
	init1 := Node{
		Kind:       KindFunction,
		Name:       "init",
		QName:      "cmd::init",
		Occurrence: 1,
		File:       "cmd/worker.go",
		StartLine:  5,
		Params:     []Param{{Name: "b", Type: "[]byte"}},
	}
	extNode := Node{
		Kind:  KindExternal,
		Name:  "Println",
		QName: "fmt.Println",
	}

	var uids []UID
	for _, n := range []Node{fileNode, fn1, fn2, init0, init1, extNode} {
		uid, err := g.AddNode(n)
		if err != nil {
			t.Fatalf("AddNode(%s) failed: %v", n.Name, err)
		}
		uids = append(uids, uid)
	}
	fileUID, fn1UID, _, init0UID, init1UID, _ := uids[0], uids[1], uids[2], uids[3], uids[4], uids[5]
	mustAssertConsistent(t, g)

	if g.NodeCount() != 6 {
		t.Fatalf("NodeCount() = %d, want 6", g.NodeCount())
	}

	// Duplicate AddNode fails and includes existing File:StartLine
	_, err := g.AddNode(fn1)
	if !errors.Is(err, ErrNodeExists) {
		t.Fatalf("duplicate AddNode error = %v, want ErrNodeExists", err)
	}
	if !strings.Contains(err.Error(), "cmd/server.go:25") {
		t.Fatalf("ErrNodeExists message %q missing existing File:StartLine", err.Error())
	}

	// Mutating caller slice or returned getter slice MUST NOT corrupt graph or indexes
	fn1.Params[0].Type = "mutated"
	gotFn1, ok := g.GetNode(fn1UID)
	if !ok || gotFn1.Params[0].Type != "string" {
		t.Fatal("mutating caller Node after AddNode affected stored node")
	}
	gotFn1.Params[0].Type = "corrupted"
	gotFn1.Name = "Corrupted"
	gotAgain, _ := g.GetNode(fn1UID)
	if gotAgain.Params[0].Type != "string" || gotAgain.Name != "Start" {
		t.Fatal("mutating Node returned by GetNode affected graph")
	}

	byStart := g.GetByName("Start")
	if len(byStart) != 2 || byStart[0].QName != "cmd::Server::Start" || byStart[1].QName != "cmd::Worker::Start" {
		t.Fatalf("GetByName(Start) unexpected: %+v", byStart)
	}
	byStart[0].Name = "Hacked"
	mustAssertConsistent(t, g)

	// Overload / sibling honesty: GetByQName returns all, GetUniqueByQName rejects ambiguity
	inits := g.GetByQName("cmd::init")
	if len(inits) != 2 || inits[0].UID != init0UID || inits[1].UID != init1UID {
		t.Fatalf("GetByQName(cmd::init) unexpected: %+v", inits)
	}
	if _, err := g.GetUniqueByQName("cmd::init"); !errors.Is(err, ErrAmbiguousQName) {
		t.Fatalf("GetUniqueByQName ambiguous = %v, want ErrAmbiguousQName", err)
	}
	if _, err := g.GetUniqueByQName("does::not::exist"); !errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("GetUniqueByQName missing = %v, want ErrNodeNotFound", err)
	}
	uniqueStart, err := g.GetUniqueByQName("cmd::Server::Start")
	if err != nil || uniqueStart.UID != fn1UID {
		t.Fatalf("GetUniqueByQName single mismatch: (%+v, %v)", uniqueStart, err)
	}

	// Add CONTAINS edge fileUID -> fn1UID then UpsertNode to verify index consistency & edge preservation
	if err := g.AddEdge(Edge{Source: fileUID, Target: fn1UID, Kind: EdgeContains, Tier: TierStructural}); err != nil {
		t.Fatalf("AddEdge CONTAINS failed: %v", err)
	}

	updatedFn1 := gotAgain.Clone()
	updatedFn1.Name = "Serve"
	updatedFn1.QName = "cmd::Server::Serve"
	updatedFn1.File = "cmd/serve_new.go"
	updatedFn1.StartLine = 50
	updatedFn1.EndLine = 70
	if _, err := g.UpsertNode(updatedFn1); err != nil {
		t.Fatalf("UpsertNode existing failed: %v", err)
	}
	mustAssertConsistent(t, g)

	if len(g.GetByName("Start")) != 1 || len(g.GetByName("Serve")) != 1 {
		t.Fatal("Name index not updated properly on UpsertNode")
	}
	if len(g.GetByFile("cmd/serve_new.go")) != 1 {
		t.Fatal("File index not updated on UpsertNode")
	}
	if len(g.GetByQName("cmd::Server::Start")) != 0 || len(g.GetByQName("cmd::Server::Serve")) != 1 {
		t.Fatal("QName index not updated on UpsertNode")
	}
	children := g.GetContains(fileUID)
	if len(children) != 1 || children[0].Name != "Serve" {
		t.Fatalf("contains query not updated on UpsertNode: %+v", children)
	}

	// Remove one sibling leaves the other intact under GetUniqueByQName
	if err := g.RemoveNode(init0UID); err != nil {
		t.Fatalf("RemoveNode(init0) failed: %v", err)
	}
	mustAssertConsistent(t, g)
	remInit, err := g.GetUniqueByQName("cmd::init")
	if err != nil || remInit.UID != init1UID {
		t.Fatalf("GetUniqueByQName after removing sibling = (%+v, %v)", remInit, err)
	}

	// Clear resets everything
	g.Clear()
	if g.NodeCount() != 0 || g.EdgeCount() != 0 || g.CallSiteCount() != 0 {
		t.Fatal("Clear did not reset counts")
	}
	mustAssertConsistent(t, g)
}

func TestMultiSiteEdgesAndCascadeRemoval(t *testing.T) {
	g := New()

	fileA := Node{Kind: KindFile, Name: "a.go", File: "a.go", StartLine: 1}
	fileB := Node{Kind: KindFile, Name: "b.go", File: "b.go", StartLine: 1}
	fnA := Node{Kind: KindFunction, Name: "Caller", QName: "pkg::Caller", File: "a.go", StartLine: 5}
	fnB := Node{Kind: KindFunction, Name: "Target", QName: "pkg::Target", File: "b.go", StartLine: 10}
	fnC := Node{Kind: KindFunction, Name: "Helper", QName: "pkg::Helper", File: "a.go", StartLine: 20}

	uFileA, _ := g.AddNode(fileA)
	uFileB, _ := g.AddNode(fileB)
	uA, _ := g.AddNode(fnA)
	uB, _ := g.AddNode(fnB)
	uC, _ := g.AddNode(fnC)

	// Multi-site UpsertEdge merges call sites without losing earlier lines and upgrades Tier
	if err := g.AddEdge(Edge{
		Source: uA,
		Target: uB,
		Kind:   EdgeCalls,
		Tier:   TierSamePackage,
		Label:  "same pkg",
		Sites:  []CallSiteRef{{Line: 7, Column: 4}},
	}); err != nil {
		t.Fatalf("AddEdge 1st site failed: %v", err)
	}
	if err := g.UpsertEdge(Edge{
		Source: uA,
		Target: uB,
		Kind:   EdgeCalls,
		Tier:   TierQualifiedImport,
		Label:  "qualified import",
		Sites:  []CallSiteRef{{Line: 15, Column: 9}},
	}); err != nil {
		t.Fatalf("UpsertEdge 2nd site failed: %v", err)
	}
	// Duplicate AddEdge rejected
	if err := g.AddEdge(Edge{Source: uA, Target: uB, Kind: EdgeCalls, Tier: TierLocal}); !errors.Is(err, ErrEdgeExists) {
		t.Fatalf("duplicate AddEdge = %v, want ErrEdgeExists", err)
	}

	gotEdge, ok := g.GetEdge(uA, uB, EdgeCalls)
	if !ok {
		t.Fatal("GetEdge failed")
	}
	if gotEdge.Tier != TierQualifiedImport || gotEdge.ConfidenceLabel() != "certain" {
		t.Fatalf("expected upgraded tier=%v, got %v", TierQualifiedImport, gotEdge.Tier)
	}
	if len(gotEdge.Sites) != 2 || gotEdge.Sites[0].Line != 7 || gotEdge.Sites[1].Line != 15 {
		t.Fatalf("multi-site provenance lost: %+v", gotEdge.Sites)
	}
	// Mutating returned Sites does not affect graph
	gotEdge.Sites[0].Line = 999
	checkEdge, _ := g.GetEdge(uA, uB, EdgeCalls)
	if checkEdge.Sites[0].Line != 7 {
		t.Fatal("mutating Edge.Sites returned by GetEdge affected graph")
	}

	// ReplaceEdge overwrites completely
	if err := g.ReplaceEdge(Edge{
		Source: uA,
		Target: uB,
		Kind:   EdgeCalls,
		Tier:   TierLocal,
		Sites:  []CallSiteRef{{Line: 25}},
	}); err != nil {
		t.Fatalf("ReplaceEdge failed: %v", err)
	}
	replaced, _ := g.GetEdge(uA, uB, EdgeCalls)
	if len(replaced.Sites) != 1 || replaced.Sites[0].Line != 25 || replaced.Tier != TierLocal {
		t.Fatalf("ReplaceEdge did not replace completely: %+v", replaced)
	}

	// Add structural & multiple dependency edges
	_ = g.AddEdge(Edge{Source: uFileA, Target: uA, Kind: EdgeContains, Tier: TierStructural})
	_ = g.AddEdge(Edge{Source: uFileA, Target: uC, Kind: EdgeContains, Tier: TierStructural})
	_ = g.AddEdge(Edge{Source: uFileB, Target: uB, Kind: EdgeContains, Tier: TierStructural})
	_ = g.AddEdge(Edge{Source: uA, Target: uB, Kind: EdgeImports, Tier: TierStructural})
	_ = g.AddEdge(Edge{Source: uC, Target: uB, Kind: EdgeLikelyCalls, Tier: TierTypeNarrowed, Sites: []CallSiteRef{{Line: 22}}})
	_ = g.AddEdge(Edge{Source: uB, Target: uB, Kind: EdgeCalls, Tier: TierLocal, Sites: []CallSiteRef{{Line: 12}}})
	mustAssertConsistent(t, g)

	// Callers sorted stronger tier first
	callers := g.GetCallers(uB)
	if len(callers) != 3 || callers[0].Tier != TierLocal || callers[1].Tier != TierLocal || callers[2].Tier != TierTypeNarrowed {
		t.Fatalf("GetCallers ordering unexpected: %+v", callers)
	}

	// RevDeps & DependentFiles
	if deps := g.GetRevDeps(uB); len(deps) != 3 {
		t.Fatalf("GetRevDeps(uB) = %v, want 3 entries", deps)
	}
	if files := g.GetDependentFiles(uB); len(files) != 2 || files[0] != "a.go" || files[1] != "b.go" {
		t.Fatalf("GetDependentFiles(uB) = %v, want [a.go b.go]", files)
	}
	if g.GetRevDeps(uA) != nil {
		t.Fatal("uA only has CONTAINS incoming edge, should have no RevDeps")
	}

	// Removing 1 of 2 dependency edges uA->uB preserves uA in RevDeps[uB]
	if err := g.RemoveEdge(uA, uB, EdgeCalls); err != nil {
		t.Fatalf("RemoveEdge failed: %v", err)
	}
	if !slices.Contains(g.GetRevDeps(uB), uA) {
		t.Fatal("uA dropped from RevDeps while IMPORTS still exists")
	}
	_ = g.RemoveEdge(uA, uB, EdgeImports)
	if slices.Contains(g.GetRevDeps(uB), uA) {
		t.Fatal("uA remained in RevDeps after all dependency edges removed")
	}
	mustAssertConsistent(t, g)

	// RemoveNode & free-list slot reuse
	if err := g.RemoveNode(uB); err != nil {
		t.Fatalf("RemoveNode failed: %v", err)
	}
	mustAssertConsistent(t, g)
	if g.HasEdge(uB, uB, EdgeCalls) || g.HasEdge(uFileB, uB, EdgeContains) {
		t.Fatal("incident edges remained after RemoveNode")
	}

	// Re-insert into free slot and verify consistency
	newUID, err := g.AddNode(Node{Kind: KindFunction, Name: "NewTarget", QName: "pkg::NewTarget", File: "b.go", StartLine: 15})
	if err != nil || newUID.IsZero() {
		t.Fatalf("AddNode reusing free slot failed: %v", err)
	}
	mustAssertConsistent(t, g)
}

func TestAtomicReplaceFileAndNegativeDependencies(t *testing.T) {
	g := New()

	// Setup a.go defining Caller + Helper, and b.go defining Target and calling Caller,
	// plus an unresolved CallSite in b.go looking for "NewFeature".
	sigV1 := ComputeSignatureHash([]Param{{Name: "x", Type: "int"}}, "int")
	sigV2 := ComputeSignatureHash([]Param{{Name: "x", Type: "string"}}, "int")

	fileANode := Node{Kind: KindFile, Name: "a.go", File: "a.go", StartLine: 1}
	callerNode := Node{Kind: KindFunction, Name: "Caller", QName: "pkg::Caller", File: "a.go", StartLine: 5, SignatureHash: sigV1}
	vanishingNode := Node{Kind: KindFunction, Name: "OldHelper", QName: "pkg::OldHelper", File: "a.go", StartLine: 15}
	fileBNode := Node{Kind: KindFile, Name: "b.go", File: "b.go", StartLine: 1}
	bFuncNode := Node{Kind: KindFunction, Name: "UseA", QName: "pkg::UseA", File: "b.go", StartLine: 5}

	uFileA, _ := g.AddNode(fileANode)
	uCaller, _ := g.AddNode(callerNode)
	uVanish, _ := g.AddNode(vanishingNode)
	uFileB, _ := g.AddNode(fileBNode)
	uBFunc, _ := g.AddNode(bFuncNode)

	_ = g.AddEdge(Edge{Source: uFileA, Target: uCaller, Kind: EdgeContains, Tier: TierStructural})
	_ = g.AddEdge(Edge{Source: uFileA, Target: uVanish, Kind: EdgeContains, Tier: TierStructural})
	_ = g.AddEdge(Edge{Source: uFileB, Target: uBFunc, Kind: EdgeContains, Tier: TierStructural})
	// Cross-file incoming dependencies from b.go -> Caller (survivor) and OldHelper (will vanish)
	_ = g.AddEdge(Edge{Source: uBFunc, Target: uCaller, Kind: EdgeCalls, Tier: TierSamePackage, Sites: []CallSiteRef{{Line: 7}}})
	_ = g.AddEdge(Edge{Source: uBFunc, Target: uVanish, Kind: EdgeCalls, Tier: TierSamePackage, Sites: []CallSiteRef{{Line: 9}}})
	// Old outgoing edge from Caller -> OldHelper (should be removed on ReplaceFile)
	_ = g.AddEdge(Edge{Source: uCaller, Target: uVanish, Kind: EdgeCalls, Tier: TierLocal, Sites: []CallSiteRef{{Line: 6}}})

	// Unresolved call site in b.go looking for "NewFeature"
	if err := g.AddCallSite(CallSite{
		Caller:     uBFunc,
		File:       "b.go",
		Line:       11,
		CalleeExpr: "NewFeature()",
		LookupKey:  "pkg::NewFeature",
		Status:     CallStatusUnresolved,
		Tier:       TierUnresolved,
	}); err != nil {
		t.Fatalf("AddCallSite failed: %v", err)
	}
	mustAssertConsistent(t, g)

	// Invalid ReplaceFile (dangling target) MUST NOT mutate graph at all
	_, err := g.ReplaceFile("a.go", []Node{fileANode}, []Edge{
		{Source: uFileA, Target: GenerateNodeUID(KindFunction, "missing", 0), Kind: EdgeContains, Tier: TierStructural},
	}, nil)
	if !errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("expected atomic ErrNodeNotFound on invalid ReplaceFile, got %v", err)
	}
	if !g.HasNode(uVanish) || !g.HasEdge(uBFunc, uCaller, EdgeCalls) {
		t.Fatal("failed ReplaceFile partially mutated graph")
	}

	// Valid ReplaceFile on a.go:
	// - Caller survives (line moved 5->12, signature changed sigV1->sigV2)
	// - OldHelper vanishes
	// - NewFeature is added (matches unresolved CallSite in b.go!)
	updatedCaller := callerNode
	updatedCaller.StartLine = 12
	updatedCaller.SignatureHash = sigV2
	newFeature := Node{Kind: KindFunction, Name: "NewFeature", QName: "pkg::NewFeature", File: "a.go", StartLine: 30}
	uNewFeat := ComputeNodeUID(newFeature)

	res, err := g.ReplaceFile(
		"a.go",
		[]Node{fileANode, updatedCaller, newFeature},
		[]Edge{
			{Source: uFileA, Target: uCaller, Kind: EdgeContains, Tier: TierStructural},
			{Source: uFileA, Target: uNewFeat, Kind: EdgeContains, Tier: TierStructural},
			{Source: uCaller, Target: uNewFeat, Kind: EdgeCalls, Tier: TierLocal, Sites: []CallSiteRef{{Line: 14}}},
		},
		nil,
	)
	if err != nil {
		t.Fatalf("ReplaceFile failed: %v", err)
	}
	mustAssertConsistent(t, g)

	if res.Added != 1 || res.Updated != 2 || res.Removed != 1 {
		t.Fatalf("unexpected ReplaceResult counts: %+v", res)
	}
	// Surviving UID keeps incoming cross-file edge from b.go!
	if !g.HasEdge(uBFunc, uCaller, EdgeCalls) {
		t.Fatal("ReplaceFile lost incoming cross-file edge onto surviving UID")
	}
	if gotCaller, _ := g.GetNode(uCaller); gotCaller.StartLine != 12 {
		t.Fatalf("surviving node not updated in place: StartLine=%d", gotCaller.StartLine)
	}
	// Lost dependents collected BEFORE deletion
	if len(res.LostCallerUIDs) != 1 || res.LostCallerUIDs[0] != uBFunc {
		t.Fatalf("LostCallerUIDs = %v, want [%s]", res.LostCallerUIDs, uBFunc)
	}
	// Signature change detected
	if len(res.SignatureChanged) != 1 || res.SignatureChanged[0] != uCaller {
		t.Fatalf("SignatureChanged = %v, want [%s]", res.SignatureChanged, uCaller)
	}
	// Negative dependency matched unresolved CallSite in b.go!
	if len(res.PotentiallyResolvedCallSites) != 1 || res.PotentiallyResolvedCallSites[0].Caller != uBFunc {
		t.Fatalf("PotentiallyResolvedCallSites = %+v", res.PotentiallyResolvedCallSites)
	}
	if len(res.AffectedLookupKeys) != 1 || res.AffectedLookupKeys[0] != "pkg::NewFeature" {
		t.Fatalf("AffectedLookupKeys = %v", res.AffectedLookupKeys)
	}
	if len(res.AffectedFiles) != 1 || res.AffectedFiles[0] != "b.go" {
		t.Fatalf("AffectedFiles = %v, want [b.go]", res.AffectedFiles)
	}
}

func TestViewConsistencyAgainstConcurrentReplaceFile(t *testing.T) {
	g := New()
	fileA := Node{Kind: KindFile, Name: "a.go", File: "a.go", StartLine: 1}
	fn1 := Node{Kind: KindFunction, Name: "V1", QName: "pkg::V1", File: "a.go", StartLine: 5}
	fn2 := Node{Kind: KindFunction, Name: "V2", QName: "pkg::V2", File: "a.go", StartLine: 20}
	uFileA := ComputeNodeUID(fileA)
	uFn1 := ComputeNodeUID(fn1)
	uFn2 := ComputeNodeUID(fn2)

	_, err := g.ReplaceFile("a.go", []Node{fileA, fn1}, []Edge{
		{Source: uFileA, Target: uFn1, Kind: EdgeContains, Tier: TierStructural},
	}, nil)
	if err != nil {
		t.Fatalf("initial ReplaceFile failed: %v", err)
	}

	var stop atomic.Bool
	var writerWG sync.WaitGroup
	writerWG.Add(1)
	go func() {
		defer writerWG.Done()
		for i := 0; i < 1500; i++ {
			if i%2 == 0 {
				_, _ = g.ReplaceFile("a.go", []Node{fileA, fn2}, []Edge{
					{Source: uFileA, Target: uFn2, Kind: EdgeContains, Tier: TierStructural},
				}, nil)
			} else {
				_, _ = g.ReplaceFile("a.go", []Node{fileA, fn1}, []Edge{
					{Source: uFileA, Target: uFn1, Kind: EdgeContains, Tier: TierStructural},
				}, nil)
			}
		}
		stop.Store(true)
	}()

	var tornReads atomic.Int64
	var readerWG sync.WaitGroup
	for r := 0; r < 4; r++ {
		readerWG.Add(1)
		go func() {
			defer readerWG.Done()
			for !stop.Load() {
				g.View(func(v *ReadView) {
					nodes := v.GetByFile("a.go")
					children := v.GetContains(uFileA)
					if len(nodes) != 2 || len(children) != 1 || v.NodeCount() != 2 || v.EdgeCount() != 1 {
						tornReads.Add(1)
					}
				})
			}
		}()
	}

	writerWG.Wait()
	readerWG.Wait()
	if torn := tornReads.Load(); torn != 0 {
		t.Fatalf("observed %d torn reads during View()", torn)
	}
	mustAssertConsistent(t, g)
}

func TestMergeFragmentsParallelAndAtomic(t *testing.T) {
	var g Graph

	const numFiles = 12
	frags := make([]FileFragment, numFiles)
	var wg sync.WaitGroup
	for i := 0; i < numFiles; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			file := fmt.Sprintf("pkg/f%d.go", idx)
			fn := Node{
				Kind:      KindFunction,
				Name:      fmt.Sprintf("Fn%d", idx),
				QName:     fmt.Sprintf("pkg::Fn%d", idx),
				File:      file,
				StartLine: 10,
			}
			fNode := Node{Kind: KindFile, Name: fmt.Sprintf("f%d.go", idx), File: file, StartLine: 1}
			uFile := ComputeNodeUID(fNode)
			uFn := ComputeNodeUID(fn)
			edges := []Edge{{Source: uFile, Target: uFn, Kind: EdgeContains, Tier: TierStructural}}
			if idx > 0 {
				prevTarget := GenerateNodeUID(KindFunction, fmt.Sprintf("pkg::Fn%d", idx-1), 0)
				edges = append(edges, Edge{
					Source: uFn,
					Target: prevTarget,
					Kind:   EdgeCalls,
					Tier:   TierSamePackage,
					Sites:  []CallSiteRef{{Line: 12}},
				})
			}
			frags[idx] = FileFragment{
				File:  file,
				Nodes: []Node{fNode, fn},
				Edges: edges,
				CallSites: []CallSite{{
					Caller:    uFn,
					File:      file,
					Line:      14,
					LookupKey: "fmt.Println",
					Status:    CallStatusExternal,
					Tier:      TierUnresolved,
				}},
			}
		}(i)
	}
	wg.Wait()

	if err := g.MergeFragments(frags); err != nil {
		t.Fatalf("MergeFragments failed: %v", err)
	}
	mustAssertConsistent(t, &g)
	if g.NodeCount() != numFiles*2 || g.EdgeCount() != numFiles+(numFiles-1) || g.CallSiteCount() != numFiles {
		t.Fatalf("unexpected counts after MergeFragments: nodes=%d edges=%d sites=%d", g.NodeCount(), g.EdgeCount(), g.CallSiteCount())
	}
	if !g.HasCallSiteForLookup("fmt.Println") || len(g.GetCallSitesByLookupKey("fmt.Println")) != numFiles {
		t.Fatal("CallSites not indexed properly during MergeFragments")
	}

	// Atomicity test on missing endpoint
	bad := []FileFragment{{
		File:  "bad.go",
		Nodes: []Node{{Kind: KindFile, Name: "bad.go", File: "bad.go"}},
		Edges: []Edge{{
			Source: GenerateNodeUID(KindFile, "bad.go", 0),
			Target: GenerateNodeUID(KindFunction, "nonexistent", 0),
			Kind:   EdgeContains,
			Tier:   TierStructural,
		}},
	}}
	if err := g.MergeFragments(bad); !errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("expected ErrNodeNotFound, got %v", err)
	}
	if g.NodeCount() != numFiles*2 {
		t.Fatal("failed MergeFragments partially mutated graph")
	}
}

func TestVerifyIndexConsistencyDetectsCorruption(t *testing.T) {
	buildSample := func() (*Graph, UID, UID) {
		g := New()
		u1, _ := g.AddNode(Node{Kind: KindFile, Name: "a.go", File: "a.go", StartLine: 1})
		u2, _ := g.AddNode(Node{Kind: KindFunction, Name: "Foo", QName: "pkg::Foo", File: "a.go", StartLine: 5})
		_ = g.AddEdge(Edge{Source: u1, Target: u2, Kind: EdgeContains, Tier: TierStructural})
		_ = g.AddEdge(Edge{Source: u2, Target: u1, Kind: EdgeImports, Tier: TierStructural})
		return g, u1, u2
	}

	g, u1, _ := buildSample()
	g.uidToID[u1] = 99
	if err := g.VerifyIndexConsistency(); err == nil {
		t.Fatal("expected error for uidToID mismatch")
	}

	g, _, _ = buildSample()
	g.byName["wrong"] = idList{hasFirst: true, first: 0}
	if err := g.VerifyIndexConsistency(); err == nil {
		t.Fatal("expected error for byName key mismatch")
	}

	g, _, _ = buildSample()
	delete(g.byFile, "a.go")
	if err := g.VerifyIndexConsistency(); err == nil {
		t.Fatal("expected error for missing byFile entry")
	}

	g, _, _ = buildSample()
	g.edges[0].target = 99
	if err := g.VerifyIndexConsistency(); err == nil {
		t.Fatal("expected error for dangling edge target")
	}

	g, _, _ = buildSample()
	g.outgoing[0] = append(g.outgoing[0], 0)
	if err := g.VerifyIndexConsistency(); err == nil {
		t.Fatal("expected error for duplicate outgoing adjacency")
	}

	g.RebuildIndexes()
	mustAssertConsistent(t, g)
}

func TestQueriesReadViewRemoveFileAndErrorPaths(t *testing.T) {
	g := NewWithCapacity(10, 10)
	n1 := Node{Kind: KindFile, Name: "x.go", File: "x.go", StartLine: 1}
	n2 := Node{Kind: KindFunction, Name: "F1", QName: "pkg::F1", File: "x.go", StartLine: 5, EndLine: 10, Decorators: []string{"@a"}, Bases: []string{"B"}}
	n3 := Node{Kind: KindFunction, Name: "F1", QName: "pkg::F1_other", File: "x.go", StartLine: 5, EndLine: 12}
	n4 := Node{Kind: KindStruct, Name: "F1", QName: "pkg::S1", File: "x.go", StartLine: 5, EndLine: 12}
	n5 := Node{Kind: KindFunction, Name: "G1", QName: "pkg::G1", File: "y.go", StartLine: 5, EndLine: 12}

	u1, _ := g.AddNode(n1)
	u2, _ := g.AddNode(n2)
	u3, _ := g.AddNode(n3)
	u4, _ := g.AddNode(n4)
	u5, _ := g.AddNode(n5)

	_ = g.AddEdge(Edge{Source: u1, Target: u2, Kind: EdgeContains, Tier: TierStructural})
	_ = g.AddEdge(Edge{Source: u2, Target: u5, Kind: EdgeCalls, Tier: TierLocal, Sites: []CallSiteRef{{Line: 7, Label: "direct"}}})
	_ = g.AddEdge(Edge{Source: u2, Target: u5, Kind: EdgeImports, Tier: TierStructural})
	_ = g.UpsertEdge(Edge{Source: u3, Target: u5, Kind: EdgeCalls, Tier: TierSamePackage, Sites: []CallSiteRef{{Line: 8}}})
	_ = g.UpsertEdge(Edge{Source: u2, Target: u5, Kind: EdgeCalls, Tier: TierLocal, Sites: []CallSiteRef{{Line: 7, Label: "updated"}}})

	_ = g.AddCallSite(CallSite{Caller: u2, File: "x.go", Line: 9, LookupKey: "Missing", Status: CallStatusUnresolved, Tier: TierUnresolved})
	_ = g.AddCallSite(CallSite{Caller: u5, File: "y.go", Line: 10, LookupKey: "Missing2", Status: CallStatusExternal, Tier: TierUnresolved})

	// Bulk & directional queries on Graph and ReadView
	if len(g.AllNodes()) != 5 || len(g.AllEdges()) != 4 {
		t.Fatalf("unexpected AllNodes/AllEdges counts: %d/%d", len(g.AllNodes()), len(g.AllEdges()))
	}
	if len(g.GetByName("F1")) != 3 {
		t.Fatalf("expected 3 nodes named F1, got %d", len(g.GetByName("F1")))
	}
	if len(g.GetCallees(u2)) != 1 {
		t.Fatalf("expected 1 callee edge from u2, got %d", len(g.GetCallees(u2)))
	}
	if len(g.GetIncomingEdges(u5)) != 3 || len(g.GetIncomingEdges(u5, EdgeImports)) != 1 {
		t.Fatal("GetIncomingEdges mismatch")
	}
	if len(g.GetOutgoingEdges(u2)) != 2 || len(g.GetOutgoingEdges(u2, EdgeCalls)) != 1 {
		t.Fatal("GetOutgoingEdges mismatch")
	}
	if len(g.GetCallSites()) != 2 || len(g.GetCallSitesByFile("x.go")) != 1 {
		t.Fatal("GetCallSites/GetCallSitesByFile mismatch")
	}

	g.View(func(v *ReadView) {
		if v.NodeCount() != 5 || v.EdgeCount() != 4 || v.CallSiteCount() != 2 {
			t.Fatal("ReadView count mismatch")
		}
		if n, ok := v.GetNode(u2); !ok || n.Name != "F1" || !v.HasNode(u2) {
			t.Fatal("ReadView node lookup mismatch")
		}
		if e, ok := v.GetEdge(u2, u5, EdgeCalls); !ok || !v.HasEdge(u2, u5, EdgeCalls) || len(e.Sites) != 1 || e.Sites[0].Label != "updated" {
			t.Fatalf("ReadView edge lookup mismatch: %+v", e)
		}
		if len(v.AllNodes()) != 5 || len(v.AllEdges()) != 4 || len(v.GetByQName("pkg::F1")) != 1 {
			t.Fatal("ReadView bulk lookup mismatch")
		}
		if _, err := v.GetUniqueByQName("pkg::F1"); err != nil {
			t.Fatalf("ReadView.GetUniqueByQName failed: %v", err)
		}
		if len(v.GetCallers(u5)) != 2 || len(v.GetCallees(u2)) != 1 {
			t.Fatal("ReadView callers/callees mismatch")
		}
		if len(v.GetIncomingEdges(u5, EdgeCalls)) != 2 || len(v.GetOutgoingEdges(u2, EdgeImports)) != 1 {
			t.Fatal("ReadView incoming/outgoing mismatch")
		}
		if len(v.GetRevDeps(u5)) != 2 || len(v.GetDependentFiles(u5)) != 1 {
			t.Fatal("ReadView revDeps/dependentFiles mismatch")
		}
		if len(v.GetCallSites()) != 2 || len(v.GetCallSitesByFile("x.go")) != 1 || len(v.GetCallSitesByLookupKey("Missing")) != 1 || !v.HasCallSiteForLookup("Missing") {
			t.Fatal("ReadView call site lookup mismatch")
		}
	})

	// Error branches for edge & node CRUD
	missing := GenerateNodeUID(KindFunction, "missing::X", 0)
	if _, err := g.AddNode(Node{}); !errors.Is(err, ErrInvalidNode) {
		t.Fatalf("expected ErrInvalidNode, got %v", err)
	}
	if _, err := g.UpsertNode(Node{}); !errors.Is(err, ErrInvalidNode) {
		t.Fatalf("expected ErrInvalidNode on UpsertNode, got %v", err)
	}
	if _, err := g.UpsertNode(Node{Kind: KindFunction, Name: "BrandNew", QName: "pkg::BrandNew", File: "y.go", StartLine: 20}); err != nil {
		t.Fatalf("UpsertNode new failed: %v", err)
	}
	if err := g.RemoveNode(ZeroUID); !errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("RemoveNode(ZeroUID) = %v, want ErrNodeNotFound", err)
	}
	if err := g.RemoveNode(missing); !errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("RemoveNode(missing) = %v, want ErrNodeNotFound", err)
	}
	if err := g.AddEdge(Edge{Source: missing, Target: u5, Kind: EdgeCalls, Tier: TierLocal}); !errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("AddEdge missing src = %v", err)
	}
	if err := g.AddEdge(Edge{Source: u5, Target: missing, Kind: EdgeCalls, Tier: TierLocal}); !errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("AddEdge missing tgt = %v", err)
	}
	if err := g.UpsertEdge(Edge{Source: missing, Target: u5, Kind: EdgeCalls, Tier: TierLocal}); !errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("UpsertEdge missing src = %v", err)
	}
	if err := g.ReplaceEdge(Edge{Source: missing, Target: u5, Kind: EdgeCalls, Tier: TierLocal}); !errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("ReplaceEdge missing src = %v", err)
	}
	if err := g.ReplaceEdge(Edge{Source: u4, Target: u5, Kind: EdgeCalls, Tier: TierLocal}); err != nil {
		t.Fatalf("ReplaceEdge insert new failed: %v", err)
	}
	if err := g.RemoveEdge(u5, u2, EdgeCalls); !errors.Is(err, ErrEdgeNotFound) {
		t.Fatalf("RemoveEdge missing = %v, want ErrEdgeNotFound", err)
	}
	if err := g.AddCallSite(CallSite{}); !errors.Is(err, ErrInvalidCallSite) {
		t.Fatalf("AddCallSite invalid = %v, want ErrInvalidCallSite", err)
	}

	// RemoveFile batch deletion removes nodes, edges & call sites cleanly
	if n := g.RemoveFile("x.go"); n != 4 {
		t.Fatalf("RemoveFile(x.go) = %d, want 4", n)
	}
	if g.CallSiteCount() != 1 {
		t.Fatalf("CallSiteCount after RemoveFile = %d, want 1", g.CallSiteCount())
	}
	mustAssertConsistent(t, g)
}
