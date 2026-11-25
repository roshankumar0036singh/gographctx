package graph

import (
	"cmp"
	"slices"
	"strings"
)

const defaultMaxCallPaths = 256

// CallDirection selects traversal direction for CallChain queries.
type CallDirection uint8

const (
	DirectionCallees CallDirection = iota
	DirectionCallers
)

// CallChainOptions configures bounded transitive call-chain traversal.
type CallChainOptions struct {
	Direction   CallDirection
	MaxDepth    int
	MaxPaths    int  // Defaults to defaultMaxCallPaths when <= 0
	CertainOnly bool // When true, only follow certain-tier call edges
}

// CallChainResult holds the chains discovered by CallChainWithOptions plus truncation metadata.
type CallChainResult struct {
	RootUID   UID           `json:"root_uid"`
	Direction CallDirection `json:"direction"`
	MaxDepth  int           `json:"max_depth"`
	Chains    [][]Edge      `json:"chains"`
	Truncated bool          `json:"truncated"`
}

// ImpactNode describes a single symbol impacted by changes to a target node.
type ImpactNode struct {
	Node          Node           `json:"node"`
	Depth         int            `json:"depth"`
	EffectiveTier ResolutionTier `json:"effective_tier"`
	Certain       bool           `json:"certain"`
	Via           []Edge         `json:"via,omitempty"`
}

// ImpactResult describes the transitive reverse-dependency blast radius of a symbol.
type ImpactResult struct {
	Target        Node         `json:"target"`
	Found         bool         `json:"found"`
	MaxDepth      int          `json:"max_depth"`
	DirectCallers int          `json:"direct_callers"`
	TotalImpacted int          `json:"total_impacted"`
	CertainCount  int          `json:"certain_count"`
	LikelyCount   int          `json:"likely_count"`
	Impacted      []ImpactNode `json:"impacted"`
	AffectedFiles []string     `json:"affected_files,omitempty"`
}

// SearchQuery defines multi-criteria filters for symbol discovery.
type SearchQuery struct {
	NamePattern   string     // Supports '*' and '?' wildcards; plain text matches case-insensitive substring (exact first)
	QNamePattern  string     // Optional qualified-name filter (glob or case-insensitive substring)
	ExactName     bool       // Require exact case-insensitive Name match when NamePattern has no wildcards
	Kinds         []NodeKind // Empty matches all valid NodeKinds
	Languages     []string   // Empty matches all languages (case-insensitive)
	FilePattern   string     // Supports '*' and '?' wildcards or case-insensitive path substring
	ExportedOnly  bool
	MinComplexity uint16
	MaxComplexity uint16 // 0 means no upper limit
	Offset        int
	Limit         int // <= 0 means no limit
}

// SearchResult holds matching symbols along with pagination and truncation metadata.
type SearchResult struct {
	Nodes     []Node `json:"nodes"`
	Total     int    `json:"total"`
	Returned  int    `json:"returned"`
	Truncated bool   `json:"truncated"`
}

// RepoStats holds aggregate counts and resolution coverage for the indexed graph.
type RepoStats struct {
	NodeCount          int                    `json:"node_count"`
	EdgeCount          int                    `json:"edge_count"`
	CallSiteCount      int                    `json:"call_site_count"`
	FileCount          int                    `json:"file_count"`
	NodesByKind        map[NodeKind]int       `json:"nodes_by_kind"`
	EdgesByKind        map[EdgeKind]int       `json:"edges_by_kind"`
	EdgesByTier        map[ResolutionTier]int `json:"edges_by_tier"`
	CallSitesByStatus  map[CallStatus]int     `json:"call_sites_by_status"`
	NodesByLanguage    map[string]int         `json:"nodes_by_language,omitempty"`
	ResolutionCoverage float64                `json:"resolution_coverage"` // resolved calls / (resolved + unresolved/ambiguous calls) in [0, 1]
}

// ---------- Graph Query Methods ----------

// Callers returns direct incoming CALLS and LIKELY_CALLS edges for uid, sorted stronger tier first.
func (g *Graph) Callers(uid UID) []Edge {
	return g.GetCallers(uid)
}

// Callees returns direct outgoing CALLS and LIKELY_CALLS edges from uid, sorted stronger tier first.
func (g *Graph) Callees(uid UID) []Edge {
	return g.GetCallees(uid)
}

// CallChain returns bounded, cycle-safe transitive call chains starting from uid up to maxDepth hops.
func (g *Graph) CallChain(uid UID, maxDepth int, dir CallDirection) [][]Edge {
	return g.CallChainWithOptions(uid, CallChainOptions{
		Direction: dir,
		MaxDepth:  maxDepth,
	}).Chains
}

// CallChainWithOptions returns bounded, cycle-safe transitive call chains with truncation metadata.
func (g *Graph) CallChainWithOptions(uid UID, opts CallChainOptions) CallChainResult {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.callChainLocked(uid, opts)
}

// ImpactAnalysis computes the transitive reverse-dependency blast radius of targetUID up to maxDepth hops.
func (g *Graph) ImpactAnalysis(uid UID, maxDepth int) ImpactResult {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.impactAnalysisLocked(uid, maxDepth)
}

// Ancestors returns all base classes, embedded structs, and implemented interfaces reachable from uid.
func (g *Graph) Ancestors(uid UID) []Node {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.traverseHierarchyLocked(uid, true)
}

// Descendants returns all subclasses and implementors referencing uid.
func (g *Graph) Descendants(uid UID) []Node {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.traverseHierarchyLocked(uid, false)
}

// FileNodes returns all symbols defined in file path sorted deterministically by source location.
func (g *Graph) FileNodes(path string) []Node {
	return g.GetByFile(path)
}

// Search returns symbols matching the multi-criteria SearchQuery.
func (g *Graph) Search(q SearchQuery) []Node {
	return g.SearchWithStats(q).Nodes
}

// SearchWithStats returns matching symbols along with total match count and truncation metadata.
func (g *Graph) SearchWithStats(q SearchQuery) SearchResult {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.searchLocked(q)
}

// Stats returns aggregate node, edge, language, call-site, and resolution coverage metrics.
func (g *Graph) Stats() RepoStats {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.statsLocked()
}

// ---------- ReadView Mirrors ----------

func (v *ReadView) Callers(uid UID) []Edge { return v.g.getCallersLocked(uid) }
func (v *ReadView) Callees(uid UID) []Edge { return v.g.getCalleesLocked(uid) }
func (v *ReadView) CallChain(uid UID, maxDepth int, dir CallDirection) [][]Edge {
	return v.g.callChainLocked(uid, CallChainOptions{Direction: dir, MaxDepth: maxDepth}).Chains
}
func (v *ReadView) CallChainWithOptions(uid UID, opts CallChainOptions) CallChainResult {
	return v.g.callChainLocked(uid, opts)
}
func (v *ReadView) ImpactAnalysis(uid UID, maxDepth int) ImpactResult {
	return v.g.impactAnalysisLocked(uid, maxDepth)
}
func (v *ReadView) Ancestors(uid UID) []Node                   { return v.g.traverseHierarchyLocked(uid, true) }
func (v *ReadView) Descendants(uid UID) []Node                 { return v.g.traverseHierarchyLocked(uid, false) }
func (v *ReadView) FileNodes(path string) []Node               { return v.g.getByFileLocked(path) }
func (v *ReadView) Search(q SearchQuery) []Node                { return v.g.searchLocked(q).Nodes }
func (v *ReadView) SearchWithStats(q SearchQuery) SearchResult { return v.g.searchLocked(q) }
func (v *ReadView) Stats() RepoStats                           { return v.g.statsLocked() }

// ---------- Internal Query Implementations (Lock Held) ----------

func (g *Graph) callChainLocked(uid UID, opts CallChainOptions) CallChainResult {
	normalizedDepth := opts.MaxDepth
	if normalizedDepth < 0 {
		normalizedDepth = 0
	}
	res := CallChainResult{
		RootUID:   uid,
		Direction: opts.Direction,
		MaxDepth:  normalizedDepth,
	}
	if !g.init || uid.IsZero() || normalizedDepth == 0 {
		return res
	}
	startID, ok := g.uidToID[uid]
	if !ok {
		return res
	}

	// Because emitted chains are simple paths, depth cannot exceed nodeCount.
	effectiveMaxDepth := min(normalizedDepth, g.nodeCount)

	maxPaths := opts.MaxPaths
	if maxPaths <= 0 {
		maxPaths = defaultMaxCallPaths
	}

	onPath := map[uint32]bool{startID: true}
	path := make([]Edge, 0, min(effectiveMaxDepth, 16))

	emitChain := func(chain []Edge) {
		if len(chain) == 0 || res.Truncated {
			return
		}
		if len(res.Chains) >= maxPaths {
			res.Truncated = true
			return
		}
		res.Chains = append(res.Chains, chain)
	}

	var dfs func(currID uint32, depth int)
	dfs = func(currID uint32, depth int) {
		if res.Truncated {
			return
		}
		if depth == effectiveMaxDepth {
			if len(path) > 0 {
				cp := make([]Edge, len(path))
				for i := range path {
					cp[i] = path[i].Clone()
				}
				emitChain(cp)
			}
			return
		}

		var rawAdj []uint32
		if opts.Direction == DirectionCallers {
			rawAdj = g.incoming[currID]
		} else {
			rawAdj = g.outgoing[currID]
		}

		cands := make([]uint32, 0, len(rawAdj))
		for _, eid := range rawAdj {
			e := &g.edges[eid]
			if !e.alive() || !e.kind.IsCall() {
				continue
			}
			if opts.CertainOnly && !e.tier.IsCertain() {
				continue
			}
			cands = append(cands, eid)
		}
		g.sortEdgeIDsForTraversal(cands, opts.Direction)

		extended := false
		for _, eid := range cands {
			if res.Truncated {
				return
			}
			e := &g.edges[eid]
			nextID := e.target
			if opts.Direction == DirectionCallers {
				nextID = e.source
			}

			// Direct recursive self-loop at root or leaf can be emitted once as terminal 1-hop edge.
			if nextID == currID {
				extended = true
				edgeCopy := g.materializeEdgeLocked(eid)
				cp := make([]Edge, len(path)+1)
				for i := range path {
					cp[i] = path[i].Clone()
				}
				cp[len(path)] = edgeCopy
				emitChain(cp)
				continue
			}

			if onPath[nextID] {
				continue
			}

			extended = true
			onPath[nextID] = true
			path = append(path, g.materializeEdgeLocked(eid))
			dfs(nextID, depth+1)
			path = path[:len(path)-1]
			delete(onPath, nextID)
		}

		if !extended && len(path) > 0 {
			cp := make([]Edge, len(path))
			for i := range path {
				cp[i] = path[i].Clone()
			}
			emitChain(cp)
		}
	}

	dfs(startID, 0)
	return res
}

func (g *Graph) sortEdgeIDsForTraversal(eids []uint32, dir CallDirection) {
	slices.SortFunc(eids, func(aID, bID uint32) int {
		a := &g.edges[aID]
		b := &g.edges[bID]
		if c := cmp.Compare(a.tier, b.tier); c != 0 {
			return c
		}
		var aLine, bLine uint32
		if a.hasPrimary {
			aLine = a.primarySite.line
		}
		if b.hasPrimary {
			bLine = b.primarySite.line
		}
		if c := cmp.Compare(aLine, bLine); c != 0 {
			return c
		}
		aPeer, bPeer := a.target, b.target
		if dir == DirectionCallers {
			aPeer, bPeer = a.source, b.source
		}
		if c := g.nodes[aPeer].UID.Compare(g.nodes[bPeer].UID); c != 0 {
			return c
		}
		return cmp.Compare(a.kind, b.kind)
	})
}

func (g *Graph) impactAnalysisLocked(uid UID, maxDepth int) ImpactResult {
	if maxDepth < 0 {
		maxDepth = 0
	}
	res := ImpactResult{MaxDepth: maxDepth}
	if !g.init || uid.IsZero() || maxDepth == 0 {
		return res
	}
	startID, ok := g.uidToID[uid]
	if !ok {
		return res
	}

	res.Target = g.nodes[startID].Clone()
	res.Found = true

	// Pass 1: shortest paths using only certain dependency edges.
	// Pass 2: shortest paths across all dependency edges to capture likely-only impacts & all direct callers.
	// Certain paths strictly supersede shorter likely-only paths without frontier-order sensitivity.
	certainState := g.runImpactBFS(startID, maxDepth, true)
	allState := g.runImpactBFS(startID, maxDepth, false)

	res.DirectCallers = len(allState.directCallers)
	if len(allState.viaEdges) == 0 {
		return res
	}

	impacted := make([]ImpactNode, 0, len(allState.viaEdges))
	affectedFileSet := make(map[string]struct{})
	for nid := range allState.viaEdges {
		depth := allState.bestDepth[nid]
		tier := allState.bestTier[nid]
		edges := allState.viaEdges[nid]
		if _, hasCertain := certainState.viaEdges[nid]; hasCertain {
			depth = certainState.bestDepth[nid]
			tier = certainState.bestTier[nid]
			edges = certainState.viaEdges[nid]
		}

		certain := tier.IsCertain()
		if certain {
			res.CertainCount++
		} else {
			res.LikelyCount++
		}
		slices.SortFunc(edges, compareEdgesCanonical)
		nodeCopy := g.nodes[nid].Clone()
		if nodeCopy.File != "" {
			affectedFileSet[nodeCopy.File] = struct{}{}
		}
		impacted = append(impacted, ImpactNode{
			Node:          nodeCopy,
			Depth:         depth,
			EffectiveTier: tier,
			Certain:       certain,
			Via:           edges,
		})
	}

	slices.SortFunc(impacted, func(a, b ImpactNode) int {
		if c := cmp.Compare(a.Depth, b.Depth); c != 0 {
			return c
		}
		if c := cmp.Compare(a.EffectiveTier, b.EffectiveTier); c != 0 {
			return c
		}
		return compareNodesByLocation(a.Node, b.Node)
	})

	affectedFiles := make([]string, 0, len(affectedFileSet))
	for f := range affectedFileSet {
		affectedFiles = append(affectedFiles, f)
	}
	slices.Sort(affectedFiles)

	res.TotalImpacted = len(impacted)
	res.Impacted = impacted
	res.AffectedFiles = affectedFiles
	return res
}

type impactBFSState struct {
	bestDepth     map[uint32]int
	bestTier      map[uint32]ResolutionTier
	viaEdges      map[uint32][]Edge
	directCallers map[uint32]struct{}
}

func (g *Graph) runImpactBFS(startID uint32, maxDepth int, certainOnly bool) impactBFSState {
	st := impactBFSState{
		bestDepth:     map[uint32]int{startID: 0},
		bestTier:      make(map[uint32]ResolutionTier),
		viaEdges:      make(map[uint32][]Edge),
		directCallers: make(map[uint32]struct{}),
	}

	frontier := []uint32{startID}
	for depth := 1; depth <= maxDepth && len(frontier) > 0; depth++ {
		var nextFrontier []uint32
		for _, currID := range frontier {
			for _, eid := range g.incoming[currID] {
				e := &g.edges[eid]
				if !e.alive() || !e.kind.IsDependency() {
					continue
				}
				if certainOnly && !e.tier.IsCertain() {
					continue
				}
				srcID := e.source
				if srcID == startID {
					continue
				}

				if depth == 1 && e.kind.IsCall() {
					st.directCallers[srcID] = struct{}{}
				}

				// Larger ResolutionTier number = weaker confidence; a path is limited by its weakest hop.
				pathTier := e.tier
				if currID != startID {
					if parentTier := st.bestTier[currID]; parentTier > pathTier {
						pathTier = parentTier
					}
				}

				prevDepth, visited := st.bestDepth[srcID]
				if !visited {
					st.bestDepth[srcID] = depth
					st.bestTier[srcID] = pathTier
					st.viaEdges[srcID] = []Edge{g.materializeEdgeLocked(eid)}
					nextFrontier = append(nextFrontier, srcID)
				} else if prevDepth == depth {
					st.viaEdges[srcID] = append(st.viaEdges[srcID], g.materializeEdgeLocked(eid))
					// Smaller ResolutionTier number = stronger confidence.
					if pathTier < st.bestTier[srcID] {
						st.bestTier[srcID] = pathTier
					}
				}
			}
		}
		frontier = nextFrontier
	}
	return st
}

func (g *Graph) traverseHierarchyLocked(uid UID, upward bool) []Node {
	if !g.init || uid.IsZero() {
		return nil
	}
	startID, ok := g.uidToID[uid]
	if !ok {
		return nil
	}

	type item struct {
		node Node
		dist int
	}

	distMap := map[uint32]int{startID: 0}
	queue := []uint32{startID}
	var collected []item

	for head := 0; head < len(queue); head++ {
		currID := queue[head]
		currDist := distMap[currID]

		var adj []uint32
		if upward {
			adj = g.outgoing[currID]
		} else {
			adj = g.incoming[currID]
		}

		for _, eid := range adj {
			e := &g.edges[eid]
			if !e.alive() || (e.kind != EdgeInherits && e.kind != EdgeImplements) {
				continue
			}
			nextID := e.target
			if !upward {
				nextID = e.source
			}
			if _, seen := distMap[nextID]; seen {
				continue
			}
			distMap[nextID] = currDist + 1
			queue = append(queue, nextID)
			collected = append(collected, item{
				node: g.nodes[nextID].Clone(),
				dist: currDist + 1,
			})
		}
	}

	if len(collected) == 0 {
		return nil
	}

	slices.SortFunc(collected, func(a, b item) int {
		if c := cmp.Compare(a.dist, b.dist); c != 0 {
			return c
		}
		return compareNodesByLocation(a.node, b.node)
	})

	out := make([]Node, len(collected))
	for i := range collected {
		out[i] = collected[i].node
	}
	return out
}

func (g *Graph) searchLocked(q SearchQuery) SearchResult {
	var res SearchResult
	if !g.init || g.nodeCount == 0 {
		return res
	}

	var kindSet map[NodeKind]struct{}
	if len(q.Kinds) > 0 {
		kindSet = make(map[NodeKind]struct{}, len(q.Kinds))
		for _, k := range q.Kinds {
			kindSet[k] = struct{}{}
		}
	}

	var langSet map[string]struct{}
	if len(q.Languages) > 0 {
		langSet = make(map[string]struct{}, len(q.Languages))
		for _, l := range q.Languages {
			langSet[strings.ToLower(strings.TrimSpace(l))] = struct{}{}
		}
	}

	namePat := strings.TrimSpace(q.NamePattern)
	qnamePat := strings.TrimSpace(q.QNamePattern)
	filePat := strings.TrimSpace(q.FilePattern)

	type scoredMatch struct {
		idx   int
		score int
	}
	var matches []scoredMatch

	for i := range g.nodes {
		n := &g.nodes[i]
		if !n.Kind.IsValid() {
			continue
		}
		if kindSet != nil {
			if _, ok := kindSet[n.Kind]; !ok {
				continue
			}
		}
		if langSet != nil {
			if _, ok := langSet[strings.ToLower(n.Language)]; !ok {
				continue
			}
		}
		if q.ExportedOnly && !n.Exported {
			continue
		}
		if n.Complexity < q.MinComplexity {
			continue
		}
		if q.MaxComplexity > 0 && n.Complexity > q.MaxComplexity {
			continue
		}
		if filePat != "" && !matchPattern(filePat, n.File, false) {
			continue
		}
		if qnamePat != "" && !matchPattern(qnamePat, n.QName, false) {
			continue
		}

		score := 0
		if namePat != "" {
			ok, sc := matchNameScored(namePat, n.Name, q.ExactName)
			if !ok {
				continue
			}
			score = sc
		}

		matches = append(matches, scoredMatch{
			idx:   i,
			score: score,
		})
	}

	slices.SortFunc(matches, func(a, b scoredMatch) int {
		if c := cmp.Compare(a.score, b.score); c != 0 {
			return c
		}
		return compareNodesByLocation(g.nodes[a.idx], g.nodes[b.idx])
	})

	res.Total = len(matches)
	if res.Total == 0 {
		return res
	}

	start := q.Offset
	if start < 0 {
		start = 0
	}
	if start >= res.Total {
		return res
	}

	end := res.Total
	if q.Limit > 0 && start+q.Limit < end {
		end = start + q.Limit
	}
	res.Truncated = end < res.Total

	res.Nodes = make([]Node, 0, end-start)
	for i := start; i < end; i++ {
		res.Nodes = append(res.Nodes, g.nodes[matches[i].idx].Clone())
	}
	res.Returned = len(res.Nodes)
	return res
}

func (g *Graph) statsLocked() RepoStats {
	stats := RepoStats{
		NodesByKind:       make(map[NodeKind]int),
		EdgesByKind:       make(map[EdgeKind]int),
		EdgesByTier:       make(map[ResolutionTier]int),
		CallSitesByStatus: make(map[CallStatus]int),
		NodesByLanguage:   make(map[string]int),
	}
	if !g.init {
		return stats
	}

	stats.NodeCount = g.nodeCount
	stats.EdgeCount = g.edgeCount
	stats.FileCount = len(g.byFile)

	for i := range g.nodes {
		n := &g.nodes[i]
		if !n.Kind.IsValid() {
			continue
		}
		stats.NodesByKind[n.Kind]++
		if n.Language != "" {
			stats.NodesByLanguage[n.Language]++
		}
	}

	resolvedCalls := 0
	for i := range g.edges {
		e := &g.edges[i]
		if !e.alive() {
			continue
		}
		stats.EdgesByKind[e.kind]++
		stats.EdgesByTier[e.tier]++
		if e.kind.IsCall() {
			if e.hasPrimary {
				resolvedCalls += 1 + len(e.extraSites)
			} else {
				resolvedCalls++
			}
		}
	}

	unresolvedCalls := 0
	activeSites := 0
	for i := range g.callSites {
		cs := &g.callSites[i]
		if cs.Caller.IsZero() {
			continue
		}
		activeSites++
		stats.CallSitesByStatus[cs.Status]++
		if cs.Status == CallStatusUnresolved || cs.Status == CallStatusAmbiguous {
			unresolvedCalls++
		}
	}
	stats.CallSiteCount = activeSites

	if denom := resolvedCalls + unresolvedCalls; denom > 0 {
		stats.ResolutionCoverage = float64(resolvedCalls) / float64(denom)
	}
	return stats
}

// ---------- Pattern Matching & Ordering Helpers ----------

func hasGlobMeta(pattern string) bool {
	return strings.ContainsAny(pattern, "*?")
}

func matchPattern(pattern, target string, exact bool) bool {
	if hasGlobMeta(pattern) {
		return matchGlobFold(pattern, target)
	}
	if exact {
		return strings.EqualFold(target, pattern)
	}
	return strings.Contains(strings.ToLower(target), strings.ToLower(pattern))
}

func matchNameScored(pattern, name string, exact bool) (bool, int) {
	if hasGlobMeta(pattern) {
		if matchGlobFold(pattern, name) {
			return true, 1
		}
		return false, 0
	}
	lowerName := strings.ToLower(name)
	lowerPat := strings.ToLower(pattern)
	if lowerName == lowerPat {
		return true, 0
	}
	if exact {
		return false, 0
	}
	if strings.HasPrefix(lowerName, lowerPat) {
		return true, 1
	}
	if strings.Contains(lowerName, lowerPat) {
		return true, 2
	}
	return false, 0
}

// matchGlobFold matches '*' (zero or more runes) and '?' (single rune) case-insensitively.
func matchGlobFold(pattern, text string) bool {
	p := []rune(strings.ToLower(pattern))
	t := []rune(strings.ToLower(text))

	pi, ti := 0, 0
	starIdx, matchIdx := -1, 0

	for ti < len(t) {
		if pi < len(p) && (p[pi] == '?' || p[pi] == t[ti]) {
			pi++
			ti++
		} else if pi < len(p) && p[pi] == '*' {
			starIdx = pi
			matchIdx = ti
			pi++
		} else if starIdx != -1 {
			pi = starIdx + 1
			matchIdx++
			ti = matchIdx
		} else {
			return false
		}
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}

func compareNodesByLocation(a, b Node) int {
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
}
