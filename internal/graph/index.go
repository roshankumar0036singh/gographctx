package graph

import (
	"cmp"
	"fmt"
	"slices"
)

func (g *Graph) initIndexesLocked() {
	g.byName = make(map[string][]*Node)
	g.byFile = make(map[string][]*Node)
	g.byQName = make(map[string][]*Node)
	g.callers = make(map[UID][]*Edge)
	g.callees = make(map[UID][]*Edge)
	g.incoming = make(map[UID][]*Edge)
	g.outgoing = make(map[UID][]*Edge)
	g.contains = make(map[UID][]*Node)
	g.revDeps = make(map[UID]map[UID]struct{})
}

func (g *Graph) indexNodeLocked(n *Node) {
	if n.Name != "" {
		g.byName[n.Name] = append(g.byName[n.Name], n)
	}
	if n.File != "" {
		g.byFile[n.File] = append(g.byFile[n.File], n)
	}
	if n.QName != "" {
		g.byQName[n.QName] = append(g.byQName[n.QName], n)
	}
}

func (g *Graph) unindexNodeLocked(n *Node) {
	if n.Name != "" {
		g.byName[n.Name] = removeNodeFromSlice(g.byName[n.Name], n.UID)
		if len(g.byName[n.Name]) == 0 {
			delete(g.byName, n.Name)
		}
	}
	if n.File != "" {
		g.byFile[n.File] = removeNodeFromSlice(g.byFile[n.File], n.UID)
		if len(g.byFile[n.File]) == 0 {
			delete(g.byFile, n.File)
		}
	}
	if n.QName != "" {
		g.byQName[n.QName] = removeNodeFromSlice(g.byQName[n.QName], n.UID)
		if len(g.byQName[n.QName]) == 0 {
			delete(g.byQName, n.QName)
		}
	}
}

func (g *Graph) updateContainsNodePtrLocked(uid UID, oldNode, newNode *Node) {
	for _, e := range g.incoming[uid] {
		if e.Kind != EdgeContains {
			continue
		}
		children := g.contains[e.Source]
		for i, child := range children {
			if child == oldNode || child.UID == uid {
				children[i] = newNode
			}
		}
	}
}

func (g *Graph) indexEdgeLocked(e *Edge) {
	g.outgoing[e.Source] = append(g.outgoing[e.Source], e)
	g.incoming[e.Target] = append(g.incoming[e.Target], e)

	if e.Kind.IsCall() {
		g.callees[e.Source] = append(g.callees[e.Source], e)
		g.callers[e.Target] = append(g.callers[e.Target], e)
	}
	if e.Kind == EdgeContains {
		if targetNode, ok := g.nodes[e.Target]; ok {
			g.contains[e.Source] = append(g.contains[e.Source], targetNode)
		}
	}
	if e.Kind.IsDependency() {
		deps := g.revDeps[e.Target]
		if deps == nil {
			deps = make(map[UID]struct{})
			g.revDeps[e.Target] = deps
		}
		deps[e.Source] = struct{}{}
	}
}

func (g *Graph) unindexEdgeLocked(e *Edge) {
	g.outgoing[e.Source] = removeEdgeFromSlice(g.outgoing[e.Source], e)
	if len(g.outgoing[e.Source]) == 0 {
		delete(g.outgoing, e.Source)
	}

	g.incoming[e.Target] = removeEdgeFromSlice(g.incoming[e.Target], e)
	if len(g.incoming[e.Target]) == 0 {
		delete(g.incoming, e.Target)
	}

	if e.Kind.IsCall() {
		g.callees[e.Source] = removeEdgeFromSlice(g.callees[e.Source], e)
		if len(g.callees[e.Source]) == 0 {
			delete(g.callees, e.Source)
		}

		g.callers[e.Target] = removeEdgeFromSlice(g.callers[e.Target], e)
		if len(g.callers[e.Target]) == 0 {
			delete(g.callers, e.Target)
		}
	}

	if e.Kind == EdgeContains {
		g.contains[e.Source] = removeNodeFromSlice(g.contains[e.Source], e.Target)
		if len(g.contains[e.Source]) == 0 {
			delete(g.contains, e.Source)
		}
	}

	if e.Kind.IsDependency() {
		stillDepends := false
		for _, rem := range g.outgoing[e.Source] {
			if rem.Target == e.Target && rem.Kind.IsDependency() {
				stillDepends = true
				break
			}
		}
		if !stillDepends {
			if deps := g.revDeps[e.Target]; deps != nil {
				delete(deps, e.Source)
				if len(deps) == 0 {
					delete(g.revDeps, e.Target)
				}
			}
		}
	}
}

func (g *Graph) replaceEdgePtrLocked(oldEdge, newEdge *Edge) {
	replacePtrInEdgeSlice(g.outgoing[newEdge.Source], oldEdge, newEdge)
	replacePtrInEdgeSlice(g.incoming[newEdge.Target], oldEdge, newEdge)
	if newEdge.Kind.IsCall() {
		replacePtrInEdgeSlice(g.callees[newEdge.Source], oldEdge, newEdge)
		replacePtrInEdgeSlice(g.callers[newEdge.Target], oldEdge, newEdge)
	}
}

// GetByName returns all nodes with the given short Name, sorted deterministically.
func (g *Graph) GetByName(name string) []*Node {
	if name == "" {
		return nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()

	nodes := slices.Clone(g.byName[name])
	sortNodesByLocation(nodes)
	return nodes
}

// GetByFile returns all nodes defined in the given file path, sorted by source location.
func (g *Graph) GetByFile(file string) []*Node {
	if file == "" {
		return nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()

	nodes := slices.Clone(g.byFile[file])
	sortNodesByLocation(nodes)
	return nodes
}

// GetByQName returns the first node matching the qualified name in deterministic order.
func (g *Graph) GetByQName(qname string) (*Node, bool) {
	if qname == "" {
		return nil, false
	}
	g.mu.RLock()
	defer g.mu.RUnlock()

	nodes := g.byQName[qname]
	if len(nodes) == 0 {
		return nil, false
	}
	if len(nodes) == 1 {
		return nodes[0], true
	}
	cp := slices.Clone(nodes)
	sortNodesByLocation(cp)
	return cp[0], true
}

// GetAllByQName returns all nodes sharing the given qualified name (e.g. overloaded methods).
func (g *Graph) GetAllByQName(qname string) []*Node {
	if qname == "" {
		return nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()

	nodes := slices.Clone(g.byQName[qname])
	sortNodesByLocation(nodes)
	return nodes
}

// GetCallers returns incoming CALLS and LIKELY_CALLS edges for the target node,
// sorted by confidence descending, then call line and source UID.
func (g *Graph) GetCallers(uid UID) []*Edge {
	if uid.IsZero() {
		return nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()

	edges := slices.Clone(g.callers[uid])
	sortCallEdges(edges)
	return edges
}

// GetCallees returns outgoing CALLS and LIKELY_CALLS edges from the source node,
// sorted by confidence descending, then call line and target UID.
func (g *Graph) GetCallees(uid UID) []*Edge {
	if uid.IsZero() {
		return nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()

	edges := slices.Clone(g.callees[uid])
	sortCallEdges(edges)
	return edges
}

// GetIncomingEdges returns incoming edges to uid, optionally filtered by kinds.
func (g *Graph) GetIncomingEdges(uid UID, kinds ...EdgeKind) []*Edge {
	if uid.IsZero() {
		return nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()

	edges := filterEdgesByKind(g.incoming[uid], kinds)
	slices.SortFunc(edges, compareEdgesCanonical)
	return edges
}

// GetOutgoingEdges returns outgoing edges from uid, optionally filtered by kinds.
func (g *Graph) GetOutgoingEdges(uid UID, kinds ...EdgeKind) []*Edge {
	if uid.IsZero() {
		return nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()

	edges := filterEdgesByKind(g.outgoing[uid], kinds)
	slices.SortFunc(edges, compareEdgesCanonical)
	return edges
}

// GetContains returns all direct child nodes contained by parentUID, sorted by source location.
func (g *Graph) GetContains(parentUID UID) []*Node {
	if parentUID.IsZero() {
		return nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()

	nodes := slices.Clone(g.contains[parentUID])
	sortNodesByLocation(nodes)
	return nodes
}

// GetRevDeps returns the sorted list of node UIDs that depend on targetUID.
func (g *Graph) GetRevDeps(targetUID UID) []UID {
	if targetUID.IsZero() {
		return nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()

	deps := g.revDeps[targetUID]
	if len(deps) == 0 {
		return nil
	}
	result := make([]UID, 0, len(deps))
	for srcUID := range deps {
		result = append(result, srcUID)
	}
	slices.SortFunc(result, func(a, b UID) int {
		return a.Compare(b)
	})
	return result
}

// GetDependentFiles returns the sorted unique list of files containing nodes that depend on targetUID.
func (g *Graph) GetDependentFiles(targetUID UID) []string {
	if targetUID.IsZero() {
		return nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()

	deps := g.revDeps[targetUID]
	if len(deps) == 0 {
		return nil
	}
	seen := make(map[string]struct{})
	for srcUID := range deps {
		if n, ok := g.nodes[srcUID]; ok && n.File != "" {
			seen[n.File] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	files := make([]string, 0, len(seen))
	for f := range seen {
		files = append(files, f)
	}
	slices.Sort(files)
	return files
}

// RebuildIndexes clears and reconstructs all secondary indexes from the stored nodes and edges.
func (g *Graph) RebuildIndexes() {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.initIndexesLocked()
	for _, n := range g.nodes {
		g.indexNodeLocked(n)
	}
	for _, e := range g.edges {
		g.indexEdgeLocked(e)
	}
}

// VerifyIndexConsistency checks that every secondary index matches the canonical nodes and edges maps.
func (g *Graph) VerifyIndexConsistency() error {
	g.mu.RLock()
	defer g.mu.RUnlock()

	expectedNameCount := 0
	expectedFileCount := 0
	expectedQNameCount := 0
	for uid, n := range g.nodes {
		if n == nil {
			return fmt.Errorf("nil node stored for UID %s", uid)
		}
		if n.UID != uid {
			return fmt.Errorf("node UID mismatch: key %s != node.UID %s", uid, n.UID)
		}
		if n.Name != "" {
			expectedNameCount++
		}
		if n.File != "" {
			expectedFileCount++
		}
		if n.QName != "" {
			expectedQNameCount++
		}
	}

	if err := verifyNodeIndexMap("byName", g.byName, g.nodes, expectedNameCount, func(n *Node) string { return n.Name }); err != nil {
		return err
	}
	if err := verifyNodeIndexMap("byFile", g.byFile, g.nodes, expectedFileCount, func(n *Node) string { return n.File }); err != nil {
		return err
	}
	if err := verifyNodeIndexMap("byQName", g.byQName, g.nodes, expectedQNameCount, func(n *Node) string { return n.QName }); err != nil {
		return err
	}

	expectedCallCount := 0
	expectedContainsCount := 0
	expectedRevDeps := make(map[UID]map[UID]struct{})
	for key, e := range g.edges {
		if e == nil {
			return fmt.Errorf("nil edge stored for key %v", key)
		}
		if e.Source != key.source || e.Target != key.target || e.Kind != key.kind {
			return fmt.Errorf("edge key mismatch")
		}
		if _, ok := g.nodes[e.Source]; !ok {
			return fmt.Errorf("dangling edge source %s", e.Source)
		}
		if _, ok := g.nodes[e.Target]; !ok {
			return fmt.Errorf("dangling edge target %s", e.Target)
		}
		if e.Kind.IsCall() {
			expectedCallCount++
		}
		if e.Kind == EdgeContains {
			expectedContainsCount++
		}
		if e.Kind.IsDependency() {
			if expectedRevDeps[e.Target] == nil {
				expectedRevDeps[e.Target] = make(map[UID]struct{})
			}
			expectedRevDeps[e.Target][e.Source] = struct{}{}
		}
	}

	if err := verifyEdgeIndexMap("outgoing", g.outgoing, g.edges, len(g.edges), func(e *Edge) UID { return e.Source }); err != nil {
		return err
	}
	if err := verifyEdgeIndexMap("incoming", g.incoming, g.edges, len(g.edges), func(e *Edge) UID { return e.Target }); err != nil {
		return err
	}
	if err := verifyEdgeIndexMap("callees", g.callees, g.edges, expectedCallCount, func(e *Edge) UID { return e.Source }); err != nil {
		return err
	}
	if err := verifyEdgeIndexMap("callers", g.callers, g.edges, expectedCallCount, func(e *Edge) UID { return e.Target }); err != nil {
		return err
	}

	actualContainsCount := 0
	for parentUID, children := range g.contains {
		if len(children) == 0 {
			return fmt.Errorf("contains index has empty slice for parent %s", parentUID)
		}
		seen := make(map[UID]struct{}, len(children))
		for _, child := range children {
			if child == nil {
				return fmt.Errorf("contains index has nil child for parent %s", parentUID)
			}
			if g.nodes[child.UID] != child {
				return fmt.Errorf("contains index has stale pointer for child %s", child.UID)
			}
			if _, dup := seen[child.UID]; dup {
				return fmt.Errorf("contains index has duplicate child %s for parent %s", child.UID, parentUID)
			}
			seen[child.UID] = struct{}{}
			if _, ok := g.edges[edgeKey{source: parentUID, target: child.UID, kind: EdgeContains}]; !ok {
				return fmt.Errorf("contains index has entry %s -> %s without CONTAINS edge", parentUID, child.UID)
			}
			actualContainsCount++
		}
	}
	if actualContainsCount != expectedContainsCount {
		return fmt.Errorf("contains index count %d != expected %d", actualContainsCount, expectedContainsCount)
	}

	if len(g.revDeps) != len(expectedRevDeps) {
		return fmt.Errorf("revDeps key count %d != expected %d", len(g.revDeps), len(expectedRevDeps))
	}
	for target, expectedSrcs := range expectedRevDeps {
		actualSrcs := g.revDeps[target]
		if len(actualSrcs) != len(expectedSrcs) {
			return fmt.Errorf("revDeps[%s] size %d != expected %d", target, len(actualSrcs), len(expectedSrcs))
		}
		for src := range expectedSrcs {
			if _, ok := actualSrcs[src]; !ok {
				return fmt.Errorf("revDeps[%s] missing source %s", target, src)
			}
		}
	}

	return nil
}

func verifyNodeIndexMap(name string, index map[string][]*Node, canonical map[UID]*Node, expectedTotal int, keyFn func(*Node) string) error {
	actualTotal := 0
	for k, list := range index {
		if len(list) == 0 {
			return fmt.Errorf("%s index has empty slice for key %q", name, k)
		}
		seen := make(map[UID]struct{}, len(list))
		for _, n := range list {
			if n == nil || canonical[n.UID] != n {
				return fmt.Errorf("%s index has stale or unknown node for key %q", name, k)
			}
			if keyFn(n) != k {
				return fmt.Errorf("%s index key mismatch: got %q, want %q", name, k, keyFn(n))
			}
			if _, dup := seen[n.UID]; dup {
				return fmt.Errorf("%s index has duplicate UID %s under key %q", name, n.UID, k)
			}
			seen[n.UID] = struct{}{}
			actualTotal++
		}
	}
	if actualTotal != expectedTotal {
		return fmt.Errorf("%s index count %d != expected %d", name, actualTotal, expectedTotal)
	}
	return nil
}

func verifyEdgeIndexMap(name string, index map[UID][]*Edge, canonical map[edgeKey]*Edge, expectedTotal int, keyFn func(*Edge) UID) error {
	actualTotal := 0
	for uid, list := range index {
		if len(list) == 0 {
			return fmt.Errorf("%s index has empty slice for UID %s", name, uid)
		}
		seen := make(map[edgeKey]struct{}, len(list))
		for _, e := range list {
			if e == nil {
				return fmt.Errorf("%s index has nil edge for UID %s", name, uid)
			}
			ek := edgeKey{source: e.Source, target: e.Target, kind: e.Kind}
			if canonical[ek] != e {
				return fmt.Errorf("%s index has stale or unknown edge %v", name, ek)
			}
			if keyFn(e) != uid {
				return fmt.Errorf("%s index key mismatch for UID %s", name, uid)
			}
			if _, dup := seen[ek]; dup {
				return fmt.Errorf("%s index has duplicate edge %v", name, ek)
			}
			seen[ek] = struct{}{}
			actualTotal++
		}
	}
	if actualTotal != expectedTotal {
		return fmt.Errorf("%s index count %d != expected %d", name, actualTotal, expectedTotal)
	}
	return nil
}

func removeNodeFromSlice(list []*Node, uid UID) []*Node {
	for i, n := range list {
		if n.UID == uid {
			last := len(list) - 1
			list[i] = list[last]
			list[last] = nil
			return list[:last]
		}
	}
	return list
}

func removeEdgeFromSlice(list []*Edge, target *Edge) []*Edge {
	for i, e := range list {
		if e == target || (e.Source == target.Source && e.Target == target.Target && e.Kind == target.Kind) {
			last := len(list) - 1
			list[i] = list[last]
			list[last] = nil
			return list[:last]
		}
	}
	return list
}

func replacePtrInEdgeSlice(list []*Edge, oldEdge, newEdge *Edge) {
	for i, e := range list {
		if e == oldEdge {
			list[i] = newEdge
			return
		}
	}
}

func filterEdgesByKind(list []*Edge, kinds []EdgeKind) []*Edge {
	if len(list) == 0 {
		return nil
	}
	if len(kinds) == 0 {
		return slices.Clone(list)
	}
	var out []*Edge
	for _, e := range list {
		if slices.Contains(kinds, e.Kind) {
			out = append(out, e)
		}
	}
	return out
}

func sortNodesByLocation(nodes []*Node) {
	slices.SortFunc(nodes, func(a, b *Node) int {
		if c := cmp.Compare(a.File, b.File); c != 0 {
			return c
		}
		if c := cmp.Compare(a.StartLine, b.StartLine); c != 0 {
			return c
		}
		if c := cmp.Compare(a.EndLine, b.EndLine); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Kind, b.Kind); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return a.UID.Compare(b.UID)
	})
}

func sortCallEdges(edges []*Edge) {
	slices.SortFunc(edges, func(a, b *Edge) int {
		// Higher confidence first
		if c := cmp.Compare(b.Confidence, a.Confidence); c != 0 {
			return c
		}
		// Lower tier number (stronger tier) first
		if c := cmp.Compare(a.Tier, b.Tier); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Line, b.Line); c != 0 {
			return c
		}
		return compareEdgesCanonical(a, b)
	})
}

