package main

// Never write diagnostics to stdout here: MCP stdio reserves stdout for
// JSON-RPC frames.

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

const (
	defaultMCPCols  = 120
	defaultMCPRows  = 40
	maxMCPDimension = 1000
)

func newMCPProtocolServer(s *mcpServer) *mcpserver.MCPServer {
	server := mcpserver.NewMCPServer("trec", appVersion, mcpserver.WithToolCapabilities(false), mcpserver.WithRecovery())
	add := func(name string, tool mcp.Tool) {
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
	add("run", mcp.NewTool("run", mcp.WithDescription("Run any local command once."), mcp.WithArray("command", mcp.Required()), mcp.WithString("stdin"), mcp.WithNumber("timeout_seconds"), mcp.WithString("working_directory")))
	add("terminal_start", mcp.NewTool("terminal_start",
		mcp.WithDescription("Start a persistent terminal process."),
		mcp.WithArray("command", mcp.Required()),
		mcp.WithString("working_directory"),
		mcp.WithInteger("cols", mcp.Description("PTY columns."), mcp.DefaultNumber(defaultMCPCols), mcp.Min(1), mcp.Max(maxMCPDimension)),
		mcp.WithInteger("rows", mcp.Description("PTY rows."), mcp.DefaultNumber(defaultMCPRows), mcp.Min(1), mcp.Max(maxMCPDimension)),
	))
	add("terminal_write", mcp.NewTool("terminal_write", mcp.WithDescription("Write to a persistent terminal process."), mcp.WithString("session_id", mcp.Required()), mcp.WithString("data", mcp.Required())))
	add("terminal_read", mcp.NewTool("terminal_read", mcp.WithDescription("Read a persistent terminal process."), mcp.WithString("session_id", mcp.Required())))
	add("terminal_close", mcp.NewTool("terminal_close", mcp.WithDescription("Close a persistent terminal process."), mcp.WithString("session_id", mcp.Required())))
	add("session_list", mcp.NewTool("session_list", mcp.WithDescription("List persistent terminal sessions.")))
	return server
}

func runMCP() {
	s := &mcpServer{sessions: map[string]*mcpSession{}}
	server := newMCPProtocolServer(s)
	if err := mcpserver.ServeStdio(server); err != nil {
		fmt.Fprintln(os.Stderr, "trec mcp:", err)
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

func mcpTerminalSize(raw json.RawMessage) (*pty.Winsize, error) {
	var options struct {
		Cols *int `json:"cols"`
		Rows *int `json:"rows"`
	}
	if err := json.Unmarshal(raw, &options); err != nil {
		return nil, fmt.Errorf("decode terminal size: %w", err)
	}
	cols, rows := defaultMCPCols, defaultMCPRows
	if options.Cols != nil {
		cols = *options.Cols
	}
	if options.Rows != nil {
		rows = *options.Rows
	}
	if cols < 1 || cols > maxMCPDimension {
		return nil, fmt.Errorf("cols must be between 1 and %d", maxMCPDimension)
	}
	if rows < 1 || rows > maxMCPDimension {
		return nil, fmt.Errorf("rows must be between 1 and %d", maxMCPDimension)
	}
	return &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)}, nil
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
		size, e := mcpTerminalSize(raw)
		if e != nil {
			return nil, e
		}
		c := exec.Command(a[0], a[1:]...)
		c.Dir = dir
		ptmx, e := pty.StartWithSize(c, size)
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
