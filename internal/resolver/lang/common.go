package lang

import (
	"cmp"
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

// FindEnclosingClass walks parent "::" segments from callerQName to locate the
// innermost enclosing class/struct/interface definition locally or in rc.View.
func FindEnclosingClass(callerQName string, localClassesByQName map[string]parser.ParsedClass, rc *resolver.ResolutionContext) parser.ParsedClass {
	cur := callerQName
	for cur != "" {
		idx := strings.LastIndex(cur, "::")
		if idx <= 0 {
			break
		}
		parent := cur[:idx]
		if localClassesByQName != nil {
			if cls, ok := localClassesByQName[parent]; ok {
				return cls
			}
		}
		if rc != nil && rc.View != nil {
			for _, n := range rc.View.GetByQName(parent) {
				if n.Kind == graph.KindClass || n.Kind == graph.KindStruct || n.Kind == graph.KindInterface {
					return parser.ParsedClass{Name: n.Name, QName: n.QName, Kind: n.Kind, Bases: n.Bases}
				}
			}
		}
		cur = parent
	}
	return parser.ParsedClass{}
}

// LookupScopedReceiverType resolves direct or dotted receiver chains (including
// `self.attr`, `cls.attr`, `this.attr`, and cross-file struct/class fields).
func LookupScopedReceiverType(
	rc *resolver.ResolutionContext,
	callerQName, enclosingClsQName, recv string,
	types map[string]resolver.TypeInfo,
	localClassesByQName map[string]parser.ParsedClass,
) (resolver.TypeInfo, bool) {
	if info, ok := resolver.LookupFlat(types, callerQName, recv); ok {
		return info, true
	}
	if enclosingClsQName != "" {
		if info, ok := resolver.LookupFlat(types, enclosingClsQName, recv); ok {
			return info, true
		}
	}

	parts := strings.Split(recv, ".")
	if len(parts) < 2 {
		return resolver.TypeInfo{}, false
	}

	startIdx := 1
	var cur resolver.TypeInfo
	var ok bool

	first := strings.TrimSpace(parts[0])
	if first == "self" || first == "cls" || first == "this" {
		twoPart := first + "." + strings.TrimSpace(parts[1])
		if cur, ok = resolver.LookupFlat(types, callerQName, twoPart); ok {
			startIdx = 2
		} else if enclosingClsQName != "" {
			if cur, ok = resolver.LookupFlat(types, enclosingClsQName, strings.TrimSpace(parts[1])); ok {
				startIdx = 2
			}
		}
	}
	if !ok {
		cur, ok = resolver.LookupFlat(types, callerQName, first)
		if !ok {
			return resolver.TypeInfo{}, false
		}
	}

	for _, seg := range parts[startIdx:] {
		fieldName := strings.TrimSpace(seg)
		if fieldName == "" {
			return resolver.TypeInfo{}, false
		}
		if info, found := resolver.LookupFlat(types, cur.QName, fieldName); found {
			cur = info
			continue
		}
		if info, found := resolver.LookupFlat(types, cur.Name, fieldName); found {
			cur = info
			continue
		}
		if localClassesByQName != nil {
			if cls, found := localClassesByQName[cur.QName]; found {
				if next, matched := lookupFieldOnParsedClass(cls, fieldName, cur.Package); matched {
					cur = next
					continue
				}
			}
		}
		if next, matched := lookupCrossFileFieldType(rc, cur, fieldName); matched {
			cur = next
			continue
		}
		return resolver.TypeInfo{}, false
	}

	return cur, true
}

func lookupFieldOnParsedClass(cls parser.ParsedClass, fieldName, fallbackPkg string) (resolver.TypeInfo, bool) {
	for _, f := range cls.Fields {
		if f.Name == fieldName && f.Type != "" {
			return resolver.TypeInfo{Name: resolver.StripTypeDecorations(f.Type), Package: fallbackPkg}, true
		}
	}
	return resolver.TypeInfo{}, false
}

func lookupCrossFileFieldType(rc *resolver.ResolutionContext, cur resolver.TypeInfo, fieldName string) (resolver.TypeInfo, bool) {
	if rc == nil || rc.View == nil {
		return resolver.TypeInfo{}, false
	}
	var classQNames []string
	if cur.QName != "" {
		classQNames = append(classQNames, cur.QName)
	} else if cur.Package != "" && cur.Name != "" {
		classQNames = append(classQNames, cur.Package+"::"+resolver.StripTypeDecorations(cur.Name))
	}
	for _, cq := range classQNames {
		for _, n := range rc.View.GetByQName(cq) {
			if pf := rc.LookupFile(n.File); pf != nil {
				for _, pc := range pf.Classes {
					if pc.QName == n.QName {
						if info, ok := lookupFieldOnParsedClass(pc, fieldName, PackageScopePrefix(n.QName)); ok {
							return info, true
						}
					}
				}
			}
		}
	}
	return resolver.TypeInfo{}, false
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

// ResolveClassOrSymbolUID looks up the UID for qname in rc.View matching allowed kinds,
// falling back deterministically to GenerateNodeUID when View is unavailable or unindexed.
func ResolveClassOrSymbolUID(rc *resolver.ResolutionContext, fallbackKind graph.NodeKind, qname string) graph.UID {
	if rc != nil && rc.View != nil && qname != "" {
		for _, n := range rc.View.GetByQName(qname) {
			if n.Kind == fallbackKind || n.Kind == graph.KindClass || n.Kind == graph.KindStruct || n.Kind == graph.KindInterface {
				return n.UID
			}
		}
		if nodes := rc.View.GetByQName(qname); len(nodes) > 0 {
			return nodes[0].UID
		}
	}
	return graph.GenerateNodeUID(fallbackKind, qname, 0)
}

// MakeResolvedLink constructs a ResolvedLink targeting target with kind and tier metadata.
func MakeResolvedLink(
	srcUID graph.UID,
	srcQName string,
	target graph.Node,
	kind graph.EdgeKind,
	tier graph.ResolutionTier,
	line uint32,
	method string,
) resolver.ResolvedLink {
	return resolver.ResolvedLink{
		SourceUID:   srcUID,
		SourceQName: srcQName,
		TargetUID:   target.UID,
		TargetQName: target.QName,
		Kind:        kind,
		Tier:        tier,
		Line:        line,
		Method:      method,
	}
}

// NormalizeDecoratorTarget strips leading '@', trailing call argument lists '(...)',
// and generic parameters '<...>' from decorator/annotation text, returning "" for
// internal parser tag markers (such as "ts:", "js:", "rust:", "java:", "go:", "metaclass:").
func NormalizeDecoratorTarget(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	for _, prefix := range []string{"ts:", "js:", "rust:", "java:", "go:", "metaclass:"} {
		if strings.HasPrefix(s, prefix) {
			return ""
		}
	}
	s = strings.TrimSpace(strings.TrimPrefix(s, "@"))
	if idx := strings.IndexByte(s, '('); idx > 0 {
		s = strings.TrimSpace(s[:idx])
	}
	if idx := strings.IndexByte(s, '<'); idx > 0 {
		s = strings.TrimSpace(s[:idx])
	}
	return s
}

// ResolveTransitiveChain walks multi-level inheritance or implementation chains
// from startQName using nextTargets (with cycle prevention) and returns all
// reachable ancestor nodes in breadth-first discovery order.
func ResolveTransitiveChain(rc *resolver.ResolutionContext, startQName string) []graph.Node {
	if rc == nil || rc.View == nil || startQName == "" {
		return nil
	}
	visited := map[string]struct{}{startQName: {}}
	queue := []string{startQName}
	var chain []graph.Node

	for len(queue) > 0 {
		curQName := queue[0]
		queue = queue[1:]

		for _, srcNode := range rc.View.GetByQName(curQName) {
			for _, edge := range rc.View.GetOutgoingEdges(srcNode.UID, graph.EdgeInherits, graph.EdgeImplements) {
				target, ok := rc.View.GetNode(edge.Target)
				if !ok || target.QName == "" {
					continue
				}
				if _, seen := visited[target.QName]; seen {
					continue
				}
				visited[target.QName] = struct{}{}
				chain = append(chain, target)
				queue = append(queue, target.QName)
			}
		}
	}
	return chain
}

// DedupeResolvedLinks deduplicates links by (SourceUID, TargetUID, Kind) and sorts
// them deterministically by (Line, Kind, SourceQName, TargetQName).
func DedupeResolvedLinks(links []resolver.ResolvedLink) []resolver.ResolvedLink {
	if len(links) == 0 {
		return nil
	}
	type key struct {
		src  graph.UID
		tgt  graph.UID
		kind graph.EdgeKind
	}
	seen := make(map[key]struct{}, len(links))
	out := make([]resolver.ResolvedLink, 0, len(links))
	for _, l := range links {
		if err := l.Validate(); err != nil {
			continue
		}
		k := key{src: l.SourceUID, tgt: l.TargetUID, kind: l.Kind}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, l)
	}
	slices.SortFunc(out, func(a, b resolver.ResolvedLink) int {
		if c := cmp.Compare(a.Line, b.Line); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Kind, b.Kind); c != 0 {
			return c
		}
		if c := cmp.Compare(a.SourceQName, b.SourceQName); c != 0 {
			return c
		}
		return cmp.Compare(a.TargetQName, b.TargetQName)
	})
	return out
}
