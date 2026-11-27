package lang

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/roshankumar0036singh/gographctx/internal/graph"
	"github.com/roshankumar0036singh/gographctx/internal/parser"
	"github.com/roshankumar0036singh/gographctx/internal/resolver"
)

// CargoInfo holds crate metadata parsed from Cargo.toml.
type CargoInfo struct {
	PackageName string // raw [package].name (e.g. "acme-orders")
	CrateIdent  string // Rust module identifier with '-' normalized to '_' ("acme_orders")
}

// ParseCargoTOML parses Cargo.toml inside repoRoot for the crate's [package].name.
func ParseCargoTOML(repoRoot string) (CargoInfo, error) {
	var info CargoInfo
	if strings.TrimSpace(repoRoot) == "" {
		return info, nil
	}
	f, err := os.Open(filepath.Join(repoRoot, "Cargo.toml"))
	if err != nil {
		if os.IsNotExist(err) {
			return info, nil
		}
		return info, err
	}
	defer f.Close()

	inPkg := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(stripTOMLComment(sc.Text()))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section := strings.TrimSpace(line[1 : len(line)-1])
			inPkg = section == "package"
			continue
		}
		if !inPkg {
			continue
		}
		if eq := strings.IndexByte(line, '='); eq > 0 {
			key := strings.TrimSpace(line[:eq])
			val := strings.TrimSpace(line[eq+1:])
			if key == "name" {
				val = strings.Trim(val, `"'`)
				info.PackageName = val
				info.CrateIdent = strings.ReplaceAll(val, "-", "_")
				break
			}
		}
	}
	return info, sc.Err()
}

func stripTOMLComment(s string) string {
	inQuotes := false
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inQuotes {
			if c == '\\' {
				i++
				continue
			}
			if c == quote {
				inQuotes = false
			}
			continue
		}
		if c == '"' || c == '\'' {
			inQuotes = true
			quote = c
			continue
		}
		if c == '#' {
			return s[:i]
		}
	}
	return s
}

// RustResolver implements LanguageResolver for Rust crates, resolving Cargo.toml
// crate paths, use/mod trees (`crate::`, `self::`, `super::`, `mod.rs`), pub use
// re-exports, inherent/trait impl methods, and associated functions.
type RustResolver struct{}

// NewRustResolver returns a Rust LanguageResolver.
func NewRustResolver() *RustResolver {
	return &RustResolver{}
}

// Language returns "rust".
func (r *RustResolver) Language() string {
	return "rust"
}

// ResolveImports resolves Rust `use` and `mod` declarations against Cargo.toml
// and indexed crate modules.
func (r *RustResolver) ResolveImports(ctx context.Context, file *parser.ParsedFile, rc *resolver.ResolutionContext) ([]resolver.ResolvedImport, error) {
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

	cargoInfo, _ := ParseCargoTOML(rc.RepoRoot)
	curMod := rustModuleScope(file)

	out := make([]resolver.ResolvedImport, 0, len(file.Imports))
	for _, imp := range file.Imports {
		ri := resolver.ResolvedImport{
			SourceFile:    file.Path,
			ImportPath:    imp.Path,
			Alias:         imp.Alias,
			ImportedNames: append([]string(nil), imp.Names...),
			Line:          imp.Line,
			IsWildcard:    imp.IsWildcard,
			IsRelative:    imp.IsRelative || isRustRelativeImport(imp.Path),
		}
		if ri.Alias == "" && len(imp.Names) == 1 && imp.Names[0] != "*" {
			ri.Alias = imp.Names[0]
		}

		// Handle `mod foo;` (`Path == "./foo"` emitted by rust parser).
		if strings.HasPrefix(imp.Path, "./") {
			modName := strings.TrimPrefix(imp.Path, "./")
			targetMod, targetFile, found := resolveRustModDecl(rc, curMod, modName)
			if found {
				ri.TargetPackage = targetMod
				ri.TargetFile = targetFile
				ri.IsExternal = false
			} else {
				ri.IsExternal = true
			}
			out = append(out, ri)
			continue
		}

		cands := resolveRustPathCandidates(curMod, imp.Path, cargoInfo)
		var matchedPkg, matchedFile string
		for _, cand := range cands {
			if f, found := findRustModuleFile(rc, cand); found {
				matchedPkg = cand
				matchedFile = f
				break
			}
		}
		if matchedPkg != "" {
			ri.TargetPackage = matchedPkg
			ri.TargetFile = matchedFile
			ri.IsExternal = false
		} else {
			ri.IsExternal = true
		}

		out = append(out, ri)
	}

	return out, nil
}

// InferTypes builds scoped Rust local, parameter, associated constructor, and
// factory return types with lifetime/reference normalization.
func (r *RustResolver) InferTypes(ctx context.Context, file *parser.ParsedFile, rc *resolver.ResolutionContext) (map[string]resolver.TypeInfo, error) {
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
	modScope := rustModuleScope(file)
	cargoInfo, _ := ParseCargoTOML(rc.RepoRoot)
	bindings := buildRustBindings(file, imports, rc, cargoInfo)

	// Normalize parameter types (strip lifetimes, generics, refs) and resolve imported types.
	for _, fn := range file.Functions {
		for _, p := range fn.Params {
			if p.Name == "" || p.Type == "" {
				continue
			}
			base := stripRustWrapperType(p.Type)
			if base == "" {
				continue
			}
			if clsNode, pkg, found := resolveRustSymbolType(rc, bindings, modScope, base, cargoInfo); found {
				tm.Set(fn.QName, p.Name, resolver.TypeInfo{
					Name:       clsNode.Name,
					QName:      clsNode.QName,
					Package:    pkg,
					Origin:     resolver.OriginParameter,
					Confidence: 0.96,
					Line:       fn.StartLine,
				})
			} else if prev, ok := tm.Lookup(fn.QName, p.Name); ok {
				prev.Name = base
				tm.Set(fn.QName, p.Name, prev)
			}
		}
	}

	// Enrich LocalVars initialized via associated calls (`Repo::new()`, `store::make()`) or imported functions.
	for _, lv := range file.LocalVars {
		if lv.ExplicitType != "" {
			if prev, ok := tm.Lookup(lv.ScopeQName, lv.Name); ok {
				base := stripRustWrapperType(prev.Name)
				if clsNode, pkg, found := resolveRustSymbolType(rc, bindings, modScope, base, cargoInfo); found {
					prev.Name = clsNode.Name
					prev.QName = clsNode.QName
					prev.Package = pkg
					tm.Set(lv.ScopeQName, lv.Name, prev)
				}
			}
			continue
		}
		if lv.InitCallee == "" {
			continue
		}

		var typeNode graph.Node
		var pkg string
		var found bool

		if lv.InitReceiver == "" {
			if fn, modPkg, ok := resolveRustUnqualifiedSymbol(rc, file, bindings, modScope, lv.InitCallee); ok {
				typeNode, pkg, found = inferRustReturnOrType(rc, fn, modPkg, bindings, modScope, cargoInfo)
			}
		} else {
			recvClean := stripRustWrapperType(lv.InitReceiver)
			if fn, modPkg, ok := resolveRustAssociatedTarget(rc, bindings, modScope, recvClean, lv.InitCallee, cargoInfo); ok {
				typeNode, pkg, found = inferRustReturnOrType(rc, fn, modPkg, bindings, modScope, cargoInfo)
			}
		}

		if found && typeNode.Name != "" {
			origin := resolver.OriginReturnType
			conf := float32(0.88)
			if lv.IsConstructor {
				origin = resolver.OriginConstructor
				conf = 0.93
			}
			tm.Set(lv.ScopeQName, lv.Name, resolver.TypeInfo{
				Name:       typeNode.Name,
				QName:      typeNode.QName,
				Package:    pkg,
				Origin:     origin,
				Confidence: conf,
				Line:       lv.Line,
			})
		}
	}

	return tm.ToFlatMap(), nil
}

// ResolveCalls resolves Rust free functions, path/module calls, associated functions,
// trait/inherent impl methods, self methods, and macro calls.
func (r *RustResolver) ResolveCalls(
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

	modScope := rustModuleScope(file)
	cargoInfo, _ := ParseCargoTOML(rc.RepoRoot)
	bindings := buildRustBindings(file, imports, rc, cargoInfo)

	localFnByQName := make(map[string]parser.ParsedFunction, len(file.Functions))
	for _, fn := range file.Functions {
		localFnByQName[fn.QName] = fn
	}
	localClassByName := make(map[string]parser.ParsedClass, len(file.Classes))
	localClassByQName := make(map[string]parser.ParsedClass, len(file.Classes))
	for _, cls := range file.Classes {
		localClassByName[cls.Name] = cls
		localClassByQName[cls.QName] = cls
	}

	var resolved []resolver.ResolvedCall
	var unresolved []resolver.UnresolvedCall

	for _, call := range file.Calls {
		callerUID := ResolveCallerUID(rc, call.CallerQName, call.Line)
		res, unres := r.resolveOneCall(file, rc, callerUID, call, modScope, cargoInfo, bindings, localFnByQName, localClassByName, localClassByQName, types)
		if res != nil {
			resolved = append(resolved, *res)
		}
		if unres != nil {
			unresolved = append(unresolved, *unres)
		}
	}

	return resolved, unresolved, nil
}

func (r *RustResolver) resolveOneCall(
	file *parser.ParsedFile,
	rc *resolver.ResolutionContext,
	callerUID graph.UID,
	call parser.ParsedCall,
	modScope string,
	cargoInfo CargoInfo,
	bindings *rustBindings,
	localFnByQName map[string]parser.ParsedFunction,
	localClassByName map[string]parser.ParsedClass,
	localClassByQName map[string]parser.ParsedClass,
	types map[string]resolver.TypeInfo,
) (*resolver.ResolvedCall, *resolver.UnresolvedCall) {
	callerFn, inFn := localFnByQName[call.CallerQName]
	enclosingType := ""
	enclosingPkg := modScope
	if inFn && callerFn.ReceiverType != "" {
		enclosingType = callerFn.ReceiverType
		if last := strings.LastIndex(call.CallerQName, "::"); last > 0 {
			parent := call.CallerQName[:last]
			if idx := strings.LastIndex(parent, "::"); idx > 0 {
				enclosingPkg = parent[:idx]
			}
		}
	}

	// 1. Direct / unqualified function or macro call.
	if !call.IsMethodCall || call.ReceiverName == "" {
		if strings.HasSuffix(call.CalleeName, "!") {
			return nil, &resolver.UnresolvedCall{
				CallerUID:   callerUID,
				CallerQName: call.CallerQName,
				File:        file.Path,
				Line:        call.Line,
				Column:      uint16(call.Col),
				CalleeExpr:  call.CalleeName,
				LookupKey:   resolver.BuildLookupKey("", "macros", call.CalleeName),
				Status:      graph.CallStatusExternal,
				Tier:        resolver.TierUnresolved,
				Reason:      "Rust macro invocation: " + call.CalleeName,
			}
		}

		// Nested / same-file function.
		if target, ok := FindSameFileFuncUID(rc, file, call.CalleeName, ""); ok {
			return makeRustResolvedCall(callerUID, call, target, resolver.TierLocal, "local function: "+target.QName, modScope), nil
		}

		// Imported symbol (`use crate::foo::bar`, wildcards, or pub use re-exports).
		if target, hitMod, extOnly, found := resolveRustDirectCall(rc, bindings, call.CalleeName); found {
			return makeRustResolvedCall(callerUID, call, target, resolver.TierQualifiedImport, "use import: "+target.QName, hitMod), nil
		} else if extOnly {
			return nil, &resolver.UnresolvedCall{
				CallerUID:   callerUID,
				CallerQName: call.CallerQName,
				File:        file.Path,
				Line:        call.Line,
				Column:      uint16(call.Col),
				CalleeExpr:  call.CalleeName,
				LookupKey:   resolver.BuildLookupKey("", hitMod, call.CalleeName),
				Status:      graph.CallStatusExternal,
				Tier:        resolver.TierUnresolved,
				Reason:      "external crate/std function",
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
			Reason:      "unresolved Rust function call",
		}
	}

	recv := strings.TrimSpace(call.ReceiverName)

	// 2. Method / associated path call.
	// Tier 1: `self.method()` or `Self::assoc()`.
	if enclosingType != "" && (recv == "self" || recv == "Self") {
		if target, viaTrait, ok := resolveRustTypeMethod(rc, enclosingPkg, enclosingType, call.CalleeName); ok {
			desc := recv + "::" + call.CalleeName
			if viaTrait != "" {
				desc = recv + " via trait " + viaTrait + ": " + call.CalleeName
			}
			return makeRustResolvedCall(callerUID, call, target, resolver.TierSelf, desc, enclosingPkg), nil
		}
	}

	// Tier 3 / 4: Scoped path (`store::Client::new`, `Client::new`, `crate::util::helper`, `std::mem::drop`).
	if isRustExternalPath(recv, bindings) {
		return nil, &resolver.UnresolvedCall{
			CallerUID:   callerUID,
			CallerQName: call.CallerQName,
			File:        file.Path,
			Line:        call.Line,
			Column:      uint16(call.Col),
			CalleeExpr:  recv + "::" + call.CalleeName,
			LookupKey:   resolver.BuildLookupKey("", recv, call.CalleeName),
			Status:      graph.CallStatusExternal,
			Tier:        resolver.TierUnresolved,
			Reason:      "external Rust crate/std call",
		}
	}
	if strings.Contains(recv, "::") || isUpperStart(recv) || hasRustModuleAlias(bindings, recv) {
		if target, hitPkg, ok := resolveRustAssociatedTarget(rc, bindings, modScope, recv, call.CalleeName, cargoInfo); ok {
			tier := resolver.TierQualifiedImport
			if hitPkg == modScope && !hasRustModuleAlias(bindings, recv) {
				if _, localType := localClassByName[recv]; localType {
					tier = resolver.TierLocal
				}
			}
			return makeRustResolvedCall(callerUID, call, target, tier, "associated/module path: "+target.QName, hitPkg), nil
		}
	}

	// Tier 4: Value method call via inferred receiver type (`svc.fetch()`, `self.repo.load()`).
	if recvType, ok := LookupScopedReceiverType(rc, call.CallerQName, enclosingPkg+"::"+enclosingType, recv, types, localClassByQName); ok {
		typeName := stripRustWrapperType(recvType.Name)
		targetPkg := recvType.Package
		if targetPkg == "" && recvType.QName != "" {
			targetPkg = PackageScopePrefix(recvType.QName)
		}
		if targetPkg == "" {
			if _, boundPkg, found := resolveRustSymbolType(rc, bindings, modScope, typeName, cargoInfo); found {
				targetPkg = boundPkg
			} else {
				targetPkg = modScope
			}
		}

		if target, viaTrait, found := resolveRustTypeMethod(rc, targetPkg, typeName, call.CalleeName); found {
			desc := "receiver type inferred: " + typeName + "::" + call.CalleeName
			if viaTrait != "" {
				desc = "trait method " + viaTrait + " on " + typeName + ": " + call.CalleeName
			}
			return makeRustResolvedCall(callerUID, call, target, resolver.TierReceiverType, desc, targetPkg), nil
		}

		// Graph-wide unique fallback constrained by Receiver == typeName.
		if rc.View != nil && typeName != "" {
			if target, unique, cands := FilterUniqueFunctionMatches(rc.View.GetByName(call.CalleeName), false, func(n graph.Node) bool {
				return stripRustWrapperType(n.Receiver) == typeName
			}); unique {
				return makeRustResolvedCall(callerUID, call, target, resolver.TierReceiverType, "receiver type fallback: "+typeName+"::"+call.CalleeName, targetPkg), nil
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
					Reason:      "multiple candidate Rust trait/inherent methods on " + typeName,
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
		Reason:      "unresolved Rust method or path call",
	}
}

type rustSymbolBinding struct {
	TargetModule string
	OrigName     string
	IsExternal   bool
	RawPath      string
}

type rustBindings struct {
	Symbols   map[string][]rustSymbolBinding
	Modules   map[string]rustSymbolBinding
	Wildcards []string
	ExtPaths  map[string]struct{}
}

func buildRustBindings(file *parser.ParsedFile, resolved []resolver.ResolvedImport, rc *resolver.ResolutionContext, cargoInfo CargoInfo) *rustBindings {
	b := &rustBindings{
		Symbols:  make(map[string][]rustSymbolBinding),
		Modules:  make(map[string]rustSymbolBinding),
		ExtPaths: make(map[string]struct{}),
	}
	curMod := rustModuleScope(file)

	for i, imp := range file.Imports {
		var targetMod string
		var isExt bool
		if i < len(resolved) {
			targetMod = resolved[i].TargetPackage
			isExt = resolved[i].IsExternal
		} else {
			cands := resolveRustPathCandidates(curMod, imp.Path, cargoInfo)
			isExt = true
			for _, c := range cands {
				if _, ok := findRustModuleFile(rc, c); ok {
					targetMod = c
					isExt = false
					break
				}
			}
		}

		if isExt {
			b.ExtPaths[imp.Path] = struct{}{}
		}

		// Whole-module import (`use crate::store;`, `use crate::store as s;`, or `mod store;`).
		if len(imp.Names) == 0 {
			localAlias := imp.Alias
			if localAlias == "" {
				if strings.HasPrefix(imp.Path, "./") {
					localAlias = strings.TrimPrefix(imp.Path, "./")
				} else {
					segs := strings.Split(imp.Path, "::")
					localAlias = segs[len(segs)-1]
				}
			}
			if localAlias != "" && localAlias != "self" && localAlias != "super" && localAlias != "crate" {
				b.Modules[localAlias] = rustSymbolBinding{
					TargetModule: targetMod,
					IsExternal:   isExt,
					RawPath:      imp.Path,
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
			if orig == "self" {
				// `use crate::store::{self};` binds `store`.
				localAlias := imp.Alias
				if localAlias == "" {
					segs := strings.Split(imp.Path, "::")
					localAlias = segs[len(segs)-1]
				}
				b.Modules[localAlias] = rustSymbolBinding{
					TargetModule: targetMod,
					IsExternal:   isExt,
					RawPath:      imp.Path,
				}
				continue
			}
			localName := orig
			if imp.Alias != "" && len(imp.Names) == 1 {
				localName = imp.Alias
			}
			sb := rustSymbolBinding{
				TargetModule: targetMod,
				OrigName:     orig,
				IsExternal:   isExt,
				RawPath:      imp.Path + "::" + orig,
			}
			b.Symbols[localName] = append(b.Symbols[localName], sb)

			// A `use crate::store;` form emits Path="crate", Names=["store"] when parsed from scoped_identifier!
			// Check whether `targetMod::orig` (or resolved child module) is itself a module!
			for _, subCand := range resolveRustPathCandidates(curMod, imp.Path+"::"+orig, cargoInfo) {
				if _, ok := findRustModuleFile(rc, subCand); ok {
					b.Modules[localName] = rustSymbolBinding{
						TargetModule: subCand,
						IsExternal:   false,
						RawPath:      imp.Path + "::" + orig,
					}
					break
				}
			}
		}
	}

	return b
}

func resolveRustDirectCall(rc *resolver.ResolutionContext, b *rustBindings, callee string) (target graph.Node, hitMod string, extOnly bool, found bool) {
	if b == nil {
		return graph.Node{}, "", false, false
	}
	var anyExt bool
	for _, sb := range b.Symbols[callee] {
		if sb.IsExternal {
			anyExt = true
			if hitMod == "" {
				hitMod = sb.RawPath
			}
			continue
		}
		if n, mod, ok := resolveRustSymbolOrReexport(rc, sb.TargetModule, sb.OrigName, make(map[string]struct{}), 0); ok {
			return n, mod, false, true
		}
	}
	for _, wmod := range b.Wildcards {
		if n, mod, ok := resolveRustSymbolOrReexport(rc, wmod, callee, make(map[string]struct{}), 0); ok {
			return n, mod, false, true
		}
	}
	return graph.Node{}, hitMod, anyExt, false
}

func resolveRustSymbolOrReexport(rc *resolver.ResolutionContext, modScope, symName string, visited map[string]struct{}, depth int) (graph.Node, string, bool) {
	if rc == nil || modScope == "" || symName == "" || depth > 5 {
		return graph.Node{}, "", false
	}
	vKey := modScope + "::" + symName
	if _, seen := visited[vKey]; seen {
		return graph.Node{}, "", false
	}
	visited[vKey] = struct{}{}

	if rc.View != nil {
		qname := modScope + "::" + symName
		if fn, ok, _ := FilterUniqueFunctionMatches(rc.View.GetByQName(qname), false, nil); ok {
			return fn, modScope, true
		}
		for _, n := range rc.View.GetByQName(qname) {
			if n.Kind == graph.KindStruct || n.Kind == graph.KindClass || n.Kind == graph.KindInterface || n.Kind == graph.KindTypeAlias {
				return n, modScope, true
			}
		}
	}

	// Follow `pub use` re-exports inside modScope's ParsedFile.
	cargoInfo, _ := ParseCargoTOML(rc.RepoRoot)
	for _, fpath := range rustModuleFiles(rc, modScope) {
		pf := rc.LookupFile(fpath)
		if pf == nil {
			continue
		}
		pfMod := rustModuleScope(pf)
		for _, imp := range pf.Imports {
			if !imp.Exported {
				continue
			}
			if imp.IsWildcard {
				for _, cand := range resolveRustPathCandidates(pfMod, imp.Path, cargoInfo) {
					if _, ok := findRustModuleFile(rc, cand); ok {
						if n, hitMod, found := resolveRustSymbolOrReexport(rc, cand, symName, visited, depth+1); found {
							return n, hitMod, true
						}
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
					for _, cand := range resolveRustPathCandidates(pfMod, imp.Path, cargoInfo) {
						if _, ok := findRustModuleFile(rc, cand); ok {
							if n, hitMod, found := resolveRustSymbolOrReexport(rc, cand, orig, visited, depth+1); found {
								return n, hitMod, true
							}
						}
					}
				}
			}
		}
	}

	return graph.Node{}, "", false
}

func resolveRustAssociatedTarget(
	rc *resolver.ResolutionContext,
	b *rustBindings,
	modScope, recvPath, callee string,
	cargoInfo CargoInfo,
) (graph.Node, string, bool) {
	if rc == nil || rc.View == nil {
		return graph.Node{}, "", false
	}

	// Case A: Type::assoc_fn() where Type is defined locally or imported via `use`.
	if !strings.Contains(recvPath, "::") {
		if n, _, ok := resolveRustTypeMethod(rc, modScope, recvPath, callee); ok {
			return n, modScope, true
		}
		if b != nil {
			for _, sb := range b.Symbols[recvPath] {
				if sb.IsExternal {
					continue
				}
				if typeNode, hitMod, ok := resolveRustSymbolOrReexport(rc, sb.TargetModule, sb.OrigName, make(map[string]struct{}), 0); ok {
					if m, _, okM := resolveRustTypeMethod(rc, hitMod, typeNode.Name, callee); okM {
						return m, hitMod, true
					}
				}
			}
			if mb, ok := b.Modules[recvPath]; ok && !mb.IsExternal {
				if n, hitMod, okS := resolveRustSymbolOrReexport(rc, mb.TargetModule, callee, make(map[string]struct{}), 0); okS {
					return n, hitMod, true
				}
			}
		}
		return graph.Node{}, "", false
	}

	// Case B: Multi-segment path `crate::store::MemoryRepo::new` or `store::helper`.
	// B1. Direct module path -> free function.
	for _, cand := range resolveRustPathCandidates(modScope, recvPath, cargoInfo) {
		if n, hitMod, ok := resolveRustSymbolOrReexport(rc, cand, callee, make(map[string]struct{}), 0); ok {
			return n, hitMod, true
		}
	}
	// B2. Module alias prefix: `store::MemoryRepo::new`.
	segs := strings.Split(recvPath, "::")
	if b != nil && len(segs) == 2 {
		if mb, ok := b.Modules[segs[0]]; ok && !mb.IsExternal {
			if m, _, okM := resolveRustTypeMethod(rc, mb.TargetModule, segs[1], callee); okM {
				return m, mb.TargetModule, true
			}
		}
	}
	// B3. Full path terminating in Type: `crate::store::MemoryRepo` + `new`.
	if len(segs) >= 2 {
		typeName := segs[len(segs)-1]
		modRaw := strings.Join(segs[:len(segs)-1], "::")
		for _, modCand := range resolveRustPathCandidates(modScope, modRaw, cargoInfo) {
			if m, _, okM := resolveRustTypeMethod(rc, modCand, typeName, callee); okM {
				return m, modCand, true
			}
		}
	}

	return graph.Node{}, "", false
}

func resolveRustTypeMethod(rc *resolver.ResolutionContext, modScope, typeName, methodName string) (node graph.Node, viaTrait string, ok bool) {
	if rc == nil || rc.View == nil || typeName == "" || methodName == "" || modScope == "" {
		return graph.Node{}, "", false
	}
	qname := modScope + "::" + typeName + "::" + methodName
	if fn, found, _ := FilterUniqueFunctionMatches(rc.View.GetByQName(qname), false, nil); found {
		for _, dec := range fn.Decorators {
			if strings.HasPrefix(dec, "rust:impl_trait:") {
				viaTrait = strings.TrimPrefix(dec, "rust:impl_trait:")
				break
			}
		}
		return fn, viaTrait, true
	}

	// Also check if type implements trait(s) with default trait methods in this or parent module.
	for _, tn := range rc.View.GetByQName(modScope + "::" + typeName) {
		if pf := rc.LookupFile(tn.File); pf != nil {
			for _, pc := range pf.Classes {
				if pc.QName != tn.QName {
					continue
				}
				for _, tr := range pc.Implements {
					if fn, found, _ := FilterUniqueFunctionMatches(rc.View.GetByQName(modScope+"::"+tr+"::"+methodName), false, nil); found {
						return fn, tr, true
					}
				}
			}
		}
	}

	return graph.Node{}, "", false
}

func resolveRustUnqualifiedSymbol(rc *resolver.ResolutionContext, file *parser.ParsedFile, b *rustBindings, modScope, name string) (graph.Node, string, bool) {
	if fn, ok := FindSameFileFuncUID(rc, file, name, ""); ok {
		return fn, modScope, true
	}
	if fn, hitMod, _, found := resolveRustDirectCall(rc, b, name); found {
		return fn, hitMod, true
	}
	return graph.Node{}, "", false
}

func resolveRustSymbolType(rc *resolver.ResolutionContext, b *rustBindings, modScope, name string, cargoInfo CargoInfo) (graph.Node, string, bool) {
	clean := stripRustWrapperType(name)
	if clean == "" {
		return graph.Node{}, "", false
	}
	if strings.Contains(clean, "::") {
		segs := strings.Split(clean, "::")
		typeName := segs[len(segs)-1]
		modRaw := strings.Join(segs[:len(segs)-1], "::")
		for _, cand := range resolveRustPathCandidates(modScope, modRaw, cargoInfo) {
			if n, hitMod, ok := resolveRustSymbolOrReexport(rc, cand, typeName, make(map[string]struct{}), 0); ok {
				return n, hitMod, true
			}
		}
	}
	if rc != nil && rc.View != nil && modScope != "" {
		for _, n := range rc.View.GetByQName(modScope + "::" + clean) {
			if n.Kind == graph.KindStruct || n.Kind == graph.KindClass || n.Kind == graph.KindInterface || n.Kind == graph.KindTypeAlias {
				return n, modScope, true
			}
		}
	}
	if b != nil {
		for _, sb := range b.Symbols[clean] {
			if sb.IsExternal {
				continue
			}
			if n, hitMod, ok := resolveRustSymbolOrReexport(rc, sb.TargetModule, sb.OrigName, make(map[string]struct{}), 0); ok {
				return n, hitMod, true
			}
		}
	}
	return graph.Node{}, "", false
}

func inferRustReturnOrType(rc *resolver.ResolutionContext, fn graph.Node, modPkg string, bindings *rustBindings, modScope string, cargoInfo CargoInfo) (graph.Node, string, bool) {
	if fn.UID.IsZero() {
		return graph.Node{}, "", false
	}
	pkg := pkgOrDefault(PackageScopePrefix(fn.QName), modPkg)
	ret := stripRustWrapperType(fn.ReturnType)
	if ret == "" || ret == "Self" {
		// Extract owning struct from `<pkg>::<Struct>::new`.
		withoutFn := strings.TrimSuffix(fn.QName, "::"+fn.Name)
		if withoutFn != "" && withoutFn != fn.QName && rc != nil && rc.View != nil {
			for _, n := range rc.View.GetByQName(withoutFn) {
				if n.Kind == graph.KindStruct || n.Kind == graph.KindClass {
					return n, pkg, true
				}
			}
		}
		if fn.Receiver != "" {
			rName := stripRustWrapperType(fn.Receiver)
			return graph.Node{Name: rName, QName: pkg + "::" + rName, Kind: graph.KindStruct}, pkg, true
		}
		return graph.Node{}, "", false
	}
	if clsNode, hitPkg, ok := resolveRustSymbolType(rc, bindings, pkg, ret, cargoInfo); ok {
		return clsNode, hitPkg, true
	}
	if clsNode, hitPkg, ok := resolveRustSymbolType(rc, bindings, modScope, ret, cargoInfo); ok {
		return clsNode, hitPkg, true
	}
	return graph.Node{Name: ret, QName: pkg + "::" + ret, Kind: graph.KindStruct}, pkg, true
}

func resolveRustModDecl(rc *resolver.ResolutionContext, curMod, modName string) (string, string, bool) {
	var cands []string
	if curMod == "src::lib" || curMod == "src::main" || curMod == "crate" {
		cands = append(cands, "src::"+modName)
	}
	if curMod != "" {
		cands = append(cands, curMod+"::"+modName)
	}
	for _, c := range cands {
		if f, ok := findRustModuleFile(rc, c); ok {
			return c, f, true
		}
	}
	return "", "", false
}

func resolveRustPathCandidates(curMod, rawPath string, cargoInfo CargoInfo) []string {
	trimmed := strings.TrimSpace(rawPath)
	if trimmed == "" {
		return nil
	}
	crateRoot := rustCrateRoot(curMod)
	var out []string
	add := func(s string) {
		if s == "" {
			return
		}
		for _, existing := range out {
			if existing == s {
				return
			}
		}
		out = append(out, s)
	}

	switch {
	case trimmed == "crate":
		add(crateRoot + "::lib")
		add(crateRoot + "::main")
		add(crateRoot)
	case strings.HasPrefix(trimmed, "crate::"):
		rest := strings.TrimPrefix(trimmed, "crate::")
		add(crateRoot + "::" + rest)
		add(crateRoot + "::lib::" + rest)
		add(crateRoot + "::main::" + rest)
	case cargoInfo.CrateIdent != "" && trimmed == cargoInfo.CrateIdent:
		add(crateRoot + "::lib")
		add(crateRoot + "::main")
	case cargoInfo.CrateIdent != "" && strings.HasPrefix(trimmed, cargoInfo.CrateIdent+"::"):
		rest := strings.TrimPrefix(trimmed, cargoInfo.CrateIdent+"::")
		add(crateRoot + "::" + rest)
	case trimmed == "self":
		add(curMod)
	case strings.HasPrefix(trimmed, "self::"):
		rest := strings.TrimPrefix(trimmed, "self::")
		base := curMod
		if base == "src::lib" || base == "src::main" {
			add("src::" + rest)
		}
		if base != "" {
			add(base + "::" + rest)
		}
	case trimmed == "super" || strings.HasPrefix(trimmed, "super::"):
		segs := strings.Split(curMod, "::")
		if len(segs) > 0 && (segs[len(segs)-1] == "lib" || segs[len(segs)-1] == "main") {
			segs = segs[:len(segs)-1]
		}
		rest := strings.TrimPrefix(trimmed, "super")
		rest = strings.TrimPrefix(rest, "::")
		climbs := 1
		for strings.HasPrefix(rest, "super::") || rest == "super" {
			climbs++
			rest = strings.TrimPrefix(rest, "super")
			rest = strings.TrimPrefix(rest, "::")
		}
		if climbs < len(segs) {
			parent := strings.Join(segs[:len(segs)-climbs], "::")
			if rest != "" {
				add(parent + "::" + rest)
			} else {
				add(parent)
			}
		}
	default:
		add(trimmed)
		if curMod != "" {
			add(curMod + "::" + trimmed)
		}
		if crateRoot != "" {
			add(crateRoot + "::" + trimmed)
		}
	}

	return out
}

func rustCrateRoot(curMod string) string {
	if strings.HasPrefix(curMod, "src::") || curMod == "src" {
		return "src"
	}
	return "crate"
}

func rustModuleScope(file *parser.ParsedFile) string {
	if file == nil {
		return ""
	}
	if file.Package != "" {
		return file.Package
	}
	return ""
}

func findRustModuleFile(rc *resolver.ResolutionContext, modScope string) (string, bool) {
	files := rustModuleFiles(rc, modScope)
	if len(files) == 0 {
		return "", false
	}
	return files[0], true
}

func rustModuleFiles(rc *resolver.ResolutionContext, modScope string) []string {
	if rc == nil || modScope == "" {
		return nil
	}
	if rc.NameIndex != nil {
		if files := rc.NameIndex.FilesForPackage(modScope); len(files) > 0 {
			return files
		}
	}
	if len(rc.Files) > 0 {
		var out []string
		for _, pf := range rc.Files {
			if pf != nil && pf.Package == modScope {
				out = append(out, pf.Path)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

func isRustRelativeImport(p string) bool {
	return p == "crate" || strings.HasPrefix(p, "crate::") ||
		p == "self" || strings.HasPrefix(p, "self::") ||
		p == "super" || strings.HasPrefix(p, "super::") ||
		strings.HasPrefix(p, "./")
}

func isRustExternalPath(recv string, b *rustBindings) bool {
	top := strings.SplitN(recv, "::", 2)[0]
	switch top {
	case "std", "core", "alloc", "proc_macro":
		return true
	}
	if b != nil {
		if mb, ok := b.Modules[top]; ok && mb.IsExternal {
			return true
		}
	}
	return false
}

func hasRustModuleAlias(b *rustBindings, recv string) bool {
	if b == nil {
		return false
	}
	top := strings.SplitN(recv, "::", 2)[0]
	_, ok := b.Modules[top]
	return ok
}

func stripRustWrapperType(raw string) string {
	s := strings.TrimSpace(raw)
	for {
		prev := s
		// Strip references & lifetimes: `&'a mut Foo` / `&mut Foo` / `&Foo`
		for strings.HasPrefix(s, "&") {
			s = strings.TrimSpace(s[1:])
			if strings.HasPrefix(s, "'") {
				if sp := strings.IndexByte(s, ' '); sp > 0 {
					s = strings.TrimSpace(s[sp+1:])
				}
			}
			if strings.HasPrefix(s, "mut ") {
				s = strings.TrimSpace(strings.TrimPrefix(s, "mut "))
			}
		}
		// Unwrap smart-pointer / Result / Option wrappers (`Box<T>`, `Arc<T>`, `Result<T, E>`, `Option<T>`).
		for _, wrap := range []string{"Box<", "Arc<", "Rc<", "RefCell<", "Mutex<", "Option<", "Result<"} {
			if strings.HasPrefix(s, wrap) && strings.HasSuffix(s, ">") {
				inner := s[len(wrap) : len(s)-1]
				if wrap == "Result<" {
					if comma := topLevelCommaIndex(inner); comma > 0 {
						inner = inner[:comma]
					}
				}
				s = strings.TrimSpace(inner)
			}
		}
		s = strings.TrimSpace(strings.TrimPrefix(s, "dyn "))
		s = strings.TrimSpace(strings.TrimPrefix(s, "impl "))
		if s == prev {
			break
		}
	}
	if idx := strings.IndexByte(s, '<'); idx > 0 {
		s = s[:idx]
	}
	return strings.TrimSpace(s)
}

func topLevelCommaIndex(s string) int {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '<', '(', '[', '{':
			depth++
		case '>', ')', ']', '}':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func makeRustResolvedCall(callerUID graph.UID, call parser.ParsedCall, target graph.Node, tier graph.ResolutionTier, methodDesc, pkgScope string) *resolver.ResolvedCall {
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

// ResolveLinks resolves Rust trait super-bounds (INHERITS), `impl Trait for Struct`
// relationships (IMPLEMENTS), and attribute/decorator references (DECORATED_BY).
func (r *RustResolver) ResolveLinks(ctx context.Context, file *parser.ParsedFile, rc *resolver.ResolutionContext, imports []resolver.ResolvedImport) ([]resolver.ResolvedLink, error) {
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

	modScope := rustModuleScope(file)
	cargoInfo, _ := ParseCargoTOML(rc.RepoRoot)
	bindings := buildRustBindings(file, imports, rc, cargoInfo)

	var links []resolver.ResolvedLink
	for _, cls := range file.Classes {
		srcUID := ResolveClassOrSymbolUID(rc, cls.EffectiveKind(), cls.QName)
		for _, base := range cls.Bases {
			if targetNode, hitPkg, ok := resolveRustSymbolType(rc, bindings, modScope, base, cargoInfo); ok {
				tier := resolver.TierQualifiedImport
				if hitPkg == modScope {
					tier = resolver.TierLocal
				}
				links = append(links, MakeResolvedLink(srcUID, cls.QName, targetNode, graph.EdgeInherits, tier, cls.StartLine, "trait bound: "+base))
			}
		}
		for _, trait := range cls.Implements {
			if targetNode, hitPkg, ok := resolveRustSymbolType(rc, bindings, modScope, trait, cargoInfo); ok {
				tier := resolver.TierQualifiedImport
				if hitPkg == modScope {
					tier = resolver.TierLocal
				}
				links = append(links, MakeResolvedLink(srcUID, cls.QName, targetNode, graph.EdgeImplements, tier, cls.StartLine, "impl trait: "+trait))
			}
		}
		for _, dec := range cls.Decorators {
			if sym := NormalizeDecoratorTarget(dec); sym != "" {
				if targetNode, hitPkg, ok := resolveRustUnqualifiedSymbol(rc, file, bindings, modScope, sym); ok {
					tier := resolver.TierQualifiedImport
					if hitPkg == modScope {
						tier = resolver.TierLocal
					}
					links = append(links, MakeResolvedLink(srcUID, cls.QName, targetNode, graph.EdgeDecoratedBy, tier, cls.StartLine, "attribute: "+dec))
				}
			}
		}
	}

	// Capture `impl Trait for RemoteStruct` defined in a file separate from RemoteStruct's definition.
	for _, fn := range file.Functions {
		if fn.ReceiverType == "" {
			continue
		}
		for _, dec := range fn.Decorators {
			if !strings.HasPrefix(dec, "rust:impl_trait:") {
				continue
			}
			traitName := strings.TrimPrefix(dec, "rust:impl_trait:")
			if structNode, _, okStruct := resolveRustSymbolType(rc, bindings, modScope, fn.ReceiverType, cargoInfo); okStruct {
				if traitNode, hitPkg, okTrait := resolveRustSymbolType(rc, bindings, modScope, traitName, cargoInfo); okTrait {
					tier := resolver.TierQualifiedImport
					if hitPkg == modScope {
						tier = resolver.TierLocal
					}
					links = append(links, MakeResolvedLink(structNode.UID, structNode.QName, traitNode, graph.EdgeImplements, tier, fn.StartLine, "impl "+traitName+" for "+structNode.Name))
				}
			}
		}
	}

	return DedupeResolvedLinks(links), nil
}
