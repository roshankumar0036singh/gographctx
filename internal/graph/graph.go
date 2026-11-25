package graph

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
)

var (
	ErrNilNode         = errors.New("graph: nil node")
	ErrInvalidNode     = errors.New("graph: invalid node")
	ErrNodeExists      = errors.New("graph: node already exists")
	ErrNodeNotFound    = errors.New("graph: node not found")
	ErrNilEdge         = errors.New("graph: nil edge")
	ErrInvalidEdge     = errors.New("graph: invalid edge")
	ErrEdgeExists      = errors.New("graph: edge already exists")
	ErrEdgeNotFound    = errors.New("graph: edge not found")
	ErrInvalidCallSite = errors.New("graph: invalid call site")
	ErrAmbiguousQName  = errors.New("graph: ambiguous qualified name")
)

type compactEdgeKey struct {
	source uint32
	target uint32
	kind   EdgeKind
}

type compactSite struct {
	line    uint32
	column  uint16
	tier    ResolutionTier
	labelID uint32
}

type edgeEntry struct {
	source      uint32
	target      uint32
	kind        EdgeKind
	tier        ResolutionTier
	hasPrimary  bool
	labelID     uint32
	primarySite compactSite
	extraSites  []compactSite
}

func (e *edgeEntry) alive() bool {
	return e.kind != EdgeUnknown
}

// idList stores node IDs without a heap slice allocation for singleton entries.
type idList struct {
	hasFirst bool
	first    uint32
	rest     []uint32
}

func (l *idList) len() int {
	if !l.hasFirst {
		return 0
	}
	return 1 + len(l.rest)
}

func (l *idList) add(id uint32) {
	if !l.hasFirst {
		l.hasFirst = true
		l.first = id
		return
	}
	l.rest = append(l.rest, id)
}

func (l *idList) remove(id uint32) (empty bool) {
	if !l.hasFirst {
		return true
	}
	if l.first == id {
		n := len(l.rest)
		if n == 0 {
			l.hasFirst = false
			l.first = 0
			return true
		}
		l.first = l.rest[n-1]
		l.rest = l.rest[:n-1]
		return false
	}
	for i, v := range l.rest {
		if v == id {
			last := len(l.rest) - 1
			l.rest[i] = l.rest[last]
			l.rest = l.rest[:last]
			return false
		}
	}
	return false
}

func (l *idList) appendTo(dst []uint32) []uint32 {
	if !l.hasFirst {
		return dst
	}
	dst = append(dst, l.first)
	return append(dst, l.rest...)
}

// Graph is the in-memory code graph holding compact nodes, edges, call sites, and secondary indexes.
// All exported methods are safe for concurrent use. A zero-value Graph is safe to use without
// calling New(), though New() and NewWithCapacity() avoid initial map growth.
type Graph struct {
	mu   sync.RWMutex
	init bool

	// Dense node storage with free-list reuse.
	nodes       []Node
	nodeCount   int
	freeNodeIDs []uint32
	uidToID     map[UID]uint32

	// Dense edge storage with free-list reuse.
	edges       []edgeEntry
	edgeCount   int
	freeEdgeIDs []uint32
	edgeToID    map[compactEdgeKey]uint32

	// Adjacency indexed by internal node ID (no separate callers/callees/contains/revDeps maps).
	outgoing [][]uint32
	incoming [][]uint32

	// Secondary node indexes keyed to internal node IDs.
	byName  map[string]idList
	byFile  map[string]idList
	byQName map[string]idList

	// Unresolved, ambiguous, and external call sites + negative-dependency lookup index.
	callSites         []CallSite
	callSitesByCaller map[UID][]int
	callSitesByFile   map[string][]int
	callSitesByLookup map[string][]int

	// String and label deduplication pools.
	strings *stringPool
	labels  *labelPool
}

// New creates an initialized, empty Graph.
func New() *Graph {
	g := &Graph{}
	g.ensureInitLocked()
	return g
}

// NewWithCapacity creates a Graph pre-sized for the given approximate node and edge counts.
func NewWithCapacity(nodes, edges int) *Graph {
	g := &Graph{}
	g.initWithCapacityLocked(nodes, edges)
	return g
}

func (g *Graph) initWithCapacityLocked(nodes, edges int) {
	if nodes < 0 {
		nodes = 0
	}
	if edges < 0 {
		edges = 0
	}
	g.nodes = make([]Node, 0, nodes)
	g.nodeCount = 0
	g.freeNodeIDs = nil
	g.uidToID = make(map[UID]uint32, nodes)

	g.edges = make([]edgeEntry, 0, edges)
	g.edgeCount = 0
	g.freeEdgeIDs = nil
	g.edgeToID = make(map[compactEdgeKey]uint32, edges)

	g.outgoing = make([][]uint32, 0, nodes)
	g.incoming = make([][]uint32, 0, nodes)

	g.byName = make(map[string]idList, nodes)
	g.byFile = make(map[string]idList, nodes/10+1)
	g.byQName = make(map[string]idList, nodes)

	g.callSites = nil
	g.callSitesByCaller = make(map[UID][]int)
	g.callSitesByFile = make(map[string][]int)
	g.callSitesByLookup = make(map[string][]int)

	g.strings = newStringPool()
	g.labels = newLabelPool()
	g.init = true
}

func (g *Graph) ensureInitLocked() {
	if g.init {
		return
	}
	g.nodes = nil
	g.nodeCount = 0
	g.freeNodeIDs = nil
	g.uidToID = make(map[UID]uint32)

	g.edges = nil
	g.edgeCount = 0
	g.freeEdgeIDs = nil
	g.edgeToID = make(map[compactEdgeKey]uint32)

	g.outgoing = nil
	g.incoming = nil

	g.callSites = nil
	g.callSitesByCaller = make(map[UID][]int)
	g.callSitesByFile = make(map[string][]int)
	g.callSitesByLookup = make(map[string][]int)

	g.strings = newStringPool()
	g.labels = newLabelPool()

	g.initIndexesLocked()
	g.init = true
}

// Clear removes all nodes, edges, call sites, and index entries from the graph.
func (g *Graph) Clear() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.init = false
	g.ensureInitLocked()
}

// ---------- Node CRUD ----------

// AddNode inserts a new node into the graph and updates all indexes.
// If n.UID is zero, a stable UID is computed from (Kind, QName, Occurrence).
// Returns the assigned UID, or ErrNodeExists (including existing File:StartLine) on collision.
func (g *Graph) AddNode(n Node) (UID, error) {
	if err := n.Validate(); err != nil {
		return ZeroUID, err
	}
	if n.UID.IsZero() {
		n.UID = ComputeNodeUID(n)
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.ensureInitLocked()

	if existingID, exists := g.uidToID[n.UID]; exists {
		existing := &g.nodes[existingID]
		return n.UID, fmt.Errorf("%w: %s (existing: %s:%d)", ErrNodeExists, n.UID, existing.File, existing.StartLine)
	}

	g.insertNewNodeLocked(n)
	return n.UID, nil
}

// UpsertNode inserts a node or replaces an existing node with the same UID,
// keeping all existing incident edges and updating all indexes consistently.
func (g *Graph) UpsertNode(n Node) (UID, error) {
	if err := n.Validate(); err != nil {
		return ZeroUID, err
	}
	if n.UID.IsZero() {
		n.UID = ComputeNodeUID(n)
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.ensureInitLocked()

	if existingID, exists := g.uidToID[n.UID]; exists {
		g.updateExistingNodeLocked(existingID, n)
		return n.UID, nil
	}

	g.insertNewNodeLocked(n)
	return n.UID, nil
}

func (g *Graph) insertNewNodeLocked(n Node) uint32 {
	stored := n.Clone()
	g.internNodeStrings(&stored)

	var id uint32
	if numFree := len(g.freeNodeIDs); numFree > 0 {
		id = g.freeNodeIDs[numFree-1]
		g.freeNodeIDs = g.freeNodeIDs[:numFree-1]
		g.nodes[id] = stored
		g.outgoing[id] = g.outgoing[id][:0]
		g.incoming[id] = g.incoming[id][:0]
	} else {
		id = uint32(len(g.nodes))
		g.nodes = append(g.nodes, stored)
		g.outgoing = append(g.outgoing, nil)
		g.incoming = append(g.incoming, nil)
	}

	g.uidToID[stored.UID] = id
	g.nodeCount++
	g.indexNodeLocked(id, &g.nodes[id])
	return id
}

func (g *Graph) updateExistingNodeLocked(id uint32, n Node) {
	stored := n.Clone()
	g.internNodeStrings(&stored)

	old := &g.nodes[id]
	g.unindexNodeLocked(id, old)
	g.nodes[id] = stored
	g.indexNodeLocked(id, &g.nodes[id])
}

// GetNode returns a safe copy of the node with the given UID, if present.
func (g *Graph) GetNode(uid UID) (Node, bool) {
	if uid.IsZero() {
		return Node{}, false
	}
	g.mu.RLock()
	defer g.mu.RUnlock()

	return g.getNodeLocked(uid)
}

func (g *Graph) getNodeLocked(uid UID) (Node, bool) {
	if !g.init || uid.IsZero() {
		return Node{}, false
	}
	id, ok := g.uidToID[uid]
	if !ok {
		return Node{}, false
	}
	return g.nodes[id].Clone(), true
}

// HasNode reports whether a node with the given UID exists in the graph.
func (g *Graph) HasNode(uid UID) bool {
	if uid.IsZero() {
		return false
	}
	g.mu.RLock()
	defer g.mu.RUnlock()

	return g.hasNodeLocked(uid)
}

func (g *Graph) hasNodeLocked(uid UID) bool {
	if !g.init || uid.IsZero() {
		return false
	}
	_, ok := g.uidToID[uid]
	return ok
}

// RemoveNode removes the node with the given UID, all incident edges, and all index entries in O(d).
func (g *Graph) RemoveNode(uid UID) error {
	if uid.IsZero() {
		return ErrNodeNotFound
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.ensureInitLocked()

	id, ok := g.uidToID[uid]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNodeNotFound, uid)
	}
	g.removeNodeByIDLocked(id)
	return nil
}

// removeNodeByIDLocked deletes a node and its incident edges in O(d) by dropping
// incoming[id] and outgoing[id] wholesale and unindexing only the opposite endpoints.
func (g *Graph) removeNodeByIDLocked(id uint32) {
	node := &g.nodes[id]

	for _, eid := range g.outgoing[id] {
		e := &g.edges[eid]
		if !e.alive() {
			continue
		}
		if e.target != id {
			g.incoming[e.target] = swapRemoveUint32(g.incoming[e.target], eid)
		}
		g.deleteEdgeSlotLocked(eid, e)
	}
	g.outgoing[id] = nil

	for _, eid := range g.incoming[id] {
		e := &g.edges[eid]
		if !e.alive() {
			continue
		}
		g.outgoing[e.source] = swapRemoveUint32(g.outgoing[e.source], eid)
		g.deleteEdgeSlotLocked(eid, e)
	}
	g.incoming[id] = nil

	g.unindexNodeLocked(id, node)
	delete(g.uidToID, node.UID)
	g.nodes[id] = Node{}
	g.freeNodeIDs = append(g.freeNodeIDs, id)
	g.nodeCount--
}

// RemoveFile removes all nodes associated with the given file path, their incident edges,
// and call sites recorded in that file. Returns the number of nodes removed.
func (g *Graph) RemoveFile(file string) int {
	if file == "" {
		return 0
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.ensureInitLocked()

	ids := g.collectIDsFromList(g.byFile[file])
	if len(ids) > 0 {
		g.removeNodesBatchLocked(ids)
	}
	g.removeCallSitesForFileLocked(file)
	return len(ids)
}

func (g *Graph) removeNodesBatchLocked(ids []uint32) {
	if len(ids) == 0 {
		return
	}
	if len(ids) == 1 {
		g.removeNodeByIDLocked(ids[0])
		return
	}

	deadNodes := make(map[uint32]struct{}, len(ids))
	for _, id := range ids {
		deadNodes[id] = struct{}{}
	}

	affectedOut := make(map[uint32]struct{})
	affectedIn := make(map[uint32]struct{})

	for _, id := range ids {
		for _, eid := range g.outgoing[id] {
			e := &g.edges[eid]
			if !e.alive() {
				continue
			}
			if _, isDeadTarget := deadNodes[e.target]; !isDeadTarget {
				affectedIn[e.target] = struct{}{}
			}
			g.deleteEdgeSlotLocked(eid, e)
		}
		g.outgoing[id] = nil

		for _, eid := range g.incoming[id] {
			e := &g.edges[eid]
			if !e.alive() {
				continue
			}
			if _, isDeadSrc := deadNodes[e.source]; !isDeadSrc {
				affectedOut[e.source] = struct{}{}
			}
			g.deleteEdgeSlotLocked(eid, e)
		}
		g.incoming[id] = nil

		node := &g.nodes[id]
		g.unindexNodeLocked(id, node)
		delete(g.uidToID, node.UID)
		g.nodes[id] = Node{}
		g.freeNodeIDs = append(g.freeNodeIDs, id)
		g.nodeCount--
	}

	for nid := range affectedIn {
		g.incoming[nid] = filterAliveEdges(g.incoming[nid], g.edges)
	}
	for nid := range affectedOut {
		g.outgoing[nid] = filterAliveEdges(g.outgoing[nid], g.edges)
	}
}

func filterAliveEdges(list []uint32, edges []edgeEntry) []uint32 {
	out := list[:0]
	for _, eid := range list {
		if edges[eid].alive() {
			out = append(out, eid)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ReplaceResult holds the outcome of an atomic ReplaceFile operation.
type ReplaceResult struct {
	Added                        int
	Updated                      int
	Removed                      int
	SignatureChanged             []UID
	LostCallerUIDs               []UID
	AffectedFiles                []string
	AffectedLookupKeys           []string
	PotentiallyResolvedCallSites []CallSite
}

// ReplaceFile atomically replaces all nodes, outgoing edges, and call sites for a file under one write lock.
// Incoming dependency edges from other files targeting surviving UIDs are preserved.
// Vanished nodes' external dependents are collected prior to deletion and returned in ReplaceResult.
func (g *Graph) ReplaceFile(file string, newNodes []Node, newEdges []Edge, newSites []CallSite) (ReplaceResult, error) {
	trimmedFile := strings.TrimSpace(file)
	if trimmedFile == "" {
		return ReplaceResult{}, fmt.Errorf("%w: empty file path", ErrInvalidNode)
	}

	// Prepare and validate caller inputs without mutating caller slices.
	prepNodes := make([]Node, len(newNodes))
	newNodeUIDs := make(map[UID]struct{}, len(newNodes))
	for i := range newNodes {
		n := newNodes[i].Clone()
		if n.File == "" {
			n.File = trimmedFile
		} else if n.File != trimmedFile {
			return ReplaceResult{}, fmt.Errorf("%w: node %q belongs to file %q, expected %q", ErrInvalidNode, n.Name, n.File, trimmedFile)
		}
		if err := n.Validate(); err != nil {
			return ReplaceResult{}, fmt.Errorf("node %d: %w", i, err)
		}
		if n.UID.IsZero() {
			n.UID = ComputeNodeUID(n)
		}
		if _, dup := newNodeUIDs[n.UID]; dup {
			return ReplaceResult{}, fmt.Errorf("%w: duplicate UID %s in ReplaceFile(%q)", ErrNodeExists, n.UID, trimmedFile)
		}
		newNodeUIDs[n.UID] = struct{}{}
		prepNodes[i] = n
	}

	for i := range newEdges {
		if err := newEdges[i].Validate(); err != nil {
			return ReplaceResult{}, fmt.Errorf("edge %d: %w", i, err)
		}
	}
	for i := range newSites {
		if err := newSites[i].Validate(); err != nil {
			return ReplaceResult{}, fmt.Errorf("call site %d: %w", i, err)
		}
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.ensureInitLocked()

	// Collect existing nodes for this file.
	oldIDs := g.collectIDsFromList(g.byFile[trimmedFile])
	oldUIDSet := make(map[UID]uint32, len(oldIDs))
	vanishedIDs := make([]uint32, 0)
	for _, id := range oldIDs {
		uid := g.nodes[id].UID
		oldUIDSet[uid] = id
		if _, keeps := newNodeUIDs[uid]; !keeps {
			vanishedIDs = append(vanishedIDs, id)
		}
	}

	// Atomically verify all newEdges endpoints will exist after replacement before mutating.
	vanishedUIDSet := make(map[UID]struct{}, len(vanishedIDs))
	for _, vid := range vanishedIDs {
		vanishedUIDSet[g.nodes[vid].UID] = struct{}{}
	}
	endpointExists := func(uid UID) bool {
		if _, ok := newNodeUIDs[uid]; ok {
			return true
		}
		if _, gone := vanishedUIDSet[uid]; gone {
			return false
		}
		_, ok := g.uidToID[uid]
		return ok
	}
	for i := range newEdges {
		e := &newEdges[i]
		if !endpointExists(e.Source) {
			return ReplaceResult{}, fmt.Errorf("%w: edge %d source %s", ErrNodeNotFound, i, e.Source)
		}
		if !endpointExists(e.Target) {
			return ReplaceResult{}, fmt.Errorf("%w: edge %d target %s", ErrNodeNotFound, i, e.Target)
		}
	}

	var result ReplaceResult
	lostCallers := make(map[UID]struct{})
	affectedFiles := make(map[string]struct{})

	// Phase 1: Collect lost dependents for vanished nodes BEFORE removing anything.
	for _, vid := range vanishedIDs {
		for _, eid := range g.incoming[vid] {
			e := &g.edges[eid]
			if !e.alive() || !e.kind.IsDependency() || e.source == vid {
				continue
			}
			srcNode := &g.nodes[e.source]
			if _, inSameReplacedFile := oldUIDSet[srcNode.UID]; inSameReplacedFile {
				continue
			}
			lostCallers[srcNode.UID] = struct{}{}
			if srcNode.File != "" {
				affectedFiles[srcNode.File] = struct{}{}
			}
		}
	}

	// Phase 2: Detect signature changes on survivors BEFORE removing/overwriting.
	for i := range prepNodes {
		n := &prepNodes[i]
		if oldID, exists := oldUIDSet[n.UID]; exists {
			oldNode := &g.nodes[oldID]
			if oldNode.SignatureHash != n.SignatureHash {
				result.SignatureChanged = append(result.SignatureChanged, n.UID)
				for _, eid := range g.incoming[oldID] {
					e := &g.edges[eid]
					if e.alive() && e.kind.IsDependency() && e.source != oldID {
						if f := g.nodes[e.source].File; f != "" && f != trimmedFile {
							affectedFiles[f] = struct{}{}
						}
					}
				}
			}
		}
	}

	// Phase 3: Remove all old outgoing edges from all old nodes of this file
	// (preserves incoming dependency edges from other files onto survivors!).
	if len(oldIDs) > 0 {
		g.removeOutgoingEdgesBatchLocked(oldIDs)
	}

	// Phase 4: Remove vanished nodes (and their incoming edges).
	if len(vanishedIDs) > 0 {
		g.removeNodesBatchLocked(vanishedIDs)
		result.Removed = len(vanishedIDs)
	}

	// Phase 5: Upsert survivors and insert brand new nodes.
	var addedNodes []*Node
	for i := range prepNodes {
		n := prepNodes[i]
		if existingID, exists := g.uidToID[n.UID]; exists {
			g.updateExistingNodeLocked(existingID, n)
			result.Updated++
		} else {
			newID := g.insertNewNodeLocked(n)
			addedNodes = append(addedNodes, &g.nodes[newID])
			result.Added++
		}
	}

	// Phase 6: Insert/merge new edges.
	for i := range newEdges {
		g.upsertEdgeLocked(newEdges[i])
	}

	// Phase 7: Replace call sites for this file.
	g.removeCallSitesForFileLocked(trimmedFile)
	for i := range newSites {
		cs := newSites[i]
		if cs.File == "" {
			cs.File = trimmedFile
		}
		g.addCallSiteLocked(cs)
	}

	// Phase 8: Negative dependency check — did newly added symbols match any existing unresolved/ambiguous CallSites?
	affectedKeys := make(map[string]struct{})
	seenCallSites := make(map[int]struct{})
	for _, added := range addedNodes {
		for _, key := range []string{added.QName, added.Name} {
			if key == "" {
				continue
			}
			indices := g.callSitesByLookup[key]
			if len(indices) == 0 {
				continue
			}
			affectedKeys[key] = struct{}{}
			for _, idx := range indices {
				if idx < len(g.callSites) && !g.callSites[idx].Caller.IsZero() {
					cs := &g.callSites[idx]
					if cs.File != "" && cs.File != trimmedFile {
						affectedFiles[cs.File] = struct{}{}
					}
					if _, seen := seenCallSites[idx]; !seen {
						seenCallSites[idx] = struct{}{}
						result.PotentiallyResolvedCallSites = append(result.PotentiallyResolvedCallSites, cs.Clone())
					}
				}
			}
		}
	}

	// Also include lookup keys from lost callers' call sites.
	for uid := range lostCallers {
		result.LostCallerUIDs = append(result.LostCallerUIDs, uid)
		for _, idx := range g.callSitesByCaller[uid] {
			if idx < len(g.callSites) && g.callSites[idx].LookupKey != "" {
				affectedKeys[g.callSites[idx].LookupKey] = struct{}{}
			}
		}
	}
	slices.SortFunc(result.LostCallerUIDs, func(a, b UID) int { return a.Compare(b) })

	for f := range affectedFiles {
		result.AffectedFiles = append(result.AffectedFiles, f)
	}
	slices.Sort(result.AffectedFiles)

	for k := range affectedKeys {
		result.AffectedLookupKeys = append(result.AffectedLookupKeys, k)
	}
	slices.Sort(result.AffectedLookupKeys)

	return result, nil
}

func (g *Graph) removeOutgoingEdgesBatchLocked(sourceIDs []uint32) {
	affectedTargets := make(map[uint32]struct{})
	for _, sid := range sourceIDs {
		for _, eid := range g.outgoing[sid] {
			e := &g.edges[eid]
			if !e.alive() {
				continue
			}
			affectedTargets[e.target] = struct{}{}
			g.deleteEdgeSlotLocked(eid, e)
		}
		g.outgoing[sid] = nil
	}
	for tid := range affectedTargets {
		g.incoming[tid] = filterAliveEdges(g.incoming[tid], g.edges)
	}
}

// ---------- Edge CRUD ----------

// AddEdge inserts a new directed edge between two existing nodes.
// Returns ErrNodeNotFound if either endpoint is missing, or ErrEdgeExists on duplicate (Source, Target, Kind).
func (g *Graph) AddEdge(e Edge) error {
	if err := e.Validate(); err != nil {
		return err
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.ensureInitLocked()

	srcID, ok := g.uidToID[e.Source]
	if !ok {
		return fmt.Errorf("%w: source %s", ErrNodeNotFound, e.Source)
	}
	tgtID, ok := g.uidToID[e.Target]
	if !ok {
		return fmt.Errorf("%w: target %s", ErrNodeNotFound, e.Target)
	}

	key := compactEdgeKey{source: srcID, target: tgtID, kind: e.Kind}
	if _, exists := g.edgeToID[key]; exists {
		return fmt.Errorf("%w: %s -> %s (%s)", ErrEdgeExists, e.Source, e.Target, e.Kind)
	}

	g.insertNewEdgeLocked(srcID, tgtID, e)
	return nil
}

// UpsertEdge inserts an edge or merges with an existing edge sharing (Source, Target, Kind).
// When merging, distinct call sites are appended and the aggregate Tier is upgraded to the strongest (lowest).
func (g *Graph) UpsertEdge(e Edge) error {
	if err := e.Validate(); err != nil {
		return err
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.ensureInitLocked()

	if _, ok := g.uidToID[e.Source]; !ok {
		return fmt.Errorf("%w: source %s", ErrNodeNotFound, e.Source)
	}
	if _, ok := g.uidToID[e.Target]; !ok {
		return fmt.Errorf("%w: target %s", ErrNodeNotFound, e.Target)
	}

	g.upsertEdgeLocked(e)
	return nil
}

func (g *Graph) upsertEdgeLocked(e Edge) {
	srcID := g.uidToID[e.Source]
	tgtID := g.uidToID[e.Target]
	key := compactEdgeKey{source: srcID, target: tgtID, kind: e.Kind}

	eid, exists := g.edgeToID[key]
	if !exists {
		g.insertNewEdgeLocked(srcID, tgtID, e)
		return
	}

	entry := &g.edges[eid]
	if e.Tier < entry.tier {
		entry.tier = e.Tier
		if e.Label != "" {
			entry.labelID = g.labels.intern(e.Label)
		}
	} else if entry.labelID == 0 && e.Label != "" {
		entry.labelID = g.labels.intern(e.Label)
	}

	for _, s := range e.Sites {
		cs := g.toCompactSite(s, e.Kind, e.Tier)
		g.appendUniqueSiteLocked(entry, cs)
	}
}

// ReplaceEdge overwrites an existing edge completely (no call-site merge), or inserts it if absent.
func (g *Graph) ReplaceEdge(e Edge) error {
	if err := e.Validate(); err != nil {
		return err
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.ensureInitLocked()

	srcID, ok := g.uidToID[e.Source]
	if !ok {
		return fmt.Errorf("%w: source %s", ErrNodeNotFound, e.Source)
	}
	tgtID, ok := g.uidToID[e.Target]
	if !ok {
		return fmt.Errorf("%w: target %s", ErrNodeNotFound, e.Target)
	}

	key := compactEdgeKey{source: srcID, target: tgtID, kind: e.Kind}
	if eid, exists := g.edgeToID[key]; exists {
		g.populateEdgeEntry(&g.edges[eid], srcID, tgtID, e)
		return nil
	}

	g.insertNewEdgeLocked(srcID, tgtID, e)
	return nil
}

func (g *Graph) insertNewEdgeLocked(srcID, tgtID uint32, e Edge) uint32 {
	var eid uint32
	if numFree := len(g.freeEdgeIDs); numFree > 0 {
		eid = g.freeEdgeIDs[numFree-1]
		g.freeEdgeIDs = g.freeEdgeIDs[:numFree-1]
	} else {
		eid = uint32(len(g.edges))
		g.edges = append(g.edges, edgeEntry{})
	}

	g.populateEdgeEntry(&g.edges[eid], srcID, tgtID, e)
	g.edgeToID[compactEdgeKey{source: srcID, target: tgtID, kind: e.Kind}] = eid
	g.outgoing[srcID] = append(g.outgoing[srcID], eid)
	g.incoming[tgtID] = append(g.incoming[tgtID], eid)
	g.edgeCount++
	return eid
}

func (g *Graph) populateEdgeEntry(entry *edgeEntry, srcID, tgtID uint32, e Edge) {
	*entry = edgeEntry{
		source:  srcID,
		target:  tgtID,
		kind:    e.Kind,
		tier:    e.Tier,
		labelID: g.labels.intern(e.Label),
	}
	for _, s := range e.Sites {
		cs := g.toCompactSite(s, e.Kind, e.Tier)
		g.appendUniqueSiteLocked(entry, cs)
	}
}

func (g *Graph) toCompactSite(s CallSiteRef, kind EdgeKind, edgeTier ResolutionTier) compactSite {
	tier := s.Tier
	if tier == 0 && kind.IsCall() {
		tier = edgeTier
	}
	return compactSite{
		line:    s.Line,
		column:  s.Column,
		tier:    tier,
		labelID: g.labels.intern(s.Label),
	}
}

func (g *Graph) appendUniqueSiteLocked(entry *edgeEntry, cs compactSite) {
	if !entry.hasPrimary {
		entry.hasPrimary = true
		entry.primarySite = cs
		return
	}
	if entry.primarySite.line == cs.line && entry.primarySite.column == cs.column {
		if cs.tier < entry.primarySite.tier {
			entry.primarySite.tier = cs.tier
		}
		if cs.labelID != 0 {
			entry.primarySite.labelID = cs.labelID
		}
		return
	}
	for i := range entry.extraSites {
		if entry.extraSites[i].line == cs.line && entry.extraSites[i].column == cs.column {
			if cs.tier < entry.extraSites[i].tier {
				entry.extraSites[i].tier = cs.tier
			}
			if cs.labelID != 0 {
				entry.extraSites[i].labelID = cs.labelID
			}
			return
		}
	}
	entry.extraSites = append(entry.extraSites, cs)
}

func (g *Graph) deleteEdgeSlotLocked(eid uint32, e *edgeEntry) {
	delete(g.edgeToID, compactEdgeKey{source: e.source, target: e.target, kind: e.kind})
	*e = edgeEntry{}
	g.freeEdgeIDs = append(g.freeEdgeIDs, eid)
	g.edgeCount--
}

// GetEdge returns a safe copy of the edge matching (source, target, kind), if present.
func (g *Graph) GetEdge(source, target UID, kind EdgeKind) (Edge, bool) {
	if source.IsZero() || target.IsZero() || !kind.IsValid() {
		return Edge{}, false
	}

	g.mu.RLock()
	defer g.mu.RUnlock()

	return g.getEdgeLocked(source, target, kind)
}

func (g *Graph) getEdgeLocked(source, target UID, kind EdgeKind) (Edge, bool) {
	if !g.init || source.IsZero() || target.IsZero() || !kind.IsValid() {
		return Edge{}, false
	}
	srcID, ok := g.uidToID[source]
	if !ok {
		return Edge{}, false
	}
	tgtID, ok := g.uidToID[target]
	if !ok {
		return Edge{}, false
	}
	eid, ok := g.edgeToID[compactEdgeKey{source: srcID, target: tgtID, kind: kind}]
	if !ok {
		return Edge{}, false
	}
	return g.materializeEdgeLocked(eid), true
}

// HasEdge reports whether an edge matching (source, target, kind) exists.
func (g *Graph) HasEdge(source, target UID, kind EdgeKind) bool {
	if source.IsZero() || target.IsZero() || !kind.IsValid() {
		return false
	}
	g.mu.RLock()
	defer g.mu.RUnlock()

	return g.hasEdgeLocked(source, target, kind)
}

func (g *Graph) hasEdgeLocked(source, target UID, kind EdgeKind) bool {
	if !g.init || source.IsZero() || target.IsZero() || !kind.IsValid() {
		return false
	}
	srcID, ok := g.uidToID[source]
	if !ok {
		return false
	}
	tgtID, ok := g.uidToID[target]
	if !ok {
		return false
	}
	_, ok = g.edgeToID[compactEdgeKey{source: srcID, target: tgtID, kind: kind}]
	return ok
}

// RemoveEdge deletes the edge matching (source, target, kind) and updates adjacency indexes.
func (g *Graph) RemoveEdge(source, target UID, kind EdgeKind) error {
	if source.IsZero() || target.IsZero() || !kind.IsValid() {
		return ErrEdgeNotFound
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.ensureInitLocked()

	srcID, ok := g.uidToID[source]
	if !ok {
		return fmt.Errorf("%w: %s -> %s (%s)", ErrEdgeNotFound, source, target, kind)
	}
	tgtID, ok := g.uidToID[target]
	if !ok {
		return fmt.Errorf("%w: %s -> %s (%s)", ErrEdgeNotFound, source, target, kind)
	}
	key := compactEdgeKey{source: srcID, target: tgtID, kind: kind}
	eid, exists := g.edgeToID[key]
	if !exists {
		return fmt.Errorf("%w: %s -> %s (%s)", ErrEdgeNotFound, source, target, kind)
	}

	g.outgoing[srcID] = swapRemoveUint32(g.outgoing[srcID], eid)
	g.incoming[tgtID] = swapRemoveUint32(g.incoming[tgtID], eid)
	g.deleteEdgeSlotLocked(eid, &g.edges[eid])
	return nil
}

func (g *Graph) materializeEdgeLocked(eid uint32) Edge {
	e := &g.edges[eid]
	out := Edge{
		Source: g.nodes[e.source].UID,
		Target: g.nodes[e.target].UID,
		Kind:   e.kind,
		Tier:   e.tier,
		Label:  g.labels.get(e.labelID),
	}
	if e.hasPrimary {
		out.Sites = make([]CallSiteRef, 0, 1+len(e.extraSites))
		out.Sites = append(out.Sites, CallSiteRef{
			Line:   e.primarySite.line,
			Column: e.primarySite.column,
			Tier:   e.primarySite.tier,
			Label:  g.labels.get(e.primarySite.labelID),
		})
		for _, es := range e.extraSites {
			out.Sites = append(out.Sites, CallSiteRef{
				Line:   es.line,
				Column: es.column,
				Tier:   es.tier,
				Label:  g.labels.get(es.labelID),
			})
		}
	}
	return out
}

// ---------- Call Site CRUD ----------

// AddCallSite records an unresolved, ambiguous, or external call site.
func (g *Graph) AddCallSite(cs CallSite) error {
	if err := cs.Validate(); err != nil {
		return err
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.ensureInitLocked()

	g.addCallSiteLocked(cs)
	return nil
}

func (g *Graph) addCallSiteLocked(cs CallSite) {
	stored := cs.Clone()
	stored.File = g.strings.intern(stored.File)
	stored.LookupKey = g.strings.intern(stored.LookupKey)

	idx := len(g.callSites)
	g.callSites = append(g.callSites, stored)
	g.callSitesByCaller[stored.Caller] = append(g.callSitesByCaller[stored.Caller], idx)
	if stored.File != "" {
		g.callSitesByFile[stored.File] = append(g.callSitesByFile[stored.File], idx)
	}
	if stored.LookupKey != "" {
		g.callSitesByLookup[stored.LookupKey] = append(g.callSitesByLookup[stored.LookupKey], idx)
	}
}

func (g *Graph) removeCallSitesForFileLocked(file string) {
	indices, ok := g.callSitesByFile[file]
	if !ok {
		return
	}
	for _, idx := range indices {
		if idx < len(g.callSites) {
			g.callSites[idx] = CallSite{}
		}
	}
	delete(g.callSitesByFile, file)
	g.rebuildCallSiteIndexesLocked()
}

func (g *Graph) rebuildCallSiteIndexesLocked() {
	compacted := g.callSites[:0]
	for _, cs := range g.callSites {
		if !cs.Caller.IsZero() {
			compacted = append(compacted, cs)
		}
	}
	g.callSites = compacted

	g.callSitesByCaller = make(map[UID][]int, len(compacted))
	g.callSitesByFile = make(map[string][]int, len(compacted))
	g.callSitesByLookup = make(map[string][]int, len(compacted))
	for i := range g.callSites {
		cs := &g.callSites[i]
		g.callSitesByCaller[cs.Caller] = append(g.callSitesByCaller[cs.Caller], i)
		if cs.File != "" {
			g.callSitesByFile[cs.File] = append(g.callSitesByFile[cs.File], i)
		}
		if cs.LookupKey != "" {
			g.callSitesByLookup[cs.LookupKey] = append(g.callSitesByLookup[cs.LookupKey], i)
		}
	}
}

// CallSiteCount returns the number of active call sites.
func (g *Graph) CallSiteCount() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.callSiteCountLocked()
}

func (g *Graph) callSiteCountLocked() int {
	return len(g.callSites)
}

// GetCallSites returns copies of all call sites, sorted deterministically by file, line, column, lookup key.
func (g *Graph) GetCallSites() []CallSite {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.getCallSitesLocked()
}

func (g *Graph) getCallSitesLocked() []CallSite {
	if !g.init || len(g.callSites) == 0 {
		return nil
	}
	out := make([]CallSite, 0, len(g.callSites))
	for _, cs := range g.callSites {
		if !cs.Caller.IsZero() {
			out = append(out, cs.Clone())
		}
	}
	sortCallSites(out)
	return out
}

// GetCallSitesByFile returns call sites in the given file, sorted deterministically.
func (g *Graph) GetCallSitesByFile(file string) []CallSite {
	if file == "" {
		return nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.getCallSitesByFileLocked(file)
}

func (g *Graph) getCallSitesByFileLocked(file string) []CallSite {
	if !g.init || file == "" {
		return nil
	}
	indices := g.callSitesByFile[file]
	if len(indices) == 0 {
		return nil
	}
	out := make([]CallSite, 0, len(indices))
	for _, idx := range indices {
		if idx < len(g.callSites) && !g.callSites[idx].Caller.IsZero() {
			out = append(out, g.callSites[idx].Clone())
		}
	}
	sortCallSites(out)
	return out
}

// GetCallSitesByLookupKey returns call sites matching the given lookup key, sorted deterministically.
func (g *Graph) GetCallSitesByLookupKey(lookupKey string) []CallSite {
	if lookupKey == "" {
		return nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.getCallSitesByLookupKeyLocked(lookupKey)
}

func (g *Graph) getCallSitesByLookupKeyLocked(lookupKey string) []CallSite {
	if !g.init || lookupKey == "" {
		return nil
	}
	indices := g.callSitesByLookup[lookupKey]
	if len(indices) == 0 {
		return nil
	}
	out := make([]CallSite, 0, len(indices))
	for _, idx := range indices {
		if idx < len(g.callSites) && !g.callSites[idx].Caller.IsZero() {
			out = append(out, g.callSites[idx].Clone())
		}
	}
	sortCallSites(out)
	return out
}

// HasCallSiteForLookup reports whether any call site exists for the given lookup key.
func (g *Graph) HasCallSiteForLookup(lookupKey string) bool {
	if lookupKey == "" {
		return false
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.hasCallSiteForLookupLocked(lookupKey)
}

func (g *Graph) hasCallSiteForLookupLocked(lookupKey string) bool {
	if !g.init || lookupKey == "" {
		return false
	}
	return len(g.callSitesByLookup[lookupKey]) > 0
}

// ---------- Bulk Queries ----------

// AllNodes returns copies of all nodes in the graph sorted deterministically by UID.
func (g *Graph) AllNodes() []Node {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.allNodesLocked()
}

func (g *Graph) allNodesLocked() []Node {
	if !g.init || g.nodeCount == 0 {
		return nil
	}
	result := make([]Node, 0, g.nodeCount)
	for i := range g.nodes {
		if g.nodes[i].Kind.IsValid() {
			result = append(result, g.nodes[i].Clone())
		}
	}
	slices.SortFunc(result, func(a, b Node) int {
		return a.UID.Compare(b.UID)
	})
	return result
}

// NodeCount returns the number of nodes in the graph.
func (g *Graph) NodeCount() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.nodeCount
}

// AllEdges returns copies of all edges in the graph sorted deterministically by (Source, Target, Kind).
func (g *Graph) AllEdges() []Edge {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.allEdgesLocked()
}

func (g *Graph) allEdgesLocked() []Edge {
	if !g.init || g.edgeCount == 0 {
		return nil
	}
	result := make([]Edge, 0, g.edgeCount)
	for i := range g.edges {
		if g.edges[i].alive() {
			result = append(result, g.materializeEdgeLocked(uint32(i)))
		}
	}
	slices.SortFunc(result, compareEdgesCanonical)
	return result
}

// EdgeCount returns the number of edges in the graph.
func (g *Graph) EdgeCount() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.edgeCount
}

// ---------- ReadView for consistent multi-call reads ----------

// ReadView provides read-only access to the graph under a single RLock
// so multi-call queries observe a consistent snapshot with zero torn reads.
type ReadView struct {
	g *Graph
}

// View executes fn while holding the graph's read lock.
// Callers must not invoke mutating methods on g inside fn.
func (g *Graph) View(fn func(v *ReadView)) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	fn(&ReadView{g: g})
}

func (v *ReadView) GetNode(uid UID) (Node, bool) { return v.g.getNodeLocked(uid) }
func (v *ReadView) HasNode(uid UID) bool         { return v.g.hasNodeLocked(uid) }
func (v *ReadView) GetEdge(source, target UID, kind EdgeKind) (Edge, bool) {
	return v.g.getEdgeLocked(source, target, kind)
}
func (v *ReadView) HasEdge(source, target UID, kind EdgeKind) bool {
	return v.g.hasEdgeLocked(source, target, kind)
}
func (v *ReadView) NodeCount() int                 { return v.g.nodeCount }
func (v *ReadView) EdgeCount() int                 { return v.g.edgeCount }
func (v *ReadView) CallSiteCount() int             { return v.g.callSiteCountLocked() }
func (v *ReadView) AllNodes() []Node               { return v.g.allNodesLocked() }
func (v *ReadView) AllEdges() []Edge               { return v.g.allEdgesLocked() }
func (v *ReadView) GetByName(name string) []Node   { return v.g.getByNameLocked(name) }
func (v *ReadView) GetByFile(file string) []Node   { return v.g.getByFileLocked(file) }
func (v *ReadView) GetByQName(qname string) []Node { return v.g.getByQNameLocked(qname) }
func (v *ReadView) GetUniqueByQName(qname string) (Node, error) {
	return v.g.getUniqueByQNameLocked(qname)
}
func (v *ReadView) GetCallers(uid UID) []Edge { return v.g.getCallersLocked(uid) }
func (v *ReadView) GetCallees(uid UID) []Edge { return v.g.getCalleesLocked(uid) }
func (v *ReadView) GetIncomingEdges(uid UID, kinds ...EdgeKind) []Edge {
	return v.g.getIncomingEdgesLocked(uid, kinds...)
}
func (v *ReadView) GetOutgoingEdges(uid UID, kinds ...EdgeKind) []Edge {
	return v.g.getOutgoingEdgesLocked(uid, kinds...)
}
func (v *ReadView) GetContains(parentUID UID) []Node { return v.g.getContainsLocked(parentUID) }
func (v *ReadView) GetRevDeps(targetUID UID) []UID   { return v.g.getRevDepsLocked(targetUID) }
func (v *ReadView) GetDependentFiles(targetUID UID) []string {
	return v.g.getDependentFilesLocked(targetUID)
}
func (v *ReadView) GetCallSites() []CallSite { return v.g.getCallSitesLocked() }
func (v *ReadView) GetCallSitesByFile(file string) []CallSite {
	return v.g.getCallSitesByFileLocked(file)
}
func (v *ReadView) GetCallSitesByLookupKey(key string) []CallSite {
	return v.g.getCallSitesByLookupKeyLocked(key)
}
func (v *ReadView) HasCallSiteForLookup(key string) bool { return v.g.hasCallSiteForLookupLocked(key) }

// ---------- String interning & utilities ----------

func (g *Graph) internNodeStrings(n *Node) {
	if g.strings == nil {
		return
	}
	n.File = g.strings.intern(n.File)
	n.Language = g.strings.intern(n.Language)
	n.Type = g.strings.intern(n.Type)
	n.Receiver = g.strings.intern(n.Receiver)
	n.ReturnType = g.strings.intern(n.ReturnType)
	for i := range n.Params {
		n.Params[i].Type = g.strings.intern(n.Params[i].Type)
	}
	for i := range n.Decorators {
		n.Decorators[i] = g.strings.intern(n.Decorators[i])
	}
	for i := range n.Bases {
		n.Bases[i] = g.strings.intern(n.Bases[i])
	}
}

func swapRemoveUint32(list []uint32, target uint32) []uint32 {
	for i, v := range list {
		if v == target {
			last := len(list) - 1
			list[i] = list[last]
			return list[:last]
		}
	}
	return list
}

func compareEdgesCanonical(a, b Edge) int {
	if c := a.Source.Compare(b.Source); c != 0 {
		return c
	}
	if c := a.Target.Compare(b.Target); c != 0 {
		return c
	}
	return cmp.Compare(a.Kind, b.Kind)
}

func sortCallSites(sites []CallSite) {
	slices.SortFunc(sites, func(a, b CallSite) int {
		if c := cmp.Compare(a.File, b.File); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Line, b.Line); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Column, b.Column); c != 0 {
			return c
		}
		if c := cmp.Compare(a.LookupKey, b.LookupKey); c != 0 {
			return c
		}
		return a.Caller.Compare(b.Caller)
	})
}
