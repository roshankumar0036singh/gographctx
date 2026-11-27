package lang

import (
	"context"
	"fmt"

	"github.com/roshankumar0036singh/gographctx/internal/parser"
	"github.com/roshankumar0036singh/gographctx/internal/parser/treesitter"
)

// TypeScriptParser extracts structural symbols, types, imports, exports,
// namespaces, decorators, and call sites from TypeScript (.ts, .mts, .cts) files.
type TypeScriptParser struct {
	mgr *treesitter.Manager
}

// NewTypeScriptParser constructs a TypeScript LanguageParser backed by mgr.
func NewTypeScriptParser(mgr *treesitter.Manager) (*TypeScriptParser, error) {
	if mgr == nil {
		return nil, ErrNilTreeSitterManager
	}
	return &TypeScriptParser{mgr: mgr}, nil
}

// Language returns the canonical language name ("typescript").
func (p *TypeScriptParser) Language() string {
	return treesitter.LangTypeScript
}

// Extensions returns the TypeScript non-JSX extensions ([".ts", ".mts", ".cts"]).
func (p *TypeScriptParser) Extensions() []string {
	return treesitter.ExtensionsForLanguage(treesitter.LangTypeScript)
}

// Parse parses a TypeScript source file using the compiled-in typescript grammar.
func (p *TypeScriptParser) Parse(ctx context.Context, path string, source []byte) (*parser.ParsedFile, error) {
	if p == nil || p.mgr == nil {
		return nil, ErrNilTreeSitterManager
	}
	return parseJSTS(ctx, p.mgr, jstsConfig{
		canonicalLang: treesitter.LangTypeScript,
		supportsTypes: true,
		supportsJSX:   false,
	}, path, source)
}

// TSXParser extracts structural symbols, types, imports, exports, and JSX
// component call sites from TSX (.tsx) source files.
type TSXParser struct {
	mgr *treesitter.Manager
}

// NewTSXParser constructs a TSX LanguageParser backed by mgr.
func NewTSXParser(mgr *treesitter.Manager) (*TSXParser, error) {
	if mgr == nil {
		return nil, ErrNilTreeSitterManager
	}
	return &TSXParser{mgr: mgr}, nil
}

// Language returns the canonical language name ("tsx").
func (p *TSXParser) Language() string {
	return treesitter.LangTSX
}

// Extensions returns the TSX file extension ([".tsx"]).
func (p *TSXParser) Extensions() []string {
	return treesitter.ExtensionsForLanguage(treesitter.LangTSX)
}

// Parse parses a TSX source file using the compiled-in tsx grammar.
func (p *TSXParser) Parse(ctx context.Context, path string, source []byte) (*parser.ParsedFile, error) {
	if p == nil || p.mgr == nil {
		return nil, ErrNilTreeSitterManager
	}
	return parseJSTS(ctx, p.mgr, jstsConfig{
		canonicalLang: treesitter.LangTSX,
		supportsTypes: true,
		supportsJSX:   true,
	}, path, source)
}

// RegisterTypeScript registers TypeScriptParser (.ts, .mts, .cts) into reg.
func RegisterTypeScript(reg *parser.Registry, mgr *treesitter.Manager) (*TypeScriptParser, error) {
	if reg == nil {
		return nil, fmt.Errorf("%w: nil registry", parser.ErrInvalidParser)
	}
	tp, err := NewTypeScriptParser(mgr)
	if err != nil {
		return nil, err
	}
	if err := reg.Register(tp); err != nil {
		return nil, err
	}
	return tp, nil
}

// RegisterTSX registers TSXParser (.tsx) into reg.
func RegisterTSX(reg *parser.Registry, mgr *treesitter.Manager) (*TSXParser, error) {
	if reg == nil {
		return nil, fmt.Errorf("%w: nil registry", parser.ErrInvalidParser)
	}
	tp, err := NewTSXParser(mgr)
	if err != nil {
		return nil, err
	}
	if err := reg.Register(tp); err != nil {
		return nil, err
	}
	return tp, nil
}
