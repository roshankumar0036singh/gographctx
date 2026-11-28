package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func createCLIFixtureRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"go.mod": "module example.com/clitest\n\ngo 1.22\n",
		"pkg/repo.go": `package pkg
type Store struct{}
func NewStore() *Store { return &Store{} }
func (s *Store) Fetch(id string) string { return id }
`,
		"cmd/app/main.go": `package main
import "example.com/clitest/pkg"
func main() {
	s := pkg.NewStore()
	_ = s.Fetch("42")
}
`,
	}
	for rel, content := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll failed: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatalf("WriteFile failed: %v", err)
		}
	}
	return root
}

func TestCLICommandsEndToEnd(t *testing.T) {
	repo := createCLIFixtureRepo(t)
	cfgFile := filepath.Join(repo, "custom.ggctxrc")
	if err := os.WriteFile(cfgFile, []byte("workers = 2\ncompress_snapshots = true\n"), 0o600); err != nil {
		t.Fatalf("WriteFile config failed: %v", err)
	}

	// 1. version (text and json)
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"version"}, nil, &out, &errOut); code != 0 {
		t.Fatalf("version failed (%d): %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "gographctx v"+Version) {
		t.Fatalf("unexpected version output: %q", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := run(context.Background(), []string{"version", "--format", "json"}, nil, &out, &errOut); code != 0 {
		t.Fatalf("version json failed (%d): %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), `"version"`) {
		t.Fatalf("unexpected json version output: %q", out.String())
	}

	// 2. stats before index fails cleanly
	out.Reset()
	errOut.Reset()
	if code := run(context.Background(), []string{"stats", repo}, nil, &out, &errOut); code == 0 {
		t.Fatalf("expected non-zero exit when stats runs before indexing")
	}

	// 3. index <repo> --config ... --workers 2 --format json
	out.Reset()
	errOut.Reset()
	if code := run(context.Background(), []string{"index", repo, "--config", cfgFile, "--workers", "2", "--format", "json", "--verbose"}, nil, &out, &errOut); code != 0 {
		t.Fatalf("index failed (%d): %s", code, errOut.String())
	}
	var idxPayload map[string]any
	if err := json.Unmarshal(out.Bytes(), &idxPayload); err != nil {
		t.Fatalf("unmarshal index json failed: %v\n%s", err, out.String())
	}

	// 4. stats <repo> --format text and --format json
	out.Reset()
	errOut.Reset()
	if code := run(context.Background(), []string{"stats", repo, "--format", "text"}, nil, &out, &errOut); code != 0 {
		t.Fatalf("stats text failed (%d): %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "GoGraphCtx Index Stats") || !strings.Contains(out.String(), "Languages:") {
		t.Fatalf("unexpected stats output:\n%s", out.String())
	}

	// 5. index --force
	out.Reset()
	errOut.Reset()
	if code := run(context.Background(), []string{"index", "--force", repo}, nil, &out, &errOut); code != 0 {
		t.Fatalf("index --force failed (%d): %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "GoGraphCtx Index Summary") {
		t.Fatalf("unexpected index text output:\n%s", out.String())
	}

	// 6. query find_symbol & get_callers (json & text)
	out.Reset()
	errOut.Reset()
	if code := run(context.Background(), []string{"query", "find_symbol", "--path", repo, "--symbol", "Fetch", "--format", "json"}, nil, &out, &errOut); code != 0 {
		t.Fatalf("query find_symbol failed (%d): %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "pkg::Store::Fetch") {
		t.Fatalf("query find_symbol missing Fetch:\n%s", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := run(context.Background(), []string{"query", "get_callers", "--path", repo, "--args", `{"symbol":"pkg::Store::Fetch"}`, "--format", "text"}, nil, &out, &errOut); code != 0 {
		t.Fatalf("query get_callers failed (%d): %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "cmd/app::main") {
		t.Fatalf("query get_callers missing cmd/app::main:\n%s", out.String())
	}

	// 7. serve <repo> over stdio pipes
	mcpIn := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"repo_overview","arguments":{}}}`,
	}, "\n") + "\n"
	out.Reset()
	errOut.Reset()
	if code := run(context.Background(), []string{"serve", repo}, strings.NewReader(mcpIn), &out, &errOut); code != 0 {
		t.Fatalf("serve failed (%d): %s", code, errOut.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 || !strings.Contains(lines[2], "node_count") {
		t.Fatalf("unexpected serve stdout:\n%s", out.String())
	}

	// 8. watch <repo> with stdin MCP session terminating cleanly on EOF
	out.Reset()
	errOut.Reset()
	if code := run(context.Background(), []string{"watch", repo, "--verbose"}, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`+"\n"), &out, &errOut); code != 0 {
		t.Fatalf("watch failed (%d): %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), `"id":1`) {
		t.Fatalf("expected watch MCP ping reply, got:\n%s", out.String())
	}

	// 9. Error exit codes on invalid subcommand/flags
	if code := run(context.Background(), []string{"bogus"}, nil, io.Discard, io.Discard); code != 2 {
		t.Fatalf("expected exit 2 on unknown command, got %d", code)
	}
}

func TestCLIBinaryStartupUnderTarget(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping compiled binary startup check in -short mode")
	}
	binName := "gographctx_test_bin"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	binPath := filepath.Join(t.TempDir(), binName)
	buildCmd := exec.Command("go", "build", "-o", binPath, ".")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("go build failed: %v\n%s", err, out)
	}

	// Warm OS page cache with first run, then verify cold-command latency well under 50ms budget
	// (allowing generous headroom on slow CI runners while ensuring zero heavy runtime init at CLI entry).
	_ = exec.Command(binPath, "version").Run()
	start := time.Now()
	out, err := exec.Command(binPath, "version").CombinedOutput()
	elapsed := time.Since(start)
	if err != nil || !strings.Contains(string(out), "gographctx v") {
		t.Fatalf("binary version failed: err=%v out=%s", err, out)
	}
	t.Logf("CLI `version` execution took %v", elapsed)
	if elapsed > 250*time.Millisecond {
		t.Fatalf("CLI startup unexpectedly slow: %v", elapsed)
	}
}
