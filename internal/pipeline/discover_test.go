package pipeline_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/roshankumar0036singh/gographctx/internal/config"
	"github.com/roshankumar0036singh/gographctx/internal/pipeline"
)

func TestDiscoverFilesMixedLanguagesIgnoresAndDeterminism(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		".ggctxignore":               "*.gen.go\nignored_dir/\n!sub/keep.py\n",
		".gitignore":                 "*.tmp.js\n",
		"README.md":                  "# docs\n",
		"cmd/app/main.go":            "package main\nfunc main() {}\n",
		"internal/types.gen.go":      "package internal\n",
		"ignored_dir/skip.py":        "x = 1\n",
		"pkg/service.py":             "def run(): pass\n",
		"sub/.ggctxignore":           "secret.ts\n",
		"sub/keep.py":                "x = 2\n",
		"sub/secret.ts":              "export const k = 1;\n",
		"web/app.tsx":                "export function App() { return null; }\n",
		"web/temp.tmp.js":            "var a = 1;\n",
		"node_modules/dep/index.js":  "module.exports = {};\n",
		"crates/core/src/lib.rs":     "pub fn ping() {}\n",
		"jvm/src/com/acme/Main.java": "package com.acme; class Main {}\n",
		"scripts/run.sh":             "echo hi\n",
	}
	for rel, body := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll(%q) failed: %v", full, err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatalf("WriteFile(%q) failed: %v", full, err)
		}
	}

	cfg := config.DefaultConfig()
	res, err := pipeline.DiscoverFiles(context.Background(), root, cfg, nil)
	if err != nil {
		t.Fatalf("DiscoverFiles failed: %v", err)
	}

	var rels []string
	for _, f := range res.Files {
		rels = append(rels, f.RelPath)
	}

	wantRels := []string{
		"cmd/app/main.go",
		"crates/core/src/lib.rs",
		"jvm/src/com/acme/Main.java",
		"pkg/service.py",
		"sub/keep.py",
		"web/app.tsx",
	}
	if !slices.Equal(rels, wantRels) {
		t.Fatalf("discovered files = %v, want %v (stats=%+v)", rels, wantRels, res.Stats)
	}
	if res.Stats.MatchedFiles != len(wantRels) {
		t.Fatalf("MatchedFiles = %d, want %d", res.Stats.MatchedFiles, len(wantRels))
	}
	if res.Stats.IgnoredFiles < 3 || res.Stats.IgnoredDirs < 2 {
		t.Fatalf("expected ignored counts recorded, got %+v", res.Stats)
	}
	if res.Stats.UnsupportedFiles < 2 { // README.md, scripts/run.sh
		t.Fatalf("expected unsupported counts >= 2, got %+v", res.Stats)
	}
	if res.Stats.CountByLang["python"] != 2 || res.Stats.CountByLang["tsx"] != 1 {
		t.Fatalf("unexpected CountByLang: %+v", res.Stats.CountByLang)
	}

	// Verify language filtering respects cfg.Languages allow-list.
	goOnly := config.DefaultConfig()
	goOnly.Languages = []string{"go"}
	goRes, err := pipeline.DiscoverFiles(context.Background(), root, goOnly, nil)
	if err != nil {
		t.Fatalf("DiscoverFiles(goOnly) failed: %v", err)
	}
	if len(goRes.Files) != 1 || goRes.Files[0].RelPath != "cmd/app/main.go" {
		t.Fatalf("expected only cmd/app/main.go for goOnly, got %+v", goRes.Files)
	}
}

func TestDiscoverFilesSymlinkHandling(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(realDir, "mod.go"), []byte("package real\n"), 0o600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	linkFile := filepath.Join(root, "alias.go")
	if err := os.Symlink(filepath.Join(realDir, "mod.go"), linkFile); err != nil {
		t.Skipf("symlinks not permitted in test environment: %v", err)
	}
	// Add a circular symlink directory to ensure cycle protection holds.
	loopDir := filepath.Join(realDir, "loop")
	if err := os.Symlink(realDir, loopDir); err != nil {
		t.Skipf("directory symlinks not permitted in test environment: %v", err)
	}

	cfg := config.DefaultConfig()
	cfg.FollowSymlinks = false
	skipRes, err := pipeline.DiscoverFiles(context.Background(), root, cfg, nil)
	if err != nil {
		t.Fatalf("DiscoverFiles(FollowSymlinks=false) failed: %v", err)
	}
	if skipRes.Stats.SymlinksSkipped != 2 {
		t.Fatalf("expected 2 symlinks skipped, got %+v", skipRes.Stats)
	}

	cfg.FollowSymlinks = true
	followRes, err := pipeline.DiscoverFiles(context.Background(), root, cfg, nil)
	if err != nil {
		t.Fatalf("DiscoverFiles(FollowSymlinks=true) failed: %v", err)
	}
	if followRes.Stats.MatchedFiles != 2 || followRes.Stats.SymlinksSkipped != 1 {
		t.Fatalf("expected 2 matched files and 1 circular loop skipped, got files=%+v stats=%+v", followRes.Files, followRes.Stats)
	}
}
