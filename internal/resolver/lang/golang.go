package lang

import (
	"bufio"
	"cmp"
	"context"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/roshankumar0036singh/gographctx/internal/graph"
	"github.com/roshankumar0036singh/gographctx/internal/parser"
	"github.com/roshankumar0036singh/gographctx/internal/resolver"
)

// GoModInfo holds module identity and local replace directives parsed from go.mod.
type GoModInfo struct {
	ModulePath string
	GoVersion  string
	Replaces   map[string]string // module prefix -> relative local path
}

// ParseGoMod reads and parses go.mod inside repoRoot (returning zero GoModInfo
// if no go.mod exists).
func ParseGoMod(repoRoot string) (GoModInfo, error) {
	var info GoModInfo
	if strings.TrimSpace(repoRoot) == "" {
		return info, nil
	}
	modPath := filepath.Join(repoRoot, "go.mod")
	f, err := os.Open(modPath)
	if err != nil {
		if os.IsNotExist(err) {
			return info, nil
		}
		return info, err
	}
	defer f.Close()

	info.Replaces = make(map[string]string)
	inReplaceBlock := false

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(stripGoLineComment(sc.Text()))
		if line == "" {
			continue
		}
		if inReplaceBlock {
			if line == ")" {
				inReplaceBlock = false
				continue
			}
			parseGoModReplaceLine(line, repoRoot, info.Replaces)
			continue
		}
		switch {
		case strings.HasPrefix(line, "module "):
			info.ModulePath = strings.TrimSpace(strings.TrimPrefix(line, "module "))
		case strings.HasPrefix(line, "go "):
			info.GoVersion = strings.TrimSpace(strings.TrimPrefix(line, "go "))
		case line == "replace (" || strings.HasPrefix(line, "replace("):
			inReplaceBlock = true
		case strings.HasPrefix(line, "replace "):
			parseGoModReplaceLine(strings.TrimPrefix(line, "replace "), repoRoot, info.Replaces)
		}
	}
	return info, sc.Err()
}

func stripGoLineComment(s string) string {
	if idx := strings.Index(s, "//"); idx >= 0 {
		return s[:idx]
	}
	return s
}

func parseGoModReplaceLine(line, repoRoot string, out map[string]string) {
	parts := strings.SplitN(line, "=>", 2)
	if len(parts) != 2 {
		return
	}
	leftFields := strings.Fields(parts[0])
	rightFields := strings.Fields(parts[1])
	if len(leftFields) == 0 || len(rightFields) == 0 {
		return
	}
	oldMod := leftFields[0]
	rawTarget := rightFields[0]
	target := filepath.ToSlash(filepath.Clean(rawTarget))
	if strings.HasPrefix(rawTarget, "./") || strings.HasPrefix(rawTarget, "../") || strings.HasPrefix(target, ".") {
		out[oldMod] = strings.TrimPrefix(target, "./")
		return
	}
	if repoRoot != "" && !filepath.IsAbs(rawTarget) {
		if st, err := os.Stat(filepath.Join(repoRoot, filepath.FromSlash(target))); err == nil && st.IsDir() {
			out[oldMod] = strings.TrimPrefix(target, "./")
		}
	}
}

// InterfaceSatisfaction summarizes one struct's structural satisfaction of a Go interface.
type InterfaceSatisfaction struct {
	StructUID      graph.UID
	StructQName    string
	InterfaceUID   graph.UID
	InterfaceQName string
	MatchedMethods []string
}

type cachedGoMod struct {
	info      GoModInfo
	exists    bool
	modTime   time.Time
	size      int64
	checkedAt time.Time
}

// GoResolver implements LanguageResolver for Go packages using go.mod module
// resolution, receiver type inference, embedded struct lookup, and Tier 1-8 scoring.
type GoResolver struct {
	modMu    sync.RWMutex
	modCache map[string]cachedGoMod
}

// NewGoResolver returns a Go LanguageResolver.
func NewGoResolver() *GoResolver {
	return &GoResolver{
		modCache: make(map[string]cachedGoMod),
	}
}

func (r *GoResolver) parseGoModCached(repoRoot string) (GoModInfo, error) {
	if r == nil || strings.TrimSpace(repoRoot) == "" {
		return ParseGoMod(repoRoot)
	}
	cleanRoot := filepath.Clean(repoRoot)
	now := time.Now()
	r.modMu.RLock()
	cached, ok := r.modCache[cleanRoot]
	r.modMu.RUnlock()
	if ok && now.Sub(cached.checkedAt) < 500*time.Millisecond {
		return cached.info, nil
	}

	modFile := filepath.Join(cleanRoot, "go.mod")
	st, statErr := os.Stat(modFile)
	exists := statErr == nil
	var modTime time.Time
	var size int64
	if exists {
		modTime = st.ModTime()
		size = st.Size()
	} else if !os.IsNotExist(statErr) {
		return ParseGoMod(cleanRoot)
	}

	if ok && cached.exists == exists && (!exists || (cached.modTime.Equal(modTime) && cached.size == size)) {
		r.modMu.Lock()
		cached.checkedAt = now
		r.modCache[cleanRoot] = cached
		r.modMu.Unlock()
		return cached.info, nil
	}

	info, err := ParseGoMod(cleanRoot)
	if err != nil {
		return info, err
	}
	r.modMu.Lock()
	if r.modCache == nil {
		r.modCache = make(map[string]cachedGoMod)
	}
	r.modCache[cleanRoot] = cachedGoMod{
		info:      info,
		exists:    exists,
		modTime:   modTime,
		size:      size,
		checkedAt: now,
	}
	r.modMu.Unlock()
	return info, nil
}

// Language returns "go".
func (r *GoResolver) Language() string {
	return "go"
}

// ResolveImports resolves Go import statements against go.mod and indexed project files.
func (r *GoResolver) ResolveImports(ctx context.Context, file *parser.ParsedFile, rc *resolver.ResolutionContext) ([]resolver.ResolvedImport, error) {
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

	modInfo, err := r.parseGoModCached(rc.RepoRoot)
	if err != nil {
		return nil, err
	}

	out := make([]resolver.ResolvedImport, 0, len(file.Imports))
	fileDir := path.Dir(strings.ReplaceAll(strings.TrimSpace(file.Path), "\\", "/"))

	for _, imp := range file.Imports {
		ri := resolver.ResolvedImport{
			SourceFile: file.Path,
			ImportPath: imp.Path,
			Alias:      imp.Alias,
			Line:       imp.Line,
			IsWildcard: imp.IsWildcard,
			IsRelative: imp.IsRelative,
		}
		if ri.Alias == "" {
			ri.Alias = path.Base(imp.Path)
		}

		// 1. Check local replace mappings, relative paths, and module-local prefix.
		var candidatePkg string
		switch {
		case imp.IsRelative:
			candidatePkg = path.Clean(path.Join(fileDir, imp.Path))
		case modInfo.ModulePath != "" && imp.Path == modInfo.ModulePath:
			candidatePkg = "."
		case modInfo.ModulePath != "" && strings.HasPrefix(imp.Path, modInfo.ModulePath+"/"):
			candidatePkg = strings.TrimPrefix(imp.Path, modInfo.ModulePath+"/")
		default:
			for repMod, localDir := range modInfo.Replaces {
				if imp.Path == repMod {
					candidatePkg = localDir
					break
				}
				if strings.HasPrefix(imp.Path, repMod+"/") {
					candidatePkg = path.Join(localDir, strings.TrimPrefix(imp.Path, repMod+"/"))
					break
				}
			}
		}

		if candidatePkg != "" {
			ri.TargetPackage = candidatePkg
			if rc.NameIndex != nil {
				if files := rc.NameIndex.FilesForPackage(candidatePkg); len(files) > 0 {
					ri.TargetFile = files[0]
				} else if files := rc.NameIndex.FilesForPackage(path.Base(candidatePkg)); len(files) > 0 {
					for _, f := range files {
						normF := strings.ReplaceAll(strings.TrimSpace(f), "\\", "/")
						if path.Dir(normF) == candidatePkg {
							ri.TargetFile = f
							break
						}
					}
					if ri.TargetFile == "" {
						ri.TargetPackage = path.Base(candidatePkg)
						ri.TargetFile = files[0]
					}
				}
			}
			ri.IsExternal = false
		} else if rc.NameIndex != nil && hasPackageDirInIndex(rc, imp.Path) {
			ri.TargetPackage = imp.Path
			if files := rc.NameIndex.FilesForPackage(imp.Path); len(files) > 0 {
				ri.TargetFile = files[0]
			}
			ri.IsExternal = false
		} else {
			ri.IsExternal = true
		}

		out = append(out, ri)
	}
	return out, nil
}

func hasPackageDirInIndex(rc *resolver.ResolutionContext, pkg string) bool {
	if rc == nil || rc.NameIndex == nil {
		return false
	}
	return len(rc.NameIndex.FilesForPackage(pkg)) > 0
}

// InferTypes infers scoped Go variable, parameter, receiver, multi-return, and
// constructor types in file.
func (r *GoResolver) InferTypes(ctx context.Context, file *parser.ParsedFile, rc *resolver.ResolutionContext) (map[string]resolver.TypeInfo, error) {
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
	if err == nil {
		impByAlias := indexGoImportsByAlias(imports)
		for _, lv := range file.LocalVars {
			if lv.ExplicitType != "" || lv.InitReceiver == "" || lv.InitCallee == "" {
				continue
			}
			imp, ok := impByAlias[lv.InitReceiver]
			if !ok || imp.IsExternal || imp.TargetPackage == "" {
				continue
			}
			if prev, already := tm.Lookup(lv.ScopeQName, lv.Name); already {
				if prev.Package == "" || prev.Package == file.Package || prev.Package == imp.Alias {
					prev.Package = imp.TargetPackage
					if prev.Name != "" && prev.QName == "" {
						prev.QName = imp.TargetPackage + "::" + resolver.StripTypeDecorations(prev.Name)
					}
					tm.Set(lv.ScopeQName, lv.Name, prev)
				}
				continue
			}
			if lv.ResultIndex == 0 && isGoNewConstructor(lv.InitCallee) {
				typeName := lv.InitCallee[3:]
				tm.Set(lv.ScopeQName, lv.Name, resolver.TypeInfo{
					Name:       typeName,
					Package:    imp.TargetPackage,
					QName:      imp.TargetPackage + "::" + typeName,
					Origin:     resolver.OriginConstructor,
					Confidence: 0.88,
					Line:       lv.Line,
				})
			}
		}
	}

	return tm.ToFlatMap(), nil
}

// ResolveCalls resolves Go function calls, selector/package calls, method calls,
// embedded struct methods, and same-package calls into ResolvedCall / UnresolvedCall.
func (r *GoResolver) ResolveCalls(
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

	impByAlias := indexGoImportsByAlias(imports)
	var dotImports []resolver.ResolvedImport
	for _, imp := range imports {
		if imp.IsWildcard && !imp.IsExternal && imp.TargetPackage != "" {
			dotImports = append(dotImports, imp)
		}
	}

	fileScope := file.Package
	if len(file.Functions) > 0 {
		if p := PackageScopePrefix(file.Functions[0].QName); p != "" {
			fileScope = p
		}
	} else if len(file.Classes) > 0 {
		if p := PackageScopePrefix(file.Classes[0].QName); p != "" {
			fileScope = p
		}
	}

	fnByQName := make(map[string]parser.ParsedFunction, len(file.Functions))
	for _, fn := range file.Functions {
		fnByQName[fn.QName] = fn
	}
	localClassByName := make(map[string]parser.ParsedClass, len(file.Classes))
	for _, cls := range file.Classes {
		localClassByName[cls.Name] = cls
	}

	var resolved []resolver.ResolvedCall
	var unresolved []resolver.UnresolvedCall

	for _, call := range file.Calls {
		callerUID := ResolveCallerUIDForFile(rc, file, call.CallerQName, call.Line)

		// Tier 1 / Tier 2 / Tier 3 / Tier 4 / Tier 6 cascade.
		if res, unres, handled := r.resolveOneCall(file, rc, callerUID, call, fileScope, fnByQName, localClassByName, impByAlias, dotImports, types); handled {
			if res != nil {
				resolved = append(resolved, *res)
			}
			if unres != nil {
				unresolved = append(unresolved, *unres)
			}
		}
	}

	return resolved, unresolved, nil
}

func (r *GoResolver) resolveOneCall(
	file *parser.ParsedFile,
	rc *resolver.ResolutionContext,
	callerUID graph.UID,
	call parser.ParsedCall,
	fileScope string,
	fnByQName map[string]parser.ParsedFunction,
	localClassByName map[string]parser.ParsedClass,
	impByAlias map[string]resolver.ResolvedImport,
	dotImports []resolver.ResolvedImport,
	types map[string]resolver.TypeInfo,
) (*resolver.ResolvedCall, *resolver.UnresolvedCall, bool) {
	callerFn, inFunc := fnByQName[call.CallerQName]

	// 1. Direct / non-method function call: foo(...)
	if !call.IsMethodCall || call.ReceiverName == "" {
		// Tier 2: same-file function definition
		if targetNode, ok := FindSameFileFuncUID(rc, file, call.CalleeName, ""); ok {
			return &resolver.ResolvedCall{
				CallerUID:   callerUID,
				CallerQName: call.CallerQName,
				TargetUID:   targetNode.UID,
				TargetQName: targetNode.QName,
				Tier:        resolver.TierLocal,
				EdgeKind:    graph.EdgeCalls,
				Line:        call.Line,
				Column:      uint16(call.Col),
				Method:      "local definition: " + call.CalleeName,
				LookupKey:   resolver.BuildLookupKey("", fileScope, call.CalleeName),
			}, nil, true
		}

		// Local function value binding (`fn := helper; fn()`).
		if targetNode, ok := ResolveCallableAlias(rc, call.CallerQName, call.CalleeName, types); ok {
			return &resolver.ResolvedCall{
				CallerUID:   callerUID,
				CallerQName: call.CallerQName,
				TargetUID:   targetNode.UID,
				TargetQName: targetNode.QName,
				Tier:        resolver.TierLocal,
				EdgeKind:    graph.EdgeCalls,
				Line:        call.Line,
				Column:      uint16(call.Col),
				Method:      "local callable alias: " + targetNode.QName,
				LookupKey:   resolver.BuildLookupKey("", fileScope, call.CalleeName),
			}, nil, true
		}

		// Tier 6: same-package function across sibling files
		if fileScope != "" && rc.View != nil {
			qname := fileScope + "::" + call.CalleeName
			if targetNode, ok, cands := FilterUniqueFunctionMatches(rc.View.GetByQName(qname), true, nil); ok {
				return &resolver.ResolvedCall{
					CallerUID:   callerUID,
					CallerQName: call.CallerQName,
					TargetUID:   targetNode.UID,
					TargetQName: targetNode.QName,
					Tier:        resolver.TierSamePackage,
					EdgeKind:    graph.EdgeCalls,
					Line:        call.Line,
					Column:      uint16(call.Col),
					Method:      "same package: " + qname,
					LookupKey:   resolver.BuildLookupKey("", fileScope, call.CalleeName),
				}, nil, true
			} else if len(cands) > 1 {
				return nil, &resolver.UnresolvedCall{
					CallerUID:   callerUID,
					CallerQName: call.CallerQName,
					File:        file.Path,
					Line:        call.Line,
					Column:      uint16(call.Col),
					CalleeExpr:  call.CalleeName,
					LookupKey:   resolver.BuildLookupKey("", fileScope, call.CalleeName),
					Candidates:  cands,
					Status:      graph.CallStatusAmbiguous,
					Tier:        resolver.TierTypeNarrowed,
					Reason:      "multiple same-package matches",
				}, true
			}
		}

		// Dot-imported package calls
		for _, dotImp := range dotImports {
			if target, ok := resolveMethodOrFuncInPkg(rc, dotImp.TargetPackage, "", call.CalleeName); ok {
				return &resolver.ResolvedCall{
					CallerUID:   callerUID,
					CallerQName: call.CallerQName,
					TargetUID:   target.UID,
					TargetQName: target.QName,
					Tier:        resolver.TierQualifiedImport,
					EdgeKind:    graph.EdgeCalls,
					Line:        call.Line,
					Column:      uint16(call.Col),
					Method:      "dot import: " + target.QName,
					LookupKey:   resolver.BuildLookupKey("", dotImp.TargetPackage, call.CalleeName),
				}, nil, true
			}
		}

		// Unresolved / built-in (make, len, append, etc.)
		return nil, &resolver.UnresolvedCall{
			CallerUID:   callerUID,
			CallerQName: call.CallerQName,
			File:        file.Path,
			Line:        call.Line,
			Column:      uint16(call.Col),
			CalleeExpr:  call.CalleeName,
			LookupKey:   resolver.BuildLookupKey("", fileScope, call.CalleeName),
			Status:      graph.CallStatusUnresolved,
			Tier:        resolver.TierUnresolved,
			Reason:      "no matching local or package function",
		}, true
	}

	// 2. Method or package-qualified call: expr.Method(...)
	// Tier 1: Call on enclosing method's own receiver (s.validateCommand() or s.EmbeddedMethod())
	if inFunc && callerFn.ReceiverType != "" && isCallerReceiverExpr(call.ReceiverName, callerFn, types) {
		if target, viaEmbed, ok := resolveGoReceiverMethod(rc, file, fileScope, callerFn.ReceiverType, call.CalleeName, localClassByName, impByAlias); ok {
			methodDesc := "self receiver: " + callerFn.ReceiverType + "." + call.CalleeName
			if viaEmbed != "" {
				methodDesc = "self receiver via embedded " + viaEmbed + ": " + call.CalleeName
			}
			return &resolver.ResolvedCall{
				CallerUID:   callerUID,
				CallerQName: call.CallerQName,
				TargetUID:   target.UID,
				TargetQName: target.QName,
				Tier:        resolver.TierSelf,
				EdgeKind:    graph.EdgeCalls,
				Line:        call.Line,
				Column:      uint16(call.Col),
				Method:      methodDesc,
				LookupKey:   resolver.BuildLookupKey(callerFn.ReceiverType, fileScope, call.CalleeName),
			}, nil, true
		}
	}

	// Tier 3: Qualified import package call (e.g. store.NewClient(), fmt.Println())
	if imp, ok := impByAlias[call.ReceiverName]; ok {
		if imp.IsExternal {
			return nil, &resolver.UnresolvedCall{
				CallerUID:   callerUID,
				CallerQName: call.CallerQName,
				File:        file.Path,
				Line:        call.Line,
				Column:      uint16(call.Col),
				CalleeExpr:  call.ReceiverName + "." + call.CalleeName,
				LookupKey:   resolver.BuildLookupKey("", imp.ImportPath, call.CalleeName),
				Status:      graph.CallStatusExternal,
				Tier:        resolver.TierUnresolved,
				Reason:      "external Go package import: " + imp.ImportPath,
			}, true
		}
		if target, ok := resolveMethodOrFuncInPkg(rc, imp.TargetPackage, "", call.CalleeName); ok {
			return &resolver.ResolvedCall{
				CallerUID:   callerUID,
				CallerQName: call.CallerQName,
				TargetUID:   target.UID,
				TargetQName: target.QName,
				Tier:        resolver.TierQualifiedImport,
				EdgeKind:    graph.EdgeCalls,
				Line:        call.Line,
				Column:      uint16(call.Col),
				Method:      "qualified import: " + target.QName,
				LookupKey:   resolver.BuildLookupKey("", imp.TargetPackage, call.CalleeName),
			}, nil, true
		}
	}

	// Tier 4: Receiver type inferred (client.Save(), s.repo.Save(), ord.Clone())
	if recvType, ok := lookupGoReceiverType(call.CallerQName, call.ReceiverName, types, localClassByName, rc); ok {
		targetPkg, targetType := splitGoTypePackage(recvType, fileScope, file.Package, impByAlias)
		if targetPkg != "" && isExternalGoPkg(targetPkg, impByAlias) {
			return nil, &resolver.UnresolvedCall{
				CallerUID:   callerUID,
				CallerQName: call.CallerQName,
				File:        file.Path,
				Line:        call.Line,
				Column:      uint16(call.Col),
				CalleeExpr:  call.ReceiverName + "." + call.CalleeName,
				LookupKey:   resolver.BuildLookupKey(targetType, targetPkg, call.CalleeName),
				Status:      graph.CallStatusExternal,
				Tier:        resolver.TierUnresolved,
				Reason:      "method on external package type " + targetType,
			}, true
		}
		if target, viaEmbed, found := resolveGoReceiverMethod(rc, file, targetPkg, targetType, call.CalleeName, localClassByName, impByAlias); found {
			desc := "receiver type inferred: " + targetType + "." + call.CalleeName
			if viaEmbed != "" {
				desc = "receiver type via embedded " + viaEmbed + ": " + call.CalleeName
			}
			return &resolver.ResolvedCall{
				CallerUID:   callerUID,
				CallerQName: call.CallerQName,
				TargetUID:   target.UID,
				TargetQName: target.QName,
				Tier:        resolver.TierReceiverType,
				EdgeKind:    graph.EdgeCalls,
				Line:        call.Line,
				Column:      uint16(call.Col),
				Method:      desc,
				LookupKey:   resolver.BuildLookupKey(targetType, targetPkg, call.CalleeName),
			}, nil, true
		}
		// Fallback search across indexed graph methods matching Receiver == targetType.
		if rc.View != nil {
			if target, unique, cands := FilterUniqueFunctionMatches(rc.View.GetByName(call.CalleeName), false, func(n graph.Node) bool {
				return resolver.StripTypeDecorations(n.Receiver) == targetType
			}); unique {
				return &resolver.ResolvedCall{
					CallerUID:   callerUID,
					CallerQName: call.CallerQName,
					TargetUID:   target.UID,
					TargetQName: target.QName,
					Tier:        resolver.TierReceiverType,
					EdgeKind:    graph.EdgeCalls,
					Line:        call.Line,
					Column:      uint16(call.Col),
					Method:      "receiver type inferred: " + targetType + "." + call.CalleeName,
					LookupKey:   resolver.BuildLookupKey(targetType, targetPkg, call.CalleeName),
				}, nil, true
			} else if len(cands) > 1 {
				return nil, &resolver.UnresolvedCall{
					CallerUID:   callerUID,
					CallerQName: call.CallerQName,
					File:        file.Path,
					Line:        call.Line,
					Column:      uint16(call.Col),
					CalleeExpr:  call.ReceiverName + "." + call.CalleeName,
					LookupKey:   resolver.BuildLookupKey(targetType, targetPkg, call.CalleeName),
					Candidates:  cands,
					Status:      graph.CallStatusAmbiguous,
					Tier:        resolver.TierTypeNarrowed,
					Reason:      "multiple candidate methods on receiver type " + targetType,
				}, true
			}
		}
	}

	return nil, &resolver.UnresolvedCall{
		CallerUID:   callerUID,
		CallerQName: call.CallerQName,
		File:        file.Path,
		Line:        call.Line,
		Column:      uint16(call.Col),
		CalleeExpr:  call.ReceiverName + "." + call.CalleeName,
		LookupKey:   resolver.BuildLookupKey(call.ReceiverName, fileScope, call.CalleeName),
		Status:      graph.CallStatusUnresolved,
		Tier:        resolver.TierUnresolved,
		Reason:      "unresolved Go selector call",
	}, true
}

func isCallerReceiverExpr(expr string, callerFn parser.ParsedFunction, types map[string]resolver.TypeInfo) bool {
	if resolver.IsSelfOrSuperReceiver(expr) {
		return true
	}
	if strings.Contains(expr, ".") {
		return false
	}
	if info, ok := resolver.LookupFlat(types, callerFn.QName, expr); ok {
		return (info.Origin == resolver.OriginParameter && info.Confidence == 1.00) ||
			(info.Line == callerFn.StartLine && info.Name == callerFn.ReceiverType)
	}
	return false
}

func lookupGoReceiverType(
	callerQName, expr string,
	types map[string]resolver.TypeInfo,
	localClassByName map[string]parser.ParsedClass,
	rc *resolver.ResolutionContext,
) (resolver.TypeInfo, bool) {
	if info, ok := LookupScopedReceiverType(rc, callerQName, "", expr, types, nil); ok {
		return info, true
	}
	parts := splitReceiverChain(expr)
	if len(parts) < 2 {
		return resolver.TypeInfo{}, false
	}
	cur, ok := resolver.LookupFlat(types, callerQName, strings.TrimSpace(parts[0]))
	if !ok {
		return resolver.TypeInfo{}, false
	}
	for _, field := range parts[1:] {
		fieldName := strings.TrimSpace(field)
		if fieldName == "" {
			return resolver.TypeInfo{}, false
		}
		if cls, found := localClassByName[resolver.StripTypeDecorations(cur.Name)]; found {
			if next, matched := lookupFieldOnParsedClass(cls, fieldName, cur.Package); matched {
				cur = next
				continue
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

func splitGoTypePackage(info resolver.TypeInfo, defaultScope, currentFilePkg string, impByAlias map[string]resolver.ResolvedImport) (pkg, typeName string) {
	raw := resolver.StripTypeDecorations(info.Name)
	if idx := strings.LastIndexByte(raw, '.'); idx >= 0 {
		alias := raw[:idx]
		typeName = raw[idx+1:]
		if imp, ok := impByAlias[alias]; ok {
			if imp.IsExternal {
				return imp.ImportPath, typeName
			}
			return imp.TargetPackage, typeName
		}
		return alias, typeName
	}
	if info.Package != "" && info.Package != currentFilePkg {
		return info.Package, raw
	}
	return defaultScope, raw
}

func isExternalGoPkg(pkg string, impByAlias map[string]resolver.ResolvedImport) bool {
	for _, imp := range impByAlias {
		if imp.IsExternal && (imp.ImportPath == pkg || imp.Alias == pkg) {
			return true
		}
	}
	return false
}

func resolveGoReceiverMethod(
	rc *resolver.ResolutionContext,
	file *parser.ParsedFile,
	pkgScope string,
	typeName string,
	methodName string,
	localClassByName map[string]parser.ParsedClass,
	impByAlias map[string]resolver.ResolvedImport,
) (node graph.Node, viaEmbed string, ok bool) {
	// Breadth-first search over typeName and any embedded Bases.
	type queueItem struct {
		pkg  string
		typ  string
		via  string
		step int
	}
	visited := make(map[string]struct{})
	queue := []queueItem{{pkg: pkgScope, typ: typeName}}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		visitKey := cur.pkg + "::" + cur.typ
		if _, seen := visited[visitKey]; seen {
			continue
		}
		visited[visitKey] = struct{}{}

		// 1. Check same-file methods first when same package.
		if file != nil && (cur.pkg == "" || cur.pkg == pkgScope) {
			if n, found := FindSameFileFuncUID(rc, file, methodName, cur.typ); found {
				return n, cur.via, true
			}
		}

		// 2. Check graph View for <pkg>::<typ>::<methodName>.
		if rc != nil && rc.View != nil && cur.typ != "" {
			if cur.pkg != "" {
				qname := cur.pkg + "::" + cur.typ + "::" + methodName
				if n, found, _ := FilterUniqueFunctionMatches(rc.View.GetByQName(qname), false, nil); found {
					return n, cur.via, true
				}
			}
		}

		// 3. Enqueue embedded Bases from local classes or graph View.
		for _, base := range lookupStructBases(rc, cur.pkg, cur.typ, localClassByName) {
			basePkg, baseType := cur.pkg, base
			if idx := strings.LastIndexByte(base, '.'); idx >= 0 {
				alias := base[:idx]
				baseType = base[idx+1:]
				if imp, ok := impByAlias[alias]; ok && !imp.IsExternal && imp.TargetPackage != "" {
					basePkg = imp.TargetPackage
				} else {
					basePkg = alias
				}
			}
			via := baseType
			if cur.via != "" {
				via = cur.via + "." + baseType
			}
			queue = append(queue, queueItem{pkg: basePkg, typ: baseType, via: via, step: cur.step + 1})
		}
	}

	return graph.Node{}, "", false
}

func lookupStructBases(rc *resolver.ResolutionContext, pkgScope, typeName string, localClassByName map[string]parser.ParsedClass) []string {
	if cls, ok := localClassByName[typeName]; ok {
		return cls.Bases
	}
	if rc != nil && rc.View != nil && pkgScope != "" {
		nodes := rc.View.GetByQName(pkgScope + "::" + typeName)
		for _, n := range nodes {
			if (n.Kind == graph.KindStruct || n.Kind == graph.KindInterface) && len(n.Bases) > 0 {
				return n.Bases
			}
		}
	}
	return nil
}

func resolveMethodOrFuncInPkg(rc *resolver.ResolutionContext, pkgScope, typeName, fnName string) (graph.Node, bool) {
	if rc == nil || rc.View == nil || pkgScope == "" {
		return graph.Node{}, false
	}
	qname := pkgScope + "::" + fnName
	if typeName != "" {
		qname = pkgScope + "::" + typeName + "::" + fnName
	}
	n, ok, _ := FilterUniqueFunctionMatches(rc.View.GetByQName(qname), false, nil)
	return n, ok
}

func indexGoImportsByAlias(imports []resolver.ResolvedImport) map[string]resolver.ResolvedImport {
	out := make(map[string]resolver.ResolvedImport, len(imports))
	for _, imp := range imports {
		if imp.Alias != "" && imp.Alias != "." && imp.Alias != "_" {
			out[imp.Alias] = imp
		}
	}
	return out
}

func isGoNewConstructor(name string) bool {
	if !strings.HasPrefix(name, "New") || len(name) <= 3 {
		return false
	}
	r, _ := utf8.DecodeRuneInString(name[3:])
	return unicode.IsUpper(r)
}

// DetectInterfaceSatisfaction inspects all structs and interfaces in view and
// reports structural interface satisfaction pairs where a struct provides all
// required methods (directly or via embedded structs).
func DetectInterfaceSatisfaction(view *graph.ReadView) []InterfaceSatisfaction {
	if view == nil {
		return nil
	}
	return detectInterfaceSatisfactionForStructs(view, nil)
}

func detectInterfaceSatisfactionForStructs(view *graph.ReadView, targetStructs []graph.Node) []InterfaceSatisfaction {
	if view == nil {
		return nil
	}
	interfaces := view.GetByKind(graph.KindInterface)
	if len(interfaces) == 0 {
		return nil
	}

	structs := targetStructs
	if structs == nil {
		structs = view.GetByKind(graph.KindStruct)
	}
	if len(structs) == 0 {
		return nil
	}

	// Collect required methods only from files declaring interfaces (Go interface methods
	// live inside the interface type declaration in the same file).
	ifaceMethodsByQName := make(map[string]map[string]graph.Node, len(interfaces))
	fileNodesCache := make(map[string][]graph.Node)
	for _, iface := range interfaces {
		var nodes []graph.Node
		if iface.File != "" {
			var cached bool
			nodes, cached = fileNodesCache[iface.File]
			if !cached {
				nodes = view.GetByFile(iface.File)
				fileNodesCache[iface.File] = nodes
			}
		} else {
			nodes = view.GetContains(iface.UID)
		}
		for _, n := range nodes {
			if n.Kind == graph.KindFunction && n.Receiver != "" {
				key := PackageScopePrefix(n.QName) + "::" + resolver.StripTypeDecorations(n.Receiver)
				if key != iface.QName {
					continue
				}
				mset, ok := ifaceMethodsByQName[iface.QName]
				if !ok {
					mset = make(map[string]graph.Node)
					ifaceMethodsByQName[iface.QName] = mset
				}
				mset[n.Name] = n
			}
		}
	}

	lookupStructMethod := func(st graph.Node, methodName string) (graph.Node, bool) {
		visited := make(map[string]struct{}, 4)
		queue := []graph.Node{st}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			if _, seen := visited[cur.QName]; seen {
				continue
			}
			visited[cur.QName] = struct{}{}
			for _, cand := range view.GetByQName(cur.QName + "::" + methodName) {
				if cand.Kind == graph.KindFunction && cand.Receiver != "" {
					return cand, true
				}
			}
			pkg := PackageScopePrefix(cur.QName)
			for _, base := range cur.Bases {
				baseKey := pkg + "::" + resolver.StripTypeDecorations(base)
				if idx := strings.LastIndexByte(base, '.'); idx >= 0 {
					baseKey = base[:idx] + "::" + resolver.StripTypeDecorations(base[idx+1:])
				}
				for _, bNode := range view.GetByQName(baseKey) {
					if bNode.Kind == graph.KindStruct {
						queue = append(queue, bNode)
						break
					}
				}
			}
		}
		return graph.Node{}, false
	}

	var results []InterfaceSatisfaction
	for _, iface := range interfaces {
		reqMethods := ifaceMethodsByQName[iface.QName]
		if len(reqMethods) == 0 {
			continue
		}
		reqNames := make([]string, 0, len(reqMethods))
		for mName := range reqMethods {
			reqNames = append(reqNames, mName)
		}
		slices.Sort(reqNames)

		for _, st := range structs {
			var matched []string
			satisfied := true
			for _, mName := range reqNames {
				reqFn := reqMethods[mName]
				implFn, ok := lookupStructMethod(st, mName)
				if !ok || !goInterfaceMethodMatch(implFn, reqFn) {
					satisfied = false
					break
				}
				matched = append(matched, mName)
			}
			if satisfied && len(matched) > 0 {
				results = append(results, InterfaceSatisfaction{
					StructUID:      st.UID,
					StructQName:    st.QName,
					InterfaceUID:   iface.UID,
					InterfaceQName: iface.QName,
					MatchedMethods: matched,
				})
			}
		}
	}
	slices.SortFunc(results, func(a, b InterfaceSatisfaction) int {
		if c := cmp.Compare(a.StructQName, b.StructQName); c != 0 {
			return c
		}
		return cmp.Compare(a.InterfaceQName, b.InterfaceQName)
	})
	return results
}

func goInterfaceMethodMatch(implFn, reqFn graph.Node) bool {
	if len(implFn.Params) != len(reqFn.Params) {
		return false
	}
	return stripGoPkgQualifiers(implFn.ReturnType) == stripGoPkgQualifiers(reqFn.ReturnType)
}

func stripGoPkgQualifiers(sig string) string {
	sig = strings.TrimSpace(sig)
	if sig == "" {
		return ""
	}
	var out strings.Builder
	out.Grow(len(sig))
	i := 0
	for i < len(sig) {
		if isGoIdentStart(sig[i]) {
			start := i
			i++
			for i < len(sig) && isGoIdentChar(sig[i]) {
				i++
			}
			// Drop "<pkg>." when followed immediately by an identifier.
			if i < len(sig) && sig[i] == '.' && i+1 < len(sig) && isGoIdentStart(sig[i+1]) {
				i++
				continue
			}
			out.WriteString(sig[start:i])
			continue
		}
		if sig[i] != ' ' && sig[i] != '\t' {
			out.WriteByte(sig[i])
		}
		i++
	}
	return out.String()
}

func isGoIdentStart(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == '_'
}

func isGoIdentChar(b byte) bool {
	return isGoIdentStart(b) || (b >= '0' && b <= '9')
}

// ResolveLinks resolves Go embedded struct/interface INHERITS edges and
// structural interface satisfaction IMPLEMENTS edges.
func (r *GoResolver) ResolveLinks(ctx context.Context, file *parser.ParsedFile, rc *resolver.ResolutionContext, imports []resolver.ResolvedImport) ([]resolver.ResolvedLink, error) {
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

	impByAlias := indexGoImportsByAlias(imports)
	fileScope := file.Package
	if len(file.Classes) > 0 {
		if p := PackageScopePrefix(file.Classes[0].QName); p != "" {
			fileScope = p
		}
	} else if len(file.Functions) > 0 {
		if p := PackageScopePrefix(file.Functions[0].QName); p != "" {
			fileScope = p
		}
	}

	localClassByQName := make(map[string]parser.ParsedClass, len(file.Classes))
	for _, cls := range file.Classes {
		localClassByQName[cls.QName] = cls
	}

	var links []resolver.ResolvedLink
	for _, cls := range file.Classes {
		srcUID := ResolveClassOrSymbolUID(rc, cls.EffectiveKind(), cls.QName)
		for _, base := range cls.Bases {
			targetPkg := fileScope
			baseType := resolver.StripTypeDecorations(base)
			tier := resolver.TierSamePackage
			if idx := strings.LastIndexByte(baseType, '.'); idx >= 0 {
				alias := baseType[:idx]
				baseType = baseType[idx+1:]
				if imp, ok := impByAlias[alias]; ok && !imp.IsExternal && imp.TargetPackage != "" {
					targetPkg = imp.TargetPackage
					tier = resolver.TierQualifiedImport
				}
			}
			targetQName := targetPkg + "::" + baseType
			if _, isLocal := localClassByQName[targetQName]; isLocal {
				tier = resolver.TierLocal
			}
			if targetNode, ok := lookupGoClassNode(rc, targetQName); ok {
				links = append(links, MakeResolvedLink(srcUID, cls.QName, targetNode, graph.EdgeInherits, tier, cls.StartLine, "embedded: "+base))
			}
		}
	}

	if rc.View != nil && len(localClassByQName) > 0 {
		var localStructs []graph.Node
		for _, cls := range file.Classes {
			if cls.EffectiveKind() == graph.KindStruct {
				if stNode, ok := lookupGoClassNode(rc, cls.QName); ok && stNode.Kind == graph.KindStruct {
					localStructs = append(localStructs, stNode)
				}
			}
		}
		for _, sat := range detectInterfaceSatisfactionForStructs(rc.View, localStructs) {
			if cls, ok := localClassByQName[sat.StructQName]; ok {
				if ifaceNode, okIface := lookupGoClassNode(rc, sat.InterfaceQName); okIface {
					links = append(links, MakeResolvedLink(sat.StructUID, sat.StructQName, ifaceNode, graph.EdgeImplements, resolver.TierStructural, cls.StartLine, "structural interface satisfaction"))
				}
			}
		}
	}

	return DedupeResolvedLinks(links), nil
}

func lookupGoClassNode(rc *resolver.ResolutionContext, qname string) (graph.Node, bool) {
	if rc == nil || rc.View == nil || qname == "" {
		return graph.Node{}, false
	}
	for _, n := range rc.View.GetByQName(qname) {
		if n.Kind == graph.KindStruct || n.Kind == graph.KindInterface || n.Kind == graph.KindClass {
			return n, true
		}
	}
	return graph.Node{}, false
}
