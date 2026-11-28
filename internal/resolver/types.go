package resolver

import (
	"context"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/roshankumar0036singh/gographctx/internal/graph"
	"github.com/roshankumar0036singh/gographctx/internal/parser"
)

// TypeMap stores inferred symbol and expression types partitioned cleanly by
// enclosing scope QName so local variables never leak across sibling functions.
type TypeMap struct {
	scopes      map[string]map[string]TypeInfo
	classFields map[string]map[string]TypeInfo
}

// NewTypeMap initializes an empty scoped TypeMap.
func NewTypeMap() *TypeMap {
	return &TypeMap{
		scopes:      make(map[string]map[string]TypeInfo),
		classFields: make(map[string]map[string]TypeInfo),
	}
}

// Set records info for varName inside scopeQName ("" represents module/file scope).
// Higher-confidence bindings replace lower-confidence guesses; equal-confidence
// explicit bindings update in source order.
func (tm *TypeMap) Set(scopeQName, varName string, info TypeInfo) {
	if tm == nil || strings.TrimSpace(varName) == "" || strings.TrimSpace(info.Name) == "" {
		return
	}
	bucket, ok := tm.scopes[scopeQName]
	if !ok {
		bucket = make(map[string]TypeInfo)
		tm.scopes[scopeQName] = bucket
	}
	if prev, exists := bucket[varName]; exists && prev.Confidence > info.Confidence {
		return
	}
	bucket[varName] = info
}

// SetField records a known field/property type for classOrTypeName.
func (tm *TypeMap) SetField(classOrTypeName, fieldName string, info TypeInfo) {
	if tm == nil || strings.TrimSpace(classOrTypeName) == "" || strings.TrimSpace(fieldName) == "" || strings.TrimSpace(info.Name) == "" {
		return
	}
	bucket, ok := tm.classFields[classOrTypeName]
	if !ok {
		bucket = make(map[string]TypeInfo)
		tm.classFields[classOrTypeName] = bucket
	}
	bucket[fieldName] = info
}

// Lookup searches varName strictly along scopeQName's lexical/class hierarchy:
// exact scope -> parent "::" scopes -> file/module globals (""). It never leaks
// across sibling functions or classes.
func (tm *TypeMap) Lookup(scopeQName, varName string) (TypeInfo, bool) {
	if tm == nil || strings.TrimSpace(varName) == "" {
		return TypeInfo{}, false
	}
	cur := scopeQName
	for {
		if bucket, ok := tm.scopes[cur]; ok {
			if info, found := bucket[varName]; found {
				return info, true
			}
		}
		if cur == "" {
			break
		}
		idx := strings.LastIndex(cur, "::")
		if idx < 0 {
			cur = ""
		} else {
			cur = cur[:idx]
		}
	}
	return TypeInfo{}, false
}

// LookupField resolves fieldName on classOrTypeName (matching short name or QName).
func (tm *TypeMap) LookupField(classOrTypeName, fieldName string) (TypeInfo, bool) {
	if tm == nil || classOrTypeName == "" || fieldName == "" {
		return TypeInfo{}, false
	}
	if fields, ok := tm.classFields[classOrTypeName]; ok {
		if info, found := fields[fieldName]; found {
			return info, true
		}
	}
	base := StripTypeDecorations(classOrTypeName)
	if base != classOrTypeName {
		if fields, ok := tm.classFields[base]; ok {
			if info, found := fields[fieldName]; found {
				return info, true
			}
		}
	}
	return TypeInfo{}, false
}

// LookupReceiver resolves a possibly dotted receiver expression (such as "client",
// "self.cache", or "s.repo.store") inside scopeQName.
func (tm *TypeMap) LookupReceiver(scopeQName, receiverExpr string) (TypeInfo, bool) {
	if tm == nil {
		return TypeInfo{}, false
	}
	expr := strings.TrimSpace(receiverExpr)
	if expr == "" {
		return TypeInfo{}, false
	}
	if direct, ok := tm.Lookup(scopeQName, expr); ok {
		return direct, true
	}
	parts := strings.Split(expr, ".")
	if len(parts) < 2 {
		return TypeInfo{}, false
	}
	curInfo, ok := tm.Lookup(scopeQName, strings.TrimSpace(parts[0]))
	if !ok {
		return TypeInfo{}, false
	}
	for _, field := range parts[1:] {
		fieldName := strings.TrimSpace(field)
		if fieldName == "" {
			return TypeInfo{}, false
		}
		next, found := tm.LookupField(curInfo.Name, fieldName)
		if !found && curInfo.QName != "" {
			next, found = tm.LookupField(curInfo.QName, fieldName)
		}
		if !found {
			return TypeInfo{}, false
		}
		curInfo = next
	}
	return curInfo, true
}

// LookupFlat queries a flattened map produced by ToFlatMap using the same
// lexical scope chain as TypeMap.Lookup.
func LookupFlat(flat map[string]TypeInfo, scopeQName, varName string) (TypeInfo, bool) {
	if len(flat) == 0 || strings.TrimSpace(varName) == "" {
		return TypeInfo{}, false
	}
	cur := scopeQName
	for {
		key := varName
		if cur != "" {
			key = cur + "::" + varName
		}
		if info, ok := flat[key]; ok {
			return info, true
		}
		if cur == "" {
			break
		}
		idx := strings.LastIndex(cur, "::")
		if idx < 0 {
			cur = ""
		} else {
			cur = cur[:idx]
		}
	}
	return TypeInfo{}, false
}

// ToFlatMap flattens all scoped bindings using "scope::name" keys (and "name"
// for file/module-scope variables) so LanguageResolver.InferTypes preserves strict
// scope isolation.
func (tm *TypeMap) ToFlatMap() map[string]TypeInfo {
	if tm == nil {
		return map[string]TypeInfo{}
	}
	total := 0
	for _, bucket := range tm.scopes {
		total += len(bucket)
	}
	out := make(map[string]TypeInfo, total)
	for scope, bucket := range tm.scopes {
		for name, info := range bucket {
			key := name
			if scope != "" {
				key = scope + "::" + name
			}
			out[key] = info
		}
	}
	return out
}

// InferTypes builds a scope-aware TypeMap for file from parameters, receivers,
// class fields, module globals, and local variable assignments/constructors.
func InferTypes(ctx context.Context, file *parser.ParsedFile, rc *ResolutionContext) (*TypeMap, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	tm := NewTypeMap()
	if file == nil {
		return tm, nil
	}
	if rc == nil {
		rc = &ResolutionContext{}
	}

	knownClasses := make(map[string]parser.ParsedClass, len(file.Classes)*2)
	fnReturnsByName := make(map[string]string, len(file.Functions))
	ambiguousShortReturns := make(map[string]bool)
	fnReturnsByQName := make(map[string]string, len(file.Functions))

	// Pass 1: Classes, fields, module variables, function params & receivers.
	for _, cls := range file.Classes {
		knownClasses[cls.Name] = cls
		knownClasses[cls.QName] = cls
		for _, field := range cls.Fields {
			ft := StripTypeDecorations(field.Type)
			if ft == "" {
				continue
			}
			info := TypeInfo{
				Name:       ft,
				Package:    file.Package,
				Origin:     OriginField,
				Confidence: 0.96,
				Line:       field.StartLine,
			}
			tm.SetField(cls.Name, field.Name, info)
			tm.SetField(cls.QName, field.Name, info)
			tm.Set(cls.QName, field.Name, info)
			tm.Set(cls.QName, "self."+field.Name, info)
			tm.Set(cls.QName, "this."+field.Name, info)
		}
	}

	for _, v := range file.Variables {
		vt := StripTypeDecorations(v.Type)
		if vt == "" {
			continue
		}
		tm.Set("", v.Name, TypeInfo{
			Name:        vt,
			ElementType: ExtractContainerElementType(v.Type),
			Package:     file.Package,
			Origin:      OriginAssignment,
			Confidence:  0.95,
			Line:        v.StartLine,
		})
	}

	for _, fn := range file.Functions {
		if fn.ReturnType != "" {
			fnReturnsByQName[fn.QName] = fn.ReturnType
			if fn.ReceiverType != "" {
				fnReturnsByName[fn.ReceiverType+"::"+fn.Name] = fn.ReturnType
			}
			if !ambiguousShortReturns[fn.Name] {
				if prev, exists := fnReturnsByName[fn.Name]; !exists {
					fnReturnsByName[fn.Name] = fn.ReturnType
				} else if prev != fn.ReturnType {
					delete(fnReturnsByName, fn.Name)
					ambiguousShortReturns[fn.Name] = true
				}
			}
		}

		if fn.ReceiverType != "" {
			recvType := StripTypeDecorations(fn.ReceiverType)
			recvInfo := TypeInfo{
				Name:       recvType,
				Package:    file.Package,
				Origin:     OriginParameter,
				Confidence: 1.00,
				Line:       fn.StartLine,
			}
			tm.Set(fn.QName, "this", recvInfo)
			tm.Set(fn.QName, "self", recvInfo)
			tm.Set(fn.QName, "cls", recvInfo)
			if fn.Receiver != "" && !strings.ContainsAny(fn.Receiver, "*&[]()") {
				tm.Set(fn.QName, fn.Receiver, recvInfo)
			}
		}

		for _, p := range fn.Params {
			pt := StripTypeDecorations(p.Type)
			if p.Name == "" || pt == "" {
				continue
			}
			tm.Set(fn.QName, p.Name, TypeInfo{
				Name:        pt,
				ElementType: ExtractContainerElementType(p.Type),
				Package:     file.Package,
				Origin:      OriginParameter,
				Confidence:  0.96,
				Line:        fn.StartLine,
			})
		}
	}

	// Pass 2: Iterative inference over local assignments & constructors.
	const maxIterations = 3
	for iter := 0; iter < maxIterations; iter++ {
		changed := false
		for _, lv := range file.LocalVars {
			info, ok := inferLocalVarType(file, lv, tm, rc, knownClasses, fnReturnsByName, fnReturnsByQName)
			if !ok {
				continue
			}
			prev, hadPrev := tm.Lookup(lv.ScopeQName, lv.Name)
			tm.Set(lv.ScopeQName, lv.Name, info)
			promoteSelfOrThisAttribute(tm, lv, info)
			if !hadPrev || prev.Name != info.Name || info.Confidence > prev.Confidence {
				changed = true
			}
		}
		if !changed {
			break
		}
	}

	return tm, nil
}

func inferLocalVarType(
	file *parser.ParsedFile,
	lv parser.ParsedLocalVar,
	tm *TypeMap,
	rc *ResolutionContext,
	knownClasses map[string]parser.ParsedClass,
	fnReturnsByName map[string]string,
	fnReturnsByQName map[string]string,
) (TypeInfo, bool) {
	if lv.ExplicitType != "" {
		rawType := lv.ExplicitType
		elemType := ExtractContainerElementType(rawType)
		base := StripTypeDecorations(rawType)
		if lv.UnwrapElement && elemType != "" {
			base = StripTypeDecorations(elemType)
			elemType = ExtractContainerElementType(elemType)
		}
		if base != "" {
			origin := OriginAssignment
			conf := float32(0.95)
			if lv.IsConstructor {
				origin = OriginConstructor
			}
			return TypeInfo{
				Name:        base,
				ElementType: elemType,
				Package:     file.Package,
				Origin:      origin,
				Confidence:  conf,
				Line:        lv.Line,
			}, true
		}
	}

	if lv.IsConstructor && lv.InitCallee != "" {
		base := StripTypeDecorations(lv.InitCallee)
		if lv.InitCallee == "new" && lv.InitReceiver != "" {
			base = StripTypeDecorations(lv.InitReceiver)
		}
		if base != "" {
			return TypeInfo{
				Name:       base,
				Package:    file.Package,
				Origin:     OriginConstructor,
				Confidence: 0.93,
				Line:       lv.Line,
			}, true
		}
	}

	if lv.InitCallee != "" {
		if retType, found := resolveCallReturnType(file, lv, tm, rc, fnReturnsByName, fnReturnsByQName); found {
			part := pickReturnElement(retType, lv.ResultIndex)
			elemType := ExtractContainerElementType(part)
			base := StripTypeDecorations(part)
			if lv.UnwrapElement && elemType != "" {
				base = StripTypeDecorations(elemType)
				elemType = ExtractContainerElementType(elemType)
			}
			if isUsefulInferredType(base) {
				return TypeInfo{
					Name:        base,
					ElementType: elemType,
					Package:     file.Package,
					Origin:      OriginReturnType,
					Confidence:  0.88,
					Line:        lv.Line,
				}, true
			}
		}

		// Go constructor heuristic: NewClient(...) -> Client (or New<Struct>).
		if file.Language == "go" && lv.ResultIndex == 0 && isGoNewConstructorName(lv.InitCallee) {
			candidate := lv.InitCallee[3:]
			return TypeInfo{
				Name:       candidate,
				Package:    file.Package,
				Origin:     OriginConstructor,
				Confidence: 0.85,
				Line:       lv.Line,
			}, true
		}

		// Python/capitalized class instantiation heuristic: x = Client()
		if _, isKnown := knownClasses[lv.InitCallee]; isKnown {
			return TypeInfo{
				Name:       lv.InitCallee,
				Package:    file.Package,
				Origin:     OriginConstructor,
				Confidence: 0.92,
				Line:       lv.Line,
			}, true
		}
		if nodes := rc.LookupByName(lv.InitCallee); len(nodes) > 0 {
			for _, n := range nodes {
				if n.Kind == graph.KindClass || n.Kind == graph.KindStruct {
					return TypeInfo{
						Name:       n.Name,
						QName:      n.QName,
						Package:    file.Package,
						Origin:     OriginConstructor,
						Confidence: 0.90,
						Line:       lv.Line,
					}, true
				}
			}
		}
	}

	if lv.InitAlias != "" {
		if aliased, ok := tm.LookupReceiver(lv.ScopeQName, lv.InitAlias); ok {
			if lv.UnwrapElement && aliased.ElementType != "" {
				elemBase := StripTypeDecorations(aliased.ElementType)
				if isUsefulInferredType(elemBase) {
					return TypeInfo{
						Name:        elemBase,
						ElementType: ExtractContainerElementType(aliased.ElementType),
						Package:     aliased.Package,
						Origin:      OriginAssignment,
						Confidence:  0.84,
						Line:        lv.Line,
					}, true
				}
			}
			aliased.Origin = OriginAssignment
			if aliased.Confidence > 0.82 {
				aliased.Confidence = 0.82
			}
			aliased.Line = lv.Line
			return aliased, true
		}
		for _, fn := range file.Functions {
			if fn.Name == lv.InitAlias && !fn.IsMethod {
				return TypeInfo{
					Name:          fn.Name,
					QName:         fn.QName,
					CallableQName: fn.QName,
					Package:       file.Package,
					Origin:        OriginCallable,
					Confidence:    0.90,
					Line:          lv.Line,
				}, true
			}
		}
		if rc != nil {
			if nodes := rc.LookupByName(lv.InitAlias); len(nodes) == 1 && nodes[0].Kind == graph.KindFunction {
				return TypeInfo{
					Name:          nodes[0].Name,
					QName:         nodes[0].QName,
					CallableQName: nodes[0].QName,
					Package:       file.Package,
					Origin:        OriginCallable,
					Confidence:    0.85,
					Line:          lv.Line,
				}, true
			}
		}
	}

	return TypeInfo{}, false
}

func resolveCallReturnType(
	file *parser.ParsedFile,
	lv parser.ParsedLocalVar,
	tm *TypeMap,
	rc *ResolutionContext,
	fnReturnsByName map[string]string,
	fnReturnsByQName map[string]string,
) (string, bool) {
	if lv.InitReceiver != "" {
		if recvInfo, ok := tm.LookupReceiver(lv.ScopeQName, lv.InitReceiver); ok {
			if ret, ok := fnReturnsByName[recvInfo.Name+"::"+lv.InitCallee]; ok {
				return ret, true
			}
			if nodes := rc.LookupByName(lv.InitCallee); len(nodes) > 0 {
				for _, n := range nodes {
					if n.Kind == graph.KindFunction && StripTypeDecorations(n.Receiver) == recvInfo.Name && n.ReturnType != "" {
						return n.ReturnType, true
					}
				}
			}
		}
		// Qualified package/static call such as Service.from_env() or pkg.NewClient().
		if ret, ok := fnReturnsByName[lv.InitReceiver+"::"+lv.InitCallee]; ok {
			return ret, true
		}
	}

	if ret, ok := fnReturnsByName[lv.InitCallee]; ok {
		return ret, true
	}
	if ret, ok := fnReturnsByQName[lv.InitCallee]; ok {
		return ret, true
	}
	if nodes := rc.LookupByName(lv.InitCallee); len(nodes) > 0 {
		var retType string
		for _, n := range nodes {
			if n.Kind == graph.KindFunction && n.ReturnType != "" {
				if retType == "" {
					retType = n.ReturnType
				} else if retType != n.ReturnType {
					return "", false // Ambiguous return types across overloads
				}
			}
		}
		if retType != "" {
			return retType, true
		}
	}
	return "", false
}

func promoteSelfOrThisAttribute(tm *TypeMap, lv parser.ParsedLocalVar, info TypeInfo) {
	var attrName string
	switch {
	case strings.HasPrefix(lv.Name, "self."):
		attrName = strings.TrimPrefix(lv.Name, "self.")
	case strings.HasPrefix(lv.Name, "this."):
		attrName = strings.TrimPrefix(lv.Name, "this.")
	default:
		return
	}
	if attrName == "" {
		return
	}
	idx := strings.LastIndex(lv.ScopeQName, "::")
	if idx <= 0 {
		return
	}
	classScope := lv.ScopeQName[:idx]
	tm.Set(classScope, lv.Name, info)
	tm.SetField(classScope, attrName, info)
	if lastSep := strings.LastIndex(classScope, "::"); lastSep >= 0 {
		tm.SetField(classScope[lastSep+2:], attrName, info)
	}
}

func pickReturnElement(retType string, idx uint8) string {
	trimmed := strings.TrimSpace(retType)
	if !strings.HasPrefix(trimmed, "(") || !strings.HasSuffix(trimmed, ")") {
		if idx == 0 {
			return trimmed
		}
		return ""
	}
	inner := strings.TrimSpace(trimmed[1 : len(trimmed)-1])
	parts := splitTopLevelCommas(inner)
	if int(idx) >= len(parts) {
		return ""
	}
	part := strings.TrimSpace(parts[idx])
	// Handle named Go returns like "(order *Order, err error)" -> take last token or type part.
	fields := strings.Fields(part)
	if len(fields) >= 2 && isSimpleIdent(fields[0]) {
		return strings.Join(fields[1:], " ")
	}
	return part
}

func splitTopLevelCommas(s string) []string {
	var parts []string
	depth := 0
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(', '[', '<', '{':
			depth++
		case ')', ']', '>', '}':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				parts = append(parts, strings.TrimSpace(s[start:i]))
				start = i + 1
			}
		}
	}
	parts = append(parts, strings.TrimSpace(s[start:]))
	return parts
}

func isSimpleIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if i == 0 && !unicode.IsLetter(r) && r != '_' {
			return false
		}
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' {
			return false
		}
	}
	return true
}

func isGoNewConstructorName(name string) bool {
	if !strings.HasPrefix(name, "New") || len(name) <= 3 {
		return false
	}
	r, _ := utf8.DecodeRuneInString(name[3:])
	return unicode.IsUpper(r)
}

func isUsefulInferredType(base string) bool {
	switch strings.ToLower(strings.TrimSpace(base)) {
	case "", "void", "none", "null", "undefined", "error", "bool", "boolean", "int", "int64", "string", "str", "usize", "()", "any", "object":
		return false
	default:
		return true
	}
}

// ExtractContainerElementType extracts the inner element/item type T from typed
// container annotations such as `list[T]`, `List[T]`, `Sequence[T]`, `Vec<T>`, `&[T]`, `[]T`, `map[K]V`.
func ExtractContainerElementType(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	for {
		prev := s
		s = strings.TrimSpace(strings.TrimPrefix(s, "&"))
		if strings.HasPrefix(s, "'") {
			if sp := strings.IndexByte(s, ' '); sp > 0 {
				s = strings.TrimSpace(s[sp+1:])
			}
		}
		s = strings.TrimSpace(strings.TrimPrefix(s, "mut "))
		s = strings.TrimSpace(strings.TrimPrefix(s, "*"))
		if s == prev {
			break
		}
	}
	if strings.HasPrefix(s, "[]") {
		return strings.TrimSpace(s[2:])
	}
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		inner := strings.TrimSpace(s[1 : len(s)-1])
		if semi := strings.IndexByte(inner, ';'); semi >= 0 {
			return strings.TrimSpace(inner[:semi])
		}
		return inner
	}
	if strings.HasPrefix(s, "map[") {
		depth := 0
		for i := 3; i < len(s); i++ {
			switch s[i] {
			case '[':
				depth++
			case ']':
				depth--
				if depth == 0 && i+1 < len(s) {
					return strings.TrimSpace(s[i+1:])
				}
			}
		}
	}
	for _, wrap := range []string{"list[", "List[", "Sequence[", "Iterable[", "Iterator[", "Set[", "set[", "Vec<", "VecDeque<", "HashSet<", "BTreeSet<", "List<", "Set<", "Collection<", "Iterable<", "Stream<", "Array<", "ReadonlyArray<"} {
		if strings.HasPrefix(s, wrap) && (strings.HasSuffix(s, "]") || strings.HasSuffix(s, ">")) {
			return strings.TrimSpace(s[len(wrap) : len(s)-1])
		}
	}
	for _, dictWrap := range []string{"dict[", "Dict[", "Mapping[", "HashMap<", "BTreeMap<", "Map<"} {
		if strings.HasPrefix(s, dictWrap) && (strings.HasSuffix(s, "]") || strings.HasSuffix(s, ">")) {
			inner := s[len(dictWrap) : len(s)-1]
			parts := splitTopLevelCommas(inner)
			if len(parts) >= 2 {
				return strings.TrimSpace(parts[1])
			}
		}
	}
	if strings.HasPrefix(s, "chan ") {
		return strings.TrimSpace(strings.TrimPrefix(s, "chan "))
	}
	return ""
}
