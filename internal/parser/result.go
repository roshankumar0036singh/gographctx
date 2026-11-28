package parser

import (
	"slices"

	"github.com/roshankumar0036singh/gographctx/internal/graph"
)

// ParsedFile is the language-agnostic intermediate extraction output produced
// by a LanguageParser before graph node/edge materialization and cross-file
// call resolution.
type ParsedFile struct {
	Path            string            `json:"path"`
	Language        string            `json:"language"`
	Package         string            `json:"package,omitempty"`
	ContentHash     string            `json:"content_hash,omitempty"`
	LineCount       uint32            `json:"line_count,omitempty"`
	HasSyntaxErrors bool              `json:"has_syntax_errors,omitempty"`
	ParseErrors     []string          `json:"parse_errors,omitempty"`
	Functions       []ParsedFunction  `json:"functions,omitempty"`
	Classes         []ParsedClass     `json:"classes,omitempty"`
	Imports         []ParsedImport    `json:"imports,omitempty"`
	Calls           []ParsedCall      `json:"calls,omitempty"`
	Variables       []ParsedVariable  `json:"variables,omitempty"`
	TypeAliases     []ParsedTypeAlias `json:"type_aliases,omitempty"`
	LocalVars       []ParsedLocalVar  `json:"local_vars,omitempty"`
}

// ParsedLocalVar records an in-function variable/attribute binding extracted
// during the single AST pass for scope-aware receiver type inference.
type ParsedLocalVar struct {
	ScopeQName       string `json:"scope_qname"`
	Name             string `json:"name"`
	ExplicitType     string `json:"explicit_type,omitempty"`
	InitCallee       string `json:"init_callee,omitempty"`
	InitReceiver     string `json:"init_receiver,omitempty"`
	InitAlias        string `json:"init_alias,omitempty"`
	DestructuredProp string `json:"destructured_prop,omitempty"`
	IsConstructor    bool   `json:"is_constructor,omitempty"`
	UnwrapElement    bool   `json:"unwrap_element,omitempty"`
	ResultIndex      uint8  `json:"result_index,omitempty"`
	Line             uint32 `json:"line,omitempty"`
}

// ParsedCounts summarizes extracted entity totals for a ParsedFile.
type ParsedCounts struct {
	Functions   int
	Classes     int
	Imports     int
	Calls       int
	Variables   int
	TypeAliases int
}

// ParsedFunction represents a top-level function, method, or closure definition.
type ParsedFunction struct {
	Name          string         `json:"name"`
	QName         string         `json:"qualified_name"`
	Kind          graph.NodeKind `json:"kind,omitempty"`
	Receiver      string         `json:"receiver,omitempty"`
	ReceiverType  string         `json:"receiver_type,omitempty"`
	StartLine     uint32         `json:"start_line"`
	EndLine       uint32         `json:"end_line"`
	Params        []graph.Param  `json:"params,omitempty"`
	ReturnType    string         `json:"return_type,omitempty"`
	Signature     string         `json:"signature,omitempty"`
	TypeParams    []string       `json:"type_params,omitempty"`
	Exported      bool           `json:"exported,omitempty"`
	IsMethod      bool           `json:"is_method,omitempty"`
	IsAsync       bool           `json:"is_async,omitempty"`
	IsConstructor bool           `json:"is_constructor,omitempty"`
	Decorators    []string       `json:"decorators,omitempty"`
	Docstring     string         `json:"docstring,omitempty"`
	Complexity    uint16         `json:"complexity,omitempty"`
}

// EffectiveKind returns Kind when explicitly set, defaulting to KindFunction.
func (f ParsedFunction) EffectiveKind() graph.NodeKind {
	if f.Kind.IsValid() {
		return f.Kind
	}
	return graph.KindFunction
}

// ParsedClass represents a class, struct, interface, trait, or enum declaration.
type ParsedClass struct {
	Name       string         `json:"name"`
	QName      string         `json:"qualified_name"`
	Kind       graph.NodeKind `json:"kind"`
	StartLine  uint32         `json:"start_line"`
	EndLine    uint32         `json:"end_line"`
	Exported   bool           `json:"exported,omitempty"`
	Bases      []string       `json:"bases,omitempty"`
	Implements []string       `json:"implements,omitempty"`
	TypeParams []string       `json:"type_params,omitempty"`
	Fields     []ParsedField  `json:"fields,omitempty"`
	Decorators []string       `json:"decorators,omitempty"`
	Docstring  string         `json:"docstring,omitempty"`
}

// EffectiveKind returns Kind when explicitly set, defaulting to KindClass.
func (c ParsedClass) EffectiveKind() graph.NodeKind {
	if c.Kind.IsValid() {
		return c.Kind
	}
	return graph.KindClass
}

// ParsedField represents a field, property, or enum member on a ParsedClass.
type ParsedField struct {
	Name       string   `json:"name"`
	QName      string   `json:"qualified_name,omitempty"`
	Type       string   `json:"type,omitempty"`
	StartLine  uint32   `json:"start_line,omitempty"`
	EndLine    uint32   `json:"end_line,omitempty"`
	Exported   bool     `json:"exported,omitempty"`
	IsStatic   bool     `json:"is_static,omitempty"`
	Decorators []string `json:"decorators,omitempty"`
	Docstring  string   `json:"docstring,omitempty"`
}

// ParsedImport represents a single import, use, or module dependency statement.
type ParsedImport struct {
	Path       string   `json:"path"`
	Alias      string   `json:"alias,omitempty"`
	Names      []string `json:"names,omitempty"`
	Line       uint32   `json:"line,omitempty"`
	IsWildcard bool     `json:"is_wildcard,omitempty"`
	IsTypeOnly bool     `json:"is_type_only,omitempty"`
	IsRelative bool     `json:"is_relative,omitempty"`
	Exported   bool     `json:"exported,omitempty"`
}

// ParsedCall represents a syntactic function, method, or constructor call site.
type ParsedCall struct {
	CallerQName   string `json:"caller_qname"`
	CalleeName    string `json:"callee_name"`
	ReceiverName  string `json:"receiver_name,omitempty"`
	Qualifier     string `json:"qualifier,omitempty"`
	Line          uint32 `json:"line"`
	Col           uint32 `json:"col,omitempty"`
	ArgumentCount uint16 `json:"argument_count,omitempty"`
	IsMethodCall  bool   `json:"is_method_call,omitempty"`
	IsDynamic     bool   `json:"is_dynamic,omitempty"`
	IsConstructor bool   `json:"is_constructor,omitempty"`
}

// ParsedVariable represents a module-level variable or constant declaration.
type ParsedVariable struct {
	Name      string `json:"name"`
	QName     string `json:"qualified_name"`
	Type      string `json:"type,omitempty"`
	StartLine uint32 `json:"start_line"`
	EndLine   uint32 `json:"end_line"`
	Exported  bool   `json:"exported,omitempty"`
	IsConst   bool   `json:"is_const,omitempty"`
	Docstring string `json:"docstring,omitempty"`
}

// ParsedTypeAlias represents a type alias or typedef declaration.
type ParsedTypeAlias struct {
	Name           string   `json:"name"`
	QName          string   `json:"qualified_name"`
	UnderlyingType string   `json:"underlying_type,omitempty"`
	StartLine      uint32   `json:"start_line"`
	EndLine        uint32   `json:"end_line"`
	Exported       bool     `json:"exported,omitempty"`
	TypeParams     []string `json:"type_params,omitempty"`
	Docstring      string   `json:"docstring,omitempty"`
}

// HasErrors reports whether parsing encountered syntax errors or recorded messages.
func (p *ParsedFile) HasErrors() bool {
	if p == nil {
		return false
	}
	return p.HasSyntaxErrors || len(p.ParseErrors) > 0
}

// Counts returns entity totals for the parsed file.
func (p *ParsedFile) Counts() ParsedCounts {
	if p == nil {
		return ParsedCounts{}
	}
	return ParsedCounts{
		Functions:   len(p.Functions),
		Classes:     len(p.Classes),
		Imports:     len(p.Imports),
		Calls:       len(p.Calls),
		Variables:   len(p.Variables),
		TypeAliases: len(p.TypeAliases),
	}
}

// Clone returns a deep copy of the ParsedFile, or nil if p is nil.
func (p *ParsedFile) Clone() *ParsedFile {
	if p == nil {
		return nil
	}
	cp := *p
	cp.ParseErrors = slices.Clone(p.ParseErrors)
	cp.Calls = slices.Clone(p.Calls)
	cp.Variables = slices.Clone(p.Variables)
	cp.LocalVars = slices.Clone(p.LocalVars)

	if p.Functions != nil {
		cp.Functions = make([]ParsedFunction, len(p.Functions))
		for i, fn := range p.Functions {
			fn.Params = slices.Clone(fn.Params)
			fn.TypeParams = slices.Clone(fn.TypeParams)
			fn.Decorators = slices.Clone(fn.Decorators)
			cp.Functions[i] = fn
		}
	}
	if p.Classes != nil {
		cp.Classes = make([]ParsedClass, len(p.Classes))
		for i, cls := range p.Classes {
			cls.Bases = slices.Clone(cls.Bases)
			cls.Implements = slices.Clone(cls.Implements)
			cls.TypeParams = slices.Clone(cls.TypeParams)
			cls.Decorators = slices.Clone(cls.Decorators)
			if cls.Fields != nil {
				cls.Fields = make([]ParsedField, len(cls.Fields))
				for j, f := range p.Classes[i].Fields {
					f.Decorators = slices.Clone(f.Decorators)
					cls.Fields[j] = f
				}
			}
			cp.Classes[i] = cls
		}
	}
	if p.Imports != nil {
		cp.Imports = make([]ParsedImport, len(p.Imports))
		for i, imp := range p.Imports {
			imp.Names = slices.Clone(imp.Names)
			cp.Imports[i] = imp
		}
	}
	if p.TypeAliases != nil {
		cp.TypeAliases = make([]ParsedTypeAlias, len(p.TypeAliases))
		for i, ta := range p.TypeAliases {
			ta.TypeParams = slices.Clone(ta.TypeParams)
			cp.TypeAliases[i] = ta
		}
	}
	return &cp
}
