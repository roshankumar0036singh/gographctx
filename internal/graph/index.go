package graph

import (
	"cmp"
	"fmt"
	"slices"
)

func (g *Graph) initIndexesLocked() {
	g.byName = make(map[string]idList)
	g.byFile = make(map[string]idList)
	g.byQName = make(map[string]idList)
}

func (g *Graph) indexNodeLocked(id uint32, n *Node) {
	if n.Name != "" {
		l := g.byName[n.Name]
		l.add(id)
		g.byName[n.Name] = l
	}
	if n.File != "" {
		l := g.byFile[n.File]
		l.add(id)
		g.byFile[n.File] = l
	}
	if n.QName != "" {
		l := g.byQName[n.QName]
		l.add(id)
		g.byQName[n.QName] = l
	}
}

func (g *Graph) unindexNodeLocked(id uint32, n *Node) {
	if n.Name != "" {
		if l, ok := g.byName[n.Name]; ok {
			if l.remove(id) {
				delete(g.byName, n.Name)
			} else {
				g.byName[n.Name] = l
			}
		}
	}
	if n.File != "" {
		if l, ok := g.byFile[n.File]; ok {
			if l.remove(id) {
				delete(g.byFile, n.File)
			} else {
				g.byFile[n.File] = l
			}
		}
	}
	if n.QName != "" {
		if l, ok := g.byQName[n.QName]; ok {
			if l.remove(id) {
				delete(g.byQName, n.QName)
			} else {
				g.byQName[n.QName] = l
			}
		}
	}
}

func (g *Graph) collectIDsFromList(l idList) []uint32 {
	n := l.len()
	if n == 0 {
		return nil
	}
	out := make([]uint32, 0, n)
	return l.appendTo(out)
}

func (g *Graph) collectNodesFromListLocked(l idList) []Node {
	n := l.len()
	if n == 0 {
		return nil
	}
	out := make([]Node, 0, n)
	if l.hasFirst {
		out = append(out, g.nodes[l.first].Clone())
		for _, id := range l.rest {
			out = append(out, g.nodes[id].Clone())
		}
	}
	sortNodesByLocation(out)
	return out
}

// GetByName returns copies of all nodes with the given short Name, sorted deterministically.
func (g *Graph) GetByName(name string) []Node {
	if name == "" {
		return nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.getByNameLocked(name)
}

func (g *Graph) getByNameLocked(name string) []Node {
	if !g.init || name == "" {
		return nil
	}
	return g.collectNodesFromListLocked(g.byName[name])
}

// GetByFile returns copies of all nodes defined in the given file path, sorted by source location.
func (g *Graph) GetByFile(file string) []Node {
	if file == "" {
		return nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.getByFileLocked(file)
}

func (g *Graph) getByFileLocked(file string) []Node {
	if !g.init || file == "" {
		return nil
	}
	return g.collectNodesFromListLocked(g.byFile[file])
}

// GetByQName returns copies of all nodes matching the given qualified name, sorted by source location.
// Unlike GetUniqueByQName, it never silently picks a single overload or sibling.
func (g *Graph) GetByQName(qname string) []Node {
	if qname == "" {
		return nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.getByQNameLocked(qname)
}

func (g *Graph) getByQNameLocked(qname string) []Node {
	if !g.init || qname == "" {
		return nil
	}
	return g.collectNodesFromListLocked(g.byQName[qname])
}

// GetUniqueByQName returns the single node matching qname, or ErrNodeNotFound / ErrAmbiguousQName.
func (g *Graph) GetUniqueByQName(qname string) (Node, error) {
	if qname == "" {
		return Node{}, fmt.Errorf("%w: empty qualified name", ErrNodeNotFound)
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.getUniqueByQNameLocked(qname)
}

func (g *Graph) getUniqueByQNameLocked(qname string) (Node, error) {
	if !g.init || qname == "" {
		return Node{}, fmt.Errorf("%w: %q", ErrNodeNotFound, qname)
	}
	l := g.byQName[qname]
	count := l.len()
	if count == 0 {
		return Node{}, fmt.Errorf("%w: %q", ErrNodeNotFound, qname)
	}
	if count > 1 {
		return Node{}, fmt.Errorf("%w: %q matched %d symbols", ErrAmbiguousQName, qname, count)
	}
	return g.nodes[l.first].Clone(), nil
}

// GetCallers returns copies of incoming CALLS and LIKELY_CALLS edges for the target node,
// sorted by stronger tier first, then first call line and source UID.
func (g *Graph) GetCallers(uid UID) []Edge {
	if uid.IsZero() {
		return nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.getCallersLocked(uid)
}

func (g *Graph) getCallersLocked(uid UID) []Edge {
	if !g.init || uid.IsZero() {
		return nil
	}
	id, ok := g.uidToID[uid]
	if !ok {
		return nil
	}
	var out []Edge
	for _, eid := range g.incoming[id] {
		e := &g.edges[eid]
		if e.alive() && e.kind.IsCall() {
			out = append(out, g.materializeEdgeLocked(eid))
		}
	}
	sortCallEdges(out)
	return out
}

// GetCallees returns copies of outgoing CALLS and LIKELY_CALLS edges from the source node,
// sorted by stronger tier first, then first call line and target UID.
func (g *Graph) GetCallees(uid UID) []Edge {
	if uid.IsZero() {
		return nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.getCalleesLocked(uid)
}

func (g *Graph) getCalleesLocked(uid UID) []Edge {
	if !g.init || uid.IsZero() {
		return nil
	}
	id, ok := g.uidToID[uid]
	if !ok {
		return nil
	}
	var out []Edge
	for _, eid := range g.outgoing[id] {
		e := &g.edges[eid]
		if e.alive() && e.kind.IsCall() {
			out = append(out, g.materializeEdgeLocked(eid))
		}
	}
	sortCallEdges(out)
	return out
}

// GetIncomingEdges returns copies of incoming edges to uid, optionally filtered by kinds.
func (g *Graph) GetIncomingEdges(uid UID, kinds ...EdgeKind) []Edge {
	if uid.IsZero() {
		return nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.getIncomingEdgesLocked(uid, kinds...)
}

func (g *Graph) getIncomingEdgesLocked(uid UID, kinds ...EdgeKind) []Edge {
	if !g.init || uid.IsZero() {
		return nil
	}
	id, ok := g.uidToID[uid]
	if !ok {
		return nil
	}
	out := g.collectFilteredEdgesLocked(g.incoming[id], kinds)
	slices.SortFunc(out, compareEdgesCanonical)
	return out
}

// GetOutgoingEdges returns copies of outgoing edges from uid, optionally filtered by kinds.
func (g *Graph) GetOutgoingEdges(uid UID, kinds ...EdgeKind) []Edge {
	if uid.IsZero() {
		return nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.getOutgoingEdgesLocked(uid, kinds...)
}

func (g *Graph) getOutgoingEdgesLocked(uid UID, kinds ...EdgeKind) []Edge {
	if !g.init || uid.IsZero() {
		return nil
	}
	id, ok := g.uidToID[uid]
	if !ok {
		return nil
	}
	out := g.collectFilteredEdgesLocked(g.outgoing[id], kinds)
	slices.SortFunc(out, compareEdgesCanonical)
	return out
}

func (g *Graph) collectFilteredEdgesLocked(eids []uint32, kinds []EdgeKind) []Edge {
	if len(eids) == 0 {
		return nil
	}
	var out []Edge
	for _, eid := range eids {
		e := &g.edges[eid]
		if !e.alive() {
			continue
		}
		if len(kinds) == 0 || slices.Contains(kinds, e.kind) {
			out = append(out, g.materializeEdgeLocked(eid))
		}
	}
	return out
}

// GetContains returns copies of all direct child nodes contained by parentUID, sorted by source location.
func (g *Graph) GetContains(parentUID UID) []Node {
	if parentUID.IsZero() {
		return nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.getContainsLocked(parentUID)
}

func (g *Graph) getContainsLocked(parentUID UID) []Node {
	if !g.init || parentUID.IsZero() {
		return nil
	}
	id, ok := g.uidToID[parentUID]
	if !ok {
		return nil
	}
	var out []Node
	for _, eid := range g.outgoing[id] {
		e := &g.edges[eid]
		if e.alive() && e.kind == EdgeContains {
			out = append(out, g.nodes[e.target].Clone())
		}
	}
	sortNodesByLocation(out)
	return out
}

// GetRevDeps returns the sorted unique list of node UIDs that depend on targetUID.
func (g *Graph) GetRevDeps(targetUID UID) []UID {
	if targetUID.IsZero() {
		return nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.getRevDepsLocked(targetUID)
}

func (g *Graph) getRevDepsLocked(targetUID UID) []UID {
	if !g.init || targetUID.IsZero() {
		return nil
	}
	id, ok := g.uidToID[targetUID]
	if !ok {
		return nil
	}
	seen := make(map[UID]struct{})
	for _, eid := range g.incoming[id] {
		e := &g.edges[eid]
		if e.alive() && e.kind.IsDependency() {
			seen[g.nodes[e.source].UID] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	result := make([]UID, 0, len(seen))
	for srcUID := range seen {
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
	return g.getDependentFilesLocked(targetUID)
}

func (g *Graph) getDependentFilesLocked(targetUID UID) []string {
	if !g.init || targetUID.IsZero() {
		return nil
	}
	id, ok := g.uidToID[targetUID]
	if !ok {
		return nil
	}
	seen := make(map[string]struct{})
	for _, eid := range g.incoming[id] {
		e := &g.edges[eid]
		if e.alive() && e.kind.IsDependency() {
			if f := g.nodes[e.source].File; f != "" {
				seen[f] = struct{}{}
			}
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

// RebuildIndexes clears and reconstructs all secondary and adjacency indexes from canonical storage.
func (g *Graph) RebuildIndexes() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ensureInitLocked()

	g.initIndexesLocked()
	g.uidToID = make(map[UID]uint32, g.nodeCount)
	for i := range g.outgoing {
		g.outgoing[i] = g.outgoing[i][:0]
	}
	for i := range g.incoming {
		g.incoming[i] = g.incoming[i][:0]
	}
	for id := range g.nodes {
		n := &g.nodes[id]
		if n.Kind.IsValid() {
			g.uidToID[n.UID] = uint32(id)
			g.indexNodeLocked(uint32(id), n)
		}
	}

	g.edgeToID = make(map[compactEdgeKey]uint32, g.edgeCount)
	for eid := range g.edges {
		e := &g.edges[eid]
		if e.alive() {
			g.edgeToID[compactEdgeKey{source: e.source, target: e.target, kind: e.kind}] = uint32(eid)
			g.outgoing[e.source] = append(g.outgoing[e.source], uint32(eid))
			g.incoming[e.target] = append(g.incoming[e.target], uint32(eid))
		}
	}

	g.rebuildCallSiteIndexesLocked()
}

// VerifyIndexConsistency checks that every secondary index and adjacency list matches canonical storage.
func (g *Graph) VerifyIndexConsistency() error {
	g.mu.RLock()
	defer g.mu.RUnlock()

	if !g.init {
		if g.nodeCount != 0 || g.edgeCount != 0 {
			return fmt.Errorf("uninitialized graph has non-zero counts")
		}
		return nil
	}

	if len(g.outgoing) != len(g.nodes) || len(g.incoming) != len(g.nodes) {
		return fmt.Errorf("adjacency length mismatch: nodes=%d out=%d in=%d", len(g.nodes), len(g.outgoing), len(g.incoming))
	}

	freeNodeSet := make(map[uint32]struct{}, len(g.freeNodeIDs))
	for _, fid := range g.freeNodeIDs {
		if int(fid) >= len(g.nodes) {
			return fmt.Errorf("free node ID %d out of range", fid)
		}
		if _, dup := freeNodeSet[fid]; dup {
			return fmt.Errorf("duplicate free node ID %d", fid)
		}
		freeNodeSet[fid] = struct{}{}
	}

	aliveNodes := 0
	expectedNameCount := 0
	expectedFileCount := 0
	expectedQNameCount := 0
	for id := range g.nodes {
		uid32 := uint32(id)
		n := &g.nodes[id]
		if !n.Kind.IsValid() {
			if _, ok := freeNodeSet[uid32]; !ok {
				return fmt.Errorf("dead node slot %d missing from freeNodeIDs", id)
			}
			continue
		}
		if _, isFree := freeNodeSet[uid32]; isFree {
			return fmt.Errorf("alive node slot %d listed in freeNodeIDs", id)
		}
		if mappedID, ok := g.uidToID[n.UID]; !ok || mappedID != uid32 {
			return fmt.Errorf("uidToID mismatch for %s: mapped=%d ok=%v want=%d", n.UID, mappedID, ok, id)
		}
		aliveNodes++
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
	if aliveNodes != g.nodeCount || len(g.uidToID) != g.nodeCount {
		return fmt.Errorf("node count mismatch: counted=%d nodeCount=%d uidToID=%d", aliveNodes, g.nodeCount, len(g.uidToID))
	}

	if err := verifyIDIndexMap("byName", g.byName, g.nodes, expectedNameCount, func(n *Node) string { return n.Name }); err != nil {
		return err
	}
	if err := verifyIDIndexMap("byFile", g.byFile, g.nodes, expectedFileCount, func(n *Node) string { return n.File }); err != nil {
		return err
	}
	if err := verifyIDIndexMap("byQName", g.byQName, g.nodes, expectedQNameCount, func(n *Node) string { return n.QName }); err != nil {
		return err
	}

	freeEdgeSet := make(map[uint32]struct{}, len(g.freeEdgeIDs))
	for _, fid := range g.freeEdgeIDs {
		if int(fid) >= len(g.edges) {
			return fmt.Errorf("free edge ID %d out of range", fid)
		}
		if _, dup := freeEdgeSet[fid]; dup {
			return fmt.Errorf("duplicate free edge ID %d", fid)
		}
		freeEdgeSet[fid] = struct{}{}
	}

	aliveEdges := 0
	for eid := range g.edges {
		eid32 := uint32(eid)
		e := &g.edges[eid]
		if !e.alive() {
			if _, ok := freeEdgeSet[eid32]; !ok {
				return fmt.Errorf("dead edge slot %d missing from freeEdgeIDs", eid)
			}
			continue
		}
		if _, isFree := freeEdgeSet[eid32]; isFree {
			return fmt.Errorf("alive edge slot %d listed in freeEdgeIDs", eid)
		}
		if int(e.source) >= len(g.nodes) || !g.nodes[e.source].Kind.IsValid() {
			return fmt.Errorf("dangling edge %d source id %d", eid, e.source)
		}
		if int(e.target) >= len(g.nodes) || !g.nodes[e.target].Kind.IsValid() {
			return fmt.Errorf("dangling edge %d target id %d", eid, e.target)
		}
		ek := compactEdgeKey{source: e.source, target: e.target, kind: e.kind}
		if mappedEID, ok := g.edgeToID[ek]; !ok || mappedEID != eid32 {
			return fmt.Errorf("edgeToID mismatch for %v", ek)
		}
		aliveEdges++
	}
	if aliveEdges != g.edgeCount || len(g.edgeToID) != g.edgeCount {
		return fmt.Errorf("edge count mismatch: counted=%d edgeCount=%d edgeToID=%d", aliveEdges, g.edgeCount, len(g.edgeToID))
	}

	if err := verifyAdjacency("outgoing", g.outgoing, g.nodes, g.edges, g.edgeCount, func(e *edgeEntry) uint32 { return e.source }); err != nil {
		return err
	}
	if err := verifyAdjacency("incoming", g.incoming, g.nodes, g.edges, g.edgeCount, func(e *edgeEntry) uint32 { return e.target }); err != nil {
		return err
	}

	return nil
}

func verifyIDIndexMap(name string, index map[string]idList, nodes []Node, expectedTotal int, keyFn func(*Node) string) error {
	actualTotal := 0
	for k, l := range index {
		if l.len() == 0 {
			return fmt.Errorf("%s index has empty entry for key %q", name, k)
		}
		ids := make([]uint32, 0, l.len())
		ids = l.appendTo(ids)
		seen := make(map[uint32]struct{}, len(ids))
		for _, id := range ids {
			if int(id) >= len(nodes) || !nodes[id].Kind.IsValid() {
				return fmt.Errorf("%s index has stale or out-of-range node ID %d for key %q", name, id, k)
			}
			if keyFn(&nodes[id]) != k {
				return fmt.Errorf("%s index key mismatch: got %q, want %q", name, k, keyFn(&nodes[id]))
			}
			if _, dup := seen[id]; dup {
				return fmt.Errorf("%s index has duplicate node ID %d under key %q", name, id, k)
			}
			seen[id] = struct{}{}
			actualTotal++
		}
	}
	if actualTotal != expectedTotal {
		return fmt.Errorf("%s index count %d != expected %d", name, actualTotal, expectedTotal)
	}
	return nil
}

func verifyAdjacency(name string, adj [][]uint32, nodes []Node, edges []edgeEntry, expectedTotal int, endpointFn func(*edgeEntry) uint32) error {
	actualTotal := 0
	for nid, eids := range adj {
		if !nodes[nid].Kind.IsValid() && len(eids) > 0 {
			return fmt.Errorf("%s adjacency non-empty for dead node %d", name, nid)
		}
		seen := make(map[uint32]struct{}, len(eids))
		for _, eid := range eids {
			if int(eid) >= len(edges) || !edges[eid].alive() {
				return fmt.Errorf("%s[%d] references invalid/dead edge %d", name, nid, eid)
			}
			if endpointFn(&edges[eid]) != uint32(nid) {
				return fmt.Errorf("%s[%d] references mismatched edge %d", name, nid, eid)
			}
			if _, dup := seen[eid]; dup {
				return fmt.Errorf("%s[%d] has duplicate edge %d", name, nid, eid)
			}
			seen[eid] = struct{}{}
			actualTotal++
		}
	}
	if actualTotal != expectedTotal {
		return fmt.Errorf("%s adjacency count %d != expected %d", name, actualTotal, expectedTotal)
	}
	return nil
}

func sortNodesByLocation(nodes []Node) {
	slices.SortFunc(nodes, func(a, b Node) int {
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

func sortCallEdges(edges []Edge) {
	slices.SortFunc(edges, func(a, b Edge) int {
		// Lower tier number (stronger tier / higher confidence) first.
		if c := cmp.Compare(a.Tier, b.Tier); c != 0 {
			return c
		}
		var aLine, bLine uint32
		if len(a.Sites) > 0 {
			aLine = a.Sites[0].Line
		}
		if len(b.Sites) > 0 {
			bLine = b.Sites[0].Line
		}
		if c := cmp.Compare(aLine, bLine); c != 0 {
			return c
		}
		return compareEdgesCanonical(a, b)
	})
}
