package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/roshankumar0036singh/gographctx/internal/graph"
	"github.com/roshankumar0036singh/gographctx/internal/mcp"
)

func buildMockHandlerGraph(t *testing.T) *graph.Graph {
	t.Helper()
	g := graph.New()

	fileCore := graph.Node{Kind: graph.KindFile, Name: "pkg/core.go", QName: "pkg/core.go", File: "pkg/core.go", Language: "go"}
	fileSvc := graph.Node{Kind: graph.KindFile, Name: "pkg/service.go", QName: "pkg/service.go", File: "pkg/service.go", Language: "go"}
	fileAPI := graph.Node{Kind: graph.KindFile, Name: "api/handler.py", QName: "api/handler.py", File: "api/handler.py", Language: "python"}
	modCore := graph.Node{Kind: graph.KindModule, Name: "pkg", QName: "mod:pkg/core.go", File: "pkg/core.go", Language: "go"}

	clsSvc := graph.Node{Kind: graph.KindStruct, Name: "OrderService", QName: "pkg::OrderService", File: "pkg/service.go", StartLine: 5, EndLine: 25, Language: "go", Exported: true}
	fnSave := graph.Node{Kind: graph.KindFunction, Name: "Save", QName: "pkg::Save", File: "pkg/core.go", StartLine: 10, EndLine: 20, Language: "go", Exported: true, Complexity: 4}
	fnProcess := graph.Node{Kind: graph.KindFunction, Name: "Process", QName: "pkg::OrderService::Process", File: "pkg/service.go", StartLine: 12, EndLine: 22, Language: "go", Receiver: "OrderService", Exported: true, Complexity: 8}
	fnRoute := graph.Node{Kind: graph.KindFunction, Name: "handle_order", QName: "api.handler::handle_order", File: "api/handler.py", StartLine: 4, EndLine: 14, Language: "python", Exported: true, Complexity: 12}
	fnPriv := graph.Node{Kind: graph.KindFunction, Name: "helper", QName: "pkg::helper", File: "pkg/core.go", StartLine: 25, EndLine: 30, Language: "go", Exported: false, Complexity: 2}

	for _, n := range []graph.Node{fileCore, fileSvc, fileAPI, modCore, clsSvc, fnSave, fnProcess, fnRoute, fnPriv} {
		if _, err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode(%s) failed: %v", n.QName, err)
		}
	}

	uSave := graph.ComputeNodeUID(fnSave)
	uProc := graph.ComputeNodeUID(fnProcess)
	uRoute := graph.ComputeNodeUID(fnRoute)
	uSvcFile := graph.ComputeNodeUID(fileSvc)
	uCoreFile := graph.ComputeNodeUID(fileCore)

	edges := []graph.Edge{
		{Source: uSvcFile, Target: uCoreFile, Kind: graph.EdgeImports, Tier: graph.TierQualifiedImport, Label: "demo/pkg/core"},
		{Source: uProc, Target: uSave, Kind: graph.EdgeCalls, Tier: graph.TierQualifiedImport, Sites: []graph.CallSiteRef{{Line: 16, Column: 8, Tier: graph.TierQualifiedImport}}},
		{Source: uRoute, Target: uProc, Kind: graph.EdgeLikelyCalls, Tier: graph.TierTypeNarrowed, Sites: []graph.CallSiteRef{{Line: 9, Column: 12, Tier: graph.TierTypeNarrowed}}},
	}
	for _, e := range edges {
		if err := g.AddEdge(e); err != nil {
			t.Fatalf("AddEdge(%v) failed: %v", e, err)
		}
	}
	return g
}

func TestAllSevenMCPToolHandlers(t *testing.T) {
	g := buildMockHandlerGraph(t)
	h := mcp.NewHandler(g, mcp.FormatterConfig{
		RepoRoot:       "C:/repo/demo",
		RepoName:       "demo",
		FixedTimestamp: "2026-11-28T00:00:00Z",
	})
	ctx := context.Background()

	execTool := func(name, argsJSON string) mcp.FormattedResponse {
		t.Helper()
		raw, err := h.HandleCall(ctx, name, json.RawMessage(argsJSON))
		if err != nil {
			t.Fatalf("HandleCall(%q, %s) failed: %v", name, argsJSON, err)
		}
		var env mcp.FormattedResponse
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("unmarshal %s output: %v\n%s", name, err, raw)
		}
		return env
	}

	// 1. find_symbol
	rFind := execTool("find_symbol", `{"name":"Save","kind":"Function","language":"go"}`)
	if rFind.Returned != 1 || rFind.Total != 1 {
		t.Fatalf("find_symbol unexpected counts: %+v", rFind)
	}

	// 2. get_callers
	rCallers := execTool("get_callers", `{"symbol":"pkg::Save"}`)
	if rCallers.Returned != 1 || rCallers.ConfidenceSummary.Certain != 1 {
		t.Fatalf("get_callers unexpected: %+v", rCallers)
	}
	// Exclude Tier 7 LIKELY_CALLS on pkg::OrderService::Process
	rCallersStrict := execTool("get_callers", `{"symbol":"pkg::OrderService::Process","include_likely":false}`)
	if rCallersStrict.Returned != 0 {
		t.Fatalf("expected 0 callers when include_likely=false, got %+v", rCallersStrict)
	}

	// 3. get_callees
	rCallees := execTool("get_callees", `{"symbol":"Process","file":"pkg/service.go"}`)
	if rCallees.Returned != 1 || !strings.Contains(rCallees.Summary, "callees of pkg::OrderService::Process") {
		t.Fatalf("get_callees unexpected: %+v", rCallees)
	}

	// 4. get_impact (transitive: Process (depth 1, certain) + handle_order (depth 2, likely))
	rImpact := execTool("get_impact", `{"symbol":"pkg::Save","max_depth":3}`)
	if rImpact.Returned != 2 || rImpact.ConfidenceSummary.Certain != 1 || rImpact.ConfidenceSummary.Likely != 1 {
		t.Fatalf("get_impact unexpected: %+v", rImpact)
	}

	// 5. file_outline (with include_private=false filtering out helper)
	rOutline := execTool("file_outline", `{"file":"pkg/core.go","include_private":false}`)
	if rOutline.Returned != 1 {
		t.Fatalf("file_outline(include_private=false) expected 1 symbol (Save), got %+v", rOutline)
	}

	// 6. search_code (complexity + language filters)
	rSearch := execTool("search_code", `{"min_complexity":10,"languages":["python"]}`)
	if rSearch.Returned != 1 || rSearch.Total != 1 {
		t.Fatalf("search_code unexpected: %+v", rSearch)
	}

	// 7. repo_overview
	rOver := execTool("repo_overview", `{"include_top_complex":true,"limit":5}`)
	dataBytes, _ := json.Marshal(rOver.Data)
	if !strings.Contains(string(dataBytes), `"complexity_distribution"`) || !strings.Contains(string(dataBytes), `"nodes_by_language"`) {
		t.Fatalf("repo_overview missing expected stats: %s", dataBytes)
	}

	// Unknown tool error & invalid arg validation
	if _, err := h.HandleCall(ctx, "does_not_exist", nil); !errors.Is(err, mcp.ErrUnknownTool) {
		t.Fatalf("expected ErrUnknownTool, got %v", err)
	}
	if _, err := h.HandleCall(ctx, "get_callers", json.RawMessage(`{"symbol":""}`)); !errors.Is(err, mcp.ErrInvalidArgs) {
		t.Fatalf("expected ErrInvalidArgs for empty symbol, got %v", err)
	}
}

func TestGetCallersAndImpactSurfaceUnresolvedHints(t *testing.T) {
	g := buildMockHandlerGraph(t)
	var privUID, routeUID graph.UID
	g.View(func(v *graph.ReadView) {
		privUID = v.GetByQName("pkg::helper")[0].UID
		routeUID = v.GetByQName("api.handler::handle_order")[0].UID
	})
	if err := g.AddCallSite(graph.CallSite{
		Caller:     routeUID,
		File:       "api/handler.py",
		Line:       11,
		Column:     8,
		CalleeExpr: "dyn.helper",
		LookupKey:  "api.handler::helper",
		Status:     graph.CallStatusUnresolved,
		Tier:       graph.TierUnresolved,
		Reason:     "dynamic receiver type unknown",
	}); err != nil {
		t.Fatalf("AddCallSite(unresolved) failed: %v", err)
	}
	if err := g.AddCallSite(graph.CallSite{
		Caller:     routeUID,
		File:       "api/handler.py",
		Line:       12,
		Column:     4,
		CalleeExpr: "print",
		LookupKey:  "builtins::print",
		Status:     graph.CallStatusExternal,
		Tier:       graph.TierUnresolved,
		Reason:     "external builtin",
	}); err != nil {
		t.Fatalf("AddCallSite(external) failed: %v", err)
	}

	h := mcp.NewHandler(g, mcp.FormatterConfig{
		RepoRoot:       "C:/repo/demo",
		RepoName:       "demo",
		FixedTimestamp: "2026-11-28T00:00:00Z",
	})
	_ = privUID

	rawCallers, err := h.HandleCall(context.Background(), "get_callers", json.RawMessage(`{"symbol":"pkg::helper"}`))
	if err != nil {
		t.Fatalf("get_callers failed: %v", err)
	}
	if !strings.Contains(string(rawCallers), `"unresolved_hints"`) || !strings.Contains(string(rawCallers), "1 unresolved call site(s) might match") {
		t.Fatalf("expected unresolved_hints in get_callers output, got: %s", rawCallers)
	}

	rawImpact, err := h.HandleCall(context.Background(), "get_impact", json.RawMessage(`{"symbol":"pkg::helper"}`))
	if err != nil {
		t.Fatalf("get_impact failed: %v", err)
	}
	if !strings.Contains(string(rawImpact), `"unresolved_hints"`) || !strings.Contains(string(rawImpact), `"dyn.helper"`) {
		t.Fatalf("expected unresolved_hints in get_impact output, got: %s", rawImpact)
	}
}
