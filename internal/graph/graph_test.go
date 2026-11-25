package graph

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

func mustAssertConsistent(t *testing.T, g *Graph) {
	t.Helper()
	if err := g.VerifyIndexConsistency(); err != nil {
		t.Fatalf("index consistency check failed: %v", err)
	}
}

func TestGraphNodeCRUDAndIndexes(t *testing.T) {
	g := New()
	mustAssertConsistent(t, g)

	// Invalid add
	if err := g.AddNode(nil); !errors.Is(err, ErrNilNode) {
		t.Fatalf("AddNode(nil) = %v, want ErrNilNode", err)
	}
	if err := g.UpsertNode(nil); !errors.Is(err, ErrNilNode) {
		t.Fatalf("UpsertNode(nil) = %v, want ErrNilNode", err)
	}

	fileNode := &Node{
		Kind:      KindFile,
		Name:      "server.go",
		QName:     "file::cmd/server.go",
		File:      "cmd/server.go",
		StartLine: 1,
		Language:  "go",
	}
	fn1 := &Node{
		Kind:       KindFunction,
		Name:       "Start",
		QName:      "cmd::Server::Start",
		File:       "cmd/server.go",
		StartLine:  25,
		EndLine:    40,
		Params:     []Param{{Name: "addr", Type: "string"}},
		ReturnType: "error",
	}
	fn2 := &Node{
		Kind:       KindFunction,
		Name:       "Start", // same short name, different QName
		QName:      "cmd::Worker::Start",
		File:       "cmd/worker.go",
		StartLine:  10,
		EndLine:    20,
		Params:     []Param{},
		ReturnType: "",
	}
	// Overloaded methods sharing QName, differing signature
	overload1 := &Node{
		Kind:      KindFunction,
		Name:      "Parse",
		QName:     "parser::Parse",
		File:      "parser.go",
		StartLine: 5,
		Params:    []Param{{Name: "s", Type: "string"}},
	}
	overload2 := &Node{
		Kind:      KindFunction,
		Name:      "Parse",
		QName:     "parser::Parse",
		File:      "parser.go",
		StartLine: 15,
		Params:    []Param{{Name: "b", Type: "[]byte"}},
	}
	extMod := &Node{
		Kind:  KindModule,
		Name:  "fmt",
		QName: "fmt",
		// File intentionally empty for external module
	}

	for _, n := range []*Node{fileNode, fn1, fn2, overload1, overload2, extMod} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode(%s) failed: %v", n.Name, err)
		}
	}
	mustAssertConsistent(t, g)

	if g.NodeCount() != 6 {
		t.Fatalf("NodeCount() = %d, want 6", g.NodeCount())
	}

	// Duplicate AddNode fails
	dup := fn1.Clone()
	if err := g.AddNode(dup); !errors.Is(err, ErrNodeExists) {
		t.Fatalf("duplicate AddNode error = %v, want ErrNodeExists", err)
	}

	// Mutating original input slice must not mutate graph node
	fn1.Params[0].Type = "mutated"
	gotFn1, ok := g.GetNode(fn1.UID)
	if !ok || gotFn1.Params[0].Type != "string" {
		t.Fatal("mutating caller Node after AddNode affected stored node")
	}

	// Zero-UID lookups return false / nil
	if _, ok := g.GetNode(ZeroUID); ok {
		t.Fatal("GetNode(ZeroUID) should return false")
	}
	if g.HasNode(ZeroUID) {
		t.Fatal("HasNode(ZeroUID) should return false")
	}
	if !g.HasNode(fn1.UID) {
		t.Fatal("HasNode(fn1.UID) = false, want true")
	}

	// Index lookups: ByName, ByFile, ByQName
	if got := g.GetByName(""); got != nil {
		t.Fatal("GetByName empty should return nil")
	}
	byStart := g.GetByName("Start")
	if len(byStart) != 2 || byStart[0].QName != "cmd::Server::Start" || byStart[1].QName != "cmd::Worker::Start" {
		t.Fatalf("GetByName(Start) unexpected: %+v", byStart)
	}

	if got := g.GetByFile(""); got != nil {
		t.Fatal("GetByFile empty should return nil")
	}
	byServerFile := g.GetByFile("cmd/server.go")
	if len(byServerFile) != 2 || byServerFile[0].StartLine != 1 || byServerFile[1].StartLine != 25 {
		t.Fatalf("GetByFile(cmd/server.go) unexpected: %+v", byServerFile)
	}

	if _, ok := g.GetByQName(""); ok {
		t.Fatal("GetByQName empty should return false")
	}
	if _, ok := g.GetByQName("does::not::exist"); ok {
		t.Fatal("GetByQName missing should return false")
	}
	if got := g.GetAllByQName(""); got != nil {
		t.Fatal("GetAllByQName empty should return nil")
	}
	if got, ok := g.GetByQName("cmd::Server::Start"); !ok || got.UID != fn1.UID {
		t.Fatalf("GetByQName single mismatch: (%+v, %v)", got, ok)
	}
	if first, ok := g.GetByQName("parser::Parse"); !ok || first.UID != overload1.UID {
		t.Fatalf("GetByQName overloaded mismatch: (%+v, %v)", first, ok)
	}
	overloads := g.GetAllByQName("parser::Parse")
	if len(overloads) != 2 || overloads[0].StartLine != 5 || overloads[1].StartLine != 15 {
		t.Fatalf("GetAllByQName(parser::Parse) unexpected: %+v", overloads)
	}

	// Add CONTAINS edge fileNode -> fn1 so UpsertNode can test updating contains pointer
	if err := g.AddEdge(&Edge{Source: fileNode.UID, Target: fn1.UID, Kind: EdgeContains, Confidence: 1.0}); err != nil {
		t.Fatalf("AddEdge CONTAINS failed: %v", err)
	}

	// UpsertNode updates existing node's Name, File, QName, StartLine while preserving UID & edges
	updatedFn1 := gotFn1.Clone()
	updatedFn1.Name = "Serve"
	updatedFn1.QName = "cmd::Server::Serve"
	updatedFn1.File = "cmd/serve_new.go"
	updatedFn1.StartLine = 50
	updatedFn1.EndLine = 70
	if err := g.UpsertNode(updatedFn1); err != nil {
		t.Fatalf("UpsertNode existing failed: %v", err)
	}
	mustAssertConsistent(t, g)

	if len(g.GetByName("Start")) != 1 {
		t.Fatal("old Name index entry not removed on UpsertNode")
	}
	if len(g.GetByName("Serve")) != 1 {
		t.Fatal("new Name index entry not added on UpsertNode")
	}
	if len(g.GetByFile("cmd/serve_new.go")) != 1 {
		t.Fatal("new File index entry not added on UpsertNode")
	}
	if _, ok := g.GetByQName("cmd::Server::Start"); ok {
		t.Fatal("old QName index entry not removed on UpsertNode")
	}
	if got, ok := g.GetByQName("cmd::Server::Serve"); !ok || got.StartLine != 50 {
		t.Fatal("new QName index entry missing or stale")
	}
	children := g.GetContains(fileNode.UID)
	if len(children) != 1 || children[0].Name != "Serve" {
		t.Fatalf("contains index pointer not updated on UpsertNode: %+v", children)
	}

	// UpsertNode inserts a brand new node when UID absent (and computes UID if zero)
	brandNew := &Node{
		Kind:  KindStruct,
		Name:  "Options",
		QName: "cmd::Options",
		File:  "cmd/options.go",
	}
	if err := g.UpsertNode(brandNew); err != nil {
		t.Fatalf("UpsertNode new failed: %v", err)
	}
	if brandNew.UID.IsZero() || !g.HasNode(brandNew.UID) {
		t.Fatal("UpsertNode new did not assign UID or store node")
	}
	mustAssertConsistent(t, g)

	// Remove one overload preserves the other overload under QName
	if err := g.RemoveNode(overload1.UID); err != nil {
		t.Fatalf("RemoveNode(overload1) failed: %v", err)
	}
	mustAssertConsistent(t, g)
	if remaining := g.GetAllByQName("parser::Parse"); len(remaining) != 1 || remaining[0].UID != overload2.UID {
		t.Fatalf("overloaded QName corrupted after removing one overload: %+v", remaining)
	}

	// AllNodes deterministic ordering
	all := g.AllNodes()
	for i := 1; i < len(all); i++ {
		if all[i-1].UID.Compare(all[i].UID) >= 0 {
			t.Fatal("AllNodes not sorted strictly by UID")
		}
	}

	// Remove missing / zero node errors
	if err := g.RemoveNode(ZeroUID); !errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("RemoveNode(ZeroUID) = %v, want ErrNodeNotFound", err)
	}
	if err := g.RemoveNode(overload1.UID); !errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("RemoveNode(missing) = %v, want ErrNodeNotFound", err)
	}

	// Clear resets graph completely
	g.Clear()
	if g.NodeCount() != 0 || g.EdgeCount() != 0 {
		t.Fatalf("Clear left nodes=%d edges=%d", g.NodeCount(), g.EdgeCount())
	}
	mustAssertConsistent(t, g)
}

func TestGraphEdgeCRUDIndexesAndCascadeRemoval(t *testing.T) {
	g := New()

	fileA := &Node{Kind: KindFile, Name: "a.go", QName: "file::a.go", File: "a.go", StartLine: 1}
	fileB := &Node{Kind: KindFile, Name: "b.go", QName: "file::b.go", File: "b.go", StartLine: 1}
	fnA := &Node{Kind: KindFunction, Name: "Caller", QName: "pkg::Caller", File: "a.go", StartLine: 5}
	fnB := &Node{Kind: KindFunction, Name: "Target", QName: "pkg::Target", File: "b.go", StartLine: 10}
	fnC := &Node{Kind: KindFunction, Name: "Helper", QName: "pkg::Helper", File: "a.go", StartLine: 20}

	for _, n := range []*Node{fileA, fileB, fnA, fnB, fnC} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode failed: %v", err)
		}
	}

	// Edge validation & missing endpoint checks
	if err := g.AddEdge(nil); !errors.Is(err, ErrNilEdge) {
		t.Fatalf("AddEdge(nil) = %v, want ErrNilEdge", err)
	}
	if err := g.UpsertEdge(nil); !errors.Is(err, ErrNilEdge) {
		t.Fatalf("UpsertEdge(nil) = %v, want ErrNilEdge", err)
	}

	missingUID := GenerateUID("missing", "1")
	if err := g.AddEdge(&Edge{Source: missingUID, Target: fnB.UID, Kind: EdgeCalls, Confidence: 1}); !errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("missing source error = %v, want ErrNodeNotFound", err)
	}
	if err := g.AddEdge(&Edge{Source: fnA.UID, Target: missingUID, Kind: EdgeCalls, Confidence: 1}); !errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("missing target error = %v, want ErrNodeNotFound", err)
	}
	if err := g.UpsertEdge(&Edge{Source: missingUID, Target: fnB.UID, Kind: EdgeCalls, Confidence: 1}); !errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("UpsertEdge missing source = %v, want ErrNodeNotFound", err)
	}
	if err := g.UpsertEdge(&Edge{Source: fnA.UID, Target: missingUID, Kind: EdgeCalls, Confidence: 1}); !errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("UpsertEdge missing target = %v, want ErrNodeNotFound", err)
	}

	edges := []*Edge{
		{Source: fileA.UID, Target: fnA.UID, Kind: EdgeContains, Confidence: 1.0},
		{Source: fileA.UID, Target: fnC.UID, Kind: EdgeContains, Confidence: 1.0},
		{Source: fileB.UID, Target: fnB.UID, Kind: EdgeContains, Confidence: 1.0},
		{Source: fileA.UID, Target: fileB.UID, Kind: EdgeImports, Confidence: 1.0},
		{Source: fnA.UID, Target: fnB.UID, Kind: EdgeCalls, Confidence: 0.95, Tier: 3, Line: 7, Label: "qualified import"},
		{Source: fnC.UID, Target: fnB.UID, Kind: EdgeLikelyCalls, Confidence: 0.60, Tier: 7, Line: 22, Label: "type narrowed"},
		{Source: fnA.UID, Target: fnB.UID, Kind: EdgeImports, Confidence: 1.0}, // second dependency edge between fnA -> fnB
		{Source: fnB.UID, Target: fnB.UID, Kind: EdgeCalls, Confidence: 1.0, Tier: 2, Line: 12, Label: "recursive"},
	}
	for _, e := range edges {
		if err := g.AddEdge(e); err != nil {
			t.Fatalf("AddEdge(%v) failed: %v", e, err)
		}
	}
	mustAssertConsistent(t, g)

	if g.EdgeCount() != len(edges) {
		t.Fatalf("EdgeCount() = %d, want %d", g.EdgeCount(), len(edges))
	}

	// Duplicate AddEdge rejected
	if err := g.AddEdge(&Edge{Source: fnA.UID, Target: fnB.UID, Kind: EdgeCalls, Confidence: 0.9}); !errors.Is(err, ErrEdgeExists) {
		t.Fatalf("duplicate AddEdge = %v, want ErrEdgeExists", err)
	}

	// Edge lookups & sorting
	if _, ok := g.GetEdge(ZeroUID, fnB.UID, EdgeCalls); ok {
		t.Fatal("GetEdge with ZeroUID should return false")
	}
	if !g.HasEdge(fnA.UID, fnB.UID, EdgeCalls) {
		t.Fatal("HasEdge(fnA -> fnB, CALLS) = false, want true")
	}

	// Callers of fnB: fnB (1.0), fnA (0.95), fnC (0.60)
	callers := g.GetCallers(fnB.UID)
	if len(callers) != 3 || callers[0].Source != fnB.UID || callers[1].Source != fnA.UID || callers[2].Source != fnC.UID {
		t.Fatalf("GetCallers(fnB) order/content unexpected: %+v", callers)
	}
	callees := g.GetCallees(fnA.UID)
	if len(callees) != 1 || callees[0].Target != fnB.UID {
		t.Fatalf("GetCallees(fnA) unexpected: %+v", callees)
	}

	// Zero UID queries return nil
	if g.GetCallers(ZeroUID) != nil || g.GetCallees(ZeroUID) != nil ||
		g.GetIncomingEdges(ZeroUID) != nil || g.GetOutgoingEdges(ZeroUID) != nil ||
		g.GetContains(ZeroUID) != nil || g.GetRevDeps(ZeroUID) != nil ||
		g.GetDependentFiles(ZeroUID) != nil {
		t.Fatal("zero UID index lookups should return nil")
	}

	// Incoming / Outgoing filters
	if in := g.GetIncomingEdges(fnB.UID, EdgeCalls); len(in) != 2 {
		t.Fatalf("GetIncomingEdges(fnB, EdgeCalls) = %d, want 2", len(in))
	}
	if inAll := g.GetIncomingEdges(fnB.UID); len(inAll) != 5 {
		t.Fatalf("GetIncomingEdges(fnB) = %d, want 5", len(inAll))
	}
	if out := g.GetOutgoingEdges(fnA.UID, EdgeImports); len(out) != 1 {
		t.Fatalf("GetOutgoingEdges(fnA, EdgeImports) = %d, want 1", len(out))
	}
	if none := g.GetOutgoingEdges(fnC.UID, EdgeInherits); len(none) != 0 {
		t.Fatalf("expected 0 inherits edges, got %d", len(none))
	}

	// Contains ordering (by StartLine)
	contained := g.GetContains(fileA.UID)
	if len(contained) != 2 || contained[0].UID != fnA.UID || contained[1].UID != fnC.UID {
		t.Fatalf("GetContains(fileA) unexpected: %+v", contained)
	}

	// RevDeps & DependentFiles
	revDeps := g.GetRevDeps(fnB.UID)
	if len(revDeps) != 3 { // fnA, fnB, fnC
		t.Fatalf("GetRevDeps(fnB) len = %d, want 3", len(revDeps))
	}
	depFiles := g.GetDependentFiles(fnB.UID)
	if len(depFiles) != 2 || depFiles[0] != "a.go" || depFiles[1] != "b.go" {
		t.Fatalf("GetDependentFiles(fnB) = %v, want [a.go b.go]", depFiles)
	}
	if g.GetRevDeps(fnA.UID) != nil || g.GetDependentFiles(fnA.UID) != nil {
		t.Fatal("fnA has only CONTAINS incoming edge, should have no RevDeps")
	}

	// UpsertEdge updates existing edge metadata without duplicating
	if err := g.UpsertEdge(&Edge{
		Source:     fnC.UID,
		Target:     fnB.UID,
		Kind:       EdgeLikelyCalls,
		Confidence: 0.75,
		Tier:       7,
		Line:       23,
		Label:      "upgraded",
	}); err != nil {
		t.Fatalf("UpsertEdge existing failed: %v", err)
	}
	mustAssertConsistent(t, g)
	upEdge, _ := g.GetEdge(fnC.UID, fnB.UID, EdgeLikelyCalls)
	if upEdge.Confidence != 0.75 || upEdge.Label != "upgraded" {
		t.Fatalf("UpsertEdge did not update edge: %+v", upEdge)
	}

	// UpsertEdge adds new edge when absent
	if err := g.UpsertEdge(&Edge{Source: fnA.UID, Target: fnC.UID, Kind: EdgeCalls, Confidence: 0.98, Tier: 2}); err != nil {
		t.Fatalf("UpsertEdge new failed: %v", err)
	}
	mustAssertConsistent(t, g)

	// Removing 1 of 2 dependency edges between fnA -> fnB MUST keep fnA in RevDeps[fnB]
	if err := g.RemoveEdge(fnA.UID, fnB.UID, EdgeCalls); err != nil {
		t.Fatalf("RemoveEdge(fnA->fnB, CALLS) failed: %v", err)
	}
	mustAssertConsistent(t, g)
	if !containsUID(g.GetRevDeps(fnB.UID), fnA.UID) {
		t.Fatal("fnA dropped from RevDeps[fnB] while IMPORTS edge still exists")
	}

	// Removing the 2nd dependency edge between fnA -> fnB MUST remove fnA from RevDeps[fnB]
	if err := g.RemoveEdge(fnA.UID, fnB.UID, EdgeImports); err != nil {
		t.Fatalf("RemoveEdge(fnA->fnB, IMPORTS) failed: %v", err)
	}
	mustAssertConsistent(t, g)
	if containsUID(g.GetRevDeps(fnB.UID), fnA.UID) {
		t.Fatal("fnA remained in RevDeps[fnB] after all dependency edges removed")
	}

	// RemoveEdge errors
	if err := g.RemoveEdge(ZeroUID, fnB.UID, EdgeCalls); !errors.Is(err, ErrEdgeNotFound) {
		t.Fatalf("RemoveEdge(ZeroUID) = %v, want ErrEdgeNotFound", err)
	}
	if err := g.RemoveEdge(fnA.UID, fnB.UID, EdgeCalls); !errors.Is(err, ErrEdgeNotFound) {
		t.Fatalf("RemoveEdge(missing) = %v, want ErrEdgeNotFound", err)
	}

	// RemoveEdge CONTAINS cleans up contains index
	if err := g.RemoveEdge(fileA.UID, fnC.UID, EdgeContains); err != nil {
		t.Fatalf("RemoveEdge CONTAINS failed: %v", err)
	}
	mustAssertConsistent(t, g)
	if len(g.GetContains(fileA.UID)) != 1 {
		t.Fatal("contains index not updated after RemoveEdge CONTAINS")
	}

	// AllEdges deterministic ordering
	allEdges := g.AllEdges()
	for i := 1; i < len(allEdges); i++ {
		if compareEdgesCanonical(allEdges[i-1], allEdges[i]) >= 0 {
			t.Fatal("AllEdges not sorted canonically")
		}
	}

	// RemoveNode cascades self-loops, callers, callees, contains, revDeps
	if err := g.RemoveNode(fnB.UID); err != nil {
		t.Fatalf("RemoveNode(fnB) failed: %v", err)
	}
	mustAssertConsistent(t, g)
	if g.HasEdge(fnB.UID, fnB.UID, EdgeCalls) || g.HasEdge(fileB.UID, fnB.UID, EdgeContains) {
		t.Fatal("incident edges remained after RemoveNode(fnB)")
	}

	// RemoveFile removes all remaining nodes in a.go and their edges
	if n := g.RemoveFile(""); n != 0 {
		t.Fatalf("RemoveFile('') = %d, want 0", n)
	}
	removed := g.RemoveFile("a.go")
	if removed != 3 { // fileA, fnA, fnC
		t.Fatalf("RemoveFile('a.go') = %d, want 3", removed)
	}
	mustAssertConsistent(t, g)
	if g.NodeCount() != 1 || g.EdgeCount() != 0 { // only fileB remains
		t.Fatalf("after RemoveFile('a.go'), nodes=%d edges=%d, want 1, 0", g.NodeCount(), g.EdgeCount())
	}

	// RebuildIndexes preserves everything cleanly
	if err := g.AddNode(fnA); err != nil {
		t.Fatalf("AddNode failed: %v", err)
	}
	if err := g.AddEdge(&Edge{Source: fileB.UID, Target: fnA.UID, Kind: EdgeImports, Confidence: 1}); err != nil {
		t.Fatalf("AddEdge failed: %v", err)
	}
	g.RebuildIndexes()
	mustAssertConsistent(t, g)
}

func TestGraphVerifyIndexConsistencyDetectsCorruption(t *testing.T) {
	buildSample := func() (*Graph, *Node, *Node, *Edge) {
		g := New()
		n1 := &Node{Kind: KindFile, Name: "a.go", QName: "file::a.go", File: "a.go", StartLine: 1}
		n2 := &Node{Kind: KindFunction, Name: "Foo", QName: "pkg::Foo", File: "a.go", StartLine: 5}
		_ = g.AddNode(n1)
		_ = g.AddNode(n2)
		e1 := &Edge{Source: n1.UID, Target: n2.UID, Kind: EdgeContains, Confidence: 1}
		e2 := &Edge{Source: n2.UID, Target: n1.UID, Kind: EdgeImports, Confidence: 1}
		_ = g.AddEdge(e1)
		_ = g.AddEdge(e2)
		return g, n1, n2, e1
	}

	// Corrupt nodes map
	g, n1, n2, _ := buildSample()
	g.nodes[n1.UID] = nil
	if err := g.VerifyIndexConsistency(); err == nil {
		t.Fatal("expected error for nil node in g.nodes")
	}

	g, n1, _, _ = buildSample()
	wrongUID := GenerateUID("other", "x")
	g.nodes[wrongUID] = n1
	if err := g.VerifyIndexConsistency(); err == nil {
		t.Fatal("expected error for UID key mismatch")
	}

	// Corrupt byName
	g, _, _, _ = buildSample()
	g.byName["empty"] = []*Node{}
	if err := g.VerifyIndexConsistency(); err == nil {
		t.Fatal("expected error for empty byName slice")
	}

	g, n1, _, _ = buildSample()
	g.byName["wrong"] = []*Node{n1}
	if err := g.VerifyIndexConsistency(); err == nil {
		t.Fatal("expected error for byName key mismatch")
	}

	g, n1, _, _ = buildSample()
	g.byName[n1.Name] = append(g.byName[n1.Name], n1)
	if err := g.VerifyIndexConsistency(); err == nil {
		t.Fatal("expected error for duplicate in byName")
	}

	g, _, _, _ = buildSample()
	g.byName["ghost"] = []*Node{{UID: GenerateUID("ghost", "1"), Name: "ghost"}}
	if err := g.VerifyIndexConsistency(); err == nil {
		t.Fatal("expected error for unknown node in byName")
	}

	g, _, n2, _ = buildSample()
	delete(g.byFile, "a.go")
	if err := g.VerifyIndexConsistency(); err == nil {
		t.Fatal("expected error for missing byFile entries")
	}
	_ = n2

	g, _, _, _ = buildSample()
	delete(g.byQName, "pkg::Foo")
	if err := g.VerifyIndexConsistency(); err == nil {
		t.Fatal("expected error for missing byQName entries")
	}

	// Corrupt edges & edge indexes
	g, n1, n2, _ = buildSample()
	g.edges[edgeKey{source: n1.UID, target: n2.UID, kind: EdgeContains}] = nil
	if err := g.VerifyIndexConsistency(); err == nil {
		t.Fatal("expected error for nil edge")
	}

	g, n1, n2, _ = buildSample()
	g.edges[edgeKey{source: n2.UID, target: n1.UID, kind: EdgeCalls}] = &Edge{Source: n1.UID, Target: n2.UID, Kind: EdgeCalls}
	if err := g.VerifyIndexConsistency(); err == nil {
		t.Fatal("expected error for edgeKey mismatch")
	}

	g, _, n2, _ = buildSample()
	delete(g.nodes, n2.UID)
	if err := g.VerifyIndexConsistency(); err == nil {
		t.Fatal("expected error for dangling edge target")
	}

	g, n1, _, _ = buildSample()
	delete(g.nodes, n1.UID)
	if err := g.VerifyIndexConsistency(); err == nil {
		t.Fatal("expected error for dangling edge source")
	}

	// Corrupt outgoing/incoming/callers/callees
	g, n1, _, _ = buildSample()
	g.outgoing[n1.UID] = []*Edge{}
	if err := g.VerifyIndexConsistency(); err == nil {
		t.Fatal("expected error for empty outgoing slice")
	}

	g, n1, _, _ = buildSample()
	g.incoming[n1.UID] = append(g.incoming[n1.UID], g.incoming[n1.UID][0])
	if err := g.VerifyIndexConsistency(); err == nil {
		t.Fatal("expected error for duplicate incoming edge")
	}

	g, n1, n2, _ = buildSample()
	_ = g.AddEdge(&Edge{Source: n2.UID, Target: n2.UID, Kind: EdgeCalls, Confidence: 1})
	g.callers[n1.UID] = []*Edge{{Source: n2.UID, Target: n2.UID, Kind: EdgeCalls, Confidence: 1}}
	if err := g.VerifyIndexConsistency(); err == nil {
		t.Fatal("expected error for callers stale/key mismatch")
	}

	g, _, n2, _ = buildSample()
	_ = g.AddEdge(&Edge{Source: n2.UID, Target: n2.UID, Kind: EdgeCalls, Confidence: 1})
	delete(g.callees, n2.UID)
	if err := g.VerifyIndexConsistency(); err == nil {
		t.Fatal("expected error for callees count mismatch")
	}

	// Corrupt contains
	g, n1, _, _ = buildSample()
	g.contains[n1.UID] = []*Node{}
	if err := g.VerifyIndexConsistency(); err == nil {
		t.Fatal("expected error for empty contains slice")
	}

	g, n1, _, _ = buildSample()
	g.contains[n1.UID] = []*Node{nil}
	if err := g.VerifyIndexConsistency(); err == nil {
		t.Fatal("expected error for nil in contains")
	}

	g, n1, n2, _ = buildSample()
	g.contains[n1.UID] = []*Node{n2.Clone()}
	if err := g.VerifyIndexConsistency(); err == nil {
		t.Fatal("expected error for stale pointer in contains")
	}

	g, n1, n2, _ = buildSample()
	g.contains[n1.UID] = append(g.contains[n1.UID], g.nodes[n2.UID])
	if err := g.VerifyIndexConsistency(); err == nil {
		t.Fatal("expected error for duplicate in contains")
	}

	g, n1, n2, _ = buildSample()
	g.contains[n2.UID] = []*Node{g.nodes[n1.UID]}
	if err := g.VerifyIndexConsistency(); err == nil {
		t.Fatal("expected error for contains without CONTAINS edge")
	}

	g, n1, _, _ = buildSample()
	delete(g.contains, n1.UID)
	if err := g.VerifyIndexConsistency(); err == nil {
		t.Fatal("expected error for contains count mismatch")
	}

	// Corrupt revDeps
	g, n1, n2, _ = buildSample()
	delete(g.revDeps, n1.UID)
	if err := g.VerifyIndexConsistency(); err == nil {
		t.Fatal("expected error for revDeps key count mismatch")
	}

	g, n1, _, _ = buildSample()
	g.revDeps[n1.UID][n1.UID] = struct{}{}
	if err := g.VerifyIndexConsistency(); err == nil {
		t.Fatal("expected error for revDeps size mismatch")
	}

	g, n1, n2, _ = buildSample()
	delete(g.revDeps[n1.UID], n2.UID)
	g.revDeps[n1.UID][n1.UID] = struct{}{}
	if err := g.VerifyIndexConsistency(); err == nil {
		t.Fatal("expected error for revDeps source mismatch")
	}
}

func TestGraphConcurrency(t *testing.T) {
	g := New()
	const workers = 8
	const perWorker = 50

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			file := fmt.Sprintf("pkg/file_%d.go", id)
			var prev *Node
			for i := 0; i < perWorker; i++ {
				n := &Node{
					Kind:      KindFunction,
					Name:      fmt.Sprintf("Fn_%d_%d", id, i),
					QName:     fmt.Sprintf("pkg::Fn_%d_%d", id, i),
					File:      file,
					StartLine: uint32(i + 1),
				}
				if err := g.AddNode(n); err != nil {
					t.Errorf("AddNode failed: %v", err)
					return
				}
				if prev != nil {
					_ = g.UpsertEdge(&Edge{
						Source:     prev.UID,
						Target:     n.UID,
						Kind:       EdgeCalls,
						Confidence: 0.95,
						Tier:       2,
					})
				}
				prev = n
				_ = g.GetByName(n.Name)
				_ = g.GetByFile(file)
				_, _ = g.GetByQName(n.QName)
				_ = g.GetCallers(n.UID)
				_ = g.GetCallees(n.UID)
			}
			// Update one node and verify Reads/Writes don't race
			if prev != nil {
				cp := prev.Clone()
				cp.EndLine = cp.StartLine + 10
				_ = g.UpsertNode(cp)
			}
		}(w)
	}
	wg.Wait()

	mustAssertConsistent(t, g)
	if g.NodeCount() != workers*perWorker {
		t.Fatalf("NodeCount() = %d, want %d", g.NodeCount(), workers*perWorker)
	}

	// Concurrent removals & reads
	wg = sync.WaitGroup{}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			if id%2 == 0 {
				g.RemoveFile(fmt.Sprintf("pkg/file_%d.go", id))
			} else {
				_ = g.AllNodes()
				_ = g.AllEdges()
			}
		}(w)
	}
	wg.Wait()

	mustAssertConsistent(t, g)
	if g.NodeCount() != (workers/2)*perWorker {
		t.Fatalf("NodeCount() after concurrent RemoveFile = %d, want %d", g.NodeCount(), (workers/2)*perWorker)
	}
}

func TestGraphSortingTieBreakersAndEdgeCases(t *testing.T) {
	g := New()

	// Multiple nodes in same file & start line to exercise every tie-breaker in sortNodesByLocation
	n1 := &Node{Kind: KindFunction, Name: "Alpha", QName: "pkg::Alpha", File: "a.go", StartLine: 10, EndLine: 20}
	n2 := &Node{Kind: KindFunction, Name: "Alpha", QName: "pkg::Alpha2", File: "a.go", StartLine: 10, EndLine: 25}
	n3 := &Node{Kind: KindStruct, Name: "Alpha", QName: "pkg::StructAlpha", File: "a.go", StartLine: 10, EndLine: 25}
	n4 := &Node{Kind: KindFunction, Name: "Alpha", QName: "pkg::Alpha_overload", File: "a.go", StartLine: 10, EndLine: 25, Params: []Param{{Name: "x", Type: "int"}}}
	n5 := &Node{Kind: KindFunction, Name: "Beta", QName: "pkg::Beta", File: "a.go", StartLine: 10, EndLine: 25}
	// External node without File depending on n1
	ext := &Node{Kind: KindModule, Name: "ext", QName: "ext"}

	for _, n := range []*Node{n1, n2, n3, n4, n5, ext} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode failed: %v", err)
		}
	}

	byName := g.GetByName("Alpha")
	if len(byName) != 4 {
		t.Fatalf("len(GetByName(Alpha)) = %d, want 4", len(byName))
	}

	// Fileless dependent edge: ext -> n1 (IMPORTS)
	if err := g.AddEdge(&Edge{Source: ext.UID, Target: n1.UID, Kind: EdgeImports, Confidence: 1}); err != nil {
		t.Fatalf("AddEdge failed: %v", err)
	}
	if files := g.GetDependentFiles(n1.UID); files != nil {
		t.Fatalf("expected nil GetDependentFiles for fileless dependent, got %v", files)
	}

	// Empty incoming/outgoing edges return nil
	if edges := g.GetIncomingEdges(n5.UID, EdgeCalls); edges != nil {
		t.Fatalf("expected nil incoming edges, got %v", edges)
	}

	// Multiple callers with identical confidence -> exercise Tier, Line, Source tie-breakers in sortCallEdges
	_ = g.AddEdge(&Edge{Source: n2.UID, Target: n1.UID, Kind: EdgeCalls, Confidence: 0.90, Tier: 4, Line: 30})
	_ = g.AddEdge(&Edge{Source: n3.UID, Target: n1.UID, Kind: EdgeCalls, Confidence: 0.90, Tier: 3, Line: 50})
	_ = g.AddEdge(&Edge{Source: n4.UID, Target: n1.UID, Kind: EdgeCalls, Confidence: 0.90, Tier: 4, Line: 10})
	_ = g.AddEdge(&Edge{Source: n5.UID, Target: n1.UID, Kind: EdgeCalls, Confidence: 0.90, Tier: 4, Line: 10})

	callers := g.GetCallers(n1.UID)
	if len(callers) != 4 {
		t.Fatalf("len(GetCallers) = %d, want 4", len(callers))
	}
	// First caller must be tier 3 (n3)
	if callers[0].Source != n3.UID {
		t.Fatalf("expected tier-3 caller first, got tier %d", callers[0].Tier)
	}
	// Last caller must be line 30 (n2)
	if callers[3].Source != n2.UID {
		t.Fatalf("expected line-30 caller last, got line %d", callers[3].Line)
	}

	// VerifyIndexConsistency check for nil edge in edge index
	g.outgoing[n2.UID][0] = nil
	if err := g.VerifyIndexConsistency(); err == nil {
		t.Fatal("expected error for nil edge in outgoing index")
	}
	g.RebuildIndexes()
	mustAssertConsistent(t, g)
}

func containsUID(list []UID, target UID) bool {
	for _, u := range list {
		if u == target {
			return true
		}
	}
	return false
}

