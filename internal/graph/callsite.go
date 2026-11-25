package graph

import (
	"fmt"
	"slices"
	"strings"
)

// CallStatus classifies call sites that cannot be emitted as a single certain Edge.
type CallStatus uint8

const (
	CallStatusUnknown CallStatus = iota
	CallStatusUnresolved
	CallStatusAmbiguous
	CallStatusExternal
	numCallStatuses
)

var callStatusNames = [numCallStatuses]string{
	CallStatusUnknown:    "unknown",
	CallStatusUnresolved: "unresolved",
	CallStatusAmbiguous:  "ambiguous",
	CallStatusExternal:   "external",
}

// IsValid reports whether s is a valid CallStatus.
func (s CallStatus) IsValid() bool {
	return s >= CallStatusUnresolved && s < numCallStatuses
}

// String returns the canonical string name of the CallStatus.
func (s CallStatus) String() string {
	if s < numCallStatuses {
		return callStatusNames[s]
	}
	return "unknown"
}

// CallSite records source provenance for unresolved, ambiguous, or external calls,
// plus a LookupKey so newly added symbols can invalidate and resolve negative dependencies.
type CallSite struct {
	Caller     UID            `json:"caller"`
	File       string         `json:"file"`
	Line       uint32         `json:"line"`
	Column     uint16         `json:"column,omitempty"`
	CalleeExpr string         `json:"callee_expr"`
	LookupKey  string         `json:"lookup_key"`
	Candidates []UID          `json:"candidates,omitempty"`
	Status     CallStatus     `json:"status"`
	Tier       ResolutionTier `json:"tier"`
	Reason     string         `json:"reason,omitempty"`
}

// Validate checks that the CallSite satisfies provenance and candidate invariants.
func (c CallSite) Validate() error {
	if c.Caller.IsZero() {
		return fmt.Errorf("%w: empty caller UID", ErrInvalidCallSite)
	}
	if !c.Status.IsValid() {
		return fmt.Errorf("%w: unknown status %d", ErrInvalidCallSite, c.Status)
	}
	if c.Line == 0 {
		return fmt.Errorf("%w: line must be > 0", ErrInvalidCallSite)
	}
	if strings.TrimSpace(c.LookupKey) == "" {
		return fmt.Errorf("%w: empty lookup_key", ErrInvalidCallSite)
	}
	if !c.Tier.IsValid() {
		return fmt.Errorf("%w: invalid tier %d", ErrInvalidCallSite, c.Tier)
	}
	if c.Status == CallStatusAmbiguous && len(c.Candidates) == 0 {
		return fmt.Errorf("%w: ambiguous call site must record candidate UIDs", ErrInvalidCallSite)
	}
	for _, cand := range c.Candidates {
		if cand.IsZero() {
			return fmt.Errorf("%w: zero candidate UID", ErrInvalidCallSite)
		}
	}
	return nil
}

// Clone returns a deep copy of the CallSite.
func (c CallSite) Clone() CallSite {
	cp := c
	cp.Candidates = slices.Clone(c.Candidates)
	return cp
}
