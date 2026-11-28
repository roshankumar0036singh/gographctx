package mcp

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"testing"
)

func FuzzMCPReadRPCMessage(f *testing.F) {
	f.Add([]byte("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"ping\"}\n"))
	f.Add([]byte("Content-Length: 40\r\n\r\n{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"ping\"}"))
	f.Add([]byte("Content-Length: 999999999999999\r\n\r\n{}"))
	f.Add([]byte("Content-Length: -5\r\n\r\n"))
	f.Add([]byte("\n   \n{bad json\n"))

	f.Fuzz(func(t *testing.T, data []byte) {
		br := bufio.NewReader(bytes.NewReader(data))
		for i := 0; i < 8; i++ {
			_, _, err := readRPCMessage(br)
			if err != nil {
				break
			}
		}
		var out bytes.Buffer
		srv := NewServer(nil, bytes.NewReader(data), &out, io.Discard)
		_ = srv.Serve(context.Background())
	})
}
