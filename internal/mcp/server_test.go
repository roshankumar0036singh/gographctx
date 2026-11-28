package mcp_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/roshankumar0036singh/gographctx/internal/graph"
	"github.com/roshankumar0036singh/gographctx/internal/mcp"
)

func TestServerStdioProtocolRouting(t *testing.T) {
	g := graph.New()
	fnNode := graph.Node{
		Kind:       graph.KindFunction,
		Name:       "ProcessOrder",
		QName:      "service::ProcessOrder",
		File:       "service/order.go",
		StartLine:  10,
		EndLine:    25,
		Language:   "go",
		Exported:   true,
		Complexity: 3,
	}
	if _, err := g.AddNode(fnNode); err != nil {
		t.Fatalf("AddNode failed: %v", err)
	}

	h := mcp.NewHandler(g, mcp.FormatterConfig{
		RepoName:       "demo",
		FixedTimestamp: "2026-11-28T00:00:00Z",
	})

	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"find_symbol","arguments":{"name":"Process*"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"unknown/method"}`,
		`{bad json`,
	}, "\n") + "\n"

	var outBuf, errBuf bytes.Buffer
	srv := mcp.NewServer(h, strings.NewReader(input), &outBuf, &errBuf)
	if err := srv.Serve(context.Background()); err != nil {
		t.Fatalf("Serve failed: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(outBuf.String()), "\n")
	// 5 valid ID requests + 1 parse error = 6 responses (notification emits 0 responses).
	if len(lines) != 6 {
		t.Fatalf("expected 6 JSON-RPC responses, got %d:\n%s", len(lines), outBuf.String())
	}

	// 1. initialize response
	var initResp mcp.RPCResponse
	if err := json.Unmarshal([]byte(lines[0]), &initResp); err != nil {
		t.Fatalf("unmarshal initialize resp: %v", err)
	}
	if initResp.Error != nil {
		t.Fatalf("unexpected initialize error: %+v", initResp.Error)
	}
	initResult, _ := json.Marshal(initResp.Result)
	if !strings.Contains(string(initResult), `"protocolVersion":"2025-03-26"`) || !strings.Contains(string(initResult), `"tools"`) {
		t.Fatalf("unexpected initialize payload: %s", initResult)
	}

	// 2. ping response
	var pingResp mcp.RPCResponse
	if err := json.Unmarshal([]byte(lines[1]), &pingResp); err != nil || pingResp.Error != nil {
		t.Fatalf("unexpected ping resp: %+v err=%v", pingResp, err)
	}

	// 3. tools/list response contains all 7 tools
	var listResp mcp.RPCResponse
	if err := json.Unmarshal([]byte(lines[2]), &listResp); err != nil || listResp.Error != nil {
		t.Fatalf("unexpected tools/list resp: %+v err=%v", listResp, err)
	}
	listJSON, _ := json.Marshal(listResp.Result)
	for _, toolName := range []string{"find_symbol", "get_callers", "get_callees", "get_impact", "file_outline", "search_code", "repo_overview"} {
		if !strings.Contains(string(listJSON), `"`+toolName+`"`) {
			t.Fatalf("tools/list missing %q: %s", toolName, listJSON)
		}
	}

	// 4. tools/call find_symbol result
	var callResp mcp.RPCResponse
	if err := json.Unmarshal([]byte(lines[3]), &callResp); err != nil || callResp.Error != nil {
		t.Fatalf("unexpected tools/call resp: %+v err=%v", callResp, err)
	}
	callJSON, _ := json.Marshal(callResp.Result)
	if !strings.Contains(string(callJSON), "service::ProcessOrder") {
		t.Fatalf("tools/call output missing ProcessOrder: %s", callJSON)
	}

	// 5. unknown method -> -32601
	var methErr mcp.RPCResponse
	_ = json.Unmarshal([]byte(lines[4]), &methErr)
	if methErr.Error == nil || methErr.Error.Code != -32601 {
		t.Fatalf("expected -32601 method not found, got %+v", methErr)
	}

	// 6. parse error -> -32700
	var parseErr mcp.RPCResponse
	_ = json.Unmarshal([]byte(lines[5]), &parseErr)
	if parseErr.Error == nil || parseErr.Error.Code != -32700 {
		t.Fatalf("expected -32700 parse error, got %+v", parseErr)
	}
}

func TestServerContentLengthFramingAndCancelShutdown(t *testing.T) {
	srv := mcp.NewServer(mcp.NewHandler(graph.New(), mcp.FormatterConfig{}), nil, nil, nil)
	_ = srv

	pr, pw := io.Pipe()
	outR, outW := io.Pipe()
	defer func() {
		_ = pw.Close()
		_ = outR.Close()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	s := mcp.NewServer(mcp.NewHandler(graph.New(), mcp.FormatterConfig{}), pr, outW, io.Discard)
	go func() {
		done <- s.Serve(ctx)
	}()

	body := `{"jsonrpc":"2.0","id":"req-cl","method":"ping"}`
	frame := fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body)
	go func() {
		_, _ = io.WriteString(pw, frame)
	}()

	br := bufio.NewReader(outR)
	hdr, err := br.ReadString('\n')
	if err != nil || !strings.HasPrefix(hdr, "Content-Length:") {
		t.Fatalf("expected Content-Length framed reply, got hdr=%q err=%v", hdr, err)
	}
	_, _ = br.ReadString('\n') // blank separator
	var lenVal int
	_, _ = fmt.Sscanf(strings.TrimSpace(hdr), "Content-Length: %d", &lenVal)
	respBytes := make([]byte, lenVal)
	if _, err := io.ReadFull(br, respBytes); err != nil {
		t.Fatalf("ReadFull framed response failed: %v", err)
	}
	if !strings.Contains(string(respBytes), `"req-cl"`) {
		t.Fatalf("unexpected framed response: %s", respBytes)
	}

	// Verify ctx cancellation shuts down Serve promptly even with open stdin.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned error on cancel: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Serve did not shut down cleanly on context cancel")
	}
}
