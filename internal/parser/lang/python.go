package lang

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/odvcencio/gotreesitter"
	"github.com/roshankumar0036singh/gographctx/internal/graph"
	"github.com/roshankumar0036singh/gographctx/internal/parser"
	"github.com/roshankumar0036singh/gographctx/internal/parser/treesitter"
)

// PythonParser extracts structural symbols, type hints, decorators, imports,
// variables, classes, and call sites from Python (.py, .pyi) source files.
type PythonParser struct {
	mgr *treesitter.Manager
}

// NewPythonParser constructs a Python LanguageParser backed by mgr.
func NewPythonParser(mgr *treesitter.Manager) (*PythonParser, error) {
	if mgr == nil {
		return nil, ErrNilTreeSitterManager
	}
	return &PythonParser{mgr: mgr}, nil
}

// Language returns the canonical language name ("python").
func (p *PythonParser) Language() string {
	return treesitter.LangPython
}

// Extensions returns the file extensions handled by PythonParser ([".py", ".pyi"]).
func (p *PythonParser) Extensions() []string {
	return treesitter.ExtensionsForLanguage(treesitter.LangPython)
}

// Parse parses a Python source file in a single AST pass and returns a ParsedFile.
func (p *PythonParser) Parse(ctx context.Context, path string, source []byte) (*parser.ParsedFile, error) {
	if p == nil || p.mgr == nil {
		return nil, ErrNilTreeSitterManager
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	pt, err := p.mgr.ParseContext(ctx, treesitter.LangPython, source)
	if err != nil {
		return nil, err
	}
	defer pt.Release()

	root := pt.RootNode()
	lang := pt.Grammar

	moduleName, isInitModule := derivePythonModuleName(path)
	out := &parser.ParsedFile{
		Path:        path,
		Language:    treesitter.LangPython,
		Package:     moduleName,
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

	scope := moduleName
	if scope == "" {
		scope = "<module>"
	}
	ext := &pythonExtractor{
		path:         path,
		scope:        scope,
		moduleCaller: JoinQName(scope, "<module>"),
		isInitModule: isInitModule,
		src:          source,
		lang:         lang,
		out:          out,
	}
	ext.walkModule(root)

	return out, nil
}

func derivePythonModuleName(path string) (moduleName string, isInit bool) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return "", false
	}
	slash := filepath.ToSlash(filepath.Clean(trimmed))
	base := filepath.Base(slash)
	isInit = base == "__init__.py" || base == "__init__.pyi"

	if filepath.IsAbs(trimmed) || strings.HasPrefix(slash, "/") || strings.HasPrefix(slash, "..") {
		if isInit {
			parent := filepath.Base(filepath.Dir(slash))
			if parent != "." && parent != "/" && parent != "" {
				return parent, true
			}
			return "__init__", true
		}
		return strings.TrimSuffix(base, filepath.Ext(base)), false
	}

	if isInit {
		dir := filepath.ToSlash(filepath.Dir(slash))
		if dir == "." || dir == "" {
			return "__init__", true
		}
		return strings.ReplaceAll(dir, "/", "."), true
	}

	noExt := strings.TrimSuffix(slash, filepath.Ext(slash))
	noExt = strings.TrimPrefix(noExt, "./")
	return strings.ReplaceAll(noExt, "/", "."), false
}

type pythonExtractor struct {
	path         string
	scope        string
	moduleCaller string
	isInitModule bool
	src          []byte
	lang         *gotreesitter.Language
	out          *parser.ParsedFile
}

func (e *pythonExtractor) walkModule(root *gotreesitter.Node) {
	e.walkStatements(root, e.scope, nil, e.moduleCaller, false)
}

func (e *pythonExtractor) walkStatements(container *gotreesitter.Node, scopeQName string, enclosingClass *parser.ParsedClass, callerQName string, insideFunc bool) {
	if container == nil {
		return
	}
	inClassBody := enclosingClass != nil && !insideFunc
	count := container.NamedChildCount()
	for i := 0; i < count; i++ {
		stmt := unwrapPythonStatement(container.NamedChild(i), e.lang)
		if stmt == nil {
			continue
		}
		switch stmt.Type(e.lang) {
		case "import_statement":
			e.extractImportStatement(stmt)
		case "import_from_statement":
			e.extractImportFromStatement(stmt)
		case "future_import_statement":
			e.extractFutureImport(stmt)
		case "decorated_definition":
			e.extractDecoratedDefinition(stmt, scopeQName, enclosingClass, callerQName, insideFunc)
		case "function_definition":
			e.extractFunction(stmt, nil, scopeQName, enclosingClass, insideFunc)
		case "class_definition":
			e.extractClass(stmt, nil, scopeQName, callerQName)
		case "type_alias_statement":
			e.extractTypeAlias(stmt, scopeQName)
		case "assignment":
			if !insideFunc {
				e.extractAssignment(stmt, scopeQName, enclosingClass, inClassBody)
			}
			if right := stmt.ChildByFieldName("right", e.lang); right != nil {
				e.collectCalls(right, callerQName)
			}
		case "augmented_assignment":
			if right := stmt.ChildByFieldName("right", e.lang); right != nil {
				e.collectCalls(right, callerQName)
			}
		case "if_statement", "for_statement", "while_statement", "with_statement", "try_statement", "match_statement":
			// Walk control-flow statements at module/class level for nested defs or calls.
			e.walkControlFlow(stmt, scopeQName, enclosingClass, callerQName, insideFunc)
		default:
			e.collectCalls(stmt, callerQName)
		}
	}
}

func (e *pythonExtractor) walkControlFlow(node *gotreesitter.Node, scopeQName string, enclosingClass *parser.ParsedClass, callerQName string, insideFunc bool) {
	if node == nil {
		return
	}
	for i := 0; i < node.NamedChildCount(); i++ {
		child := unwrapPythonStatement(node.NamedChild(i), e.lang)
		if child == nil {
			continue
		}
		switch child.Type(e.lang) {
		case "block":
			e.walkStatements(child, scopeQName, enclosingClass, callerQName, insideFunc)
		case "elif_clause", "else_clause", "except_clause", "finally_clause", "case_clause":
			e.walkControlFlow(child, scopeQName, enclosingClass, callerQName, insideFunc)
		case "decorated_definition":
			e.extractDecoratedDefinition(child, scopeQName, enclosingClass, callerQName, insideFunc)
		case "function_definition":
			e.extractFunction(child, nil, scopeQName, enclosingClass, insideFunc)
		case "class_definition":
			e.extractClass(child, nil, scopeQName, callerQName)
		default:
			e.collectCalls(child, callerQName)
		}
	}
}

func unwrapPythonStatement(n *gotreesitter.Node, lang *gotreesitter.Language) *gotreesitter.Node {
	for n != nil && n.Type(lang) == "expression_statement" {
		if n.NamedChildCount() == 0 {
			return n
		}
		n = n.NamedChild(0)
	}
	return n
}

func (e *pythonExtractor) extractImportStatement(stmt *gotreesitter.Node) {
	line := NodeStartLine(stmt)
	for i := 0; i < stmt.NamedChildCount(); i++ {
		child := stmt.NamedChild(i)
		if child == nil {
			continue
		}
		switch child.Type(e.lang) {
		case "dotted_name":
			mod := CollapseWhitespace(child.Text(e.src))
			if mod != "" {
				e.out.Imports = append(e.out.Imports, parser.ParsedImport{
					Path: mod,
					Line: line,
				})
			}
		case "aliased_import":
			pathNode := child.ChildByFieldName("name", e.lang)
			aliasNode := child.ChildByFieldName("alias", e.lang)
			var mod, alias string
			if pathNode != nil {
				mod = CollapseWhitespace(pathNode.Text(e.src))
			} else if child.NamedChildCount() >= 1 {
				mod = CollapseWhitespace(child.NamedChild(0).Text(e.src))
			}
			if aliasNode != nil {
				alias = strings.TrimSpace(aliasNode.Text(e.src))
			} else if child.NamedChildCount() >= 2 {
				alias = strings.TrimSpace(child.NamedChild(1).Text(e.src))
			}
			if mod != "" {
				e.out.Imports = append(e.out.Imports, parser.ParsedImport{
					Path:  mod,
					Alias: alias,
					Line:  line,
				})
			}
		}
	}
}

// extractImportFromStatement records unaliased names as one ParsedImport entry
// and each aliased symbol ("name as alias") as its own entry so downstream
// import resolution always receives unambiguous 1:1 Alias bindings.
func (e *pythonExtractor) extractImportFromStatement(stmt *gotreesitter.Node) {
	line := NodeStartLine(stmt)
	modNode := stmt.ChildByFieldName("module_name", e.lang)
	if modNode == nil {
		// Fallback to first module_name-shaped child.
		for i := 0; i < stmt.NamedChildCount(); i++ {
			c := stmt.NamedChild(i)
			if c != nil && (c.Type(e.lang) == "relative_import" || c.Type(e.lang) == "dotted_name") {
				modNode = c
				break
			}
		}
	}
	if modNode == nil {
		return
	}

	modPath := strings.TrimSpace(modNode.Text(e.src))
	isRel := modNode.Type(e.lang) == "relative_import" || strings.HasPrefix(modPath, ".")

	var unaliasedNames []string
	var aliased []parser.ParsedImport
	isWildcard := false

	for i := 0; i < stmt.NamedChildCount(); i++ {
		child := stmt.NamedChild(i)
		if child == nil || child == modNode {
			continue
		}
		switch child.Type(e.lang) {
		case "wildcard_import":
			isWildcard = true
			unaliasedNames = append(unaliasedNames, "*")
		case "dotted_name", "identifier":
			if name := strings.TrimSpace(child.Text(e.src)); name != "" {
				unaliasedNames = append(unaliasedNames, name)
			}
		case "aliased_import":
			nameNode := child.ChildByFieldName("name", e.lang)
			aliasNode := child.ChildByFieldName("alias", e.lang)
			var sym, alias string
			if nameNode != nil {
				sym = strings.TrimSpace(nameNode.Text(e.src))
			} else if child.NamedChildCount() >= 1 {
				sym = strings.TrimSpace(child.NamedChild(0).Text(e.src))
			}
			if aliasNode != nil {
				alias = strings.TrimSpace(aliasNode.Text(e.src))
			} else if child.NamedChildCount() >= 2 {
				alias = strings.TrimSpace(child.NamedChild(1).Text(e.src))
			}
			if sym != "" {
				aliased = append(aliased, parser.ParsedImport{
					Path:       modPath,
					Alias:      alias,
					Names:      []string{sym},
					Line:       line,
					IsRelative: isRel,
				})
			}
		}
	}

	if len(unaliasedNames) > 0 || isWildcard {
		e.out.Imports = append(e.out.Imports, parser.ParsedImport{
			Path:       modPath,
			Names:      unaliasedNames,
			Line:       line,
			IsWildcard: isWildcard,
			IsRelative: isRel,
		})
	}
	e.out.Imports = append(e.out.Imports, aliased...)
}

func (e *pythonExtractor) extractFutureImport(stmt *gotreesitter.Node) {
	line := NodeStartLine(stmt)
	var names []string
	for i := 0; i < stmt.NamedChildCount(); i++ {
		child := stmt.NamedChild(i)
		if child == nil {
			continue
		}
		switch child.Type(e.lang) {
		case "dotted_name", "identifier":
			if name := strings.TrimSpace(child.Text(e.src)); name != "" {
				names = append(names, name)
			}
		case "aliased_import":
			nameNode := child.ChildByFieldName("name", e.lang)
			aliasNode := child.ChildByFieldName("alias", e.lang)
			var sym, alias string
			if nameNode != nil {
				sym = strings.TrimSpace(nameNode.Text(e.src))
			}
			if aliasNode != nil {
				alias = strings.TrimSpace(aliasNode.Text(e.src))
			}
			if sym != "" {
				e.out.Imports = append(e.out.Imports, parser.ParsedImport{
					Path:  "__future__",
					Alias: alias,
					Names: []string{sym},
					Line:  line,
				})
			}
		}
	}
	if len(names) > 0 {
		e.out.Imports = append(e.out.Imports, parser.ParsedImport{
			Path:  "__future__",
			Names: names,
			Line:  line,
		})
	}
}

func (e *pythonExtractor) extractDecoratedDefinition(node *gotreesitter.Node, scopeQName string, enclosingClass *parser.ParsedClass, callerQName string, insideFunc bool) {
	var decorators []string
	for i := 0; i < node.NamedChildCount(); i++ {
		child := node.NamedChild(i)
		if child == nil || child.Type(e.lang) != "decorator" {
			continue
		}
		raw := strings.TrimSpace(child.Text(e.src))
		raw = strings.TrimSpace(strings.TrimPrefix(raw, "@"))
		if raw != "" {
			decorators = append(decorators, raw)
		}
		e.collectCalls(child, callerQName)
	}

	def := node.ChildByFieldName("definition", e.lang)
	if def == nil {
		for i := node.NamedChildCount() - 1; i >= 0; i-- {
			c := node.NamedChild(i)
			if c != nil && (c.Type(e.lang) == "function_definition" || c.Type(e.lang) == "class_definition") {
				def = c
				break
			}
		}
	}
	if def == nil {
		return
	}

	switch def.Type(e.lang) {
	case "function_definition":
		e.extractFunction(def, decorators, scopeQName, enclosingClass, insideFunc)
	case "class_definition":
		e.extractClass(def, decorators, scopeQName, callerQName)
	}
}

func (e *pythonExtractor) extractClass(clsNode *gotreesitter.Node, decorators []string, scopeQName, callerQName string) {
	nameNode := clsNode.ChildByFieldName("name", e.lang)
	if nameNode == nil {
		return
	}
	name := strings.TrimSpace(nameNode.Text(e.src))
	if name == "" {
		return
	}

	qname := JoinQName(scopeQName, name)
	bases, metaclass := e.extractSuperclasses(clsNode.ChildByFieldName("superclasses", e.lang), callerQName)
	decs := append([]string(nil), decorators...)
	if metaclass != "" {
		decs = append(decs, "metaclass:"+metaclass)
	}

	body := clsNode.ChildByFieldName("body", e.lang)
	cls := parser.ParsedClass{
		Name:       name,
		QName:      qname,
		Kind:       graph.KindClass,
		StartLine:  NodeStartLine(clsNode),
		EndLine:    NodeEndLine(clsNode),
		Exported:   isPythonExported(name),
		Bases:      bases,
		TypeParams: e.extractPythonTypeParams(clsNode.ChildByFieldName("type_parameters", e.lang)),
		Decorators: decs,
		Docstring:  extractPythonDocstring(body, e.src, e.lang),
	}

	if body != nil {
		e.walkStatements(body, qname, &cls, callerQName, false)
	}
	e.out.Classes = append(e.out.Classes, cls)
}

func (e *pythonExtractor) extractSuperclasses(argList *gotreesitter.Node, callerQName string) (bases []string, metaclass string) {
	if argList == nil {
		return nil, ""
	}
	for i := 0; i < argList.NamedChildCount(); i++ {
		child := argList.NamedChild(i)
		if child == nil {
			continue
		}
		if child.Type(e.lang) == "keyword_argument" {
			keyNode := child.ChildByFieldName("name", e.lang)
			valNode := child.ChildByFieldName("value", e.lang)
			if keyNode != nil && valNode != nil && strings.TrimSpace(keyNode.Text(e.src)) == "metaclass" {
				metaclass = CollapseWhitespace(valNode.Text(e.src))
			}
			e.collectCalls(child, callerQName)
			continue
		}
		if base := CollapseWhitespace(child.Text(e.src)); base != "" {
			bases = append(bases, base)
		}
		e.collectCalls(child, callerQName)
	}
	return bases, metaclass
}

func (e *pythonExtractor) extractFunction(fnNode *gotreesitter.Node, decorators []string, scopeQName string, enclosingClass *parser.ParsedClass, insideFunc bool) {
	nameNode := fnNode.ChildByFieldName("name", e.lang)
	if nameNode == nil {
		return
	}
	name := strings.TrimSpace(nameNode.Text(e.src))
	if name == "" {
		return
	}

	isAsync := fnNode.ChildCount() > 0 && !fnNode.Child(0).IsNamed() && fnNode.Child(0).Text(e.src) == "async"
	inClassMethod := enclosingClass != nil && !insideFunc
	qname := JoinQName(scopeQName, name)

	params := e.extractPythonParams(fnNode.ChildByFieldName("parameters", e.lang))
	retType := e.extractPythonReturnType(fnNode.ChildByFieldName("return_type", e.lang))
	typeParams := e.extractPythonTypeParams(fnNode.ChildByFieldName("type_parameters", e.lang))
	body := fnNode.ChildByFieldName("body", e.lang)

	var recv, recvType string
	if inClassMethod {
		recvType = enclosingClass.Name
		if len(params) > 0 && !hasStaticmethodDecorator(decorators) {
			recv = params[0].Name
		}
	}

	fn := parser.ParsedFunction{
		Name:          name,
		QName:         qname,
		Kind:          graph.KindFunction,
		Receiver:      recv,
		ReceiverType:  recvType,
		StartLine:     NodeStartLine(fnNode),
		EndLine:       NodeEndLine(fnNode),
		Params:        params,
		ReturnType:    retType,
		Signature:     formatPythonSignature(isAsync, name, params, retType),
		TypeParams:    typeParams,
		Exported:      isPythonExported(name),
		IsMethod:      inClassMethod,
		IsAsync:       isAsync,
		IsConstructor: inClassMethod && (name == "__init__" || name == "__new__"),
		Decorators:    append([]string(nil), decorators...),
		Docstring:     extractPythonDocstring(body, e.src, e.lang),
		Complexity:    computePythonComplexity(body, e.lang, e.src),
	}
	e.out.Functions = append(e.out.Functions, fn)

	if body != nil {
		// Walk body statements for nested functions/classes and calls.
		e.walkFunctionBody(body, qname)
	}
}

func (e *pythonExtractor) walkFunctionBody(node *gotreesitter.Node, funcQName string) {
	if node == nil {
		return
	}
	count := node.NamedChildCount()
	for i := 0; i < count; i++ {
		child := unwrapPythonStatement(node.NamedChild(i), e.lang)
		if child == nil {
			continue
		}
		switch child.Type(e.lang) {
		case "decorated_definition":
			e.extractDecoratedDefinition(child, funcQName, nil, funcQName, true)
		case "function_definition":
			e.extractFunction(child, nil, funcQName, nil, true)
		case "class_definition":
			e.extractClass(child, nil, funcQName, funcQName)
		case "call":
			e.recordPythonCall(child, funcQName)
			e.walkFunctionBody(child, funcQName)
		default:
			e.walkFunctionBody(child, funcQName)
		}
	}
}

func (e *pythonExtractor) extractPythonParams(paramsNode *gotreesitter.Node) []graph.Param {
	if paramsNode == nil {
		return nil
	}
	var out []graph.Param
	for i := 0; i < paramsNode.NamedChildCount(); i++ {
		child := paramsNode.NamedChild(i)
		if child == nil {
			continue
		}
		switch child.Type(e.lang) {
		case "identifier":
			out = append(out, graph.Param{Name: strings.TrimSpace(child.Text(e.src))})

		case "list_splat_pattern":
			if inner := child.NamedChild(0); inner != nil {
				out = append(out, graph.Param{Name: "*" + strings.TrimSpace(inner.Text(e.src))})
			}

		case "dictionary_splat_pattern":
			if inner := child.NamedChild(0); inner != nil {
				out = append(out, graph.Param{Name: "**" + strings.TrimSpace(inner.Text(e.src))})
			}

		case "default_parameter":
			nameNode := child.ChildByFieldName("name", e.lang)
			if nameNode != nil {
				out = append(out, graph.Param{Name: strings.TrimSpace(nameNode.Text(e.src))})
			}

		case "typed_parameter":
			var pName string
			for j := 0; j < child.NamedChildCount(); j++ {
				nc := child.NamedChild(j)
				if nc == nil {
					continue
				}
				switch nc.Type(e.lang) {
				case "identifier":
					pName = strings.TrimSpace(nc.Text(e.src))
				case "list_splat_pattern":
					pName = strings.TrimSpace(nc.Text(e.src))
				case "dictionary_splat_pattern":
					pName = strings.TrimSpace(nc.Text(e.src))
				}
				if pName != "" {
					break
				}
			}
			var pType string
			if typeNode := child.ChildByFieldName("type", e.lang); typeNode != nil {
				pType = CollapseWhitespace(typeNode.Text(e.src))
			}
			if pName != "" {
				out = append(out, graph.Param{Name: pName, Type: pType})
			}

		case "typed_default_parameter":
			var pName, pType string
			if nameNode := child.ChildByFieldName("name", e.lang); nameNode != nil {
				pName = strings.TrimSpace(nameNode.Text(e.src))
			}
			if typeNode := child.ChildByFieldName("type", e.lang); typeNode != nil {
				pType = CollapseWhitespace(typeNode.Text(e.src))
			}
			if pName != "" {
				out = append(out, graph.Param{Name: pName, Type: pType})
			}
		}
	}
	return out
}

func (e *pythonExtractor) extractPythonReturnType(retNode *gotreesitter.Node) string {
	if retNode == nil {
		return ""
	}
	txt := CollapseWhitespace(retNode.Text(e.src))
	return stripOuterMatchingQuotes(txt)
}

func (e *pythonExtractor) extractPythonTypeParams(tpNode *gotreesitter.Node) []string {
	if tpNode == nil {
		return nil
	}
	var out []string
	for i := 0; i < tpNode.NamedChildCount(); i++ {
		if child := tpNode.NamedChild(i); child != nil {
			if txt := CollapseWhitespace(child.Text(e.src)); txt != "" {
				out = append(out, txt)
			}
		}
	}
	return out
}

func (e *pythonExtractor) extractTypeAlias(node *gotreesitter.Node, scopeQName string) {
	leftNode := node.ChildByFieldName("left", e.lang)
	rightNode := node.ChildByFieldName("right", e.lang)
	if leftNode == nil && node.NamedChildCount() >= 2 {
		leftNode = node.NamedChild(0)
		rightNode = node.NamedChild(1)
	}
	if leftNode == nil {
		return
	}

	name, typeParams := parsePythonTypeAliasLeft(leftNode, e.src, e.lang)
	if name == "" {
		return
	}
	var underlying string
	if rightNode != nil {
		underlying = CollapseWhitespace(rightNode.Text(e.src))
	}

	e.out.TypeAliases = append(e.out.TypeAliases, parser.ParsedTypeAlias{
		Name:           name,
		QName:          JoinQName(scopeQName, name),
		UnderlyingType: underlying,
		StartLine:      NodeStartLine(node),
		EndLine:        NodeEndLine(node),
		Exported:       isPythonExported(name),
		TypeParams:     typeParams,
	})
}

func parsePythonTypeAliasLeft(left *gotreesitter.Node, src []byte, lang *gotreesitter.Language) (string, []string) {
	cur := left
	if cur.Type(lang) == "type" && cur.NamedChildCount() > 0 {
		cur = cur.NamedChild(0)
	}
	switch cur.Type(lang) {
	case "identifier":
		return strings.TrimSpace(cur.Text(src)), nil
	case "generic_type":
		var name string
		var tparams []string
		for i := 0; i < cur.NamedChildCount(); i++ {
			c := cur.NamedChild(i)
			if c == nil {
				continue
			}
			switch c.Type(lang) {
			case "identifier":
				if name == "" {
					name = strings.TrimSpace(c.Text(src))
				}
			case "type_parameter":
				for j := 0; j < c.NamedChildCount(); j++ {
					if tp := c.NamedChild(j); tp != nil {
						if txt := CollapseWhitespace(tp.Text(src)); txt != "" {
							tparams = append(tparams, txt)
						}
					}
				}
			}
		}
		return name, tparams
	default:
		return strings.TrimSpace(cur.Text(src)), nil
	}
}

func (e *pythonExtractor) extractAssignment(stmt *gotreesitter.Node, scopeQName string, enclosingClass *parser.ParsedClass, inClassBody bool) {
	leftNode := stmt.ChildByFieldName("left", e.lang)
	if leftNode == nil {
		return
	}
	var typeStr string
	if typeNode := stmt.ChildByFieldName("type", e.lang); typeNode != nil {
		typeStr = CollapseWhitespace(typeNode.Text(e.src))
	}

	targets := collectPythonAssignTargets(leftNode, e.lang, e.src)
	for _, name := range targets {
		if name == "" || name == "_" {
			continue
		}
		if inClassBody && enclosingClass != nil {
			enclosingClass.Fields = append(enclosingClass.Fields, parser.ParsedField{
				Name:      name,
				QName:     JoinQName(enclosingClass.QName, name),
				Type:      typeStr,
				StartLine: NodeStartLine(stmt),
				EndLine:   NodeEndLine(stmt),
				Exported:  isPythonExported(name),
				IsStatic:  true,
			})
			continue
		}
		e.out.Variables = append(e.out.Variables, parser.ParsedVariable{
			Name:      name,
			QName:     JoinQName(scopeQName, name),
			Type:      typeStr,
			StartLine: NodeStartLine(stmt),
			EndLine:   NodeEndLine(stmt),
			Exported:  isPythonExported(name),
			IsConst:   isPythonAllCaps(name),
		})
	}
}

func collectPythonAssignTargets(node *gotreesitter.Node, lang *gotreesitter.Language, src []byte) []string {
	if node == nil {
		return nil
	}
	switch node.Type(lang) {
	case "identifier":
		return []string{strings.TrimSpace(node.Text(src))}
	case "pattern_list", "tuple_pattern", "list_pattern":
		var out []string
		for i := 0; i < node.NamedChildCount(); i++ {
			out = append(out, collectPythonAssignTargets(node.NamedChild(i), lang, src)...)
		}
		return out
	default:
		return nil
	}
}

func (e *pythonExtractor) collectCalls(node *gotreesitter.Node, callerQName string) {
	if node == nil {
		return
	}
	if node.Type(e.lang) == "call" {
		e.recordPythonCall(node, callerQName)
	}
	count := node.NamedChildCount()
	for i := 0; i < count; i++ {
		e.collectCalls(node.NamedChild(i), callerQName)
	}
}

func (e *pythonExtractor) recordPythonCall(callNode *gotreesitter.Node, callerQName string) {
	fnNode := callNode.ChildByFieldName("function", e.lang)
	if fnNode == nil {
		return
	}
	argsNode := callNode.ChildByFieldName("arguments", e.lang)
	argCount := countPythonArguments(argsNode, e.lang)

	call := parser.ParsedCall{
		CallerQName:   callerQName,
		Line:          NodeStartLine(callNode),
		Col:           NodeStartCol(callNode),
		ArgumentCount: argCount,
	}

	switch fnNode.Type(e.lang) {
	case "identifier":
		call.CalleeName = strings.TrimSpace(fnNode.Text(e.src))
	case "attribute":
		call.IsMethodCall = true
		if obj := fnNode.ChildByFieldName("object", e.lang); obj != nil {
			call.ReceiverName = CollapseWhitespace(obj.Text(e.src))
		}
		if attr := fnNode.ChildByFieldName("attribute", e.lang); attr != nil {
			call.CalleeName = strings.TrimSpace(attr.Text(e.src))
		}
	default:
		call.IsDynamic = true
		call.CalleeName = CollapseWhitespace(fnNode.Text(e.src))
	}

	if call.CalleeName != "" {
		e.out.Calls = append(e.out.Calls, call)
	}
}

func countPythonArguments(argsNode *gotreesitter.Node, lang *gotreesitter.Language) uint16 {
	if argsNode == nil {
		return 0
	}
	if argsNode.Type(lang) == "generator_expression" {
		return 1
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

// computePythonComplexity computes cyclomatic complexity starting at 1 for the
// given function body, intentionally stopping at nested function_definition and
// class_definition boundaries since nested definitions emit their own functions.
func computePythonComplexity(body *gotreesitter.Node, lang *gotreesitter.Language, src []byte) uint16 {
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
		case "function_definition", "class_definition":
			return
		case "if_statement", "elif_clause", "for_statement", "while_statement",
			"except_clause", "case_clause", "conditional_expression",
			"for_in_clause", "if_clause":
			complexity++
		case "boolean_operator":
			if op := n.ChildByFieldName("operator", lang); op != nil {
				opTxt := strings.TrimSpace(op.Text(src))
				if opTxt == "and" || opTxt == "or" {
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

func extractPythonDocstring(body *gotreesitter.Node, src []byte, lang *gotreesitter.Language) string {
	if body == nil {
		return ""
	}
	for i := 0; i < body.NamedChildCount(); i++ {
		first := unwrapPythonStatement(body.NamedChild(i), lang)
		if first == nil {
			continue
		}
		if first.Type(lang) == "comment" {
			continue
		}
		if first.Type(lang) != "string" {
			return ""
		}
		return cleanPythonDocstring(first.Text(src))
	}
	return ""
}

func cleanPythonDocstring(raw string) string {
	s := strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(raw), "\r\n", "\n"), "\r", "\n")
	// Strip optional string prefixes like r, u, b, f (e.g. r"""...""").
	for len(s) > 0 {
		ch := s[0]
		if ch == '"' || ch == '\'' {
			break
		}
		if strings.ContainsRune("rRuUbBfF", rune(ch)) {
			s = s[1:]
			continue
		}
		break
	}
	switch {
	case strings.HasPrefix(s, `"""`) && strings.HasSuffix(s, `"""`) && len(s) >= 6:
		s = s[3 : len(s)-3]
	case strings.HasPrefix(s, `'''`) && strings.HasSuffix(s, `'''`) && len(s) >= 6:
		s = s[3 : len(s)-3]
	case strings.HasPrefix(s, `"`) && strings.HasSuffix(s, `"`) && len(s) >= 2:
		s = s[1 : len(s)-1]
	case strings.HasPrefix(s, `'`) && strings.HasSuffix(s, `'`) && len(s) >= 2:
		s = s[1 : len(s)-1]
	}

	lines := strings.Split(s, "\n")
	if len(lines) <= 1 {
		return strings.TrimSpace(s)
	}
	// Mimic inspect.cleandoc: expand tabs to 8-column stops, trim first line,
	// compute minimum indent over non-blank lines[1:], and trim leading columns.
	lines[0] = strings.TrimSpace(lines[0])
	minIndent := -1
	for i := 1; i < len(lines); i++ {
		expanded := expandTabs8(lines[i])
		lines[i] = expanded
		if strings.TrimSpace(expanded) == "" {
			continue
		}
		indent := len(expanded) - len(strings.TrimLeft(expanded, " "))
		if minIndent < 0 || indent < minIndent {
			minIndent = indent
		}
	}
	if minIndent > 0 {
		for i := 1; i < len(lines); i++ {
			if len(lines[i]) >= minIndent {
				lines[i] = lines[i][minIndent:]
			} else {
				lines[i] = strings.TrimLeft(lines[i], " ")
			}
		}
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func expandTabs8(s string) string {
	if !strings.ContainsRune(s, '\t') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	col := 0
	for _, r := range s {
		if r == '\t' {
			spaces := 8 - (col % 8)
			for i := 0; i < spaces; i++ {
				b.WriteByte(' ')
			}
			col += spaces
			continue
		}
		b.WriteRune(r)
		col++
	}
	return b.String()
}

func stripOuterMatchingQuotes(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

func isPythonExported(name string) bool {
	if name == "" {
		return false
	}
	if !strings.HasPrefix(name, "_") {
		return true
	}
	// Python dunder methods/names (__init__, __call__, etc.) are part of public protocols.
	return len(name) > 4 && strings.HasPrefix(name, "__") && strings.HasSuffix(name, "__")
}

func isPythonAllCaps(name string) bool {
	if name == "" {
		return false
	}
	hasLetter := false
	for _, r := range name {
		if unicode.IsLetter(r) {
			hasLetter = true
			if !unicode.IsUpper(r) {
				return false
			}
		} else if !unicode.IsDigit(r) && r != '_' {
			return false
		}
	}
	return hasLetter
}

func hasStaticmethodDecorator(decorators []string) bool {
	for _, d := range decorators {
		if d == "staticmethod" {
			return true
		}
	}
	return false
}

func formatPythonSignature(isAsync bool, name string, params []graph.Param, ret string) string {
	var b strings.Builder
	if isAsync {
		b.WriteString("async ")
	}
	b.WriteString("def ")
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
		b.WriteString(" -> ")
		b.WriteString(ret)
	}
	return b.String()
}

// RegisterPython creates a PythonParser backed by mgr and registers it into reg.
func RegisterPython(reg *parser.Registry, mgr *treesitter.Manager) (*PythonParser, error) {
	if reg == nil {
		return nil, fmt.Errorf("%w: nil registry", parser.ErrInvalidParser)
	}
	pp, err := NewPythonParser(mgr)
	if err != nil {
		return nil, err
	}
	if err := reg.Register(pp); err != nil {
		return nil, err
	}
	return pp, nil
}
