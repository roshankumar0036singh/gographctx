package lang

import (
	"context"
	"errors"
	"fmt"
	"go/token"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/odvcencio/gotreesitter"
	"github.com/roshankumar0036singh/gographctx/internal/graph"
	"github.com/roshankumar0036singh/gographctx/internal/parser"
	"github.com/roshankumar0036singh/gographctx/internal/parser/treesitter"
)

// ErrNilTreeSitterManager indicates a GoParser was constructed with a nil treesitter.Manager.
var ErrNilTreeSitterManager = errors.New("lang: nil treesitter manager")

// GoParser extracts structural symbols, types, imports, package variables,
// and call sites from Go source files using compiled-in gotreesitter grammars.
type GoParser struct {
	mgr *treesitter.Manager
}

// NewGoParser constructs a Go LanguageParser backed by mgr.
func NewGoParser(mgr *treesitter.Manager) (*GoParser, error) {
	if mgr == nil {
		return nil, ErrNilTreeSitterManager
	}
	return &GoParser{mgr: mgr}, nil
}

// Language returns the canonical language name ("go").
func (p *GoParser) Language() string {
	return treesitter.LangGo
}

// Extensions returns the file extensions handled by GoParser ([".go"]).
func (p *GoParser) Extensions() []string {
	return treesitter.ExtensionsForLanguage(treesitter.LangGo)
}

// Parse parses a Go source file in a single AST pass and returns a ParsedFile.
func (p *GoParser) Parse(ctx context.Context, path string, source []byte) (*parser.ParsedFile, error) {
	if p == nil || p.mgr == nil {
		return nil, ErrNilTreeSitterManager
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	pt, err := p.mgr.ParseContext(ctx, treesitter.LangGo, source)
	if err != nil {
		return nil, err
	}
	defer pt.Release()

	root := pt.RootNode()
	lang := pt.Grammar

	out := &parser.ParsedFile{
		Path:        path,
		Language:    treesitter.LangGo,
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

	out.Package = extractGoPackageName(root, source, lang)
	scope := DeriveModuleScope(path, out.Package)
	pkgInitCaller := JoinQName(scope, "<package_init>")
	isTest := strings.HasSuffix(strings.ToLower(strings.TrimSpace(path)), "_test.go")

	ext := &goExtractor{
		path:          path,
		scope:         scope,
		pkgInitCaller: pkgInitCaller,
		isTestFile:    isTest,
		src:           source,
		lang:          lang,
		out:           out,
	}
	ext.walkTopLevel(root)

	return out, nil
}

type goExtractor struct {
	path          string
	scope         string
	pkgInitCaller string
	isTestFile    bool
	src           []byte
	lang          *gotreesitter.Language
	out           *parser.ParsedFile
}

func extractGoPackageName(root *gotreesitter.Node, src []byte, lang *gotreesitter.Language) string {
	count := root.NamedChildCount()
	for i := 0; i < count; i++ {
		child := root.NamedChild(i)
		if child == nil || child.Type(lang) != "package_clause" {
			continue
		}
		if nameNode := child.ChildByFieldName("name", lang); nameNode != nil {
			return strings.TrimSpace(nameNode.Text(src))
		}
		for j := 0; j < child.NamedChildCount(); j++ {
			nc := child.NamedChild(j)
			if nc != nil && nc.Type(lang) == "package_identifier" {
				return strings.TrimSpace(nc.Text(src))
			}
		}
	}
	return ""
}

func (e *goExtractor) walkTopLevel(root *gotreesitter.Node) {
	count := root.NamedChildCount()
	for i := 0; i < count; i++ {
		node := root.NamedChild(i)
		if node == nil {
			continue
		}
		switch node.Type(e.lang) {
		case "import_declaration":
			e.extractImports(node)
		case "const_declaration":
			e.extractValueDeclaration(node, true)
		case "var_declaration":
			e.extractValueDeclaration(node, false)
		case "type_declaration":
			e.extractTypeDeclaration(node)
		case "function_declaration":
			e.extractFunction(node)
		case "method_declaration":
			e.extractMethod(node)
		}
	}
}

func (e *goExtractor) extractImports(decl *gotreesitter.Node) {
	count := decl.NamedChildCount()
	for i := 0; i < count; i++ {
		child := decl.NamedChild(i)
		if child == nil {
			continue
		}
		switch child.Type(e.lang) {
		case "import_spec":
			e.extractImportSpec(child)
		case "import_spec_list":
			for j := 0; j < child.NamedChildCount(); j++ {
				spec := child.NamedChild(j)
				if spec != nil && spec.Type(e.lang) == "import_spec" {
					e.extractImportSpec(spec)
				}
			}
		}
	}
}

func (e *goExtractor) extractImportSpec(spec *gotreesitter.Node) {
	pathNode := spec.ChildByFieldName("path", e.lang)
	if pathNode == nil {
		for i := spec.NamedChildCount() - 1; i >= 0; i-- {
			nc := spec.NamedChild(i)
			if nc != nil {
				typ := nc.Type(e.lang)
				if typ == "interpreted_string_literal" || typ == "raw_string_literal" {
					pathNode = nc
					break
				}
			}
		}
	}
	if pathNode == nil {
		return
	}

	rawPath := strings.TrimSpace(pathNode.Text(e.src))
	unquoted, err := strconv.Unquote(rawPath)
	if err != nil {
		unquoted = strings.Trim(rawPath, "\"`")
	}

	var alias string
	if nameNode := spec.ChildByFieldName("name", e.lang); nameNode != nil {
		alias = strings.TrimSpace(nameNode.Text(e.src))
	} else {
		for i := 0; i < spec.NamedChildCount(); i++ {
			nc := spec.NamedChild(i)
			if nc == nil || nc == pathNode {
				continue
			}
			switch nc.Type(e.lang) {
			case "package_identifier", "dot", "blank_identifier":
				alias = strings.TrimSpace(nc.Text(e.src))
			}
			if alias != "" {
				break
			}
		}
	}

	e.out.Imports = append(e.out.Imports, parser.ParsedImport{
		Path:       unquoted,
		Alias:      alias,
		Line:       NodeStartLine(spec),
		IsWildcard: alias == ".",
		IsRelative: strings.HasPrefix(unquoted, "./") || strings.HasPrefix(unquoted, "../") || unquoted == ".",
	})
}

func (e *goExtractor) extractValueDeclaration(decl *gotreesitter.Node, isConst bool) {
	declDoc := LeadingDocComment(decl, e.src, e.lang)
	var specs []*gotreesitter.Node

	for i := 0; i < decl.NamedChildCount(); i++ {
		child := decl.NamedChild(i)
		if child == nil {
			continue
		}
		typ := child.Type(e.lang)
		switch typ {
		case "var_spec", "const_spec":
			specs = append(specs, child)
		case "var_spec_list", "const_spec_list":
			for j := 0; j < child.NamedChildCount(); j++ {
				sc := child.NamedChild(j)
				if sc != nil && (sc.Type(e.lang) == "var_spec" || sc.Type(e.lang) == "const_spec") {
					specs = append(specs, sc)
				}
			}
		}
	}

	singleSpec := len(specs) == 1
	for _, spec := range specs {
		e.extractValueSpec(spec, isConst, declDoc, singleSpec)
	}
}

func (e *goExtractor) extractValueSpec(spec *gotreesitter.Node, isConst bool, declDoc string, useDeclDoc bool) {
	doc := LeadingDocComment(spec, e.src, e.lang)
	if doc == "" && useDeclDoc {
		doc = declDoc
	}

	names := ChildrenByField(spec, "name", e.lang)
	if len(names) == 0 {
		for i := 0; i < spec.NamedChildCount(); i++ {
			nc := spec.NamedChild(i)
			if nc != nil && nc.Type(e.lang) == "identifier" {
				names = append(names, nc)
			}
		}
	}

	var typeStr string
	if typeNode := spec.ChildByFieldName("type", e.lang); typeNode != nil {
		typeStr = CollapseWhitespace(typeNode.Text(e.src))
	}

	for _, id := range names {
		name := strings.TrimSpace(id.Text(e.src))
		if name == "" || name == "_" {
			continue
		}
		e.out.Variables = append(e.out.Variables, parser.ParsedVariable{
			Name:      name,
			QName:     JoinQName(e.scope, name),
			Type:      typeStr,
			StartLine: NodeStartLine(spec),
			EndLine:   NodeEndLine(spec),
			Exported:  token.IsExported(name),
			IsConst:   isConst,
			Docstring: doc,
		})
	}

	if valueNode := spec.ChildByFieldName("value", e.lang); valueNode != nil {
		e.collectCalls(valueNode, e.pkgInitCaller)
	}
}

func (e *goExtractor) extractTypeDeclaration(decl *gotreesitter.Node) {
	declDoc := LeadingDocComment(decl, e.src, e.lang)
	var specs []*gotreesitter.Node
	for i := 0; i < decl.NamedChildCount(); i++ {
		child := decl.NamedChild(i)
		if child == nil {
			continue
		}
		typ := child.Type(e.lang)
		if typ == "type_spec" || typ == "type_alias" {
			specs = append(specs, child)
		}
	}

	singleSpec := len(specs) == 1
	for _, spec := range specs {
		doc := LeadingDocComment(spec, e.src, e.lang)
		if doc == "" && singleSpec {
			doc = declDoc
		}
		if spec.Type(e.lang) == "type_alias" {
			e.extractTypeAlias(spec, doc)
		} else {
			e.extractTypeSpec(spec, doc)
		}
	}
}

func (e *goExtractor) extractTypeAlias(spec *gotreesitter.Node, doc string) {
	nameNode := spec.ChildByFieldName("name", e.lang)
	if nameNode == nil {
		return
	}
	name := strings.TrimSpace(nameNode.Text(e.src))
	if name == "" {
		return
	}
	var underlying string
	if typeNode := spec.ChildByFieldName("type", e.lang); typeNode != nil {
		underlying = CollapseWhitespace(typeNode.Text(e.src))
	}
	e.out.TypeAliases = append(e.out.TypeAliases, parser.ParsedTypeAlias{
		Name:           name,
		QName:          JoinQName(e.scope, name),
		UnderlyingType: underlying,
		StartLine:      NodeStartLine(spec),
		EndLine:        NodeEndLine(spec),
		Exported:       token.IsExported(name),
		TypeParams:     e.extractTypeParams(spec.ChildByFieldName("type_parameters", e.lang)),
		Docstring:      doc,
	})
}

func (e *goExtractor) extractTypeSpec(spec *gotreesitter.Node, doc string) {
	nameNode := spec.ChildByFieldName("name", e.lang)
	typeNode := spec.ChildByFieldName("type", e.lang)
	if nameNode == nil || typeNode == nil {
		return
	}
	name := strings.TrimSpace(nameNode.Text(e.src))
	if name == "" {
		return
	}
	qname := JoinQName(e.scope, name)
	typeParams := e.extractTypeParams(spec.ChildByFieldName("type_parameters", e.lang))

	switch typeNode.Type(e.lang) {
	case "struct_type":
		cls := parser.ParsedClass{
			Name:       name,
			QName:      qname,
			Kind:       graph.KindStruct,
			StartLine:  NodeStartLine(spec),
			EndLine:    NodeEndLine(spec),
			Exported:   token.IsExported(name),
			TypeParams: typeParams,
			Docstring:  doc,
		}
		e.populateStructMembers(&cls, typeNode)
		e.out.Classes = append(e.out.Classes, cls)

	case "interface_type":
		cls := parser.ParsedClass{
			Name:       name,
			QName:      qname,
			Kind:       graph.KindInterface,
			StartLine:  NodeStartLine(spec),
			EndLine:    NodeEndLine(spec),
			Exported:   token.IsExported(name),
			TypeParams: typeParams,
			Docstring:  doc,
		}
		e.populateInterfaceMembers(&cls, typeNode)
		e.out.Classes = append(e.out.Classes, cls)

	default:
		e.out.TypeAliases = append(e.out.TypeAliases, parser.ParsedTypeAlias{
			Name:           name,
			QName:          qname,
			UnderlyingType: CollapseWhitespace(typeNode.Text(e.src)),
			StartLine:      NodeStartLine(spec),
			EndLine:        NodeEndLine(spec),
			Exported:       token.IsExported(name),
			TypeParams:     typeParams,
			Docstring:      doc,
		})
	}
}

func (e *goExtractor) populateStructMembers(cls *parser.ParsedClass, structNode *gotreesitter.Node) {
	for i := 0; i < structNode.NamedChildCount(); i++ {
		listNode := structNode.NamedChild(i)
		if listNode == nil || listNode.Type(e.lang) != "field_declaration_list" {
			continue
		}
		for j := 0; j < listNode.NamedChildCount(); j++ {
			fd := listNode.NamedChild(j)
			if fd == nil || fd.Type(e.lang) != "field_declaration" {
				continue
			}
			fieldNames := ChildrenByField(fd, "name", e.lang)
			typeNode := fd.ChildByFieldName("type", e.lang)
			if len(fieldNames) == 0 {
				// Embedded struct field (e.g. Base, *alias.Client, Box[T]).
				if typeNode != nil {
					if base := normalizeGoEmbeddedType(typeNode.Text(e.src)); base != "" {
						cls.Bases = append(cls.Bases, base)
					}
				}
				continue
			}

			var typeStr string
			if typeNode != nil {
				typeStr = CollapseWhitespace(typeNode.Text(e.src))
			}
			doc := LeadingDocComment(fd, e.src, e.lang)
			var tagDecorators []string
			if tagNode := fd.ChildByFieldName("tag", e.lang); tagNode != nil {
				tagDecorators = append(tagDecorators, strings.TrimSpace(tagNode.Text(e.src)))
			}

			for _, fn := range fieldNames {
				fname := strings.TrimSpace(fn.Text(e.src))
				if fname == "" {
					continue
				}
				cls.Fields = append(cls.Fields, parser.ParsedField{
					Name:       fname,
					QName:      JoinQName(cls.QName, fname),
					Type:       typeStr,
					StartLine:  NodeStartLine(fd),
					EndLine:    NodeEndLine(fd),
					Exported:   token.IsExported(fname),
					Decorators: append([]string(nil), tagDecorators...),
					Docstring:  doc,
				})
			}
		}
	}
}

func (e *goExtractor) populateInterfaceMembers(cls *parser.ParsedClass, ifaceNode *gotreesitter.Node) {
	for i := 0; i < ifaceNode.NamedChildCount(); i++ {
		elem := ifaceNode.NamedChild(i)
		if elem == nil {
			continue
		}
		switch elem.Type(e.lang) {
		case "type_elem", "constraint_elem":
			// Interface embeddings and union constraints.
			for j := 0; j < elem.NamedChildCount(); j++ {
				child := elem.NamedChild(j)
				if child == nil {
					continue
				}
				if base := normalizeGoEmbeddedType(child.Text(e.src)); base != "" {
					cls.Bases = append(cls.Bases, base)
				}
			}
			if elem.NamedChildCount() == 0 {
				if base := normalizeGoEmbeddedType(elem.Text(e.src)); base != "" {
					cls.Bases = append(cls.Bases, base)
				}
			}

		case "method_elem":
			nameNode := elem.ChildByFieldName("name", e.lang)
			if nameNode == nil {
				continue
			}
			mname := strings.TrimSpace(nameNode.Text(e.src))
			if mname == "" {
				continue
			}
			params := e.extractParameters(elem.ChildByFieldName("parameters", e.lang))
			retType := e.extractReturnType(elem.ChildByFieldName("result", e.lang))
			e.out.Functions = append(e.out.Functions, parser.ParsedFunction{
				Name:         mname,
				QName:        JoinQName(cls.QName, mname),
				Kind:         graph.KindFunction,
				Receiver:     cls.Name,
				ReceiverType: cls.Name,
				StartLine:    NodeStartLine(elem),
				EndLine:      NodeEndLine(elem),
				Params:       params,
				ReturnType:   retType,
				Signature:    formatGoSignature("", mname, params, retType),
				Exported:     token.IsExported(mname),
				IsMethod:     true,
				Decorators:   []string{"go:interface_method"},
				Docstring:    LeadingDocComment(elem, e.src, e.lang),
			})
		}
	}
}

func (e *goExtractor) extractFunction(fnNode *gotreesitter.Node) {
	nameNode := fnNode.ChildByFieldName("name", e.lang)
	if nameNode == nil {
		return
	}
	name := strings.TrimSpace(nameNode.Text(e.src))
	if name == "" {
		return
	}

	params := e.extractParameters(fnNode.ChildByFieldName("parameters", e.lang))
	retType := e.extractReturnType(fnNode.ChildByFieldName("result", e.lang))
	typeParams := e.extractTypeParams(fnNode.ChildByFieldName("type_parameters", e.lang))
	qname := JoinQName(e.scope, name)
	body := fnNode.ChildByFieldName("body", e.lang)

	var decorators []string
	if name == "init" {
		decorators = append(decorators, "go:init")
	}
	if e.isTestFile {
		if testTag := goTestKindDecorator(name); testTag != "" {
			decorators = append(decorators, testTag)
		}
	}

	e.out.Functions = append(e.out.Functions, parser.ParsedFunction{
		Name:       name,
		QName:      qname,
		Kind:       graph.KindFunction,
		StartLine:  NodeStartLine(fnNode),
		EndLine:    NodeEndLine(fnNode),
		Params:     params,
		ReturnType: retType,
		Signature:  formatGoSignature("", name, params, retType),
		TypeParams: typeParams,
		Exported:   token.IsExported(name),
		Decorators: decorators,
		Docstring:  LeadingDocComment(fnNode, e.src, e.lang),
		Complexity: computeGoComplexity(body, e.lang, e.src),
	})

	if body != nil {
		e.collectCalls(body, qname)
	}
}

func (e *goExtractor) extractMethod(mNode *gotreesitter.Node) {
	nameNode := mNode.ChildByFieldName("name", e.lang)
	if nameNode == nil {
		return
	}
	name := strings.TrimSpace(nameNode.Text(e.src))
	if name == "" {
		return
	}

	recvRaw, recvBase := e.extractReceiver(mNode.ChildByFieldName("receiver", e.lang))
	params := e.extractParameters(mNode.ChildByFieldName("parameters", e.lang))
	retType := e.extractReturnType(mNode.ChildByFieldName("result", e.lang))
	qname := JoinQName(e.scope, recvBase, name)
	body := mNode.ChildByFieldName("body", e.lang)

	e.out.Functions = append(e.out.Functions, parser.ParsedFunction{
		Name:         name,
		QName:        qname,
		Kind:         graph.KindFunction,
		Receiver:     recvRaw,
		ReceiverType: recvBase,
		StartLine:    NodeStartLine(mNode),
		EndLine:      NodeEndLine(mNode),
		Params:       params,
		ReturnType:   retType,
		Signature:    formatGoSignature(recvRaw, name, params, retType),
		Exported:     token.IsExported(name),
		IsMethod:     true,
		Docstring:    LeadingDocComment(mNode, e.src, e.lang),
		Complexity:   computeGoComplexity(body, e.lang, e.src),
	})

	if body != nil {
		e.collectCalls(body, qname)
	}
}

func (e *goExtractor) extractReceiver(recvList *gotreesitter.Node) (rawType, baseType string) {
	if recvList == nil {
		return "", ""
	}
	for i := 0; i < recvList.NamedChildCount(); i++ {
		pd := recvList.NamedChild(i)
		if pd == nil || pd.Type(e.lang) != "parameter_declaration" {
			continue
		}
		typeNode := pd.ChildByFieldName("type", e.lang)
		if typeNode == nil {
			continue
		}
		rawType = CollapseWhitespace(typeNode.Text(e.src))
		baseType = stripGoReceiverType(rawType)
		return rawType, baseType
	}
	return "", ""
}

func (e *goExtractor) extractParameters(paramList *gotreesitter.Node) []graph.Param {
	if paramList == nil {
		return nil
	}
	var out []graph.Param
	for i := 0; i < paramList.NamedChildCount(); i++ {
		pd := paramList.NamedChild(i)
		if pd == nil {
			continue
		}
		switch pd.Type(e.lang) {
		case "parameter_declaration":
			typeNode := pd.ChildByFieldName("type", e.lang)
			var typeStr string
			if typeNode != nil {
				typeStr = CollapseWhitespace(typeNode.Text(e.src))
			}
			names := ChildrenByField(pd, "name", e.lang)
			if len(names) == 0 {
				if typeStr != "" {
					out = append(out, graph.Param{Type: typeStr})
				}
				continue
			}
			for _, id := range names {
				out = append(out, graph.Param{
					Name: strings.TrimSpace(id.Text(e.src)),
					Type: typeStr,
				})
			}

		case "variadic_parameter_declaration":
			typeNode := pd.ChildByFieldName("type", e.lang)
			var typeStr string
			if typeNode != nil {
				typeStr = "..." + CollapseWhitespace(typeNode.Text(e.src))
			}
			nameNode := pd.ChildByFieldName("name", e.lang)
			var pName string
			if nameNode != nil {
				pName = strings.TrimSpace(nameNode.Text(e.src))
			}
			out = append(out, graph.Param{
				Name: pName,
				Type: typeStr,
			})
		}
	}
	return out
}

func (e *goExtractor) extractReturnType(resultNode *gotreesitter.Node) string {
	if resultNode == nil {
		return ""
	}
	return CollapseWhitespace(resultNode.Text(e.src))
}

func (e *goExtractor) extractTypeParams(tpList *gotreesitter.Node) []string {
	if tpList == nil {
		return nil
	}
	var params []string
	for i := 0; i < tpList.NamedChildCount(); i++ {
		decl := tpList.NamedChild(i)
		if decl == nil || decl.Type(e.lang) != "type_parameter_declaration" {
			continue
		}
		var constraint string
		if cNode := decl.ChildByFieldName("type", e.lang); cNode != nil {
			constraint = CollapseWhitespace(cNode.Text(e.src))
		}
		names := ChildrenByField(decl, "name", e.lang)
		for _, id := range names {
			n := strings.TrimSpace(id.Text(e.src))
			if constraint != "" {
				params = append(params, n+" "+constraint)
			} else {
				params = append(params, n)
			}
		}
	}
	return params
}

func (e *goExtractor) collectCalls(node *gotreesitter.Node, callerQName string) {
	if node == nil {
		return
	}
	switch node.Type(e.lang) {
	case "call_expression":
		e.recordCallExpression(node, callerQName)
	case "type_conversion_expression":
		// tree-sitter-go parses single-argument generic calls (e.g. pkg.Decode[T](x))
		// as type_conversion_expression wrapping a generic_type.
		if typeNode := node.ChildByFieldName("type", e.lang); typeNode != nil && typeNode.Type(e.lang) == "generic_type" {
			e.recordGenericConversionCall(node, typeNode, callerQName)
		}
	}
	count := node.NamedChildCount()
	for i := 0; i < count; i++ {
		e.collectCalls(node.NamedChild(i), callerQName)
	}
}

func (e *goExtractor) recordGenericConversionCall(convNode, genericType *gotreesitter.Node, callerQName string) {
	var target *gotreesitter.Node
	for i := 0; i < genericType.NamedChildCount(); i++ {
		child := genericType.NamedChild(i)
		if child == nil {
			continue
		}
		typ := child.Type(e.lang)
		if typ == "qualified_type" || typ == "type_identifier" {
			target = child
			break
		}
	}
	if target == nil {
		return
	}

	call := parser.ParsedCall{
		CallerQName:   callerQName,
		Line:          NodeStartLine(convNode),
		Col:           NodeStartCol(convNode),
		ArgumentCount: 1,
	}
	switch target.Type(e.lang) {
	case "type_identifier":
		call.CalleeName = strings.TrimSpace(target.Text(e.src))
	case "qualified_type":
		call.IsMethodCall = true
		if pkgNode := target.ChildByFieldName("package", e.lang); pkgNode != nil {
			call.ReceiverName = strings.TrimSpace(pkgNode.Text(e.src))
		} else if target.NamedChildCount() >= 1 {
			call.ReceiverName = strings.TrimSpace(target.NamedChild(0).Text(e.src))
		}
		if nameNode := target.ChildByFieldName("name", e.lang); nameNode != nil {
			call.CalleeName = strings.TrimSpace(nameNode.Text(e.src))
		} else if target.NamedChildCount() >= 2 {
			call.CalleeName = strings.TrimSpace(target.NamedChild(1).Text(e.src))
		}
	}
	if call.CalleeName != "" {
		e.out.Calls = append(e.out.Calls, call)
	}
}

func (e *goExtractor) recordCallExpression(callNode *gotreesitter.Node, callerQName string) {
	fnNode := callNode.ChildByFieldName("function", e.lang)
	if fnNode == nil {
		return
	}
	argsNode := callNode.ChildByFieldName("arguments", e.lang)
	argCount := countGoArguments(argsNode)

	call := parser.ParsedCall{
		CallerQName:   callerQName,
		Line:          NodeStartLine(callNode),
		Col:           NodeStartCol(callNode),
		ArgumentCount: argCount,
	}

	for fnNode != nil && fnNode.Type(e.lang) == "parenthesized_expression" {
		fnNode = fnNode.NamedChild(0)
	}
	if fnNode == nil {
		return
	}

	switch fnNode.Type(e.lang) {
	case "identifier":
		call.CalleeName = strings.TrimSpace(fnNode.Text(e.src))
	case "selector_expression":
		call.IsMethodCall = true
		if op := fnNode.ChildByFieldName("operand", e.lang); op != nil {
			call.ReceiverName = CollapseWhitespace(op.Text(e.src))
		}
		if field := fnNode.ChildByFieldName("field", e.lang); field != nil {
			call.CalleeName = strings.TrimSpace(field.Text(e.src))
		}
	case "func_literal":
		call.IsDynamic = true
		call.CalleeName = "<anonymous>"
	default:
		call.IsDynamic = true
		call.CalleeName = CollapseWhitespace(fnNode.Text(e.src))
	}

	if call.CalleeName != "" {
		e.out.Calls = append(e.out.Calls, call)
	}
}

func countGoArguments(argsNode *gotreesitter.Node) uint16 {
	if argsNode == nil {
		return 0
	}
	n := argsNode.NamedChildCount()
	if n > 0xffff {
		return 0xffff
	}
	return uint16(n)
}

func computeGoComplexity(body *gotreesitter.Node, lang *gotreesitter.Language, src []byte) uint16 {
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
		case "if_statement", "for_statement", "expression_case", "type_case", "communication_case":
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

func stripGoReceiverType(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimLeft(s, "*")
	s = strings.TrimSpace(s)
	if idx := strings.IndexByte(s, '['); idx >= 0 {
		s = s[:idx]
	}
	return strings.TrimSpace(s)
}

func normalizeGoEmbeddedType(raw string) string {
	s := CollapseWhitespace(raw)
	s = strings.TrimPrefix(s, "~")
	s = strings.TrimLeft(s, "*")
	s = strings.TrimSpace(s)
	if idx := strings.IndexByte(s, '['); idx >= 0 {
		s = s[:idx]
	}
	return strings.TrimSpace(s)
}

func formatGoSignature(recv, name string, params []graph.Param, ret string) string {
	var b strings.Builder
	b.WriteString("func ")
	if recv != "" {
		b.WriteByte('(')
		b.WriteString(recv)
		b.WriteString(") ")
	}
	b.WriteString(name)
	b.WriteByte('(')
	for i, p := range params {
		if i > 0 {
			b.WriteString(", ")
		}
		if p.Name != "" && p.Type != "" {
			b.WriteString(p.Name)
			b.WriteByte(' ')
			b.WriteString(p.Type)
		} else if p.Name != "" {
			b.WriteString(p.Name)
		} else {
			b.WriteString(p.Type)
		}
	}
	b.WriteByte(')')
	if ret != "" {
		b.WriteByte(' ')
		b.WriteString(ret)
	}
	return b.String()
}

func goTestKindDecorator(name string) string {
	switch {
	case isGoTestName(name, "Test"):
		return "go:test"
	case isGoTestName(name, "Benchmark"):
		return "go:benchmark"
	case isGoTestName(name, "Fuzz"):
		return "go:fuzz"
	case isGoTestName(name, "Example"):
		return "go:example"
	default:
		return ""
	}
}

func isGoTestName(name, prefix string) bool {
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	if len(name) == len(prefix) {
		return true
	}
	r, _ := utf8.DecodeRuneInString(name[len(prefix):])
	return !unicode.IsLower(r)
}

// RegisterGo creates a GoParser backed by mgr and registers it into reg.
func RegisterGo(reg *parser.Registry, mgr *treesitter.Manager) (*GoParser, error) {
	if reg == nil {
		return nil, fmt.Errorf("%w: nil registry", parser.ErrInvalidParser)
	}
	gp, err := NewGoParser(mgr)
	if err != nil {
		return nil, err
	}
	if err := reg.Register(gp); err != nil {
		return nil, err
	}
	return gp, nil
}
