package lang

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/odvcencio/gotreesitter"
	"github.com/roshankumar0036singh/gographctx/internal/graph"
	"github.com/roshankumar0036singh/gographctx/internal/parser"
	"github.com/roshankumar0036singh/gographctx/internal/parser/treesitter"
)

// RustParser extracts structural symbols, traits, impl blocks, enums, modules,
// use statements, macros, closures, and call sites from Rust (.rs) files.
type RustParser struct {
	mgr *treesitter.Manager
}

// NewRustParser constructs a Rust LanguageParser backed by mgr.
func NewRustParser(mgr *treesitter.Manager) (*RustParser, error) {
	if mgr == nil {
		return nil, ErrNilTreeSitterManager
	}
	return &RustParser{mgr: mgr}, nil
}

// Language returns the canonical language name ("rust").
func (p *RustParser) Language() string {
	return treesitter.LangRust
}

// Extensions returns the file extensions handled by RustParser ([".rs"]).
func (p *RustParser) Extensions() []string {
	return treesitter.ExtensionsForLanguage(treesitter.LangRust)
}

// Parse parses a Rust source file in a single AST pass and returns a ParsedFile.
func (p *RustParser) Parse(ctx context.Context, path string, source []byte) (*parser.ParsedFile, error) {
	if p == nil || p.mgr == nil {
		return nil, ErrNilTreeSitterManager
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	pt, err := p.mgr.ParseContext(ctx, treesitter.LangRust, source)
	if err != nil {
		return nil, err
	}
	defer pt.Release()

	root := pt.RootNode()
	lang := pt.Grammar

	modScope := deriveRustModuleScope(path)
	out := &parser.ParsedFile{
		Path:        path,
		Language:    treesitter.LangRust,
		Package:     modScope,
		ContentHash: ComputeContentHash(source),
		LineCount:   CountLines(source),
	}
	if root == nil {
		return out, nil
	}

	if pt.HasError() {
		out.HasSyntaxErrors = true
		out.ParseErrors = CollectSyntaxErrors(root, lang, maxCollectedSyntaxErrors)
	}

	scope := modScope
	if scope == "" {
		scope = "crate"
	}
	ext := &rustExtractor{
		path:       path,
		scope:      scope,
		src:        source,
		lang:       lang,
		out:        out,
		classIndex: make(map[string]int),
	}
	ext.walkItems(root, scope)

	return out, nil
}

// deriveRustModuleScope maps a Rust source file path to a "::"-delimited crate
// module scope: "src/store/mod.rs" -> "src::store", "src/lib.rs" -> "src::lib",
// "src/main.rs" -> "src::main", and "src/foo/bar.rs" -> "src::foo::bar".
func deriveRustModuleScope(path string) string {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return ""
	}
	slash := filepath.ToSlash(filepath.Clean(trimmed))
	base := filepath.Base(slash)
	if filepath.IsAbs(trimmed) || strings.HasPrefix(slash, "/") || strings.HasPrefix(slash, "..") {
		if base == "mod.rs" {
			parent := filepath.Base(filepath.Dir(slash))
			if parent != "." && parent != "/" && parent != "" {
				return parent
			}
		}
		return strings.TrimSuffix(base, filepath.Ext(base))
	}

	slash = strings.TrimPrefix(slash, "./")
	if base == "mod.rs" {
		dir := filepath.ToSlash(filepath.Dir(slash))
		if dir == "." || dir == "" {
			return "crate"
		}
		return strings.ReplaceAll(dir, "/", "::")
	}
	noExt := strings.TrimSuffix(slash, filepath.Ext(slash))
	return strings.ReplaceAll(noExt, "/", "::")
}

type rustExtractor struct {
	path       string
	scope      string
	src        []byte
	lang       *gotreesitter.Language
	out        *parser.ParsedFile
	classIndex map[string]int
}

func (e *rustExtractor) walkItems(container *gotreesitter.Node, scopeQName string) {
	if container == nil {
		return
	}
	count := container.NamedChildCount()
	for i := 0; i < count; i++ {
		node := container.NamedChild(i)
		if node == nil {
			continue
		}
		switch node.Type(e.lang) {
		case "use_declaration":
			e.extractUseDeclaration(node)
		case "mod_item":
			e.extractModItem(node, scopeQName)
		case "struct_item":
			e.extractStruct(node, scopeQName)
		case "enum_item":
			e.extractEnum(node, scopeQName)
		case "trait_item":
			e.extractTrait(node, scopeQName)
		case "type_item":
			e.extractTypeItem(node, scopeQName)
		case "const_item":
			e.extractConstOrStatic(node, scopeQName, true)
		case "static_item":
			e.extractConstOrStatic(node, scopeQName, false)
		case "function_item":
			e.extractFunction(node, scopeQName, "", "", false)
		case "impl_item":
			e.extractImplItem(node, scopeQName)
		}
	}
}

func (e *rustExtractor) extractUseDeclaration(decl *gotreesitter.Node) {
	line := NodeStartLine(decl)
	isPub := hasRustVisibility(decl, e.lang)

	var argNode *gotreesitter.Node
	if arg := decl.ChildByFieldName("argument", e.lang); arg != nil {
		argNode = arg
	} else {
		for i := 0; i < decl.NamedChildCount(); i++ {
			c := decl.NamedChild(i)
			if c != nil && c.Type(e.lang) != "visibility_modifier" && c.Type(e.lang) != "attribute_item" {
				argNode = c
				break
			}
		}
	}
	if argNode == nil {
		return
	}

	e.flattenUseTree("", argNode, line, isPub)
}

func (e *rustExtractor) flattenUseTree(prefix string, node *gotreesitter.Node, line uint32, isPub bool) {
	if node == nil {
		return
	}
	switch node.Type(e.lang) {
	case "identifier", "crate", "self", "super":
		sym := strings.TrimSpace(node.Text(e.src))
		if prefix == "" {
			e.out.Imports = append(e.out.Imports, parser.ParsedImport{
				Path:       sym,
				Line:       line,
				IsRelative: isRustRelativePath(sym),
				Exported:   isPub,
			})
			return
		}
		e.out.Imports = append(e.out.Imports, parser.ParsedImport{
			Path:       prefix,
			Names:      []string{sym},
			Line:       line,
			IsRelative: isRustRelativePath(prefix),
			Exported:   isPub,
		})

	case "scoped_identifier":
		full := CollapseWhitespace(node.Text(e.src))
		if prefix != "" {
			full = prefix + "::" + full
		}
		if idx := strings.LastIndex(full, "::"); idx >= 0 {
			modPath := full[:idx]
			sym := full[idx+2:]
			e.out.Imports = append(e.out.Imports, parser.ParsedImport{
				Path:       modPath,
				Names:      []string{sym},
				Line:       line,
				IsRelative: isRustRelativePath(modPath),
				Exported:   isPub,
			})
		} else {
			e.out.Imports = append(e.out.Imports, parser.ParsedImport{
				Path:       full,
				Line:       line,
				IsRelative: isRustRelativePath(full),
				Exported:   isPub,
			})
		}

	case "use_as_clause":
		pathNode := node.ChildByFieldName("path", e.lang)
		aliasNode := node.ChildByFieldName("alias", e.lang)
		if pathNode == nil || aliasNode == nil {
			return
		}
		alias := strings.TrimSpace(aliasNode.Text(e.src))
		rawPath := CollapseWhitespace(pathNode.Text(e.src))
		if prefix != "" {
			rawPath = prefix + "::" + rawPath
		}
		modPath := rawPath
		var names []string
		if idx := strings.LastIndex(rawPath, "::"); idx >= 0 {
			modPath = rawPath[:idx]
			names = []string{rawPath[idx+2:]}
		}
		e.out.Imports = append(e.out.Imports, parser.ParsedImport{
			Path:       modPath,
			Alias:      alias,
			Names:      names,
			Line:       line,
			IsRelative: isRustRelativePath(modPath),
			Exported:   isPub,
		})

	case "use_wildcard":
		var modPath string
		for i := 0; i < node.NamedChildCount(); i++ {
			if c := node.NamedChild(i); c != nil {
				modPath = CollapseWhitespace(c.Text(e.src))
				break
			}
		}
		if prefix != "" && modPath != "" {
			modPath = prefix + "::" + modPath
		} else if modPath == "" {
			modPath = prefix
		}
		if modPath != "" {
			e.out.Imports = append(e.out.Imports, parser.ParsedImport{
				Path:       modPath,
				Line:       line,
				IsWildcard: true,
				IsRelative: isRustRelativePath(modPath),
				Exported:   isPub,
			})
		}

	case "scoped_use_list":
		pathNode := node.ChildByFieldName("path", e.lang)
		listNode := node.ChildByFieldName("list", e.lang)
		nextPrefix := prefix
		if pathNode != nil {
			p := CollapseWhitespace(pathNode.Text(e.src))
			if nextPrefix != "" {
				nextPrefix = nextPrefix + "::" + p
			} else {
				nextPrefix = p
			}
		}
		if listNode != nil {
			e.flattenUseTree(nextPrefix, listNode, line, isPub)
		}

	case "use_list":
		for i := 0; i < node.NamedChildCount(); i++ {
			e.flattenUseTree(prefix, node.NamedChild(i), line, isPub)
		}
	}
}

func (e *rustExtractor) extractModItem(modNode *gotreesitter.Node, scopeQName string) {
	nameNode := modNode.ChildByFieldName("name", e.lang)
	if nameNode == nil {
		return
	}
	modName := strings.TrimSpace(nameNode.Text(e.src))
	if modName == "" {
		return
	}
	body := modNode.ChildByFieldName("body", e.lang)
	if body == nil {
		// External module declaration: `mod internal;`
		e.out.Imports = append(e.out.Imports, parser.ParsedImport{
			Path:       "./" + modName,
			Names:      []string{modName},
			Line:       NodeStartLine(modNode),
			IsRelative: true,
		})
		return
	}
	e.walkItems(body, JoinQName(scopeQName, modName))
}

func (e *rustExtractor) extractStruct(node *gotreesitter.Node, scopeQName string) {
	nameNode := node.ChildByFieldName("name", e.lang)
	if nameNode == nil {
		return
	}
	name := strings.TrimSpace(nameNode.Text(e.src))
	if name == "" {
		return
	}
	qname := JoinQName(scopeQName, name)
	decs, doc := collectRustAttributesAndDocs(node, e.src, e.lang)

	cls := parser.ParsedClass{
		Name:       name,
		QName:      qname,
		Kind:       graph.KindStruct,
		StartLine:  NodeStartLine(node),
		EndLine:    NodeEndLine(node),
		Exported:   hasRustVisibility(node, e.lang),
		TypeParams: e.extractRustTypeParams(node.ChildByFieldName("type_parameters", e.lang)),
		Decorators: decs,
		Docstring:  doc,
	}

	if body := node.ChildByFieldName("body", e.lang); body != nil && body.Type(e.lang) == "field_declaration_list" {
		for i := 0; i < body.NamedChildCount(); i++ {
			fd := body.NamedChild(i)
			if fd == nil || fd.Type(e.lang) != "field_declaration" {
				continue
			}
			fnameNode := fd.ChildByFieldName("name", e.lang)
			if fnameNode == nil {
				continue
			}
			fname := strings.TrimSpace(fnameNode.Text(e.src))
			if fname == "" {
				continue
			}
			var ftype string
			if tNode := fd.ChildByFieldName("type", e.lang); tNode != nil {
				ftype = CollapseWhitespace(tNode.Text(e.src))
			}
			fDecs, fDoc := collectRustAttributesAndDocs(fd, e.src, e.lang)
			cls.Fields = append(cls.Fields, parser.ParsedField{
				Name:       fname,
				QName:      JoinQName(qname, fname),
				Type:       ftype,
				StartLine:  NodeStartLine(fd),
				EndLine:    NodeEndLine(fd),
				Exported:   hasRustVisibility(fd, e.lang),
				Decorators: fDecs,
				Docstring:  fDoc,
			})
		}
	}

	e.classIndex[qname] = len(e.out.Classes)
	e.out.Classes = append(e.out.Classes, cls)
}

func (e *rustExtractor) extractEnum(node *gotreesitter.Node, scopeQName string) {
	nameNode := node.ChildByFieldName("name", e.lang)
	if nameNode == nil {
		return
	}
	name := strings.TrimSpace(nameNode.Text(e.src))
	if name == "" {
		return
	}
	qname := JoinQName(scopeQName, name)
	decs, doc := collectRustAttributesAndDocs(node, e.src, e.lang)

	cls := parser.ParsedClass{
		Name:       name,
		QName:      qname,
		Kind:       graph.KindEnum,
		StartLine:  NodeStartLine(node),
		EndLine:    NodeEndLine(node),
		Exported:   hasRustVisibility(node, e.lang),
		TypeParams: e.extractRustTypeParams(node.ChildByFieldName("type_parameters", e.lang)),
		Decorators: decs,
		Docstring:  doc,
	}

	if body := node.ChildByFieldName("body", e.lang); body != nil {
		for i := 0; i < body.NamedChildCount(); i++ {
			v := body.NamedChild(i)
			if v == nil || v.Type(e.lang) != "enum_variant" {
				continue
			}
			vNameNode := v.ChildByFieldName("name", e.lang)
			if vNameNode == nil {
				continue
			}
			vname := strings.TrimSpace(vNameNode.Text(e.src))
			if vname == "" {
				continue
			}
			vDecs, vDoc := collectRustAttributesAndDocs(v, e.src, e.lang)
			cls.Fields = append(cls.Fields, parser.ParsedField{
				Name:       vname,
				QName:      JoinQName(qname, vname),
				Type:       name,
				StartLine:  NodeStartLine(v),
				EndLine:    NodeEndLine(v),
				Exported:   cls.Exported,
				IsStatic:   true,
				Decorators: vDecs,
				Docstring:  vDoc,
			})
		}
	}

	e.classIndex[qname] = len(e.out.Classes)
	e.out.Classes = append(e.out.Classes, cls)
}

func (e *rustExtractor) extractTrait(node *gotreesitter.Node, scopeQName string) {
	nameNode := node.ChildByFieldName("name", e.lang)
	if nameNode == nil {
		return
	}
	name := strings.TrimSpace(nameNode.Text(e.src))
	if name == "" {
		return
	}
	qname := JoinQName(scopeQName, name)
	decs, doc := collectRustAttributesAndDocs(node, e.src, e.lang)

	var bases []string
	if bounds := node.ChildByFieldName("bounds", e.lang); bounds != nil {
		bases = appendRustTraitBounds(bases, bounds, e.lang, e.src)
	}
	for i := 0; i < node.NamedChildCount(); i++ {
		wc := node.NamedChild(i)
		if wc == nil || wc.Type(e.lang) != "where_clause" {
			continue
		}
		for j := 0; j < wc.NamedChildCount(); j++ {
			pred := wc.NamedChild(j)
			if pred == nil || pred.Type(e.lang) != "where_predicate" {
				continue
			}
			target := pred.ChildByFieldName("left", e.lang)
			bounds := pred.ChildByFieldName("bounds", e.lang)
			if target != nil && bounds != nil && strings.TrimSpace(target.Text(e.src)) == "Self" {
				bases = appendRustTraitBounds(bases, bounds, e.lang, e.src)
			}
		}
	}

	cls := parser.ParsedClass{
		Name:       name,
		QName:      qname,
		Kind:       graph.KindInterface,
		StartLine:  NodeStartLine(node),
		EndLine:    NodeEndLine(node),
		Exported:   hasRustVisibility(node, e.lang),
		Bases:      bases,
		TypeParams: e.extractRustTypeParams(node.ChildByFieldName("type_parameters", e.lang)),
		Decorators: decs,
		Docstring:  doc,
	}
	e.classIndex[qname] = len(e.out.Classes)
	e.out.Classes = append(e.out.Classes, cls)

	if body := node.ChildByFieldName("body", e.lang); body != nil {
		for i := 0; i < body.NamedChildCount(); i++ {
			m := body.NamedChild(i)
			if m == nil {
				continue
			}
			switch m.Type(e.lang) {
			case "function_signature_item":
				e.extractFunction(m, scopeQName, name, "", true)
			case "function_item":
				e.extractFunction(m, scopeQName, name, "", true)
			}
		}
	}
}

func (e *rustExtractor) extractImplItem(node *gotreesitter.Node, scopeQName string) {
	typeNode := node.ChildByFieldName("type", e.lang)
	if typeNode == nil {
		return
	}
	targetType := stripRustGenericsAndLifetimes(typeNode.Text(e.src))
	if targetType == "" {
		return
	}

	var traitName string
	if traitNode := node.ChildByFieldName("trait", e.lang); traitNode != nil {
		traitName = stripRustGenericsAndLifetimes(traitNode.Text(e.src))
	}

	// Link trait implementation directly onto the struct/enum if defined in the same module scope.
	if traitName != "" {
		targetQName := JoinQName(scopeQName, targetType)
		if idx, ok := e.classIndex[targetQName]; ok {
			if !containsString(e.out.Classes[idx].Implements, traitName) {
				e.out.Classes[idx].Implements = append(e.out.Classes[idx].Implements, traitName)
			}
		}
	}

	body := node.ChildByFieldName("body", e.lang)
	if body == nil {
		return
	}
	for i := 0; i < body.NamedChildCount(); i++ {
		item := body.NamedChild(i)
		if item == nil {
			continue
		}
		switch item.Type(e.lang) {
		case "function_item":
			e.extractFunction(item, scopeQName, targetType, traitName, false)
		case "const_item":
			e.extractConstOrStatic(item, JoinQName(scopeQName, targetType), true)
		}
	}
}

func (e *rustExtractor) extractTypeItem(node *gotreesitter.Node, scopeQName string) {
	nameNode := node.ChildByFieldName("name", e.lang)
	if nameNode == nil {
		return
	}
	name := strings.TrimSpace(nameNode.Text(e.src))
	if name == "" {
		return
	}
	var underlying string
	if tNode := node.ChildByFieldName("type", e.lang); tNode != nil {
		underlying = CollapseWhitespace(tNode.Text(e.src))
	}
	_, doc := collectRustAttributesAndDocs(node, e.src, e.lang)
	e.out.TypeAliases = append(e.out.TypeAliases, parser.ParsedTypeAlias{
		Name:           name,
		QName:          JoinQName(scopeQName, name),
		UnderlyingType: underlying,
		StartLine:      NodeStartLine(node),
		EndLine:        NodeEndLine(node),
		Exported:       hasRustVisibility(node, e.lang),
		TypeParams:     e.extractRustTypeParams(node.ChildByFieldName("type_parameters", e.lang)),
		Docstring:      doc,
	})
}

func (e *rustExtractor) extractConstOrStatic(node *gotreesitter.Node, scopeQName string, isConst bool) {
	nameNode := node.ChildByFieldName("name", e.lang)
	if nameNode == nil {
		return
	}
	name := strings.TrimSpace(nameNode.Text(e.src))
	if name == "" {
		return
	}
	var typeStr string
	if tNode := node.ChildByFieldName("type", e.lang); tNode != nil {
		typeStr = CollapseWhitespace(tNode.Text(e.src))
	}
	_, doc := collectRustAttributesAndDocs(node, e.src, e.lang)
	e.out.Variables = append(e.out.Variables, parser.ParsedVariable{
		Name:      name,
		QName:     JoinQName(scopeQName, name),
		Type:      typeStr,
		StartLine: NodeStartLine(node),
		EndLine:   NodeEndLine(node),
		Exported:  hasRustVisibility(node, e.lang),
		IsConst:   isConst,
		Docstring: doc,
	})
}

func (e *rustExtractor) extractFunction(node *gotreesitter.Node, scopeQName, implTarget, implTrait string, isTraitSig bool) {
	nameNode := node.ChildByFieldName("name", e.lang)
	if nameNode == nil {
		return
	}
	name := strings.TrimSpace(nameNode.Text(e.src))
	if name == "" {
		return
	}

	decs, doc := collectRustAttributesAndDocs(node, e.src, e.lang)
	if isTraitSig {
		decs = append(decs, "rust:trait_method")
	}
	if implTrait != "" {
		decs = append(decs, "rust:impl_trait:"+implTrait)
	}

	isAsync := hasRustAsyncModifier(node, e.src, e.lang)
	params, hasSelf, selfText := e.extractRustParams(node.ChildByFieldName("parameters", e.lang))
	retType := ""
	if rNode := node.ChildByFieldName("return_type", e.lang); rNode != nil {
		retType = CollapseWhitespace(rNode.Text(e.src))
	}

	qname := JoinQName(scopeQName, name)
	if implTarget != "" {
		qname = JoinQName(scopeQName, implTarget, name)
	}

	var recv string
	if hasSelf {
		recv = selfText
	}
	body := node.ChildByFieldName("body", e.lang)

	e.out.Functions = append(e.out.Functions, parser.ParsedFunction{
		Name:          name,
		QName:         qname,
		Kind:          graph.KindFunction,
		Receiver:      recv,
		ReceiverType:  implTarget,
		StartLine:     NodeStartLine(node),
		EndLine:       NodeEndLine(node),
		Params:        params,
		ReturnType:    retType,
		Signature:     formatRustSignature(isAsync, name, selfText, params, retType),
		TypeParams:    e.extractRustTypeParams(node.ChildByFieldName("type_parameters", e.lang)),
		Exported:      hasRustVisibility(node, e.lang) || implTrait != "",
		IsMethod:      hasSelf,
		IsAsync:       isAsync,
		IsConstructor: implTarget != "" && !hasSelf && name == "new",
		Decorators:    decs,
		Docstring:     doc,
		Complexity:    computeRustComplexity(body, e.lang, e.src),
	})

	if body != nil {
		e.collectCalls(body, qname)
	}
}

func (e *rustExtractor) extractRustParams(paramsNode *gotreesitter.Node) (params []graph.Param, hasSelf bool, selfText string) {
	if paramsNode == nil {
		return nil, false, ""
	}
	for i := 0; i < paramsNode.NamedChildCount(); i++ {
		p := paramsNode.NamedChild(i)
		if p == nil {
			continue
		}
		switch p.Type(e.lang) {
		case "self_parameter":
			hasSelf = true
			selfText = CollapseWhitespace(p.Text(e.src))
		case "parameter":
			var pName, pType string
			if pat := p.ChildByFieldName("pattern", e.lang); pat != nil {
				pName = CollapseWhitespace(pat.Text(e.src))
			}
			if tNode := p.ChildByFieldName("type", e.lang); tNode != nil {
				pType = CollapseWhitespace(tNode.Text(e.src))
			}
			if pName != "" {
				params = append(params, graph.Param{Name: pName, Type: pType})
			}
		}
	}
	return params, hasSelf, selfText
}

func (e *rustExtractor) extractRustTypeParams(tpNode *gotreesitter.Node) []string {
	if tpNode == nil {
		return nil
	}
	var out []string
	for i := 0; i < tpNode.NamedChildCount(); i++ {
		c := tpNode.NamedChild(i)
		if c == nil || c.Type(e.lang) == "lifetime_parameter" {
			continue // Ignore lifetimes for resolution clarity
		}
		if txt := CollapseWhitespace(c.Text(e.src)); txt != "" {
			out = append(out, txt)
		}
	}
	return out
}

func (e *rustExtractor) collectCalls(node *gotreesitter.Node, callerQName string) {
	if node == nil {
		return
	}
	switch node.Type(e.lang) {
	case "call_expression":
		e.recordRustCall(node, callerQName)
	case "macro_invocation":
		e.recordRustMacroCall(node, callerQName)
	}
	count := node.NamedChildCount()
	for i := 0; i < count; i++ {
		e.collectCalls(node.NamedChild(i), callerQName)
	}
}

func (e *rustExtractor) recordRustCall(callNode *gotreesitter.Node, callerQName string) {
	fnNode := callNode.ChildByFieldName("function", e.lang)
	if fnNode == nil {
		return
	}
	argsNode := callNode.ChildByFieldName("arguments", e.lang)
	argCount := countRustArguments(argsNode, e.lang)

	call := parser.ParsedCall{
		CallerQName:   callerQName,
		Line:          NodeStartLine(callNode),
		Col:           NodeStartCol(callNode),
		ArgumentCount: argCount,
	}

	if fnNode.Type(e.lang) == "generic_function" {
		if inner := fnNode.ChildByFieldName("function", e.lang); inner != nil {
			fnNode = inner
		} else if fnNode.NamedChildCount() > 0 {
			fnNode = fnNode.NamedChild(0)
		}
	}

	switch fnNode.Type(e.lang) {
	case "identifier":
		call.CalleeName = strings.TrimSpace(fnNode.Text(e.src))
	case "field_expression":
		call.IsMethodCall = true
		if val := fnNode.ChildByFieldName("value", e.lang); val != nil {
			call.ReceiverName = CollapseWhitespace(val.Text(e.src))
		}
		if field := fnNode.ChildByFieldName("field", e.lang); field != nil {
			call.CalleeName = strings.TrimSpace(field.Text(e.src))
		}
	case "scoped_identifier":
		pathNode := fnNode.ChildByFieldName("path", e.lang)
		nameNode := fnNode.ChildByFieldName("name", e.lang)
		if pathNode != nil {
			call.ReceiverName = stripRustGenericsAndLifetimes(pathNode.Text(e.src))
		}
		if nameNode != nil {
			call.CalleeName = strings.TrimSpace(nameNode.Text(e.src))
		}
		if call.ReceiverName != "" {
			call.IsMethodCall = true
		}
	default:
		call.IsDynamic = true
		call.CalleeName = CollapseWhitespace(fnNode.Text(e.src))
	}

	if call.CalleeName != "" {
		e.out.Calls = append(e.out.Calls, call)
	}
}

func (e *rustExtractor) recordRustMacroCall(macroNode *gotreesitter.Node, callerQName string) {
	nameNode := macroNode.ChildByFieldName("macro", e.lang)
	if nameNode == nil && macroNode.NamedChildCount() > 0 {
		nameNode = macroNode.NamedChild(0)
	}
	if nameNode == nil {
		return
	}
	rawName := CollapseWhitespace(nameNode.Text(e.src))
	if rawName == "" {
		return
	}
	var recv, callee string
	isMethod := false
	if idx := strings.LastIndex(rawName, "::"); idx >= 0 {
		recv = rawName[:idx]
		callee = rawName[idx+2:] + "!"
		isMethod = true
	} else {
		callee = rawName + "!"
	}
	e.out.Calls = append(e.out.Calls, parser.ParsedCall{
		CallerQName:  callerQName,
		CalleeName:   callee,
		ReceiverName: recv,
		Line:         NodeStartLine(macroNode),
		Col:          NodeStartCol(macroNode),
		IsMethodCall: isMethod,
	})
}

func countRustArguments(argsNode *gotreesitter.Node, lang *gotreesitter.Language) uint16 {
	if argsNode == nil {
		return 0
	}
	var count uint32
	n := argsNode.NamedChildCount()
	for i := 0; i < n; i++ {
		if c := argsNode.NamedChild(i); c != nil && c.Type(lang) != "line_comment" && c.Type(lang) != "block_comment" {
			count++
		}
	}
	if count > 0xffff {
		return 0xffff
	}
	return uint16(count)
}

func computeRustComplexity(body *gotreesitter.Node, lang *gotreesitter.Language, src []byte) uint16 {
	if body == nil {
		return 1
	}
	complexity := uint32(1)
	var walk func(n *gotreesitter.Node)
	walk = func(n *gotreesitter.Node) {
		if n == nil {
			return
		}
		switch n.Type(lang) {
		case "if_expression", "for_expression", "while_expression", "loop_expression", "match_arm", "try_expression":
			complexity++
		case "binary_expression":
			if op := n.ChildByFieldName("operator", lang); op != nil {
				opTxt := strings.TrimSpace(op.Text(src))
				if opTxt == "&&" || opTxt == "||" {
					complexity++
				}
			}
		}
		count := n.NamedChildCount()
		for i := 0; i < count; i++ {
			walk(n.NamedChild(i))
		}
	}
	walk(body)
	if complexity > 0xffff {
		return 0xffff
	}
	return uint16(complexity)
}

func collectRustAttributesAndDocs(target *gotreesitter.Node, src []byte, lang *gotreesitter.Language) (decs []string, doc string) {
	if target == nil || lang == nil {
		return nil, ""
	}
	var docLines []string
	var attrList []string
	nextRow := target.StartPoint().Row

	for prev := target.PrevSibling(); prev != nil; prev = prev.PrevSibling() {
		typ := prev.Type(lang)
		if typ != "attribute_item" && typ != "line_comment" && typ != "block_comment" {
			break
		}
		if nextRow > prev.EndPoint().Row+1 {
			break
		}
		switch typ {
		case "attribute_item":
			raw := strings.TrimSpace(prev.Text(src))
			raw = strings.TrimSuffix(strings.TrimPrefix(raw, "#["), "]")
			if raw != "" {
				attrList = append(attrList, CollapseWhitespace(raw))
			}
		case "line_comment", "block_comment":
			raw := strings.TrimSpace(prev.Text(src))
			if strings.HasPrefix(raw, "///") || strings.HasPrefix(raw, "//!") {
				txt := strings.TrimPrefix(strings.TrimPrefix(raw, "///"), "//!")
				docLines = append(docLines, strings.TrimPrefix(txt, " "))
			} else if strings.HasPrefix(raw, "/**") {
				docLines = append(docLines, cleanCommentText(raw))
			}
		}
		nextRow = prev.StartPoint().Row
	}

	// Reverse slices from bottom-up into source order.
	for i, j := 0, len(attrList)-1; i < j; i, j = i+1, j-1 {
		attrList[i], attrList[j] = attrList[j], attrList[i]
	}
	for i, j := 0, len(docLines)-1; i < j; i, j = i+1, j-1 {
		docLines[i], docLines[j] = docLines[j], docLines[i]
	}
	return attrList, strings.TrimSpace(strings.Join(docLines, "\n"))
}

func hasRustVisibility(node *gotreesitter.Node, lang *gotreesitter.Language) bool {
	if node == nil {
		return false
	}
	for i := 0; i < node.NamedChildCount(); i++ {
		if c := node.NamedChild(i); c != nil && c.Type(lang) == "visibility_modifier" {
			return true
		}
	}
	return false
}

func hasRustAsyncModifier(node *gotreesitter.Node, src []byte, lang *gotreesitter.Language) bool {
	if node == nil {
		return false
	}
	for i := 0; i < node.NamedChildCount(); i++ {
		c := node.NamedChild(i)
		if c != nil && c.Type(lang) == "function_modifiers" {
			if strings.Contains(c.Text(src), "async") {
				return true
			}
		}
	}
	return false
}

func appendRustTraitBounds(bases []string, bounds *gotreesitter.Node, lang *gotreesitter.Language, src []byte) []string {
	for i := 0; i < bounds.NamedChildCount(); i++ {
		b := bounds.NamedChild(i)
		if b == nil || b.Type(lang) == "lifetime" {
			continue
		}
		if base := stripRustGenericsAndLifetimes(b.Text(src)); base != "" && !containsString(bases, base) {
			bases = append(bases, base)
		}
	}
	return bases
}

func stripRustGenericsAndLifetimes(raw string) string {
	s := CollapseWhitespace(raw)
	s = strings.TrimPrefix(s, "&mut ")
	s = strings.TrimLeft(s, "&")
	s = strings.TrimSpace(s)
	// Strip balanced ::<...> turbofish and <...> generic/lifetime groups while
	// keeping multi-segment paths such as a::<T>::b intact.
	s = strings.ReplaceAll(s, "::<", "<")
	var b strings.Builder
	b.Grow(len(s))
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '<':
			depth++
		case '>':
			if depth > 0 {
				depth--
			} else {
				b.WriteByte('>')
			}
		default:
			if depth == 0 {
				b.WriteByte(s[i])
			}
		}
	}
	return strings.TrimSpace(b.String())
}

func isRustRelativePath(p string) bool {
	return strings.HasPrefix(p, "crate::") || strings.HasPrefix(p, "self::") || strings.HasPrefix(p, "super::") ||
		p == "crate" || p == "self" || p == "super" || strings.HasPrefix(p, "./")
}

func containsString(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

func formatRustSignature(isAsync bool, name, selfText string, params []graph.Param, ret string) string {
	var b strings.Builder
	if isAsync {
		b.WriteString("async ")
	}
	b.WriteString("fn ")
	b.WriteString(name)
	b.WriteByte('(')
	wrote := false
	if selfText != "" {
		b.WriteString(selfText)
		wrote = true
	}
	for _, p := range params {
		if wrote {
			b.WriteString(", ")
		}
		b.WriteString(p.Name)
		if p.Type != "" {
			b.WriteString(": ")
			b.WriteString(p.Type)
		}
		wrote = true
	}
	b.WriteByte(')')
	if ret != "" {
		b.WriteString(" -> ")
		b.WriteString(ret)
	}
	return b.String()
}

// RegisterRust creates a RustParser backed by mgr and registers it into reg.
func RegisterRust(reg *parser.Registry, mgr *treesitter.Manager) (*RustParser, error) {
	if reg == nil {
		return nil, fmt.Errorf("%w: nil registry", parser.ErrInvalidParser)
	}
	rp, err := NewRustParser(mgr)
	if err != nil {
		return nil, err
	}
	if err := reg.Register(rp); err != nil {
		return nil, err
	}
	return rp, nil
}
