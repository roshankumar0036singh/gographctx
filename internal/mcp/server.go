package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// ServerVersion identifies the GoGraphCtx MCP server implementation.
const ServerVersion = "0.1.0"

// DefaultProtocolVersion matches standard MCP JSON-RPC stdio negotiation.
const DefaultProtocolVersion = "2024-11-05"

const (
	rpcErrParseError     = -32700
	rpcErrInvalidRequest = -32600
	rpcErrMethodNotFound = -32601
	rpcErrInvalidParams  = -32602
	rpcErrInternal       = -32603
)

// RPCRequest represents an incoming JSON-RPC 2.0 request or notification.
type RPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// RPCError represents a standard JSON-RPC 2.0 error payload.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// RPCResponse represents an outgoing JSON-RPC 2.0 response.
type RPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// TextContent is an MCP tool result text block.
type TextContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// CallToolResult wraps formatted tool JSON output per MCP specification.
type CallToolResult struct {
	Content []TextContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

// Server serves MCP JSON-RPC 2.0 messages over stdio streams, routing protocol
// lifecycle methods and tools/list + tools/call to Handler.
type Server struct {
	handler *Handler
	in      io.Reader
	out     io.Writer
	errOut  io.Writer
	version string

	writeMu sync.Mutex
}

// NewServer constructs an MCP stdio Server wired to handler and IO streams.
func NewServer(handler *Handler, in io.Reader, out io.Writer, errOut io.Writer) *Server {
	if handler == nil {
		handler = NewHandler(nil, FormatterConfig{})
	}
	if in == nil {
		in = os.Stdin
	}
	if out == nil {
		out = os.Stdout
	}
	if errOut == nil {
		errOut = os.Stderr
	}
	return &Server{
		handler: handler,
		in:      in,
		out:     out,
		errOut:  errOut,
		version: ServerVersion,
	}
}

// ServeStdio runs Server against os.Stdin/os.Stdout/os.Stderr with SIGINT/SIGTERM shutdown.
func ServeStdio(ctx context.Context, handler *Handler) error {
	if ctx == nil {
		ctx = context.Background()
	}
	sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := NewServer(handler, os.Stdin, os.Stdout, os.Stderr)
	return srv.Serve(sigCtx)
}

// Serve processes JSON-RPC messages from in until EOF or ctx cancellation.
func (s *Server) Serve(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	br := bufio.NewReaderSize(s.in, 64*1024)
	type readResult struct {
		payload []byte
		framed  bool
		err     error
	}
	msgCh := make(chan readResult)
	go func() {
		defer close(msgCh)
		for {
			payload, framed, err := readRPCMessage(br)
			select {
			case msgCh <- readResult{payload: payload, framed: framed, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case item, ok := <-msgCh:
			if !ok {
				return nil
			}
			if item.err != nil {
				if errors.Is(item.err, io.EOF) {
					return nil
				}
				return fmt.Errorf("mcp read message: %w", item.err)
			}
			if len(item.payload) == 0 {
				continue
			}
			if resp := s.handleRawMessage(ctx, item.payload); resp != nil {
				if err := s.writeResponse(resp, item.framed); err != nil {
					return fmt.Errorf("mcp write response: %w", err)
				}
			}
		}
	}
}

func (s *Server) handleRawMessage(ctx context.Context, raw []byte) *RPCResponse {
	var req RPCRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return &RPCResponse{
			JSONRPC: "2.0",
			ID:      json.RawMessage("null"),
			Error: &RPCError{
				Code:    rpcErrParseError,
				Message: "invalid JSON-RPC payload",
				Data:    err.Error(),
			},
		}
	}

	isNotification := isNullOrEmptyID(req.ID)
	if req.JSONRPC != "2.0" || strings.TrimSpace(req.Method) == "" {
		if isNotification {
			return nil
		}
		return &RPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error: &RPCError{
				Code:    rpcErrInvalidRequest,
				Message: "expected jsonrpc=\"2.0\" and non-empty method",
			},
		}
	}

	// JSON-RPC notifications must never emit a response.
	if isNotification {
		if req.Method != "notifications/initialized" && req.Method != "notifications/cancelled" {
			_, _ = fmt.Fprintf(s.errOut, "mcp notification: %s\n", req.Method)
		}
		return nil
	}

	result, rpcErr := s.routeMethod(ctx, req.Method, req.Params)
	resp := &RPCResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
	}
	if rpcErr != nil {
		resp.Error = rpcErr
	} else {
		resp.Result = result
	}
	return resp
}

func (s *Server) routeMethod(ctx context.Context, method string, params json.RawMessage) (any, *RPCError) {
	switch method {
	case "initialize":
		return s.handleInitialize(params)
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": AllToolDefinitions()}, nil
	case "tools/call":
		return s.handleToolsCall(ctx, params)
	default:
		return nil, &RPCError{
			Code:    rpcErrMethodNotFound,
			Message: fmt.Sprintf("method %q not found", method),
		}
	}
}

func (s *Server) handleInitialize(params json.RawMessage) (any, *RPCError) {
	var initReq struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if len(params) > 0 && string(params) != "null" {
		if err := json.Unmarshal(params, &initReq); err != nil {
			return nil, &RPCError{
				Code:    rpcErrInvalidParams,
				Message: "invalid initialize params",
				Data:    err.Error(),
			}
		}
	}
	proto := DefaultProtocolVersion
	switch initReq.ProtocolVersion {
	case "2024-11-05", "2025-03-26", "2025-06-18":
		proto = initReq.ProtocolVersion
	}

	return map[string]any{
		"protocolVersion": proto,
		"capabilities": map[string]any{
			"tools": map[string]any{
				"listChanged": false,
			},
		},
		"serverInfo": map[string]any{
			"name":    "gographctx",
			"version": s.version,
		},
	}, nil
}

func (s *Server) handleToolsCall(ctx context.Context, params json.RawMessage) (any, *RPCError) {
	var callReq struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if len(params) == 0 {
		return nil, &RPCError{Code: rpcErrInvalidParams, Message: "missing tools/call params"}
	}
	if err := json.Unmarshal(params, &callReq); err != nil || strings.TrimSpace(callReq.Name) == "" {
		return nil, &RPCError{Code: rpcErrInvalidParams, Message: "tools/call requires non-empty name"}
	}

	outBytes, err := s.handler.HandleCall(ctx, callReq.Name, callReq.Arguments)
	if err != nil {
		if errors.Is(err, ErrUnknownTool) {
			return nil, &RPCError{Code: rpcErrInvalidParams, Message: err.Error()}
		}
		if errors.Is(err, ErrInvalidArgs) {
			return CallToolResult{
				Content: []TextContent{{Type: "text", Text: err.Error()}},
				IsError: true,
			}, nil
		}
		return nil, &RPCError{Code: rpcErrInternal, Message: err.Error()}
	}

	return CallToolResult{
		Content: []TextContent{{Type: "text", Text: string(outBytes)}},
	}, nil
}

func (s *Server) writeResponse(resp *RPCResponse, contentLengthFramed bool) error {
	data, err := json.Marshal(resp)
	if err != nil {
		return err
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	if contentLengthFramed {
		header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(data))
		if _, err := io.WriteString(s.out, header); err != nil {
			return err
		}
		_, err = s.out.Write(data)
		return err
	}

	if _, err := s.out.Write(data); err != nil {
		return err
	}
	_, err = io.WriteString(s.out, "\n")
	return err
}

const maxRPCMessageBytes = 32 << 20 // 32 MiB defensive framing cap

// readRPCMessage supports both standard MCP newline-delimited JSON and LSP-style
// Content-Length header framing transparently.
func readRPCMessage(br *bufio.Reader) (payload []byte, framed bool, err error) {
	for {
		line, readErr := br.ReadString('\n')
		if readErr != nil && len(line) == 0 {
			return nil, false, readErr
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			if readErr != nil {
				return nil, false, readErr
			}
			continue
		}

		// Check Content-Length header framing.
		if len(trimmed) > 15 && strings.EqualFold(trimmed[:15], "content-length:") {
			lengthStr := strings.TrimSpace(trimmed[15:])
			n, convErr := strconv.Atoi(lengthStr)
			if convErr != nil || n < 0 || n > maxRPCMessageBytes {
				return nil, true, fmt.Errorf("invalid Content-Length header %q", trimmed)
			}
			// Consume remaining headers up to blank line.
			for {
				hdrLine, hdrErr := br.ReadString('\n')
				if strings.TrimSpace(hdrLine) == "" {
					break
				}
				if hdrErr != nil {
					return nil, true, hdrErr
				}
			}
			body := make([]byte, n)
			if _, err := io.ReadFull(br, body); err != nil {
				return nil, true, err
			}
			return body, true, nil
		}

		return []byte(trimmed), false, nil
	}
}

func isNullOrEmptyID(id json.RawMessage) bool {
	s := strings.TrimSpace(string(id))
	return s == "" || s == "null"
}
