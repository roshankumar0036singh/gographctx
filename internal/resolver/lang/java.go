package lang

import (
	"context"
	"strings"

	"github.com/roshankumar0036singh/gographctx/internal/graph"
	"github.com/roshankumar0036singh/gographctx/internal/parser"
	"github.com/roshankumar0036singh/gographctx/internal/resolver"
)

// JavaResolver implements LanguageResolver for Java packages, handling package ->
// directory imports, wildcard & static imports, class/interface inheritance
// hierarchies, inner classes, constructors, and overloaded methods.
type JavaResolver struct{}

// NewJavaResolver returns a Java LanguageResolver.
func NewJavaResolver() *JavaResolver {
	return &JavaResolver{}
}

// Language returns "java".
func (r *JavaResolver) Language() string {
	return "java"
}

// ResolveImports resolves Java explicit, wildcard, and static import statements
// against indexed project packages and classes.
func (r *JavaResolver) ResolveImports(ctx context.Context, file *parser.ParsedFile, rc *resolver.ResolutionContext) ([]resolver.ResolvedImport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if file == nil {
		return nil, nil
	}
	if rc == nil {
		rc = &resolver.ResolutionContext{}
	}

	out := make([]resolver.ResolvedImport, 0, len(file.Imports))
	for _, imp := range file.Imports {
		ri := resolver.ResolvedImport{
			SourceFile:    file.Path,
			ImportPath:    imp.Path,
			Alias:         imp.Alias,
			ImportedNames: append([]string(nil), imp.Names...),
			Line:          imp.Line,
			IsWildcard:    imp.IsWildcard,
		}
		if ri.Alias == "" && len(imp.Names) == 1 {
			ri.Alias = imp.Names[0]
		}

		// Case 1: Package import (`import com.example.store.*;` or `import com.example.store.Repo;`)
		if hasJavaPackage(rc, imp.Path) {
			ri.TargetPackage = imp.Path
			if len(imp.Names) == 1 {
				if f, ok := findJavaTypeFile(rc, imp.Path, imp.Names[0]); ok {
					ri.TargetFile = f
				}
			}
			if ri.TargetFile == "" {
				if files := javaPackageFiles(rc, imp.Path); len(files) > 0 {
					ri.TargetFile = files[0]
				}
			}
			ri.IsExternal = false
			out = append(out, ri)
			continue
		}

		// Case 2: Static import or inner-class import (`import static pkg.Class.method;` or `import pkg.Outer.Inner;`)
		if pkg, clsQName, targetFile, ok := resolveJavaStaticOrInnerTarget(rc, imp.Path); ok {
			ri.TargetPackage = pkg
			ri.TargetFile = targetFile
			_ = clsQName
			ri.IsExternal = false
			out = append(out, ri)
			continue
		}

		ri.IsExternal = true
		out = append(out, ri)
	}

	return out, nil
}

// InferTypes builds scoped Java variable, parameter, field, constructor, and
// factory return types enriched with imported package coordinates.
func (r *JavaResolver) InferTypes(ctx context.Context, file *parser.ParsedFile, rc *resolver.ResolutionContext) (map[string]resolver.TypeInfo, error) {
	if file == nil {
		return map[string]resolver.TypeInfo{}, nil
	}
	if rc == nil {
		rc = &resolver.ResolutionContext{}
	}
	tm, err := resolver.InferTypes(ctx, file, rc)
	if err != nil {
		return nil, err
	}

	imports, err := r.ResolveImports(ctx, file, rc)
	if err != nil {
		return nil, err
	}
	pkgScope := javaPackageScope(file)
	bindings := buildJavaBindings(file, imports, rc)

	localClassByName := make(map[string]parser.ParsedClass, len(file.Classes))
	for _, cls := range file.Classes {
		localClassByName[cls.Name] = cls
	}

	// Enrich class field and parameter types with resolved package coordinates.
	for _, cls := range file.Classes {
		for _, f := range cls.Fields {
			if f.Type == "" {
				continue
			}
			base := resolver.StripTypeDecorations(f.Type)
			if clsNode, targetPkg, ok := resolveJavaClassNode(rc, bindings, pkgScope, base, localClassByName); ok {
				info := resolver.TypeInfo{
					Name:       clsNode.Name,
					QName:      clsNode.QName,
					Package:    targetPkg,
					Origin:     resolver.OriginField,
					Confidence: 0.96,
					Line:       f.StartLine,
				}
				tm.Set(cls.QName, f.Name, info)
				tm.Set(cls.QName, "this."+f.Name, info)
				tm.SetField(cls.Name, f.Name, info)
				tm.SetField(cls.QName, f.Name, info)
			}
		}
	}

	for _, fn := range file.Functions {
		for _, p := range fn.Params {
			if p.Name == "" || p.Type == "" {
				continue
			}
			base := resolver.StripTypeDecorations(p.Type)
			if clsNode, targetPkg, ok := resolveJavaClassNode(rc, bindings, pkgScope, base, localClassByName); ok {
				tm.Set(fn.QName, p.Name, resolver.TypeInfo{
					Name:       clsNode.Name,
					QName:      clsNode.QName,
					Package:    targetPkg,
					Origin:     resolver.OriginParameter,
					Confidence: 0.96,
					Line:       fn.StartLine,
				})
			}
		}
	}

	for _, lv := range file.LocalVars {
		if lv.ExplicitType != "" {
			if prev, ok := tm.Lookup(lv.ScopeQName, lv.Name); ok {
				base := resolver.StripTypeDecorations(prev.Name)
				if clsNode, targetPkg, okCls := resolveJavaClassNode(rc, bindings, pkgScope, base, localClassByName); okCls {
					prev.Name = clsNode.Name
					prev.QName = clsNode.QName
					prev.Package = targetPkg
					tm.Set(lv.ScopeQName, lv.Name, prev)
				}
			}
			continue
		}
		if lv.InitCallee == "" {
			continue
		}
		var typeNode graph.Node
		var targetPkg string
		var found bool

		if lv.IsConstructor {
			clsExpr := lv.InitCallee
			if lv.InitReceiver != "" && lv.InitReceiver != "this" {
				clsExpr = lv.InitReceiver + "." + lv.InitCallee
			}
			typeNode, targetPkg, found = resolveJavaClassNode(rc, bindings, pkgScope, clsExpr, localClassByName)
		} else if lv.InitReceiver != "" {
			if ownerCls, ownerPkg, okCls := resolveJavaClassNode(rc, bindings, pkgScope, lv.InitReceiver, localClassByName); okCls {
				if meths := findJavaMethodsInHierarchy(rc, ownerPkg, graphNodeToParsedClass(ownerCls), lv.InitCallee, bindings, localClassByName); len(meths) > 0 {
					if ret := pickJavaFactoryReturnType(meths); ret != "" {
						typeNode, targetPkg, found = resolveJavaClassNode(rc, bindings, ownerPkg, ret, localClassByName)
					}
				}
			} else if recvType, okRecv := LookupScopedReceiverType(rc, lv.ScopeQName, "", lv.InitReceiver, tm.ToFlatMap(), nil); okRecv {
				// Chained call on inferred/factory receiver: e.g. `Client.builder().build()` or `factory.open().client()`.
				recvTypeName := resolver.StripTypeDecorations(recvType.Name)
				recvPkg := pkgOrDefault(recvType.Package, pkgScope)
				if ownerCls, ownerPkg, okOwner := resolveJavaClassNode(rc, bindings, recvPkg, recvTypeName, localClassByName); okOwner {
					if meths := findJavaMethodsInHierarchy(rc, ownerPkg, graphNodeToParsedClass(ownerCls), lv.InitCallee, bindings, localClassByName); len(meths) > 0 {
						if ret := pickJavaFactoryReturnType(meths); ret != "" {
							typeNode, targetPkg, found = resolveJavaClassNode(rc, bindings, ownerPkg, ret, localClassByName)
						}
					}
				}
			}
		}

		if found && typeNode.Name != "" {
			origin := resolver.OriginReturnType
			conf := float32(0.90)
			if lv.IsConstructor {
				origin = resolver.OriginConstructor
				conf = 0.95
			}
			tm.Set(lv.ScopeQName, lv.Name, resolver.TypeInfo{
				Name:       typeNode.Name,
				QName:      typeNode.QName,
				Package:    targetPkg,
				Origin:     origin,
				Confidence: conf,
				Line:       lv.Line,
			})
		}
	}

	return tm.ToFlatMap(), nil
}

// ResolveCalls resolves Java constructors, static/package/wildcard imports,
// inheritance-aware this/super/instance methods, inner classes, and overloaded calls.
func (r *JavaResolver) ResolveCalls(
	ctx context.Context,
	file *parser.ParsedFile,
	rc *resolver.ResolutionContext,
	imports []resolver.ResolvedImport,
	types map[string]resolver.TypeInfo,
) ([]resolver.ResolvedCall, []resolver.UnresolvedCall, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if file == nil {
		return nil, nil, nil
	}
	if rc == nil {
		rc = &resolver.ResolutionContext{}
	}

	pkgScope := javaPackageScope(file)
	bindings := buildJavaBindings(file, imports, rc)

	localClassByName := make(map[string]parser.ParsedClass, len(file.Classes))
	localClassByQName := make(map[string]parser.ParsedClass, len(file.Classes))
	for _, cls := range file.Classes {
		localClassByName[cls.Name] = cls
		localClassByQName[cls.QName] = cls
	}

	var resolved []resolver.ResolvedCall
	var unresolved []resolver.UnresolvedCall

	for _, call := range file.Calls {
		callerUID := ResolveCallerUIDForFile(rc, file, call.CallerQName, call.Line)
		res, unres := r.resolveOneCall(file, rc, callerUID, call, pkgScope, bindings, localClassByName, localClassByQName, types)
		if res != nil {
			resolved = append(resolved, *res)
		}
		if unres != nil {
			unresolved = append(unresolved, *unres)
		}
	}

	return resolved, unresolved, nil
}

func (r *JavaResolver) resolveOneCall(
	file *parser.ParsedFile,
	rc *resolver.ResolutionContext,
	callerUID graph.UID,
	call parser.ParsedCall,
	pkgScope string,
	bindings *javaBindings,
	localClassByName map[string]parser.ParsedClass,
	localClassByQName map[string]parser.ParsedClass,
	types map[string]resolver.TypeInfo,
) (*resolver.ResolvedCall, *resolver.UnresolvedCall) {
	enclosingClass := FindEnclosingClass(call.CallerQName, localClassByQName, rc)

	// 1. Constructor instantiation: `new Foo(...)`, `new Outer.Inner(...)`, or `Foo::new`.
	if call.IsConstructor {
		clsExpr := call.CalleeName
		if call.CalleeName == "new" && call.ReceiverName != "" {
			clsExpr = call.ReceiverName
		} else if call.ReceiverName != "" {
			clsExpr = call.ReceiverName + "." + call.CalleeName
		}

		if clsNode, targetPkg, ok := resolveJavaClassNode(rc, bindings, pkgScope, clsExpr, localClassByName); ok {
			tier := resolver.TierQualifiedImport
			if targetPkg == pkgScope {
				if _, isLocal := localClassByName[clsNode.Name]; isLocal {
					tier = resolver.TierLocal
				} else {
					tier = resolver.TierSamePackage
				}
			}
			simpleName := clsNode.Name
			ctors := filterJavaOverloads(findJavaDirectMethods(rc, clsNode.QName, simpleName), call.ArgumentCount)
			switch len(ctors) {
			case 0:
				return makeJavaResolvedCall(callerUID, call, clsNode, tier, "class constructor target: "+clsNode.QName, targetPkg), nil
			case 1:
				return makeJavaResolvedCall(callerUID, call, ctors[0], tier, "constructor: "+ctors[0].QName, targetPkg), nil
			default:
				return nil, makeJavaAmbiguousCall(callerUID, file.Path, call, ctors, clsNode.Name, targetPkg, "ambiguous overloaded Java constructor")
			}
		}

		if isJavaStdType(clsExpr) || isExternalJavaImport(bindings, clsExpr) {
			return nil, &resolver.UnresolvedCall{
				CallerUID:   callerUID,
				CallerQName: call.CallerQName,
				File:        file.Path,
				Line:        call.Line,
				Column:      uint16(call.Col),
				CalleeExpr:  "new " + clsExpr,
				LookupKey:   resolver.BuildLookupKey("", "java.lang", clsExpr),
				Status:      graph.CallStatusExternal,
				Tier:        resolver.TierUnresolved,
				Reason:      "external Java constructor: " + clsExpr,
			}
		}
	}

	// 2. Direct / unqualified call: `helper()` or statically imported `nextId()`.
	if !call.IsMethodCall || call.ReceiverName == "" {
		// Enclosing class + hierarchy (including outer enclosing classes for inner classes).
		for _, owner := range collectJavaEnclosingHierarchy(enclosingClass, localClassByQName) {
			cands := filterJavaOverloads(findJavaMethodsInHierarchy(rc, pkgScope, owner, call.CalleeName, bindings, localClassByName), call.ArgumentCount)
			if len(cands) == 1 {
				return makeJavaResolvedCall(callerUID, call, cands[0], resolver.TierSelf, "class method: "+cands[0].QName, pkgScope), nil
			}
			if len(cands) > 1 {
				return nil, makeJavaAmbiguousCall(callerUID, file.Path, call, cands, call.CalleeName, pkgScope, "multiple overloaded Java methods match arity")
			}
		}

		// Static imports (`import static pkg.Class.meth;` or `import static pkg.Class.*;`).
		if cands, extOnly, hitPkg := resolveJavaStaticImportCall(rc, bindings, call.CalleeName, call.ArgumentCount); len(cands) > 0 {
			if len(cands) == 1 {
				return makeJavaResolvedCall(callerUID, call, cands[0], resolver.TierQualifiedImport, "static import: "+cands[0].QName, hitPkg), nil
			}
			return nil, makeJavaAmbiguousCall(callerUID, file.Path, call, cands, call.CalleeName, hitPkg, "multiple overloaded static import methods")
		} else if extOnly {
			return nil, &resolver.UnresolvedCall{
				CallerUID:   callerUID,
				CallerQName: call.CallerQName,
				File:        file.Path,
				Line:        call.Line,
				Column:      uint16(call.Col),
				CalleeExpr:  call.CalleeName,
				LookupKey:   resolver.BuildLookupKey("", hitPkg, call.CalleeName),
				Status:      graph.CallStatusExternal,
				Tier:        resolver.TierUnresolved,
				Reason:      "external Java static import",
			}
		}

		return nil, &resolver.UnresolvedCall{
			CallerUID:   callerUID,
			CallerQName: call.CallerQName,
			File:        file.Path,
			Line:        call.Line,
			Column:      uint16(call.Col),
			CalleeExpr:  call.CalleeName,
			LookupKey:   resolver.BuildLookupKey("", pkgScope, call.CalleeName),
			Status:      graph.CallStatusUnresolved,
			Tier:        resolver.TierUnresolved,
			Reason:      "unresolved Java unqualified call",
		}
	}

	recv := strings.TrimSpace(call.ReceiverName)

	// 3. Qualified / method call.
	// Tier 1: `this.method()` or `super.method()`.
	if enclosingClass.Name != "" && (recv == "this" || recv == "super") {
		var rawCands []graph.Node
		if recv == "super" {
			rawCands = findJavaSuperMethods(rc, pkgScope, enclosingClass, call.CalleeName, bindings, localClassByName)
		} else {
			rawCands = findJavaMethodsInHierarchy(rc, pkgScope, enclosingClass, call.CalleeName, bindings, localClassByName)
		}
		cands := filterJavaOverloads(rawCands, call.ArgumentCount)
		if len(cands) == 1 {
			return makeJavaResolvedCall(callerUID, call, cands[0], resolver.TierSelf, recv+"."+call.CalleeName, pkgScope), nil
		}
		if len(cands) > 1 {
			return nil, makeJavaAmbiguousCall(callerUID, file.Path, call, cands, recv+"."+call.CalleeName, pkgScope, "ambiguous overloaded Java "+recv+" method")
		}
	}

	// Tier 3 / Tier 6: Static or inner-class call on class name (`Ids.nextId()`, `Order.Builder.create()`).
	if clsNode, targetPkg, ok := resolveJavaClassNode(rc, bindings, pkgScope, recv, localClassByName); ok {
		tier := resolver.TierQualifiedImport
		if targetPkg == pkgScope {
			if _, isLocal := localClassByName[clsNode.Name]; isLocal {
				tier = resolver.TierLocal
			} else {
				tier = resolver.TierSamePackage
			}
		}
		cands := filterJavaOverloads(findJavaMethodsInHierarchy(rc, targetPkg, graphNodeToParsedClass(clsNode), call.CalleeName, bindings, localClassByName), call.ArgumentCount)
		if len(cands) == 1 {
			return makeJavaResolvedCall(callerUID, call, cands[0], tier, "static/class call: "+cands[0].QName, targetPkg), nil
		}
		if len(cands) > 1 {
			return nil, makeJavaAmbiguousCall(callerUID, file.Path, call, cands, recv+"."+call.CalleeName, targetPkg, "ambiguous overloaded static Java method")
		}
	}

	// Tier 4: Instance method call via inferred receiver type (`repo.load()`, `this.repo.save()`).
	if recvType, ok := LookupScopedReceiverType(rc, call.CallerQName, enclosingClass.QName, recv, types, localClassByQName); ok {
		typeName := resolver.StripTypeDecorations(recvType.Name)
		targetPkg := recvType.Package
		if targetPkg == "" && recvType.QName != "" {
			targetPkg = PackageScopePrefix(recvType.QName)
		}

		var clsNode graph.Node
		var resolvedPkg string
		var okCls bool
		if recvType.QName != "" && rc.View != nil {
			for _, n := range rc.View.GetByQName(recvType.QName) {
				if n.Kind == graph.KindClass || n.Kind == graph.KindInterface {
					clsNode = n
					resolvedPkg = pkgOrDefault(PackageScopePrefix(n.QName), pkgScope)
					okCls = true
					break
				}
			}
		}
		if !okCls {
			clsNode, resolvedPkg, okCls = resolveJavaClassNode(rc, bindings, pkgOrDefault(targetPkg, pkgScope), typeName, localClassByName)
		}
		if okCls {
			cands := filterJavaOverloads(findJavaMethodsInHierarchy(rc, resolvedPkg, graphNodeToParsedClass(clsNode), call.CalleeName, bindings, localClassByName), call.ArgumentCount)
			if len(cands) == 1 {
				return makeJavaResolvedCall(callerUID, call, cands[0], resolver.TierReceiverType, "receiver type inferred: "+clsNode.Name+"."+call.CalleeName, resolvedPkg), nil
			}
			if len(cands) > 1 {
				return nil, makeJavaAmbiguousCall(callerUID, file.Path, call, cands, recv+"."+call.CalleeName, resolvedPkg, "ambiguous overloaded instance Java method")
			}
		}

		if isJavaStdType(typeName) || isExternalJavaImport(bindings, typeName) {
			return nil, &resolver.UnresolvedCall{
				CallerUID:   callerUID,
				CallerQName: call.CallerQName,
				File:        file.Path,
				Line:        call.Line,
				Column:      uint16(call.Col),
				CalleeExpr:  recv + "." + call.CalleeName,
				LookupKey:   resolver.BuildLookupKey(typeName, targetPkg, call.CalleeName),
				Status:      graph.CallStatusExternal,
				Tier:        resolver.TierUnresolved,
				Reason:      "external Java stdlib/library method on " + typeName,
			}
		}
	}

	if isJavaStdReceiver(recv, bindings) {
		return nil, &resolver.UnresolvedCall{
			CallerUID:   callerUID,
			CallerQName: call.CallerQName,
			File:        file.Path,
			Line:        call.Line,
			Column:      uint16(call.Col),
			CalleeExpr:  recv + "." + call.CalleeName,
			LookupKey:   resolver.BuildLookupKey(recv, "java.lang", call.CalleeName),
			Status:      graph.CallStatusExternal,
			Tier:        resolver.TierUnresolved,
			Reason:      "external Java stdlib/imported call: " + recv + "." + call.CalleeName,
		}
	}

	return nil, &resolver.UnresolvedCall{
		CallerUID:   callerUID,
		CallerQName: call.CallerQName,
		File:        file.Path,
		Line:        call.Line,
		Column:      uint16(call.Col),
		CalleeExpr:  recv + "." + call.CalleeName,
		LookupKey:   resolver.BuildLookupKey(recv, pkgScope, call.CalleeName),
		Status:      graph.CallStatusUnresolved,
		Tier:        resolver.TierUnresolved,
		Reason:      "unresolved Java selector call",
	}
}

type javaClassImport struct {
	Package    string
	ClassQName string
	ClassName  string
	IsExternal bool
	RawPath    string
}

type javaBindings struct {
	Classes        map[string]javaClassImport // simple/alias -> import
	WildcardPkgs   []string
	StaticMethods  map[string][]javaClassImport // methodName -> target class
	StaticWildcard []javaClassImport
	ExtSymbols     map[string]string // simple name -> external path
}

func buildJavaBindings(file *parser.ParsedFile, resolved []resolver.ResolvedImport, rc *resolver.ResolutionContext) *javaBindings {
	b := &javaBindings{
		Classes:       make(map[string]javaClassImport),
		StaticMethods: make(map[string][]javaClassImport),
		ExtSymbols:    make(map[string]string),
	}
	for i, imp := range file.Imports {
		var targetPkg string
		var isExt bool
		if i < len(resolved) {
			targetPkg = resolved[i].TargetPackage
			isExt = resolved[i].IsExternal
		} else {
			isExt = !hasJavaPackage(rc, imp.Path)
		}

		if imp.IsWildcard {
			if isExt {
				continue
			}
			if _, clsQName, _, ok := resolveJavaStaticOrInnerTarget(rc, imp.Path); ok && clsQName != "" {
				b.StaticWildcard = append(b.StaticWildcard, javaClassImport{
					Package:    PackageScopePrefix(clsQName),
					ClassQName: clsQName,
					RawPath:    imp.Path,
				})
			} else {
				b.WildcardPkgs = append(b.WildcardPkgs, pkgOrDefault(targetPkg, imp.Path))
			}
			continue
		}

		for _, sym := range imp.Names {
			if sym == "" {
				continue
			}
			if isExt {
				b.ExtSymbols[sym] = imp.Path
				continue
			}
			if _, clsQName, _, ok := resolveJavaStaticOrInnerTarget(rc, imp.Path); ok && clsQName != "" {
				// Static method import (`import static pkg.Cls.meth;`) OR inner class (`import pkg.Outer.Inner;`).
				if rc != nil && rc.View != nil && len(rc.View.GetByQName(clsQName+"::"+sym)) > 0 {
					for _, n := range rc.View.GetByQName(clsQName + "::" + sym) {
						if n.Kind == graph.KindClass || n.Kind == graph.KindInterface {
							b.Classes[sym] = javaClassImport{
								Package:    PackageScopePrefix(clsQName),
								ClassQName: clsQName + "::" + sym,
								ClassName:  sym,
								RawPath:    imp.Path + "." + sym,
							}
						} else {
							b.StaticMethods[sym] = append(b.StaticMethods[sym], javaClassImport{
								Package:    PackageScopePrefix(clsQName),
								ClassQName: clsQName,
								RawPath:    imp.Path + "." + sym,
							})
						}
					}
					continue
				}
			}
			pkg := pkgOrDefault(targetPkg, imp.Path)
			b.Classes[sym] = javaClassImport{
				Package:    pkg,
				ClassQName: pkg + "::" + sym,
				ClassName:  sym,
				RawPath:    imp.Path + "." + sym,
			}
		}
	}
	return b
}

func resolveJavaClassNode(
	rc *resolver.ResolutionContext,
	b *javaBindings,
	pkgScope, classExpr string,
	localClassByName map[string]parser.ParsedClass,
) (graph.Node, string, bool) {
	clean := resolver.StripTypeDecorations(strings.TrimSpace(classExpr))
	if clean == "" {
		return graph.Node{}, "", false
	}

	// Dotted inner/qualified expression (`Order.Builder` or `com.example.orders.Order`).
	if strings.Contains(clean, ".") {
		// 1. Check `Outer.Inner` inside current package or imported classes.
		parts := strings.Split(clean, ".")
		if outerNode, outerPkg, ok := resolveJavaClassNode(rc, b, pkgScope, parts[0], localClassByName); ok {
			nestedQName := outerNode.QName + "::" + strings.Join(parts[1:], "::")
			if rc != nil && rc.View != nil {
				for _, n := range rc.View.GetByQName(nestedQName) {
					if n.Kind == graph.KindClass || n.Kind == graph.KindInterface {
						return n, outerPkg, true
					}
				}
			}
		}
		// 2. Full package-qualified `com.example.orders.Order`.
		if idx := strings.LastIndexByte(clean, '.'); idx > 0 && rc != nil && rc.View != nil {
			qPkg := clean[:idx]
			qName := clean[idx+1:]
			for _, n := range rc.View.GetByQName(qPkg + "::" + qName) {
				if n.Kind == graph.KindClass || n.Kind == graph.KindInterface {
					return n, qPkg, true
				}
			}
		}
		return graph.Node{}, "", false
	}

	// 1. Same-file class / inner class.
	if localClassByName != nil {
		if cls, ok := localClassByName[clean]; ok {
			if rc != nil && rc.View != nil {
				for _, n := range rc.View.GetByQName(cls.QName) {
					if n.Kind == graph.KindClass || n.Kind == graph.KindInterface {
						return n, pkgOrDefault(PackageScopePrefix(n.QName), pkgScope), true
					}
				}
			}
			return graph.Node{
				UID:   graph.GenerateNodeUID(cls.EffectiveKind(), cls.QName, 0),
				Kind:  cls.EffectiveKind(),
				Name:  cls.Name,
				QName: cls.QName,
				Bases: cls.Bases,
			}, pkgOrDefault(PackageScopePrefix(cls.QName), pkgScope), true
		}
	}

	// 2. Explicit class imports.
	if b != nil {
		if imp, ok := b.Classes[clean]; ok && !imp.IsExternal && rc != nil && rc.View != nil {
			for _, n := range rc.View.GetByQName(imp.ClassQName) {
				if n.Kind == graph.KindClass || n.Kind == graph.KindInterface {
					return n, imp.Package, true
				}
			}
		}
	}

	// 3. Same-package class across sibling files (`com.example.orders::Helper`).
	if rc != nil && rc.View != nil && pkgScope != "" {
		for _, n := range rc.View.GetByQName(pkgScope + "::" + clean) {
			if n.Kind == graph.KindClass || n.Kind == graph.KindInterface {
				return n, pkgScope, true
			}
		}
	}

	// 4. Wildcard package imports (`import com.example.store.*;`).
	if b != nil && rc != nil && rc.View != nil {
		for _, wpkg := range b.WildcardPkgs {
			for _, n := range rc.View.GetByQName(wpkg + "::" + clean) {
				if n.Kind == graph.KindClass || n.Kind == graph.KindInterface {
					return n, wpkg, true
				}
			}
		}
	}

	return graph.Node{}, "", false
}

func resolveJavaStaticImportCall(rc *resolver.ResolutionContext, b *javaBindings, methName string, argCount uint16) (cands []graph.Node, extOnly bool, hitPkg string) {
	if b == nil {
		return nil, false, ""
	}
	if _, isExt := b.ExtSymbols[methName]; isExt {
		return nil, true, b.ExtSymbols[methName]
	}
	for _, sc := range b.StaticMethods[methName] {
		cands = append(cands, findJavaDirectMethods(rc, sc.ClassQName, methName)...)
		if hitPkg == "" {
			hitPkg = sc.Package
		}
	}
	if len(cands) == 0 {
		for _, sw := range b.StaticWildcard {
			cands = append(cands, findJavaDirectMethods(rc, sw.ClassQName, methName)...)
			if hitPkg == "" {
				hitPkg = sw.Package
			}
		}
	}
	return filterJavaOverloads(cands, argCount), false, hitPkg
}

func findJavaSuperMethods(
	rc *resolver.ResolutionContext,
	pkgScope string,
	cls parser.ParsedClass,
	methodName string,
	bindings *javaBindings,
	localClassByName map[string]parser.ParsedClass,
) []graph.Node {
	for _, base := range cls.Bases {
		if baseNode, basePkg, ok := resolveJavaClassNode(rc, bindings, pkgScope, base, localClassByName); ok {
			if found := findJavaMethodsInHierarchy(rc, basePkg, graphNodeToParsedClass(baseNode), methodName, bindings, localClassByName); len(found) > 0 {
				return found
			}
		}
	}
	return nil
}

func findJavaMethodsInHierarchy(
	rc *resolver.ResolutionContext,
	pkgScope string,
	startCls parser.ParsedClass,
	methodName string,
	bindings *javaBindings,
	localClassByName map[string]parser.ParsedClass,
) []graph.Node {
	if rc == nil || rc.View == nil || startCls.Name == "" || methodName == "" {
		return nil
	}
	type item struct {
		cls parser.ParsedClass
		pkg string
	}
	visited := make(map[string]struct{})
	queue := []item{{cls: startCls, pkg: pkgOrDefault(PackageScopePrefix(startCls.QName), pkgScope)}}

	var results []graph.Node
	seenUID := make(map[graph.UID]struct{})

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		if cur.cls.QName == "" {
			continue
		}
		if _, seen := visited[cur.cls.QName]; seen {
			continue
		}
		visited[cur.cls.QName] = struct{}{}

		for _, n := range findJavaDirectMethods(rc, cur.cls.QName, methodName) {
			if _, dup := seenUID[n.UID]; !dup {
				seenUID[n.UID] = struct{}{}
				results = append(results, n)
			}
		}
		if len(results) > 0 {
			return results
		}

		// Collect superclass Bases and interface Implements for traversal.
		parents := append([]string(nil), cur.cls.Bases...)
		parents = append(parents, cur.cls.Implements...)
		if len(parents) == 0 {
			for _, n := range rc.View.GetByQName(cur.cls.QName) {
				parents = append(parents, n.Bases...)
				if pf := rc.LookupFile(n.File); pf != nil {
					for _, pc := range pf.Classes {
						if pc.QName == n.QName {
							parents = append(parents, pc.Implements...)
						}
					}
				}
			}
		}
		for _, pName := range parents {
			if pNode, pPkg, ok := resolveJavaClassNode(rc, bindings, cur.pkg, pName, localClassByName); ok {
				queue = append(queue, item{cls: graphNodeToParsedClass(pNode), pkg: pPkg})
			}
		}
	}

	return results
}

func findJavaDirectMethods(rc *resolver.ResolutionContext, clsQName, methodName string) []graph.Node {
	if rc == nil || rc.View == nil || clsQName == "" || methodName == "" {
		return nil
	}
	var out []graph.Node
	for _, n := range rc.View.GetByQName(clsQName + "::" + methodName) {
		if n.Kind == graph.KindFunction {
			out = append(out, n)
		}
	}
	return out
}

// filterJavaOverloads narrows candidate overloaded methods by argument count when
// exact-arity matches exist, preserving all matches when multiple overloads share arity.
func filterJavaOverloads(cands []graph.Node, argCount uint16) []graph.Node {
	if len(cands) <= 1 {
		return cands
	}
	var arityMatched []graph.Node
	for _, c := range cands {
		if uint16(len(c.Params)) == argCount {
			arityMatched = append(arityMatched, c)
		}
	}
	if len(arityMatched) > 0 {
		return arityMatched
	}
	return cands
}

func collectJavaEnclosingHierarchy(inner parser.ParsedClass, localClassByQName map[string]parser.ParsedClass) []parser.ParsedClass {
	if inner.QName == "" {
		return nil
	}
	out := []parser.ParsedClass{inner}
	cur := inner.QName
	for {
		idx := strings.LastIndex(cur, "::")
		if idx <= 0 {
			break
		}
		parentQName := cur[:idx]
		if parent, ok := localClassByQName[parentQName]; ok {
			out = append(out, parent)
		}
		cur = parentQName
	}
	return out
}

func resolveJavaStaticOrInnerTarget(rc *resolver.ResolutionContext, dotPath string) (pkg, clsQName, targetFile string, ok bool) {
	if rc == nil || rc.View == nil || dotPath == "" {
		return "", "", "", false
	}
	parts := strings.Split(dotPath, ".")
	for split := len(parts) - 1; split >= 1; split-- {
		candPkg := strings.Join(parts[:split], ".")
		rest := strings.Join(parts[split:], "::")
		candQName := candPkg + "::" + rest
		for _, n := range rc.View.GetByQName(candQName) {
			if n.Kind == graph.KindClass || n.Kind == graph.KindInterface {
				return candPkg, candQName, n.File, true
			}
		}
	}
	return "", "", "", false
}

func hasJavaPackage(rc *resolver.ResolutionContext, pkg string) bool {
	return len(javaPackageFiles(rc, pkg)) > 0
}

func findJavaTypeFile(rc *resolver.ResolutionContext, pkg, simpleName string) (string, bool) {
	if rc == nil || rc.View == nil || pkg == "" || simpleName == "" {
		return "", false
	}
	for _, n := range rc.View.GetByQName(pkg + "::" + simpleName) {
		if n.File != "" {
			return n.File, true
		}
	}
	return "", false
}

func pickJavaFactoryReturnType(meths []graph.Node) string {
	var agreed string
	for _, m := range meths {
		ret := resolver.StripTypeDecorations(m.ReturnType)
		if ret == "" || ret == "void" {
			continue
		}
		if agreed == "" {
			agreed = ret
		} else if agreed != ret {
			return "" // Conflicting non-void overload return types
		}
	}
	return agreed
}

func javaPackageFiles(rc *resolver.ResolutionContext, pkg string) []string {
	if rc == nil || pkg == "" {
		return nil
	}
	if rc.NameIndex != nil {
		if files := rc.NameIndex.FilesForPackage(pkg); len(files) > 0 {
			return files
		}
	}
	if len(rc.Files) > 0 {
		var out []string
		for _, pf := range rc.Files {
			if pf != nil && pf.Package == pkg {
				out = append(out, pf.Path)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

func javaPackageScope(file *parser.ParsedFile) string {
	if file == nil {
		return ""
	}
	if file.Package != "" {
		return file.Package
	}
	return ""
}

func graphNodeToParsedClass(n graph.Node) parser.ParsedClass {
	return parser.ParsedClass{
		Name:  n.Name,
		QName: n.QName,
		Kind:  n.Kind,
		Bases: n.Bases,
	}
}

func makeJavaResolvedCall(callerUID graph.UID, call parser.ParsedCall, target graph.Node, tier graph.ResolutionTier, methodDesc, pkgScope string) *resolver.ResolvedCall {
	return &resolver.ResolvedCall{
		CallerUID:   callerUID,
		CallerQName: call.CallerQName,
		TargetUID:   target.UID,
		TargetQName: target.QName,
		Tier:        tier,
		EdgeKind:    graph.EdgeCalls,
		Line:        call.Line,
		Column:      uint16(call.Col),
		Method:      methodDesc,
		LookupKey:   resolver.BuildLookupKey(call.ReceiverName, pkgScope, call.CalleeName),
	}
}

func makeJavaAmbiguousCall(callerUID graph.UID, filePath string, call parser.ParsedCall, cands []graph.Node, expr, pkgScope, reason string) *resolver.UnresolvedCall {
	uids := make([]graph.UID, 0, len(cands))
	for _, c := range cands {
		if !c.UID.IsZero() {
			uids = append(uids, c.UID)
		}
	}
	return &resolver.UnresolvedCall{
		CallerUID:   callerUID,
		CallerQName: call.CallerQName,
		File:        filePath,
		Line:        call.Line,
		Column:      uint16(call.Col),
		CalleeExpr:  expr,
		LookupKey:   resolver.BuildLookupKey(call.ReceiverName, pkgScope, call.CalleeName),
		Candidates:  uids,
		Status:      graph.CallStatusAmbiguous,
		Tier:        resolver.TierTypeNarrowed,
		Reason:      reason,
	}
}

func isJavaStdType(name string) bool {
	switch name {
	case "String", "StringBuilder", "StringBuffer", "Object", "System", "Math",
		"Integer", "Long", "Double", "Boolean", "List", "ArrayList", "Map", "HashMap",
		"Set", "HashSet", "Optional", "Objects", "Collections", "Arrays", "RuntimeException":
		return true
	default:
		return strings.HasPrefix(name, "java.") || strings.HasPrefix(name, "javax.")
	}
}

func isExternalJavaImport(b *javaBindings, name string) bool {
	if b == nil {
		return false
	}
	_, ok := b.ExtSymbols[name]
	return ok
}

func isJavaStdReceiver(recv string, b *javaBindings) bool {
	top := topSegment(recv)
	if isJavaStdType(top) || isExternalJavaImport(b, top) {
		return true
	}
	return strings.HasPrefix(recv, "System.")
}

// ResolveLinks resolves Java class/interface inheritance (extends -> INHERITS),
// interface implementations (implements -> IMPLEMENTS), and annotations (DECORATED_BY).
func (r *JavaResolver) ResolveLinks(ctx context.Context, file *parser.ParsedFile, rc *resolver.ResolutionContext, imports []resolver.ResolvedImport) ([]resolver.ResolvedLink, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if file == nil {
		return nil, nil
	}
	if rc == nil {
		rc = &resolver.ResolutionContext{}
	}

	pkgScope := javaPackageScope(file)
	bindings := buildJavaBindings(file, imports, rc)
	localClassByName := make(map[string]parser.ParsedClass, len(file.Classes))
	for _, cls := range file.Classes {
		localClassByName[cls.Name] = cls
	}

	resolveJavaTier := func(targetPkg string, targetName string) graph.ResolutionTier {
		if targetPkg == pkgScope {
			if _, isLocal := localClassByName[targetName]; isLocal {
				return resolver.TierLocal
			}
			return resolver.TierSamePackage
		}
		return resolver.TierQualifiedImport
	}

	var links []resolver.ResolvedLink
	for _, cls := range file.Classes {
		srcUID := ResolveClassOrSymbolUID(rc, cls.EffectiveKind(), cls.QName)
		for _, base := range cls.Bases {
			if targetNode, targetPkg, ok := resolveJavaClassNode(rc, bindings, pkgScope, base, localClassByName); ok {
				links = append(links, MakeResolvedLink(srcUID, cls.QName, targetNode, graph.EdgeInherits, resolveJavaTier(targetPkg, targetNode.Name), cls.StartLine, "extends: "+base))
			}
		}
		for _, iface := range cls.Implements {
			if targetNode, targetPkg, ok := resolveJavaClassNode(rc, bindings, pkgScope, iface, localClassByName); ok {
				links = append(links, MakeResolvedLink(srcUID, cls.QName, targetNode, graph.EdgeImplements, resolveJavaTier(targetPkg, targetNode.Name), cls.StartLine, "implements: "+iface))
			}
		}
		for _, dec := range cls.Decorators {
			if annName := NormalizeDecoratorTarget(dec); annName != "" {
				if targetNode, targetPkg, ok := resolveJavaClassNode(rc, bindings, pkgScope, annName, localClassByName); ok {
					links = append(links, MakeResolvedLink(srcUID, cls.QName, targetNode, graph.EdgeDecoratedBy, resolveJavaTier(targetPkg, targetNode.Name), cls.StartLine, "annotation: "+dec))
				}
			}
		}
	}

	for _, fn := range file.Functions {
		if len(fn.Decorators) == 0 {
			continue
		}
		srcUID := ResolveCallerUID(rc, fn.QName, fn.StartLine)
		for _, dec := range fn.Decorators {
			if annName := NormalizeDecoratorTarget(dec); annName != "" {
				if targetNode, targetPkg, ok := resolveJavaClassNode(rc, bindings, pkgScope, annName, localClassByName); ok {
					links = append(links, MakeResolvedLink(srcUID, fn.QName, targetNode, graph.EdgeDecoratedBy, resolveJavaTier(targetPkg, targetNode.Name), fn.StartLine, "annotation: "+dec))
				}
			}
		}
	}

	return DedupeResolvedLinks(links), nil
}
