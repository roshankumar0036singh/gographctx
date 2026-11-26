package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultIgnorePatternsAndCrossPlatformPaths(t *testing.T) {
	m := DefaultIgnoreMatcher()

	ignored := []string{
		".git/",
		".git/config",
		"node_modules/lodash/index.js",
		"pkg/node_modules/x.js",
		"vendor/github.com/pkg/mod.go",
		"src/__pycache__/main.cpython-312.pyc",
		"dist/bundle.js",
		"build/output.bin",
		"target/debug/app",
		".venv/lib/site.py",
		".ggctx/index.ggctx",
		`pkg\node_modules\win.js`,
		`./build/app.exe`,
	}
	for _, p := range ignored {
		if !m.ShouldIgnore(p) {
			t.Errorf("ShouldIgnore(%q) = false, want true", p)
		}
	}

	kept := []string{
		"main.go",
		"internal/graph/graph.go",
		"pkg/builder/build.go", // contains "build" as substring, NOT a "build" dir
		"build",                // regular file named "build" should NOT match dir-only "build/"
		"src/target.go",
		".gitignore",
	}
	for _, p := range kept {
		if m.ShouldIgnore(p) {
			t.Errorf("ShouldIgnore(%q) = true, want false", p)
		}
	}

	// Explicit directory flag matches directory-only rule
	if !m.ShouldIgnorePath("build", true) {
		t.Fatal("ShouldIgnorePath(\"build\", true) should be true for build/ rule")
	}
	if m.ShouldIgnorePath("build", false) {
		t.Fatal("ShouldIgnorePath(\"build\", false) should be false for build/ rule")
	}

	// Nil matcher safety
	var nilMatcher *IgnoreMatcher
	if nilMatcher.ShouldIgnore("a.go") {
		t.Fatal("nil IgnoreMatcher should return false")
	}
}

func TestGitignoreSemanticsGlobsNegationAndChildScoping(t *testing.T) {
	rules := `
# Comments and blank lines should be ignored

*.log
!important.log
/root_only.txt
docs/generated/
**/temp/*.go
a/**/z.py
[0-9]?.tmp
[!a-z]_file.txt
\#literal_hash.txt
\!literal_bang.txt
trailing\ 
`
	m, err := ParseIgnoreRules("", rules)
	if err != nil {
		t.Fatalf("ParseIgnoreRules failed: %v", err)
	}

	cases := []struct {
		path  string
		isDir bool
		want  bool
	}{
		// Negation overrides earlier wildcard
		{"debug.log", false, true},
		{"sub/debug.log", false, true},
		{"important.log", false, false},
		{"sub/important.log", false, false},

		// Anchored root rule vs nested
		{"root_only.txt", false, true},
		{"sub/root_only.txt", false, false},

		// Directory-only anchored rule
		{"docs/generated", true, true},
		{"docs/generated", false, false},
		{"docs/generated/api.md", false, true},
		{"other/docs/generated/api.md", false, false},

		// Double-star globs (leading & middle)
		{"temp/a.go", false, true},
		{"pkg/sub/temp/b.go", false, true},
		{"pkg/sub/temp/b.py", false, false},
		{"a/z.py", false, true},
		{"a/b/z.py", false, true},
		{"a/b/c/z.py", false, true},

		// Character classes & '?'
		{"1a.tmp", false, true},
		{"1ab.tmp", false, false},
		{"9_file.txt", false, true},
		{"a_file.txt", false, false},

		// Escaped # and ! and trailing space
		{"#literal_hash.txt", false, true},
		{"!literal_bang.txt", false, true},
		{"trailing ", false, true},
		{"trailing", false, false},
	}
	for _, tc := range cases {
		if got := m.ShouldIgnorePath(tc.path, tc.isDir); got != tc.want {
			t.Errorf("ShouldIgnorePath(%q, isDir=%v) = %v, want %v", tc.path, tc.isDir, got, tc.want)
		}
	}

	// Excluded ancestor directory blocks re-inclusion of nested files
	dirM, err := NewIgnoreMatcher([]string{"out/", "!out/keep.go"})
	if err != nil {
		t.Fatal(err)
	}
	if !dirM.ShouldIgnore("out/keep.go") {
		t.Fatal("excluded parent dir out/ must keep out/keep.go ignored")
	}

	// Whereas out/* allows re-inclusion because out itself is not excluded
	starM, err := NewIgnoreMatcher([]string{"out/*", "!out/keep.go"})
	if err != nil {
		t.Fatal(err)
	}
	if starM.ShouldIgnore("out/keep.go") {
		t.Fatal("!out/keep.go should un-ignore under non-excluded parent out/*")
	}
	if !starM.ShouldIgnore("out/drop.go") {
		t.Fatal("out/drop.go should remain ignored")
	}

	// Child ignore matcher scoping
	child, err := m.Child("pkg/sub", []string{"!debug.log", "*.gen.go"})
	if err != nil {
		t.Fatal(err)
	}
	if child.ShouldIgnore("pkg/sub/debug.log") {
		t.Fatal("child negation !debug.log should apply inside pkg/sub")
	}
	if !child.ShouldIgnore("pkg/other/debug.log") {
		t.Fatal("child negation under pkg/sub must not affect pkg/other")
	}
	if !child.ShouldIgnore("pkg/sub/deep/foo.gen.go") {
		t.Fatal("child rule *.gen.go should match under pkg/sub")
	}
	if child.ShouldIgnore("root.gen.go") {
		t.Fatal("child rule *.gen.go must not affect files outside pkg/sub")
	}
}

func TestLoadRepoIgnoreMatcherLayeringAndErrors(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*.tmp\n!keep.tmp\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".ggctxignore"), []byte("*.generated.go\nkeep.tmp\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultConfig()
	m, err := LoadRepoIgnoreMatcher(dir, cfg)
	if err != nil {
		t.Fatalf("LoadRepoIgnoreMatcher failed: %v", err)
	}
	if !m.ShouldIgnore("foo.tmp") || !m.ShouldIgnore("keep.tmp") || !m.ShouldIgnore("api.generated.go") {
		t.Fatal(".ggctxignore did not layer on top of .gitignore")
	}
	if !m.ShouldIgnore("node_modules/a.js") {
		t.Fatal("default ignores missing from LoadRepoIgnoreMatcher")
	}

	// UseGitignore = false skips .gitignore
	cfg.UseGitignore = false
	mNoGit, err := LoadRepoIgnoreMatcher(dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if mNoGit.ShouldIgnore("foo.tmp") {
		t.Fatal("UseGitignore=false should not load .gitignore")
	}
	if !mNoGit.ShouldIgnore("api.generated.go") {
		t.Fatal("UseGitignore=false should still load .ggctxignore")
	}

	// Custom DataDir ignored automatically
	cfg.DataDir = ".my_index"
	mCustom, err := LoadRepoIgnoreMatcher(dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !mCustom.ShouldIgnore(".my_index/index.ggctx") {
		t.Fatal("custom DataDir should be ignored automatically")
	}

	// Invalid bracket glob pattern rejected cleanly
	if _, err := NewIgnoreMatcher([]string{"[unclosed"}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig on malformed ignore pattern, got %v", err)
	}

	// ChildFromFile missing file returns parent cleanly
	same, err := m.ChildFromFile("sub", filepath.Join(dir, "nonexistent.ignore"))
	if err != nil || same != m {
		t.Fatalf("ChildFromFile missing file = (%v, %v)", same, err)
	}
}
