package main

// A deliberately small stdio MCP transport.  It has no dependency on an MCP
// SDK so `trec mcp` remains a single binary.  Never write diagnostics to
// stdout here: stdout is JSON-RPC only.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

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
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	enc := json.NewEncoder(os.Stdout)
	for sc.Scan() {
		var r struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			continue
		}
		if len(r.ID) == 0 {
			continue
		}
		result, e := s.handle(r.Method, r.Params)
		resp := map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(r.ID)}
		if e != nil {
			resp["error"] = map[string]any{"code": -32000, "message": e.Error()}
		} else {
			resp["result"] = result
		}
		_ = enc.Encode(resp)
	}
}

func (s *mcpServer) handle(method string, raw json.RawMessage) (any, error) {
	switch method {
	case "initialize":
		return map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "trec", "version": "1"}}, nil
	case "notifications/initialized":
		return nil, nil
	case "tools/list":
		return map[string]any{"tools": []map[string]any{
			{"name": "run", "description": "Run any local command once; use trec -o <cast-path> to choose a recording name. working_directory resolves relative paths.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"command": map[string]string{"type": "array"}, "stdin": map[string]string{"type": "string"}, "timeout_seconds": map[string]string{"type": "integer"}, "working_directory": map[string]string{"type": "string"}}, "required": []string{"command"}}},
			{"name": "terminal_start", "description": "Start any local command and retain its stdin/stdout/stderr. Use trec -o <cast-path> to choose a recording name.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"command": map[string]string{"type": "array"}, "working_directory": map[string]string{"type": "string"}}, "required": []string{"command"}}},
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
			return nil, e
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
		in, e := c.StdinPipe()
		if e != nil {
			return nil, e
		}
		ss := &mcpSession{in: in, cmd: c}
		c.Stdout = &ss.out
		c.Stderr = &ss.err
		if e = c.Start(); e != nil {
			return nil, e
		}
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
