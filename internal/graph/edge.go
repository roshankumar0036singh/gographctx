package graph

import (
	"fmt"
	"slices"
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
	numEdgeKinds
)

var edgeKindNames = [numEdgeKinds]string{
	EdgeUnknown:     "UNKNOWN",
	EdgeCalls:       "CALLS",
	EdgeLikelyCalls: "LIKELY_CALLS",
	EdgeContains:    "CONTAINS",
	EdgeImports:     "IMPORTS",
	EdgeInherits:    "INHERITS",
	EdgeImplements:  "IMPLEMENTS",
	EdgeDecoratedBy: "DECORATED_BY",
}

// AllEdgeKinds returns all defined valid EdgeKind values in canonical order.
func AllEdgeKinds() []EdgeKind {
	out := make([]EdgeKind, 0, int(numEdgeKinds)-1)
	for kind := EdgeCalls; kind < numEdgeKinds; kind++ {
		out = append(out, kind)
	}
	return out
}

// IsValid reports whether k is a valid EdgeKind.
func (k EdgeKind) IsValid() bool {
	return k >= EdgeCalls && k < numEdgeKinds
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
	if k < numEdgeKinds {
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
	for kind := EdgeCalls; kind < numEdgeKinds; kind++ {
		if strings.EqualFold(edgeKindNames[kind], trimmed) {
			return kind, nil
		}
	}
	return EdgeUnknown, fmt.Errorf("%w: unknown edge kind %q", ErrInvalidEdge, s)
}

// ResolutionTier is the single source of truth for how an edge or call site was resolved.
// Both numeric confidence scores and human/LLM confidence labels derive strictly from it.
type ResolutionTier uint8

const (
	TierStructural      ResolutionTier = iota // Direct syntactic containment/import/inheritance
	TierSelf                                  // Explicit self/this/super receiver (1.00, certain)
	TierLocal                                 // Local definition in same file (0.98, certain)
	TierQualifiedImport                       // Fully-qualified import, unique match (0.95, certain)
	TierReceiverType                          // Receiver-type inferred + method resolved (0.90, certain)
	TierModuleUnique                          // Module resolution, unique match (0.88, certain)
	TierSamePackage                           // Same-package, unique match (0.85, certain)
	TierTypeNarrowed                          // Multiple candidates, type-narrowed (0.60, likely)
	TierUnresolved                            // Unresolved — stored as CallSite, never emitted as an edge
	numResolutionTiers
)

type tierSpec struct {
	name       string
	confidence float32
	label      string
}

var resolutionTierTable = [numResolutionTiers]tierSpec{
	TierStructural:      {name: "structural", confidence: 1.00, label: "certain"},
	TierSelf:            {name: "self receiver", confidence: 1.00, label: "certain"},
	TierLocal:           {name: "local definition", confidence: 0.98, label: "certain"},
	TierQualifiedImport: {name: "qualified import", confidence: 0.95, label: "certain"},
	TierReceiverType:    {name: "receiver type", confidence: 0.90, label: "certain"},
	TierModuleUnique:    {name: "module resolution", confidence: 0.88, label: "certain"},
	TierSamePackage:     {name: "same package", confidence: 0.85, label: "certain"},
	TierTypeNarrowed:    {name: "type narrowed", confidence: 0.60, label: "likely"},
	TierUnresolved:      {name: "unresolved", confidence: 0.00, label: "unresolved"},
}

// IsValid reports whether t is a recognized resolution tier.
func (t ResolutionTier) IsValid() bool {
	return t < numResolutionTiers
}

// IsCertain reports whether t represents a certain (strong-tier or structural) resolution.
func (t ResolutionTier) IsCertain() bool {
	return t <= TierSamePackage
}

// String returns the canonical description of the resolution tier.
func (t ResolutionTier) String() string {
	if t < numResolutionTiers {
		return resolutionTierTable[t].name
	}
	return "unknown"
}

// Confidence returns the numeric confidence score derived from the tier.
func (t ResolutionTier) Confidence() float32 {
	if t < numResolutionTiers {
		return resolutionTierTable[t].confidence
	}
	return 0
}

// Label returns the LLM-facing confidence label ("certain", "likely", or "unresolved").
func (t ResolutionTier) Label() string {
	if t < numResolutionTiers {
		return resolutionTierTable[t].label
	}
	return "unresolved"
}

// CallSiteRef records exact source provenance for one occurrence of an edge
// (such as a specific call site inside a caller calling a target function).
type CallSiteRef struct {
	Line   uint32         `json:"line"`
	Column uint16         `json:"column,omitempty"`
	Tier   ResolutionTier `json:"tier,omitempty"`
	Label  string         `json:"label,omitempty"`
}

// Edge represents a directed relationship between Source and Target nodes.
// Tier is the single source of truth for Confidence() and ConfidenceLabel().
type Edge struct {
	Source UID            `json:"source"`
	Target UID            `json:"target"`
	Kind   EdgeKind       `json:"kind"`
	Tier   ResolutionTier `json:"tier"`
	Label  string         `json:"label,omitempty"`
	Sites  []CallSiteRef  `json:"sites,omitempty"`
}

// Confidence returns the numeric confidence derived from the edge's Tier.
func (e Edge) Confidence() float32 {
	return e.Tier.Confidence()
}

// ConfidenceLabel returns the confidence label derived from the edge's Tier.
func (e Edge) ConfidenceLabel() string {
	return e.Tier.Label()
}

// Validate checks that the edge satisfies structural, self-loop, and Kind/Tier cross-check invariants.
func (e Edge) Validate() error {
	if !e.Kind.IsValid() {
		return fmt.Errorf("%w: unknown edge kind %d", ErrInvalidEdge, e.Kind)
	}
	if e.Source.IsZero() {
		return fmt.Errorf("%w: empty source UID", ErrInvalidEdge)
	}
	if e.Target.IsZero() {
		return fmt.Errorf("%w: empty target UID", ErrInvalidEdge)
	}
	if e.Source == e.Target && !e.Kind.IsCall() {
		return fmt.Errorf("%w: self-loop is not allowed for %s edges", ErrInvalidEdge, e.Kind)
	}
	if err := validateTierForKind(e.Kind, e.Tier); err != nil {
		return err
	}
	for _, s := range e.Sites {
		if s.Line == 0 {
			return fmt.Errorf("%w: call site line must be > 0", ErrInvalidEdge)
		}
		siteTier := s.Tier
		if siteTier == 0 && e.Kind.IsCall() {
			siteTier = e.Tier
		}
		if err := validateTierForKind(e.Kind, siteTier); err != nil {
			return fmt.Errorf("invalid call site at line %d: %w", s.Line, err)
		}
	}
	return nil
}

func validateTierForKind(kind EdgeKind, tier ResolutionTier) error {
	if !tier.IsValid() {
		return fmt.Errorf("%w: unknown resolution tier %d", ErrInvalidEdge, tier)
	}
	switch kind {
	case EdgeCalls:
		if tier < TierSelf || tier > TierSamePackage {
			return fmt.Errorf("%w: %s requires tier in [%d, %d], got %d", ErrInvalidEdge, kind, TierSelf, TierSamePackage, tier)
		}
	case EdgeLikelyCalls:
		if tier != TierTypeNarrowed {
			return fmt.Errorf("%w: %s requires tier %d (%s), got %d", ErrInvalidEdge, kind, TierTypeNarrowed, TierTypeNarrowed, tier)
		}
	case EdgeContains:
		if tier != TierStructural {
			return fmt.Errorf("%w: %s requires structural tier %d, got %d", ErrInvalidEdge, kind, TierStructural, tier)
		}
	default:
		if tier > TierSamePackage {
			return fmt.Errorf("%w: %s cannot have weak/unresolved tier %d", ErrInvalidEdge, kind, tier)
		}
	}
	return nil
}

// Clone returns a deep copy of the Edge.
func (e Edge) Clone() Edge {
	cp := e
	cp.Sites = slices.Clone(e.Sites)
	return cp
}
