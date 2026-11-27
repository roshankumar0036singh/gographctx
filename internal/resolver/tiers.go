package resolver

import (
	"errors"
	"fmt"
	"strings"

	"github.com/roshankumar0036singh/gographctx/internal/graph"
)

var (
	// ErrInvalidResolution indicates a ResolvedCall or UnresolvedCall violates tier/edge invariants.
	ErrInvalidResolution = errors.New("resolver: invalid resolution record")
	// ErrInvalidResolver indicates a nil or malformed LanguageResolver registration.
	ErrInvalidResolver = errors.New("resolver: invalid language resolver")
	// ErrDuplicateResolver indicates a language already has a registered LanguageResolver.
	ErrDuplicateResolver = errors.New("resolver: duplicate language resolver")
	// ErrUnsupportedLanguage indicates no LanguageResolver is registered for a language.
	ErrUnsupportedLanguage = errors.New("resolver: unsupported language")
)

// Re-export graph.ResolutionTier constants so callers can reference resolver.Tier*
// with zero type conversion overhead while keeping graph.ResolutionTier as the
// single source of truth.
const (
	TierStructural      = graph.TierStructural
	TierSelf            = graph.TierSelf
	TierLocal           = graph.TierLocal
	TierQualifiedImport = graph.TierQualifiedImport
	TierReceiverType    = graph.TierReceiverType
	TierModuleUnique    = graph.TierModuleUnique
	TierSamePackage     = graph.TierSamePackage
	TierTypeNarrowed    = graph.TierTypeNarrowed
	TierUnresolved      = graph.TierUnresolved
)

var tierDescriptions = map[graph.ResolutionTier]string{
	TierStructural:      "Direct syntactic containment, import, or inheritance link",
	TierSelf:            "Explicit self/this/super receiver method call",
	TierLocal:           "Local symbol defined in the same file",
	TierQualifiedImport: "Fully-qualified import with a unique matching target",
	TierReceiverType:    "Receiver type inferred and method uniquely resolved",
	TierModuleUnique:    "Cross-file module resolution with a unique target",
	TierSamePackage:     "Same-package symbol with a unique matching target",
	TierTypeNarrowed:    "Multiple candidate targets narrowed by partial type info",
	TierUnresolved:      "Unresolved or external call recorded only as CallSite",
}

// ConfidenceForTier returns the canonical numeric confidence score in [0.0, 1.0].
func ConfidenceForTier(tier graph.ResolutionTier) float32 {
	return tier.Confidence()
}

// LabelForTier returns "certain", "likely", or "unresolved".
func LabelForTier(tier graph.ResolutionTier) string {
	return tier.Label()
}

// TierDescription returns the detailed human/LLM explanation for tier.
func TierDescription(tier graph.ResolutionTier) string {
	if desc, ok := tierDescriptions[tier]; ok {
		return desc
	}
	return "Unknown resolution tier"
}

// EdgeKindForTier maps a call-resolution tier to the EdgeKind to emit.
// Tiers 1-6 emit CALLS, Tier 7 emits LIKELY_CALLS, and Tier 8 never emits an edge.
func EdgeKindForTier(tier graph.ResolutionTier) graph.EdgeKind {
	switch {
	case tier >= TierSelf && tier <= TierSamePackage:
		return graph.EdgeCalls
	case tier == TierTypeNarrowed:
		return graph.EdgeLikelyCalls
	default:
		return graph.EdgeUnknown
	}
}

// ShouldEmitCallEdge reports whether calls at tier produce a graph Edge.
func ShouldEmitCallEdge(tier graph.ResolutionTier) bool {
	return tier >= TierSelf && tier <= TierTypeNarrowed
}

// IsCertainCall reports whether tier satisfies strict CALLS certainty (Tiers 1-6).
func IsCertainCall(tier graph.ResolutionTier) bool {
	return tier >= TierSelf && tier <= TierSamePackage
}

// ValidateCallTierAndKind verifies that tier and edgeKind obey the system's
// resolution-tier discipline (notably Tiers 7-8 never emit CALLS edges).
func ValidateCallTierAndKind(tier graph.ResolutionTier, edgeKind graph.EdgeKind) error {
	if !tier.IsValid() {
		return fmt.Errorf("%w: invalid tier %d", ErrInvalidResolution, tier)
	}
	if !ShouldEmitCallEdge(tier) {
		if edgeKind != graph.EdgeUnknown {
			return fmt.Errorf("%w: tier %d (%s) cannot emit %s edge", ErrInvalidResolution, tier, tier, edgeKind)
		}
		return nil
	}
	want := EdgeKindForTier(tier)
	if edgeKind != want {
		return fmt.Errorf("%w: tier %d (%s) requires edge kind %s, got %s", ErrInvalidResolution, tier, tier, want, edgeKind)
	}
	return nil
}

// ResolvedCall represents a resolved call site mapped to a target graph node
// along with tier provenance and line/column metadata.
type ResolvedCall struct {
	CallerUID   graph.UID            `json:"caller_uid"`
	CallerQName string               `json:"caller_qname,omitempty"`
	TargetUID   graph.UID            `json:"target_uid"`
	TargetQName string               `json:"target_qname,omitempty"`
	Tier        graph.ResolutionTier `json:"tier"`
	EdgeKind    graph.EdgeKind       `json:"edge_kind"`
	Line        uint32               `json:"line"`
	Column      uint16               `json:"column,omitempty"`
	Method      string               `json:"method"`
	LookupKey   string               `json:"lookup_key,omitempty"`
}

// Confidence returns the numeric confidence derived from Tier.
func (r ResolvedCall) Confidence() float32 {
	return ConfidenceForTier(r.Tier)
}

// ConfidenceLabel returns the confidence label derived from Tier.
func (r ResolvedCall) ConfidenceLabel() string {
	return LabelForTier(r.Tier)
}

// Validate checks that r is a valid call edge (never TierUnresolved or TierStructural).
func (r ResolvedCall) Validate() error {
	if r.CallerUID.IsZero() {
		return fmt.Errorf("%w: empty caller UID", ErrInvalidResolution)
	}
	if r.TargetUID.IsZero() {
		return fmt.Errorf("%w: empty target UID", ErrInvalidResolution)
	}
	if r.Line == 0 {
		return fmt.Errorf("%w: call line must be > 0", ErrInvalidResolution)
	}
	if !ShouldEmitCallEdge(r.Tier) {
		return fmt.Errorf("%w: resolved call tier %d (%s) cannot emit call edge", ErrInvalidResolution, r.Tier, r.Tier)
	}
	if err := ValidateCallTierAndKind(r.Tier, r.EdgeKind); err != nil {
		return err
	}
	if strings.TrimSpace(r.Method) == "" {
		return fmt.Errorf("%w: empty resolution method description", ErrInvalidResolution)
	}
	return nil
}

// ToCallSiteRef converts r into a graph.CallSiteRef for Edge provenance.
func (r ResolvedCall) ToCallSiteRef() graph.CallSiteRef {
	return graph.CallSiteRef{
		Line:   r.Line,
		Column: r.Column,
		Tier:   r.Tier,
		Label:  r.Method,
	}
}

// ToEdge converts r into a graph.Edge carrying single-site provenance.
func (r ResolvedCall) ToEdge() (graph.Edge, error) {
	if err := r.Validate(); err != nil {
		return graph.Edge{}, err
	}
	edge := graph.Edge{
		Source: r.CallerUID,
		Target: r.TargetUID,
		Kind:   r.EdgeKind,
		Tier:   r.Tier,
		Label:  r.Method,
		Sites:  []graph.CallSiteRef{r.ToCallSiteRef()},
	}
	if err := edge.Validate(); err != nil {
		return graph.Edge{}, err
	}
	return edge, nil
}

// UnresolvedCall captures a call site that could not be safely emitted as a
// CALLS/LIKELY_CALLS edge, preserving candidate UIDs and lookup keys for
// incremental invalidation and diagnostic inspection.
type UnresolvedCall struct {
	CallerUID   graph.UID            `json:"caller_uid"`
	CallerQName string               `json:"caller_qname,omitempty"`
	File        string               `json:"file"`
	Line        uint32               `json:"line"`
	Column      uint16               `json:"column,omitempty"`
	CalleeExpr  string               `json:"callee_expr"`
	LookupKey   string               `json:"lookup_key"`
	Candidates  []graph.UID          `json:"candidates,omitempty"`
	Status      graph.CallStatus     `json:"status"`
	Tier        graph.ResolutionTier `json:"tier"`
	Reason      string               `json:"reason"`
}

// Validate checks that u obeys CallSite invariants and does not claim a strong tier.
func (u UnresolvedCall) Validate() error {
	if u.Tier < TierTypeNarrowed || !u.Tier.IsValid() {
		return fmt.Errorf("%w: unresolved call requires tier >= TierTypeNarrowed, got %d", ErrInvalidResolution, u.Tier)
	}
	cs := u.ToCallSite()
	return cs.Validate()
}

// ToCallSite converts u into a graph.CallSite for insertion into Graph / FileFragment.
func (u UnresolvedCall) ToCallSite() graph.CallSite {
	return graph.CallSite{
		Caller:     u.CallerUID,
		File:       u.File,
		Line:       u.Line,
		Column:     u.Column,
		CalleeExpr: u.CalleeExpr,
		LookupKey:  u.LookupKey,
		Candidates: append([]graph.UID(nil), u.Candidates...),
		Status:     u.Status,
		Tier:       u.Tier,
		Reason:     u.Reason,
	}
}
