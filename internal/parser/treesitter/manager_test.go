package treesitter

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"
)

func TestManagerParsesAllSupportedLanguages(t *testing.T) {
	mgr, err := NewManager()
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	defer func() {
		if err := mgr.Close(); err != nil {
			t.Errorf("Close failed: %v", err)
		}
	}()

	samples := []struct {
		lang     string
		filePath string
		rootType string
		source   string
	}{
		{
			lang:     LangGo,
			filePath: "cmd/app/main.go",
			rootType: "source_file",
			source: `package main

import "fmt"

type Greeter struct {
	Name string
}

func (g Greeter) Hello(target string) string {
	return fmt.Sprintf("hello %s from %s", target, g.Name)
}
`,
		},
		{
			lang:     LangPython,
			filePath: "src/service.py",
			rootType: "module",
			source: `from dataclasses import dataclass

@dataclass
class Greeter:
    name: str

    def hello(self, target: str) -> str:
        return f"hello {target} from {self.name}"
`,
		},
		{
			lang:     LangTypeScript,
			filePath: "src/service.ts",
			rootType: "program",
			source: `export interface Greeter {
	name: string;
	hello(target: string): string;
}

export function greet<T extends Greeter>(g: T, target: string): string {
	return g.hello(target);
}
`,
		},
		{
			lang:     LangTSX,
			filePath: "src/Greeter.tsx",
			rootType: "program",
			source: `import React from "react";

type Props = { name: string };

export const Greeter: React.FC<Props> = ({ name }) => {
	return <div className="greeter">Hello, {name}</div>;
};
`,
		},
		{
			lang:     LangJavaScript,
			filePath: "src/widget.jsx",
			rootType: "program",
			source: `export class Greeter {
	constructor(name) {
		this.name = name;
	}

	render(target) {
		return <span>{this.name}: {target}</span>;
	}
}
`,
		},
		{
			lang:     LangRust,
			filePath: "src/lib.rs",
			rootType: "source_file",
			source: `pub struct Greeter {
    pub name: String,
}

impl Greeter {
    pub fn hello(&self, target: &str) -> String {
        format!("hello {} from {}", target, self.name)
    }
}
`,
		},
		{
			lang:     LangJava,
			filePath: "src/Greeter.java",
			rootType: "program",
			source: `package com.example;

public class Greeter {
    private final String name;

    public Greeter(String name) {
        this.name = name;
    }

    public String hello(String target) {
        return "hello " + target + " from " + this.name;
    }
}
`,
		},
	}

	for _, tc := range samples {
		t.Run(tc.lang, func(t *testing.T) {
			pt, err := mgr.ParseFile(tc.filePath, []byte(tc.source))
			if err != nil {
				t.Fatalf("ParseFile(%q) failed: %v", tc.filePath, err)
			}
			defer pt.Release()

			if pt.Language != tc.lang {
				t.Fatalf("Language = %q, want %q", pt.Language, tc.lang)
			}
			root := pt.RootNode()
			if root == nil {
				t.Fatal("RootNode() returned nil")
			}
			if gotType := pt.RootType(); gotType != tc.rootType {
				t.Fatalf("RootType() = %q, want %q", gotType, tc.rootType)
			}
			if pt.HasError() {
				t.Fatalf("expected error-free syntax tree for %s, got HasError()=true", tc.lang)
			}
			if pt.NodeText(root) == "" {
				t.Fatal("NodeText(root) returned empty string")
			}

			// Verify idempotent release and post-release guards.
			pt.Release()
			pt.Release()
			if pt.RootNode() != nil || pt.RootType() != "" || pt.NodeText(root) != "" {
				t.Fatal("expected nil/empty accessors after Release()")
			}
		})
	}
}

func TestLanguageAndExtensionDiscovery(t *testing.T) {
	wantLangs := []string{LangGo, LangJava, LangJavaScript, LangPython, LangRust, LangTSX, LangTypeScript}
	if got := SupportedLanguages(); !slices.Equal(got, wantLangs) {
		t.Fatalf("SupportedLanguages() = %v, want %v", got, wantLangs)
	}

	cases := map[string]string{
		"main.go":          LangGo,
		`dir\sub\APP.PY`:   LangPython,
		"types.pyi":        LangPython,
		"mod.ts":           LangTypeScript,
		"mod.mts":          LangTypeScript,
		"mod.cts":          LangTypeScript,
		"Component.tsx":    LangTSX,
		"index.js":         LangJavaScript,
		"index.mjs":        LangJavaScript,
		"index.cjs":        LangJavaScript,
		"index.jsx":        LangJavaScript,
		"lib.rs":           LangRust,
		"Application.JAVA": LangJava,
	}
	for path, want := range cases {
		got, ok := DetectLanguage(path)
		if !ok || got != want {
			t.Errorf("DetectLanguage(%q) = (%q, %v), want (%q, true)", path, got, ok, want)
		}
		if !SupportsFile(path) {
			t.Errorf("SupportsFile(%q) = false, want true", path)
		}
	}

	if _, ok := DetectLanguage("Makefile"); ok || SupportsFile("notes.md") {
		t.Fatal("unexpected language detection for unsupported files")
	}
	if !SupportsLanguage("golang") || !SupportsLanguage("py") || !SupportsLanguage("jsx") || SupportsLanguage("cobol") {
		t.Fatal("SupportsLanguage alias mismatch")
	}
	if exts := ExtensionsForLanguage("ts"); !slices.Equal(exts, []string{".ts", ".mts", ".cts"}) {
		t.Fatalf("ExtensionsForLanguage(ts) = %v", exts)
	}
	if exts := ExtensionsForLanguage("unknown"); exts != nil {
		t.Fatalf("ExtensionsForLanguage(unknown) = %v, want nil", exts)
	}
	if len(SupportedExtensions()) < 10 {
		t.Fatalf("unexpected SupportedExtensions count: %v", SupportedExtensions())
	}
}

func TestManagerSyntaxErrorsEmptyAndQueries(t *testing.T) {
	mgr, err := NewManager(
		WithParseTimeout(2*time.Second),
		WithMemoryBudgetBytes(64<<20),
	)
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	defer mgr.Close()

	// Empty source parses cleanly without panic.
	ptEmpty, err := mgr.Parse(LangGo, nil)
	if err != nil {
		t.Fatalf("Parse empty go failed: %v", err)
	}
	if ptEmpty.RootNode() == nil {
		t.Fatal("empty Go source returned nil RootNode")
	}
	ptEmpty.Release()

	// Broken syntax still returns a partial tree marked HasError() == true.
	ptBroken, err := mgr.Parse(LangGo, []byte("package main\nfunc broken( {"))
	if err != nil {
		t.Fatalf("Parse broken go failed: %v", err)
	}
	if !ptBroken.HasError() {
		t.Fatal("expected HasError() == true on malformed Go source")
	}
	ptBroken.Release()

	// ParseStrict also returns without error when grammar finishes error-recovery parse cleanly.
	ptStrict, err := mgr.ParseStrict(LangPython, []byte("def add(a, b):\n    return a + b\n"))
	if err != nil {
		t.Fatalf("ParseStrict valid python failed: %v", err)
	}
	if ptStrict.HasError() {
		t.Fatal("unexpected syntax error on valid python")
	}
	ptStrict.Release()

	// Query compilation succeeds on supported languages and rejects bad queries.
	q, err := mgr.CompileQuery(LangGo, `(function_declaration name: (identifier) @name)`)
	if err != nil {
		t.Fatalf("CompileQuery failed: %v", err)
	}
	if q == nil {
		t.Fatal("CompileQuery returned nil query")
	}

	if _, err := mgr.CompileQuery(LangGo, "   "); !errors.Is(err, ErrEmptyQueryPattern) {
		t.Fatalf("CompileQuery empty pattern err = %v, want ErrEmptyQueryPattern", err)
	}
	if _, err := mgr.CompileQuery(LangGo, "(nonexistent_node_xyz) @x"); err == nil {
		t.Fatal("expected error compiling invalid query node type")
	}
	if _, err := mgr.CompileQuery("ruby", "(identifier) @id"); !errors.Is(err, ErrUnsupportedLanguage) {
		t.Fatalf("CompileQuery unsupported lang err = %v, want ErrUnsupportedLanguage", err)
	}
	if langPtr, err := mgr.Language("golang"); err != nil || langPtr == nil {
		t.Fatalf("Language(golang) = (%v, %v), want non-nil", langPtr, err)
	}
}

func TestManagerOptionsErrorsAndClosedLifecycle(t *testing.T) {
	if _, err := NewManager(WithParseTimeout(-time.Second)); !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("negative timeout err = %v, want ErrInvalidOption", err)
	}
	if _, err := NewManager(WithMemoryBudgetBytes(-1)); !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("negative memory budget err = %v, want ErrInvalidOption", err)
	}

	var nilMgr *Manager
	if err := nilMgr.Close(); err != nil {
		t.Fatalf("nil Manager.Close() = %v", err)
	}
	if _, err := nilMgr.Parse(LangGo, []byte("package p")); !errors.Is(err, ErrNilManager) {
		t.Fatalf("nil Manager.Parse err = %v, want ErrNilManager", err)
	}

	mgr, err := NewManager()
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}

	pt, err := mgr.Parse(LangRust, []byte("fn main() {}"))
	if err != nil {
		t.Fatalf("Parse rust failed: %v", err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := mgr.ParseContext(canceled, LangGo, []byte("package p")); !errors.Is(err, context.Canceled) {
		t.Fatalf("ParseContext canceled err = %v, want context.Canceled", err)
	}
	if _, err := mgr.ParseStrictContext(canceled, LangGo, []byte("package p")); !errors.Is(err, context.Canceled) {
		t.Fatalf("ParseStrictContext canceled err = %v, want context.Canceled", err)
	}
	if _, err := mgr.ParseFileContext(canceled, "main.go", []byte("package p")); !errors.Is(err, context.Canceled) {
		t.Fatalf("ParseFileContext canceled err = %v, want context.Canceled", err)
	}
	var nilCtx context.Context
	ptNilCtx, err := mgr.ParseStrictContext(nilCtx, LangGo, []byte("package p\n"))
	if err != nil {
		t.Fatalf("ParseStrictContext(nil) failed: %v", err)
	}
	ptNilCtx.Release()
	ptFileCtx, err := mgr.ParseFileContext(nilCtx, "main.go", []byte("package p\n"))
	if err != nil {
		t.Fatalf("ParseFileContext(nil) failed: %v", err)
	}
	ptFileCtx.Release()

	if err := mgr.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	if err := mgr.Close(); err != nil {
		t.Fatalf("second Close failed: %v", err)
	}

	// Releasing already-parsed tree after Manager.Close must stay safe.
	pt.Release()

	if _, err := mgr.Parse(LangGo, []byte("package main")); !errors.Is(err, ErrManagerClosed) {
		t.Fatalf("Parse after Close err = %v, want ErrManagerClosed", err)
	}
	if _, err := mgr.ParseFile("foo.py", []byte("x = 1")); !errors.Is(err, ErrManagerClosed) {
		t.Fatalf("ParseFile after Close err = %v, want ErrManagerClosed", err)
	}
	if _, err := mgr.ParseFile("foo.unknown", nil); !errors.Is(err, ErrUnsupportedLanguage) {
		t.Fatalf("ParseFile unsupported ext err = %v, want ErrUnsupportedLanguage", err)
	}
}

func TestManagerConcurrentParsing(t *testing.T) {
	mgr, err := NewManager()
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	defer mgr.Close()

	inputs := []struct {
		lang string
		src  string
	}{
		{LangGo, "package p\nfunc F() int { return 1 }\n"},
		{LangPython, "def f(x):\n    return x + 1\n"},
		{LangTypeScript, "export const x: number = 42;\n"},
		{LangTSX, "export const App = () => <main>ok</main>;\n"},
		{LangJavaScript, "export const sum = (a, b) => a + b;\n"},
		{LangRust, "pub fn answer() -> i32 { 42 }\n"},
		{LangJava, "class App { int run() { return 1; } }\n"},
	}

	const workers = 14
	const iters = 6

	var wg sync.WaitGroup
	errCh := make(chan error, workers*iters)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				tc := inputs[(workerID+i)%len(inputs)]
				pt, err := mgr.Parse(tc.lang, []byte(tc.src))
				if err != nil {
					errCh <- fmt.Errorf("worker %d lang %s: %w", workerID, tc.lang, err)
					return
				}
				if pt.RootNode() == nil || pt.HasError() {
					pt.Release()
					errCh <- fmt.Errorf("worker %d lang %s: invalid tree", workerID, tc.lang)
					return
				}
				pt.Release()
			}
		}(w)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatal(err)
	}
}
