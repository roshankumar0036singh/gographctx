package graph

import (
	"fmt"
	"math"
	"strings"
)

// EdgeKind identifies the relationship type between two nodes in the graph.
type EdgeKind uint8

const (
	EdgeUnknown EdgeKind = iota
	EdgeCalls
	EdgeLikelyCalls
	EdgeContains
	EdgeImports
	EdgeInherits
	EdgeImplements
	EdgeDecoratedBy
)

var edgeKindNames = [...]string{
	EdgeUnknown:     "UNKNOWN",
	EdgeCalls:       "CALLS",
	EdgeLikelyCalls: "LIKELY_CALLS",
	EdgeContains:    "CONTAINS",
	EdgeImports:     "IMPORTS",
	EdgeInherits:    "INHERITS",
	EdgeImplements:  "IMPLEMENTS",
	EdgeDecoratedBy: "DECORATED_BY",
}

// AllEdgeKinds returns all 7 valid EdgeKind values in canonical order.
func AllEdgeKinds() []EdgeKind {
	return []EdgeKind{
		EdgeCalls,
		EdgeLikelyCalls,
		EdgeContains,
		EdgeImports,
		EdgeInherits,
		EdgeImplements,
		EdgeDecoratedBy,
	}
}

// IsValid reports whether k is a valid EdgeKind.
func (k EdgeKind) IsValid() bool {
	return k >= EdgeCalls && k <= EdgeDecoratedBy
}

// IsCall reports whether k represents a function/method call relationship.
func (k EdgeKind) IsCall() bool {
	return k == EdgeCalls || k == EdgeLikelyCalls
}

// IsDependency reports whether k creates a cross-symbol dependency tracked for incremental updates.
func (k EdgeKind) IsDependency() bool {
	return k.IsValid() && k != EdgeContains
}

// String returns the canonical name of the EdgeKind.
func (k EdgeKind) String() string {
	if int(k) < len(edgeKindNames) {
		return edgeKindNames[k]
	}
	return "UNKNOWN"
}

// MarshalText implements encoding.TextMarshaler.
func (k EdgeKind) MarshalText() ([]byte, error) {
	if !k.IsValid() {
		return nil, fmt.Errorf("%w: invalid edge kind %d", ErrInvalidEdge, k)
	}
	return []byte(k.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (k *EdgeKind) UnmarshalText(text []byte) error {
	parsed, err := ParseEdgeKind(string(text))
	if err != nil {
		return err
	}
	*k = parsed
	return nil
}

// ParseEdgeKind converts a string into an EdgeKind (case-insensitive).
func ParseEdgeKind(s string) (EdgeKind, error) {
	trimmed := strings.TrimSpace(s)
	for kind := EdgeCalls; kind <= EdgeDecoratedBy; kind++ {
		if strings.EqualFold(edgeKindNames[kind], trimmed) {
			return kind, nil
		}
	}
	return EdgeUnknown, fmt.Errorf("%w: unknown edge kind %q", ErrInvalidEdge, s)
}

// Edge represents a directed relationship between Source and Target nodes.
// Edges stored in a Graph are immutable snapshots; callers must not mutate returned *Edge values.
type Edge struct {
	Source     UID      `json:"source"`
	Target     UID      `json:"target"`
	Kind       EdgeKind `json:"kind"`
	Confidence float32  `json:"confidence"`
	Tier       uint8    `json:"tier,omitempty"`
	Line       uint32   `json:"line,omitempty"`
	Label      string   `json:"label,omitempty"`
}

// Validate checks that the edge satisfies structural and range invariants.
func (e *Edge) Validate() error {
	if e == nil {
		return ErrNilEdge
	}
	if !e.Kind.IsValid() {
		return fmt.Errorf("%w: unknown edge kind %d", ErrInvalidEdge, e.Kind)
	}
	if e.Source.IsZero() {
		return fmt.Errorf("%w: empty source UID", ErrInvalidEdge)
	}
	if e.Target.IsZero() {
		return fmt.Errorf("%w: empty target UID", ErrInvalidEdge)
	}
	if e.Kind == EdgeContains && e.Source == e.Target {
		return fmt.Errorf("%w: self-containment edge is invalid", ErrInvalidEdge)
	}
	if math.IsNaN(float64(e.Confidence)) || e.Confidence < 0 || e.Confidence > 1 {
		return fmt.Errorf("%w: confidence %v must be within [0, 1]", ErrInvalidEdge, e.Confidence)
	}
	return nil
}

// Clone returns a copy of the Edge.
func (e *Edge) Clone() *Edge {
	if e == nil {
		return nil
	}
	cp := *e
	return &cp
}

// ConfidenceLabel maps the edge kind and confidence score to an LLM-facing label.
func (e *Edge) ConfidenceLabel() string {
	if e == nil {
		return "unresolved"
	}
	if e.Confidence == 0 {
		return "unresolved"
	}
	if e.Kind == EdgeLikelyCalls || e.Confidence < 0.85 {
		return "likely"
	}
	return "certain"
}

