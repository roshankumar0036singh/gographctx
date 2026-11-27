package resolver

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/roshankumar0036singh/gographctx/internal/graph"
	"github.com/roshankumar0036singh/gographctx/internal/parser"
)

func TestTierScoringLabelsAndEdgeClassification(t *testing.T) {
	cases := []struct {
		tier        graph.ResolutionTier
		wantConf    float32
		wantLabel   string
		wantKind    graph.EdgeKind
		wantEmit    bool
		wantCertain bool
	}{
		{TierStructural, 1.00, "certain", graph.EdgeUnknown, false, true},
		{TierSelf, 1.00, "certain", graph.EdgeCalls, true, true},
		{TierLocal, 0.98, "certain", graph.EdgeCalls, true, true},
		{TierQualifiedImport, 0.95, "certain", graph.EdgeCalls, true, true},
		{TierReceiverType, 0.90, "certain", graph.EdgeCalls, true, true},
		{TierModuleUnique, 0.88, "certain", graph.EdgeCalls, true, true},
		{TierSamePackage, 0.85, "certain", graph.EdgeCalls, true, true},
		{TierTypeNarrowed, 0.60, "likely", graph.EdgeLikelyCalls, true, false},
		{TierUnresolved, 0.00, "unresolved", graph.EdgeUnknown, false, false},
	}

	for _, tc := range cases {
		if got := ConfidenceForTier(tc.tier); got != tc.wantConf {
			t.Errorf("tier %d Confidence = %.2f, want %.2f", tc.tier, got, tc.wantConf)
		}
		if got := LabelForTier(tc.tier); got != tc.wantLabel {
			t.Errorf("tier %d Label = %q, want %q", tc.tier, got, tc.wantLabel)
		}
		if got := EdgeKindForTier(tc.tier); got != tc.wantKind {
			t.Errorf("tier %d EdgeKind = %v, want %v", tc.tier, got, tc.wantKind)
		}
		if got := ShouldEmitCallEdge(tc.tier); got != tc.wantEmit {
			t.Errorf("tier %d ShouldEmitCallEdge = %v, want %v", tc.tier, got, tc.wantEmit)
		}
		if got := IsCertainCall(tc.tier); got != (tc.tier >= TierSelf && tc.tier <= TierSamePackage) {
			t.Errorf("tier %d IsCertainCall = %v", tc.tier, got)
		}
		if TierDescription(tc.tier) == "" || TierDescription(tc.tier) == "Unknown resolution tier" {
			t.Errorf("tier %d missing description", tc.tier)
		}
	}

	if TierDescription(graph.ResolutionTier(99)) != "Unknown resolution tier" {
		t.Fatal("expected unknown tier description fallback")
	}

	// Enforce Tiers 7-8 never emit CALLS edges.
	if err := ValidateCallTierAndKind(TierTypeNarrowed, graph.EdgeCalls); !errors.Is(err, ErrInvalidResolution) {
		t.Fatalf("TierTypeNarrowed + EdgeCalls err = %v, want ErrInvalidResolution", err)
	}
	if err := ValidateCallTierAndKind(TierUnresolved, graph.EdgeCalls); !errors.Is(err, ErrInvalidResolution) {
		t.Fatalf("TierUnresolved + EdgeCalls err = %v, want ErrInvalidResolution", err)
	}
	if err := ValidateCallTierAndKind(TierUnresolved, graph.EdgeLikelyCalls); !errors.Is(err, ErrInvalidResolution) {
		t.Fatalf("TierUnresolved + EdgeLikelyCalls err = %v, want ErrInvalidResolution", err)
	}
	if err := ValidateCallTierAndKind(graph.ResolutionTier(99), graph.EdgeCalls); !errors.Is(err, ErrInvalidResolution) {
		t.Fatalf("invalid tier err = %v, want ErrInvalidResolution", err)
	}
	if err := ValidateCallTierAndKind(TierTypeNarrowed, graph.EdgeLikelyCalls); err != nil {
		t.Fatalf("TierTypeNarrowed + EdgeLikelyCalls err = %v", err)
	}
}

func TestResolvedAndUnresolvedCallValidation(t *testing.T) {
	caller := graph.GenerateNodeUID(graph.KindFunction, "pkg::Caller", 0)
	target := graph.GenerateNodeUID(graph.KindFunction, "pkg::Target", 0)

	rc := ResolvedCall{
		CallerUID:   caller,
		CallerQName: "pkg::Caller",
		TargetUID:   target,
		TargetQName: "pkg::Target",
		Tier:        TierReceiverType,
		EdgeKind:    graph.EdgeCalls,
		Line:        42,
		Column:      9,
		Method:      "receiver-type inferred: Client.Fetch",
	}
	if rc.Confidence() != 0.90 || rc.ConfidenceLabel() != "certain" {
		t.Fatalf("confidence mismatch: %.2f %q", rc.Confidence(), rc.ConfidenceLabel())
	}
	edge, err := rc.ToEdge()
	if err != nil {
		t.Fatalf("ToEdge failed: %v", err)
	}
	if edge.Kind != graph.EdgeCalls || edge.Tier != TierReceiverType || len(edge.Sites) != 1 || edge.Sites[0].Line != 42 {
		t.Fatalf("edge conversion mismatch: %+v", edge)
	}

	// Reject emitting CALLS with TierTypeNarrowed or TierUnresolved.
	bad := rc
	bad.Tier = TierTypeNarrowed
	if _, err := bad.ToEdge(); !errors.Is(err, ErrInvalidResolution) {
		t.Fatalf("bad TierTypeNarrowed+EdgeCalls ToEdge = %v, want ErrInvalidResolution", err)
	}
	bad.Tier = TierUnresolved
	bad.EdgeKind = graph.EdgeUnknown
	if _, err := bad.ToEdge(); !errors.Is(err, ErrInvalidResolution) {
		t.Fatalf("bad TierUnresolved ToEdge = %v, want ErrInvalidResolution", err)
	}

	// Validate UnresolvedCall -> graph.CallSite conversion.
	uc := UnresolvedCall{
		CallerUID:  caller,
		File:       "pkg/caller.go",
		Line:       50,
		CalleeExpr: "dyn.Execute()",
		LookupKey:  "Client::Execute",
		Candidates: []graph.UID{target},
		Status:     graph.CallStatusAmbiguous,
		Tier:       TierTypeNarrowed,
		Reason:     "2 matching overloads",
	}
	if err := uc.Validate(); err != nil {
		t.Fatalf("UnresolvedCall.Validate failed: %v", err)
	}
	cs := uc.ToCallSite()
	if err := cs.Validate(); err != nil || cs.Status != graph.CallStatusAmbiguous || len(cs.Candidates) != 1 {
		t.Fatalf("ToCallSite mismatch: cs=%+v err=%v", cs, err)
	}

	ucUnresolved := UnresolvedCall{
		CallerUID:  caller,
		File:       "pkg/caller.go",
		Line:       51,
		CalleeExpr: "ext.Ping()",
		LookupKey:  "ext::Ping",
		Status:     graph.CallStatusUnresolved,
		Tier:       TierUnresolved,
		Reason:     "external symbol not indexed",
	}
	if err := ucUnresolved.Validate(); err != nil {
		t.Fatalf("UnresolvedCall(TierUnresolved).Validate failed: %v", err)
	}

	ucBad := uc
	ucBad.Tier = TierLocal
	if err := ucBad.Validate(); !errors.Is(err, ErrInvalidResolution) {
		t.Fatalf("UnresolvedCall with TierLocal = %v, want ErrInvalidResolution", err)
	}
}

type stubLangResolver struct {
	lang string
}

func (s *stubLangResolver) Language() string { return s.lang }
func (s *stubLangResolver) ResolveImports(_ context.Context, file *parser.ParsedFile, _ *ResolutionContext) ([]ResolvedImport, error) {
	return []ResolvedImport{{SourceFile: file.Path, ImportPath: "fmt"}}, nil
}
func (s *stubLangResolver) InferTypes(_ context.Context, _ *parser.ParsedFile, _ *ResolutionContext) (map[string]TypeInfo, error) {
	return map[string]TypeInfo{"c": {Name: "Client", Origin: OriginConstructor, Confidence: 0.9}}, nil
}
func (s *stubLangResolver) ResolveCalls(_ context.Context, _ *parser.ParsedFile, _ *ResolutionContext, _ []ResolvedImport, _ map[string]TypeInfo) ([]ResolvedCall, []UnresolvedCall, error) {
	return nil, nil, nil
}

func TestRegistryCommonHelpersAndResolutionContext(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(nil); !errors.Is(err, ErrInvalidResolver) {
		t.Fatalf("Register(nil) = %v, want ErrInvalidResolver", err)
	}
	if err := reg.Register(&stubLangResolver{lang: "  "}); !errors.Is(err, ErrInvalidResolver) {
		t.Fatalf("Register empty lang = %v, want ErrInvalidResolver", err)
	}
	reg.MustRegister(&stubLangResolver{lang: "Golang"})
	if err := reg.Register(&stubLangResolver{lang: "go"}); !errors.Is(err, ErrDuplicateResolver) {
		t.Fatalf("Register duplicate lang = %v, want ErrDuplicateResolver", err)
	}

	if !reg.Supports("go") || reg.Supports("python") {
		t.Fatal("Supports mismatch")
	}
	if !slices.Equal(reg.Languages(), []string{"go"}) {
		t.Fatalf("Languages() = %v", reg.Languages())
	}

	pf := &parser.ParsedFile{Path: "main.go", Language: "go"}
	res, err := reg.ResolveFile(context.Background(), pf, nil)
	if err != nil {
		t.Fatalf("ResolveFile(nil rc) failed: %v", err)
	}
	if len(res.Imports) != 1 || res.Types["c"].Name != "Client" {
		t.Fatalf("ResolveFile output mismatch: %+v", res)
	}

	if _, err := reg.ResolveFile(context.Background(), &parser.ParsedFile{Path: "a.py", Language: "python"}, nil); !errors.Is(err, ErrUnsupportedLanguage) {
		t.Fatalf("ResolveFile unsupported = %v, want ErrUnsupportedLanguage", err)
	}

	// Test common resolution helpers.
	if got := BuildLookupKey("Client", "orders", "Fetch"); got != "orders::Client::Fetch" {
		t.Fatalf("BuildLookupKey = %q", got)
	}
	if got := BuildLookupKey("", "", "Run"); got != "Run" {
		t.Fatalf("BuildLookupKey short = %q", got)
	}
	if !IsSelfOrSuperReceiver("this") || !IsSelfOrSuperReceiver("&mut self") || IsSelfOrSuperReceiver("client") {
		t.Fatal("IsSelfOrSuperReceiver mismatch")
	}
	if got := StripTypeDecorations("*pkg.Service[T]"); got != "pkg.Service" {
		t.Fatalf("StripTypeDecorations(*pkg.Service[T]) = %q", got)
	}
	if got := StripTypeDecorations("(Order | null)"); got != "Order" {
		t.Fatalf("StripTypeDecorations((Order | null)) = %q", got)
	}
	if got := StripTypeDecorations("(*pkg.Service[T])"); got != "pkg.Service" {
		t.Fatalf("StripTypeDecorations((*pkg.Service[T])) = %q", got)
	}
	if got := StripTypeDecorations("&mut Client<'a, T>"); got != "Client" {
		t.Fatalf("StripTypeDecorations(&mut Client<'a, T>) = %q", got)
	}

	n1 := graph.Node{UID: graph.GenerateNodeUID(graph.KindFunction, "a::F", 0), Kind: graph.KindFunction, Name: "F"}
	n2 := graph.Node{UID: graph.GenerateNodeUID(graph.KindFunction, "b::F", 0), Kind: graph.KindFunction, Name: "F"}
	_, okA, candsA := MatchUniqueNode([]graph.Node{n1, n2}, nil)
	_, okB, candsB := MatchUniqueNode([]graph.Node{n2, n1, n1}, nil)
	if okA || okB || len(candsA) != 2 || !slices.Equal(candsA, candsB) {
		t.Fatalf("MatchUniqueNode deterministic ordering/dedup mismatch: A=%v B=%v", candsA, candsB)
	}
	if u, ok, _ := MatchUniqueNode([]graph.Node{n1, n2}, func(n graph.Node) bool { return n.UID == n1.UID }); !ok || u.UID != n1.UID {
		t.Fatalf("MatchUniqueNode filtered mismatch: u=%+v ok=%v", u, ok)
	}
	if OriginAssignment.String() != "assignment" || OriginConstructor.String() != "constructor" || OriginUnknown.String() != "unknown" {
		t.Fatal("TypeOrigin.String mismatch")
	}
}
