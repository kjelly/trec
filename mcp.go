package main

// Never write diagnostics to stdout here: MCP stdio reserves stdout for
// JSON-RPC frames.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/spf13/cobra"
)

type mcpServer struct {
	mu       sync.Mutex
	next     int
	sessions map[string]*terminalSession
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
			return s.handleCallTool(ctx, name, req)
		})
	}
	add("run", mcp.NewTool("run", mcp.WithDescription("Run any local command once."), mcp.WithArray("command", mcp.Required(), mcp.WithStringItems(), mcp.Description("Argv: command name followed by its arguments, e.g. [\"echo\", \"hi\"].")), mcp.WithString("stdin"), mcp.WithNumber("timeout_seconds"), mcp.WithString("working_directory")))
	add("terminal_start", mcp.NewTool("terminal_start",
		mcp.WithDescription("Start a persistent terminal process."),
		mcp.WithArray("command", mcp.Required(), mcp.WithStringItems(), mcp.Description("Argv: command name followed by its arguments, e.g. [\"bash\"].")),
		mcp.WithString("working_directory"),
		mcp.WithInteger("cols", mcp.Description("PTY columns."), mcp.DefaultNumber(defaultMCPCols), mcp.Min(1), mcp.Max(maxMCPDimension)),
		mcp.WithInteger("rows", mcp.Description("PTY rows."), mcp.DefaultNumber(defaultMCPRows), mcp.Min(1), mcp.Max(maxMCPDimension)),
		mcp.WithString("record_file", mcp.Description("Path to record the session (.cast file). Must not exist.")),
		mcp.WithString("record_title", mcp.Description("Title of the recording.")),
		mcp.WithArray("secret_env", mcp.Description("Environment variables to redact from the recording.")),
		mcp.WithArray("secret_file", mcp.Description("Files to redact (NAME=path).")),
	))
	add("terminal_write", mcp.NewTool("terminal_write",
		mcp.WithDescription("Write raw bytes to a persistent terminal process. Send \"\\r\" (carriage return) for Enter; \"\\n\" is Ctrl+J and most TUIs do not treat it as Enter."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("data", mcp.Required()),
		mcp.WithNumber("delay_ms", mcp.Description("Per-character delay in milliseconds, for TUIs that drop unpaced keystrokes. Leave unset for escape sequences, which must arrive as one unpaced burst.")),
	))
	add("terminal_read", mcp.NewTool("terminal_read", mcp.WithDescription("Read a persistent terminal process incremental raw output."), mcp.WithString("session_id", mcp.Required())))
	add("terminal_read_screen", mcp.NewTool("terminal_read_screen", mcp.WithDescription("Read the emulated screen from a persistent terminal process."), mcp.WithString("session_id", mcp.Required())))
	add("terminal_resize", mcp.NewTool("terminal_resize", mcp.WithDescription("Resize the terminal to a new width and height."), mcp.WithString("session_id", mcp.Required()), mcp.WithNumber("cols", mcp.Required()), mcp.WithNumber("rows", mcp.Required())))
	add("terminal_expect", mcp.NewTool("terminal_expect", mcp.WithDescription("Wait until text appears on the terminal screen."), mcp.WithString("session_id", mcp.Required()), mcp.WithString("text", mcp.Required()), mcp.WithNumber("timeout_seconds", mcp.Required())))
	add("terminal_wait_quiet", mcp.NewTool("terminal_wait_quiet", mcp.WithDescription("Wait until terminal output is quiet."), mcp.WithString("session_id", mcp.Required()), mcp.WithNumber("quiet_duration_seconds", mcp.Required()), mcp.WithNumber("timeout_seconds", mcp.Required())))
	add("terminal_close", mcp.NewTool("terminal_close", mcp.WithDescription("Close a persistent terminal process."), mcp.WithString("session_id", mcp.Required())))
	add("terminal_select", mcp.NewTool("terminal_select",
		mcp.WithDescription("Move the menu selection pointer to match the label."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("text", mcp.Required(), mcp.Description("The label text to select.")),
		mcp.WithString("pointer", mcp.Description("Regex pattern matching a menu selection pointer.")),
		mcp.WithNumber("timeout_seconds"),
	))
	add("session_list", mcp.NewTool("session_list", mcp.WithDescription("List persistent terminal sessions.")))
	return server
}
func (s *mcpServer) handleCallTool(ctx context.Context, name string, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args, _ := json.Marshal(req.GetArguments())
	value, err := s.tool(ctx, name, args)
	var res *mcp.CallToolResult
	if value != nil {
		out, _ := json.Marshal(value)
		res = mcp.NewToolResultText(string(out))
		if err != nil {
			res.IsError = true
			res.Content = append(res.Content, mcp.TextContent{
				Type: "text",
				Text: err.Error(),
			})
		}
	} else if err != nil {
		res = mcp.NewToolResultError(err.Error())
	} else {
		res = mcp.NewToolResultText("null")
	}
	return res, nil
}

func runMCP() {
	s := &mcpServer{sessions: map[string]*terminalSession{}}
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

type mcpScreenError struct {
	err error
	msg string
}

func (e *mcpScreenError) Error() string { return e.msg }
func (e *mcpScreenError) Unwrap() error { return e.err }

func mcpErrorWithScreen(e error, ss *terminalSession) error {
	lines, _, _, _, _ := ss.redactedScreenSnapshot()
	msg := fmt.Sprintf("%v\nScreen:\n%s", e, strings.Join(lines, "\n"))
	if ss.redactor != nil {
		msg = ss.redactor.RedactString(msg)
	}
	return &mcpScreenError{err: e, msg: msg}
}

func (s *mcpServer) tool(ctx context.Context, name string, raw []byte) (any, error) {
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
		var mcpOpts struct {
			RecordFile  string   `json:"record_file"`
			RecordTitle string   `json:"record_title"`
			SecretEnv   []string `json:"secret_env"`
			SecretFile  []string `json:"secret_file"`
		}
		if err := json.Unmarshal(raw, &mcpOpts); err != nil {
			return nil, err
		}

		redactor, err := newSecretRedactor(mcpOpts.SecretEnv)
		if err != nil {
			return nil, err
		}
		if err := addSecretFileSpecs(redactor, mcpOpts.SecretFile); err != nil {
			return nil, err
		}

		var recorder *recordingWriter
		var extraClosers []io.Closer
		if mcpOpts.RecordFile != "" {
			if _, err := os.Stat(mcpOpts.RecordFile); err == nil {
				return nil, fmt.Errorf("record_file %q already exists", mcpOpts.RecordFile)
			}

			f, err := os.OpenFile(mcpOpts.RecordFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
			if err != nil {
				return nil, err
			}
			bw := bufio.NewWriterSize(f, 256*1024)
			var bwMu sync.Mutex
			recorder = newRecordingWriter(bw, &bwMu, redactor)

			hdr := castHeader{
				Version:     2,
				Width:       int(size.Cols),
				Height:      int(size.Rows),
				Timestamp:   time.Now().Unix(),
				TrecVersion: appVersion,
				Title:       mcpOpts.RecordTitle,
				Env: map[string]string{
					"TERM": "xterm-256color",
					"CI":   "1",
				},
			}
			extraClosers = append(extraClosers, f)
			if err := recorder.writeHeader(hdr); err != nil {
				for _, cl := range extraClosers {
					cl.Close()
				}
				os.Remove(mcpOpts.RecordFile)
				return nil, fmt.Errorf("write header: %w", err)
			}
			if err := recorder.flush(); err != nil {
				for _, cl := range extraClosers {
					cl.Close()
				}
				os.Remove(mcpOpts.RecordFile)
				return nil, fmt.Errorf("write header: %w", err)
			}
		}

		c := exec.Command(a[0], a[1:]...)
		c.Dir = dir
		c.Env = append(os.Environ(), "TERM=xterm-256color", "CI=1")
		ptmx, e := pty.StartWithSize(c, size)
		if e != nil {
			for _, cl := range extraClosers {
				cl.Close()
			}
			if mcpOpts.RecordFile != "" {
				os.Remove(mcpOpts.RecordFile)
			}
			return nil, e
		}

		ts := newTerminalSession(ptmx, ptmx, c, int(size.Cols), int(size.Rows), recorder, redactor, true, extraClosers)
		s.mu.Lock()
		s.next++
		id := "session-" + strconv.Itoa(s.next)
		s.sessions[id] = ts
		s.mu.Unlock()
		return map[string]string{"session_id": id}, nil
	case "terminal_write":
		var a struct {
			SessionID string   `json:"session_id"`
			Data      string   `json:"data"`
			DelayMS   *float64 `json:"delay_ms"`
		}
		if e := json.Unmarshal(raw, &a); e != nil {
			return nil, e
		}
		if a.DelayMS != nil && *a.DelayMS < 0 {
			return nil, fmt.Errorf("delay_ms must not be negative")
		}
		ss, e := s.session(a.SessionID)
		if e != nil {
			return nil, e
		}
		var n int
		if a.DelayMS != nil && *a.DelayMS > 0 {
			n, e = ss.sendText(a.Data, "", time.Duration(*a.DelayMS*float64(time.Millisecond)))
		} else {
			n, e = ss.sendBytes([]byte(a.Data), "")
		}
		return map[string]any{"written": n}, e
	case "terminal_read":
		// Note: terminal_read returns unredacted raw output by design, so clients can process raw bytes.
		// Use terminal_read_screen for redacted rendered output.
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
		o, truncated := ss.readRaw()
		exited, code, _ := ss.getExitState()
		return map[string]any{"stdout": o, "stderr": "", "running": !exited, "exit_code": code, "truncated": truncated}, nil
	case "terminal_read_screen":
		// Note: terminal_read_screen handles redaction internally now
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
		lines, r, c, rows, cols := ss.redactedScreenSnapshot()
		exited, code, _ := ss.getExitState()
		return map[string]any{
			"screen":    lines,
			"cursor":    map[string]int{"row": r, "col": c},
			"size":      map[string]int{"rows": rows, "cols": cols},
			"running":   !exited,
			"exit_code": code,
		}, nil
	case "terminal_resize":
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
		size, e := mcpTerminalSize(raw)
		if e != nil {
			return nil, e
		}
		if e := ss.resize(int(size.Cols), int(size.Rows)); e != nil {
			return nil, e
		}
		return map[string]any{"cols": int(size.Cols), "rows": int(size.Rows)}, nil
	case "terminal_expect":
		var a struct {
			SessionID string  `json:"session_id"`
			Text      string  `json:"text"`
			Timeout   float64 `json:"timeout_seconds"`
		}
		if e := json.Unmarshal(raw, &a); e != nil {
			return nil, e
		}
		if a.Timeout <= 0 {
			return nil, fmt.Errorf("timeout_seconds must be greater than zero")
		}
		ss, e := s.session(a.SessionID)
		if e != nil {
			return nil, e
		}
		e = ss.waitForText(ctx, "EXPECT", a.Text, time.Duration(a.Timeout*float64(time.Second)))
		if e != nil {
			return nil, mcpErrorWithScreen(e, ss)
		}
		return map[string]bool{"found": true}, nil
	case "terminal_wait_quiet":
		var a struct {
			SessionID string  `json:"session_id"`
			Quiet     float64 `json:"quiet_duration_seconds"`
			Timeout   float64 `json:"timeout_seconds"`
		}
		if e := json.Unmarshal(raw, &a); e != nil {
			return nil, e
		}
		if a.Quiet <= 0 {
			return nil, fmt.Errorf("quiet_duration_seconds must be greater than zero")
		}
		if a.Timeout <= 0 {
			return nil, fmt.Errorf("timeout_seconds must be greater than zero")
		}
		ss, e := s.session(a.SessionID)
		if e != nil {
			return nil, e
		}
		e = ss.waitQuiet(ctx, time.Duration(a.Quiet*float64(time.Second)), time.Duration(a.Timeout*float64(time.Second)))
		if e != nil {
			return nil, mcpErrorWithScreen(e, ss)
		}
		return map[string]bool{"quiet": true}, nil
	case "terminal_select":
		var a struct {
			SessionID string   `json:"session_id"`
			Text      string   `json:"text"`
			Pointer   string   `json:"pointer"`
			Timeout   *float64 `json:"timeout_seconds"`
		}
		if e := json.Unmarshal(raw, &a); e != nil {
			return nil, e
		}
		if a.Text == "" {
			return nil, fmt.Errorf("text (label) must be a non-empty string")
		}
		if a.Pointer == "" {
			a.Pointer = `^\s*(?:❯|▸|›|→|»|>)\s`
		}
		timeoutVal := 5.0
		if a.Timeout != nil {
			if *a.Timeout <= 0 {
				return nil, fmt.Errorf("timeout_seconds must be greater than zero")
			}
			timeoutVal = *a.Timeout
		}
		ss, e := s.session(a.SessionID)
		if e != nil {
			return nil, e
		}

		pointerRe, e := regexp.Compile(a.Pointer)
		if e != nil {
			return nil, fmt.Errorf("bad pointer regexp: %w", e)
		}

		ctxSelect, cancel := context.WithTimeout(ctx, time.Duration(timeoutVal*float64(time.Second)))
		defer cancel()

		_, e = ss.selectLabel(ctxSelect, a.Text, pointerRe, 40*time.Millisecond)
		if e != nil {
			return nil, mcpErrorWithScreen(e, ss)
		}
		return map[string]bool{"selected": true}, nil
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
		errClose := ss.close()
		s.mu.Lock()
		delete(s.sessions, a.SessionID)
		s.mu.Unlock()
		if errClose != nil {
			return nil, errClose
		}
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
func (s *mcpServer) session(id string) (*terminalSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss := s.sessions[id]
	if ss == nil {
		return nil, fmt.Errorf("unknown session %q", id)
	}
	return ss, nil
}
