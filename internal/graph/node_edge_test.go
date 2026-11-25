package graph

import (
	"errors"
	"math"
	"testing"
)

func TestNodeKindAndValidation(t *testing.T) {
	kinds := AllNodeKinds()
	if len(kinds) != 12 {
		t.Fatalf("len(AllNodeKinds()) = %d, want 12", len(kinds))
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

	if KindUnknown.IsValid() || NodeKind(99).IsValid() {
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
	if parsed, err := ParseNodeKind("  function "); err != nil || parsed != KindFunction {
		t.Fatalf("ParseNodeKind(function) = (%v, %v), want (Function, nil)", parsed, err)
	}
	var badKind NodeKind
	if err := badKind.UnmarshalText([]byte("NotAKind")); !errors.Is(err, ErrInvalidNode) {
		t.Fatalf("UnmarshalText(NotAKind) error = %v, want ErrInvalidNode", err)
	}

	// Node validation cases
	var nilNode *Node
	if err := nilNode.Validate(); !errors.Is(err, ErrNilNode) {
		t.Fatalf("nilNode.Validate() = %v, want ErrNilNode", err)
	}
	if nilNode.Clone() != nil {
		t.Fatal("nilNode.Clone() should return nil")
	}

	invalidCases := []struct {
		name string
		node *Node
	}{
		{name: "unknown kind", node: &Node{Kind: KindUnknown, Name: "x"}},
		{name: "empty name", node: &Node{Kind: KindFunction, Name: "   "}},
		{name: "end before start", node: &Node{Kind: KindFunction, Name: "x", StartLine: 20, EndLine: 10}},
	}
	for _, tc := range invalidCases {
		if err := tc.node.Validate(); !errors.Is(err, ErrInvalidNode) {
			t.Errorf("%s: Validate() = %v, want ErrInvalidNode", tc.name, err)
		}
	}

	valid := &Node{
		Kind:       KindFunction,
		Name:       "Run",
		QName:      "pkg::Run",
		File:       "run.go",
		StartLine:  10,
		EndLine:    0, // omitted EndLine allowed
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
}

func TestEdgeKindAndValidation(t *testing.T) {
	kinds := AllEdgeKinds()
	if len(kinds) != 7 {
		t.Fatalf("len(AllEdgeKinds()) = %d, want 7", len(kinds))
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
	if EdgeUnknown.IsValid() || EdgeKind(55).IsValid() {
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

	var nilEdge *Edge
	if err := nilEdge.Validate(); !errors.Is(err, ErrNilEdge) {
		t.Fatalf("nilEdge.Validate() = %v, want ErrNilEdge", err)
	}
	if nilEdge.Clone() != nil {
		t.Fatal("nilEdge.Clone() should return nil")
	}
	if nilEdge.ConfidenceLabel() != "unresolved" {
		t.Fatalf("nilEdge.ConfidenceLabel() = %q, want unresolved", nilEdge.ConfidenceLabel())
	}

	u1 := GenerateUID("a", "1")
	u2 := GenerateUID("b", "1")

	invalidEdges := []struct {
		name string
		edge *Edge
	}{
		{"unknown kind", &Edge{Source: u1, Target: u2, Kind: EdgeUnknown, Confidence: 1}},
		{"zero source", &Edge{Source: ZeroUID, Target: u2, Kind: EdgeCalls, Confidence: 1}},
		{"zero target", &Edge{Source: u1, Target: ZeroUID, Kind: EdgeCalls, Confidence: 1}},
		{"self containment", &Edge{Source: u1, Target: u1, Kind: EdgeContains, Confidence: 1}},
		{"negative confidence", &Edge{Source: u1, Target: u2, Kind: EdgeCalls, Confidence: -0.1}},
		{"confidence > 1", &Edge{Source: u1, Target: u2, Kind: EdgeCalls, Confidence: 1.1}},
		{"NaN confidence", &Edge{Source: u1, Target: u2, Kind: EdgeCalls, Confidence: float32(math.NaN())}},
	}
	for _, tc := range invalidEdges {
		if err := tc.edge.Validate(); !errors.Is(err, ErrInvalidEdge) {
			t.Errorf("%s: Validate() = %v, want ErrInvalidEdge", tc.name, err)
		}
	}

	// Self-loop CALLS is valid (recursion)
	selfCall := &Edge{Source: u1, Target: u1, Kind: EdgeCalls, Confidence: 1.0, Tier: 2, Line: 12, Label: "recursive"}
	if err := selfCall.Validate(); err != nil {
		t.Fatalf("self-loop CALLS should be valid, got: %v", err)
	}
	if cp := selfCall.Clone(); *cp != *selfCall {
		t.Fatal("Edge.Clone() mismatch")
	}

	if lbl := (&Edge{Kind: EdgeCalls, Confidence: 0.90}).ConfidenceLabel(); lbl != "certain" {
		t.Fatalf("got %q, want certain", lbl)
	}
	if lbl := (&Edge{Kind: EdgeLikelyCalls, Confidence: 0.90}).ConfidenceLabel(); lbl != "likely" {
		t.Fatalf("LIKELY_CALLS got %q, want likely", lbl)
	}
	if lbl := (&Edge{Kind: EdgeCalls, Confidence: 0.70}).ConfidenceLabel(); lbl != "likely" {
		t.Fatalf("low confidence CALLS got %q, want likely", lbl)
	}
	if lbl := (&Edge{Kind: EdgeCalls, Confidence: 0}).ConfidenceLabel(); lbl != "unresolved" {
		t.Fatalf("zero confidence got %q, want unresolved", lbl)
	}
}

