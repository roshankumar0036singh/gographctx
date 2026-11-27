package resolver

import (
	"strings"

	"github.com/roshankumar0036singh/gographctx/internal/graph"
)

// BuildLookupKey builds a canonical negative-dependency key from a call site's
// receiver type, package qualifier, and callee name.
func BuildLookupKey(receiverType, qualifier, callee string) string {
	recv := strings.TrimSpace(receiverType)
	qual := strings.TrimSpace(qualifier)
	fn := strings.TrimSpace(callee)

	switch {
	case qual != "" && recv != "" && fn != "":
		return qual + "::" + recv + "::" + fn
	case recv != "" && fn != "":
		return recv + "::" + fn
	case qual != "" && fn != "":
		return qual + "::" + fn
	default:
		return fn
	}
}

// IsSelfOrSuperReceiver reports whether recv refers to the current receiver instance
// across supported languages (self, &self, &mut self, this, super, cls).
func IsSelfOrSuperReceiver(recv string) bool {
	switch strings.TrimSpace(recv) {
	case "self", "&self", "&mut self", "this", "super", "cls":
		return true
	default:
		return false
	}
}

// StripTypeDecorations reduces pointer/reference/slice/generic/union wrappers
// on a type expression to its primary base type identifier for lookup matching.
func StripTypeDecorations(typeExpr string) string {
	s := strings.TrimSpace(typeExpr)
	if s == "" {
		return ""
	}
	// Handle nullable TS unions like "Client | null" or "Client | undefined".
	if idx := strings.IndexByte(s, '|'); idx >= 0 {
		s = strings.TrimSpace(s[:idx])
	}
	// Trim common slice/reference/pointer prefixes repeatedly.
	for {
		switch {
		case strings.HasPrefix(s, "[]"):
			s = strings.TrimSpace(s[2:])
		case strings.HasPrefix(s, "*"):
			s = strings.TrimSpace(s[1:])
		case strings.HasPrefix(s, "&mut "):
			s = strings.TrimSpace(s[5:])
		case strings.HasPrefix(s, "&"):
			s = strings.TrimSpace(s[1:])
		case strings.HasPrefix(s, "..."):
			s = strings.TrimSpace(s[3:])
		default:
			goto stripGenerics
		}
	}

stripGenerics:
	// Unwrap Optional[T] / Promise<T> / Arc<T> if needed when caller resolves inner type?
	// Preserve outer name unless caller inspects generics directly; strip angle/square args.
	if idx := strings.IndexByte(s, '['); idx >= 0 {
		s = s[:idx]
	}
	if idx := strings.IndexByte(s, '<'); idx >= 0 {
		s = s[:idx]
	}
	return strings.TrimSpace(s)
}

// MatchUniqueNode filters nodes with filter (if non-nil) and reports whether
// exactly one candidate remains. If zero or multiple match, it returns the
// collected candidate UIDs (sorted deterministically? preserves input order, deduped).
func MatchUniqueNode(nodes []graph.Node, filter func(graph.Node) bool) (unique graph.Node, ok bool, candidateUIDs []graph.UID) {
	var matched []graph.Node
	for _, n := range nodes {
		if filter != nil && !filter(n) {
			continue
		}
		matched = append(matched, n)
	}
	if len(matched) == 1 {
		return matched[0], true, []graph.UID{matched[0].UID}
	}
	if len(matched) == 0 {
		return graph.Node{}, false, nil
	}
	seen := make(map[graph.UID]struct{}, len(matched))
	candidateUIDs = make([]graph.UID, 0, len(matched))
	for _, m := range matched {
		if m.UID.IsZero() {
			continue
		}
		if _, dup := seen[m.UID]; !dup {
			seen[m.UID] = struct{}{}
			candidateUIDs = append(candidateUIDs, m.UID)
		}
	}
	return graph.Node{}, false, candidateUIDs
}
