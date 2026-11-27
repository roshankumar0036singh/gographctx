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

// JavaParser extracts structural symbols, annotations, generics, inner and
// anonymous classes, lambdas, imports, and call sites from Java (.java) files.
type JavaParser struct {
	mgr *treesitter.Manager
}

// NewJavaParser constructs a Java LanguageParser backed by mgr.
func NewJavaParser(mgr *treesitter.Manager) (*JavaParser, error) {
	if mgr == nil {
		return nil, ErrNilTreeSitterManager
	}
	return &JavaParser{mgr: mgr}, nil
}

// Language returns the canonical language name ("java").
func (p *JavaParser) Language() string {
	return treesitter.LangJava
}

// Extensions returns the file extensions handled by JavaParser ([".java"]).
func (p *JavaParser) Extensions() []string {
	return treesitter.ExtensionsForLanguage(treesitter.LangJava)
}

// Parse parses a Java source file in a single AST pass and returns a ParsedFile.
func (p *JavaParser) Parse(ctx context.Context, path string, source []byte) (*parser.ParsedFile, error) {
	if p == nil || p.mgr == nil {
		return nil, ErrNilTreeSitterManager
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	pt, err := p.mgr.ParseContext(ctx, treesitter.LangJava, source)
	if err != nil {
		return nil, err
	}
	defer pt.Release()

	root := pt.RootNode()
	lang := pt.Grammar

	out := &parser.ParsedFile{
		Path:        path,
		Language:    treesitter.LangJava,
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

	pkg := extractJavaPackage(root, source, lang)
	out.Package = pkg
	scope := pkg
	if scope == "" {
		scope = deriveJavaFallbackScope(path)
	}

	ext := &javaExtractor{
		path:  path,
		scope: scope,
		src:   source,
		lang:  lang,
		out:   out,
	}
	ext.walkProgram(root)

	return out, nil
}

func extractJavaPackage(root *gotreesitter.Node, src []byte, lang *gotreesitter.Language) string {
	for i := 0; i < root.NamedChildCount(); i++ {
		c := root.NamedChild(i)
		if c == nil || c.Type(lang) != "package_declaration" {
			continue
		}
		for j := 0; j < c.NamedChildCount(); j++ {
			nc := c.NamedChild(j)
			if nc == nil {
				continue
			}
			if nc.Type(lang) == "scoped_identifier" || nc.Type(lang) == "identifier" {
				return CollapseWhitespace(nc.Text(src))
			}
		}
	}
	return ""
}

func deriveJavaFallbackScope(path string) string {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return ""
	}
	slash := filepath.ToSlash(filepath.Clean(trimmed))
	dir := filepath.ToSlash(filepath.Dir(slash))
	if dir == "." || dir == "/" || filepath.IsAbs(trimmed) || strings.HasPrefix(slash, "/") || strings.HasPrefix(dir, "..") {
		return strings.TrimSuffix(filepath.Base(slash), filepath.Ext(slash))
	}
	return strings.ReplaceAll(dir, "/", ".")
}

type javaExtractor struct {
	path  string
	scope string
	src   []byte
	lang  *gotreesitter.Language
	out   *parser.ParsedFile
}

func (e *javaExtractor) walkProgram(root *gotreesitter.Node) {
	for i := 0; i < root.NamedChildCount(); i++ {
		node := root.NamedChild(i)
		if node == nil {
			continue
		}
		switch node.Type(e.lang) {
		case "import_declaration":
			e.extractImport(node)
		case "class_declaration":
			e.extractClass(node, e.scope)
		case "interface_declaration":
			e.extractInterface(node, e.scope)
		case "enum_declaration":
			e.extractEnum(node, e.scope)
		case "record_declaration":
			e.extractRecord(node, e.scope)
		case "annotation_type_declaration":
			e.extractAnnotationType(node, e.scope)
		}
	}
}

func (e *javaExtractor) extractImport(stmt *gotreesitter.Node) {
	line := NodeStartLine(stmt)
	isWildcard := hasJavaToken(stmt, e.lang, e.src, "asterisk") || strings.Contains(stmt.Text(e.src), ".*")

	var target string
	for i := 0; i < stmt.NamedChildCount(); i++ {
		c := stmt.NamedChild(i)
		if c == nil {
			continue
		}
		if c.Type(e.lang) == "scoped_identifier" || c.Type(e.lang) == "identifier" {
			target = CollapseWhitespace(c.Text(e.src))
			break
		}
	}
	if target == "" {
		return
	}

	if isWildcard {
		e.out.Imports = append(e.out.Imports, parser.ParsedImport{
			Path:       target,
			Line:       line,
			IsWildcard: true,
		})
		return
	}

	if idx := strings.LastIndexByte(target, '.'); idx >= 0 {
		e.out.Imports = append(e.out.Imports, parser.ParsedImport{
			Path:  target[:idx],
			Names: []string{target[idx+1:]},
			Line:  line,
		})
		return
	}

	e.out.Imports = append(e.out.Imports, parser.ParsedImport{
		Path: target,
		Line: line,
	})
}

func (e *javaExtractor) extractClass(node *gotreesitter.Node, scopeQName string) {
	nameNode := node.ChildByFieldName("name", e.lang)
	if nameNode == nil {
		return
	}
	name := strings.TrimSpace(nameNode.Text(e.src))
	if name == "" {
		return
	}
	qname := JoinQName(scopeQName, name)
	decs, mods := e.extractModifiers(node)

	var bases, impls []string
	if sc := node.ChildByFieldName("superclass", e.lang); sc != nil {
		for i := 0; i < sc.NamedChildCount(); i++ {
			if base := stripJavaGenerics(sc.NamedChild(i).Text(e.src)); base != "" {
				bases = append(bases, base)
			}
		}
	}
	if si := node.ChildByFieldName("interfaces", e.lang); si != nil {
		impls = e.extractJavaTypeList(si)
	}
	// Fallback scan for superclass/super_interfaces child types.
	if len(bases) == 0 || len(impls) == 0 {
		for i := 0; i < node.NamedChildCount(); i++ {
			c := node.NamedChild(i)
			if c == nil {
				continue
			}
			switch c.Type(e.lang) {
			case "superclass":
				if len(bases) == 0 {
					for j := 0; j < c.NamedChildCount(); j++ {
						if base := stripJavaGenerics(c.NamedChild(j).Text(e.src)); base != "" {
							bases = append(bases, base)
						}
					}
				}
			case "super_interfaces":
				if len(impls) == 0 {
					impls = e.extractJavaTypeList(c)
				}
			}
		}
	}

	cls := parser.ParsedClass{
		Name:       name,
		QName:      qname,
		Kind:       graph.KindClass,
		StartLine:  NodeStartLine(node),
		EndLine:    NodeEndLine(node),
		Exported:   isJavaExported(mods),
		Bases:      bases,
		Implements: impls,
		TypeParams: e.extractJavaTypeParams(node.ChildByFieldName("type_parameters", e.lang)),
		Decorators: decs,
		Docstring:  LeadingDocComment(node, e.src, e.lang),
	}

	if body := node.ChildByFieldName("body", e.lang); body != nil {
		e.walkTypeBody(&cls, body, false)
	}
	e.out.Classes = append(e.out.Classes, cls)
}

func (e *javaExtractor) extractInterface(node *gotreesitter.Node, scopeQName string) {
	nameNode := node.ChildByFieldName("name", e.lang)
	if nameNode == nil {
		return
	}
	name := strings.TrimSpace(nameNode.Text(e.src))
	if name == "" {
		return
	}
	qname := JoinQName(scopeQName, name)
	decs, mods := e.extractModifiers(node)

	var bases []string
	for i := 0; i < node.NamedChildCount(); i++ {
		c := node.NamedChild(i)
		if c != nil && c.Type(e.lang) == "extends_interfaces" {
			bases = append(bases, e.extractJavaTypeList(c)...)
		}
	}

	cls := parser.ParsedClass{
		Name:       name,
		QName:      qname,
		Kind:       graph.KindInterface,
		StartLine:  NodeStartLine(node),
		EndLine:    NodeEndLine(node),
		Exported:   isJavaExported(mods),
		Bases:      bases,
		TypeParams: e.extractJavaTypeParams(node.ChildByFieldName("type_parameters", e.lang)),
		Decorators: decs,
		Docstring:  LeadingDocComment(node, e.src, e.lang),
	}

	if body := node.ChildByFieldName("body", e.lang); body != nil {
		e.walkTypeBody(&cls, body, true)
	}
	e.out.Classes = append(e.out.Classes, cls)
}

func (e *javaExtractor) extractEnum(node *gotreesitter.Node, scopeQName string) {
	nameNode := node.ChildByFieldName("name", e.lang)
	if nameNode == nil {
		return
	}
	name := strings.TrimSpace(nameNode.Text(e.src))
	if name == "" {
		return
	}
	qname := JoinQName(scopeQName, name)
	decs, mods := e.extractModifiers(node)

	var impls []string
	for i := 0; i < node.NamedChildCount(); i++ {
		if c := node.NamedChild(i); c != nil && c.Type(e.lang) == "super_interfaces" {
			impls = e.extractJavaTypeList(c)
		}
	}

	cls := parser.ParsedClass{
		Name:       name,
		QName:      qname,
		Kind:       graph.KindEnum,
		StartLine:  NodeStartLine(node),
		EndLine:    NodeEndLine(node),
		Exported:   isJavaExported(mods),
		Implements: impls,
		Decorators: decs,
		Docstring:  LeadingDocComment(node, e.src, e.lang),
	}

	if body := node.ChildByFieldName("body", e.lang); body != nil {
		for i := 0; i < body.NamedChildCount(); i++ {
			c := body.NamedChild(i)
			if c == nil {
				continue
			}
			switch c.Type(e.lang) {
			case "enum_constant":
				cn := c.ChildByFieldName("name", e.lang)
				if cn == nil {
					continue
				}
				cname := strings.TrimSpace(cn.Text(e.src))
				if cname == "" {
					continue
				}
				cDecs, _ := e.extractModifiers(c)
				cls.Fields = append(cls.Fields, parser.ParsedField{
					Name:       cname,
					QName:      JoinQName(qname, cname),
					Type:       name,
					StartLine:  NodeStartLine(c),
					EndLine:    NodeEndLine(c),
					Exported:   cls.Exported,
					IsStatic:   true,
					Decorators: cDecs,
					Docstring:  LeadingDocComment(c, e.src, e.lang),
				})
				e.collectCalls(c, JoinQName(qname, cname))
			case "enum_body_declarations":
				e.walkTypeBody(&cls, c, false)
			}
		}
	}

	e.out.Classes = append(e.out.Classes, cls)
}

func (e *javaExtractor) extractRecord(node *gotreesitter.Node, scopeQName string) {
	nameNode := node.ChildByFieldName("name", e.lang)
	if nameNode == nil {
		return
	}
	name := strings.TrimSpace(nameNode.Text(e.src))
	if name == "" {
		return
	}
	qname := JoinQName(scopeQName, name)
	decs, mods := e.extractModifiers(node)
	decs = append(decs, "java:record")

	var impls []string
	for i := 0; i < node.NamedChildCount(); i++ {
		if c := node.NamedChild(i); c != nil && c.Type(e.lang) == "super_interfaces" {
			impls = e.extractJavaTypeList(c)
		}
	}

	cls := parser.ParsedClass{
		Name:       name,
		QName:      qname,
		Kind:       graph.KindClass,
		StartLine:  NodeStartLine(node),
		EndLine:    NodeEndLine(node),
		Exported:   isJavaExported(mods),
		Implements: impls,
		TypeParams: e.extractJavaTypeParams(node.ChildByFieldName("type_parameters", e.lang)),
		Decorators: decs,
		Docstring:  LeadingDocComment(node, e.src, e.lang),
	}

	for _, param := range e.extractJavaParams(node.ChildByFieldName("parameters", e.lang)) {
		if param.Name == "" {
			continue
		}
		cls.Fields = append(cls.Fields, parser.ParsedField{
			Name:      param.Name,
			QName:     JoinQName(qname, param.Name),
			Type:      param.Type,
			StartLine: cls.StartLine,
			EndLine:   cls.StartLine,
			Exported:  cls.Exported,
		})
	}

	if body := node.ChildByFieldName("body", e.lang); body != nil {
		e.walkTypeBody(&cls, body, false)
	}
	e.out.Classes = append(e.out.Classes, cls)
}

func (e *javaExtractor) extractAnnotationType(node *gotreesitter.Node, scopeQName string) {
	nameNode := node.ChildByFieldName("name", e.lang)
	if nameNode == nil {
		return
	}
	name := strings.TrimSpace(nameNode.Text(e.src))
	if name == "" {
		return
	}
	decs, mods := e.extractModifiers(node)
	decs = append(decs, "java:annotation")

	cls := parser.ParsedClass{
		Name:       name,
		QName:      JoinQName(scopeQName, name),
		Kind:       graph.KindInterface,
		StartLine:  NodeStartLine(node),
		EndLine:    NodeEndLine(node),
		Exported:   isJavaExported(mods),
		Decorators: decs,
		Docstring:  LeadingDocComment(node, e.src, e.lang),
	}
	if body := node.ChildByFieldName("body", e.lang); body != nil {
		e.walkTypeBody(&cls, body, true)
	}
	e.out.Classes = append(e.out.Classes, cls)
}

func (e *javaExtractor) walkTypeBody(cls *parser.ParsedClass, body *gotreesitter.Node, isInterface bool) {
	for i := 0; i < body.NamedChildCount(); i++ {
		m := body.NamedChild(i)
		if m == nil {
			continue
		}
		switch m.Type(e.lang) {
		case "field_declaration":
			e.extractField(cls, m, isInterface)
		case "constant_declaration":
			e.extractField(cls, m, true)
		case "method_declaration":
			e.extractMethod(cls, m, isInterface)
		case "constructor_declaration", "compact_constructor_declaration":
			e.extractConstructor(cls, m)
		case "class_declaration":
			e.extractClass(m, cls.QName)
		case "interface_declaration":
			e.extractInterface(m, cls.QName)
		case "enum_declaration":
			e.extractEnum(m, cls.QName)
		case "record_declaration":
			e.extractRecord(m, cls.QName)
		case "annotation_type_declaration":
			e.extractAnnotationType(m, cls.QName)
		case "static_initializer", "block":
			e.collectCalls(m, JoinQName(cls.QName, "<static_init>"))
		}
	}
}

func (e *javaExtractor) extractField(cls *parser.ParsedClass, fieldNode *gotreesitter.Node, isInterface bool) {
	decs, mods := e.extractModifiers(fieldNode)
	var typeStr string
	if tNode := fieldNode.ChildByFieldName("type", e.lang); tNode != nil {
		typeStr = CollapseWhitespace(tNode.Text(e.src))
	}
	doc := LeadingDocComment(fieldNode, e.src, e.lang)
	isStatic := isInterface || mods["static"]
	exported := isInterface || isJavaExported(mods)

	for i := 0; i < fieldNode.NamedChildCount(); i++ {
		c := fieldNode.NamedChild(i)
		if c == nil || c.Type(e.lang) != "variable_declarator" {
			continue
		}
		nameNode := c.ChildByFieldName("name", e.lang)
		if nameNode == nil {
			continue
		}
		fname := strings.TrimSpace(nameNode.Text(e.src))
		if fname == "" {
			continue
		}
		cls.Fields = append(cls.Fields, parser.ParsedField{
			Name:       fname,
			QName:      JoinQName(cls.QName, fname),
			Type:       typeStr,
			StartLine:  NodeStartLine(fieldNode),
			EndLine:    NodeEndLine(fieldNode),
			Exported:   exported,
			IsStatic:   isStatic,
			Decorators: append([]string(nil), decs...),
			Docstring:  doc,
		})
		if valNode := c.ChildByFieldName("value", e.lang); valNode != nil {
			e.collectCalls(valNode, JoinQName(cls.QName, fname))
		}
	}
}

func (e *javaExtractor) extractMethod(cls *parser.ParsedClass, mNode *gotreesitter.Node, isInterface bool) {
	nameNode := mNode.ChildByFieldName("name", e.lang)
	if nameNode == nil {
		return
	}
	name := strings.TrimSpace(nameNode.Text(e.src))
	if name == "" {
		return
	}

	decs, mods := e.extractModifiers(mNode)
	if isInterface && !mods["default"] && !mods["static"] {
		decs = append(decs, "java:interface_method")
	}
	if mods["static"] {
		decs = append(decs, "java:static")
	}

	var retType string
	if tNode := mNode.ChildByFieldName("type", e.lang); tNode != nil {
		retType = CollapseWhitespace(tNode.Text(e.src))
	}
	params := e.extractJavaParams(mNode.ChildByFieldName("parameters", e.lang))
	typeParams := e.extractJavaTypeParams(mNode.ChildByFieldName("type_parameters", e.lang))
	body := mNode.ChildByFieldName("body", e.lang)
	qname := JoinQName(cls.QName, name)

	exported := isJavaExported(mods)
	if isInterface && !mods["private"] {
		exported = true
	}

	e.out.Functions = append(e.out.Functions, parser.ParsedFunction{
		Name:         name,
		QName:        qname,
		Kind:         graph.KindFunction,
		Receiver:     cls.Name,
		ReceiverType: cls.Name,
		StartLine:    NodeStartLine(mNode),
		EndLine:      NodeEndLine(mNode),
		Params:       params,
		ReturnType:   retType,
		Signature:    formatJavaSignature(name, params, retType),
		TypeParams:   typeParams,
		Exported:     exported,
		IsMethod:     true,
		Decorators:   decs,
		Docstring:    LeadingDocComment(mNode, e.src, e.lang),
		Complexity:   computeJavaComplexity(body, e.lang, e.src),
	})

	if body != nil {
		e.collectCalls(body, qname)
	}
}

func (e *javaExtractor) extractConstructor(cls *parser.ParsedClass, cNode *gotreesitter.Node) {
	nameNode := cNode.ChildByFieldName("name", e.lang)
	name := cls.Name
	if nameNode != nil {
		if txt := strings.TrimSpace(nameNode.Text(e.src)); txt != "" {
			name = txt
		}
	}
	decs, mods := e.extractModifiers(cNode)
	params := e.extractJavaParams(cNode.ChildByFieldName("parameters", e.lang))
	body := cNode.ChildByFieldName("body", e.lang)
	qname := JoinQName(cls.QName, name)

	e.out.Functions = append(e.out.Functions, parser.ParsedFunction{
		Name:          name,
		QName:         qname,
		Kind:          graph.KindFunction,
		Receiver:      cls.Name,
		ReceiverType:  cls.Name,
		StartLine:     NodeStartLine(cNode),
		EndLine:       NodeEndLine(cNode),
		Params:        params,
		ReturnType:    cls.Name,
		Signature:     formatJavaSignature(name, params, ""),
		TypeParams:    e.extractJavaTypeParams(cNode.ChildByFieldName("type_parameters", e.lang)),
		Exported:      isJavaExported(mods),
		IsMethod:      true,
		IsConstructor: true,
		Decorators:    decs,
		Docstring:     LeadingDocComment(cNode, e.src, e.lang),
		Complexity:    computeJavaComplexity(body, e.lang, e.src),
	})

	if body != nil {
		e.collectCalls(body, qname)
	}
}

func (e *javaExtractor) extractJavaParams(paramsNode *gotreesitter.Node) []graph.Param {
	if paramsNode == nil {
		return nil
	}
	var out []graph.Param
	for i := 0; i < paramsNode.NamedChildCount(); i++ {
		p := paramsNode.NamedChild(i)
		if p == nil {
			continue
		}
		switch p.Type(e.lang) {
		case "formal_parameter":
			var pName, pType string
			if tNode := p.ChildByFieldName("type", e.lang); tNode != nil {
				pType = CollapseWhitespace(tNode.Text(e.src))
			}
			if nNode := p.ChildByFieldName("name", e.lang); nNode != nil {
				pName = strings.TrimSpace(nNode.Text(e.src))
			}
			if pName != "" || pType != "" {
				out = append(out, graph.Param{Name: pName, Type: pType})
			}
		case "spread_parameter":
			var pType, pName string
			for j := 0; j < p.NamedChildCount(); j++ {
				c := p.NamedChild(j)
				if c == nil {
					continue
				}
				switch c.Type(e.lang) {
				case "variable_declarator":
					if n := c.ChildByFieldName("name", e.lang); n != nil {
						pName = strings.TrimSpace(n.Text(e.src))
					}
				case "identifier":
					pName = strings.TrimSpace(c.Text(e.src))
				default:
					if pType == "" {
						pType = CollapseWhitespace(c.Text(e.src)) + "..."
					}
				}
			}
			if pName != "" || pType != "" {
				out = append(out, graph.Param{Name: pName, Type: pType})
			}
		}
	}
	return out
}

func (e *javaExtractor) extractJavaTypeParams(tpNode *gotreesitter.Node) []string {
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

func (e *javaExtractor) extractJavaTypeList(clauseNode *gotreesitter.Node) []string {
	if clauseNode == nil {
		return nil
	}
	var out []string
	for i := 0; i < clauseNode.NamedChildCount(); i++ {
		c := clauseNode.NamedChild(i)
		if c == nil {
			continue
		}
		if c.Type(e.lang) == "type_list" {
			for j := 0; j < c.NamedChildCount(); j++ {
				if item := c.NamedChild(j); item != nil {
					if name := stripJavaGenerics(item.Text(e.src)); name != "" {
						out = append(out, name)
					}
				}
			}
		} else if name := stripJavaGenerics(c.Text(e.src)); name != "" {
			out = append(out, name)
		}
	}
	return out
}

func (e *javaExtractor) extractModifiers(node *gotreesitter.Node) (decorators []string, flags map[string]bool) {
	flags = make(map[string]bool)
	for i := 0; i < node.NamedChildCount(); i++ {
		m := node.NamedChild(i)
		if m == nil || m.Type(e.lang) != "modifiers" {
			continue
		}
		for j := 0; j < m.ChildCount(); j++ {
			c := m.Child(j)
			if c == nil {
				continue
			}
			switch c.Type(e.lang) {
			case "annotation", "marker_annotation":
				dec := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(c.Text(e.src)), "@"))
				if dec != "" {
					decorators = append(decorators, CollapseWhitespace(dec))
				}
			default:
				if !c.IsNamed() {
					kw := strings.TrimSpace(c.Text(e.src))
					if kw != "" {
						flags[kw] = true
					}
				}
			}
		}
	}
	return decorators, flags
}

// collectCalls walks method/constructor bodies recursively. Calls inside
// lambda expressions and anonymous class bodies intentionally roll into the
// enclosing callerQName so inline callbacks connect directly in the call graph.
func (e *javaExtractor) collectCalls(node *gotreesitter.Node, callerQName string) {
	if node == nil {
		return
	}
	switch node.Type(e.lang) {
	case "local_variable_declaration":
		e.recordJavaLocalVar(node, callerQName)
	case "assignment_expression":
		e.recordJavaLocalAssign(node, callerQName)
	case "method_invocation":
		e.recordJavaMethodCall(node, callerQName)
	case "object_creation_expression":
		e.recordJavaConstructorCall(node, callerQName)
	case "explicit_constructor_invocation":
		e.recordJavaExplicitCtorCall(node, callerQName)
	case "method_reference":
		e.recordJavaMethodRef(node, callerQName)
	}
	count := node.NamedChildCount()
	for i := 0; i < count; i++ {
		e.collectCalls(node.NamedChild(i), callerQName)
	}
}

func (e *javaExtractor) recordJavaLocalVar(decl *gotreesitter.Node, scopeQName string) {
	var typeStr string
	if tNode := decl.ChildByFieldName("type", e.lang); tNode != nil {
		t := CollapseWhitespace(tNode.Text(e.src))
		if t != "var" {
			typeStr = t
		}
	}
	for i := 0; i < decl.NamedChildCount(); i++ {
		vd := decl.NamedChild(i)
		if vd == nil || vd.Type(e.lang) != "variable_declarator" {
			continue
		}
		nameNode := vd.ChildByFieldName("name", e.lang)
		if nameNode == nil {
			continue
		}
		name := strings.TrimSpace(nameNode.Text(e.src))
		if name == "" {
			continue
		}
		lv := parser.ParsedLocalVar{
			ScopeQName:   scopeQName,
			Name:         name,
			ExplicitType: typeStr,
			Line:         NodeStartLine(decl),
		}
		if val := vd.ChildByFieldName("value", e.lang); val != nil {
			e.populateJavaRHS(&lv, val)
		}
		if lv.ExplicitType != "" || lv.InitCallee != "" || lv.InitAlias != "" {
			e.out.LocalVars = append(e.out.LocalVars, lv)
		}
	}
}

func (e *javaExtractor) recordJavaLocalAssign(expr *gotreesitter.Node, scopeQName string) {
	left := expr.ChildByFieldName("left", e.lang)
	right := expr.ChildByFieldName("right", e.lang)
	if left == nil || right == nil {
		return
	}
	var name string
	switch left.Type(e.lang) {
	case "identifier", "field_access":
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
	e.populateJavaRHS(&lv, right)
	if lv.ExplicitType != "" || lv.InitCallee != "" || lv.InitAlias != "" {
		e.out.LocalVars = append(e.out.LocalVars, lv)
	}
}

func (e *javaExtractor) populateJavaRHS(lv *parser.ParsedLocalVar, rhs *gotreesitter.Node) {
	if rhs == nil {
		return
	}
	switch rhs.Type(e.lang) {
	case "object_creation_expression":
		if t := rhs.ChildByFieldName("type", e.lang); t != nil {
			lv.InitCallee = stripJavaGenerics(t.Text(e.src))
			lv.IsConstructor = true
		}
	case "method_invocation":
		if obj := rhs.ChildByFieldName("object", e.lang); obj != nil {
			lv.InitReceiver = CollapseWhitespace(obj.Text(e.src))
		}
		if nm := rhs.ChildByFieldName("name", e.lang); nm != nil {
			lv.InitCallee = strings.TrimSpace(nm.Text(e.src))
		}
	case "identifier", "field_access":
		lv.InitAlias = CollapseWhitespace(rhs.Text(e.src))
	}
}

func (e *javaExtractor) recordJavaMethodCall(callNode *gotreesitter.Node, callerQName string) {
	nameNode := callNode.ChildByFieldName("name", e.lang)
	if nameNode == nil {
		return
	}
	callee := strings.TrimSpace(nameNode.Text(e.src))
	if callee == "" {
		return
	}
	var recv string
	isMethod := false
	if obj := callNode.ChildByFieldName("object", e.lang); obj != nil {
		recv = CollapseWhitespace(obj.Text(e.src))
		isMethod = recv != ""
	}
	argsNode := callNode.ChildByFieldName("arguments", e.lang)

	e.out.Calls = append(e.out.Calls, parser.ParsedCall{
		CallerQName:   callerQName,
		CalleeName:    callee,
		ReceiverName:  recv,
		Line:          NodeStartLine(callNode),
		Col:           NodeStartCol(callNode),
		ArgumentCount: countJavaArguments(argsNode, e.lang),
		IsMethodCall:  isMethod,
	})
}

func (e *javaExtractor) recordJavaConstructorCall(newNode *gotreesitter.Node, callerQName string) {
	typeNode := newNode.ChildByFieldName("type", e.lang)
	if typeNode == nil {
		return
	}
	callee := stripJavaGenerics(typeNode.Text(e.src))
	if callee == "" {
		return
	}
	argsNode := newNode.ChildByFieldName("arguments", e.lang)
	e.out.Calls = append(e.out.Calls, parser.ParsedCall{
		CallerQName:   callerQName,
		CalleeName:    callee,
		Line:          NodeStartLine(newNode),
		Col:           NodeStartCol(newNode),
		ArgumentCount: countJavaArguments(argsNode, e.lang),
		IsConstructor: true,
	})
}

func (e *javaExtractor) recordJavaExplicitCtorCall(node *gotreesitter.Node, callerQName string) {
	callee := "super"
	if hasJavaToken(node, e.lang, e.src, "this") {
		callee = "this"
	}
	argsNode := node.ChildByFieldName("arguments", e.lang)
	e.out.Calls = append(e.out.Calls, parser.ParsedCall{
		CallerQName:   callerQName,
		CalleeName:    callee,
		Line:          NodeStartLine(node),
		Col:           NodeStartCol(node),
		ArgumentCount: countJavaArguments(argsNode, e.lang),
		IsConstructor: true,
	})
}

func (e *javaExtractor) recordJavaMethodRef(node *gotreesitter.Node, callerQName string) {
	if node.NamedChildCount() == 0 {
		return
	}
	recv := CollapseWhitespace(node.NamedChild(0).Text(e.src))
	if node.NamedChildCount() == 1 && hasJavaToken(node, e.lang, e.src, "new") {
		e.out.Calls = append(e.out.Calls, parser.ParsedCall{
			CallerQName:   callerQName,
			CalleeName:    "new",
			ReceiverName:  recv,
			Line:          NodeStartLine(node),
			Col:           NodeStartCol(node),
			IsMethodCall:  recv != "",
			IsConstructor: true,
		})
		return
	}
	if node.NamedChildCount() < 2 {
		return
	}
	callee := strings.TrimSpace(node.NamedChild(node.NamedChildCount() - 1).Text(e.src))
	if callee == "" {
		return
	}
	e.out.Calls = append(e.out.Calls, parser.ParsedCall{
		CallerQName:  callerQName,
		CalleeName:   callee,
		ReceiverName: recv,
		Line:         NodeStartLine(node),
		Col:          NodeStartCol(node),
		IsMethodCall: recv != "",
	})
}

func countJavaArguments(argsNode *gotreesitter.Node, lang *gotreesitter.Language) uint16 {
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

func computeJavaComplexity(body *gotreesitter.Node, lang *gotreesitter.Language, src []byte) uint16 {
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
		case "if_statement", "for_statement", "enhanced_for_statement", "while_statement", "do_statement", "catch_clause", "switch_block_statement_group", "switch_rule", "ternary_expression":
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

func stripJavaGenerics(raw string) string {
	s := CollapseWhitespace(raw)
	if idx := strings.IndexByte(s, '<'); idx >= 0 {
		s = s[:idx]
	}
	return strings.TrimSpace(s)
}

func hasJavaToken(node *gotreesitter.Node, lang *gotreesitter.Language, src []byte, token string) bool {
	for i := 0; i < node.ChildCount(); i++ {
		c := node.Child(i)
		if c != nil && (c.Type(lang) == token || (!c.IsNamed() && strings.TrimSpace(c.Text(src)) == token)) {
			return true
		}
	}
	return false
}

func isJavaExported(mods map[string]bool) bool {
	return mods["public"] || mods["protected"]
}

func formatJavaSignature(name string, params []graph.Param, ret string) string {
	var b strings.Builder
	if ret != "" {
		b.WriteString(ret)
		b.WriteByte(' ')
	}
	b.WriteString(name)
	b.WriteByte('(')
	for i, p := range params {
		if i > 0 {
			b.WriteString(", ")
		}
		if p.Type != "" {
			b.WriteString(p.Type)
			b.WriteByte(' ')
		}
		b.WriteString(p.Name)
	}
	b.WriteByte(')')
	return b.String()
}

// RegisterJava creates a JavaParser backed by mgr and registers it into reg.
func RegisterJava(reg *parser.Registry, mgr *treesitter.Manager) (*JavaParser, error) {
	if reg == nil {
		return nil, fmt.Errorf("%w: nil registry", parser.ErrInvalidParser)
	}
	jp, err := NewJavaParser(mgr)
	if err != nil {
		return nil, err
	}
	if err := reg.Register(jp); err != nil {
		return nil, err
	}
	return jp, nil
}
