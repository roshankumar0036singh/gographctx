package lang

import (
	"context"
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/roshankumar0036singh/gographctx/internal/graph"
	"github.com/roshankumar0036singh/gographctx/internal/parser"
	"github.com/roshankumar0036singh/gographctx/internal/resolver"
)

// TSConfig holds TypeScript compilerOptions module-resolution settings parsed
// from tsconfig.json (baseUrl and path alias mappings).
type TSConfig struct {
	BaseURL string              // cleaned slash path relative to repoRoot
	Paths   map[string][]string // pattern (e.g. "@/*") -> replacement targets
}

// PackageJSON holds subset fields from package.json needed for Node/ESM resolution.
type PackageJSON struct {
	Name    string
	Main    string
	Module  string
	Types   string
	Exports map[string]string // export subpath (e.g. ".", "./utils") -> target relative file
}

// ParseTSConfig reads and parses tsconfig.json inside repoRoot, supporting JSONC
// line/block comments, trailing commas, and one level of relative "extends".
func ParseTSConfig(repoRoot string) (TSConfig, error) {
	if strings.TrimSpace(repoRoot) == "" {
		return TSConfig{Paths: map[string][]string{}}, nil
	}
	return parseTSConfigFile(filepath.Join(repoRoot, "tsconfig.json"), repoRoot, 0)
}

func parseTSConfigFile(cfgPath, repoRoot string, depth int) (TSConfig, error) {
	out := TSConfig{Paths: make(map[string][]string)}
	if depth > 4 {
		return out, nil
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return out, err
	}

	cleaned := stripJSONCommentsAndTrailingCommas(raw)
	var doc struct {
		Extends         string `json:"extends"`
		CompilerOptions struct {
			BaseURL string              `json:"baseUrl"`
			Paths   map[string][]string `json:"paths"`
		} `json:"compilerOptions"`
	}
	if err := json.Unmarshal(cleaned, &doc); err != nil {
		return out, err
	}

	if doc.Extends != "" && (strings.HasPrefix(doc.Extends, "./") || strings.HasPrefix(doc.Extends, "../")) {
		parentPath := filepath.Join(filepath.Dir(cfgPath), doc.Extends)
		if filepath.Ext(parentPath) == "" {
			parentPath += ".json"
		}
		if baseCfg, err := parseTSConfigFile(parentPath, repoRoot, depth+1); err == nil {
			out.BaseURL = baseCfg.BaseURL
			for k, v := range baseCfg.Paths {
				out.Paths[k] = append([]string(nil), v...)
			}
		}
	}

	if doc.CompilerOptions.BaseURL != "" {
		cfgDirRel, err := filepath.Rel(repoRoot, filepath.Dir(cfgPath))
		if err != nil {
			cfgDirRel = "."
		}
		joined := path.Clean(path.Join(filepath.ToSlash(cfgDirRel), filepath.ToSlash(doc.CompilerOptions.BaseURL)))
		out.BaseURL = strings.TrimPrefix(joined, "./")
		if out.BaseURL == "." {
			out.BaseURL = ""
		}
	}
	for k, targets := range doc.CompilerOptions.Paths {
		out.Paths[k] = append([]string(nil), targets...)
	}
	return out, nil
}

// ParsePackageJSON reads and parses a package.json file into PackageJSON.
func ParsePackageJSON(pkgPath string) (PackageJSON, error) {
	out := PackageJSON{Exports: make(map[string]string)}
	raw, err := os.ReadFile(pkgPath)
	if err != nil {
		return out, err
	}
	var doc struct {
		Name    string          `json:"name"`
		Main    string          `json:"main"`
		Module  string          `json:"module"`
		Types   string          `json:"types"`
		Exports json.RawMessage `json:"exports"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return out, err
	}
	out.Name = doc.Name
	out.Main = strings.TrimSpace(doc.Main)
	out.Module = strings.TrimSpace(doc.Module)
	out.Types = strings.TrimSpace(doc.Types)
	parsePackageExports(doc.Exports, out.Exports)
	return out, nil
}

func parsePackageExports(raw json.RawMessage, out map[string]string) {
	if len(raw) == 0 {
		return
	}
	var s string
	if json.Unmarshal(raw, &s) == nil && s != "" {
		out["."] = s
		return
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return
	}
	// Conditional map at top level: {"import": "./esm/index.js", "default": "./index.js"}
	if target := pickExportTarget(obj); target != "" {
		out["."] = target
	}
	for k, val := range obj {
		if !strings.HasPrefix(k, ".") {
			continue
		}
		var strVal string
		if json.Unmarshal(val, &strVal) == nil && strVal != "" {
			out[k] = strVal
			continue
		}
		var sub map[string]json.RawMessage
		if json.Unmarshal(val, &sub) == nil {
			if t := pickExportTarget(sub); t != "" {
				out[k] = t
			}
		}
	}
}

func pickExportTarget(obj map[string]json.RawMessage) string {
	for _, key := range []string{"import", "module", "types", "default", "require", "node"} {
		raw, ok := obj[key]
		if !ok {
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			return s
		}
		var nested map[string]json.RawMessage
		if json.Unmarshal(raw, &nested) == nil {
			if t := pickExportTarget(nested); t != "" {
				return t
			}
		}
	}
	return ""
}

// TypeScriptResolver implements LanguageResolver for TypeScript, TSX, and
// JavaScript modules using tsconfig path aliases, Node/ESM module resolution,
// barrel re-exports, class method resolution, and JSX component resolution.
type TypeScriptResolver struct {
	language string
}

// NewTypeScriptResolver returns a LanguageResolver for TypeScript ("typescript").
func NewTypeScriptResolver() *TypeScriptResolver {
	return &TypeScriptResolver{language: "typescript"}
}

// NewTSXResolver returns a LanguageResolver for TSX ("tsx").
func NewTSXResolver() *TypeScriptResolver {
	return &TypeScriptResolver{language: "tsx"}
}

// NewJavaScriptResolver returns a LanguageResolver for JavaScript ("javascript").
func NewJavaScriptResolver() *TypeScriptResolver {
	return &TypeScriptResolver{language: "javascript"}
}

// Language returns the resolver's canonical language identifier.
func (r *TypeScriptResolver) Language() string {
	return r.language
}

// ResolveImports resolves relative ESM, tsconfig alias/baseUrl, barrel index,
// and node_modules / external package imports.
func (r *TypeScriptResolver) ResolveImports(ctx context.Context, file *parser.ParsedFile, rc *resolver.ResolutionContext) ([]resolver.ResolvedImport, error) {
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

	tsCfg, _ := ParseTSConfig(rc.RepoRoot)
	fileDir := path.Dir(normalizeTSPath(file.Path))
	if fileDir == "." {
		fileDir = ""
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
			IsTypeOnly:    imp.IsTypeOnly,
			IsRelative:    imp.IsRelative || isTSRelativeSpecifier(imp.Path),
		}
		if ri.Alias == "" && len(imp.Names) == 1 {
			ri.Alias = imp.Names[0]
		}

		resFile, resScope, isNodeMod, found := resolveTSImportTarget(rc, tsCfg, fileDir, imp.Path)
		if found {
			ri.TargetFile = resFile
			ri.TargetPackage = resScope
			ri.IsExternal = isNodeMod
		} else {
			ri.IsExternal = !ri.IsRelative
		}

		out = append(out, ri)
	}

	return out, nil
}

// InferTypes builds scoped TypeScript/JavaScript types for local variables,
// parameters, class properties, imported constructors, and async/factory returns.
func (r *TypeScriptResolver) InferTypes(ctx context.Context, file *parser.ParsedFile, rc *resolver.ResolutionContext) (map[string]resolver.TypeInfo, error) {
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
	bindings := buildTSBindings(file, imports, rc)
	modScope := tsModuleScope(file)

	// Enrich parameter types from imported classes/interfaces.
	for _, fn := range file.Functions {
		for _, p := range fn.Params {
			if p.Name == "" || p.Type == "" {
				continue
			}
			base := resolver.StripTypeDecorations(p.Type)
			if clsNode, pkg, found := resolveTSSymbolType(rc, bindings, modScope, base); found {
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

	// Enrich LocalVars constructed from imported classes, namespace constructors (`new ns.Client()`),
	// or cross-module factory functions.
	for pass := 0; pass < 2; pass++ {
		for _, lv := range file.LocalVars {
			if lv.ExplicitType != "" {
				if prev, ok := tm.Lookup(lv.ScopeQName, lv.Name); ok {
					base := stripTSPromiseWrapper(resolver.StripTypeDecorations(prev.Name))
					if clsNode, pkg, found := resolveTSSymbolType(rc, bindings, modScope, base); found {
						prev.Name = clsNode.Name
						prev.QName = clsNode.QName
						prev.Package = pkg
						tm.Set(lv.ScopeQName, lv.Name, prev)
					}
				}
				continue
			}
			if lv.InitCallee == "" && lv.InitAlias == "" {
				continue
			}

			var inferred resolver.TypeInfo
			var hasInferred bool

			switch {
			case lv.InitCallee != "":
				var target graph.Node
				var modPkg string
				var foundTarget bool

				if lv.InitReceiver == "" {
					target, modPkg, foundTarget = resolveTSDirectBindingTarget(rc, bindings, modScope, lv.InitCallee)
				} else if mb, ok := bindings.Namespaces[lv.InitReceiver]; ok && !mb.IsExternal {
					if n, hitMod, okSym := resolveTSSymbolOrReexport(rc, mb.TargetPackage, mb.TargetFile, lv.InitCallee, make(map[string]struct{}), 0); okSym {
						target = n
						modPkg = hitMod
						foundTarget = true
					}
				} else if recvInfo, ok := tm.LookupReceiver(lv.ScopeQName, lv.InitReceiver); ok {
					recvPkg := pkgOrDefault(recvInfo.Package, modScope)
					if n, _, okMeth := resolveTSMethodOnHierarchy(rc, recvPkg, resolver.StripTypeDecorations(recvInfo.Name), lv.InitCallee, nil, bindings, modScope); okMeth {
						target = n
						modPkg = PackageScopePrefix(n.QName)
						foundTarget = true
					}
				}

				if foundTarget {
					if clsNode, pkg, okType := inferTSTargetReturnOrClass(rc, target, modPkg, bindings, modScope); okType {
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
				}

			case lv.InitAlias != "":
				if rhsInfo, ok := tm.LookupReceiver(lv.ScopeQName, lv.InitAlias); ok && rhsInfo.Name != "" {
					inferred = rhsInfo
					inferred.Line = lv.Line
					hasInferred = true
				}
			}

			if hasInferred {
				tm.Set(lv.ScopeQName, lv.Name, inferred)
				promoteTSFieldIfThis(tm, lv, inferred)
			}
		}
	}

	return tm.ToFlatMap(), nil
}

// ResolveCalls resolves local, this/super, imported/barrel/alias, namespace,
// class instance method, and JSX component calls into Tier 1-8 outcomes.
func (r *TypeScriptResolver) ResolveCalls(
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

	modScope := tsModuleScope(file)
	bindings := buildTSBindings(file, imports, rc)

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

func (r *TypeScriptResolver) resolveOneCall(
	file *parser.ParsedFile,
	rc *resolver.ResolutionContext,
	callerUID graph.UID,
	call parser.ParsedCall,
	modScope string,
	bindings *tsBindings,
	localClassesByName map[string]parser.ParsedClass,
	localClassesByQName map[string]parser.ParsedClass,
	types map[string]resolver.TypeInfo,
) (*resolver.ResolvedCall, *resolver.UnresolvedCall) {
	enclosingClass := FindEnclosingClass(call.CallerQName, localClassesByQName, rc)

	// 1. Direct / unqualified call: fn(), new Foo(), <Component />
	if !call.IsMethodCall || call.ReceiverName == "" {
		// Same-file function or component.
		if !call.IsConstructor {
			if target, ok := FindSameFileFuncUID(rc, file, call.CalleeName, ""); ok {
				return makeTSResolvedCall(callerUID, call, target, resolver.TierLocal, "local definition: "+target.QName, modScope), nil
			}
		}

		// Same-file class instantiation (`new Foo()`) or class component (`<Foo />`).
		if cls, ok := localClassesByName[call.CalleeName]; ok {
			if target, okCtor := resolveTSConstructorOrClass(rc, modScope, cls.Name, localClassesByName, bindings, modScope); okCtor {
				return makeTSResolvedCall(callerUID, call, target, resolver.TierLocal, "local class target: "+target.QName, modScope), nil
			}
		}

		// Imported symbol (`import { fn } from ...`, `import Svc from ...`, barrel re-exports, CommonJS).
		if targets, extOnly, hitPkg := resolveTSSymbolCallTargets(rc, bindings, call.CalleeName); len(targets) > 0 {
			if len(targets) == 1 {
				return makeTSResolvedCall(callerUID, call, targets[0], resolver.TierQualifiedImport, "imported symbol: "+targets[0].QName, hitPkg), nil
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
				Reason:      "multiple candidate TS/JS import targets",
			}
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
				Reason:      "external JS/TS package import",
			}
		}

		// Common JS/TS builtins & module globals (`require`, `fetch`, `Promise`, `parseInt`, etc.).
		if isJSBuiltin(call.CalleeName) {
			return nil, &resolver.UnresolvedCall{
				CallerUID:   callerUID,
				CallerQName: call.CallerQName,
				File:        file.Path,
				Line:        call.Line,
				Column:      uint16(call.Col),
				CalleeExpr:  call.CalleeName,
				LookupKey:   resolver.BuildLookupKey("", "globals", call.CalleeName),
				Status:      graph.CallStatusExternal,
				Tier:        resolver.TierUnresolved,
				Reason:      "JS/TS builtin or global: " + call.CalleeName,
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
			Reason:      "unresolved JS/TS call",
		}
	}

	// 2. Qualified / method / member JSX call: recv.callee(...) / <UI.Panel />
	recv := strings.TrimSpace(call.ReceiverName)

	// Tier 1: `this.method()` or `super.method()` inside enclosing class.
	if enclosingClass.Name != "" && (recv == "this" || recv == "super") {
		clsPkg := pkgOrDefault(PackageScopePrefix(enclosingClass.QName), modScope)
		if recv == "super" {
			if target, viaBase, ok := resolveTSSuperMethod(rc, clsPkg, enclosingClass, call.CalleeName, localClassesByName, bindings, modScope); ok {
				return makeTSResolvedCall(callerUID, call, target, resolver.TierSelf, "super via "+viaBase+": "+call.CalleeName, clsPkg), nil
			}
		} else if target, viaBase, ok := resolveTSMethodOnHierarchy(rc, clsPkg, enclosingClass.Name, call.CalleeName, localClassesByName, bindings, modScope); ok {
			desc := "this." + call.CalleeName
			if viaBase != "" {
				desc = "this inherited from " + viaBase + ": " + call.CalleeName
			}
			return makeTSResolvedCall(callerUID, call, target, resolver.TierSelf, desc, clsPkg), nil
		}
	}

	// Tier 3: Namespace import (`import * as ns from ...`, CommonJS `const ns = require(...)`, `<UI.Panel />`).
	if nb, ok := bindings.Namespaces[recv]; ok {
		if nb.IsExternal {
			return nil, &resolver.UnresolvedCall{
				CallerUID:   callerUID,
				CallerQName: call.CallerQName,
				File:        file.Path,
				Line:        call.Line,
				Column:      uint16(call.Col),
				CalleeExpr:  recv + "." + call.CalleeName,
				LookupKey:   resolver.BuildLookupKey("", nb.ImportPath, call.CalleeName),
				Status:      graph.CallStatusExternal,
				Tier:        resolver.TierUnresolved,
				Reason:      "external package namespace call: " + nb.ImportPath,
			}
		}
		if target, hitMod, okSym := resolveTSSymbolOrReexport(rc, nb.TargetPackage, nb.TargetFile, call.CalleeName, make(map[string]struct{}), 0); okSym {
			return makeTSResolvedCall(callerUID, call, target, resolver.TierQualifiedImport, "namespace import call: "+target.QName, hitMod), nil
		}
	}

	// Tier 4: Receiver type inferred (`this.repo.save()`, `svc.fetch()`, `client.query()`).
	if recvType, ok := LookupScopedReceiverType(rc, call.CallerQName, enclosingClass.QName, recv, types, localClassesByQName); ok {
		typeName := stripTSPromiseWrapper(resolver.StripTypeDecorations(recvType.Name))
		targetPkg := recvType.Package
		if targetPkg == "" && recvType.QName != "" {
			targetPkg = PackageScopePrefix(recvType.QName)
		}
		if targetPkg == "" {
			if _, boundPkg, found := resolveTSSymbolType(rc, bindings, modScope, typeName); found {
				targetPkg = boundPkg
			} else {
				targetPkg = modScope
			}
		}

		if target, viaBase, found := resolveTSMethodOnHierarchy(rc, targetPkg, typeName, call.CalleeName, localClassesByName, bindings, modScope); found {
			desc := "receiver type inferred: " + typeName + "." + call.CalleeName
			if viaBase != "" {
				desc = "receiver type inherited via " + viaBase + ": " + call.CalleeName
			}
			return makeTSResolvedCall(callerUID, call, target, resolver.TierReceiverType, desc, targetPkg), nil
		}

		if rc.View != nil && typeName != "" {
			if target, unique, cands := FilterUniqueFunctionMatches(rc.View.GetByName(call.CalleeName), false, func(n graph.Node) bool {
				return resolver.StripTypeDecorations(n.Receiver) == typeName
			}); unique {
				return makeTSResolvedCall(callerUID, call, target, resolver.TierReceiverType, "receiver class fallback: "+typeName+"."+call.CalleeName, targetPkg), nil
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
					Reason:      "multiple candidate TS/JS methods on " + typeName,
				}
			}
		}
	}

	if isJSBuiltinReceiver(recv) {
		return nil, &resolver.UnresolvedCall{
			CallerUID:   callerUID,
			CallerQName: call.CallerQName,
			File:        file.Path,
			Line:        call.Line,
			Column:      uint16(call.Col),
			CalleeExpr:  recv + "." + call.CalleeName,
			LookupKey:   resolver.BuildLookupKey(recv, "globals", call.CalleeName),
			Status:      graph.CallStatusExternal,
			Tier:        resolver.TierUnresolved,
			Reason:      "JS/TS runtime global call: " + recv + "." + call.CalleeName,
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
		Reason:      "unresolved JS/TS member call",
	}
}

type tsSymbolBinding struct {
	TargetPackage string
	TargetFile    string
	ImportPath    string
	OrigName      string
	IsExternal    bool
}

type tsNamespaceBinding struct {
	TargetPackage string
	TargetFile    string
	ImportPath    string
	IsExternal    bool
}

type tsBindings struct {
	Symbols    map[string][]tsSymbolBinding
	Namespaces map[string]tsNamespaceBinding
}

func buildTSBindings(file *parser.ParsedFile, resolved []resolver.ResolvedImport, rc *resolver.ResolutionContext) *tsBindings {
	b := &tsBindings{
		Symbols:    make(map[string][]tsSymbolBinding),
		Namespaces: make(map[string]tsNamespaceBinding),
	}
	for i, imp := range file.Imports {
		if imp.Exported {
			// Re-exports (`export { ... } from`) do not create local scope bindings in the barrel file itself.
			continue
		}
		var targetPkg, targetFile string
		var isExt bool
		if i < len(resolved) {
			targetPkg = resolved[i].TargetPackage
			targetFile = resolved[i].TargetFile
			isExt = resolved[i].IsExternal
		} else {
			isExt = !imp.IsRelative && !isTSRelativeSpecifier(imp.Path)
		}

		if imp.IsWildcard && imp.Alias != "" {
			b.Namespaces[imp.Alias] = tsNamespaceBinding{
				TargetPackage: targetPkg,
				TargetFile:    targetFile,
				ImportPath:    imp.Path,
				IsExternal:    isExt,
			}
			continue
		}

		// CommonJS whole-module import (`const mod = require("./mod")`): Alias set, Names empty.
		if len(imp.Names) == 0 && imp.Alias != "" {
			b.Namespaces[imp.Alias] = tsNamespaceBinding{
				TargetPackage: targetPkg,
				TargetFile:    targetFile,
				ImportPath:    imp.Path,
				IsExternal:    isExt,
			}
			b.Symbols[imp.Alias] = append(b.Symbols[imp.Alias], tsSymbolBinding{
				TargetPackage: targetPkg,
				TargetFile:    targetFile,
				ImportPath:    imp.Path,
				OrigName:      "default",
				IsExternal:    isExt,
			})
			continue
		}

		for _, orig := range imp.Names {
			if orig == "" {
				continue
			}
			localName := orig
			if imp.Alias != "" && len(imp.Names) == 1 {
				localName = imp.Alias
			}
			b.Symbols[localName] = append(b.Symbols[localName], tsSymbolBinding{
				TargetPackage: targetPkg,
				TargetFile:    targetFile,
				ImportPath:    imp.Path,
				OrigName:      orig,
				IsExternal:    isExt,
			})
		}
	}
	_ = rc
	return b
}

func resolveTSSymbolCallTargets(rc *resolver.ResolutionContext, b *tsBindings, callee string) (targets []graph.Node, extOnly bool, hitPkg string) {
	if b == nil {
		return nil, false, ""
	}
	var anyExt bool
	seen := make(map[graph.UID]struct{})

	for _, sb := range b.Symbols[callee] {
		if sb.IsExternal {
			anyExt = true
			if hitPkg == "" {
				hitPkg = sb.ImportPath
			}
			continue
		}
		if n, mod, ok := resolveTSSymbolOrReexport(rc, sb.TargetPackage, sb.TargetFile, sb.OrigName, make(map[string]struct{}), 0); ok {
			if _, dup := seen[n.UID]; !dup {
				seen[n.UID] = struct{}{}
				targets = append(targets, n)
			}
			if hitPkg == "" {
				hitPkg = mod
			}
		}
	}

	return targets, len(targets) == 0 && anyExt, hitPkg
}

func resolveTSSymbolOrReexport(rc *resolver.ResolutionContext, targetPkg, targetFile, sym string, visited map[string]struct{}, depth int) (graph.Node, string, bool) {
	if rc == nil || (targetPkg == "" && targetFile == "") || sym == "" || depth > 6 {
		return graph.Node{}, "", false
	}
	if targetPkg == "" && targetFile != "" {
		targetPkg = stripTSExt(normalizeTSPath(targetFile))
	}
	vKey := targetPkg + "::" + sym
	if _, seen := visited[vKey]; seen {
		return graph.Node{}, "", false
	}
	visited[vKey] = struct{}{}

	if sym == "default" {
		if n, ok := resolveTSDefaultExport(rc, targetPkg, targetFile); ok {
			return n, targetPkg, true
		}
	}

	if rc.View != nil && targetPkg != "" {
		qname := targetPkg + "::" + sym
		if fn, ok, _ := FilterUniqueFunctionMatches(rc.View.GetByQName(qname), false, nil); ok {
			return fn, targetPkg, true
		}
		for _, n := range rc.View.GetByQName(qname) {
			if n.Kind == graph.KindClass || n.Kind == graph.KindInterface || n.Kind == graph.KindVariable {
				if n.Kind == graph.KindClass {
					if ctor, _, okCtor := resolveTSMethodOnHierarchy(rc, targetPkg, n.Name, "constructor", nil, nil, targetPkg); okCtor {
						return ctor, targetPkg, true
					}
				}
				return n, targetPkg, true
			}
		}
	}

	// Follow barrel re-exports in targetFile (`export { a as b } from ...`, `export * from ...`).
	pf := rc.LookupFile(targetFile)
	if pf == nil && rc.NameIndex != nil && targetPkg != "" {
		if files := rc.NameIndex.FilesForPackage(targetPkg); len(files) > 0 {
			pf = rc.LookupFile(files[0])
		}
	}
	if pf == nil {
		return graph.Node{}, "", false
	}

	tsCfg, _ := ParseTSConfig(rc.RepoRoot)
	pfDir := path.Dir(normalizeTSPath(pf.Path))
	if pfDir == "." {
		pfDir = ""
	}

	for _, imp := range pf.Imports {
		if !imp.Exported {
			continue
		}
		if imp.IsWildcard {
			nextFile, nextPkg, isExt, found := resolveTSImportTarget(rc, tsCfg, pfDir, imp.Path)
			if found && !isExt {
				if n, hitPkg, ok := resolveTSSymbolOrReexport(rc, nextPkg, nextFile, sym, visited, depth+1); ok {
					return n, hitPkg, true
				}
			}
			continue
		}
		for _, orig := range imp.Names {
			exportedAs := orig
			if imp.Alias != "" && len(imp.Names) == 1 {
				exportedAs = imp.Alias
			}
			if exportedAs == sym {
				nextFile, nextPkg, isExt, found := resolveTSImportTarget(rc, tsCfg, pfDir, imp.Path)
				if found && !isExt {
					if n, hitPkg, ok := resolveTSSymbolOrReexport(rc, nextPkg, nextFile, orig, visited, depth+1); ok {
						return n, hitPkg, true
					}
				}
			}
		}
	}

	return graph.Node{}, "", false
}

func resolveTSDefaultExport(rc *resolver.ResolutionContext, targetPkg, targetFile string) (graph.Node, bool) {
	if rc == nil || rc.View == nil {
		return graph.Node{}, false
	}
	pf := rc.LookupFile(targetFile)
	if pf != nil {
		for _, cls := range pf.Classes {
			if cls.Exported {
				if ctor, ok := resolveTSConstructorOrClass(rc, targetPkg, cls.Name, nil, nil, targetPkg); ok {
					return ctor, true
				}
			}
		}
		for _, fn := range pf.Functions {
			if fn.Exported && !fn.IsMethod {
				if n, ok, _ := FilterUniqueFunctionMatches(rc.View.GetByQName(fn.QName), false, nil); ok {
					return n, true
				}
			}
		}
	}
	if targetFile != "" {
		var fallback []graph.Node
		for _, n := range rc.View.GetByFile(targetFile) {
			if n.Exported && (n.Kind == graph.KindClass || (n.Kind == graph.KindFunction && n.Receiver == "")) {
				fallback = append(fallback, n)
			}
		}
		if len(fallback) == 1 {
			n := fallback[0]
			if n.Kind == graph.KindClass {
				if ctor, ok := resolveTSConstructorOrClass(rc, targetPkg, n.Name, nil, nil, targetPkg); ok {
					return ctor, true
				}
			}
			return n, true
		}
	}
	return graph.Node{}, false
}

func resolveTSDirectBindingTarget(rc *resolver.ResolutionContext, b *tsBindings, modScope, name string) (graph.Node, string, bool) {
	if rc != nil && rc.View != nil && modScope != "" {
		qname := modScope + "::" + name
		if fn, ok, _ := FilterUniqueFunctionMatches(rc.View.GetByQName(qname), false, nil); ok {
			return fn, modScope, true
		}
		for _, n := range rc.View.GetByQName(qname) {
			if n.Kind == graph.KindClass {
				return n, modScope, true
			}
		}
	}
	targets, _, hitPkg := resolveTSSymbolCallTargets(rc, b, name)
	if len(targets) == 1 {
		return targets[0], hitPkg, true
	}
	return graph.Node{}, "", false
}

func resolveTSSymbolType(rc *resolver.ResolutionContext, b *tsBindings, modScope, name string) (graph.Node, string, bool) {
	if name == "" {
		return graph.Node{}, "", false
	}
	if rc != nil && rc.View != nil && modScope != "" {
		for _, n := range rc.View.GetByQName(modScope + "::" + name) {
			if n.Kind == graph.KindClass || n.Kind == graph.KindInterface {
				return n, modScope, true
			}
		}
	}
	if b != nil {
		for _, sb := range b.Symbols[name] {
			if sb.IsExternal {
				continue
			}
			if n, hitMod, ok := resolveTSSymbolOrReexport(rc, sb.TargetPackage, sb.TargetFile, sb.OrigName, make(map[string]struct{}), 0); ok {
				if n.Kind == graph.KindClass || n.Kind == graph.KindInterface {
					return n, hitMod, true
				}
				if n.Name == "constructor" {
					clsQName := strings.TrimSuffix(n.QName, "::constructor")
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

func inferTSTargetReturnOrClass(rc *resolver.ResolutionContext, target graph.Node, modPkg string, bindings *tsBindings, modScope string) (graph.Node, string, bool) {
	if target.UID.IsZero() {
		return graph.Node{}, "", false
	}
	pkg := pkgOrDefault(PackageScopePrefix(target.QName), modPkg)
	if target.Kind == graph.KindClass || target.Kind == graph.KindInterface {
		return target, pkg, true
	}
	if target.Name == "constructor" {
		clsQName := strings.TrimSuffix(target.QName, "::constructor")
		if rc != nil && rc.View != nil {
			for _, n := range rc.View.GetByQName(clsQName) {
				if n.Kind == graph.KindClass {
					return n, PackageScopePrefix(n.QName), true
				}
			}
		}
		return graph.Node{Name: path.Base(strings.ReplaceAll(clsQName, "::", "/")), QName: clsQName, Kind: graph.KindClass}, pkg, true
	}
	ret := stripTSPromiseWrapper(resolver.StripTypeDecorations(target.ReturnType))
	if ret != "" {
		if rc != nil && rc.View != nil && pkg != "" {
			for _, n := range rc.View.GetByQName(pkg + "::" + ret) {
				if n.Kind == graph.KindClass || n.Kind == graph.KindInterface {
					return n, pkg, true
				}
			}
		}
		if clsNode, boundPkg, ok := resolveTSSymbolType(rc, bindings, modScope, ret); ok {
			return clsNode, boundPkg, true
		}
		return graph.Node{Name: ret, QName: pkg + "::" + ret, Kind: graph.KindClass}, pkg, true
	}
	return graph.Node{}, "", false
}

func resolveTSConstructorOrClass(
	rc *resolver.ResolutionContext,
	pkgScope, className string,
	localClassesByName map[string]parser.ParsedClass,
	bindings *tsBindings,
	fallbackMod string,
) (graph.Node, bool) {
	if ctor, _, ok := resolveTSMethodOnHierarchy(rc, pkgScope, className, "constructor", localClassesByName, bindings, fallbackMod); ok {
		return ctor, true
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

func resolveTSSuperMethod(
	rc *resolver.ResolutionContext,
	pkgScope string,
	cls parser.ParsedClass,
	methodName string,
	localClassesByName map[string]parser.ParsedClass,
	bindings *tsBindings,
	fallbackMod string,
) (graph.Node, string, bool) {
	for _, base := range cls.Bases {
		basePkg, baseName := resolveTSBaseRef(rc, pkgScope, base, localClassesByName, bindings, fallbackMod)
		if n, via, ok := resolveTSMethodOnHierarchy(rc, basePkg, baseName, methodName, localClassesByName, bindings, fallbackMod); ok {
			if via == "" {
				via = baseName
			}
			return n, via, true
		}
	}
	return graph.Node{}, "", false
}

func resolveTSMethodOnHierarchy(
	rc *resolver.ResolutionContext,
	pkgScope, className, methodName string,
	localClassesByName map[string]parser.ParsedClass,
	bindings *tsBindings,
	fallbackMod string,
) (node graph.Node, viaBase string, ok bool) {
	if className == "" || methodName == "" {
		return graph.Node{}, "", false
	}
	type item struct {
		pkg string
		cls string
		via string
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

		for _, base := range lookupTSBases(rc, cur.pkg, cur.cls, localClassesByName) {
			basePkg, baseName := resolveTSBaseRef(rc, cur.pkg, base, localClassesByName, bindings, fallbackMod)
			if baseName == "" {
				continue
			}
			via := baseName
			if cur.via != "" {
				via = cur.via + " -> " + baseName
			}
			queue = append(queue, item{pkg: basePkg, cls: baseName, via: via})
		}
	}
	return graph.Node{}, "", false
}

func lookupTSBases(rc *resolver.ResolutionContext, pkgScope, clsName string, localClassesByName map[string]parser.ParsedClass) []string {
	if localClassesByName != nil {
		if cls, ok := localClassesByName[clsName]; ok {
			return cls.Bases
		}
	}
	if rc != nil && rc.View != nil && pkgScope != "" {
		for _, n := range rc.View.GetByQName(pkgScope + "::" + clsName) {
			if (n.Kind == graph.KindClass || n.Kind == graph.KindInterface) && len(n.Bases) > 0 {
				return n.Bases
			}
		}
	}
	return nil
}

func resolveTSBaseRef(
	rc *resolver.ResolutionContext,
	curPkg, rawBase string,
	localClassesByName map[string]parser.ParsedClass,
	bindings *tsBindings,
	fallbackMod string,
) (pkg, name string) {
	clean := resolver.StripTypeDecorations(strings.TrimSpace(rawBase))
	if clean == "" {
		return "", ""
	}
	if idx := strings.LastIndexByte(clean, '.'); idx >= 0 {
		ns := clean[:idx]
		sym := clean[idx+1:]
		if bindings != nil {
			if nb, ok := bindings.Namespaces[ns]; ok && !nb.IsExternal {
				return nb.TargetPackage, sym
			}
		}
		return curPkg, sym
	}
	if localClassesByName != nil {
		if _, ok := localClassesByName[clean]; ok && fallbackMod != "" {
			return fallbackMod, clean
		}
	}
	if rc != nil && rc.View != nil && curPkg != "" && len(rc.View.GetByQName(curPkg+"::"+clean)) > 0 {
		return curPkg, clean
	}
	if bindings != nil {
		for _, sb := range bindings.Symbols[clean] {
			if !sb.IsExternal {
				if n, hitMod, ok := resolveTSSymbolOrReexport(rc, sb.TargetPackage, sb.TargetFile, sb.OrigName, make(map[string]struct{}), 0); ok {
					return hitMod, n.Name
				}
				return sb.TargetPackage, sb.OrigName
			}
		}
	}
	return curPkg, clean
}

func resolveTSImportTarget(rc *resolver.ResolutionContext, cfg TSConfig, fileDir, spec string) (targetFile, targetScope string, isNodeModules, found bool) {
	trimmed := strings.TrimSpace(spec)
	if trimmed == "" {
		return "", "", false, false
	}

	// 1. Relative ESM / CommonJS import.
	if isTSRelativeSpecifier(trimmed) {
		base := path.Clean(path.Join(fileDir, trimmed))
		if f, s, ok := probeTSPath(rc, base); ok {
			return f, s, false, true
		}
		return "", "", false, false
	}

	// 2. tsconfig.json `paths` aliases (e.g. "@/*" -> ["src/*"]).
	if len(cfg.Paths) > 0 {
		for _, cand := range expandTSConfigPaths(cfg, trimmed) {
			if f, s, ok := probeTSPath(rc, cand); ok {
				return f, s, false, true
			}
		}
	}

	// 3. tsconfig.json `baseUrl` non-relative lookup (`services/auth` -> `src/services/auth`).
	if cfg.BaseURL != "" {
		baseCand := path.Clean(path.Join(cfg.BaseURL, trimmed))
		if f, s, ok := probeTSPath(rc, baseCand); ok {
			return f, s, false, true
		}
	}

	// 4. Node.js `node_modules` resolution (scoped `@scope/pkg` or unscoped `pkg`).
	if f, s, ok := resolveNodeModulesImport(rc, fileDir, trimmed); ok {
		return f, s, true, true
	}

	return "", "", false, false
}

func expandTSConfigPaths(cfg TSConfig, spec string) []string {
	var out []string
	// Exact matches first, then wildcard matches preferring longest prefix.
	bestPrefixLen := -1
	var wildcardMatches []string

	for pattern, targets := range cfg.Paths {
		star := strings.IndexByte(pattern, '*')
		if star < 0 {
			if pattern == spec {
				for _, t := range targets {
					out = append(out, applyTSBaseURL(cfg.BaseURL, t))
				}
			}
			continue
		}
		prefix := pattern[:star]
		suffix := pattern[star+1:]
		if strings.HasPrefix(spec, prefix) && strings.HasSuffix(spec, suffix) && len(spec) >= len(prefix)+len(suffix) {
			matched := spec[len(prefix) : len(spec)-len(suffix)]
			if len(prefix) >= bestPrefixLen {
				var cands []string
				for _, t := range targets {
					replaced := strings.Replace(t, "*", matched, 1)
					cands = append(cands, applyTSBaseURL(cfg.BaseURL, replaced))
				}
				if len(prefix) > bestPrefixLen {
					bestPrefixLen = len(prefix)
					wildcardMatches = cands
				} else {
					wildcardMatches = append(wildcardMatches, cands...)
				}
			}
		}
	}

	out = append(out, wildcardMatches...)
	return out
}

func applyTSBaseURL(baseURL, target string) string {
	cleaned := strings.TrimPrefix(path.Clean(filepath.ToSlash(target)), "./")
	if baseURL == "" || strings.HasPrefix(cleaned, baseURL+"/") || cleaned == baseURL {
		return cleaned
	}
	return path.Clean(path.Join(baseURL, cleaned))
}

func probeTSPath(rc *resolver.ResolutionContext, cand string) (targetFile, targetScope string, found bool) {
	cleaned := strings.TrimPrefix(path.Clean(cand), "./")
	if cleaned == "." || cleaned == "" {
		return "", "", false
	}

	stem := stripJSSpecifierExt(cleaned)
	var probes []string

	// Exact extension if caller supplied one (unless ESM .js -> .ts remapping).
	if ext := path.Ext(cleaned); ext != "" {
		probes = append(probes, cleaned)
	}
	tsExts := []string{".ts", ".tsx", ".mts", ".cts", ".d.ts", ".js", ".jsx", ".mjs", ".cjs"}
	for _, ext := range tsExts {
		probes = append(probes, stem+ext)
	}
	for _, ext := range tsExts {
		probes = append(probes, path.Join(stem, "index"+ext))
	}

	// Check package.json inside directory (`main`, `module`, `exports["."]`).
	if rc != nil && rc.RepoRoot != "" {
		pkgJSONPath := filepath.Join(rc.RepoRoot, filepath.FromSlash(cleaned), "package.json")
		if pkgInfo, err := ParsePackageJSON(pkgJSONPath); err == nil {
			if entry := pickPackageEntry(pkgInfo, "."); entry != "" {
				entryRel := path.Clean(path.Join(cleaned, entry))
				probes = append([]string{entryRel}, probes...)
			}
		}
	}

	seen := make(map[string]struct{}, len(probes))
	for _, p := range probes {
		norm := strings.TrimPrefix(path.Clean(p), "./")
		if _, dup := seen[norm]; dup {
			continue
		}
		seen[norm] = struct{}{}
		if tsFileExists(rc, norm) {
			return norm, stripTSExt(norm), true
		}
	}

	return "", "", false
}

func resolveNodeModulesImport(rc *resolver.ResolutionContext, startDir, spec string) (targetFile, targetScope string, found bool) {
	pkgName, subPath := splitNodePackageSpecifier(spec)
	if pkgName == "" {
		return "", "", false
	}

	// Walk ancestor directories looking for node_modules/<pkgName>.
	dir := startDir
	for {
		nmBase := "node_modules/" + pkgName
		if dir != "" && dir != "." {
			nmBase = dir + "/node_modules/" + pkgName
		}

		subKey := "."
		if subPath != "" {
			subKey = "./" + subPath
		}

		if rc != nil && rc.RepoRoot != "" {
			pkgJSONPath := filepath.Join(rc.RepoRoot, filepath.FromSlash(nmBase), "package.json")
			if pkgInfo, err := ParsePackageJSON(pkgJSONPath); err == nil {
				if entry := pickPackageEntry(pkgInfo, subKey); entry != "" {
					if f, s, ok := probeTSPath(rc, path.Join(nmBase, entry)); ok {
						return f, s, true
					}
				}
			}
		}

		directTarget := nmBase
		if subPath != "" {
			directTarget = nmBase + "/" + subPath
		}
		if f, s, ok := probeTSPath(rc, directTarget); ok {
			return f, s, true
		}

		if dir == "" || dir == "." {
			break
		}
		parent := path.Dir(dir)
		if parent == dir || parent == "." {
			dir = ""
		} else {
			dir = parent
		}
	}

	return "", "", false
}

func pickPackageEntry(pkg PackageJSON, subKey string) string {
	if t, ok := pkg.Exports[subKey]; ok && t != "" {
		return t
	}
	if subKey == "." {
		if pkg.Module != "" {
			return pkg.Module
		}
		if pkg.Main != "" {
			return pkg.Main
		}
		if pkg.Types != "" {
			return pkg.Types
		}
	}
	return ""
}

func splitNodePackageSpecifier(spec string) (pkgName, subPath string) {
	parts := strings.Split(spec, "/")
	if len(parts) == 0 {
		return "", ""
	}
	if strings.HasPrefix(parts[0], "@") {
		if len(parts) < 2 {
			return spec, ""
		}
		pkgName = parts[0] + "/" + parts[1]
		if len(parts) > 2 {
			subPath = strings.Join(parts[2:], "/")
		}
		return pkgName, subPath
	}
	pkgName = parts[0]
	if len(parts) > 1 {
		subPath = strings.Join(parts[1:], "/")
	}
	return pkgName, subPath
}

func tsFileExists(rc *resolver.ResolutionContext, slashPath string) bool {
	if rc == nil || slashPath == "" {
		return false
	}
	if rc.LookupFile(slashPath) != nil {
		return true
	}
	if rc.View != nil && len(rc.View.GetByFile(slashPath)) > 0 {
		return true
	}
	if rc.NameIndex != nil {
		scope := stripTSExt(slashPath)
		if files := rc.NameIndex.FilesForPackage(scope); len(files) > 0 {
			for _, f := range files {
				if normalizeTSPath(f) == slashPath {
					return true
				}
			}
		}
	}
	if rc.RepoRoot != "" {
		if st, err := os.Stat(filepath.Join(rc.RepoRoot, filepath.FromSlash(slashPath))); err == nil && !st.IsDir() {
			return true
		}
	}
	return false
}

func tsModuleScope(file *parser.ParsedFile) string {
	if file == nil {
		return ""
	}
	if file.Package != "" {
		return file.Package
	}
	return stripTSExt(normalizeTSPath(file.Path))
}

func normalizeTSPath(p string) string {
	s := strings.ReplaceAll(strings.TrimSpace(p), "\\", "/")
	s = strings.TrimPrefix(s, "./")
	return path.Clean(s)
}

func isTSRelativeSpecifier(spec string) bool {
	return strings.HasPrefix(spec, "./") || strings.HasPrefix(spec, "../") || spec == "." || spec == ".."
}

func stripTSExt(slashPath string) string {
	if strings.HasSuffix(slashPath, ".d.ts") {
		return strings.TrimSuffix(slashPath, ".d.ts")
	}
	return strings.TrimSuffix(slashPath, path.Ext(slashPath))
}

func stripJSSpecifierExt(slashPath string) string {
	for _, ext := range []string{".d.ts", ".ts", ".tsx", ".mts", ".cts", ".js", ".jsx", ".mjs", ".cjs"} {
		if strings.HasSuffix(slashPath, ext) {
			return strings.TrimSuffix(slashPath, ext)
		}
	}
	return slashPath
}

func stripTSPromiseWrapper(t string) string {
	trimmed := strings.TrimSpace(t)
	if strings.HasPrefix(trimmed, "Promise<") && strings.HasSuffix(trimmed, ">") {
		return resolver.StripTypeDecorations(trimmed[len("Promise<") : len(trimmed)-1])
	}
	return trimmed
}

func promoteTSFieldIfThis(tm *resolver.TypeMap, lv parser.ParsedLocalVar, info resolver.TypeInfo) {
	if !strings.HasPrefix(lv.Name, "this.") {
		return
	}
	clsScope := PackageScopePrefix(lv.ScopeQName)
	if lastSep := strings.LastIndex(lv.ScopeQName, "::"); lastSep > 0 {
		clsScope = lv.ScopeQName[:lastSep]
	}
	if clsScope == "" {
		return
	}
	attr := strings.TrimPrefix(lv.Name, "this.")
	tm.Set(clsScope, lv.Name, info)
	tm.Set(clsScope, attr, info)
	tm.SetField(clsScope, attr, info)
	tm.Set(clsScope, "this."+attr, info)
}

func makeTSResolvedCall(callerUID graph.UID, call parser.ParsedCall, target graph.Node, tier graph.ResolutionTier, methodDesc, pkgScope string) *resolver.ResolvedCall {
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

func stripJSONCommentsAndTrailingCommas(src []byte) []byte {
	var noComments []byte
	inStr := false
	escaped := false
	for i := 0; i < len(src); i++ {
		c := src[i]
		if inStr {
			noComments = append(noComments, c)
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		if c == '"' {
			inStr = true
			noComments = append(noComments, c)
			continue
		}
		if c == '/' && i+1 < len(src) {
			if src[i+1] == '/' {
				i += 2
				for i < len(src) && src[i] != '\n' {
					i++
				}
				if i < len(src) {
					noComments = append(noComments, '\n')
				}
				continue
			}
			if src[i+1] == '*' {
				i += 2
				closed := false
				for i+1 < len(src) {
					if src[i] == '*' && src[i+1] == '/' {
						i++
						closed = true
						break
					}
					i++
				}
				if !closed {
					break
				}
				continue
			}
		}
		noComments = append(noComments, c)
	}

	// Remove trailing commas before } or ] outside strings.
	var out []byte
	inStr = false
	escaped = false
	for i := 0; i < len(noComments); i++ {
		c := noComments[i]
		if inStr {
			out = append(out, c)
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		if c == '"' {
			inStr = true
			out = append(out, c)
			continue
		}
		if c == ',' {
			j := i + 1
			for j < len(noComments) && (noComments[j] == ' ' || noComments[j] == '\t' || noComments[j] == '\n' || noComments[j] == '\r') {
				j++
			}
			if j < len(noComments) && (noComments[j] == '}' || noComments[j] == ']') {
				continue
			}
		}
		out = append(out, c)
	}
	return out
}

func isJSBuiltin(name string) bool {
	switch name {
	case "require", "fetch", "setTimeout", "clearTimeout", "setInterval", "clearInterval",
		"parseInt", "parseFloat", "isNaN", "isFinite", "encodeURI", "decodeURI",
		"encodeURIComponent", "decodeURIComponent", "Promise", "Error", "TypeError",
		"RangeError", "Map", "Set", "WeakMap", "WeakSet", "Array", "Object",
		"String", "Number", "Boolean", "Symbol", "BigInt", "Date", "RegExp",
		"structuredClone", "atob", "btoa":
		return true
	default:
		return false
	}
}

func isJSBuiltinReceiver(recv string) bool {
	switch topSegment(recv) {
	case "console", "Math", "JSON", "Object", "Array", "Promise", "Reflect", "Intl", "process", "Buffer", "window", "document":
		return true
	default:
		return false
	}
}

// ResolveLinks resolves TypeScript/JavaScript inheritance (extends -> INHERITS),
// interface implementation (implements -> IMPLEMENTS), and decorator (DECORATED_BY) edges.
func (r *TypeScriptResolver) ResolveLinks(ctx context.Context, file *parser.ParsedFile, rc *resolver.ResolutionContext, imports []resolver.ResolvedImport) ([]resolver.ResolvedLink, error) {
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

	modScope := tsModuleScope(file)
	bindings := buildTSBindings(file, imports, rc)
	localClassesByName := make(map[string]parser.ParsedClass, len(file.Classes))
	for _, cls := range file.Classes {
		localClassesByName[cls.Name] = cls
	}

	var links []resolver.ResolvedLink
	for _, cls := range file.Classes {
		srcUID := ResolveClassOrSymbolUID(rc, cls.EffectiveKind(), cls.QName)
		for _, base := range cls.Bases {
			basePkg, baseName := resolveTSBaseRef(rc, modScope, base, localClassesByName, bindings, modScope)
			if basePkg == "" || baseName == "" {
				continue
			}
			if targetNode, ok := lookupTSClassOrInterfaceNode(rc, basePkg+"::"+baseName); ok {
				tier := resolver.TierQualifiedImport
				if basePkg == modScope {
					tier = resolver.TierLocal
				}
				links = append(links, MakeResolvedLink(srcUID, cls.QName, targetNode, graph.EdgeInherits, tier, cls.StartLine, "extends: "+base))
			}
		}
		for _, iface := range cls.Implements {
			if targetNode, hitPkg, ok := resolveTSSymbolType(rc, bindings, modScope, resolver.StripTypeDecorations(iface)); ok {
				tier := resolver.TierQualifiedImport
				if hitPkg == modScope {
					tier = resolver.TierLocal
				}
				links = append(links, MakeResolvedLink(srcUID, cls.QName, targetNode, graph.EdgeImplements, tier, cls.StartLine, "implements: "+iface))
			}
		}
		for _, dec := range cls.Decorators {
			if targetNode, tier, ok := resolveTSDecoratorTarget(rc, modScope, bindings, dec); ok {
				links = append(links, MakeResolvedLink(srcUID, cls.QName, targetNode, graph.EdgeDecoratedBy, tier, cls.StartLine, "decorator: "+dec))
			}
		}
	}

	for _, fn := range file.Functions {
		if len(fn.Decorators) == 0 {
			continue
		}
		srcUID := ResolveCallerUID(rc, fn.QName, fn.StartLine)
		for _, dec := range fn.Decorators {
			if targetNode, tier, ok := resolveTSDecoratorTarget(rc, modScope, bindings, dec); ok {
				links = append(links, MakeResolvedLink(srcUID, fn.QName, targetNode, graph.EdgeDecoratedBy, tier, fn.StartLine, "decorator: "+dec))
			}
		}
	}

	return DedupeResolvedLinks(links), nil
}

func lookupTSClassOrInterfaceNode(rc *resolver.ResolutionContext, qname string) (graph.Node, bool) {
	if rc == nil || rc.View == nil || qname == "" {
		return graph.Node{}, false
	}
	for _, n := range rc.View.GetByQName(qname) {
		if n.Kind == graph.KindClass || n.Kind == graph.KindInterface {
			return n, true
		}
	}
	return graph.Node{}, false
}

func resolveTSDecoratorTarget(rc *resolver.ResolutionContext, modScope string, bindings *tsBindings, rawDec string) (graph.Node, graph.ResolutionTier, bool) {
	expr := NormalizeDecoratorTarget(rawDec)
	if expr == "" || rc == nil || rc.View == nil {
		return graph.Node{}, 0, false
	}
	if !strings.Contains(expr, ".") {
		if n, _, ok := resolveTSDirectBindingTarget(rc, bindings, modScope, expr); ok {
			tier := resolver.TierQualifiedImport
			if PackageScopePrefix(n.QName) == modScope {
				tier = resolver.TierLocal
			}
			return n, tier, true
		}
		return graph.Node{}, 0, false
	}
	idx := strings.LastIndexByte(expr, '.')
	ns := expr[:idx]
	sym := expr[idx+1:]
	if nb, ok := bindings.Namespaces[ns]; ok && !nb.IsExternal {
		if n, _, found := resolveTSSymbolOrReexport(rc, nb.TargetPackage, nb.TargetFile, sym, make(map[string]struct{}), 0); found {
			return n, resolver.TierQualifiedImport, true
		}
	}
	return graph.Node{}, 0, false
}
