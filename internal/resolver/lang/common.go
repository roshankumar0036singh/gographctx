package lang

import (
	"slices"
	"strings"

	"github.com/roshankumar0036singh/gographctx/internal/graph"
	"github.com/roshankumar0036singh/gographctx/internal/parser"
	"github.com/roshankumar0036singh/gographctx/internal/resolver"
)

// ResolveCallerUID resolves the canonical UID for callerQName from rc.View when
// available, falling back deterministically to GenerateNodeUID(KindFunction, callerQName, 0).
func ResolveCallerUID(rc *resolver.ResolutionContext, callerQName string, line uint32) graph.UID {
	if rc != nil && rc.View != nil && callerQName != "" {
		nodes := rc.View.GetByQName(callerQName)
		if len(nodes) == 1 {
			return nodes[0].UID
		}
		if len(nodes) > 1 {
			// Match sibling functions (such as multiple Go init() functions) by closest span.
			var best graph.Node
			bestDist := uint32(0xffffffff)
			for _, n := range nodes {
				if line >= n.StartLine && (n.EndLine == 0 || line <= n.EndLine) {
					return n.UID
				}
				var dist uint32
				if line >= n.StartLine {
					dist = line - n.StartLine
				} else {
					dist = n.StartLine - line
				}
				if dist < bestDist {
					bestDist = dist
					best = n
				}
			}
			if !best.UID.IsZero() {
				return best.UID
			}
		}
	}
	return graph.GenerateNodeUID(graph.KindFunction, callerQName, 0)
}

// FindSameFileFuncUID locates a function/method in file matching predicate and
// returns its UID from rc.View (or deterministic fallback UID).
func FindSameFileFuncUID(rc *resolver.ResolutionContext, file *parser.ParsedFile, fnName, recvType string) (graph.Node, bool) {
	if file == nil {
		return graph.Node{}, false
	}
	for _, fn := range file.Functions {
		if fn.Name != fnName {
			continue
		}
		if recvType != "" && fn.ReceiverType != recvType {
			continue
		}
		if recvType == "" && fn.IsMethod {
			continue
		}
		return nodeForParsedFunc(rc, file.Path, file.Language, fn), true
	}
	return graph.Node{}, false
}

func nodeForParsedFunc(rc *resolver.ResolutionContext, filePath, language string, fn parser.ParsedFunction) graph.Node {
	if rc != nil && rc.View != nil {
		for _, n := range rc.View.GetByQName(fn.QName) {
			if n.File == "" || n.File == filePath {
				return n
			}
		}
	}
	return graph.Node{
		UID:        graph.GenerateNodeUID(fn.EffectiveKind(), fn.QName, 0),
		Kind:       fn.EffectiveKind(),
		Name:       fn.Name,
		QName:      fn.QName,
		File:       filePath,
		StartLine:  fn.StartLine,
		EndLine:    fn.EndLine,
		Language:   language,
		Receiver:   fn.Receiver,
		Exported:   fn.Exported,
		Params:     slices.Clone(fn.Params),
		ReturnType: fn.ReturnType,
	}
}

// PackageScopePrefix extracts the leading package/module prefix before "::" from a QName.
func PackageScopePrefix(qname string) string {
	idx := strings.Index(qname, "::")
	if idx < 0 {
		return ""
	}
	return qname[:idx]
}

// FilterUniqueFunctionMatches filters nodes to KindFunction (excluding interface/trait signatures
// when concreteOnly is true) matching an optional predicate.
func FilterUniqueFunctionMatches(nodes []graph.Node, concreteOnly bool, pred func(graph.Node) bool) (graph.Node, bool, []graph.UID) {
	return resolver.MatchUniqueNode(nodes, func(n graph.Node) bool {
		if n.Kind != graph.KindFunction {
			return false
		}
		if concreteOnly && hasInterfaceMethodDecorator(n.Decorators) {
			return false
		}
		if pred != nil && !pred(n) {
			return false
		}
		return true
	})
}

func hasInterfaceMethodDecorator(decs []string) bool {
	for _, d := range decs {
		if strings.HasSuffix(d, ":interface_method") || d == "rust:trait_method" {
			return true
		}
	}
	return false
}

// AddParsedFileToGraph materializes nodes from pf into g cleanly (useful for
// multi-file resolver tests and standalone resolution pipelines prior to Phase 21).
func AddParsedFileToGraph(g *graph.Graph, pf *parser.ParsedFile) error {
	if g == nil || pf == nil {
		return nil
	}
	var frag graph.FileFragment
	frag.File = pf.Path

	frag.Nodes = append(frag.Nodes, graph.Node{
		Kind:     graph.KindFile,
		Name:     pf.Path,
		QName:    pf.Path,
		File:     pf.Path,
		Language: pf.Language,
	})

	counts := make(map[string]uint16)
	nextOcc := func(kind graph.NodeKind, qname string) uint16 {
		key := kind.String() + "|" + qname
		occ := counts[key]
		counts[key] = occ + 1
		return occ
	}

	for _, cls := range pf.Classes {
		k := cls.EffectiveKind()
		occ := nextOcc(k, cls.QName)
		frag.Nodes = append(frag.Nodes, graph.Node{
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
		})
	}
	for _, fn := range pf.Functions {
		k := fn.EffectiveKind()
		occ := nextOcc(k, fn.QName)
		recv := fn.ReceiverType
		if recv == "" {
			recv = fn.Receiver
		}
		frag.Nodes = append(frag.Nodes, graph.Node{
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
		})
	}
	for _, v := range pf.Variables {
		occ := nextOcc(graph.KindVariable, v.QName)
		frag.Nodes = append(frag.Nodes, graph.Node{
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
		})
	}
	for _, ta := range pf.TypeAliases {
		occ := nextOcc(graph.KindTypeAlias, ta.QName)
		frag.Nodes = append(frag.Nodes, graph.Node{
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
		})
	}

	return g.MergeFragments([]graph.FileFragment{frag})
}
