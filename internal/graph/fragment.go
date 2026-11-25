package graph

import (
	"fmt"
	"strings"
)

// FileFragment holds the nodes, edges, and unresolved/ambiguous call sites
// extracted from a single file. Workers can construct FileFragments completely
// concurrently without acquiring any lock on Graph, then merge them in bulk.
type FileFragment struct {
	File      string
	Nodes     []Node
	Edges     []Edge
	CallSites []CallSite
}

// MergeFragments validates and inserts a batch of FileFragments into the graph
// under a single lock, pre-sizing internal maps and slices from total counts.
func (g *Graph) MergeFragments(fragments []FileFragment) error {
	totalNodes := 0
	totalEdges := 0
	totalSites := 0

	prepFragments := make([]FileFragment, len(fragments))
	for i := range fragments {
		frag := fragments[i]
		trimmedFile := strings.TrimSpace(frag.File)
		if trimmedFile == "" && (len(frag.Nodes) > 0 || len(frag.Edges) > 0 || len(frag.CallSites) > 0) {
			return fmt.Errorf("%w: fragment %d has empty file path", ErrInvalidNode, i)
		}

		nodes := make([]Node, len(frag.Nodes))
		for j := range frag.Nodes {
			n := frag.Nodes[j].Clone()
			if n.File == "" && trimmedFile != "" {
				n.File = trimmedFile
			}
			if err := n.Validate(); err != nil {
				return fmt.Errorf("fragment %q node %d: %w", trimmedFile, j, err)
			}
			if n.UID.IsZero() {
				n.UID = ComputeNodeUID(n)
			}
			nodes[j] = n
		}

		edges := make([]Edge, len(frag.Edges))
		for j := range frag.Edges {
			if err := frag.Edges[j].Validate(); err != nil {
				return fmt.Errorf("fragment %q edge %d: %w", trimmedFile, j, err)
			}
			edges[j] = frag.Edges[j].Clone()
		}

		sites := make([]CallSite, len(frag.CallSites))
		for j := range frag.CallSites {
			cs := frag.CallSites[j].Clone()
			if cs.File == "" && trimmedFile != "" {
				cs.File = trimmedFile
			}
			if err := cs.Validate(); err != nil {
				return fmt.Errorf("fragment %q call site %d: %w", trimmedFile, j, err)
			}
			sites[j] = cs
		}

		prepFragments[i] = FileFragment{
			File:      trimmedFile,
			Nodes:     nodes,
			Edges:     edges,
			CallSites: sites,
		}
		totalNodes += len(nodes)
		totalEdges += len(edges)
		totalSites += len(sites)
	}

	fragUIDs := make(map[UID]struct{}, totalNodes)
	for i := range prepFragments {
		for j := range prepFragments[i].Nodes {
			fragUIDs[prepFragments[i].Nodes[j].UID] = struct{}{}
		}
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if !g.init {
		g.initWithCapacityLocked(totalNodes, totalEdges)
	}

	endpointExists := func(uid UID) bool {
		if _, ok := fragUIDs[uid]; ok {
			return true
		}
		_, ok := g.uidToID[uid]
		return ok
	}
	for i := range prepFragments {
		for j := range prepFragments[i].Edges {
			e := &prepFragments[i].Edges[j]
			if !endpointExists(e.Source) {
				return fmt.Errorf("%w: fragment %q edge %d source %s", ErrNodeNotFound, prepFragments[i].File, j, e.Source)
			}
			if !endpointExists(e.Target) {
				return fmt.Errorf("%w: fragment %q edge %d target %s", ErrNodeNotFound, prepFragments[i].File, j, e.Target)
			}
		}
	}

	// Pre-allocate node, adjacency, edge & call-site slice capacity.
	if cap(g.nodes)-len(g.nodes) < totalNodes {
		grownNodes := make([]Node, len(g.nodes), len(g.nodes)+totalNodes)
		copy(grownNodes, g.nodes)
		g.nodes = grownNodes

		grownOut := make([][]uint32, len(g.outgoing), len(g.outgoing)+totalNodes)
		copy(grownOut, g.outgoing)
		g.outgoing = grownOut

		grownIn := make([][]uint32, len(g.incoming), len(g.incoming)+totalNodes)
		copy(grownIn, g.incoming)
		g.incoming = grownIn
	}
	if cap(g.edges)-len(g.edges) < totalEdges {
		grownEdges := make([]edgeEntry, len(g.edges), len(g.edges)+totalEdges)
		copy(grownEdges, g.edges)
		g.edges = grownEdges
	}
	if cap(g.callSites)-len(g.callSites) < totalSites {
		grownSites := make([]CallSite, len(g.callSites), len(g.callSites)+totalSites)
		copy(grownSites, g.callSites)
		g.callSites = grownSites
	}

	// Insert all nodes first so cross-file edges can resolve both endpoints cleanly.
	for i := range prepFragments {
		for j := range prepFragments[i].Nodes {
			n := prepFragments[i].Nodes[j]
			if existingID, exists := g.uidToID[n.UID]; exists {
				g.updateExistingNodeLocked(existingID, n)
			} else {
				g.insertNewNodeLocked(n)
			}
		}
	}

	// Insert all edges.
	for i := range prepFragments {
		for j := range prepFragments[i].Edges {
			g.upsertEdgeLocked(prepFragments[i].Edges[j])
		}
	}

	// Insert all call sites.
	for i := range prepFragments {
		for j := range prepFragments[i].CallSites {
			g.addCallSiteLocked(prepFragments[i].CallSites[j])
		}
	}

	return nil
}

// BulkLoad validates and inserts a complete set of nodes, edges, and call sites
// (such as from a deserialized snapshot) without requiring per-file fragment grouping.
func (g *Graph) BulkLoad(nodes []Node, edges []Edge, sites []CallSite) error {
	nodeUIDs := make(map[UID]struct{}, len(nodes))
	prepNodes := make([]Node, len(nodes))
	for i := range nodes {
		n := nodes[i].Clone()
		if err := n.Validate(); err != nil {
			return fmt.Errorf("node %d: %w", i, err)
		}
		if n.UID.IsZero() {
			n.UID = ComputeNodeUID(n)
		}
		nodeUIDs[n.UID] = struct{}{}
		prepNodes[i] = n
	}

	for i := range edges {
		if err := edges[i].Validate(); err != nil {
			return fmt.Errorf("edge %d: %w", i, err)
		}
	}
	for i := range sites {
		if err := sites[i].Validate(); err != nil {
			return fmt.Errorf("call site %d: %w", i, err)
		}
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if !g.init {
		g.initWithCapacityLocked(len(prepNodes), len(edges))
	}

	endpointExists := func(uid UID) bool {
		if _, ok := nodeUIDs[uid]; ok {
			return true
		}
		_, ok := g.uidToID[uid]
		return ok
	}
	for i := range edges {
		e := &edges[i]
		if !endpointExists(e.Source) {
			return fmt.Errorf("%w: edge %d source %s", ErrNodeNotFound, i, e.Source)
		}
		if !endpointExists(e.Target) {
			return fmt.Errorf("%w: edge %d target %s", ErrNodeNotFound, i, e.Target)
		}
	}

	for i := range prepNodes {
		n := prepNodes[i]
		if existingID, exists := g.uidToID[n.UID]; exists {
			g.updateExistingNodeLocked(existingID, n)
		} else {
			g.insertNewNodeLocked(n)
		}
	}
	for i := range edges {
		g.upsertEdgeLocked(edges[i])
	}
	for i := range sites {
		g.addCallSiteLocked(sites[i])
	}
	return nil
}

// DenseEdge represents an edge referencing 0-based node indices into a parallel []Node slice
// (used for zero-lookup bulk loading from snapshot decoders).
// Single-site edges can populate HasPrimary + PrimarySite to avoid per-edge slice allocations.
type DenseEdge struct {
	SourceIdx   uint32
	TargetIdx   uint32
	Kind        EdgeKind
	Tier        ResolutionTier
	Label       string
	HasPrimary  bool
	PrimarySite CallSiteRef
	Sites       []CallSiteRef
}

// LoadDense validates and bulk-loads nodes, index-addressed edges, and call sites
// directly into pre-sized graph storage, eliminating per-edge UID map lookups and
// intermediate adjacency slice reallocations on empty graphs.
func (g *Graph) LoadDense(nodes []Node, edges []DenseEdge, sites []CallSite) error {
	for i := range nodes {
		if err := nodes[i].Validate(); err != nil {
			return fmt.Errorf("node %d: %w", i, err)
		}
		if nodes[i].UID.IsZero() {
			nodes[i].UID = ComputeNodeUID(nodes[i])
		}
	}

	numNodes := uint32(len(nodes))
	outDeg := make([]uint32, len(nodes))
	inDeg := make([]uint32, len(nodes))

	for i := range edges {
		e := &edges[i]
		if e.SourceIdx >= numNodes {
			return fmt.Errorf("%w: edge %d source index %d >= %d", ErrNodeNotFound, i, e.SourceIdx, numNodes)
		}
		if e.TargetIdx >= numNodes {
			return fmt.Errorf("%w: edge %d target index %d >= %d", ErrNodeNotFound, i, e.TargetIdx, numNodes)
		}
		if !e.Kind.IsValid() {
			return fmt.Errorf("%w: edge %d unknown kind %d", ErrInvalidEdge, i, e.Kind)
		}
		if e.SourceIdx == e.TargetIdx && !e.Kind.IsCall() {
			return fmt.Errorf("%w: self-loop is not allowed for %s edges", ErrInvalidEdge, e.Kind)
		}
		if err := validateTierForKind(e.Kind, e.Tier); err != nil {
			return fmt.Errorf("edge %d: %w", i, err)
		}
		if e.HasPrimary {
			if err := validateDenseSite(i, e.Kind, e.Tier, e.PrimarySite); err != nil {
				return err
			}
		}
		for _, s := range e.Sites {
			if err := validateDenseSite(i, e.Kind, e.Tier, s); err != nil {
				return err
			}
		}
		outDeg[e.SourceIdx]++
		inDeg[e.TargetIdx]++
	}

	for i := range sites {
		if err := sites[i].Validate(); err != nil {
			return fmt.Errorf("call site %d: %w", i, err)
		}
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if !g.init || g.nodeCount == 0 {
		g.initWithCapacityLocked(len(nodes), len(edges))
		g.nodes = make([]Node, len(nodes))
		g.outgoing = make([][]uint32, len(nodes))
		g.incoming = make([][]uint32, len(nodes))
		g.edges = make([]edgeEntry, 0, len(edges))

		idMap := make([]uint32, len(nodes))
		for i := range nodes {
			stored := nodes[i].Clone()
			g.internNodeStrings(&stored)
			if existingID, dup := g.uidToID[stored.UID]; dup {
				// Overwrite existing node slot on duplicate UID
				idMap[i] = existingID
				g.updateExistingNodeLocked(existingID, stored)
				continue
			}
			id := uint32(g.nodeCount)
			idMap[i] = id
			g.nodes[id] = stored
			g.uidToID[stored.UID] = id
			g.nodeCount++
			g.indexNodeLocked(id, &g.nodes[id])
			if outDeg[i] > 0 {
				g.outgoing[id] = make([]uint32, 0, outDeg[i])
			}
			if inDeg[i] > 0 {
				g.incoming[id] = make([]uint32, 0, inDeg[i])
			}
		}
		if g.nodeCount < len(g.nodes) {
			g.nodes = g.nodes[:g.nodeCount]
			g.outgoing = g.outgoing[:g.nodeCount]
			g.incoming = g.incoming[:g.nodeCount]
		}

		for i := range edges {
			e := &edges[i]
			srcID := idMap[e.SourceIdx]
			tgtID := idMap[e.TargetIdx]
			key := compactEdgeKey{source: srcID, target: tgtID, kind: e.Kind}
			if eid, exists := g.edgeToID[key]; exists {
				entry := &g.edges[eid]
				if e.Tier < entry.tier {
					entry.tier = e.Tier
					if e.Label != "" {
						entry.labelID = g.labels.intern(e.Label)
					}
				}
				if e.HasPrimary {
					g.appendUniqueSiteLocked(entry, g.toCompactSite(e.PrimarySite, e.Kind, e.Tier))
				}
				for _, s := range e.Sites {
					g.appendUniqueSiteLocked(entry, g.toCompactSite(s, e.Kind, e.Tier))
				}
				continue
			}
			g.insertDenseEdgeLocked(srcID, tgtID, e)
		}

		for i := range sites {
			g.addCallSiteLocked(sites[i])
		}
		return nil
	}

	// Non-empty graph fallback: map dense nodes/edges cleanly via normal upsert paths.
	idMap := make([]UID, len(nodes))
	for i := range nodes {
		n := nodes[i]
		idMap[i] = n.UID
		if existingID, exists := g.uidToID[n.UID]; exists {
			g.updateExistingNodeLocked(existingID, n)
		} else {
			g.insertNewNodeLocked(n)
		}
	}
	for i := range edges {
		e := &edges[i]
		var allSites []CallSiteRef
		if e.HasPrimary {
			allSites = make([]CallSiteRef, 0, 1+len(e.Sites))
			allSites = append(allSites, e.PrimarySite)
			allSites = append(allSites, e.Sites...)
		} else {
			allSites = e.Sites
		}
		g.upsertEdgeLocked(Edge{
			Source: idMap[e.SourceIdx],
			Target: idMap[e.TargetIdx],
			Kind:   e.Kind,
			Tier:   e.Tier,
			Label:  e.Label,
			Sites:  allSites,
		})
	}
	for i := range sites {
		g.addCallSiteLocked(sites[i])
	}
	return nil
}

func validateDenseSite(edgeIdx int, kind EdgeKind, edgeTier ResolutionTier, s CallSiteRef) error {
	if s.Line == 0 {
		return fmt.Errorf("%w: edge %d call site line must be > 0", ErrInvalidEdge, edgeIdx)
	}
	siteTier := s.Tier
	if siteTier == 0 && kind.IsCall() {
		siteTier = edgeTier
	}
	if err := validateTierForKind(kind, siteTier); err != nil {
		return fmt.Errorf("edge %d call site at line %d: %w", edgeIdx, s.Line, err)
	}
	return nil
}

func (g *Graph) insertDenseEdgeLocked(srcID, tgtID uint32, e *DenseEdge) {
	var eid uint32
	if numFree := len(g.freeEdgeIDs); numFree > 0 {
		eid = g.freeEdgeIDs[numFree-1]
		g.freeEdgeIDs = g.freeEdgeIDs[:numFree-1]
	} else {
		eid = uint32(len(g.edges))
		g.edges = append(g.edges, edgeEntry{})
	}
	entry := &g.edges[eid]
	*entry = edgeEntry{
		source:  srcID,
		target:  tgtID,
		kind:    e.Kind,
		tier:    e.Tier,
		labelID: g.labels.intern(e.Label),
	}
	if e.HasPrimary {
		g.appendUniqueSiteLocked(entry, g.toCompactSite(e.PrimarySite, e.Kind, e.Tier))
	}
	for _, s := range e.Sites {
		g.appendUniqueSiteLocked(entry, g.toCompactSite(s, e.Kind, e.Tier))
	}
	g.edgeToID[compactEdgeKey{source: srcID, target: tgtID, kind: e.Kind}] = eid
	g.outgoing[srcID] = append(g.outgoing[srcID], eid)
	g.incoming[tgtID] = append(g.incoming[tgtID], eid)
	g.edgeCount++
}
