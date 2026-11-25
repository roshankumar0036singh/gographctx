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
