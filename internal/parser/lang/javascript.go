package lang

import (
	"context"
	"fmt"

	"github.com/roshankumar0036singh/gographctx/internal/parser"
	"github.com/roshankumar0036singh/gographctx/internal/parser/treesitter"
)

// JavaScriptParser extracts structural symbols, ES/CommonJS imports and exports,
// private class members (#syntax), JSX component calls, and call sites from
// JavaScript (.js, .mjs, .cjs, .jsx) source files.
type JavaScriptParser struct {
	mgr *treesitter.Manager
}

// NewJavaScriptParser constructs a JavaScript LanguageParser backed by mgr.
func NewJavaScriptParser(mgr *treesitter.Manager) (*JavaScriptParser, error) {
	if mgr == nil {
		return nil, ErrNilTreeSitterManager
	}
	return &JavaScriptParser{mgr: mgr}, nil
}

// Language returns the canonical language name ("javascript").
func (p *JavaScriptParser) Language() string {
	return treesitter.LangJavaScript
}

// Extensions returns the JavaScript file extensions ([".js", ".mjs", ".cjs", ".jsx"]).
func (p *JavaScriptParser) Extensions() []string {
	return treesitter.ExtensionsForLanguage(treesitter.LangJavaScript)
}

// Parse parses a JavaScript/JSX source file using the compiled-in javascript grammar.
func (p *JavaScriptParser) Parse(ctx context.Context, path string, source []byte) (*parser.ParsedFile, error) {
	if p == nil || p.mgr == nil {
		return nil, ErrNilTreeSitterManager
	}
	return parseJSTS(ctx, p.mgr, jstsConfig{
		canonicalLang: treesitter.LangJavaScript,
		supportsTypes: false,
		supportsJSX:   true,
	}, path, source)
}

// RegisterJavaScript registers JavaScriptParser (.js, .mjs, .cjs, .jsx) into reg.
func RegisterJavaScript(reg *parser.Registry, mgr *treesitter.Manager) (*JavaScriptParser, error) {
	if reg == nil {
		return nil, fmt.Errorf("%w: nil registry", parser.ErrInvalidParser)
	}
	jp, err := NewJavaScriptParser(mgr)
	if err != nil {
		return nil, err
	}
	if err := reg.Register(jp); err != nil {
		return nil, err
	}
	return jp, nil
}
