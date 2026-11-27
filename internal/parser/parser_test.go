package parser

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/roshankumar0036singh/gographctx/internal/graph"
)

type stubParser struct {
	lang string
	exts []string
	fn   func(ctx context.Context, path string, source []byte) (*ParsedFile, error)
}

func (s *stubParser) Language() string     { return s.lang }
func (s *stubParser) Extensions() []string { return s.exts }
func (s *stubParser) Parse(ctx context.Context, path string, source []byte) (*ParsedFile, error) {
	if s.fn != nil {
		return s.fn(ctx, path, source)
	}
	return &ParsedFile{
		Path:     path,
		Language: NormalizeLanguage(s.lang),
	}, nil
}

func TestNormalizeLanguageAndExtension(t *testing.T) {
	langCases := map[string]string{
		"go":         "go",
		" GOLANG ":   "go",
		"py":         "python",
		"Python3":    "python",
		"TS":         "typescript",
		"tsx":        "tsx",
		"js":         "javascript",
		"JSX":        "javascript",
		"rs":         "rust",
		"java":       "java",
		"  ":         "",
		"customlang": "customlang",
	}
	for in, want := range langCases {
		if got := NormalizeLanguage(in); got != want {
			t.Errorf("NormalizeLanguage(%q) = %q, want %q", in, got, want)
		}
	}

	extCases := map[string]string{
		".GO": ".go",
		"py":  ".py",
		" Ts": ".ts",
		"":    "",
		"  ":  "",
	}
	for in, want := range extCases {
		if got := NormalizeExtension(in); got != want {
			t.Errorf("NormalizeExtension(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRegistryLifecycleAndLookup(t *testing.T) {
	reg := NewRegistry()
	goParser := &stubParser{lang: "Go", exts: []string{"go", ".GO"}}
	pyParser := &stubParser{lang: "python", exts: []string{".py", ".pyi"}}

	if err := reg.Register(goParser); err != nil {
		t.Fatalf("Register go failed: %v", err)
	}
	reg.MustRegister(pyParser)

	if err := reg.Register(nil); !errors.Is(err, ErrInvalidParser) {
		t.Fatalf("Register(nil) err = %v, want ErrInvalidParser", err)
	}
	if err := reg.Register(&stubParser{lang: "   ", exts: []string{".x"}}); !errors.Is(err, ErrInvalidParser) {
		t.Fatalf("Register empty lang err = %v, want ErrInvalidParser", err)
	}
	if err := reg.Register(&stubParser{lang: "rust", exts: []string{"."}}); !errors.Is(err, ErrInvalidParser) {
		t.Fatalf("Register invalid ext err = %v, want ErrInvalidParser", err)
	}
	if err := reg.Register(&stubParser{lang: "golang", exts: []string{".go2"}}); !errors.Is(err, ErrDuplicateParser) {
		t.Fatalf("Register duplicate lang err = %v, want ErrDuplicateParser", err)
	}
	if err := reg.Register(&stubParser{lang: "custom", exts: []string{".py"}}); !errors.Is(err, ErrDuplicateParser) {
		t.Fatalf("Register duplicate ext err = %v, want ErrDuplicateParser", err)
	}

	if !reg.SupportsLanguage("golang") || !reg.SupportsLanguage("py") || reg.SupportsLanguage("ruby") {
		t.Fatal("SupportsLanguage mismatch")
	}
	if !reg.SupportsFile("pkg/main.GO") || !reg.SupportsFile(`src\app.PYI`) || reg.SupportsFile("README.md") {
		t.Fatal("SupportsFile mismatch")
	}

	gotLangs := reg.Languages()
	if !slices.Equal(gotLangs, []string{"go", "python"}) {
		t.Fatalf("Languages() = %v, want [go python]", gotLangs)
	}
	gotExts := reg.Extensions()
	if !slices.Equal(gotExts, []string{".go", ".py", ".pyi"}) {
		t.Fatalf("Extensions() = %v, want [.go .py .pyi]", gotExts)
	}

	parsed, err := reg.ParseFile(context.Background(), "main.go", []byte("package main"))
	if err != nil {
		t.Fatalf("ParseFile failed: %v", err)
	}
	if parsed.Language != "go" || parsed.Path != "main.go" {
		t.Fatalf("ParseFile result = %+v", parsed)
	}

	if _, err := reg.ParseFile(context.Background(), "unknown.xyz", nil); !errors.Is(err, ErrUnsupportedLanguage) {
		t.Fatalf("ParseFile unknown ext err = %v, want ErrUnsupportedLanguage", err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := reg.ParseFile(canceled, "main.go", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("ParseFile canceled err = %v, want context.Canceled", err)
	}

	var nilReg *Registry
	if nilReg.SupportsFile("a.go") || nilReg.SupportsLanguage("go") || nilReg.Languages() != nil || nilReg.Extensions() != nil {
		t.Fatal("nil Registry did not degrade gracefully")
	}
}

func TestParsedFileHelpersAndClone(t *testing.T) {
	var nilPF *ParsedFile
	if nilPF.HasErrors() || nilPF.Clone() != nil || nilPF.Counts() != (ParsedCounts{}) {
		t.Fatal("nil ParsedFile helpers mismatch")
	}

	pf := &ParsedFile{
		Path:            "pkg/mod.go",
		Language:        "go",
		HasSyntaxErrors: true,
		ParseErrors:     []string{"unexpected EOF"},
		Functions: []ParsedFunction{
			{
				Name:   "Foo",
				QName:  "pkg.Foo",
				Params: []graph.Param{{Name: "x", Type: "int"}},
			},
		},
		Classes: []ParsedClass{
			{
				Name:   "Bar",
				QName:  "pkg.Bar",
				Kind:   graph.KindStruct,
				Bases:  []string{"Base"},
				Fields: []ParsedField{{Name: "id", Decorators: []string{"json"}}},
			},
		},
		Imports:     []ParsedImport{{Path: "fmt", Names: []string{"Println"}}},
		Calls:       []ParsedCall{{CallerQName: "pkg.Foo", CalleeName: "Println", Line: 5}},
		Variables:   []ParsedVariable{{Name: "Ver", QName: "pkg.Ver", IsConst: true}},
		TypeAliases: []ParsedTypeAlias{{Name: "ID", QName: "pkg.ID", TypeParams: []string{"T"}}},
	}

	if !pf.HasErrors() {
		t.Fatal("HasErrors() = false, want true")
	}
	if pf.Functions[0].EffectiveKind() != graph.KindFunction {
		t.Fatalf("EffectiveKind default = %v, want KindFunction", pf.Functions[0].EffectiveKind())
	}
	if (ParsedClass{}).EffectiveKind() != graph.KindClass {
		t.Fatal("ParsedClass.EffectiveKind default mismatch")
	}
	if pf.Classes[0].EffectiveKind() != graph.KindStruct {
		t.Fatalf("ParsedClass.EffectiveKind = %v, want KindStruct", pf.Classes[0].EffectiveKind())
	}

	counts := pf.Counts()
	if counts != (ParsedCounts{Functions: 1, Classes: 1, Imports: 1, Calls: 1, Variables: 1, TypeAliases: 1}) {
		t.Fatalf("Counts() = %+v", counts)
	}

	cloned := pf.Clone()
	cloned.ParseErrors[0] = "mutated"
	cloned.Functions[0].Params[0].Name = "y"
	cloned.Classes[0].Bases[0] = "Other"
	cloned.Classes[0].Fields[0].Decorators[0] = "yaml"
	cloned.Imports[0].Names[0] = "Sprintf"
	cloned.TypeAliases[0].TypeParams[0] = "U"

	if pf.ParseErrors[0] != "unexpected EOF" ||
		pf.Functions[0].Params[0].Name != "x" ||
		pf.Classes[0].Bases[0] != "Base" ||
		pf.Classes[0].Fields[0].Decorators[0] != "json" ||
		pf.Imports[0].Names[0] != "Println" ||
		pf.TypeAliases[0].TypeParams[0] != "T" {
		t.Fatal("Clone mutated original ParsedFile slices")
	}
}
