package pipeline

import (
	"cmp"
	"context"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/roshankumar0036singh/gographctx/internal/graph"
	"github.com/roshankumar0036singh/gographctx/internal/parser"
	"github.com/roshankumar0036singh/gographctx/internal/resolver"
)

// BuildSymbolFragment extracts all structural nodes (File, Module, Classes/Structs/
// Interfaces/Enums, Functions, Fields, Variables, TypeAliases) and structural
// CONTAINS edges for one ParsedFile in deterministic order.
func BuildSymbolFragment(pf *parser.ParsedFile) (graph.FileFragment, error) {
	if pf == nil {
		return graph.FileFragment{}, fmt.Errorf("pipeline: nil ParsedFile")
	}

	var frag graph.FileFragment
	frag.File = pf.Path

	fileNode := graph.Node{
		Kind:     graph.KindFile,
		Name:     pf.Path,
		QName:    pf.Path,
		File:     pf.Path,
		Language: pf.Language,
	}
	fileNode.UID = graph.ComputeNodeUID(fileNode)
	frag.Nodes = append(frag.Nodes, fileNode)

	modName := pf.Package
	if modName == "" {
		modName = strings.TrimSuffix(path.Base(pf.Path), path.Ext(pf.Path))
	}
	var modUID graph.UID
	if modName != "" {
		modNode := graph.Node{
			Kind:     graph.KindModule,
			Name:     modName,
			QName:    "mod:" + pf.Path,
			File:     pf.Path,
			Language: pf.Language,
		}
		modNode.UID = graph.ComputeNodeUID(modNode)
		modUID = modNode.UID
		frag.Nodes = append(frag.Nodes, modNode)
		frag.Edges = append(frag.Edges, graph.Edge{
			Source: fileNode.UID,
			Target: modUID,
			Kind:   graph.EdgeContains,
			Tier:   resolver.TierStructural,
		})
	}

	counts := make(map[string]uint16)
	nextOcc := func(kind graph.NodeKind, qname string) uint16 {
		key := kind.String() + "|" + qname
		occ := counts[key]
		counts[key] = occ + 1
		return occ
	}

	classUIDByQName := make(map[string]graph.UID, len(pf.Classes))

	// Emit Classes / Structs / Interfaces / Enums deterministically sorted by (StartLine, QName).
	classes := slices.Clone(pf.Classes)
	slices.SortFunc(classes, func(a, b parser.ParsedClass) int {
		if c := cmp.Compare(a.StartLine, b.StartLine); c != 0 {
			return c
		}
		return cmp.Compare(a.QName, b.QName)
	})
	for _, cls := range classes {
		k := cls.EffectiveKind()
		occ := nextOcc(k, cls.QName)
		n := graph.Node{
			Kind:       k,
			Name:       cls.Name,
			QName:      cls.QName,
			Occurrence: occ,
			File:       pf.Path,
			StartLine:  cls.StartLine,
			EndLine:    cls.EndLine,
			Language:   pf.Language,
			Exported:   cls.Exported,
			Decorators: slices.Clone(cls.Decorators),
			Docstring:  cls.Docstring,
			Bases:      slices.Clone(cls.Bases),
		}
		n.UID = graph.ComputeNodeUID(n)
		classUIDByQName[cls.QName] = n.UID
		frag.Nodes = append(frag.Nodes, n)
		frag.Edges = append(frag.Edges, graph.Edge{
			Source: fileNode.UID,
			Target: n.UID,
			Kind:   graph.EdgeContains,
			Tier:   resolver.TierStructural,
		})

		// Emit Fields contained inside cls.
		fields := slices.Clone(cls.Fields)
		slices.SortFunc(fields, func(a, b parser.ParsedField) int {
			if c := cmp.Compare(a.StartLine, b.StartLine); c != 0 {
				return c
			}
			return cmp.Compare(a.QName, b.QName)
		})
		for _, f := range fields {
			fOcc := nextOcc(graph.KindProperty, f.QName)
			fn := graph.Node{
				Kind:       graph.KindProperty,
				Name:       f.Name,
				QName:      f.QName,
				Occurrence: fOcc,
				File:       pf.Path,
				StartLine:  f.StartLine,
				EndLine:    f.EndLine,
				Language:   pf.Language,
				Type:       f.Type,
				Receiver:   cls.Name,
				Exported:   f.Exported,
				Decorators: slices.Clone(f.Decorators),
				Docstring:  f.Docstring,
			}
			fn.UID = graph.ComputeNodeUID(fn)
			frag.Nodes = append(frag.Nodes, fn)
			frag.Edges = append(frag.Edges, graph.Edge{
				Source: n.UID,
				Target: fn.UID,
				Kind:   graph.EdgeContains,
				Tier:   resolver.TierStructural,
			})
		}
	}

	// Link nested inner classes cleanly to outer classes when parent QName exists.
	for _, cls := range classes {
		if lastSep := strings.LastIndex(cls.QName, "::"); lastSep > 0 {
			parentQName := cls.QName[:lastSep]
			if parentUID, ok := classUIDByQName[parentQName]; ok {
				childUID := classUIDByQName[cls.QName]
				if parentUID != childUID {
					frag.Edges = append(frag.Edges, graph.Edge{
						Source: parentUID,
						Target: childUID,
						Kind:   graph.EdgeContains,
						Tier:   resolver.TierStructural,
					})
				}
			}
		}
	}

	// Emit Functions & Methods sorted deterministically.
	funcs := slices.Clone(pf.Functions)
	slices.SortFunc(funcs, func(a, b parser.ParsedFunction) int {
		if c := cmp.Compare(a.StartLine, b.StartLine); c != 0 {
			return c
		}
		if c := cmp.Compare(a.QName, b.QName); c != 0 {
			return c
		}
		return cmp.Compare(a.Signature, b.Signature)
	})
	for _, fn := range funcs {
		k := fn.EffectiveKind()
		occ := nextOcc(k, fn.QName)
		recv := fn.ReceiverType
		if recv == "" {
			recv = fn.Receiver
		}
		n := graph.Node{
			Kind:          k,
			Name:          fn.Name,
			QName:         fn.QName,
			Occurrence:    occ,
			SignatureHash: graph.ComputeSignatureHash(fn.Params, fn.ReturnType),
			File:          pf.Path,
			StartLine:     fn.StartLine,
			EndLine:       fn.EndLine,
			Language:      pf.Language,
			Receiver:      recv,
			Exported:      fn.Exported,
			Params:        slices.Clone(fn.Params),
			ReturnType:    fn.ReturnType,
			Decorators:    slices.Clone(fn.Decorators),
			Docstring:     fn.Docstring,
			Complexity:    fn.Complexity,
		}
		n.UID = graph.ComputeNodeUID(n)
		frag.Nodes = append(frag.Nodes, n)

		parentUID := fileNode.UID
		if idx := strings.LastIndex(fn.QName, "::"); idx > 0 {
			if clsUID, ok := classUIDByQName[fn.QName[:idx]]; ok {
				parentUID = clsUID
			}
		}
		frag.Edges = append(frag.Edges, graph.Edge{
			Source: parentUID,
			Target: n.UID,
			Kind:   graph.EdgeContains,
			Tier:   resolver.TierStructural,
		})
	}

	// Emit top-level Variables sorted deterministically.
	vars := slices.Clone(pf.Variables)
	slices.SortFunc(vars, func(a, b parser.ParsedVariable) int {
		if c := cmp.Compare(a.StartLine, b.StartLine); c != 0 {
			return c
		}
		return cmp.Compare(a.QName, b.QName)
	})
	for _, v := range vars {
		occ := nextOcc(graph.KindVariable, v.QName)
		n := graph.Node{
			Kind:       graph.KindVariable,
			Name:       v.Name,
			QName:      v.QName,
			Occurrence: occ,
			File:       pf.Path,
			StartLine:  v.StartLine,
			EndLine:    v.EndLine,
			Language:   pf.Language,
			Type:       v.Type,
			Exported:   v.Exported,
			Docstring:  v.Docstring,
		}
		n.UID = graph.ComputeNodeUID(n)
		frag.Nodes = append(frag.Nodes, n)
		frag.Edges = append(frag.Edges, graph.Edge{
			Source: fileNode.UID,
			Target: n.UID,
			Kind:   graph.EdgeContains,
			Tier:   resolver.TierStructural,
		})
	}

	// Emit TypeAliases sorted deterministically.
	aliases := slices.Clone(pf.TypeAliases)
	slices.SortFunc(aliases, func(a, b parser.ParsedTypeAlias) int {
		if c := cmp.Compare(a.StartLine, b.StartLine); c != 0 {
			return c
		}
		return cmp.Compare(a.QName, b.QName)
	})
	for _, ta := range aliases {
		occ := nextOcc(graph.KindTypeAlias, ta.QName)
		n := graph.Node{
			Kind:       graph.KindTypeAlias,
			Name:       ta.Name,
			QName:      ta.QName,
			Occurrence: occ,
			File:       pf.Path,
			StartLine:  ta.StartLine,
			EndLine:    ta.EndLine,
			Language:   pf.Language,
			Type:       ta.UnderlyingType,
			Exported:   ta.Exported,
			Docstring:  ta.Docstring,
		}
		n.UID = graph.ComputeNodeUID(n)
		frag.Nodes = append(frag.Nodes, n)
		frag.Edges = append(frag.Edges, graph.Edge{
			Source: fileNode.UID,
			Target: n.UID,
			Kind:   graph.EdgeContains,
			Tier:   resolver.TierStructural,
		})
	}

	_ = modUID
	return frag, nil
}

// BuildResolvedFragment builds the complete FileFragment (structural nodes/CONTAINS
// edges plus IMPORTS, CALLS/LIKELY_CALLS, INHERITS/IMPLEMENTS/DECORATED_BY edges,
// and unresolved/ambiguous CallSites) for one file against existing view nodes.
func BuildResolvedFragment(pf *parser.ParsedFile, fr *resolver.FileResolution, view *graph.ReadView) (graph.FileFragment, error) {
	frag, err := BuildSymbolFragment(pf)
	if err != nil {
		return graph.FileFragment{}, err
	}
	if fr == nil {
		return frag, nil
	}

	localUIDs := make(map[graph.UID]struct{}, len(frag.Nodes))
	for _, n := range frag.Nodes {
		localUIDs[n.UID] = struct{}{}
	}
	hasEndpoint := func(uid graph.UID) bool {
		if uid.IsZero() {
			return false
		}
		if _, ok := localUIDs[uid]; ok {
			return true
		}
		return view != nil && view.HasNode(uid)
	}

	fileUID := frag.Nodes[0].UID
	extByKey := make(map[string]graph.UID)
	seenImports := make(map[graph.UID]struct{})

	// 1. Materialize IMPORTS edges (internal TargetFile or per-file external node).
	for _, imp := range fr.Imports {
		if !imp.IsExternal && imp.TargetFile != "" {
			if targetFileUID, ok := lookupFileNodeUID(imp.TargetFile, pf.Path, fileUID, view); ok && targetFileUID != fileUID {
				if _, dup := seenImports[targetFileUID]; !dup {
					seenImports[targetFileUID] = struct{}{}
					frag.Edges = append(frag.Edges, graph.Edge{
						Source: fileUID,
						Target: targetFileUID,
						Kind:   graph.EdgeImports,
						Tier:   resolver.TierQualifiedImport,
						Label:  imp.ImportPath,
					})
				}
			}
			continue
		}
		if imp.IsExternal && imp.ImportPath != "" {
			extQName := "ext:" + pf.Path + "::" + imp.ImportPath
			extUID, seen := extByKey[extQName]
			if !seen {
				extNode := graph.Node{
					Kind:     graph.KindExternal,
					Name:     imp.ImportPath,
					QName:    extQName,
					File:     pf.Path,
					Language: pf.Language,
				}
				extNode.UID = graph.ComputeNodeUID(extNode)
				extUID = extNode.UID
				extByKey[extQName] = extUID
				localUIDs[extUID] = struct{}{}
				frag.Nodes = append(frag.Nodes, extNode)
				frag.Edges = append(frag.Edges, graph.Edge{
					Source: fileUID,
					Target: extUID,
					Kind:   graph.EdgeContains,
					Tier:   resolver.TierStructural,
				})
			}
			if extUID != fileUID {
				if _, dup := seenImports[extUID]; !dup {
					seenImports[extUID] = struct{}{}
					frag.Edges = append(frag.Edges, graph.Edge{
						Source: fileUID,
						Target: extUID,
						Kind:   graph.EdgeImports,
						Tier:   resolver.TierQualifiedImport,
						Label:  imp.ImportPath,
					})
				}
			}
		}
	}

	// 2. Materialize CALLS / LIKELY_CALLS edges coalescing repeated sites per (Source, Target, Kind).
	type callEdgeKey struct {
		src  graph.UID
		tgt  graph.UID
		kind graph.EdgeKind
	}
	callEdgeMap := make(map[callEdgeKey]*graph.Edge)
	for _, rc := range fr.Resolved {
		if err := rc.Validate(); err != nil {
			continue
		}
		if !hasEndpoint(rc.CallerUID) || !hasEndpoint(rc.TargetUID) {
			continue
		}
		col := rc.Column
		if col == 0 {
			col = 1
		}
		site := graph.CallSiteRef{
			Line:   rc.Line,
			Column: col,
			Tier:   rc.Tier,
		}
		key := callEdgeKey{src: rc.CallerUID, tgt: rc.TargetUID, kind: rc.EdgeKind}
		if existing, exists := callEdgeMap[key]; exists {
			if rc.Tier < existing.Tier {
				existing.Tier = rc.Tier
				existing.Label = rc.Method
			}
			existing.Sites = append(existing.Sites, site)
			continue
		}
		callEdgeMap[key] = &graph.Edge{
			Source: rc.CallerUID,
			Target: rc.TargetUID,
			Kind:   rc.EdgeKind,
			Tier:   rc.Tier,
			Label:  rc.Method,
			Sites:  []graph.CallSiteRef{site},
		}
	}
	for _, e := range callEdgeMap {
		slices.SortFunc(e.Sites, func(a, b graph.CallSiteRef) int {
			if c := cmp.Compare(a.Line, b.Line); c != 0 {
				return c
			}
			if c := cmp.Compare(a.Column, b.Column); c != 0 {
				return c
			}
			return cmp.Compare(a.Tier, b.Tier)
		})
		frag.Edges = append(frag.Edges, *e)
	}

	// 3. Materialize INHERITS / IMPLEMENTS / DECORATED_BY edges.
	for _, l := range fr.Links {
		if err := l.Validate(); err != nil {
			continue
		}
		if !hasEndpoint(l.SourceUID) || !hasEndpoint(l.TargetUID) {
			continue
		}
		frag.Edges = append(frag.Edges, graph.Edge{
			Source: l.SourceUID,
			Target: l.TargetUID,
			Kind:   l.Kind,
			Tier:   l.Tier,
			Label:  l.Method,
		})
	}

	// 4. Materialize unresolved/ambiguous CallSites for incremental lookup re-resolution.
	for _, uc := range fr.Unresolved {
		if uc.CallerUID.IsZero() || !hasEndpoint(uc.CallerUID) {
			continue
		}
		line := uc.Line
		if line == 0 {
			line = 1
		}
		col := uc.Column
		if col == 0 {
			col = 1
		}
		cs := graph.CallSite{
			Caller:     uc.CallerUID,
			File:       pf.Path,
			Line:       line,
			Column:     col,
			CalleeExpr: uc.CalleeExpr,
			LookupKey:  uc.LookupKey,
			Candidates: slices.Clone(uc.Candidates),
			Status:     uc.Status,
			Tier:       uc.Tier,
			Reason:     uc.Reason,
		}
		if cs.CalleeExpr == "" {
			cs.CalleeExpr = "<unknown>"
		}
		if cs.LookupKey == "" {
			cs.LookupKey = cs.CalleeExpr
		}
		if err := cs.Validate(); err == nil {
			frag.CallSites = append(frag.CallSites, cs)
		}
	}

	// Canonicalize edge and call-site order inside fragment for deterministic output.
	slices.SortFunc(frag.Edges, func(a, b graph.Edge) int {
		if c := a.Source.Compare(b.Source); c != 0 {
			return c
		}
		if c := a.Target.Compare(b.Target); c != 0 {
			return c
		}
		return cmp.Compare(a.Kind, b.Kind)
	})
	slices.SortFunc(frag.CallSites, func(a, b graph.CallSite) int {
		if c := cmp.Compare(a.File, b.File); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Line, b.Line); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Column, b.Column); c != 0 {
			return c
		}
		return cmp.Compare(a.CalleeExpr, b.CalleeExpr)
	})

	return frag, nil
}

// BuildGraph performs deterministic two-pass graph construction across all parsed
// files and their resolution outputs: first loading structural symbols & CONTAINS
// edges, then merging IMPORTS, CALLS, links, and unresolved CallSites.
func BuildGraph(ctx context.Context, parsed []*parser.ParsedFile, resolved map[string]*resolver.FileResolution) (*graph.Graph, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	sortedFiles := make([]*parser.ParsedFile, 0, len(parsed))
	for _, pf := range parsed {
		if pf != nil {
			sortedFiles = append(sortedFiles, pf)
		}
	}
	slices.SortFunc(sortedFiles, func(a, b *parser.ParsedFile) int {
		return cmp.Compare(a.Path, b.Path)
	})

	g := graph.New()

	// Pass 1: Insert all structural file/module/symbol nodes and CONTAINS edges.
	symFrags := make([]graph.FileFragment, 0, len(sortedFiles))
	for _, pf := range sortedFiles {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		frag, err := BuildSymbolFragment(pf)
		if err != nil {
			return nil, err
		}
		symFrags = append(symFrags, frag)
	}
	if err := g.MergeFragments(symFrags); err != nil {
		return nil, fmt.Errorf("merge structural fragments: %w", err)
	}

	if len(resolved) == 0 {
		return g, nil
	}

	// Pass 2: Build resolved edges & CallSites against populated ReadView, then merge.
	var resFrags []graph.FileFragment
	var buildErr error
	g.View(func(v *graph.ReadView) {
		resFrags = make([]graph.FileFragment, 0, len(sortedFiles))
		for _, pf := range sortedFiles {
			if err := ctx.Err(); err != nil {
				buildErr = err
				return
			}
			fr := resolved[pf.Path]
			if fr == nil {
				continue
			}
			frag, err := BuildResolvedFragment(pf, fr, v)
			if err != nil {
				buildErr = err
				return
			}
			resFrags = append(resFrags, frag)
		}
	})
	if buildErr != nil {
		return nil, buildErr
	}

	for _, frag := range resFrags {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, err := g.ReplaceFile(frag.File, frag.Nodes, frag.Edges, frag.CallSites); err != nil {
			return nil, fmt.Errorf("apply resolved fragment %q: %w", frag.File, err)
		}
	}

	return g, nil
}

func lookupFileNodeUID(targetFile, selfPath string, selfUID graph.UID, view *graph.ReadView) (graph.UID, bool) {
	if targetFile == "" {
		return graph.UID{}, false
	}
	if targetFile == selfPath {
		return selfUID, true
	}
	if view == nil {
		return graph.UID{}, false
	}
	for _, n := range view.GetByQName(targetFile) {
		if n.Kind == graph.KindFile {
			return n.UID, true
		}
	}
	return graph.UID{}, false
}
