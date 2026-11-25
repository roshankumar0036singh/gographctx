package graph

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"sync"
)

var (
	ErrNilNode      = errors.New("graph: nil node")
	ErrInvalidNode  = errors.New("graph: invalid node")
	ErrNodeExists   = errors.New("graph: node already exists")
	ErrNodeNotFound = errors.New("graph: node not found")
	ErrNilEdge      = errors.New("graph: nil edge")
	ErrInvalidEdge  = errors.New("graph: invalid edge")
	ErrEdgeExists   = errors.New("graph: edge already exists")
	ErrEdgeNotFound = errors.New("graph: edge not found")
)

type edgeKey struct {
	source UID
	target UID
	kind   EdgeKind
}

// Graph is the in-memory code graph holding nodes, edges, and secondary indexes.
// All exported methods are safe for concurrent use.
type Graph struct {
	mu sync.RWMutex

	nodes map[UID]*Node
	edges map[edgeKey]*Edge

	byName   map[string][]*Node
	byFile   map[string][]*Node
	byQName  map[string][]*Node
	callers  map[UID][]*Edge
	callees  map[UID][]*Edge
	incoming map[UID][]*Edge
	outgoing map[UID][]*Edge
	contains map[UID][]*Node
	revDeps  map[UID]map[UID]struct{}
}

// New creates an initialized, empty Graph.
func New() *Graph {
	g := &Graph{}
	g.initLocked()
	return g
}

func (g *Graph) initLocked() {
	g.nodes = make(map[UID]*Node)
	g.edges = make(map[edgeKey]*Edge)
	g.initIndexesLocked()
}

// Clear removes all nodes, edges, and index entries from the graph.
func (g *Graph) Clear() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.initLocked()
}

// AddNode inserts a new node into the graph and updates all indexes.
// If n.UID is zero, a stable UID is computed and assigned to n.UID.
// Returns ErrNodeExists if a node with the same UID is already present.
func (g *Graph) AddNode(n *Node) error {
	if err := n.Validate(); err != nil {
		return err
	}
	if n.UID.IsZero() {
		n.UID = ComputeNodeUID(n)
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if _, exists := g.nodes[n.UID]; exists {
		return fmt.Errorf("%w: %s", ErrNodeExists, n.UID)
	}

	stored := n.Clone()
	g.nodes[stored.UID] = stored
	g.indexNodeLocked(stored)
	return nil
}

// UpsertNode inserts a node or replaces an existing node with the same UID,
// keeping all existing incident edges and updating all indexes consistently.
func (g *Graph) UpsertNode(n *Node) error {
	if err := n.Validate(); err != nil {
		return err
	}
	if n.UID.IsZero() {
		n.UID = ComputeNodeUID(n)
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	stored := n.Clone()
	if old, exists := g.nodes[stored.UID]; exists {
		g.unindexNodeLocked(old)
		g.nodes[stored.UID] = stored
		g.indexNodeLocked(stored)
		g.updateContainsNodePtrLocked(stored.UID, old, stored)
		return nil
	}

	g.nodes[stored.UID] = stored
	g.indexNodeLocked(stored)
	return nil
}

// GetNode returns the node with the given UID, if present.
// Callers must not mutate the returned Node.
func (g *Graph) GetNode(uid UID) (*Node, bool) {
	if uid.IsZero() {
		return nil, false
	}
	g.mu.RLock()
	defer g.mu.RUnlock()

	n, ok := g.nodes[uid]
	return n, ok
}

// HasNode reports whether a node with the given UID exists in the graph.
func (g *Graph) HasNode(uid UID) bool {
	if uid.IsZero() {
		return false
	}
	g.mu.RLock()
	defer g.mu.RUnlock()

	_, ok := g.nodes[uid]
	return ok
}

// RemoveNode removes the node with the given UID, all incident edges, and all index entries.
func (g *Graph) RemoveNode(uid UID) error {
	if uid.IsZero() {
		return ErrNodeNotFound
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	return g.removeNodeLocked(uid)
}

func (g *Graph) removeNodeLocked(uid UID) error {
	node, exists := g.nodes[uid]
	if !exists {
		return fmt.Errorf("%w: %s", ErrNodeNotFound, uid)
	}

	outEdges := slices.Clone(g.outgoing[uid])
	for _, e := range outEdges {
		key := edgeKey{source: e.Source, target: e.Target, kind: e.Kind}
		if _, ok := g.edges[key]; ok {
			delete(g.edges, key)
			g.unindexEdgeLocked(e)
		}
	}

	inEdges := slices.Clone(g.incoming[uid])
	for _, e := range inEdges {
		key := edgeKey{source: e.Source, target: e.Target, kind: e.Kind}
		if _, ok := g.edges[key]; ok {
			delete(g.edges, key)
			g.unindexEdgeLocked(e)
		}
	}

	g.unindexNodeLocked(node)
	delete(g.nodes, uid)
	return nil
}

// RemoveFile removes all nodes associated with the given file path and their incident edges.
// Returns the number of nodes removed.
func (g *Graph) RemoveFile(file string) int {
	if file == "" {
		return 0
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	nodes := slices.Clone(g.byFile[file])
	for _, n := range nodes {
		_ = g.removeNodeLocked(n.UID)
	}
	return len(nodes)
}

// AllNodes returns all nodes in the graph sorted deterministically by UID.
func (g *Graph) AllNodes() []*Node {
	g.mu.RLock()
	defer g.mu.RUnlock()

	result := make([]*Node, 0, len(g.nodes))
	for _, n := range g.nodes {
		result = append(result, n)
	}
	slices.SortFunc(result, func(a, b *Node) int {
		return a.UID.Compare(b.UID)
	})
	return result
}

// NodeCount returns the number of nodes in the graph.
func (g *Graph) NodeCount() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.nodes)
}

// AddEdge inserts a new directed edge between two existing nodes.
// Returns ErrNodeNotFound if either endpoint is missing, or ErrEdgeExists on duplicate (Source, Target, Kind).
func (g *Graph) AddEdge(e *Edge) error {
	if err := e.Validate(); err != nil {
		return err
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if _, ok := g.nodes[e.Source]; !ok {
		return fmt.Errorf("%w: source %s", ErrNodeNotFound, e.Source)
	}
	if _, ok := g.nodes[e.Target]; !ok {
		return fmt.Errorf("%w: target %s", ErrNodeNotFound, e.Target)
	}

	key := edgeKey{source: e.Source, target: e.Target, kind: e.Kind}
	if _, exists := g.edges[key]; exists {
		return fmt.Errorf("%w: %s -> %s (%s)", ErrEdgeExists, e.Source, e.Target, e.Kind)
	}

	stored := e.Clone()
	g.edges[key] = stored
	g.indexEdgeLocked(stored)
	return nil
}

// UpsertEdge inserts an edge or replaces an existing edge with the same (Source, Target, Kind).
func (g *Graph) UpsertEdge(e *Edge) error {
	if err := e.Validate(); err != nil {
		return err
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if _, ok := g.nodes[e.Source]; !ok {
		return fmt.Errorf("%w: source %s", ErrNodeNotFound, e.Source)
	}
	if _, ok := g.nodes[e.Target]; !ok {
		return fmt.Errorf("%w: target %s", ErrNodeNotFound, e.Target)
	}

	key := edgeKey{source: e.Source, target: e.Target, kind: e.Kind}
	stored := e.Clone()
	if old, exists := g.edges[key]; exists {
		g.edges[key] = stored
		g.replaceEdgePtrLocked(old, stored)
		return nil
	}

	g.edges[key] = stored
	g.indexEdgeLocked(stored)
	return nil
}

// GetEdge returns the edge matching (source, target, kind), if present.
// Callers must not mutate the returned Edge.
func (g *Graph) GetEdge(source, target UID, kind EdgeKind) (*Edge, bool) {
	if source.IsZero() || target.IsZero() || !kind.IsValid() {
		return nil, false
	}

	g.mu.RLock()
	defer g.mu.RUnlock()

	e, ok := g.edges[edgeKey{source: source, target: target, kind: kind}]
	return e, ok
}

// HasEdge reports whether an edge matching (source, target, kind) exists.
func (g *Graph) HasEdge(source, target UID, kind EdgeKind) bool {
	_, ok := g.GetEdge(source, target, kind)
	return ok
}

// RemoveEdge deletes the edge matching (source, target, kind) and updates all indexes.
func (g *Graph) RemoveEdge(source, target UID, kind EdgeKind) error {
	if source.IsZero() || target.IsZero() || !kind.IsValid() {
		return ErrEdgeNotFound
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	key := edgeKey{source: source, target: target, kind: kind}
	e, exists := g.edges[key]
	if !exists {
		return fmt.Errorf("%w: %s -> %s (%s)", ErrEdgeNotFound, source, target, kind)
	}

	delete(g.edges, key)
	g.unindexEdgeLocked(e)
	return nil
}

// AllEdges returns all edges in the graph sorted deterministically by (Source, Target, Kind).
func (g *Graph) AllEdges() []*Edge {
	g.mu.RLock()
	defer g.mu.RUnlock()

	result := make([]*Edge, 0, len(g.edges))
	for _, e := range g.edges {
		result = append(result, e)
	}
	slices.SortFunc(result, compareEdgesCanonical)
	return result
}

// EdgeCount returns the number of edges in the graph.
func (g *Graph) EdgeCount() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.edges)
}

func compareEdgesCanonical(a, b *Edge) int {
	if c := a.Source.Compare(b.Source); c != 0 {
		return c
	}
	if c := a.Target.Compare(b.Target); c != 0 {
		return c
	}
	return cmp.Compare(a.Kind, b.Kind)
}

