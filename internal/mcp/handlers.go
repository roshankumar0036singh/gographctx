package mcp

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/roshankumar0036singh/gographctx/internal/graph"
	"github.com/roshankumar0036singh/gographctx/internal/parser"
)

var (
	// ErrUnknownTool indicates an unrecognized MCP tool name.
	ErrUnknownTool = errors.New("mcp: unknown tool")
	// ErrInvalidArgs indicates missing or malformed tool call arguments.
	ErrInvalidArgs = errors.New("mcp: invalid tool arguments")
)

const (
	defaultToolLimit  = 50
	maxToolLimit      = 200
	defaultMaxDepth   = 3
	maxImpactMaxDepth = 10
)

// Handler dispatches the 7 GoGraphCtx MCP tools against a *graph.Graph and
// formats responses through *Formatter.
type Handler struct {
	graph     *graph.Graph
	formatter *Formatter
}

// NewHandler constructs a Handler backed by g and fmtCfg.
func NewHandler(g *graph.Graph, fmtCfg FormatterConfig) *Handler {
	if g == nil {
		g = graph.New()
	}
	return &Handler{
		graph:     g,
		formatter: NewFormatter(fmtCfg),
	}
}

// Formatter returns the Handler's response formatter.
func (h *Handler) Formatter() *Formatter {
	return h.formatter
}

// Graph returns the underlying Graph queried by the Handler.
func (h *Handler) Graph() *graph.Graph {
	return h.graph
}

// SymbolSummary is the canonical LLM-facing representation of a graph.Node.
type SymbolSummary struct {
	UID        string   `json:"uid"`
	Kind       string   `json:"kind"`
	Name       string   `json:"name"`
	QName      string   `json:"qualified_name"`
	File       string   `json:"file,omitempty"`
	StartLine  uint32   `json:"start_line,omitempty"`
	EndLine    uint32   `json:"end_line,omitempty"`
	Language   string   `json:"language,omitempty"`
	Type       string   `json:"type,omitempty"`
	Receiver   string   `json:"receiver,omitempty"`
	ReturnType string   `json:"return_type,omitempty"`
	Exported   bool     `json:"exported,omitempty"`
	Complexity uint16   `json:"complexity,omitempty"`
	Docstring  string   `json:"docstring,omitempty"`
	Bases      []string `json:"bases,omitempty"`
	Decorators []string `json:"decorators,omitempty"`
}

// CallEdgeSummary describes one direct caller or callee relationship with tier metadata.
type CallEdgeSummary struct {
	Symbol          SymbolSummary       `json:"symbol"`
	EdgeKind        string              `json:"edge_kind"`
	Tier            string              `json:"tier"`
	Confidence      float32             `json:"confidence"`
	ConfidenceLabel string              `json:"confidence_label"`
	Reason          string              `json:"reason,omitempty"`
	Sites           []graph.CallSiteRef `json:"sites,omitempty"`
}

// ImpactEntry describes one symbol affected by changes to a target symbol.
type ImpactEntry struct {
	Symbol          SymbolSummary `json:"symbol"`
	Depth           int           `json:"depth"`
	EffectiveTier   string        `json:"effective_tier"`
	Confidence      float32       `json:"confidence"`
	ConfidenceLabel string        `json:"confidence_label"`
	Certain         bool          `json:"certain"`
	Via             []string      `json:"via,omitempty"`
}

// UnresolvedSiteSample summarizes one unresolved or ambiguous call site that might target the queried symbol.
type UnresolvedSiteSample struct {
	File       string `json:"file"`
	Line       uint32 `json:"line"`
	Column     uint16 `json:"column,omitempty"`
	CalleeExpr string `json:"callee_expr"`
	Status     string `json:"status"`
	Reason     string `json:"reason,omitempty"`
}

// UnresolvedHints discloses plausible unresolved/ambiguous call sites so agents are warned of incomplete call graphs.
type UnresolvedHints struct {
	Count   int                    `json:"count"`
	Samples []UnresolvedSiteSample `json:"samples,omitempty"`
}

// HandleCall validates rawArgs, executes toolName against the graph, and returns
// token-budget-enforced JSON bytes.
func (h *Handler) HandleCall(ctx context.Context, toolName string, rawArgs json.RawMessage) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var payload ToolPayload
	var coverage float64
	var err error

	h.graph.View(func(v *graph.ReadView) {
		coverage = v.Stats().ResolutionCoverage
		switch toolName {
		case "find_symbol":
			payload, err = h.handleFindSymbol(v, rawArgs)
		case "get_callers":
			payload, err = h.handleCallNeighbor(v, rawArgs, true)
		case "get_callees":
			payload, err = h.handleCallNeighbor(v, rawArgs, false)
		case "get_impact":
			payload, err = h.handleGetImpact(v, rawArgs)
		case "file_outline":
			payload, err = h.handleFileOutline(v, rawArgs)
		case "search_code":
			payload, err = h.handleSearchCode(v, rawArgs)
		case "repo_overview":
			payload, err = h.handleRepoOverview(v, rawArgs)
		default:
			err = fmt.Errorf("%w: %q", ErrUnknownTool, toolName)
		}
	})
	if err != nil {
		return nil, err
	}

	return h.formatter.Format(toolName, payload, coverage)
}

func (h *Handler) handleFindSymbol(v *graph.ReadView, rawArgs json.RawMessage) (ToolPayload, error) {
	var args struct {
		Name         string `json:"name"`
		Kind         string `json:"kind"`
		File         string `json:"file"`
		Language     string `json:"language"`
		ExportedOnly bool   `json:"exported_only"`
		Limit        int    `json:"limit"`
	}
	if err := decodeToolArgs(rawArgs, &args); err != nil {
		return ToolPayload{}, err
	}

	limit := clampLimit(args.Limit, defaultToolLimit, maxToolLimit)
	var kinds []graph.NodeKind
	if strings.TrimSpace(args.Kind) != "" {
		k, err := graph.ParseNodeKind(args.Kind)
		if err != nil {
			return ToolPayload{}, fmt.Errorf("%w: %v", ErrInvalidArgs, err)
		}
		kinds = []graph.NodeKind{k}
	}
	var langs []string
	if strings.TrimSpace(args.Language) != "" {
		langs = []string{parser.NormalizeLanguage(args.Language)}
	}

	sr := v.SearchWithStats(graph.SearchQuery{
		NamePattern:  args.Name,
		Kinds:        kinds,
		Languages:    langs,
		FilePattern:  h.formatter.StripRepoPath(args.File),
		ExportedOnly: args.ExportedOnly,
		Limit:        limit,
	})

	items := make([]SymbolSummary, len(sr.Nodes))
	for i, n := range sr.Nodes {
		items[i] = h.toSymbolSummary(n)
	}

	return ToolPayload{
		Summary:      fmt.Sprintf("Found %d matching symbol(s)", sr.Total),
		Total:        sr.Total,
		MaxItems:     len(items),
		PreTruncated: sr.Truncated,
		BuildEnvelope: func(n int) (int, any) {
			n = min(n, len(items))
			return n, map[string]any{"symbols": items[:n]}
		},
	}, nil
}

func (h *Handler) handleCallNeighbor(v *graph.ReadView, rawArgs json.RawMessage, isCallers bool) (ToolPayload, error) {
	var args struct {
		Symbol        string `json:"symbol"`
		File          string `json:"file"`
		IncludeLikely *bool  `json:"include_likely"`
		Limit         int    `json:"limit"`
	}
	if err := decodeToolArgs(rawArgs, &args); err != nil {
		return ToolPayload{}, err
	}
	if strings.TrimSpace(args.Symbol) == "" {
		return ToolPayload{}, fmt.Errorf("%w: symbol is required", ErrInvalidArgs)
	}
	includeLikely := true
	if args.IncludeLikely != nil {
		includeLikely = *args.IncludeLikely
	}
	limit := clampLimit(args.Limit, defaultToolLimit, maxToolLimit)

	target, candidates, err := h.resolveTargetNode(v, args.Symbol, args.File)
	if err != nil {
		return ToolPayload{}, err
	}
	if len(candidates) > 1 {
		return h.buildAmbiguousPayload(args.Symbol, candidates), nil
	}
	if target.UID.IsZero() {
		return ToolPayload{
			Summary:  fmt.Sprintf("Symbol %q not found", args.Symbol),
			Total:    0,
			MaxItems: 0,
			BuildEnvelope: func(_ int) (int, any) {
				return 0, map[string]any{"found": false, "symbol": args.Symbol}
			},
		}, nil
	}

	var edges []graph.Edge
	if isCallers {
		edges = v.Callers(target.UID)
	} else {
		edges = v.Callees(target.UID)
	}

	var all []CallEdgeSummary
	var tiers []graph.ResolutionTier
	for _, e := range edges {
		if !includeLikely && !e.Tier.IsCertain() {
			continue
		}
		peerUID := e.Target
		if isCallers {
			peerUID = e.Source
		}
		peerNode, ok := v.GetNode(peerUID)
		if !ok {
			continue
		}
		all = append(all, CallEdgeSummary{
			Symbol:          h.toSymbolSummary(peerNode),
			EdgeKind:        e.Kind.String(),
			Tier:            e.Tier.String(),
			Confidence:      e.Confidence(),
			ConfidenceLabel: e.ConfidenceLabel(),
			Reason:          e.Label,
			Sites:           slices.Clone(e.Sites),
		})
		tiers = append(tiers, e.Tier)
	}

	total := len(all)
	capped := all
	preTruncated := false
	if len(capped) > limit {
		capped = capped[:limit]
		tiers = tiers[:limit]
		preTruncated = true
	}

	targetSummary := h.toSymbolSummary(target)
	key := "callees"
	dirWord := "callees of"
	if isCallers {
		key = "callers"
		dirWord = "callers of"
	}

	var hints UnresolvedHints
	if isCallers {
		hints = h.collectUnresolvedHints(v, target)
	}
	summary := fmt.Sprintf("%d %s %s", total, dirWord, target.QName)
	if isCallers && hints.Count > 0 {
		if total == 0 {
			summary = fmt.Sprintf("0 confirmed callers of %s (%d unresolved call site(s) might match)", target.QName, hints.Count)
		} else {
			summary = fmt.Sprintf("%d callers of %s (%d unresolved call site(s) might match)", total, target.QName, hints.Count)
		}
	}

	return ToolPayload{
		Summary:      summary,
		Total:        total,
		MaxItems:     len(capped),
		PreTruncated: preTruncated,
		ItemTiers:    tiers,
		BuildEnvelope: func(n int) (int, any) {
			n = min(n, len(capped))
			env := map[string]any{
				"found":  true,
				"target": targetSummary,
				key:      capped[:n],
			}
			if isCallers && hints.Count > 0 {
				env["unresolved_hints"] = hints
			}
			return n, env
		},
	}, nil
}

func (h *Handler) handleGetImpact(v *graph.ReadView, rawArgs json.RawMessage) (ToolPayload, error) {
	var args struct {
		Symbol   string `json:"symbol"`
		File     string `json:"file"`
		MaxDepth int    `json:"max_depth"`
		Limit    int    `json:"limit"`
	}
	if err := decodeToolArgs(rawArgs, &args); err != nil {
		return ToolPayload{}, err
	}
	if strings.TrimSpace(args.Symbol) == "" {
		return ToolPayload{}, fmt.Errorf("%w: symbol is required", ErrInvalidArgs)
	}
	depth := args.MaxDepth
	if depth <= 0 {
		depth = defaultMaxDepth
	}
	if depth > maxImpactMaxDepth {
		depth = maxImpactMaxDepth
	}
	limit := clampLimit(args.Limit, defaultToolLimit, maxToolLimit)

	target, candidates, err := h.resolveTargetNode(v, args.Symbol, args.File)
	if err != nil {
		return ToolPayload{}, err
	}
	if len(candidates) > 1 {
		return h.buildAmbiguousPayload(args.Symbol, candidates), nil
	}
	if target.UID.IsZero() {
		return ToolPayload{
			Summary:  fmt.Sprintf("Symbol %q not found", args.Symbol),
			Total:    0,
			MaxItems: 0,
			BuildEnvelope: func(_ int) (int, any) {
				return 0, map[string]any{"found": false, "symbol": args.Symbol}
			},
		}, nil
	}

	impact := v.ImpactAnalysis(target.UID, depth)
	items := make([]ImpactEntry, 0, len(impact.Impacted))
	tiers := make([]graph.ResolutionTier, 0, len(impact.Impacted))
	for _, imp := range impact.Impacted {
		var viaPath []string
		for _, ve := range imp.Via {
			if srcNode, ok := v.GetNode(ve.Source); ok && srcNode.QName != "" {
				viaPath = append(viaPath, srcNode.QName)
			}
		}
		items = append(items, ImpactEntry{
			Symbol:          h.toSymbolSummary(imp.Node),
			Depth:           imp.Depth,
			EffectiveTier:   imp.EffectiveTier.String(),
			Confidence:      imp.EffectiveTier.Confidence(),
			ConfidenceLabel: imp.EffectiveTier.Label(),
			Certain:         imp.Certain,
			Via:             viaPath,
		})
		tiers = append(tiers, imp.EffectiveTier)
	}

	affectedFiles := make([]string, len(impact.AffectedFiles))
	for i, af := range impact.AffectedFiles {
		affectedFiles[i] = h.formatter.StripRepoPath(af)
	}

	capped := items
	preTruncated := false
	if len(capped) > limit {
		capped = capped[:limit]
		tiers = tiers[:limit]
		preTruncated = true
	}
	targetSummary := h.toSymbolSummary(target)
	hints := h.collectUnresolvedHints(v, target)
	summary := fmt.Sprintf("%d impacted symbol(s) across %d file(s) within depth %d", impact.TotalImpacted, len(affectedFiles), depth)
	if hints.Count > 0 {
		if impact.TotalImpacted == 0 {
			summary = fmt.Sprintf("0 confirmed impacted symbols for %s (%d unresolved call site(s) might match)", target.QName, hints.Count)
		} else {
			summary = fmt.Sprintf("%s (%d unresolved call site(s) might match)", summary, hints.Count)
		}
	}

	return ToolPayload{
		Summary:      summary,
		Total:        impact.TotalImpacted,
		MaxItems:     len(capped),
		PreTruncated: preTruncated,
		ItemTiers:    tiers,
		BuildEnvelope: func(n int) (int, any) {
			n = min(n, len(capped))
			env := map[string]any{
				"found":          true,
				"target":         targetSummary,
				"max_depth":      impact.MaxDepth,
				"direct_callers": impact.DirectCallers,
				"total_impacted": impact.TotalImpacted,
				"certain_count":  impact.CertainCount,
				"likely_count":   impact.LikelyCount,
				"affected_files": affectedFiles,
				"impacted":       capped[:n],
			}
			if hints.Count > 0 {
				env["unresolved_hints"] = hints
			}
			return n, env
		},
	}, nil
}

func (h *Handler) collectUnresolvedHints(v *graph.ReadView, target graph.Node) UnresolvedHints {
	if v == nil || target.UID.IsZero() || target.Name == "" {
		return UnresolvedHints{}
	}
	const maxSamples = 5
	var samples []UnresolvedSiteSample
	total := 0
	for _, cs := range v.GetCallSites() {
		if cs.Status == graph.CallStatusExternal {
			continue
		}
		matched := cs.Status == graph.CallStatusAmbiguous && slices.Contains(cs.Candidates, target.UID)
		if !matched && callSiteMatchesTargetName(cs.LookupKey, cs.CalleeExpr, target.Name) {
			matched = true
		}
		if !matched {
			continue
		}
		total++
		if len(samples) < maxSamples {
			samples = append(samples, UnresolvedSiteSample{
				File:       h.formatter.StripRepoPath(cs.File),
				Line:       cs.Line,
				Column:     cs.Column,
				CalleeExpr: cs.CalleeExpr,
				Status:     cs.Status.String(),
				Reason:     cs.Reason,
			})
		}
	}
	return UnresolvedHints{Count: total, Samples: samples}
}

func callSiteMatchesTargetName(lookupKey, calleeExpr, targetName string) bool {
	if targetName == "" {
		return false
	}
	if idx := strings.LastIndex(lookupKey, "::"); idx >= 0 {
		if lookupKey[idx+2:] == targetName {
			return true
		}
	} else if lookupKey == targetName {
		return true
	}
	if calleeExpr == targetName || strings.HasSuffix(calleeExpr, "."+targetName) || strings.HasSuffix(calleeExpr, "::"+targetName) {
		return true
	}
	return false
}

func (h *Handler) handleFileOutline(v *graph.ReadView, rawArgs json.RawMessage) (ToolPayload, error) {
	var args struct {
		File           string `json:"file"`
		IncludePrivate *bool  `json:"include_private"`
	}
	if err := decodeToolArgs(rawArgs, &args); err != nil {
		return ToolPayload{}, err
	}
	relPath := h.formatter.StripRepoPath(args.File)
	if relPath == "" {
		return ToolPayload{}, fmt.Errorf("%w: file is required", ErrInvalidArgs)
	}
	includePrivate := true
	if args.IncludePrivate != nil {
		includePrivate = *args.IncludePrivate
	}

	nodes := v.FileNodes(relPath)
	if len(nodes) == 0 {
		return ToolPayload{
			Summary:  fmt.Sprintf("File %q not indexed", relPath),
			Total:    0,
			MaxItems: 0,
			BuildEnvelope: func(_ int) (int, any) {
				return 0, map[string]any{"found": false, "file": relPath}
			},
		}, nil
	}

	var fileLang, moduleName string
	var fileUID graph.UID
	var symbols []SymbolSummary

	for _, n := range nodes {
		switch n.Kind {
		case graph.KindFile:
			fileUID = n.UID
			if fileLang == "" {
				fileLang = n.Language
			}
		case graph.KindModule:
			moduleName = n.Name
		case graph.KindExternal:
			continue
		default:
			if !includePrivate && !n.Exported {
				continue
			}
			if fileLang == "" {
				fileLang = n.Language
			}
			symbols = append(symbols, h.toSymbolSummary(n))
		}
	}

	var imports []string
	if !fileUID.IsZero() {
		for _, e := range v.GetOutgoingEdges(fileUID, graph.EdgeImports) {
			if e.Label != "" {
				imports = append(imports, e.Label)
			} else if tgt, ok := v.GetNode(e.Target); ok {
				imports = append(imports, h.formatter.StripRepoPath(tgt.Name))
			}
		}
	}
	slices.Sort(imports)

	return ToolPayload{
		Summary:  fmt.Sprintf("Outline for %s: %d symbol(s), %d import(s)", relPath, len(symbols), len(imports)),
		Total:    len(symbols),
		MaxItems: len(symbols),
		BuildEnvelope: func(n int) (int, any) {
			n = min(n, len(symbols))
			return n, map[string]any{
				"found":    true,
				"file":     relPath,
				"language": fileLang,
				"module":   moduleName,
				"imports":  imports,
				"symbols":  symbols[:n],
			}
		},
	}, nil
}

func (h *Handler) handleSearchCode(v *graph.ReadView, rawArgs json.RawMessage) (ToolPayload, error) {
	var args struct {
		NamePattern   string   `json:"name_pattern"`
		QNamePattern  string   `json:"qname_pattern"`
		Kinds         []string `json:"kinds"`
		Languages     []string `json:"languages"`
		FilePattern   string   `json:"file_pattern"`
		ExportedOnly  bool     `json:"exported_only"`
		MinComplexity uint16   `json:"min_complexity"`
		MaxComplexity uint16   `json:"max_complexity"`
		Offset        int      `json:"offset"`
		Limit         int      `json:"limit"`
	}
	if err := decodeToolArgs(rawArgs, &args); err != nil {
		return ToolPayload{}, err
	}

	limit := clampLimit(args.Limit, defaultToolLimit, maxToolLimit)
	var kinds []graph.NodeKind
	for _, rawKind := range args.Kinds {
		k, err := graph.ParseNodeKind(rawKind)
		if err != nil {
			return ToolPayload{}, fmt.Errorf("%w: %v", ErrInvalidArgs, err)
		}
		kinds = append(kinds, k)
	}
	var langs []string
	for _, rawLang := range args.Languages {
		if norm := parser.NormalizeLanguage(rawLang); norm != "" {
			langs = append(langs, norm)
		}
	}

	sr := v.SearchWithStats(graph.SearchQuery{
		NamePattern:   args.NamePattern,
		QNamePattern:  args.QNamePattern,
		Kinds:         kinds,
		Languages:     langs,
		FilePattern:   h.formatter.StripRepoPath(args.FilePattern),
		ExportedOnly:  args.ExportedOnly,
		MinComplexity: args.MinComplexity,
		MaxComplexity: args.MaxComplexity,
		Offset:        args.Offset,
		Limit:         limit,
	})

	items := make([]SymbolSummary, len(sr.Nodes))
	for i, n := range sr.Nodes {
		items[i] = h.toSymbolSummary(n)
	}

	return ToolPayload{
		Summary:      fmt.Sprintf("Search matched %d symbol(s)", sr.Total),
		Total:        sr.Total,
		MaxItems:     len(items),
		PreTruncated: sr.Truncated,
		BuildEnvelope: func(n int) (int, any) {
			n = min(n, len(items))
			return n, map[string]any{"results": items[:n]}
		},
	}, nil
}

func (h *Handler) handleRepoOverview(v *graph.ReadView, rawArgs json.RawMessage) (ToolPayload, error) {
	var args struct {
		IncludeTopComplex *bool `json:"include_top_complex"`
		Limit             int   `json:"limit"`
	}
	if err := decodeToolArgs(rawArgs, &args); err != nil {
		return ToolPayload{}, err
	}
	includeTop := true
	if args.IncludeTopComplex != nil {
		includeTop = *args.IncludeTopComplex
	}
	limit := clampLimit(args.Limit, 10, 50)

	stats := v.Stats()

	nodesByKind := make(map[string]int, len(stats.NodesByKind))
	for k, cnt := range stats.NodesByKind {
		nodesByKind[k.String()] = cnt
	}
	edgesByKind := make(map[string]int, len(stats.EdgesByKind))
	for k, cnt := range stats.EdgesByKind {
		edgesByKind[k.String()] = cnt
	}
	edgesByTier := make(map[string]int, len(stats.EdgesByTier))
	var tierSample []graph.ResolutionTier
	for t, cnt := range stats.EdgesByTier {
		edgesByTier[t.String()] = cnt
		if t != graph.TierStructural {
			for i := 0; i < min(cnt, 32); i++ {
				tierSample = append(tierSample, t)
			}
		}
	}

	type complexityDist struct {
		Low1To5     int `json:"low_1_5"`
		Medium6To10 int `json:"medium_6_10"`
		High11To20  int `json:"high_11_20"`
		VeryHigh21P int `json:"very_high_gt_20"`
	}
	var compDist complexityDist
	var fnNodes []graph.Node
	for _, n := range v.AllNodes() {
		if n.Kind != graph.KindFunction {
			continue
		}
		fnNodes = append(fnNodes, n)
		c := n.Complexity
		switch {
		case c <= 5:
			compDist.Low1To5++
		case c <= 10:
			compDist.Medium6To10++
		case c <= 20:
			compDist.High11To20++
		default:
			compDist.VeryHigh21P++
		}
	}

	var topComplex []SymbolSummary
	if includeTop && len(fnNodes) > 0 {
		slices.SortFunc(fnNodes, func(a, b graph.Node) int {
			if c := cmp.Compare(b.Complexity, a.Complexity); c != 0 {
				return c
			}
			return cmp.Compare(a.QName, b.QName)
		})
		capN := min(limit, len(fnNodes))
		topComplex = make([]SymbolSummary, capN)
		for i := 0; i < capN; i++ {
			topComplex[i] = h.toSymbolSummary(fnNodes[i])
		}
	}

	totalTop := len(topComplex)
	return ToolPayload{
		Summary:     fmt.Sprintf("%d file(s), %d symbol node(s), %d edge(s), %.1f%% resolution coverage", stats.FileCount, stats.NodeCount, stats.EdgeCount, stats.ResolutionCoverage*100),
		Total:       totalTop,
		MaxItems:    totalTop,
		StaticTiers: tierSample,
		BuildEnvelope: func(n int) (int, any) {
			n = min(n, len(topComplex))
			return n, map[string]any{
				"file_count":              stats.FileCount,
				"node_count":              stats.NodeCount,
				"edge_count":              stats.EdgeCount,
				"call_site_count":         stats.CallSiteCount,
				"resolution_coverage":     roundCoverage(stats.ResolutionCoverage),
				"nodes_by_kind":           nodesByKind,
				"edges_by_kind":           edgesByKind,
				"edges_by_tier":           edgesByTier,
				"nodes_by_language":       stats.NodesByLanguage,
				"complexity_distribution": compDist,
				"top_complex_functions":   topComplex[:n],
			}
		},
	}, nil
}

func (h *Handler) resolveTargetNode(v *graph.ReadView, symbolRef, fileHint string) (graph.Node, []SymbolSummary, error) {
	ref := strings.TrimSpace(symbolRef)
	relHint := h.formatter.StripRepoPath(fileHint)

	// 1. Try 32-char UID hex lookup directly.
	if len(ref) == 32 {
		if uid, err := graph.ParseUID(ref); err == nil {
			if n, ok := v.GetNode(uid); ok {
				return n, nil, nil
			}
		}
	}

	// 2. Exact qualified name lookup.
	var matches []graph.Node
	if qNodes := v.GetByQName(ref); len(qNodes) > 0 {
		matches = filterNonFileNodes(qNodes, relHint)
	}

	// 3. Exact short name lookup.
	if len(matches) == 0 {
		if nNodes := v.GetByName(ref); len(nNodes) > 0 {
			matches = filterNonFileNodes(nNodes, relHint)
		}
	}

	if len(matches) == 0 {
		return graph.Node{}, nil, nil
	}
	if len(matches) == 1 {
		return matches[0], nil, nil
	}

	// Prefer KindFunction when overloaded/shadowed across kinds, still guarding multiple funcs.
	var fnOnly []graph.Node
	for _, m := range matches {
		if m.Kind == graph.KindFunction {
			fnOnly = append(fnOnly, m)
		}
	}
	if len(fnOnly) == 1 {
		return fnOnly[0], nil, nil
	}

	cands := make([]SymbolSummary, len(matches))
	for i, m := range matches {
		cands[i] = h.toSymbolSummary(m)
	}
	return graph.Node{}, cands, nil
}

func (h *Handler) buildAmbiguousPayload(symbol string, candidates []SymbolSummary) ToolPayload {
	return ToolPayload{
		Summary:      fmt.Sprintf("Ambiguous symbol %q matched %d candidates; specify qualified_name or file", symbol, len(candidates)),
		Total:        len(candidates),
		MaxItems:     len(candidates),
		PreTruncated: false,
		BuildEnvelope: func(n int) (int, any) {
			n = min(n, len(candidates))
			return n, map[string]any{
				"found":      false,
				"ambiguous":  true,
				"symbol":     symbol,
				"candidates": candidates[:n],
			}
		},
	}
}

func (h *Handler) toSymbolSummary(n graph.Node) SymbolSummary {
	return SymbolSummary{
		UID:        n.UID.String(),
		Kind:       n.Kind.String(),
		Name:       n.Name,
		QName:      n.QName,
		File:       h.formatter.StripRepoPath(n.File),
		StartLine:  n.StartLine,
		EndLine:    n.EndLine,
		Language:   n.Language,
		Type:       n.Type,
		Receiver:   n.Receiver,
		ReturnType: n.ReturnType,
		Exported:   n.Exported,
		Complexity: n.Complexity,
		Docstring:  n.Docstring,
		Bases:      slices.Clone(n.Bases),
		Decorators: slices.Clone(n.Decorators),
	}
}

func filterNonFileNodes(nodes []graph.Node, fileHint string) []graph.Node {
	var out []graph.Node
	for _, n := range nodes {
		if n.Kind == graph.KindFile || n.Kind == graph.KindModule || n.Kind == graph.KindExternal {
			continue
		}
		if fileHint != "" && n.File != fileHint {
			continue
		}
		out = append(out, n)
	}
	slices.SortFunc(out, func(a, b graph.Node) int {
		if c := cmp.Compare(a.File, b.File); c != 0 {
			return c
		}
		if c := cmp.Compare(a.StartLine, b.StartLine); c != 0 {
			return c
		}
		return cmp.Compare(a.QName, b.QName)
	})
	return out
}

func decodeToolArgs(raw json.RawMessage, dst any) error {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		trimmed = "{}"
	}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidArgs, err)
	}
	return nil
}

func clampLimit(v, defVal, maxVal int) int {
	if v <= 0 {
		return defVal
	}
	if v > maxVal {
		return maxVal
	}
	return v
}
