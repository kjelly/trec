package main

// A deliberately small stdio MCP transport.  It has no dependency on an MCP
// SDK so `trec mcp` remains a single binary.  Never write diagnostics to
// stdout here: stdout is JSON-RPC only.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/spf13/cobra"
)

type mcpSession struct {
	in       io.WriteCloser
	cmd      *exec.Cmd
	out, err bytes.Buffer
	mu       sync.Mutex
}
type mcpServer struct {
	mu       sync.Mutex
	next     int
	sessions map[string]*mcpSession
}

func newMCPCommand() *cobra.Command {
	return &cobra.Command{Use: "mcp", Short: "Run trec as a stdio MCP server", Args: cobra.NoArgs, Run: func(*cobra.Command, []string) { runMCP() }}
}

func runMCP() {
	s := &mcpServer{sessions: map[string]*mcpSession{}}
	server := mcpserver.NewMCPServer("trec", appVersion, mcpserver.WithToolCapabilities(false), mcpserver.WithRecovery())
	add := func(name, description string, tool mcp.Tool) {
		server.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args, _ := json.Marshal(req.GetArguments())
			value, err := s.tool(name, args)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			out, _ := json.Marshal(value)
			return mcp.NewToolResultText(string(out)), nil
		})
	}
	add("run", "Run any local command once, including any trec subcommand.", mcp.NewTool("run", mcp.WithDescription("Run any local command once."), mcp.WithArray("command", mcp.Required()), mcp.WithString("stdin"), mcp.WithNumber("timeout_seconds"), mcp.WithString("working_directory")))
	add("terminal_start", "Start a persistent terminal process.", mcp.NewTool("terminal_start", mcp.WithDescription("Start a persistent terminal process."), mcp.WithArray("command", mcp.Required()), mcp.WithString("working_directory")))
	add("terminal_write", "Write to a persistent terminal process.", mcp.NewTool("terminal_write", mcp.WithDescription("Write to a persistent terminal process."), mcp.WithString("session_id", mcp.Required()), mcp.WithString("data", mcp.Required())))
	add("terminal_read", "Read a persistent terminal process.", mcp.NewTool("terminal_read", mcp.WithDescription("Read a persistent terminal process."), mcp.WithString("session_id", mcp.Required())))
	add("terminal_close", "Close a persistent terminal process.", mcp.NewTool("terminal_close", mcp.WithDescription("Close a persistent terminal process."), mcp.WithString("session_id", mcp.Required())))
	add("session_list", "List persistent terminal sessions.", mcp.NewTool("session_list", mcp.WithDescription("List persistent terminal sessions.")))
	if err := mcpserver.ServeStdio(server); err != nil {
		fmt.Fprintln(os.Stderr, "trec mcp:", err)
	}
}

func (s *mcpServer) handle(method string, raw json.RawMessage) (any, error) {
	switch method {
	case "initialize":
		version := "2024-11-05"
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(raw, &p)
		for _, supported := range []string{"2025-06-18", "2025-03-26", "2024-11-05"} {
			if p.ProtocolVersion == supported {
				version = supported
				break
			}
		}
		return map[string]any{"protocolVersion": version, "capabilities": map[string]any{"tools": map[string]any{"listChanged": false}, "resources": map[string]any{"listChanged": false}, "prompts": map[string]any{}}, "serverInfo": map[string]string{"name": "trec", "version": appVersion}}, nil
	case "ping":
		return map[string]any{}, nil
	case "notifications/initialized":
		return nil, nil
	case "resources/list":
		return map[string]any{"resources": []any{}}, nil
	case "resources/templates/list":
		return map[string]any{"resourceTemplates": []any{}}, nil
	case "prompts/list":
		return map[string]any{"prompts": []any{}}, nil
	case "tools/list":
		return map[string]any{"tools": []map[string]any{
			{"name": "run", "description": "Run any local command once; use trec -o <cast-path> to choose a recording name. working_directory resolves relative paths.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"command": map[string]string{"type": "array"}, "stdin": map[string]string{"type": "string"}, "timeout_seconds": map[string]string{"type": "integer"}, "working_directory": map[string]string{"type": "string"}}, "required": []string{"command"}}},
			{"name": "terminal_start", "description": "Start any local command under a PTY and retain its stdin/stdout. Use trec -o <cast-path> to choose a recording name.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"command": map[string]string{"type": "array"}, "working_directory": map[string]string{"type": "string"}}, "required": []string{"command"}}},
			{"name": "terminal_write", "description": "Write bytes to a retained terminal session.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"session_id": map[string]string{"type": "string"}, "data": map[string]string{"type": "string"}}, "required": []string{"session_id", "data"}}},
			{"name": "terminal_read", "description": "Read accumulated stdout/stderr and process state.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"session_id": map[string]string{"type": "string"}}, "required": []string{"session_id"}}},
			{"name": "terminal_close", "description": "Terminate a retained session.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"session_id": map[string]string{"type": "string"}}, "required": []string{"session_id"}}},
			{"name": "session_list", "description": "List retained sessions.", "inputSchema": map[string]any{"type": "object"}}}}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		v, e := s.tool(p.Name, p.Arguments)
		if e != nil {
			return map[string]any{"content": []map[string]string{{"type": "text", "text": e.Error()}}, "isError": true}, nil
		}
		b, _ := json.Marshal(v)
		return map[string]any{"content": []map[string]string{{"type": "text", "text": string(b)}}}, nil
	default:
		return nil, fmt.Errorf("unsupported MCP method %q", method)
	}
}

func commandFrom(raw json.RawMessage) ([]string, string, int, string, error) {
	var a struct {
		Command []string `json:"command"`
		Stdin   string   `json:"stdin"`
		Timeout int      `json:"timeout_seconds"`
		Dir     string   `json:"working_directory"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, "", 0, "", err
	}
	if len(a.Command) == 0 {
		return nil, "", 0, "", fmt.Errorf("command is required")
	}
	if a.Dir != "" {
		if info, err := os.Stat(a.Dir); err != nil || !info.IsDir() {
			return nil, "", 0, "", fmt.Errorf("working_directory %q is not a directory", a.Dir)
		}
	}
	return a.Command, a.Stdin, a.Timeout, a.Dir, nil
}
func (s *mcpServer) tool(name string, raw json.RawMessage) (any, error) {
	switch name {
	case "run":
		a, in, to, dir, e := commandFrom(raw)
		if e != nil {
			return nil, e
		}
		c := exec.Command(a[0], a[1:]...)
		c.Dir = dir
		c.Stdin = bytes.NewBufferString(in)
		var out, er bytes.Buffer
		c.Stdout = &out
		c.Stderr = &er
		if to > 0 {
			timer := time.AfterFunc(time.Duration(to)*time.Second, func() { _ = c.Process.Kill() })
			e = c.Run()
			timer.Stop()
		} else {
			e = c.Run()
		}
		code := 0
		if e != nil {
			code = 1
			if x, ok := e.(*exec.ExitError); ok {
				code = x.ExitCode()
			}
		}
		return map[string]any{"stdout": out.String(), "stderr": er.String(), "exit_code": code}, nil
	case "terminal_start":
		a, _, _, dir, e := commandFrom(raw)
		if e != nil {
			return nil, e
		}
		c := exec.Command(a[0], a[1:]...)
		c.Dir = dir
		ptmx, e := pty.Start(c)
		if e != nil {
			return nil, e
		}
		ss := &mcpSession{in: ptmx, cmd: c}
		go func() {
			buf := make([]byte, 4096)
			for {
				n, err := ptmx.Read(buf)
				if n > 0 {
					ss.mu.Lock()
					ss.out.Write(buf[:n])
					ss.mu.Unlock()
				}
				if err != nil {
					break
				}
			}
		}()
		s.mu.Lock()
		s.next++
		id := "session-" + strconv.Itoa(s.next)
		s.sessions[id] = ss
		s.mu.Unlock()
		go func() { _ = c.Wait() }()
		return map[string]string{"session_id": id}, nil
	case "terminal_write":
		var a struct {
			SessionID string `json:"session_id"`
			Data      string `json:"data"`
		}
		if e := json.Unmarshal(raw, &a); e != nil {
			return nil, e
		}
		ss, e := s.session(a.SessionID)
		if e != nil {
			return nil, e
		}
		ss.mu.Lock()
		_, e = io.WriteString(ss.in, a.Data)
		ss.mu.Unlock()
		return map[string]any{"written": len(a.Data)}, e
	case "terminal_read":
		var a struct {
			SessionID string `json:"session_id"`
		}
		if e := json.Unmarshal(raw, &a); e != nil {
			return nil, e
		}
		ss, e := s.session(a.SessionID)
		if e != nil {
			return nil, e
		}
		ss.mu.Lock()
		o, er := ss.out.String(), ss.err.String()
		ss.out.Reset()
		ss.err.Reset()
		ss.mu.Unlock()
		running := ss.cmd.ProcessState == nil
		code := 0
		if !running {
			code = ss.cmd.ProcessState.ExitCode()
		}
		return map[string]any{"stdout": o, "stderr": er, "running": running, "exit_code": code}, nil
	case "terminal_close":
		var a struct {
			SessionID string `json:"session_id"`
		}
		if e := json.Unmarshal(raw, &a); e != nil {
			return nil, e
		}
		ss, e := s.session(a.SessionID)
		if e != nil {
			return nil, e
		}
		_ = ss.in.Close()
		_ = ss.cmd.Process.Kill()
		s.mu.Lock()
		delete(s.sessions, a.SessionID)
		s.mu.Unlock()
		return map[string]bool{"closed": true}, nil
	case "session_list":
		s.mu.Lock()
		defer s.mu.Unlock()
		ids := make([]string, 0, len(s.sessions))
		for id := range s.sessions {
			ids = append(ids, id)
		}
		return map[string]any{"sessions": ids}, nil
	}
	return nil, fmt.Errorf("unknown tool %q", name)
}
func (s *mcpServer) session(id string) (*mcpSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss := s.sessions[id]
	if ss == nil {
		return nil, fmt.Errorf("unknown session %q", id)
	}
	return ss, nil
}
