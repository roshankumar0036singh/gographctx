package lang

import (
	"context"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/odvcencio/gotreesitter"
	"github.com/roshankumar0036singh/gographctx/internal/graph"
	"github.com/roshankumar0036singh/gographctx/internal/parser"
	"github.com/roshankumar0036singh/gographctx/internal/parser/treesitter"
)

type jstsConfig struct {
	canonicalLang string
	supportsTypes bool
	supportsJSX   bool
}

func parseJSTS(ctx context.Context, mgr *treesitter.Manager, cfg jstsConfig, path string, source []byte) (*parser.ParsedFile, error) {
	if mgr == nil {
		return nil, ErrNilTreeSitterManager
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	pt, err := mgr.ParseContext(ctx, cfg.canonicalLang, source)
	if err != nil {
		return nil, err
	}
	defer pt.Release()

	root := pt.RootNode()
	lang := pt.Grammar

	scope := deriveJSTSModuleScope(path)
	out := &parser.ParsedFile{
		Path:        path,
		Language:    cfg.canonicalLang,
		Package:     scope,
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

	ext := &jstsExtractor{
		cfg:           cfg,
		path:          path,
		scope:         scope,
		moduleCaller:  JoinQName(scope, "<module>"),
		src:           source,
		lang:          lang,
		out:           out,
		exportedNames: make(map[string]struct{}),
	}
	ext.walkProgram(root)
	ext.markDeferredExports()

	return out, nil
}

func deriveJSTSModuleScope(path string) string {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return ""
	}
	slash := filepath.ToSlash(filepath.Clean(trimmed))
	base := filepath.Base(slash)
	if filepath.IsAbs(trimmed) || strings.HasPrefix(slash, "/") || strings.HasPrefix(slash, "..") {
		return strings.TrimSuffix(base, filepath.Ext(base))
	}
	noExt := strings.TrimSuffix(slash, filepath.Ext(slash))
	noExt = strings.TrimPrefix(noExt, "./")
	return noExt
}

type jstsExtractor struct {
	cfg           jstsConfig
	path          string
	scope         string
	moduleCaller  string
	src           []byte
	lang          *gotreesitter.Language
	out           *parser.ParsedFile
	exportedNames map[string]struct{}
}

func (e *jstsExtractor) walkProgram(root *gotreesitter.Node) {
	e.walkStatements(root, e.scope, e.moduleCaller)
}

func (e *jstsExtractor) walkStatements(container *gotreesitter.Node, scopeQName, callerQName string) {
	if container == nil {
		return
	}
	var pendingDecs []string
	count := container.NamedChildCount()
	for i := 0; i < count; i++ {
		node := container.NamedChild(i)
		if node == nil {
			continue
		}
		switch node.Type(e.lang) {
		case "comment":
			continue
		case "decorator":
			if dec := cleanJSTSDecorator(node.Text(e.src)); dec != "" {
				pendingDecs = append(pendingDecs, dec)
			}
			e.collectCalls(node, callerQName)
		case "import_statement":
			pendingDecs = nil
			e.extractImportStatement(node)
		case "export_statement":
			doc := LeadingDocComment(node, e.src, e.lang)
			decs := append(pendingDecs, e.extractLeadingDecorators(node, callerQName)...)
			pendingDecs = nil
			e.extractExportStatement(node, scopeQName, callerQName, doc, decs)
		case "function_declaration", "generator_function_declaration":
			decs := pendingDecs
			pendingDecs = nil
			e.extractFunctionDecl(node, scopeQName, false, LeadingDocComment(node, e.src, e.lang), decs)
		case "class_declaration", "class", "abstract_class_declaration":
			decs := append(pendingDecs, e.extractLeadingDecorators(node, callerQName)...)
			pendingDecs = nil
			e.extractClass(node, scopeQName, callerQName, false, LeadingDocComment(node, e.src, e.lang), decs)
		case "interface_declaration":
			pendingDecs = nil
			if e.cfg.supportsTypes {
				e.extractInterface(node, scopeQName, false, LeadingDocComment(node, e.src, e.lang))
			}
		case "type_alias_declaration":
			pendingDecs = nil
			if e.cfg.supportsTypes {
				e.extractTypeAlias(node, scopeQName, false, LeadingDocComment(node, e.src, e.lang))
			}
		case "enum_declaration":
			pendingDecs = nil
			if e.cfg.supportsTypes {
				e.extractEnum(node, scopeQName, false, LeadingDocComment(node, e.src, e.lang))
			}
		case "internal_module", "module":
			pendingDecs = nil
			if e.cfg.supportsTypes {
				e.extractNamespace(node, scopeQName, callerQName)
			}
		case "lexical_declaration", "variable_declaration":
			pendingDecs = nil
			e.extractVariableDecl(node, scopeQName, callerQName, false, LeadingDocComment(node, e.src, e.lang))
		case "expression_statement":
			pendingDecs = nil
			e.extractExpressionStatement(node, scopeQName, callerQName)
		default:
			pendingDecs = nil
			e.collectCalls(node, callerQName)
		}
	}
}

func (e *jstsExtractor) extractLeadingDecorators(node *gotreesitter.Node, callerQName string) []string {
	var decs []string
	for i := 0; i < node.NamedChildCount(); i++ {
		c := node.NamedChild(i)
		if c != nil && c.Type(e.lang) == "decorator" {
			if dec := cleanJSTSDecorator(c.Text(e.src)); dec != "" {
				decs = append(decs, dec)
			}
			before := len(e.out.Calls)
			e.collectCalls(c, callerQName)
			if len(e.out.Calls) == before && c.NamedChildCount() > 0 {
				e.recordJSCallableExpr(c.NamedChild(0), callerQName, 1)
			}
		}
	}
	return decs
}

func (e *jstsExtractor) recordJSCallableExpr(expr *gotreesitter.Node, callerQName string, argCount uint16) {
	if expr == nil || callerQName == "" {
		return
	}
	call := parser.ParsedCall{
		CallerQName:   callerQName,
		Line:          NodeStartLine(expr),
		Col:           NodeStartCol(expr),
		ArgumentCount: argCount,
	}
	switch expr.Type(e.lang) {
	case "identifier":
		call.CalleeName = strings.TrimSpace(expr.Text(e.src))
	case "member_expression":
		call.IsMethodCall = true
		if obj := expr.ChildByFieldName("object", e.lang); obj != nil {
			call.ReceiverName = CollapseWhitespace(obj.Text(e.src))
		}
		if prop := expr.ChildByFieldName("property", e.lang); prop != nil {
			call.CalleeName = strings.TrimSpace(prop.Text(e.src))
		}
	}
	if call.CalleeName != "" {
		e.out.Calls = append(e.out.Calls, call)
	}
}

func cleanJSTSDecorator(raw string) string {
	s := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "@"))
	return CollapseWhitespace(s)
}

func (e *jstsExtractor) extractImportStatement(stmt *gotreesitter.Node) {
	line := NodeStartLine(stmt)
	srcNode := stmt.ChildByFieldName("source", e.lang)
	if srcNode == nil {
		for i := stmt.NamedChildCount() - 1; i >= 0; i-- {
			c := stmt.NamedChild(i)
			if c != nil && c.Type(e.lang) == "string" {
				srcNode = c
				break
			}
		}
	}
	if srcNode == nil {
		return
	}
	modPath := unquoteJSString(srcNode.Text(e.src))
	if modPath == "" {
		return
	}
	isRel := isJSRelativePath(modPath)
	isStmtTypeOnly := hasUnnamedChildToken(stmt, e.src, "type")

	var clauseNode *gotreesitter.Node
	for i := 0; i < stmt.NamedChildCount(); i++ {
		if c := stmt.NamedChild(i); c != nil && c.Type(e.lang) == "import_clause" {
			clauseNode = c
			break
		}
	}
	if clauseNode == nil {
		// Side-effect import: import "./polyfill";
		e.out.Imports = append(e.out.Imports, parser.ParsedImport{
			Path:       modPath,
			Line:       line,
			IsTypeOnly: isStmtTypeOnly,
			IsRelative: isRel,
		})
		return
	}

	var defaultName string
	var namedUnaliased []string
	var extraImports []parser.ParsedImport

	for i := 0; i < clauseNode.NamedChildCount(); i++ {
		c := clauseNode.NamedChild(i)
		if c == nil {
			continue
		}
		switch c.Type(e.lang) {
		case "identifier":
			defaultName = strings.TrimSpace(c.Text(e.src))
		case "namespace_import":
			var alias string
			for j := 0; j < c.NamedChildCount(); j++ {
				if id := c.NamedChild(j); id != nil && id.Type(e.lang) == "identifier" {
					alias = strings.TrimSpace(id.Text(e.src))
					break
				}
			}
			extraImports = append(extraImports, parser.ParsedImport{
				Path:       modPath,
				Alias:      alias,
				Line:       line,
				IsWildcard: true,
				IsTypeOnly: isStmtTypeOnly,
				IsRelative: isRel,
			})
		case "named_imports":
			for j := 0; j < c.NamedChildCount(); j++ {
				spec := c.NamedChild(j)
				if spec == nil || spec.Type(e.lang) != "import_specifier" {
					continue
				}
				symNode := spec.ChildByFieldName("name", e.lang)
				aliasNode := spec.ChildByFieldName("alias", e.lang)
				var sym, alias string
				if symNode != nil {
					sym = strings.TrimSpace(symNode.Text(e.src))
				}
				if aliasNode != nil {
					alias = strings.TrimSpace(aliasNode.Text(e.src))
				}
				specTypeOnly := isStmtTypeOnly || hasUnnamedChildToken(spec, e.src, "type")
				if alias != "" || specTypeOnly != isStmtTypeOnly {
					extraImports = append(extraImports, parser.ParsedImport{
						Path:       modPath,
						Alias:      alias,
						Names:      []string{sym},
						Line:       line,
						IsTypeOnly: specTypeOnly,
						IsRelative: isRel,
					})
				} else if sym != "" {
					namedUnaliased = append(namedUnaliased, sym)
				}
			}
		}
	}

	if defaultName != "" {
		e.out.Imports = append(e.out.Imports, parser.ParsedImport{
			Path:       modPath,
			Alias:      defaultName,
			Names:      []string{"default"},
			Line:       line,
			IsTypeOnly: isStmtTypeOnly,
			IsRelative: isRel,
		})
	}
	if len(namedUnaliased) > 0 {
		e.out.Imports = append(e.out.Imports, parser.ParsedImport{
			Path:       modPath,
			Names:      namedUnaliased,
			Line:       line,
			IsTypeOnly: isStmtTypeOnly,
			IsRelative: isRel,
		})
	}
	e.out.Imports = append(e.out.Imports, extraImports...)
}

func (e *jstsExtractor) extractExportStatement(stmt *gotreesitter.Node, scopeQName, callerQName, doc string, decs []string) {
	line := NodeStartLine(stmt)
	isDefault := hasUnnamedChildToken(stmt, e.src, "default")
	isTypeOnly := hasUnnamedChildToken(stmt, e.src, "type")
	isStar := hasUnnamedChildToken(stmt, e.src, "*")

	// Re-exports: export { a as b } from "./client"; or export * from "./mod";
	if srcNode := stmt.ChildByFieldName("source", e.lang); srcNode != nil {
		modPath := unquoteJSString(srcNode.Text(e.src))
		isRel := isJSRelativePath(modPath)
		if isStar {
			e.out.Imports = append(e.out.Imports, parser.ParsedImport{
				Path:       modPath,
				Line:       line,
				IsWildcard: true,
				IsTypeOnly: isTypeOnly,
				IsRelative: isRel,
				Exported:   true,
			})
			return
		}
		var names []string
		for i := 0; i < stmt.NamedChildCount(); i++ {
			c := stmt.NamedChild(i)
			if c == nil || c.Type(e.lang) != "export_clause" {
				continue
			}
			for j := 0; j < c.NamedChildCount(); j++ {
				spec := c.NamedChild(j)
				if spec == nil || spec.Type(e.lang) != "export_specifier" {
					continue
				}
				nameNode := spec.ChildByFieldName("name", e.lang)
				aliasNode := spec.ChildByFieldName("alias", e.lang)
				var sym, alias string
				if nameNode != nil {
					sym = strings.TrimSpace(nameNode.Text(e.src))
				}
				if aliasNode != nil {
					alias = strings.TrimSpace(aliasNode.Text(e.src))
				}
				if alias != "" {
					e.out.Imports = append(e.out.Imports, parser.ParsedImport{
						Path:       modPath,
						Alias:      alias,
						Names:      []string{sym},
						Line:       line,
						IsTypeOnly: isTypeOnly,
						IsRelative: isRel,
						Exported:   true,
					})
				} else if sym != "" {
					names = append(names, sym)
				}
			}
		}
		if len(names) > 0 {
			e.out.Imports = append(e.out.Imports, parser.ParsedImport{
				Path:       modPath,
				Names:      names,
				Line:       line,
				IsTypeOnly: isTypeOnly,
				IsRelative: isRel,
				Exported:   true,
			})
		}
		return
	}

	decl := stmt.ChildByFieldName("declaration", e.lang)
	if decl == nil {
		// Look for exported value/declaration child (e.g. export default class / export default Service / export default function).
		for i := 0; i < stmt.NamedChildCount(); i++ {
			c := stmt.NamedChild(i)
			if c == nil || c.Type(e.lang) == "decorator" || c.Type(e.lang) == "comment" {
				continue
			}
			decl = c
			break
		}
	}
	if decl == nil {
		return
	}

	switch decl.Type(e.lang) {
	case "function_declaration", "generator_function_declaration", "function_expression":
		e.extractFunctionDecl(decl, scopeQName, true, doc, decs)
	case "class_declaration", "class", "abstract_class_declaration":
		nestedDecs := append(append([]string(nil), decs...), e.extractLeadingDecorators(decl, callerQName)...)
		e.extractClass(decl, scopeQName, callerQName, true, doc, nestedDecs)
	case "interface_declaration":
		if e.cfg.supportsTypes {
			e.extractInterface(decl, scopeQName, true, doc)
		}
	case "type_alias_declaration":
		if e.cfg.supportsTypes {
			e.extractTypeAlias(decl, scopeQName, true, doc)
		}
	case "enum_declaration":
		if e.cfg.supportsTypes {
			e.extractEnum(decl, scopeQName, true, doc)
		}
	case "internal_module", "module":
		if e.cfg.supportsTypes {
			e.extractNamespace(decl, scopeQName, callerQName)
		}
	case "lexical_declaration", "variable_declaration":
		e.extractVariableDecl(decl, scopeQName, callerQName, true, doc)
	case "identifier":
		if isDefault {
			e.exportedNames[strings.TrimSpace(decl.Text(e.src))] = struct{}{}
		}
	case "export_clause":
		for i := 0; i < decl.NamedChildCount(); i++ {
			if spec := decl.NamedChild(i); spec != nil && spec.Type(e.lang) == "export_specifier" {
				if nameNode := spec.ChildByFieldName("name", e.lang); nameNode != nil {
					e.exportedNames[strings.TrimSpace(nameNode.Text(e.src))] = struct{}{}
				}
			}
		}
	default:
		e.collectCalls(decl, callerQName)
	}
}

func (e *jstsExtractor) markDeferredExports() {
	if len(e.exportedNames) == 0 {
		return
	}
	for i := range e.out.Functions {
		if _, ok := e.exportedNames[e.out.Functions[i].Name]; ok && !e.out.Functions[i].IsMethod {
			e.out.Functions[i].Exported = true
		}
	}
	for i := range e.out.Classes {
		if _, ok := e.exportedNames[e.out.Classes[i].Name]; ok {
			e.out.Classes[i].Exported = true
		}
	}
	for i := range e.out.Variables {
		if _, ok := e.exportedNames[e.out.Variables[i].Name]; ok {
			e.out.Variables[i].Exported = true
		}
	}
	for i := range e.out.TypeAliases {
		if _, ok := e.exportedNames[e.out.TypeAliases[i].Name]; ok {
			e.out.TypeAliases[i].Exported = true
		}
	}
}

func (e *jstsExtractor) extractNamespace(nsNode *gotreesitter.Node, scopeQName, callerQName string) {
	nameNode := nsNode.ChildByFieldName("name", e.lang)
	if nameNode == nil {
		return
	}
	nsName := strings.TrimSpace(nameNode.Text(e.src))
	if nsName == "" {
		return
	}
	body := nsNode.ChildByFieldName("body", e.lang)
	if body == nil {
		return
	}
	e.walkStatements(body, JoinQName(scopeQName, nsName), callerQName)
}

func (e *jstsExtractor) extractInterface(ifaceNode *gotreesitter.Node, scopeQName string, exported bool, doc string) {
	nameNode := ifaceNode.ChildByFieldName("name", e.lang)
	if nameNode == nil {
		return
	}
	name := strings.TrimSpace(nameNode.Text(e.src))
	if name == "" {
		return
	}
	qname := JoinQName(scopeQName, name)

	var bases []string
	for i := 0; i < ifaceNode.NamedChildCount(); i++ {
		c := ifaceNode.NamedChild(i)
		if c != nil && c.Type(e.lang) == "extends_type_clause" {
			for j := 0; j < c.NamedChildCount(); j++ {
				if b := c.NamedChild(j); b != nil {
					if baseName := stripTSTypeArgs(b.Text(e.src)); baseName != "" {
						bases = append(bases, baseName)
					}
				}
			}
		}
	}

	cls := parser.ParsedClass{
		Name:       name,
		QName:      qname,
		Kind:       graph.KindInterface,
		StartLine:  NodeStartLine(ifaceNode),
		EndLine:    NodeEndLine(ifaceNode),
		Exported:   exported,
		Bases:      bases,
		TypeParams: e.extractTSTypeParams(ifaceNode.ChildByFieldName("type_parameters", e.lang)),
		Docstring:  doc,
	}

	body := ifaceNode.ChildByFieldName("body", e.lang)
	if body != nil {
		for i := 0; i < body.NamedChildCount(); i++ {
			member := body.NamedChild(i)
			if member == nil {
				continue
			}
			switch member.Type(e.lang) {
			case "property_signature":
				pnameNode := member.ChildByFieldName("name", e.lang)
				if pnameNode == nil {
					continue
				}
				pname := strings.TrimSpace(pnameNode.Text(e.src))
				if pname == "" {
					continue
				}
				var ptype string
				if tNode := member.ChildByFieldName("type", e.lang); tNode != nil {
					ptype = cleanTSTypeAnnotation(tNode.Text(e.src))
				}
				cls.Fields = append(cls.Fields, parser.ParsedField{
					Name:      pname,
					QName:     JoinQName(qname, pname),
					Type:      ptype,
					StartLine: NodeStartLine(member),
					EndLine:   NodeEndLine(member),
					Exported:  true,
					Docstring: LeadingDocComment(member, e.src, e.lang),
				})
			case "method_signature":
				mnameNode := member.ChildByFieldName("name", e.lang)
				if mnameNode == nil {
					continue
				}
				mname := strings.TrimSpace(mnameNode.Text(e.src))
				if mname == "" {
					continue
				}
				params, _ := e.extractJSParams(member.ChildByFieldName("parameters", e.lang), member)
				var retType string
				if rNode := member.ChildByFieldName("return_type", e.lang); rNode != nil {
					retType = cleanTSTypeAnnotation(rNode.Text(e.src))
				}
				e.out.Functions = append(e.out.Functions, parser.ParsedFunction{
					Name:         mname,
					QName:        JoinQName(qname, mname),
					Kind:         graph.KindFunction,
					Receiver:     name,
					ReceiverType: name,
					StartLine:    NodeStartLine(member),
					EndLine:      NodeEndLine(member),
					Params:       params,
					ReturnType:   retType,
					Signature:    formatJSSignature(false, mname, params, retType),
					TypeParams:   e.extractTSTypeParams(member.ChildByFieldName("type_parameters", e.lang)),
					Exported:     true,
					IsMethod:     true,
					Decorators:   []string{"ts:interface_method"},
					Docstring:    LeadingDocComment(member, e.src, e.lang),
				})
			}
		}
	}

	e.out.Classes = append(e.out.Classes, cls)
}

func (e *jstsExtractor) extractTypeAlias(node *gotreesitter.Node, scopeQName string, exported bool, doc string) {
	nameNode := node.ChildByFieldName("name", e.lang)
	if nameNode == nil {
		return
	}
	name := strings.TrimSpace(nameNode.Text(e.src))
	if name == "" {
		return
	}
	var underlying string
	if valNode := node.ChildByFieldName("value", e.lang); valNode != nil {
		underlying = CollapseWhitespace(valNode.Text(e.src))
	}
	e.out.TypeAliases = append(e.out.TypeAliases, parser.ParsedTypeAlias{
		Name:           name,
		QName:          JoinQName(scopeQName, name),
		UnderlyingType: underlying,
		StartLine:      NodeStartLine(node),
		EndLine:        NodeEndLine(node),
		Exported:       exported,
		TypeParams:     e.extractTSTypeParams(node.ChildByFieldName("type_parameters", e.lang)),
		Docstring:      doc,
	})
}

func (e *jstsExtractor) extractEnum(node *gotreesitter.Node, scopeQName string, exported bool, doc string) {
	nameNode := node.ChildByFieldName("name", e.lang)
	if nameNode == nil {
		return
	}
	name := strings.TrimSpace(nameNode.Text(e.src))
	if name == "" {
		return
	}
	qname := JoinQName(scopeQName, name)
	cls := parser.ParsedClass{
		Name:      name,
		QName:     qname,
		Kind:      graph.KindEnum,
		StartLine: NodeStartLine(node),
		EndLine:   NodeEndLine(node),
		Exported:  exported,
		Docstring: doc,
	}
	if body := node.ChildByFieldName("body", e.lang); body != nil {
		for i := 0; i < body.NamedChildCount(); i++ {
			m := body.NamedChild(i)
			if m == nil {
				continue
			}
			var mName string
			switch m.Type(e.lang) {
			case "property_identifier":
				mName = strings.TrimSpace(m.Text(e.src))
			case "enum_assignment":
				if mn := m.ChildByFieldName("name", e.lang); mn != nil {
					mName = strings.TrimSpace(mn.Text(e.src))
				}
			}
			if mName != "" {
				cls.Fields = append(cls.Fields, parser.ParsedField{
					Name:      mName,
					QName:     JoinQName(qname, mName),
					Type:      name,
					StartLine: NodeStartLine(m),
					EndLine:   NodeEndLine(m),
					Exported:  true,
					IsStatic:  true,
				})
			}
		}
	}
	e.out.Classes = append(e.out.Classes, cls)
}

func (e *jstsExtractor) extractClass(clsNode *gotreesitter.Node, scopeQName, callerQName string, exported bool, doc string, decs []string) {
	nameNode := clsNode.ChildByFieldName("name", e.lang)
	name := ""
	if nameNode != nil {
		name = strings.TrimSpace(nameNode.Text(e.src))
	}
	if name == "" {
		name = "<anonymous>"
	}
	qname := JoinQName(scopeQName, name)

	bases, impls := e.extractClassHeritage(clsNode, callerQName)
	cls := parser.ParsedClass{
		Name:       name,
		QName:      qname,
		Kind:       graph.KindClass,
		StartLine:  NodeStartLine(clsNode),
		EndLine:    NodeEndLine(clsNode),
		Exported:   exported,
		Bases:      bases,
		Implements: impls,
		TypeParams: e.extractTSTypeParams(clsNode.ChildByFieldName("type_parameters", e.lang)),
		Decorators: decs,
		Docstring:  doc,
	}

	if body := clsNode.ChildByFieldName("body", e.lang); body != nil {
		var pendingMemberDecs []string
		for i := 0; i < body.NamedChildCount(); i++ {
			m := body.NamedChild(i)
			if m == nil {
				continue
			}
			switch m.Type(e.lang) {
			case "comment":
				continue
			case "decorator":
				if dec := cleanJSTSDecorator(m.Text(e.src)); dec != "" {
					pendingMemberDecs = append(pendingMemberDecs, dec)
				}
				e.collectCalls(m, callerQName)
			case "public_field_definition", "field_definition":
				e.extractClassField(&cls, m, pendingMemberDecs, callerQName)
				pendingMemberDecs = nil
			case "method_definition", "abstract_method_signature":
				e.extractClassMethod(&cls, m, pendingMemberDecs)
				pendingMemberDecs = nil
			case "class_static_block":
				pendingMemberDecs = nil
				e.collectCalls(m, JoinQName(qname, "<static>"))
			default:
				pendingMemberDecs = nil
			}
		}
	}

	e.out.Classes = append(e.out.Classes, cls)
}

func (e *jstsExtractor) extractClassHeritage(clsNode *gotreesitter.Node, callerQName string) (bases, impls []string) {
	for i := 0; i < clsNode.NamedChildCount(); i++ {
		c := clsNode.NamedChild(i)
		if c == nil || c.Type(e.lang) != "class_heritage" {
			continue
		}
		for j := 0; j < c.NamedChildCount(); j++ {
			clause := c.NamedChild(j)
			if clause == nil {
				continue
			}
			switch clause.Type(e.lang) {
			case "extends_clause":
				for k := 0; k < clause.NamedChildCount(); k++ {
					if b := clause.NamedChild(k); b != nil {
						if base := stripTSTypeArgs(b.Text(e.src)); base != "" {
							bases = append(bases, base)
						}
						e.collectCalls(b, callerQName)
					}
				}
			case "implements_clause":
				for k := 0; k < clause.NamedChildCount(); k++ {
					if imp := clause.NamedChild(k); imp != nil {
						if impName := stripTSTypeArgs(imp.Text(e.src)); impName != "" {
							impls = append(impls, impName)
						}
					}
				}
			}
		}
	}
	return bases, impls
}

func (e *jstsExtractor) extractClassField(cls *parser.ParsedClass, fieldNode *gotreesitter.Node, decs []string, callerQName string) {
	nameNode := fieldNode.ChildByFieldName("name", e.lang)
	if nameNode == nil {
		for i := 0; i < fieldNode.NamedChildCount(); i++ {
			c := fieldNode.NamedChild(i)
			if c != nil && (c.Type(e.lang) == "property_identifier" || c.Type(e.lang) == "private_property_identifier") {
				nameNode = c
				break
			}
		}
	}
	if nameNode == nil {
		return
	}
	fname := strings.TrimSpace(nameNode.Text(e.src))
	if fname == "" {
		return
	}
	isPrivate := nameNode.Type(e.lang) == "private_property_identifier" || strings.HasPrefix(fname, "#") || hasAccessibilityKeyword(fieldNode, e.src, "private")
	isStatic := hasUnnamedChildToken(fieldNode, e.src, "static")

	var ftype string
	if typeNode := fieldNode.ChildByFieldName("type", e.lang); typeNode != nil {
		ftype = cleanTSTypeAnnotation(typeNode.Text(e.src))
	}
	fieldDecs := append([]string(nil), decs...)
	if isPrivate {
		fieldDecs = append(fieldDecs, "js:private")
	}

	cls.Fields = append(cls.Fields, parser.ParsedField{
		Name:       fname,
		QName:      JoinQName(cls.QName, fname),
		Type:       ftype,
		StartLine:  NodeStartLine(fieldNode),
		EndLine:    NodeEndLine(fieldNode),
		Exported:   !isPrivate,
		IsStatic:   isStatic,
		Decorators: fieldDecs,
		Docstring:  LeadingDocComment(fieldNode, e.src, e.lang),
	})

	if valNode := fieldNode.ChildByFieldName("value", e.lang); valNode != nil {
		e.collectCalls(valNode, callerQName)
	}
}

func (e *jstsExtractor) extractClassMethod(cls *parser.ParsedClass, mNode *gotreesitter.Node, decs []string) {
	nameNode := mNode.ChildByFieldName("name", e.lang)
	if nameNode == nil {
		return
	}
	mname := strings.TrimSpace(nameNode.Text(e.src))
	if mname == "" {
		return
	}
	isPrivate := nameNode.Type(e.lang) == "private_property_identifier" || strings.HasPrefix(mname, "#") || hasAccessibilityKeyword(mNode, e.src, "private")
	isAsync := hasUnnamedChildToken(mNode, e.src, "async")
	isStatic := hasUnnamedChildToken(mNode, e.src, "static")
	isConstructor := mname == "constructor"

	params, propFields := e.extractJSParams(mNode.ChildByFieldName("parameters", e.lang), mNode)
	if isConstructor && len(propFields) > 0 {
		for _, pf := range propFields {
			pf.QName = JoinQName(cls.QName, pf.Name)
			cls.Fields = append(cls.Fields, pf)
		}
	}

	var retType string
	if rNode := mNode.ChildByFieldName("return_type", e.lang); rNode != nil {
		retType = cleanTSTypeAnnotation(rNode.Text(e.src))
	}
	mDecs := append([]string(nil), decs...)
	if isPrivate {
		mDecs = append(mDecs, "js:private")
	}
	if isStatic {
		mDecs = append(mDecs, "js:static")
	}

	body := mNode.ChildByFieldName("body", e.lang)
	qname := JoinQName(cls.QName, mname)

	e.out.Functions = append(e.out.Functions, parser.ParsedFunction{
		Name:          mname,
		QName:         qname,
		Kind:          graph.KindFunction,
		Receiver:      cls.Name,
		ReceiverType:  cls.Name,
		StartLine:     NodeStartLine(mNode),
		EndLine:       NodeEndLine(mNode),
		Params:        params,
		ReturnType:    retType,
		Signature:     formatJSSignature(isAsync, mname, params, retType),
		TypeParams:    e.extractTSTypeParams(mNode.ChildByFieldName("type_parameters", e.lang)),
		Exported:      !isPrivate,
		IsMethod:      true,
		IsAsync:       isAsync,
		IsConstructor: isConstructor,
		Decorators:    mDecs,
		Docstring:     LeadingDocComment(mNode, e.src, e.lang),
		Complexity:    computeJSComplexity(body, e.lang, e.src),
	})

	if body != nil {
		e.collectCalls(body, qname)
	}
}

func (e *jstsExtractor) extractFunctionDecl(fnNode *gotreesitter.Node, scopeQName string, exported bool, doc string, decs []string) {
	nameNode := fnNode.ChildByFieldName("name", e.lang)
	var name string
	if nameNode != nil {
		name = strings.TrimSpace(nameNode.Text(e.src))
	}
	if name == "" {
		name = "<anonymous>"
	}
	e.extractFunctionValue(fnNode, name, scopeQName, exported, doc, decs)
}

func (e *jstsExtractor) extractFunctionValue(fnNode *gotreesitter.Node, name, scopeQName string, exported bool, doc string, decs []string) {
	qname := JoinQName(scopeQName, name)
	isAsync := hasUnnamedChildToken(fnNode, e.src, "async")
	params, _ := e.extractJSParams(fnNode.ChildByFieldName("parameters", e.lang), fnNode)

	var retType string
	if rNode := fnNode.ChildByFieldName("return_type", e.lang); rNode != nil {
		retType = cleanTSTypeAnnotation(rNode.Text(e.src))
	}
	body := fnNode.ChildByFieldName("body", e.lang)

	e.out.Functions = append(e.out.Functions, parser.ParsedFunction{
		Name:       name,
		QName:      qname,
		Kind:       graph.KindFunction,
		StartLine:  NodeStartLine(fnNode),
		EndLine:    NodeEndLine(fnNode),
		Params:     params,
		ReturnType: retType,
		Signature:  formatJSSignature(isAsync, name, params, retType),
		TypeParams: e.extractTSTypeParams(fnNode.ChildByFieldName("type_parameters", e.lang)),
		Exported:   exported,
		IsAsync:    isAsync,
		Decorators: append([]string(nil), decs...),
		Docstring:  doc,
		Complexity: computeJSComplexity(body, e.lang, e.src),
	})

	if body != nil {
		e.collectCalls(body, qname)
	}
}

func (e *jstsExtractor) extractVariableDecl(decl *gotreesitter.Node, scopeQName, callerQName string, exported bool, doc string) {
	isConst := decl.Type(e.lang) == "lexical_declaration" && hasUnnamedChildToken(decl, e.src, "const")
	line := NodeStartLine(decl)
	endLine := NodeEndLine(decl)

	for i := 0; i < decl.NamedChildCount(); i++ {
		vdecl := decl.NamedChild(i)
		if vdecl == nil || vdecl.Type(e.lang) != "variable_declarator" {
			continue
		}
		nameNode := vdecl.ChildByFieldName("name", e.lang)
		valNode := vdecl.ChildByFieldName("value", e.lang)
		typeNode := vdecl.ChildByFieldName("type", e.lang)

		var typeStr string
		if typeNode != nil {
			typeStr = cleanTSTypeAnnotation(typeNode.Text(e.src))
		}

		// Handle CommonJS: const x = require("..."); or const { a, b: alias } = require("...");
		if valNode != nil && isRequireCall(valNode, e.lang, e.src) {
			e.recordCommonJSRequire(nameNode, valNode, line)
		}

		if valNode != nil && (valNode.Type(e.lang) == "arrow_function" || valNode.Type(e.lang) == "function_expression" || valNode.Type(e.lang) == "generator_function") && nameNode != nil && nameNode.Type(e.lang) == "identifier" {
			fnName := strings.TrimSpace(nameNode.Text(e.src))
			if fnName != "" {
				e.extractFunctionValue(valNode, fnName, scopeQName, exported, doc, nil)
				continue
			}
		}

		if valNode != nil && (valNode.Type(e.lang) == "class" || valNode.Type(e.lang) == "class_declaration") && nameNode != nil && nameNode.Type(e.lang) == "identifier" {
			e.extractClass(valNode, scopeQName, callerQName, exported, doc, nil)
			continue
		}

		targets := collectJSBindingNames(nameNode, e.lang, e.src)
		for _, vname := range targets {
			if vname == "" {
				continue
			}
			e.out.Variables = append(e.out.Variables, parser.ParsedVariable{
				Name:      vname,
				QName:     JoinQName(scopeQName, vname),
				Type:      typeStr,
				StartLine: line,
				EndLine:   endLine,
				Exported:  exported,
				IsConst:   isConst,
				Docstring: doc,
			})
		}

		if valNode != nil {
			if callerQName == e.moduleCaller && !isRequireCall(valNode, e.lang, e.src) {
				e.recordJSTSBindingDeclarator("", typeStr, line, nameNode, valNode)
			}
			e.collectCalls(valNode, callerQName)
		}
	}
}

func (e *jstsExtractor) extractExpressionStatement(stmt *gotreesitter.Node, scopeQName, callerQName string) {
	if stmt.NamedChildCount() == 0 {
		return
	}
	expr := stmt.NamedChild(0)
	if expr == nil {
		return
	}
	if expr.Type(e.lang) == "assignment_expression" {
		left := expr.ChildByFieldName("left", e.lang)
		right := expr.ChildByFieldName("right", e.lang)
		if left != nil && right != nil {
			leftTxt := CollapseWhitespace(left.Text(e.src))
			if leftTxt == "module.exports" {
				switch right.Type(e.lang) {
				case "class", "class_declaration":
					e.extractClass(right, scopeQName, callerQName, true, LeadingDocComment(stmt, e.src, e.lang), nil)
					return
				case "function_expression", "arrow_function":
					e.extractFunctionValue(right, "default", scopeQName, true, LeadingDocComment(stmt, e.src, e.lang), nil)
					return
				case "identifier":
					e.exportedNames[strings.TrimSpace(right.Text(e.src))] = struct{}{}
					return
				case "object":
					for i := 0; i < right.NamedChildCount(); i++ {
						prop := right.NamedChild(i)
						if prop == nil {
							continue
						}
						switch prop.Type(e.lang) {
						case "shorthand_property_identifier":
							e.exportedNames[strings.TrimSpace(prop.Text(e.src))] = struct{}{}
						case "pair":
							keyNode := prop.ChildByFieldName("key", e.lang)
							valNode := prop.ChildByFieldName("value", e.lang)
							if valNode == nil {
								continue
							}
							switch valNode.Type(e.lang) {
							case "identifier":
								e.exportedNames[strings.TrimSpace(valNode.Text(e.src))] = struct{}{}
							case "function_expression", "arrow_function", "generator_function":
								if keyNode != nil {
									if fnName := strings.TrimSpace(keyNode.Text(e.src)); fnName != "" {
										e.extractFunctionValue(valNode, fnName, scopeQName, true, LeadingDocComment(prop, e.src, e.lang), nil)
									}
								}
							default:
								e.collectCalls(valNode, callerQName)
							}
						}
					}
					return
				}
			} else if strings.HasPrefix(leftTxt, "module.exports.") || strings.HasPrefix(leftTxt, "exports.") {
				exportName := leftTxt[strings.LastIndexByte(leftTxt, '.')+1:]
				switch right.Type(e.lang) {
				case "function_expression", "arrow_function", "generator_function":
					e.extractFunctionValue(right, exportName, scopeQName, true, LeadingDocComment(stmt, e.src, e.lang), nil)
					return
				case "class", "class_declaration":
					e.extractClass(right, scopeQName, callerQName, true, LeadingDocComment(stmt, e.src, e.lang), nil)
					return
				case "identifier":
					e.exportedNames[strings.TrimSpace(right.Text(e.src))] = struct{}{}
					return
				}
			}
		}
	}
	e.collectCalls(stmt, callerQName)
}

func isRequireCall(node *gotreesitter.Node, lang *gotreesitter.Language, src []byte) bool {
	if node == nil || node.Type(lang) != "call_expression" {
		return false
	}
	fn := node.ChildByFieldName("function", lang)
	return fn != nil && fn.Type(lang) == "identifier" && strings.TrimSpace(fn.Text(src)) == "require"
}

func (e *jstsExtractor) recordCommonJSRequire(bindingNode, callNode *gotreesitter.Node, line uint32) {
	args := callNode.ChildByFieldName("arguments", e.lang)
	if args == nil || args.NamedChildCount() == 0 {
		return
	}
	firstArg := args.NamedChild(0)
	if firstArg == nil || firstArg.Type(e.lang) != "string" {
		return
	}
	modPath := unquoteJSString(firstArg.Text(e.src))
	if modPath == "" {
		return
	}
	isRel := isJSRelativePath(modPath)

	if bindingNode == nil {
		e.out.Imports = append(e.out.Imports, parser.ParsedImport{
			Path:       modPath,
			Line:       line,
			IsRelative: isRel,
		})
		return
	}

	switch bindingNode.Type(e.lang) {
	case "identifier":
		e.out.Imports = append(e.out.Imports, parser.ParsedImport{
			Path:       modPath,
			Alias:      strings.TrimSpace(bindingNode.Text(e.src)),
			Line:       line,
			IsRelative: isRel,
		})
	case "object_pattern":
		var names []string
		for i := 0; i < bindingNode.NamedChildCount(); i++ {
			child := bindingNode.NamedChild(i)
			if child == nil {
				continue
			}
			switch child.Type(e.lang) {
			case "shorthand_property_identifier_pattern":
				if nm := strings.TrimSpace(child.Text(e.src)); nm != "" {
					names = append(names, nm)
				}
			case "pair_pattern":
				keyNode := child.ChildByFieldName("key", e.lang)
				valNode := child.ChildByFieldName("value", e.lang)
				if keyNode != nil && valNode != nil {
					e.out.Imports = append(e.out.Imports, parser.ParsedImport{
						Path:       modPath,
						Names:      []string{strings.TrimSpace(keyNode.Text(e.src))},
						Alias:      strings.TrimSpace(valNode.Text(e.src)),
						Line:       line,
						IsRelative: isRel,
					})
				}
			}
		}
		if len(names) > 0 || bindingNode.NamedChildCount() == 0 {
			e.out.Imports = append(e.out.Imports, parser.ParsedImport{
				Path:       modPath,
				Names:      names,
				Line:       line,
				IsRelative: isRel,
			})
		}
	default:
		e.out.Imports = append(e.out.Imports, parser.ParsedImport{
			Path:       modPath,
			Line:       line,
			IsRelative: isRel,
		})
	}
}

func collectJSBindingNames(node *gotreesitter.Node, lang *gotreesitter.Language, src []byte) []string {
	if node == nil {
		return nil
	}
	switch node.Type(lang) {
	case "identifier", "shorthand_property_identifier_pattern":
		return []string{strings.TrimSpace(node.Text(src))}
	case "object_pattern", "array_pattern", "rest_pattern":
		var out []string
		for i := 0; i < node.NamedChildCount(); i++ {
			out = append(out, collectJSBindingNames(node.NamedChild(i), lang, src)...)
		}
		return out
	case "pair_pattern", "assignment_pattern":
		if val := node.ChildByFieldName("value", lang); node.Type(lang) == "pair_pattern" && val != nil {
			return collectJSBindingNames(val, lang, src)
		}
		if left := node.ChildByFieldName("left", lang); left != nil {
			return collectJSBindingNames(left, lang, src)
		}
		return nil
	default:
		return nil
	}
}

func (e *jstsExtractor) extractJSParams(paramsNode, ownerFnNode *gotreesitter.Node) (params []graph.Param, propFields []parser.ParsedField) {
	if paramsNode == nil {
		// Arrow function with single unparenthesized parameter: `x => x + 1`
		if ownerFnNode != nil {
			if single := ownerFnNode.ChildByFieldName("parameter", e.lang); single != nil {
				return []graph.Param{{Name: CollapseWhitespace(single.Text(e.src))}}, nil
			}
		}
		return nil, nil
	}

	for i := 0; i < paramsNode.NamedChildCount(); i++ {
		p := paramsNode.NamedChild(i)
		if p == nil {
			continue
		}
		switch p.Type(e.lang) {
		case "identifier":
			params = append(params, graph.Param{Name: strings.TrimSpace(p.Text(e.src))})
		case "rest_pattern":
			params = append(params, graph.Param{Name: CollapseWhitespace(p.Text(e.src))})
		case "assignment_pattern":
			var pName string
			if left := p.ChildByFieldName("left", e.lang); left != nil {
				pName = CollapseWhitespace(left.Text(e.src))
			}
			if pName != "" {
				params = append(params, graph.Param{Name: pName})
			}
		case "required_parameter", "optional_parameter":
			var pName, pType string
			pat := p.ChildByFieldName("pattern", e.lang)
			if pat != nil {
				pName = CollapseWhitespace(pat.Text(e.src))
			}
			if typeNode := p.ChildByFieldName("type", e.lang); typeNode != nil {
				pType = cleanTSTypeAnnotation(typeNode.Text(e.src))
			}
			if pName != "" {
				params = append(params, graph.Param{Name: pName, Type: pType})
				if pat != nil && pat.Type(e.lang) == "identifier" && hasAnyAccessibilityKeyword(p, e.src) {
					isPriv := hasAccessibilityKeyword(p, e.src, "private")
					propFields = append(propFields, parser.ParsedField{
						Name:      pName,
						Type:      pType,
						StartLine: NodeStartLine(p),
						EndLine:   NodeEndLine(p),
						Exported:  !isPriv,
						Docstring: "",
					})
				}
			}
		}
	}
	return params, propFields
}

func (e *jstsExtractor) extractTSTypeParams(tpNode *gotreesitter.Node) []string {
	if tpNode == nil {
		return nil
	}
	var out []string
	for i := 0; i < tpNode.NamedChildCount(); i++ {
		if c := tpNode.NamedChild(i); c != nil {
			if txt := CollapseWhitespace(c.Text(e.src)); txt != "" {
				out = append(out, txt)
			}
		}
	}
	return out
}

// collectCalls traverses expressions and control-flow blocks recursively.
// Nested anonymous callbacks (arrow_function, function_expression) roll their
// call sites into the enclosing callerQName so callback invocations connect directly.
func (e *jstsExtractor) collectCalls(node *gotreesitter.Node, callerQName string) {
	if node == nil {
		return
	}
	switch node.Type(e.lang) {
	case "lexical_declaration", "variable_declaration":
		if callerQName != e.moduleCaller {
			e.recordJSTSLocalVarDecl(node, callerQName)
		}
	case "assignment_expression":
		if callerQName != e.moduleCaller {
			e.recordJSTSLocalAssign(node, callerQName)
		}
	case "call_expression":
		e.recordJSCall(node, callerQName)
	case "new_expression":
		e.recordJSNewCall(node, callerQName)
	case "jsx_opening_element", "jsx_self_closing_element":
		if e.cfg.supportsJSX {
			e.recordJSXComponentCall(node, callerQName)
		}
	}
	count := node.NamedChildCount()
	for i := 0; i < count; i++ {
		e.collectCalls(node.NamedChild(i), callerQName)
	}
}

func (e *jstsExtractor) recordJSTSLocalVarDecl(decl *gotreesitter.Node, scopeQName string) {
	for i := 0; i < decl.NamedChildCount(); i++ {
		vd := decl.NamedChild(i)
		if vd == nil || vd.Type(e.lang) != "variable_declarator" {
			continue
		}
		nameNode := vd.ChildByFieldName("name", e.lang)
		val := vd.ChildByFieldName("value", e.lang)
		var explicitType string
		if tNode := vd.ChildByFieldName("type", e.lang); tNode != nil {
			explicitType = cleanTSTypeAnnotation(tNode.Text(e.src))
		}
		e.recordJSTSBindingDeclarator(scopeQName, explicitType, NodeStartLine(vd), nameNode, val)
	}
}

// recordJSTSBindingDeclarator records simple identifiers and `{ prop, prop: alias }`
// destructured bindings from factory calls into LocalVars.
func (e *jstsExtractor) recordJSTSBindingDeclarator(scopeQName, explicitType string, line uint32, nameNode, valNode *gotreesitter.Node) {
	if nameNode == nil {
		return
	}
	switch nameNode.Type(e.lang) {
	case "identifier":
		name := strings.TrimSpace(nameNode.Text(e.src))
		if name == "" {
			return
		}
		lv := parser.ParsedLocalVar{
			ScopeQName:   scopeQName,
			Name:         name,
			ExplicitType: explicitType,
			Line:         line,
		}
		if valNode != nil {
			e.populateJSTSRHS(&lv, valNode)
		}
		if lv.ExplicitType != "" || lv.InitCallee != "" || lv.InitAlias != "" {
			e.out.LocalVars = append(e.out.LocalVars, lv)
		}
	case "object_pattern":
		if valNode == nil {
			return
		}
		for i := 0; i < nameNode.NamedChildCount(); i++ {
			child := nameNode.NamedChild(i)
			if child == nil {
				continue
			}
			var propName, localName string
			switch child.Type(e.lang) {
			case "shorthand_property_identifier_pattern", "shorthand_property_identifier":
				localName = strings.TrimSpace(child.Text(e.src))
				propName = localName
			case "pair_pattern":
				k := child.ChildByFieldName("key", e.lang)
				v := child.ChildByFieldName("value", e.lang)
				if k != nil && v != nil && v.Type(e.lang) == "identifier" {
					propName = strings.TrimSpace(k.Text(e.src))
					localName = strings.TrimSpace(v.Text(e.src))
				}
			}
			if propName == "" || localName == "" {
				continue
			}
			lv := parser.ParsedLocalVar{
				ScopeQName:       scopeQName,
				Name:             localName,
				DestructuredProp: propName,
				Line:             line,
			}
			e.populateJSTSRHS(&lv, valNode)
			if lv.InitCallee != "" || lv.InitAlias != "" {
				e.out.LocalVars = append(e.out.LocalVars, lv)
			}
		}
	}
}

func (e *jstsExtractor) recordJSTSLocalAssign(expr *gotreesitter.Node, scopeQName string) {
	left := expr.ChildByFieldName("left", e.lang)
	right := expr.ChildByFieldName("right", e.lang)
	if left == nil || right == nil {
		return
	}
	var name string
	switch left.Type(e.lang) {
	case "identifier", "member_expression":
		name = CollapseWhitespace(left.Text(e.src))
	default:
		return
	}
	if name == "" {
		return
	}
	lv := parser.ParsedLocalVar{
		ScopeQName: scopeQName,
		Name:       name,
		Line:       NodeStartLine(expr),
	}
	e.populateJSTSRHS(&lv, right)
	if lv.ExplicitType != "" || lv.InitCallee != "" || lv.InitAlias != "" {
		e.out.LocalVars = append(e.out.LocalVars, lv)
	}
}

func (e *jstsExtractor) populateJSTSRHS(lv *parser.ParsedLocalVar, rhs *gotreesitter.Node) {
	for rhs != nil && (rhs.Type(e.lang) == "await_expression" || rhs.Type(e.lang) == "parenthesized_expression") {
		rhs = rhs.NamedChild(0)
	}
	if rhs == nil {
		return
	}
	switch rhs.Type(e.lang) {
	case "new_expression":
		if ctor := rhs.ChildByFieldName("constructor", e.lang); ctor != nil {
			lv.IsConstructor = true
			switch ctor.Type(e.lang) {
			case "identifier":
				lv.InitCallee = strings.TrimSpace(ctor.Text(e.src))
			case "member_expression":
				if obj := ctor.ChildByFieldName("object", e.lang); obj != nil {
					lv.InitReceiver = CollapseWhitespace(obj.Text(e.src))
				}
				if prop := ctor.ChildByFieldName("property", e.lang); prop != nil {
					lv.InitCallee = strings.TrimSpace(prop.Text(e.src))
				}
			}
		}
	case "call_expression":
		if fn := rhs.ChildByFieldName("function", e.lang); fn != nil {
			switch fn.Type(e.lang) {
			case "identifier":
				lv.InitCallee = strings.TrimSpace(fn.Text(e.src))
			case "member_expression":
				if obj := fn.ChildByFieldName("object", e.lang); obj != nil {
					lv.InitReceiver = CollapseWhitespace(obj.Text(e.src))
				}
				if prop := fn.ChildByFieldName("property", e.lang); prop != nil {
					lv.InitCallee = strings.TrimSpace(prop.Text(e.src))
				}
			}
		}
	case "identifier", "member_expression":
		lv.InitAlias = CollapseWhitespace(rhs.Text(e.src))
	}
}

func (e *jstsExtractor) recordJSCall(callNode *gotreesitter.Node, callerQName string) {
	fnNode := callNode.ChildByFieldName("function", e.lang)
	if fnNode == nil {
		return
	}
	argsNode := callNode.ChildByFieldName("arguments", e.lang)
	argCount := countJSArguments(argsNode, e.lang)

	// Record dynamic `import("./mod")` as both a relative/module import and a dynamic call.
	if fnNode.Type(e.lang) == "import" && argsNode != nil && argsNode.NamedChildCount() > 0 {
		if first := argsNode.NamedChild(0); first != nil && first.Type(e.lang) == "string" {
			if modPath := unquoteJSString(first.Text(e.src)); modPath != "" {
				e.out.Imports = append(e.out.Imports, parser.ParsedImport{
					Path:       modPath,
					Line:       NodeStartLine(callNode),
					IsRelative: isJSRelativePath(modPath),
				})
			}
		}
		e.out.Calls = append(e.out.Calls, parser.ParsedCall{
			CallerQName:   callerQName,
			CalleeName:    "import",
			Line:          NodeStartLine(callNode),
			Col:           NodeStartCol(callNode),
			ArgumentCount: argCount,
			IsDynamic:     true,
		})
		return
	}

	call := parser.ParsedCall{
		CallerQName:   callerQName,
		Line:          NodeStartLine(callNode),
		Col:           NodeStartCol(callNode),
		ArgumentCount: argCount,
	}

	switch fnNode.Type(e.lang) {
	case "identifier", "super":
		call.CalleeName = strings.TrimSpace(fnNode.Text(e.src))
	case "member_expression", "subscript_expression":
		call.IsMethodCall = true
		if obj := fnNode.ChildByFieldName("object", e.lang); obj != nil {
			call.ReceiverName = CollapseWhitespace(obj.Text(e.src))
		}
		if prop := fnNode.ChildByFieldName("property", e.lang); prop != nil {
			call.CalleeName = strings.TrimSpace(prop.Text(e.src))
		} else if idx := fnNode.ChildByFieldName("index", e.lang); idx != nil {
			call.IsDynamic = true
			call.CalleeName = CollapseWhitespace(idx.Text(e.src))
		}
	default:
		call.IsDynamic = true
		call.CalleeName = CollapseWhitespace(fnNode.Text(e.src))
	}

	if call.CalleeName != "" {
		e.out.Calls = append(e.out.Calls, call)
	}
}

func (e *jstsExtractor) recordJSNewCall(newNode *gotreesitter.Node, callerQName string) {
	ctor := newNode.ChildByFieldName("constructor", e.lang)
	if ctor == nil {
		return
	}
	argsNode := newNode.ChildByFieldName("arguments", e.lang)
	call := parser.ParsedCall{
		CallerQName:   callerQName,
		Line:          NodeStartLine(newNode),
		Col:           NodeStartCol(newNode),
		ArgumentCount: countJSArguments(argsNode, e.lang),
		IsConstructor: true,
	}
	switch ctor.Type(e.lang) {
	case "identifier":
		call.CalleeName = strings.TrimSpace(ctor.Text(e.src))
	case "member_expression":
		call.IsMethodCall = true
		if obj := ctor.ChildByFieldName("object", e.lang); obj != nil {
			call.ReceiverName = CollapseWhitespace(obj.Text(e.src))
		}
		if prop := ctor.ChildByFieldName("property", e.lang); prop != nil {
			call.CalleeName = strings.TrimSpace(prop.Text(e.src))
		}
	default:
		call.IsDynamic = true
		call.CalleeName = CollapseWhitespace(ctor.Text(e.src))
	}
	if call.CalleeName != "" {
		e.out.Calls = append(e.out.Calls, call)
	}
}

func (e *jstsExtractor) recordJSXComponentCall(jsxNode *gotreesitter.Node, callerQName string) {
	nameNode := jsxNode.ChildByFieldName("name", e.lang)
	if nameNode == nil && jsxNode.NamedChildCount() > 0 {
		nameNode = jsxNode.NamedChild(0)
	}
	if nameNode == nil {
		return
	}

	switch nameNode.Type(e.lang) {
	case "identifier":
		tag := strings.TrimSpace(nameNode.Text(e.src))
		r, _ := utf8.DecodeRuneInString(tag)
		if !unicode.IsUpper(r) {
			return // Intrinsic HTML/SVG element like <div> or <span>
		}
		e.out.Calls = append(e.out.Calls, parser.ParsedCall{
			CallerQName:   callerQName,
			CalleeName:    tag,
			Line:          NodeStartLine(jsxNode),
			Col:           NodeStartCol(jsxNode),
			IsConstructor: false,
		})
	case "member_expression":
		var recv, callee string
		if obj := nameNode.ChildByFieldName("object", e.lang); obj != nil {
			recv = CollapseWhitespace(obj.Text(e.src))
		}
		if prop := nameNode.ChildByFieldName("property", e.lang); prop != nil {
			callee = strings.TrimSpace(prop.Text(e.src))
		}
		if callee != "" {
			e.out.Calls = append(e.out.Calls, parser.ParsedCall{
				CallerQName:  callerQName,
				CalleeName:   callee,
				ReceiverName: recv,
				Line:         NodeStartLine(jsxNode),
				Col:          NodeStartCol(jsxNode),
				IsMethodCall: true,
			})
		}
	}
}

func countJSArguments(argsNode *gotreesitter.Node, lang *gotreesitter.Language) uint16 {
	if argsNode == nil {
		return 0
	}
	var count uint32
	n := argsNode.NamedChildCount()
	for i := 0; i < n; i++ {
		if c := argsNode.NamedChild(i); c != nil && c.Type(lang) != "comment" {
			count++
		}
	}
	if count > 0xffff {
		return 0xffff
	}
	return uint16(count)
}

func computeJSComplexity(body *gotreesitter.Node, lang *gotreesitter.Language, src []byte) uint16 {
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
		case "if_statement", "for_statement", "for_in_statement", "while_statement", "do_statement", "switch_case", "catch_clause", "ternary_expression":
			complexity++
		case "binary_expression":
			if op := n.ChildByFieldName("operator", lang); op != nil {
				opTxt := strings.TrimSpace(op.Text(src))
				if opTxt == "&&" || opTxt == "||" || opTxt == "??" {
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

func cleanTSTypeAnnotation(raw string) string {
	s := CollapseWhitespace(raw)
	return strings.TrimSpace(strings.TrimPrefix(s, ":"))
}

func stripTSTypeArgs(raw string) string {
	s := CollapseWhitespace(raw)
	if idx := strings.IndexByte(s, '<'); idx >= 0 {
		s = s[:idx]
	}
	return strings.TrimSpace(s)
}

func unquoteJSString(raw string) string {
	s := strings.TrimSpace(raw)
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') || (s[0] == '`' && s[len(s)-1] == '`') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

func isJSRelativePath(p string) bool {
	return strings.HasPrefix(p, "./") || strings.HasPrefix(p, "../") || p == "." || p == ".."
}

func hasUnnamedChildToken(node *gotreesitter.Node, src []byte, token string) bool {
	if node == nil {
		return false
	}
	count := node.ChildCount()
	for i := 0; i < count; i++ {
		c := node.Child(i)
		if c != nil && !c.IsNamed() && strings.TrimSpace(c.Text(src)) == token {
			return true
		}
	}
	return false
}

func hasAccessibilityKeyword(node *gotreesitter.Node, src []byte, modifier string) bool {
	if node == nil {
		return false
	}
	for i := 0; i < node.ChildCount(); i++ {
		c := node.Child(i)
		if c != nil && strings.TrimSpace(c.Text(src)) == modifier {
			return true
		}
	}
	return false
}

func hasAnyAccessibilityKeyword(node *gotreesitter.Node, src []byte) bool {
	return hasAccessibilityKeyword(node, src, "public") ||
		hasAccessibilityKeyword(node, src, "private") ||
		hasAccessibilityKeyword(node, src, "protected") ||
		hasAccessibilityKeyword(node, src, "readonly")
}

func formatJSSignature(isAsync bool, name string, params []graph.Param, ret string) string {
	var b strings.Builder
	if isAsync {
		b.WriteString("async ")
	}
	b.WriteString(name)
	b.WriteByte('(')
	for i, p := range params {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(p.Name)
		if p.Type != "" {
			b.WriteString(": ")
			b.WriteString(p.Type)
		}
	}
	b.WriteByte(')')
	if ret != "" {
		b.WriteString(": ")
		b.WriteString(ret)
	}
	return b.String()
}
