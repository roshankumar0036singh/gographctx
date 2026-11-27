package lang

import (
	"context"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/roshankumar0036singh/gographctx/internal/graph"
	"github.com/roshankumar0036singh/gographctx/internal/parser"
	"github.com/roshankumar0036singh/gographctx/internal/resolver"
)

// PythonResolver implements LanguageResolver for Python modules and packages,
// handling relative imports, __init__.py packages, re-exports, wildcard imports,
// conditional imports, self/cls/super method calls, and class MRO inheritance.
type PythonResolver struct{}

// NewPythonResolver returns a Python LanguageResolver.
func NewPythonResolver() *PythonResolver {
	return &PythonResolver{}
}

// Language returns "python".
func (r *PythonResolver) Language() string {
	return "python"
}

// ResolveImports resolves Python absolute and relative imports against indexed
// project modules and __init__.py packages.
func (r *PythonResolver) ResolveImports(ctx context.Context, file *parser.ParsedFile, rc *resolver.ResolutionContext) ([]resolver.ResolvedImport, error) {
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

	curMod, curPkg, _ := pythonModuleContext(file)
	_ = curMod

	out := make([]resolver.ResolvedImport, 0, len(file.Imports))
	for _, imp := range file.Imports {
		ri := resolver.ResolvedImport{
			SourceFile:    file.Path,
			ImportPath:    imp.Path,
			Alias:         imp.Alias,
			ImportedNames: append([]string(nil), imp.Names...),
			Line:          imp.Line,
			IsWildcard:    imp.IsWildcard,
			IsRelative:    imp.IsRelative || strings.HasPrefix(imp.Path, "."),
		}

		targetMod, validRel := resolvePythonImportModule(curPkg, imp.Path, ri.IsRelative)
		if !validRel {
			ri.IsExternal = true
			out = append(out, ri)
			continue
		}

		if len(imp.Names) == 0 {
			// `import a.b` or `import a.b as alias`: bind top-level or alias name.
			if ri.Alias == "" {
				ri.Alias = topSegment(imp.Path)
			}
			if targetFile, found := findPythonModuleFile(rc, targetMod); found {
				ri.TargetPackage = targetMod
				ri.TargetFile = targetFile
				ri.IsExternal = false
			} else {
				ri.IsExternal = true
			}
			out = append(out, ri)
			continue
		}

		// `from <mod> import ...`
		if ri.Alias == "" && len(imp.Names) == 1 && imp.Names[0] != "*" {
			ri.Alias = imp.Names[0]
		}
		if targetFile, found := findPythonModuleFile(rc, targetMod); found {
			ri.TargetPackage = targetMod
			ri.TargetFile = targetFile
			ri.IsExternal = false
			out = append(out, ri)
			continue
		}

		// When `from . import submodule` names a child submodule without __init__.py
		// existing at the anchor, check "<targetMod>.<name>".
		if len(imp.Names) == 1 && imp.Names[0] != "*" && targetMod != "" {
			subMod := targetMod + "." + imp.Names[0]
			if targetFile, found := findPythonModuleFile(rc, subMod); found {
				ri.TargetPackage = subMod
				ri.TargetFile = targetFile
				ri.IsExternal = false
				out = append(out, ri)
				continue
			}
		}

		ri.IsExternal = true
		out = append(out, ri)
	}

	return out, nil
}

// InferTypes builds scoped Python variable, parameter, self/cls attribute, and
// cross-module factory/constructor types.
func (r *PythonResolver) InferTypes(ctx context.Context, file *parser.ParsedFile, rc *resolver.ResolutionContext) (map[string]resolver.TypeInfo, error) {
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
	bindings := buildPythonBindings(file, imports, rc)
	modScope, _, _ := pythonModuleContext(file)

	// Enrich parameter types from imported symbols (e.g. `cache: Cache` where `Cache`
	// is imported as an alias for `MemoryCache`).
	for _, fn := range file.Functions {
		for _, p := range fn.Params {
			if p.Name == "" || p.Type == "" {
				continue
			}
			base := resolver.StripTypeDecorations(p.Type)
			if clsNode, pkg, found := resolvePythonSymbolType(rc, bindings, modScope, base); found {
				tm.Set(fn.QName, p.Name, resolver.TypeInfo{
					Name:       clsNode.Name,
					QName:      clsNode.QName,
					Package:    pkg,
					Origin:     resolver.OriginParameter,
					Confidence: 0.96,
					Line:       fn.StartLine,
				})
			}
		}
	}

	// Enrich LocalVars constructed from imported classes, aliases, or factory functions
	// (including self.x / cls.x attributes set inside methods).
	for pass := 0; pass < 2; pass++ {
		for _, lv := range file.LocalVars {
			if lv.ExplicitType != "" {
				// Enrich annotated imported types with their target module.
				if prev, ok := tm.Lookup(lv.ScopeQName, lv.Name); ok && (prev.Package == "" || prev.Package == file.Package) {
					base := resolver.StripTypeDecorations(prev.Name)
					if clsNode, pkg, found := resolvePythonSymbolType(rc, bindings, modScope, base); found {
						prev.Name = clsNode.Name
						prev.QName = clsNode.QName
						prev.Package = pkg
						tm.Set(lv.ScopeQName, lv.Name, prev)
						promotePythonClassAttr(tm, lv, prev)
					}
				}
				continue
			}

			var inferred resolver.TypeInfo
			var hasInferred bool

			switch {
			case lv.InitCallee != "":
				var clsNode graph.Node
				var pkg string
				var found bool
				if lv.InitReceiver == "" {
					if target, ok := resolvePythonBindingCall(rc, bindings, modScope, lv.InitCallee); ok {
						clsNode, pkg, found = inferTargetReturnOrClass(rc, target)
					}
				} else {
					if target, modPkg, isMod := resolvePythonModuleMember(rc, bindings, lv.InitReceiver, lv.InitCallee); isMod && !target.UID.IsZero() {
						clsNode, pkg, found = inferTargetReturnOrClass(rc, target)
						if pkg == "" {
							pkg = modPkg
						}
					} else if recvInfo, ok := tm.LookupReceiver(lv.ScopeQName, lv.InitReceiver); ok {
						recvPkg := pkgOrDefault(recvInfo.Package, modScope)
						if target, _, okMethod := resolvePythonMethodOnHierarchy(rc, recvPkg, resolver.StripTypeDecorations(recvInfo.Name), lv.InitCallee, nil, bindings, modScope); okMethod {
							clsNode, pkg, found = inferTargetReturnOrClass(rc, target)
						}
					}
				}
				if found && clsNode.Name != "" {
					origin := resolver.OriginReturnType
					conf := float32(0.86)
					if lv.IsConstructor {
						origin = resolver.OriginConstructor
						conf = 0.92
					}
					inferred = resolver.TypeInfo{
						Name:       clsNode.Name,
						QName:      clsNode.QName,
						Package:    pkg,
						Origin:     origin,
						Confidence: conf,
						Line:       lv.Line,
					}
					hasInferred = true
				}

			case lv.InitAlias != "":
				if rhsInfo, ok := tm.LookupReceiver(lv.ScopeQName, lv.InitAlias); ok && rhsInfo.Name != "" {
					rhsInfo.Line = lv.Line
					if clsNode, pkg, found := resolvePythonSymbolType(rc, bindings, modScope, resolver.StripTypeDecorations(rhsInfo.Name)); found {
						rhsInfo.Name = clsNode.Name
						rhsInfo.QName = clsNode.QName
						rhsInfo.Package = pkg
					}
					inferred = rhsInfo
					hasInferred = true
				}
			}

			if hasInferred {
				tm.Set(lv.ScopeQName, lv.Name, inferred)
				promotePythonClassAttr(tm, lv, inferred)
			}
		}
	}

	return tm.ToFlatMap(), nil
}

func promotePythonClassAttr(tm *resolver.TypeMap, lv parser.ParsedLocalVar, info resolver.TypeInfo) {
	if !strings.HasPrefix(lv.Name, "self.") && !strings.HasPrefix(lv.Name, "cls.") {
		return
	}
	clsScope := PackageScopePrefix(lv.ScopeQName)
	if lastSep := strings.LastIndex(lv.ScopeQName, "::"); lastSep > 0 {
		clsScope = lv.ScopeQName[:lastSep]
	}
	if clsScope == "" {
		return
	}
	attr := strings.TrimPrefix(strings.TrimPrefix(lv.Name, "self."), "cls.")
	tm.Set(clsScope, lv.Name, info)
	tm.Set(clsScope, attr, info)
	tm.SetField(clsScope, attr, info)
	tm.Set(clsScope, "self."+attr, info)
	tm.Set(clsScope, "cls."+attr, info)
}

// ResolveCalls resolves direct calls, imported calls, re-exports, wildcard imports,
// self/cls/super calls, inherited methods, and inferred attribute receiver calls.
func (r *PythonResolver) ResolveCalls(
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

	modScope, _, _ := pythonModuleContext(file)
	bindings := buildPythonBindings(file, imports, rc)

	localClassesByName := make(map[string]parser.ParsedClass, len(file.Classes))
	localClassesByQName := make(map[string]parser.ParsedClass, len(file.Classes))
	for _, cls := range file.Classes {
		localClassesByName[cls.Name] = cls
		localClassesByQName[cls.QName] = cls
	}

	var resolved []resolver.ResolvedCall
	var unresolved []resolver.UnresolvedCall

	for _, call := range file.Calls {
		callerUID := ResolveCallerUID(rc, call.CallerQName, call.Line)
		res, unres := r.resolveOneCall(file, rc, callerUID, call, modScope, bindings, localClassesByName, localClassesByQName, types)
		if res != nil {
			resolved = append(resolved, *res)
		}
		if unres != nil {
			unresolved = append(unresolved, *unres)
		}
	}

	return resolved, unresolved, nil
}

func (r *PythonResolver) resolveOneCall(
	file *parser.ParsedFile,
	rc *resolver.ResolutionContext,
	callerUID graph.UID,
	call parser.ParsedCall,
	modScope string,
	bindings *pyBindings,
	localClassesByName map[string]parser.ParsedClass,
	localClassesByQName map[string]parser.ParsedClass,
	types map[string]resolver.TypeInfo,
) (*resolver.ResolvedCall, *resolver.UnresolvedCall) {
	enclosingClass := findEnclosingPythonClass(call.CallerQName, localClassesByQName, rc)

	// 1. Direct / unqualified call: foo(...) or ClassName(...)
	if !call.IsMethodCall || call.ReceiverName == "" {
		// Nested function inside caller scope (`def outer(): def inner(): ... inner()`).
		if rc.View != nil && call.CallerQName != "" {
			nestedQName := call.CallerQName + "::" + call.CalleeName
			if target, ok, _ := FilterUniqueFunctionMatches(rc.View.GetByQName(nestedQName), false, nil); ok {
				return makePythonResolvedCall(callerUID, call, target, resolver.TierLocal, "nested local function: "+target.QName, modScope), nil
			}
		}

		// Same-file top-level function.
		if target, ok := FindSameFileFuncUID(rc, file, call.CalleeName, ""); ok {
			return makePythonResolvedCall(callerUID, call, target, resolver.TierLocal, "local function: "+target.QName, modScope), nil
		}

		// Same-file class instantiation: `MyClass(...)` or `cls(...)` inside classmethod.
		if enclosingClass.Name != "" && call.CalleeName == "cls" {
			clsPkg := pkgOrDefault(PackageScopePrefix(enclosingClass.QName), modScope)
			if target, okInit := resolvePythonConstructorTarget(rc, clsPkg, enclosingClass.Name, localClassesByName, bindings, modScope); okInit {
				return makePythonResolvedCall(callerUID, call, target, resolver.TierSelf, "cls() constructor: "+target.QName, clsPkg), nil
			}
		}
		if cls, ok := localClassesByName[call.CalleeName]; ok {
			if target, okInit := resolvePythonConstructorTarget(rc, modScope, cls.Name, localClassesByName, bindings, modScope); okInit {
				return makePythonResolvedCall(callerUID, call, target, resolver.TierLocal, "local class constructor: "+target.QName, modScope), nil
			}
		}

		// Bound symbol imports (`from pkg import foo`, conditional imports, re-exports, `*`).
		if targets, extOnly, modHitPkg := resolvePythonSymbolCallTargets(rc, bindings, call.CalleeName); len(targets) > 0 {
			if len(targets) == 1 {
				return makePythonResolvedCall(callerUID, call, targets[0], resolver.TierQualifiedImport, "imported symbol: "+targets[0].QName, modHitPkg), nil
			}
			cands := make([]graph.UID, 0, len(targets))
			for _, t := range targets {
				cands = append(cands, t.UID)
			}
			return nil, &resolver.UnresolvedCall{
				CallerUID:   callerUID,
				CallerQName: call.CallerQName,
				File:        file.Path,
				Line:        call.Line,
				Column:      uint16(call.Col),
				CalleeExpr:  call.CalleeName,
				LookupKey:   resolver.BuildLookupKey("", modScope, call.CalleeName),
				Candidates:  cands,
				Status:      graph.CallStatusAmbiguous,
				Tier:        resolver.TierTypeNarrowed,
				Reason:      "multiple candidate targets from conditional or wildcard imports",
			}
		} else if extOnly {
			return nil, &resolver.UnresolvedCall{
				CallerUID:   callerUID,
				CallerQName: call.CallerQName,
				File:        file.Path,
				Line:        call.Line,
				Column:      uint16(call.Col),
				CalleeExpr:  call.CalleeName,
				LookupKey:   resolver.BuildLookupKey("", modHitPkg, call.CalleeName),
				Status:      graph.CallStatusExternal,
				Tier:        resolver.TierUnresolved,
				Reason:      "external Python symbol import",
			}
		}

		// Python builtins (`len`, `print`, `super`, etc.).
		if isPythonBuiltin(call.CalleeName) {
			return nil, &resolver.UnresolvedCall{
				CallerUID:   callerUID,
				CallerQName: call.CallerQName,
				File:        file.Path,
				Line:        call.Line,
				Column:      uint16(call.Col),
				CalleeExpr:  call.CalleeName,
				LookupKey:   resolver.BuildLookupKey("", "builtins", call.CalleeName),
				Status:      graph.CallStatusExternal,
				Tier:        resolver.TierUnresolved,
				Reason:      "Python builtin: " + call.CalleeName,
			}
		}

		return nil, &resolver.UnresolvedCall{
			CallerUID:   callerUID,
			CallerQName: call.CallerQName,
			File:        file.Path,
			Line:        call.Line,
			Column:      uint16(call.Col),
			CalleeExpr:  call.CalleeName,
			LookupKey:   resolver.BuildLookupKey("", modScope, call.CalleeName),
			Status:      graph.CallStatusUnresolved,
			Tier:        resolver.TierUnresolved,
			Reason:      "unresolved Python direct call",
		}
	}

	// 2. Attribute / qualified call: recv.method(...)
	recv := strings.TrimSpace(call.ReceiverName)

	// Tier 1: `super().method()` / `super(Cls, self).method()`
	if enclosingClass.Name != "" && isPythonSuperReceiver(recv) {
		clsPkg := pkgOrDefault(PackageScopePrefix(enclosingClass.QName), modScope)
		if target, viaBase, ok := resolvePythonSuperMethod(rc, clsPkg, enclosingClass, call.CalleeName, localClassesByName, bindings, modScope); ok {
			return makePythonResolvedCall(callerUID, call, target, resolver.TierSelf, "super() via "+viaBase+": "+call.CalleeName, clsPkg), nil
		}
	}

	// Tier 1: `self.method()` or `cls.method()`
	if enclosingClass.Name != "" && (recv == "self" || recv == "cls") {
		clsPkg := pkgOrDefault(PackageScopePrefix(enclosingClass.QName), modScope)
		if target, viaBase, ok := resolvePythonMethodOnHierarchy(rc, clsPkg, enclosingClass.Name, call.CalleeName, localClassesByName, bindings, modScope); ok {
			desc := recv + "." + call.CalleeName
			if viaBase != "" {
				desc = recv + " inherited from " + viaBase + ": " + call.CalleeName
			}
			return makePythonResolvedCall(callerUID, call, target, resolver.TierSelf, desc, clsPkg), nil
		}
	}

	// Tier 3: Imported module alias or dotted module path (`cache.get()`, `app.storage.cache.get()`)
	if target, targetMod, isMod, extOnly := resolvePythonModuleCall(rc, bindings, recv, call.CalleeName); isMod {
		if !target.UID.IsZero() {
			return makePythonResolvedCall(callerUID, call, target, resolver.TierQualifiedImport, "module import call: "+target.QName, targetMod), nil
		}
		if extOnly {
			return nil, &resolver.UnresolvedCall{
				CallerUID:   callerUID,
				CallerQName: call.CallerQName,
				File:        file.Path,
				Line:        call.Line,
				Column:      uint16(call.Col),
				CalleeExpr:  recv + "." + call.CalleeName,
				LookupKey:   resolver.BuildLookupKey("", targetMod, call.CalleeName),
				Status:      graph.CallStatusExternal,
				Tier:        resolver.TierUnresolved,
				Reason:      "external Python module call: " + targetMod,
			}
		}
	}

	// Tier 4: Receiver type inference (`self.repo.load()`, `svc.fetch()`, `client.save()`)
	if recvType, ok := lookupPythonReceiverType(call.CallerQName, enclosingClass.QName, recv, types, localClassesByName); ok {
		typeName := resolver.StripTypeDecorations(recvType.Name)
		targetPkg := recvType.Package
		if targetPkg == "" && recvType.QName != "" {
			targetPkg = PackageScopePrefix(recvType.QName)
		}
		if targetPkg == "" {
			if _, boundPkg, found := resolvePythonSymbolType(rc, bindings, modScope, typeName); found {
				targetPkg = boundPkg
			} else {
				targetPkg = modScope
			}
		}

		if target, viaBase, found := resolvePythonMethodOnHierarchy(rc, targetPkg, typeName, call.CalleeName, localClassesByName, bindings, modScope); found {
			desc := "receiver type inferred: " + typeName + "." + call.CalleeName
			if viaBase != "" {
				desc = "receiver type inherited via " + viaBase + ": " + call.CalleeName
			}
			return makePythonResolvedCall(callerUID, call, target, resolver.TierReceiverType, desc, targetPkg), nil
		}

		if isExternalPythonPackage(targetPkg, bindings) {
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
				Reason:      "method on external Python type " + typeName,
			}
		}

		// Unique method name fallback constrained to receiver class name.
		if rc.View != nil && typeName != "" {
			if target, unique, cands := FilterUniqueFunctionMatches(rc.View.GetByName(call.CalleeName), false, func(n graph.Node) bool {
				return resolver.StripTypeDecorations(n.Receiver) == typeName
			}); unique {
				return makePythonResolvedCall(callerUID, call, target, resolver.TierReceiverType, "receiver class fallback: "+typeName+"."+call.CalleeName, targetPkg), nil
			} else if len(cands) > 1 {
				return nil, &resolver.UnresolvedCall{
					CallerUID:   callerUID,
					CallerQName: call.CallerQName,
					File:        file.Path,
					Line:        call.Line,
					Column:      uint16(call.Col),
					CalleeExpr:  recv + "." + call.CalleeName,
					LookupKey:   resolver.BuildLookupKey(typeName, targetPkg, call.CalleeName),
					Candidates:  cands,
					Status:      graph.CallStatusAmbiguous,
					Tier:        resolver.TierTypeNarrowed,
					Reason:      "multiple candidate methods on Python type " + typeName,
				}
			}
		}
	}

	return nil, &resolver.UnresolvedCall{
		CallerUID:   callerUID,
		CallerQName: call.CallerQName,
		File:        file.Path,
		Line:        call.Line,
		Column:      uint16(call.Col),
		CalleeExpr:  recv + "." + call.CalleeName,
		LookupKey:   resolver.BuildLookupKey(recv, modScope, call.CalleeName),
		Status:      graph.CallStatusUnresolved,
		Tier:        resolver.TierUnresolved,
		Reason:      "unresolved Python attribute call",
	}
}

type pySymbolBinding struct {
	TargetModule string
	OrigName     string
	IsExternal   bool
}

type pyModuleBinding struct {
	TargetModule string
	IsExternal   bool
}

type pyBindings struct {
	Symbols   map[string][]pySymbolBinding
	Modules   map[string]pyModuleBinding
	Wildcards []string
	ExtMods   map[string]struct{}
}

func buildPythonBindings(file *parser.ParsedFile, resolved []resolver.ResolvedImport, rc *resolver.ResolutionContext) *pyBindings {
	b := &pyBindings{
		Symbols: make(map[string][]pySymbolBinding),
		Modules: make(map[string]pyModuleBinding),
		ExtMods: make(map[string]struct{}),
	}
	_, curPkg, _ := pythonModuleContext(file)

	// Track resolved results aligned with file.Imports order.
	for i, imp := range file.Imports {
		var targetMod string
		var isExt bool
		if i < len(resolved) {
			ri := resolved[i]
			targetMod = ri.TargetPackage
			if targetMod == "" {
				resolvedMod, valid := resolvePythonImportModule(curPkg, imp.Path, ri.IsRelative)
				if valid {
					targetMod = resolvedMod
				} else {
					targetMod = strings.TrimLeft(imp.Path, ".")
				}
			}
			isExt = ri.IsExternal
		} else {
			resolvedMod, valid := resolvePythonImportModule(curPkg, imp.Path, imp.IsRelative || strings.HasPrefix(imp.Path, "."))
			targetMod = resolvedMod
			isExt = !valid || !hasPythonModule(rc, resolvedMod)
		}

		if isExt && targetMod != "" {
			b.ExtMods[targetMod] = struct{}{}
		}

		if len(imp.Names) == 0 {
			// `import a.b` or `import a.b as c`
			if imp.Alias != "" {
				b.Modules[imp.Alias] = pyModuleBinding{TargetModule: targetMod, IsExternal: isExt}
			} else {
				b.Modules[imp.Path] = pyModuleBinding{TargetModule: targetMod, IsExternal: isExt}
				first := topSegment(imp.Path)
				if _, exists := b.Modules[first]; !exists {
					b.Modules[first] = pyModuleBinding{
						TargetModule: first,
						IsExternal:   !hasPythonModule(rc, first),
					}
				}
			}
			continue
		}

		if imp.IsWildcard {
			if !isExt && targetMod != "" {
				b.Wildcards = append(b.Wildcards, targetMod)
			}
			continue
		}

		for _, orig := range imp.Names {
			if orig == "" || orig == "*" {
				continue
			}
			localName := orig
			if imp.Alias != "" && len(imp.Names) == 1 {
				localName = imp.Alias
			}
			// Check whether `from pkg import submod` targets an indexed submodule directly.
			subMod := targetMod
			if subMod != "" && targetMod != curPkg+"."+orig && !strings.HasSuffix(targetMod, "."+orig) {
				candidateSub := targetMod + "." + orig
				if hasPythonModule(rc, candidateSub) {
					b.Modules[localName] = pyModuleBinding{TargetModule: candidateSub, IsExternal: false}
					b.Symbols[localName] = append(b.Symbols[localName], pySymbolBinding{
						TargetModule: targetMod,
						OrigName:     orig,
						IsExternal:   false,
					})
					continue
				}
			}
			if hasPythonModule(rc, targetMod) && isExt && !isExt {
				isExt = false
			}
			b.Symbols[localName] = append(b.Symbols[localName], pySymbolBinding{
				TargetModule: targetMod,
				OrigName:     orig,
				IsExternal:   isExt,
			})
			if !hasPythonModuleSymbol(rc, targetMod, orig) && !isExt {
				// Might also refer to submod in `from . import sub`.
				if hasPythonModule(rc, subMod+"."+orig) {
					b.Modules[localName] = pyModuleBinding{TargetModule: subMod + "." + orig, IsExternal: false}
				}
			}
		}
	}

	return b
}

func resolvePythonSymbolCallTargets(rc *resolver.ResolutionContext, b *pyBindings, callee string) (targets []graph.Node, extOnly bool, hitMod string) {
	if b == nil {
		return nil, false, ""
	}
	var anyExternal bool
	seenUID := make(map[graph.UID]struct{})

	addTarget := func(n graph.Node, mod string) {
		if n.UID.IsZero() {
			return
		}
		if _, dup := seenUID[n.UID]; dup {
			return
		}
		seenUID[n.UID] = struct{}{}
		targets = append(targets, n)
		if hitMod == "" {
			hitMod = mod
		}
	}

	if binds := b.Symbols[callee]; len(binds) > 0 {
		for _, sb := range binds {
			if sb.IsExternal {
				anyExternal = true
				if hitMod == "" {
					hitMod = sb.TargetModule
				}
				continue
			}
			visited := make(map[string]struct{})
			if n, mod, ok := resolvePythonSymbolOrReexport(rc, sb.TargetModule, sb.OrigName, visited, 0); ok {
				addTarget(n, mod)
			}
		}
	}

	if len(targets) == 0 && !anyExternal {
		for _, wmod := range b.Wildcards {
			visited := make(map[string]struct{})
			if n, mod, ok := resolvePythonSymbolOrReexport(rc, wmod, callee, visited, 0); ok {
				addTarget(n, mod)
			}
		}
	}

	return targets, len(targets) == 0 && anyExternal, hitMod
}

func resolvePythonSymbolOrReexport(rc *resolver.ResolutionContext, modScope, symName string, visited map[string]struct{}, depth int) (graph.Node, string, bool) {
	if rc == nil || modScope == "" || symName == "" || depth > 5 {
		return graph.Node{}, "", false
	}
	visitKey := modScope + "::" + symName
	if _, seen := visited[visitKey]; seen {
		return graph.Node{}, "", false
	}
	visited[visitKey] = struct{}{}

	if rc.View != nil {
		qname := modScope + "::" + symName
		// Direct function or module symbol.
		if fn, ok, _ := FilterUniqueFunctionMatches(rc.View.GetByQName(qname), false, nil); ok {
			return fn, modScope, true
		}
		// Direct class (prefer __init__ constructor target, fallback to Class node).
		for _, n := range rc.View.GetByQName(qname) {
			if n.Kind == graph.KindClass {
				if ctor, okInit := resolvePythonConstructorTarget(rc, modScope, symName, nil, nil, modScope); okInit {
					return ctor, modScope, true
				}
				return n, modScope, true
			}
		}
	}

	// Check submodule file if this package re-exports or accesses child submodule.
	subMod := modScope + "." + symName
	if hasPythonModule(rc, subMod) {
		if n, ok := findPythonClassOrCallable(rc, subMod, symName); ok {
			return n, subMod, true
		}
	}

	// Follow re-exports inside modScope's ParsedFile (`__init__.py` or module file).
	for _, fpath := range pythonModuleFiles(rc, modScope) {
		pf := rc.LookupFile(fpath)
		if pf == nil {
			continue
		}
		_, curPkg, _ := pythonModuleContext(pf)
		for _, imp := range pf.Imports {
			if imp.IsWildcard {
				if nextMod, ok := resolvePythonImportModule(curPkg, imp.Path, imp.IsRelative || strings.HasPrefix(imp.Path, ".")); ok {
					if n, resMod, found := resolvePythonSymbolOrReexport(rc, nextMod, symName, visited, depth+1); found {
						return n, resMod, true
					}
				}
				continue
			}
			for _, orig := range imp.Names {
				bound := orig
				if imp.Alias != "" && len(imp.Names) == 1 {
					bound = imp.Alias
				}
				if bound == symName {
					if nextMod, ok := resolvePythonImportModule(curPkg, imp.Path, imp.IsRelative || strings.HasPrefix(imp.Path, ".")); ok {
						if n, resMod, found := resolvePythonSymbolOrReexport(rc, nextMod, orig, visited, depth+1); found {
							return n, resMod, true
						}
					}
				}
			}
		}
	}

	return graph.Node{}, "", false
}

func resolvePythonModuleCall(rc *resolver.ResolutionContext, b *pyBindings, recv, callee string) (target graph.Node, modPath string, isMod bool, extOnly bool) {
	if b == nil || recv == "" {
		return graph.Node{}, "", false, false
	}
	if mb, ok := b.Modules[recv]; ok {
		if mb.IsExternal {
			return graph.Node{}, mb.TargetModule, true, true
		}
		if n, hitMod, found := resolvePythonSymbolOrReexport(rc, mb.TargetModule, callee, make(map[string]struct{}), 0); found {
			return n, hitMod, true, false
		}
		return graph.Node{}, mb.TargetModule, true, false
	}

	// Support dotted module path (`import app.storage.cache` -> `app.storage.cache.make()`).
	parts := strings.Split(recv, ".")
	if len(parts) > 1 {
		if mb, ok := b.Modules[parts[0]]; ok {
			candidateMod := recv
			if mb.TargetModule != parts[0] && mb.TargetModule != "" {
				candidateMod = mb.TargetModule + "." + strings.Join(parts[1:], ".")
			}
			if hasPythonModule(rc, candidateMod) {
				if n, hitMod, found := resolvePythonSymbolOrReexport(rc, candidateMod, callee, make(map[string]struct{}), 0); found {
					return n, hitMod, true, false
				}
			}
			if mb.IsExternal {
				return graph.Node{}, candidateMod, true, true
			}
		}
	}

	return graph.Node{}, "", false, false
}

func resolvePythonModuleMember(rc *resolver.ResolutionContext, b *pyBindings, recv, member string) (graph.Node, string, bool) {
	t, mod, isMod, ext := resolvePythonModuleCall(rc, b, recv, member)
	if !isMod || ext {
		return graph.Node{}, mod, isMod
	}
	return t, mod, true
}

func resolvePythonBindingCall(rc *resolver.ResolutionContext, b *pyBindings, modScope, name string) (graph.Node, bool) {
	if rc != nil && rc.View != nil && modScope != "" {
		if n, ok, _ := FilterUniqueFunctionMatches(rc.View.GetByQName(modScope+"::"+name), false, nil); ok {
			return n, true
		}
		for _, n := range rc.View.GetByQName(modScope + "::" + name) {
			if n.Kind == graph.KindClass {
				return n, true
			}
		}
	}
	targets, _, _ := resolvePythonSymbolCallTargets(rc, b, name)
	if len(targets) == 1 {
		return targets[0], true
	}
	return graph.Node{}, false
}

func resolvePythonSymbolType(rc *resolver.ResolutionContext, b *pyBindings, modScope, name string) (graph.Node, string, bool) {
	if name == "" {
		return graph.Node{}, "", false
	}
	if rc != nil && rc.View != nil && modScope != "" {
		for _, n := range rc.View.GetByQName(modScope + "::" + name) {
			if n.Kind == graph.KindClass {
				return n, modScope, true
			}
		}
	}
	if b != nil {
		for _, sb := range b.Symbols[name] {
			if sb.IsExternal {
				continue
			}
			if n, hitMod, ok := resolvePythonSymbolOrReexport(rc, sb.TargetModule, sb.OrigName, make(map[string]struct{}), 0); ok {
				if n.Kind == graph.KindClass {
					return n, hitMod, true
				}
				if n.Name == "__init__" || n.Name == "__new__" {
					clsQName := strings.TrimSuffix(n.QName, "::"+n.Name)
					for _, cn := range rc.View.GetByQName(clsQName) {
						if cn.Kind == graph.KindClass {
							return cn, PackageScopePrefix(cn.QName), true
						}
					}
				}
			}
		}
	}
	return graph.Node{}, "", false
}

func inferTargetReturnOrClass(rc *resolver.ResolutionContext, target graph.Node) (clsNode graph.Node, pkg string, ok bool) {
	if target.UID.IsZero() {
		return graph.Node{}, "", false
	}
	targetPkg := PackageScopePrefix(target.QName)
	if target.Kind == graph.KindClass {
		return target, targetPkg, true
	}
	if target.Name == "__init__" || target.Name == "__new__" {
		clsQName := strings.TrimSuffix(target.QName, "::"+target.Name)
		if rc != nil && rc.View != nil {
			for _, n := range rc.View.GetByQName(clsQName) {
				if n.Kind == graph.KindClass {
					return n, PackageScopePrefix(n.QName), true
				}
			}
		}
		return graph.Node{Name: path.Base(strings.ReplaceAll(clsQName, "::", "/")), QName: clsQName, Kind: graph.KindClass}, targetPkg, true
	}
	if ret := resolver.StripTypeDecorations(target.ReturnType); ret != "" && rc != nil && rc.View != nil {
		for _, n := range rc.View.GetByQName(targetPkg + "::" + ret) {
			if n.Kind == graph.KindClass {
				return n, targetPkg, true
			}
		}
		return graph.Node{Name: ret, QName: targetPkg + "::" + ret, Kind: graph.KindClass}, targetPkg, true
	}
	return graph.Node{}, "", false
}

func resolvePythonConstructorTarget(
	rc *resolver.ResolutionContext,
	pkgScope, className string,
	localClassesByName map[string]parser.ParsedClass,
	bindings *pyBindings,
	fallbackMod string,
) (graph.Node, bool) {
	if initFn, _, ok := resolvePythonMethodOnHierarchy(rc, pkgScope, className, "__init__", localClassesByName, bindings, fallbackMod); ok {
		return initFn, true
	}
	if rc != nil && rc.View != nil && pkgScope != "" {
		for _, n := range rc.View.GetByQName(pkgScope + "::" + className) {
			if n.Kind == graph.KindClass {
				return n, true
			}
		}
	}
	return graph.Node{}, false
}

func resolvePythonSuperMethod(
	rc *resolver.ResolutionContext,
	pkgScope string,
	cls parser.ParsedClass,
	methodName string,
	localClassesByName map[string]parser.ParsedClass,
	bindings *pyBindings,
	fallbackMod string,
) (graph.Node, string, bool) {
	for _, base := range cls.Bases {
		basePkg, baseName := resolvePythonBaseRef(rc, pkgScope, base, localClassesByName, bindings, fallbackMod)
		if n, via, ok := resolvePythonMethodOnHierarchy(rc, basePkg, baseName, methodName, localClassesByName, bindings, fallbackMod); ok {
			if via == "" {
				via = baseName
			}
			return n, via, true
		}
	}
	return graph.Node{}, "", false
}

func resolvePythonMethodOnHierarchy(
	rc *resolver.ResolutionContext,
	pkgScope, className, methodName string,
	localClassesByName map[string]parser.ParsedClass,
	bindings *pyBindings,
	fallbackMod string,
) (node graph.Node, viaBase string, ok bool) {
	if className == "" || methodName == "" {
		return graph.Node{}, "", false
	}
	type item struct {
		pkg  string
		cls  string
		via  string
		step int
	}
	visited := make(map[string]struct{})
	queue := []item{{pkg: pkgScope, cls: className}}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		key := cur.pkg + "::" + cur.cls
		if _, seen := visited[key]; seen {
			continue
		}
		visited[key] = struct{}{}

		if rc != nil && rc.View != nil && cur.pkg != "" {
			qname := cur.pkg + "::" + cur.cls + "::" + methodName
			if n, found, _ := FilterUniqueFunctionMatches(rc.View.GetByQName(qname), false, nil); found {
				return n, cur.via, true
			}
		}

		for _, base := range lookupPythonBases(rc, cur.pkg, cur.cls, localClassesByName) {
			basePkg, baseName := resolvePythonBaseRef(rc, cur.pkg, base, localClassesByName, bindings, fallbackMod)
			if baseName == "" {
				continue
			}
			via := baseName
			if cur.via != "" {
				via = cur.via + " -> " + baseName
			}
			queue = append(queue, item{pkg: basePkg, cls: baseName, via: via, step: cur.step + 1})
		}
	}

	return graph.Node{}, "", false
}

func lookupPythonBases(rc *resolver.ResolutionContext, pkgScope, clsName string, localClassesByName map[string]parser.ParsedClass) []string {
	if localClassesByName != nil {
		if cls, ok := localClassesByName[clsName]; ok {
			return cls.Bases
		}
	}
	if rc != nil && rc.View != nil && pkgScope != "" {
		for _, n := range rc.View.GetByQName(pkgScope + "::" + clsName) {
			if n.Kind == graph.KindClass && len(n.Bases) > 0 {
				return n.Bases
			}
		}
	}
	return nil
}

func resolvePythonBaseRef(
	rc *resolver.ResolutionContext,
	curPkg, rawBase string,
	localClassesByName map[string]parser.ParsedClass,
	bindings *pyBindings,
	fallbackMod string,
) (pkg, name string) {
	clean := resolver.StripTypeDecorations(strings.TrimSpace(rawBase))
	if clean == "" || clean == "object" || clean == "ABC" || clean == "Protocol" {
		return "", ""
	}
	if idx := strings.LastIndexByte(clean, '.'); idx >= 0 {
		modAlias := clean[:idx]
		sym := clean[idx+1:]
		if bindings != nil {
			if mb, ok := bindings.Modules[modAlias]; ok && !mb.IsExternal {
				return mb.TargetModule, sym
			}
		}
		return modAlias, sym
	}
	if localClassesByName != nil {
		if _, ok := localClassesByName[clean]; ok && (curPkg == fallbackMod || fallbackMod != "") {
			return fallbackMod, clean
		}
	}
	if rc != nil && rc.View != nil && curPkg != "" {
		if len(rc.View.GetByQName(curPkg+"::"+clean)) > 0 {
			return curPkg, clean
		}
	}
	if bindings != nil {
		for _, sb := range bindings.Symbols[clean] {
			if !sb.IsExternal {
				return sb.TargetModule, sb.OrigName
			}
		}
	}
	return curPkg, clean
}

func lookupPythonReceiverType(
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

	// Handle `self.attr` / `cls.attr` first via direct lookup, then walk dotted chain.
	startIdx := 1
	var cur resolver.TypeInfo
	var ok bool

	if parts[0] == "self" || parts[0] == "cls" {
		twoPart := parts[0] + "." + strings.TrimSpace(parts[1])
		if cur, ok = resolver.LookupFlat(types, callerQName, twoPart); ok {
			startIdx = 2
		} else if enclosingClsQName != "" {
			if cur, ok = resolver.LookupFlat(types, enclosingClsQName, strings.TrimSpace(parts[1])); ok {
				startIdx = 2
			}
		}
	}
	if !ok {
		cur, ok = resolver.LookupFlat(types, callerQName, strings.TrimSpace(parts[0]))
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
		if cls, found := localClassesByQName[cur.QName]; found {
			matched := false
			for _, f := range cls.Fields {
				if f.Name == fieldName && f.Type != "" {
					cur = resolver.TypeInfo{Name: resolver.StripTypeDecorations(f.Type), Package: cur.Package}
					matched = true
					break
				}
			}
			if matched {
				continue
			}
		}
		return resolver.TypeInfo{}, false
	}

	return cur, true
}

func findEnclosingPythonClass(callerQName string, localClassesByQName map[string]parser.ParsedClass, rc *resolver.ResolutionContext) parser.ParsedClass {
	cur := callerQName
	for cur != "" {
		idx := strings.LastIndex(cur, "::")
		if idx <= 0 {
			break
		}
		parent := cur[:idx]
		if cls, ok := localClassesByQName[parent]; ok {
			return cls
		}
		if rc != nil && rc.View != nil {
			for _, n := range rc.View.GetByQName(parent) {
				if n.Kind == graph.KindClass {
					return parser.ParsedClass{Name: n.Name, QName: n.QName, Bases: n.Bases}
				}
			}
		}
		cur = parent
	}
	return parser.ParsedClass{}
}

func pythonModuleContext(file *parser.ParsedFile) (modName, pkgName string, isInit bool) {
	if file == nil {
		return "", "", false
	}
	slash := strings.ReplaceAll(strings.TrimSpace(file.Path), "\\", "/")
	base := path.Base(slash)
	isInit = base == "__init__.py" || base == "__init__.pyi"
	modName = file.Package
	if modName == "" {
		noExt := strings.TrimPrefix(strings.TrimSuffix(slash, path.Ext(slash)), "./")
		if isInit {
			dir := path.Dir(noExt)
			if dir == "." || dir == "" {
				modName = "__init__"
			} else {
				modName = strings.ReplaceAll(dir, "/", ".")
			}
		} else {
			modName = strings.ReplaceAll(noExt, "/", ".")
		}
	}
	if isInit {
		if modName == "__init__" {
			pkgName = ""
		} else {
			pkgName = modName
		}
	} else {
		if idx := strings.LastIndexByte(modName, '.'); idx >= 0 {
			pkgName = modName[:idx]
		}
	}
	return modName, pkgName, isInit
}

func resolvePythonImportModule(curPkg, impPath string, isRel bool) (targetMod string, valid bool) {
	trimmed := strings.TrimSpace(impPath)
	if trimmed == "" {
		return "", false
	}
	if !isRel && !strings.HasPrefix(trimmed, ".") {
		return trimmed, true
	}

	dots := 0
	for dots < len(trimmed) && trimmed[dots] == '.' {
		dots++
	}
	sub := strings.TrimPrefix(trimmed[dots:], ".")

	parts := []string(nil)
	if curPkg != "" {
		parts = strings.Split(curPkg, ".")
	}
	climb := dots - 1
	if climb > len(parts) {
		return "", false
	}
	baseParts := parts[:len(parts)-climb]
	if sub != "" {
		baseParts = append(baseParts, sub)
	}
	if len(baseParts) == 0 {
		return "", false
	}
	return strings.Join(baseParts, "."), true
}

func findPythonModuleFile(rc *resolver.ResolutionContext, modName string) (string, bool) {
	files := pythonModuleFiles(rc, modName)
	if len(files) == 0 {
		return "", false
	}
	return files[0], true
}

func hasPythonModule(rc *resolver.ResolutionContext, modName string) bool {
	return len(pythonModuleFiles(rc, modName)) > 0
}

func pythonModuleFiles(rc *resolver.ResolutionContext, modName string) []string {
	if rc == nil || modName == "" {
		return nil
	}
	if rc.NameIndex != nil {
		if files := rc.NameIndex.FilesForPackage(modName); len(files) > 0 {
			return files
		}
	}
	if len(rc.Files) > 0 {
		var matches []string
		for _, pf := range rc.Files {
			if pf != nil && pf.Package == modName {
				matches = append(matches, pf.Path)
			}
		}
		if len(matches) > 0 {
			return matches
		}
	}
	return nil
}

func hasPythonModuleSymbol(rc *resolver.ResolutionContext, modName, sym string) bool {
	if rc == nil || rc.View == nil || modName == "" || sym == "" {
		return false
	}
	return len(rc.View.GetByQName(modName+"::"+sym)) > 0
}

func findPythonClassOrCallable(rc *resolver.ResolutionContext, modScope, sym string) (graph.Node, bool) {
	if rc == nil || rc.View == nil || modScope == "" || sym == "" {
		return graph.Node{}, false
	}
	qname := modScope + "::" + sym
	if n, ok, _ := FilterUniqueFunctionMatches(rc.View.GetByQName(qname), false, nil); ok {
		return n, true
	}
	for _, n := range rc.View.GetByQName(qname) {
		if n.Kind == graph.KindClass {
			return n, true
		}
	}
	return graph.Node{}, false
}

func makePythonResolvedCall(callerUID graph.UID, call parser.ParsedCall, target graph.Node, tier graph.ResolutionTier, methodDesc, pkgScope string) *resolver.ResolvedCall {
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

func isPythonSuperReceiver(recv string) bool {
	return recv == "super()" || strings.HasPrefix(recv, "super(")
}

func isExternalPythonPackage(pkg string, b *pyBindings) bool {
	if pkg == "" || b == nil {
		return false
	}
	_, ok := b.ExtMods[pkg]
	return ok
}

func pkgOrDefault(pkg, def string) string {
	if pkg != "" {
		return pkg
	}
	return def
}

func topSegment(dotted string) string {
	if idx := strings.IndexByte(dotted, '.'); idx >= 0 {
		return dotted[:idx]
	}
	return dotted
}

func isPythonBuiltin(name string) bool {
	switch name {
	case "abs", "all", "any", "ascii", "bin", "bool", "breakpoint", "bytearray", "bytes",
		"callable", "chr", "classmethod", "compile", "complex", "delattr", "dict", "dir",
		"divmod", "enumerate", "eval", "exec", "filter", "float", "format", "frozenset",
		"getattr", "globals", "hasattr", "hash", "help", "hex", "id", "input", "int",
		"isinstance", "issubclass", "iter", "len", "list", "locals", "map", "max",
		"memoryview", "min", "next", "object", "oct", "open", "ord", "pow", "print",
		"property", "range", "repr", "reversed", "round", "set", "setattr", "slice",
		"sorted", "staticmethod", "str", "sum", "super", "tuple", "type", "vars", "zip",
		"__import__":
		return true
	default:
		return false
	}
}

func isUpperStart(s string) bool {
	if s == "" {
		return false
	}
	r, _ := utf8.DecodeRuneInString(s)
	return unicode.IsUpper(r)
}

var _ = isUpperStart
