package mcp_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/roshankumar0036singh/gographctx/internal/config"
	"github.com/roshankumar0036singh/gographctx/internal/mcp"
	"github.com/roshankumar0036singh/gographctx/internal/pipeline"
)

func writeProjectFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, src := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll(%q) failed: %v", full, err)
		}
		if err := os.WriteFile(full, []byte(src), 0o600); err != nil {
			t.Fatalf("WriteFile(%q) failed: %v", full, err)
		}
	}
	return root
}

func runMCPStdioSession(t *testing.T, repoDir, repoName string, toolBudgets map[string]int, requests []string) []mcp.RPCResponse {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Workers = 2
	idxRes, err := pipeline.IndexRepository(context.Background(), repoDir, cfg, pipeline.IndexOptions{Force: true})
	if err != nil {
		t.Fatalf("IndexRepository(%q) failed: %v", repoName, err)
	}

	h := mcp.NewHandler(idxRes.Graph, mcp.FormatterConfig{
		RepoRoot:       repoDir,
		RepoName:       repoName,
		ToolBudgets:    toolBudgets,
		FixedTimestamp: "2026-11-28T00:00:00Z",
	})

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := mcp.NewServer(h, inR, outW, io.Discard)
	srvDone := make(chan error, 1)
	go func() {
		defer close(srvDone)
		srvDone <- srv.Serve(ctx)
		_ = outW.Close()
	}()

	go func() {
		for _, r := range requests {
			_, _ = io.WriteString(inW, r+"\n")
		}
		_ = inW.Close()
	}()

	var resps []mcp.RPCResponse
	sc := bufio.NewScanner(outR)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var r mcp.RPCResponse
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("invalid RPCResponse JSON %q: %v", sc.Text(), err)
		}
		resps = append(resps, r)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan MCP stdout failed: %v", err)
	}
	if err := <-srvDone; err != nil {
		t.Fatalf("Serve failed: %v", err)
	}
	return resps
}

func extractFormattedResponse(t *testing.T, resp mcp.RPCResponse) ([]byte, mcp.FormattedResponse) {
	t.Helper()
	if resp.Error != nil {
		t.Fatalf("unexpected RPC error: %+v", resp.Error)
	}
	rawRes, _ := json.Marshal(resp.Result)
	var callRes mcp.CallToolResult
	if err := json.Unmarshal(rawRes, &callRes); err != nil || len(callRes.Content) == 0 {
		t.Fatalf("expected CallToolResult with text content, got %s (err=%v)", rawRes, err)
	}
	textBytes := []byte(callRes.Content[0].Text)
	var formatted mcp.FormattedResponse
	if err := json.Unmarshal(textBytes, &formatted); err != nil {
		t.Fatalf("unmarshal FormattedResponse failed: %v\n%s", err, textBytes)
	}
	return textBytes, formatted
}

func assertGoldenJSON(t *testing.T, goldenName string, actual []byte) {
	t.Helper()
	goldenPath := filepath.Join("testdata", goldenName)

	var prettyActual bytesPrettyBuf
	prettyActual.mustPretty(t, actual)

	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, prettyActual.buf, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden file %q failed (run UPDATE_GOLDEN=1 go test ./internal/mcp to refresh): %v", goldenPath, err)
	}
	normActual := strings.ReplaceAll(strings.TrimSpace(string(prettyActual.buf)), "\r\n", "\n")
	normWant := strings.ReplaceAll(strings.TrimSpace(string(want)), "\r\n", "\n")
	if normActual != normWant {
		t.Fatalf("golden mismatch for %s:\n--- WANT ---\n%s\n--- GOT ---\n%s", goldenName, normWant, normActual)
	}
}

type bytesPrettyBuf struct {
	buf []byte
}

func (b *bytesPrettyBuf) mustPretty(t *testing.T, raw []byte) {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("json.Unmarshal for golden failed: %v", err)
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("json.MarshalIndent failed: %v", err)
	}
	b.buf = append(out, '\n')
}

func TestMCPIntegrationScenariosAndGoldenOutputs(t *testing.T) {
	// Scenario 1: Go project -> find_symbol
	goRepo := writeProjectFiles(t, map[string]string{
		"go.mod": "module github.com/acme/orders\n\ngo 1.22\n",
		"pkg/repo.go": `package pkg
type Store struct{}
func NewStore() *Store { return &Store{} }
func (s *Store) Load(id string) string { return id }
`,
		"cmd/app/main.go": `package main
import "github.com/acme/orders/pkg"
func main() {
	s := pkg.NewStore()
	_ = s.Load("1")
}
`,
	})
	goResps := runMCPStdioSession(t, goRepo, "orders-go", nil, []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"find_symbol","arguments":{"name":"Load"}}}`,
	})
	if len(goResps) != 2 {
		t.Fatalf("expected 2 Go MCP responses, got %d", len(goResps))
	}
	goRaw, goFmt := extractFormattedResponse(t, goResps[1])
	if goFmt.Returned != 1 {
		t.Fatalf("expected 1 Load symbol, got %+v", goFmt)
	}
	assertGoldenJSON(t, "go_find_symbol.golden.json", goRaw)

	// Scenario 2 & 4: Python multi-file project -> get_callers + get_impact
	pyRepo := writeProjectFiles(t, map[string]string{
		"app/__init__.py": "",
		"app/core.py": `def validate(raw: str) -> str:
    return raw.strip()
`,
		"app/service.py": `from .core import validate

def normalize(v: str) -> str:
    return validate(v)
`,
		"app/routes.py": `from .service import normalize

def endpoint(payload: str) -> str:
    return normalize(payload)
`,
	})
	pyResps := runMCPStdioSession(t, pyRepo, "flask-py", nil, []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_callers","arguments":{"symbol":"app.core::validate"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_impact","arguments":{"symbol":"app.core::validate","max_depth":4}}}`,
	})
	if len(pyResps) != 3 {
		t.Fatalf("expected 3 Python MCP responses, got %d", len(pyResps))
	}
	pyCallersRaw, pyCallersFmt := extractFormattedResponse(t, pyResps[1])
	if pyCallersFmt.ConfidenceSummary.Certain != 1 {
		t.Fatalf("expected certain caller confidence, got %+v", pyCallersFmt.ConfidenceSummary)
	}
	assertGoldenJSON(t, "py_get_callers.golden.json", pyCallersRaw)

	pyImpactRaw, pyImpactFmt := extractFormattedResponse(t, pyResps[2])
	if pyImpactFmt.Returned != 2 { // app.service::normalize (depth 1) + app.routes::endpoint (depth 2)
		t.Fatalf("expected 2 transitive impacted symbols, got %+v", pyImpactFmt)
	}
	assertGoldenJSON(t, "py_get_impact.golden.json", pyImpactRaw)

	// Scenario 3 & 5: Mixed Go + Python + TS -> repo_overview + tight budget truncation
	mixRepo := writeProjectFiles(t, map[string]string{
		"go.mod":         "module example.com/mix\n\ngo 1.22\n",
		"backend/svc.go": "package backend\nfunc Handle() {}\nfunc Helper() {}\n",
		"scripts/job.py": "def run_job():\n    pass\n",
		"web/ui.ts":      "export function render(): string { return 'ok'; }\n",
	})
	mixResps := runMCPStdioSession(t, mixRepo, "mixed-polyglot", map[string]int{"find_symbol": 260}, []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"repo_overview","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"find_symbol","arguments":{"kind":"Function"}}}`,
	})
	if len(mixResps) != 3 {
		t.Fatalf("expected 3 mixed MCP responses, got %d", len(mixResps))
	}
	mixOverRaw, _ := extractFormattedResponse(t, mixResps[1])
	assertGoldenJSON(t, "mixed_repo_overview.golden.json", mixOverRaw)

	_, truncFmt := extractFormattedResponse(t, mixResps[2])
	if !truncFmt.Truncated || truncFmt.Metadata.EstimatedTokens > 260 {
		t.Fatalf("expected token-budget truncation <= 260 tokens, got truncated=%v est=%d", truncFmt.Truncated, truncFmt.Metadata.EstimatedTokens)
	}
	if strings.Contains(string(mixOverRaw), mixRepo) {
		t.Fatalf("expected repo-relative paths only, found %q", mixRepo)
	}
}
