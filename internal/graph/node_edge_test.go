package graph

import (
	"errors"
	"testing"
)

func TestNodeKindSentinelAndValidation(t *testing.T) {
	// Sentinel table completeness check
	if len(nodeKindNames) != int(numNodeKinds) {
		t.Fatalf("len(nodeKindNames) = %d, want %d", len(nodeKindNames), numNodeKinds)
	}
	for i, name := range nodeKindNames {
		if name == "" {
			t.Fatalf("nodeKindNames[%d] is empty", i)
		}
	}

	kinds := AllNodeKinds()
	if len(kinds) != int(numNodeKinds)-1 {
		t.Fatalf("len(AllNodeKinds()) = %d, want %d", len(kinds), int(numNodeKinds)-1)
	}

	for _, k := range kinds {
		if !k.IsValid() {
			t.Fatalf("kind %v should be valid", k)
		}
		txt, err := k.MarshalText()
		if err != nil {
			t.Fatalf("MarshalText(%v) failed: %v", k, err)
		}
		var unmarshaled NodeKind
		if err := unmarshaled.UnmarshalText(txt); err != nil {
			t.Fatalf("UnmarshalText(%q) failed: %v", txt, err)
		}
		if unmarshaled != k {
			t.Fatalf("round-trip NodeKind = %v, want %v", unmarshaled, k)
		}
	}

	if KindUnknown.IsValid() || numNodeKinds.IsValid() || NodeKind(99).IsValid() {
		t.Fatal("invalid NodeKind reported IsValid() == true")
	}
	if NodeKind(99).String() != "Unknown" {
		t.Fatalf("out-of-range NodeKind String() = %q, want Unknown", NodeKind(99).String())
	}
	if _, err := KindUnknown.MarshalText(); !errors.Is(err, ErrInvalidNode) {
		t.Fatalf("MarshalText(KindUnknown) error = %v, want ErrInvalidNode", err)
	}
	if parsed, err := ParseNodeKind("type_alias"); err != nil || parsed != KindTypeAlias {
		t.Fatalf("ParseNodeKind(type_alias) = (%v, %v), want (TypeAlias, nil)", parsed, err)
	}
	if parsed, err := ParseNodeKind(" external "); err != nil || parsed != KindExternal {
		t.Fatalf("ParseNodeKind(external) = (%v, %v), want (External, nil)", parsed, err)
	}
	var badKind NodeKind
	if err := badKind.UnmarshalText([]byte("NotAKind")); !errors.Is(err, ErrInvalidNode) {
		t.Fatalf("UnmarshalText(NotAKind) error = %v, want ErrInvalidNode", err)
	}

	invalidCases := []struct {
		name string
		node Node
	}{
		{name: "unknown kind", node: Node{Kind: KindUnknown, Name: "x", QName: "pkg::x"}},
		{name: "empty name", node: Node{Kind: KindFunction, Name: "   ", QName: "pkg::x"}},
		{name: "missing QName on function", node: Node{Kind: KindFunction, Name: "__init__", File: "app.py"}},
		{name: "missing QName on external", node: Node{Kind: KindExternal, Name: "Println"}},
		{name: "file missing both file and QName", node: Node{Kind: KindFile, Name: "main.go"}},
		{name: "end before start", node: Node{Kind: KindFunction, Name: "x", QName: "pkg::x", StartLine: 20, EndLine: 10}},
	}
	for _, tc := range invalidCases {
		if err := tc.node.Validate(); !errors.Is(err, ErrInvalidNode) {
			t.Errorf("%s: Validate() = %v, want ErrInvalidNode", tc.name, err)
		}
	}

	valid := Node{
		Kind:       KindFunction,
		Name:       "Run",
		QName:      "pkg::Run",
		File:       "run.go",
		StartLine:  10,
		EndLine:    0,
		Params:     []Param{{Name: "a", Type: "int"}},
		Decorators: []string{"@cached"},
		Bases:      []string{"Base"},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid.Validate() failed: %v", err)
	}

	cloned := valid.Clone()
	cloned.Params[0].Type = "mutated"
	cloned.Decorators[0] = "mutated"
	cloned.Bases[0] = "mutated"
	if valid.Params[0].Type != "int" || valid.Decorators[0] != "@cached" || valid.Bases[0] != "Base" {
		t.Fatal("Clone() did not deep copy slices")
	}

	// File node with File but empty QName is valid; KindExternal without File is valid.
	if err := (Node{Kind: KindFile, Name: "a.go", File: "a.go"}).Validate(); err != nil {
		t.Fatalf("file node with File only should be valid: %v", err)
	}
	if err := (Node{Kind: KindExternal, Name: "Println", QName: "fmt.Println"}).Validate(); err != nil {
		t.Fatalf("external node without File should be valid: %v", err)
	}
}

func TestEdgeKindResolutionTierAndValidation(t *testing.T) {
	// Sentinel table completeness checks
	if len(edgeKindNames) != int(numEdgeKinds) {
		t.Fatalf("len(edgeKindNames) = %d, want %d", len(edgeKindNames), numEdgeKinds)
	}
	for i, name := range edgeKindNames {
		if name == "" {
			t.Fatalf("edgeKindNames[%d] is empty", i)
		}
	}
	if len(resolutionTierTable) != int(numResolutionTiers) {
		t.Fatalf("len(resolutionTierTable) = %d, want %d", len(resolutionTierTable), numResolutionTiers)
	}
	for i, spec := range resolutionTierTable {
		if spec.name == "" || spec.label == "" {
			t.Fatalf("resolutionTierTable[%d] has empty name or label: %+v", i, spec)
		}
	}

	kinds := AllEdgeKinds()
	if len(kinds) != int(numEdgeKinds)-1 {
		t.Fatalf("len(AllEdgeKinds()) = %d, want %d", len(kinds), int(numEdgeKinds)-1)
	}

	for _, k := range kinds {
		if !k.IsValid() {
			t.Fatalf("edge kind %v should be valid", k)
		}
		txt, err := k.MarshalText()
		if err != nil {
			t.Fatalf("MarshalText(%v) failed: %v", k, err)
		}
		var decoded EdgeKind
		if err := decoded.UnmarshalText(txt); err != nil {
			t.Fatalf("UnmarshalText(%q) failed: %v", txt, err)
		}
		if decoded != k {
			t.Fatalf("round-trip EdgeKind = %v, want %v", decoded, k)
		}
	}

	if !EdgeCalls.IsCall() || !EdgeLikelyCalls.IsCall() || EdgeContains.IsCall() {
		t.Fatal("IsCall() mismatch")
	}
	if EdgeContains.IsDependency() || !EdgeCalls.IsDependency() || !EdgeImports.IsDependency() || EdgeUnknown.IsDependency() {
		t.Fatal("IsDependency() mismatch")
	}
	if EdgeUnknown.IsValid() || numEdgeKinds.IsValid() || EdgeKind(55).IsValid() {
		t.Fatal("invalid EdgeKind reported IsValid() == true")
	}
	if EdgeKind(55).String() != "UNKNOWN" {
		t.Fatalf("out-of-range EdgeKind String() = %q, want UNKNOWN", EdgeKind(55).String())
	}
	if _, err := EdgeUnknown.MarshalText(); !errors.Is(err, ErrInvalidEdge) {
		t.Fatalf("MarshalText(EdgeUnknown) = %v, want ErrInvalidEdge", err)
	}
	if k, err := ParseEdgeKind("likely_calls"); err != nil || k != EdgeLikelyCalls {
		t.Fatalf("ParseEdgeKind(likely_calls) = (%v, %v)", k, err)
	}
	var badEdge EdgeKind
	if err := badEdge.UnmarshalText([]byte("BOGUS")); !errors.Is(err, ErrInvalidEdge) {
		t.Fatalf("UnmarshalText(BOGUS) = %v, want ErrInvalidEdge", err)
	}

	// Tier-driven confidence & labels
	if !TierStructural.IsCertain() || !TierSamePackage.IsCertain() {
		t.Fatal("strong tiers must report IsCertain() == true")
	}
	if TierTypeNarrowed.IsCertain() || TierUnresolved.IsCertain() {
		t.Fatal("weak/unresolved tiers must report IsCertain() == false")
	}
	if TierTypeNarrowed.Label() != "likely" || TierTypeNarrowed.Confidence() != 0.60 {
		t.Fatalf("TierTypeNarrowed = (%v, %q), want (0.60, likely)", TierTypeNarrowed.Confidence(), TierTypeNarrowed.Label())
	}
	if TierUnresolved.Label() != "unresolved" || TierUnresolved.Confidence() != 0.00 {
		t.Fatalf("TierUnresolved = (%v, %q), want (0.00, unresolved)", TierUnresolved.Confidence(), TierUnresolved.Label())
	}
	if ResolutionTier(99).IsValid() || ResolutionTier(99).Label() != "unresolved" || ResolutionTier(99).String() != "unknown" {
		t.Fatal("out-of-range ResolutionTier mismatch")
	}

	u1 := GenerateNodeUID(KindFunction, "pkg::A", 0)
	u2 := GenerateNodeUID(KindFunction, "pkg::B", 0)

	invalidEdges := []struct {
		name string
		edge Edge
	}{
		{"unknown kind", Edge{Source: u1, Target: u2, Kind: EdgeUnknown, Tier: TierStructural}},
		{"zero source", Edge{Source: ZeroUID, Target: u2, Kind: EdgeCalls, Tier: TierLocal}},
		{"zero target", Edge{Source: u1, Target: ZeroUID, Kind: EdgeCalls, Tier: TierLocal}},
		{"invalid tier", Edge{Source: u1, Target: u2, Kind: EdgeCalls, Tier: ResolutionTier(99)}},
		{"CALLS with structural tier", Edge{Source: u1, Target: u2, Kind: EdgeCalls, Tier: TierStructural}},
		{"CALLS with type narrowed tier", Edge{Source: u1, Target: u2, Kind: EdgeCalls, Tier: TierTypeNarrowed}},
		{"CALLS with unresolved tier", Edge{Source: u1, Target: u2, Kind: EdgeCalls, Tier: TierUnresolved}},
		{"LIKELY_CALLS with strong tier", Edge{Source: u1, Target: u2, Kind: EdgeLikelyCalls, Tier: TierLocal}},
		{"CONTAINS with non-structural tier", Edge{Source: u1, Target: u2, Kind: EdgeContains, Tier: TierLocal}},
		{"IMPORTS with weak tier", Edge{Source: u1, Target: u2, Kind: EdgeImports, Tier: TierTypeNarrowed}},
		{"CONTAINS self-loop", Edge{Source: u1, Target: u1, Kind: EdgeContains, Tier: TierStructural}},
		{"IMPORTS self-loop", Edge{Source: u1, Target: u1, Kind: EdgeImports, Tier: TierStructural}},
		{"INHERITS self-loop", Edge{Source: u1, Target: u1, Kind: EdgeInherits, Tier: TierStructural}},
		{"IMPLEMENTS self-loop", Edge{Source: u1, Target: u1, Kind: EdgeImplements, Tier: TierStructural}},
		{"DECORATED_BY self-loop", Edge{Source: u1, Target: u1, Kind: EdgeDecoratedBy, Tier: TierStructural}},
		{"site line 0", Edge{Source: u1, Target: u2, Kind: EdgeCalls, Tier: TierLocal, Sites: []CallSiteRef{{Line: 0}}}},
		{"site tier mismatch", Edge{Source: u1, Target: u2, Kind: EdgeCalls, Tier: TierLocal, Sites: []CallSiteRef{{Line: 5, Tier: TierTypeNarrowed}}}},
	}
	for _, tc := range invalidEdges {
		if err := tc.edge.Validate(); !errors.Is(err, ErrInvalidEdge) {
			t.Errorf("%s: Validate() = %v, want ErrInvalidEdge", tc.name, err)
		}
	}

	// Recursive CALLS self-loop with multiple call sites is valid
	selfCall := Edge{
		Source: u1,
		Target: u1,
		Kind:   EdgeCalls,
		Tier:   TierLocal,
		Label:  "recursive",
		Sites: []CallSiteRef{
			{Line: 12, Column: 8},
			{Line: 18, Column: 10, Tier: TierLocal, Label: "tail"},
		},
	}
	if err := selfCall.Validate(); err != nil {
		t.Fatalf("self-loop CALLS should be valid, got: %v", err)
	}
	if selfCall.Confidence() != TierLocal.Confidence() || selfCall.ConfidenceLabel() != "certain" {
		t.Fatalf("unexpected derived confidence/label: (%v, %q)", selfCall.Confidence(), selfCall.ConfidenceLabel())
	}
	cp := selfCall.Clone()
	cp.Sites[0].Line = 999
	if selfCall.Sites[0].Line != 12 {
		t.Fatal("Edge.Clone() did not deep-copy Sites")
	}
}

func TestCallSiteValidationAndStatus(t *testing.T) {
	if len(callStatusNames) != int(numCallStatuses) {
		t.Fatalf("len(callStatusNames) = %d, want %d", len(callStatusNames), numCallStatuses)
	}
	for i, name := range callStatusNames {
		if name == "" {
			t.Fatalf("callStatusNames[%d] is empty", i)
		}
	}
	if CallStatusUnknown.IsValid() || numCallStatuses.IsValid() || CallStatus(99).String() != "unknown" {
		t.Fatal("invalid CallStatus check failed")
	}

	caller := GenerateNodeUID(KindFunction, "pkg::Caller", 0)
	cand1 := GenerateNodeUID(KindFunction, "a::Save", 0)
	cand2 := GenerateNodeUID(KindFunction, "b::Save", 0)

	invalid := []struct {
		name string
		cs   CallSite
	}{
		{"zero caller", CallSite{Status: CallStatusUnresolved, Line: 10, LookupKey: "Save", Tier: TierUnresolved}},
		{"invalid status", CallSite{Caller: caller, Status: CallStatusUnknown, Line: 10, LookupKey: "Save", Tier: TierUnresolved}},
		{"zero line", CallSite{Caller: caller, Status: CallStatusUnresolved, Line: 0, LookupKey: "Save", Tier: TierUnresolved}},
		{"empty lookup key", CallSite{Caller: caller, Status: CallStatusUnresolved, Line: 10, LookupKey: "  ", Tier: TierUnresolved}},
		{"invalid tier", CallSite{Caller: caller, Status: CallStatusUnresolved, Line: 10, LookupKey: "Save", Tier: ResolutionTier(99)}},
		{"ambiguous no candidates", CallSite{Caller: caller, Status: CallStatusAmbiguous, Line: 10, LookupKey: "Save", Tier: TierTypeNarrowed}},
		{"zero candidate UID", CallSite{Caller: caller, Status: CallStatusAmbiguous, Line: 10, LookupKey: "Save", Tier: TierTypeNarrowed, Candidates: []UID{cand1, ZeroUID}}},
	}
	for _, tc := range invalid {
		if err := tc.cs.Validate(); !errors.Is(err, ErrInvalidCallSite) {
			t.Errorf("%s: Validate() = %v, want ErrInvalidCallSite", tc.name, err)
		}
	}

	ambig := CallSite{
		Caller:     caller,
		File:       "main.go",
		Line:       42,
		Column:     12,
		CalleeExpr: "store.Save()",
		LookupKey:  "Save",
		Candidates: []UID{cand1, cand2},
		Status:     CallStatusAmbiguous,
		Tier:       TierTypeNarrowed,
		Reason:     "receiver type implements multiple Save interfaces",
	}
	if err := ambig.Validate(); err != nil {
		t.Fatalf("valid ambiguous CallSite failed validation: %v", err)
	}
	cloned := ambig.Clone()
	cloned.Candidates[0] = ZeroUID
	if ambig.Candidates[0] != cand1 {
		t.Fatal("CallSite.Clone() did not deep-copy Candidates")
	}
}
