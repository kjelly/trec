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
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
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
	defaultMCPCols     = 120
	defaultMCPRows     = 40
	defaultMCPKeyDelay = 300 * time.Millisecond
	maxMCPDimension    = 1000
)

func newMCPProtocolServer(s *mcpServer) *mcpserver.MCPServer {
	server := mcpserver.NewMCPServer("trec", currentBuildMetadata().DisplayVersion(), mcpserver.WithToolCapabilities(false), mcpserver.WithRecovery())
	add := func(name string, tool mcp.Tool) {
		server.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return s.handleCallTool(ctx, name, req)
		})
	}
	add("run", mcp.NewTool("run", mcp.WithDescription("Run any local command once."), mcp.WithArray("command", mcp.Required(), mcp.WithStringItems(), mcp.Description("Argv: command name followed by its arguments, e.g. [\"echo\", \"hi\"].")), mcp.WithString("stdin"), mcp.WithNumber("timeout_seconds"), mcp.WithString("working_directory")))
	add("cast_verify", mcp.NewTool("cast_verify",
		mcp.WithDescription("Verify cast completion, result integrity, and secret-scan safety."),
		mcp.WithArray("paths", mcp.Required(), mcp.WithStringItems(), mcp.Description("Cast files or directories containing .cast files.")),
	))
	add("terminal_start", mcp.NewTool("terminal_start",
		mcp.WithDescription("Start a persistent terminal process."),
		mcp.WithArray("command", mcp.Required(), mcp.WithStringItems(), mcp.Description("Argv: command name followed by its arguments, e.g. [\"bash\"].")),
		mcp.WithString("working_directory"),
		mcp.WithInteger("cols", mcp.Description("PTY columns."), mcp.DefaultNumber(defaultMCPCols), mcp.Min(1), mcp.Max(maxMCPDimension)),
		mcp.WithInteger("rows", mcp.Description("PTY rows."), mcp.DefaultNumber(defaultMCPRows), mcp.Min(1), mcp.Max(maxMCPDimension)),
		mcp.WithNumber("key_delay_ms", mcp.Description("Delay between navigation keystrokes used by terminal_focus/choose/toggle/activate. Defaults to 300ms for readable recordings."), mcp.DefaultNumber(300), mcp.Min(0)),
		mcp.WithString("record_file", mcp.Description("Path to record the session (.cast file). Must not exist.")),
		mcp.WithString("record_title", mcp.Description("Title of the recording.")),
		mcp.WithArray("secret_env", mcp.Description("Environment variables to redact from the recording.")),
		mcp.WithArray("secret_file", mcp.Description("Files to redact (NAME=path).")),
	))
	add("terminal_write", mcp.NewTool("terminal_write",
		mcp.WithDescription("Write ordinary text to a persistent terminal process, or one newline-terminated drive --interactive DSL instruction. For a raw TUI, use terminal_key for keys (especially ENTER) and terminal_activate for menus; \\n is Ctrl+J, not Enter."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("data", mcp.Required()),
		mcp.WithNumber("delay_ms", mcp.Description("Per-character delay in milliseconds, for TUIs that drop unpaced keystrokes. Leave unset for escape sequences, which must arrive as one unpaced burst.")),
	))
	add("terminal_key", mcp.NewTool("terminal_key",
		mcp.WithDescription("Send a structured terminal key without relying on JSON escape handling. Supported keys: ENTER, TAB, SPACE, ESCAPE, UP, DOWN, LEFT, RIGHT, BACKSPACE, CTRLC, CTRLU, CTRLW."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("key", mcp.Required()),
	))
	add("terminal_read", mcp.NewTool("terminal_read", mcp.WithDescription("Read a persistent terminal process incremental raw output."), mcp.WithString("session_id", mcp.Required())))
	add("terminal_read_screen", mcp.NewTool("terminal_read_screen", mcp.WithDescription("Read the emulated screen from a persistent terminal process."), mcp.WithString("session_id", mcp.Required())))
	add("terminal_resize", mcp.NewTool("terminal_resize", mcp.WithDescription("Resize the terminal to a new width and height."), mcp.WithString("session_id", mcp.Required()), mcp.WithNumber("cols", mcp.Required()), mcp.WithNumber("rows", mcp.Required())))
	add("terminal_expect", mcp.NewTool("terminal_expect", mcp.WithDescription("Wait until text appears on the terminal screen."), mcp.WithString("session_id", mcp.Required()), mcp.WithString("text", mcp.Required()), mcp.WithNumber("timeout_seconds", mcp.Required())))
	add("terminal_wait_quiet", mcp.NewTool("terminal_wait_quiet", mcp.WithDescription("Wait until terminal output is quiet."), mcp.WithString("session_id", mcp.Required()), mcp.WithNumber("quiet_duration_seconds", mcp.Required()), mcp.WithNumber("timeout_seconds", mcp.Required())))
	add("terminal_close", mcp.NewTool("terminal_close", mcp.WithDescription("Close a persistent terminal process."), mcp.WithString("session_id", mcp.Required())))
	add("terminal_focus", mcp.NewTool("terminal_focus",
		mcp.WithDescription("Move the menu selection pointer to match the label."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("text", mcp.Required(), mcp.Description("The label text to select.")),
		mcp.WithString("pointer", mcp.Description("Regex pattern matching a menu selection pointer.")),
		mcp.WithNumber("timeout_seconds"),
	))
	add("terminal_select", mcp.NewTool("terminal_select",
		mcp.WithDescription("Compatibility alias for terminal_focus."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("text", mcp.Required(), mcp.Description("The label text to focus.")),
		mcp.WithString("pointer", mcp.Description("Regex pattern matching a menu selection pointer.")),
		mcp.WithNumber("timeout_seconds"),
	))
	add("terminal_choose", mcp.NewTool("terminal_choose",
		mcp.WithDescription("Atomically select a unique menu label and submit it with Enter. Use for menus that commit on Enter."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("text", mcp.Required(), mcp.Description("The unique visible menu label to choose.")),
		mcp.WithString("pointer", mcp.Description("Regex pattern matching a menu selection pointer.")),
		mcp.WithNumber("timeout_seconds"),
	))
	add("terminal_activate", mcp.NewTool("terminal_activate",
		mcp.WithDescription("Atomically select a unique menu/checklist label and send ENTER or SPACE. Specify key explicitly: ENTER commits a menu; SPACE toggles a checklist item without submitting it."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("text", mcp.Required(), mcp.Description("The unique visible label to activate.")),
		mcp.WithString("key", mcp.Required(), mcp.Description("ENTER for a menu, SPACE for a checklist.")),
		mcp.WithString("pointer", mcp.Description("Regex pattern matching a menu selection pointer.")),
		mcp.WithNumber("timeout_seconds"),
	))
	add("terminal_toggle", mcp.NewTool("terminal_toggle",
		mcp.WithDescription("Atomically select a unique checklist label and press Space. It does not submit the checklist; use terminal_key ENTER only after verifying the checked state."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("text", mcp.Required(), mcp.Description("The unique visible checklist label to toggle.")),
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
	defer func() {
		if err := s.closeAllSessions("mcp stdio transport closed"); err != nil {
			fmt.Fprintln(os.Stderr, "trec mcp: close sessions:", err)
		}
	}()
	server := newMCPProtocolServer(s)
	if err := mcpserver.ServeStdio(server); err != nil {
		fmt.Fprintln(os.Stderr, "trec mcp:", err)
	}
}

func runCommandWithGracefulTimeout(c *exec.Cmd, timeout time.Duration) (err error, timedOut bool, forcedKill bool) {
	if c.SysProcAttr == nil {
		c.SysProcAttr = &syscall.SysProcAttr{}
	}
	c.SysProcAttr.Setpgid = true
	if err := c.Start(); err != nil {
		return err, false, false
	}
	done := make(chan error, 1)
	go func() {
		done <- c.Wait()
	}()
	if timeout <= 0 {
		return <-done, false, false
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err, false, false
	case <-timer.C:
		timedOut = true
	}

	if err := syscall.Kill(-c.Process.Pid, syscall.SIGTERM); err != nil {
		_ = c.Process.Signal(syscall.SIGTERM)
	}
	grace := time.NewTimer(2 * time.Second)
	defer grace.Stop()
	select {
	case err := <-done:
		return err, true, false
	case <-grace.C:
		_ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
		_ = c.Process.Kill()
		return <-done, true, true
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
	case "cast_verify":
		var args struct {
			Paths []string `json:"paths"`
		}
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, err
		}
		if len(args.Paths) == 0 {
			return nil, fmt.Errorf("paths is required")
		}
		report, err := verifyPaths(args.Paths)
		if err != nil {
			return nil, err
		}
		if !report.Valid {
			return report, fmt.Errorf("%w: %d of %d cast(s) failed", errVerificationFailed, report.Failed, report.Checked)
		}
		return report, nil
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
		e, timedOut, forcedKill := runCommandWithGracefulTimeout(c, time.Duration(to)*time.Second)
		code := 0
		if e != nil {
			code = 1
			if x, ok := e.(*exec.ExitError); ok {
				code = x.ExitCode()
			}
		}
		return map[string]any{
			"stdout":      out.String(),
			"stderr":      er.String(),
			"exit_code":   code,
			"timed_out":   timedOut,
			"forced_kill": forcedKill,
		}, nil
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
			KeyDelayMS  *float64 `json:"key_delay_ms"`
			SecretEnv   []string `json:"secret_env"`
			SecretFile  []string `json:"secret_file"`
		}
		if err := json.Unmarshal(raw, &mcpOpts); err != nil {
			return nil, err
		}
		keyDelay := defaultMCPKeyDelay
		if mcpOpts.KeyDelayMS != nil {
			if *mcpOpts.KeyDelayMS < 0 {
				return nil, fmt.Errorf("key_delay_ms must not be negative")
			}
			keyDelay = time.Duration(*mcpOpts.KeyDelayMS * float64(time.Millisecond))
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
		var pending sessionResult
		if mcpOpts.RecordFile != "" {
			f, err := prepareRecordingOutput(mcpOpts.RecordFile, false)
			if err != nil {
				return nil, err
			}
			bw := bufio.NewWriterSize(f, 256*1024)
			var bwMu sync.Mutex
			recorder = newRecordingWriter(bw, &bwMu, redactor)

			build := currentBuildMetadata()
			hdr := castHeader{
				Version:     2,
				Width:       int(size.Cols),
				Height:      int(size.Rows),
				Timestamp:   time.Now().Unix(),
				TrecVersion: build.DisplayVersion(),
				TrecBuild:   build,
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
			pending = newPendingSessionResult(time.Now())
			pending.Mode = "mcp_terminal"
			pending.Inputs = &sessionInputFingerprint{
				CWD:       dir,
				VaultPath: pickVaultFile(dir),
			}
			if err := writePendingSessionResult(mcpOpts.RecordFile, pending); err != nil {
				for _, cl := range extraClosers {
					cl.Close()
				}
				os.Remove(mcpOpts.RecordFile)
				return nil, fmt.Errorf("write pending result: %w", err)
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
				_ = writeSessionResult(mcpOpts.RecordFile, sessionResult{
					SessionID: pending.SessionID,
					StartedAt: pending.StartedAt,
					Mode:      pending.Mode,
					Status:    "failed",
					ExitCode:  -1,
					Error:     fmt.Sprintf("start command: %v", e),
					Termination: &sessionTermination{
						Kind:   "start_failure",
						Reason: e.Error(),
					},
				})
			}
			return nil, e
		}

		var options []terminalSessionOption
		if mcpOpts.RecordFile != "" {
			recordPath := mcpOpts.RecordFile
			options = append(options, withTerminalFinalizeHook(func(ts *terminalSession) error {
				exitCode, processErr := ts.getProcessResult()
				status := "success"
				message := ""
				termination := &sessionTermination{Kind: "child_exit", Signal: processSignal(processErr)}
				if processErr != nil || exitCode != 0 {
					status = "failed"
					if processErr != nil {
						message = processErr.Error()
					}
				}
				if reason := ts.getCloseReason(); reason != "" {
					status = "aborted"
					message = reason
					termination.Kind = "operator_terminated"
					if strings.Contains(reason, "transport closed") {
						termination.Kind = "transport_close"
					}
					termination.Reason = reason
				}
				lines, _, _, _, _ := ts.redactedScreenSnapshot()
				return writeSessionResult(recordPath, sessionResult{
					SessionID:       pending.SessionID,
					StartedAt:       pending.StartedAt,
					Mode:            pending.Mode,
					Status:          status,
					ExitCode:        exitCode,
					Error:           message,
					DurationSeconds: time.Since(ts.start).Seconds(),
					FinalScreen:     lines,
					Termination:     termination,
				})
			}))
		}
		ts := newTerminalSession(ptmx, ptmx, c, int(size.Cols), int(size.Rows), recorder, redactor, true, extraClosers, options...)
		ts.keyDelay = keyDelay
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
	case "terminal_key":
		var a struct {
			SessionID string `json:"session_id"`
			Key       string `json:"key"`
		}
		if e := json.Unmarshal(raw, &a); e != nil {
			return nil, e
		}
		keys := map[string]string{
			"ENTER": "\r", "TAB": "\t", "SPACE": " ", "ESCAPE": "\x1b",
			"UP": "\x1b[A", "DOWN": "\x1b[B", "LEFT": "\x1b[D", "RIGHT": "\x1b[C",
			"BACKSPACE": "\x7f", "CTRLC": "\x03", "CTRLU": "\x15", "CTRLW": "\x17",
		}
		data, ok := keys[strings.ToUpper(strings.TrimSpace(a.Key))]
		if !ok {
			return nil, fmt.Errorf("unsupported terminal key %q", a.Key)
		}
		ss, e := s.session(a.SessionID)
		if e != nil {
			return nil, e
		}
		n, e := ss.sendBytes([]byte(data), "")
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
	case "terminal_focus", "terminal_select", "terminal_choose", "terminal_toggle", "terminal_activate":
		var a struct {
			SessionID string   `json:"session_id"`
			Text      string   `json:"text"`
			Key       string   `json:"key"`
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

		var written int
		if name == "terminal_choose" {
			written, e = ss.chooseLabel(ctxSelect, a.Text, pointerRe, ss.keyDelay)
		} else if name == "terminal_toggle" {
			written, e = ss.toggleLabel(ctxSelect, a.Text, pointerRe, ss.keyDelay)
		} else if name == "terminal_activate" {
			switch strings.ToUpper(a.Key) {
			case "ENTER":
				written, e = ss.chooseLabel(ctxSelect, a.Text, pointerRe, ss.keyDelay)
			case "SPACE":
				written, e = ss.toggleLabel(ctxSelect, a.Text, pointerRe, ss.keyDelay)
			default:
				return nil, fmt.Errorf("key must be ENTER or SPACE")
			}
		} else {
			written, e = ss.selectLabel(ctxSelect, a.Text, pointerRe, 40*time.Millisecond)
		}
		if e != nil {
			return nil, mcpErrorWithScreen(e, ss)
		}
		if name == "terminal_choose" {
			return map[string]any{"chosen": true, "written": written}, nil
		}
		if name == "terminal_toggle" {
			return map[string]any{"toggled": true, "written": written}, nil
		}
		if name == "terminal_activate" {
			return map[string]any{"activated": true, "key": strings.ToUpper(a.Key), "written": written}, nil
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
		errClose := ss.closeWithReason("terminal_close requested")
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

func (s *mcpServer) closeAllSessions(reason string) error {
	s.mu.Lock()
	sessions := make([]*terminalSession, 0, len(s.sessions))
	for id, session := range s.sessions {
		sessions = append(sessions, session)
		delete(s.sessions, id)
	}
	s.mu.Unlock()

	var closeErrors []string
	for _, session := range sessions {
		if err := session.closeWithReason(reason); err != nil {
			closeErrors = append(closeErrors, err.Error())
		}
	}
	if len(closeErrors) > 0 {
		return fmt.Errorf("%s", strings.Join(closeErrors, "; "))
	}
	return nil
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

// pickVaultFile looks for a .vault directory or a main.yaml under the
// provided working directory and returns the most likely vault path, or
// "" if none. Used by terminal_start to seed the input fingerprint.
func pickVaultFile(dir string) string {
	candidates := []string{
		filepath.Join(dir, ".vault", "main.yaml"),
		filepath.Join(dir, ".vault", "ipa-identity.yaml"),
		filepath.Join(dir, "main.yaml"),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}
