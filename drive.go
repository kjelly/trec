package main

import (
	"bufio"
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
	"github.com/spf13/cobra"
)

// driveStep is one instruction from a keystroke script, in the same format
// pilot's ptydrive scaffolding used to drive promptui/bubbletea wizards.
type driveStep struct {
	kind string // text, enter, down, up, space, tab, ctrlc, backspace, wait,
	// text_env, text_file, replace_text, replace_text_env, replace_text_file,
	// expect, expect_quiet, assert, select, snapshot, wait_child_exit, assert_exit, quit
	text string
	n    int
	hasN bool
	// timeout is an optional per-step timeout, currently used by
	// EXPECT_QUIET@<ms>. A zero value means to use the session default.
	timeout    int
	hasTimeout bool
	re         *regexp.Regexp // for EXPECT_REGEX and EXPECT_SCREEN_REGEX
	raw        string         // original script line, for markers and error reports
	line       int            // script line number (or interactive command sequence)
}

type jsonDriveStep struct {
	Kind    string `json:"kind"`
	Op      string `json:"op"`
	Text    string `json:"text"`
	Arg     string `json:"arg"`
	N       *int   `json:"n"`
	Timeout *int   `json:"timeout_ms"`
}

func validateStep(st *driveStep) error {
	switch st.kind {
	case "text", "replace_text":
		// TEXT needs no validation
	case "text_env", "replace_text_env":
		if !validEnvName(st.text) {
			return fmt.Errorf("line %d: %s needs an environment variable name", st.line, strings.ToUpper(st.kind))
		}
	case "text_file", "replace_text_file":
		if strings.TrimSpace(st.text) == "" {
			return fmt.Errorf("line %d: %s needs a path", st.line, strings.ToUpper(st.kind))
		}
	case "enter", "space", "tab", "escape", "ctrlc", "ctrlu", "ctrlw", "snapshot", "quit":
		// These commands do not use extra arguments.
	case "expect_change":
		if st.text != "" {
			return fmt.Errorf("line %d: EXPECT_CHANGE takes no arguments", st.line)
		}
		if st.hasN && st.n <= 0 {
			return fmt.Errorf("line %d: EXPECT_CHANGE needs a positive timeout duration", st.line)
		}
	case "down", "up", "left", "right", "backspace", "wait":
		if st.hasN && st.n <= 0 {
			return fmt.Errorf("line %d: %s needs a positive count", st.line, strings.ToUpper(st.kind))
		}
		if !st.hasN {
			st.n = 1
		}
	case "expect", "expect_eventually":
		if st.text == "" {
			return fmt.Errorf("line %d: %s needs text", st.line, strings.ToUpper(st.kind))
		}
		if st.hasN && st.n <= 0 {
			return fmt.Errorf("line %d: %s needs a positive timeout duration", st.line, strings.ToUpper(st.kind))
		}
	case "expect_regex", "expect_screen_regex":
		if st.text == "" {
			return fmt.Errorf("line %d: %s needs text pattern", st.line, strings.ToUpper(st.kind))
		}
		if st.re == nil {
			re, err := regexp.Compile(st.text)
			if err != nil {
				return fmt.Errorf("line %d: invalid regex", st.line)
			}
			st.re = re
		}
		if st.hasN && st.n <= 0 {
			return fmt.Errorf("line %d: %s needs a positive timeout duration", st.line, strings.ToUpper(st.kind))
		}
	case "expect_quiet":
		if st.hasN && st.n <= 0 {
			return fmt.Errorf("line %d: EXPECT_QUIET needs a positive quiet duration", st.line)
		}
		if !st.hasN {
			st.n = 300
		}
		if st.hasTimeout && st.timeout <= 0 {
			return fmt.Errorf("line %d: EXPECT_QUIET needs a positive timeout duration", st.line)
		}
	case "assert":
		if st.text == "" {
			return fmt.Errorf("line %d: ASSERT needs text", st.line)
		}
	case "select":
		if st.text == "" {
			return fmt.Errorf("line %d: SELECT needs a label", st.line)
		}
	case "wait_child_exit":
		if st.text != "" {
			return fmt.Errorf("line %d: WAIT_CHILD_EXIT takes no arguments", st.line)
		}
		if st.hasTimeout && st.timeout <= 0 {
			return fmt.Errorf("line %d: WAIT_CHILD_EXIT needs a positive timeout duration", st.line)
		}
	case "assert_exit":
		if !st.hasN {
			return fmt.Errorf("line %d: ASSERT_EXIT needs an exit code", st.line)
		}
		if st.n < 0 {
			return fmt.Errorf("line %d: ASSERT_EXIT needs a non-negative exit code", st.line)
		}
	default:
		return fmt.Errorf("line %d: unknown op %q", st.line, st.kind)
	}
	return nil
}

// parseDriveLine parses one script line. Returns (nil, nil) for blank lines
// and comments.
func parseDriveLine(raw string, lineNo int) (*driveStep, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return nil, nil
	}

	if strings.HasPrefix(trimmed, "{") {
		var jS jsonDriveStep
		if err := json.Unmarshal([]byte(trimmed), &jS); err != nil {
			return nil, fmt.Errorf("line %d: invalid JSON request: %w", lineNo, err)
		}
		kind := jS.Kind
		if kind == "" {
			kind = jS.Op
		}
		text := jS.Text
		if text == "" {
			text = jS.Arg
		}
		kind = strings.ToLower(kind)
		if kind == "clear_line" {
			kind = "ctrlu"
		}
		st := &driveStep{
			kind: kind,
			text: text,
			raw:  trimmed,
			line: lineNo,
		}
		if jS.N != nil {
			st.n = *jS.N
			st.hasN = true
		}
		if jS.Timeout != nil {
			st.timeout = *jS.Timeout
			st.hasTimeout = true
		}
		if err := validateStep(st); err != nil {
			return nil, err
		}
		return st, nil
	}
	fields := strings.SplitN(trimmed, " ", 2)
	op := strings.ToUpper(fields[0])
	arg := ""
	if len(fields) > 1 {
		// Script syntax uses whitespace as the opcode/argument separator.
		// Preserve intentional leading whitespace with JSON steps instead.
		arg = strings.TrimSpace(fields[1])
	}
	st := &driveStep{raw: trimmed, line: lineNo}

	// EXPECT@<ms> overrides the default --expect-timeout for one step.
	for _, prefix := range []string{"EXPECT", "EXPECT_EVENTUALLY", "EXPECT_REGEX", "EXPECT_SCREEN_REGEX"} {
		if ms, ok := strings.CutPrefix(op, prefix+"@"); ok {
			n, err := strconv.Atoi(ms)
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("line %d: bad timeout in %q", lineNo, fields[0])
			}
			if arg == "" {
				return nil, fmt.Errorf("line %d: %s needs text", lineNo, prefix)
			}
			st.kind, st.text, st.n, st.hasN = strings.ToLower(prefix), arg, n, true
			if err := validateStep(st); err != nil {
				return nil, err
			}
			return st, nil
		}
	}

	if ms, ok := strings.CutPrefix(op, "EXPECT_CHANGE@"); ok {
		n, err := strconv.Atoi(ms)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("line %d: bad timeout in %q", lineNo, fields[0])
		}
		if arg != "" {
			return nil, fmt.Errorf("line %d: EXPECT_CHANGE takes no arguments", lineNo)
		}
		st.kind, st.n, st.hasN = "expect_change", n, true
		if err := validateStep(st); err != nil {
			return nil, err
		}
		return st, nil
	}

	if ms, ok := strings.CutPrefix(op, "WAIT_CHILD_EXIT@"); ok {
		timeout, err := strconv.Atoi(ms)
		if err != nil || timeout <= 0 {
			return nil, fmt.Errorf("line %d: bad timeout in %q", lineNo, fields[0])
		}
		if arg != "" {
			return nil, fmt.Errorf("line %d: WAIT_CHILD_EXIT takes no arguments", lineNo)
		}
		st.kind, st.timeout, st.hasTimeout = "wait_child_exit", timeout, true
		if err := validateStep(st); err != nil {
			return nil, err
		}
		return st, nil
	}

	// EXPECT_QUIET@<ms> <quiet-ms> overrides the global timeout while keeping
	// the existing EXPECT_QUIET <quiet-ms> form backward compatible.
	if ms, ok := strings.CutPrefix(op, "EXPECT_QUIET@"); ok {
		timeout, err := strconv.Atoi(ms)
		if err != nil || timeout <= 0 {
			return nil, fmt.Errorf("line %d: bad timeout in %q", lineNo, fields[0])
		}
		quiet, err := parsePositiveCount(arg, 300)
		if err != nil {
			return nil, fmt.Errorf("line %d: EXPECT_QUIET needs a positive quiet duration", lineNo)
		}
		st.kind, st.n, st.hasN, st.timeout, st.hasTimeout = "expect_quiet", quiet, true, timeout, true
		if err := validateStep(st); err != nil {
			return nil, err
		}
		return st, nil
	}

	switch op {
	case "TEXT":
		st.kind, st.text = "text", arg
	case "TEXT_ENV":
		name := strings.TrimSpace(arg)
		if !validEnvName(name) {
			return nil, fmt.Errorf("line %d: TEXT_ENV needs an environment variable name", lineNo)
		}
		st.kind, st.text = "text_env", name
	case "TEXT_FILE":
		if strings.TrimSpace(arg) == "" {
			return nil, fmt.Errorf("line %d: TEXT_FILE needs a path", lineNo)
		}
		st.kind, st.text = "text_file", arg
	case "REPLACE_TEXT":
		st.kind, st.text = "replace_text", arg
	case "REPLACE_TEXT_ENV":
		name := strings.TrimSpace(arg)
		if !validEnvName(name) {
			return nil, fmt.Errorf("line %d: REPLACE_TEXT_ENV needs an environment variable name", lineNo)
		}
		st.kind, st.text = "replace_text_env", name
	case "REPLACE_TEXT_FILE":
		if strings.TrimSpace(arg) == "" {
			return nil, fmt.Errorf("line %d: REPLACE_TEXT_FILE needs a path", lineNo)
		}
		st.kind, st.text = "replace_text_file", arg
	case "ENTER":
		st.kind = "enter"
	case "DOWN":
		n, err := parsePositiveCount(arg, 1)
		if err != nil {
			return nil, fmt.Errorf("line %d: DOWN needs a positive count", lineNo)
		}
		st.kind, st.n, st.hasN = "down", n, arg != ""
	case "UP":
		n, err := parsePositiveCount(arg, 1)
		if err != nil {
			return nil, fmt.Errorf("line %d: UP needs a positive count", lineNo)
		}
		st.kind, st.n, st.hasN = "up", n, arg != ""
	case "LEFT":
		n, err := parsePositiveCount(arg, 1)
		if err != nil {
			return nil, fmt.Errorf("line %d: LEFT needs a positive count", lineNo)
		}
		st.kind, st.n, st.hasN = "left", n, arg != ""
	case "RIGHT":
		n, err := parsePositiveCount(arg, 1)
		if err != nil {
			return nil, fmt.Errorf("line %d: RIGHT needs a positive count", lineNo)
		}
		st.kind, st.n, st.hasN = "right", n, arg != ""
	case "SPACE":
		st.kind = "space"
	case "TAB":
		st.kind = "tab"
	case "ESCAPE":
		st.kind = "escape"
	case "CTRLC":
		st.kind = "ctrlc"
	case "CTRLU", "CLEAR_LINE":
		st.kind = "ctrlu"
	case "CTRLW":
		st.kind = "ctrlw"
	case "BACKSPACE":
		n, err := parsePositiveCount(arg, 1)
		if err != nil {
			return nil, fmt.Errorf("line %d: BACKSPACE needs a positive count", lineNo)
		}
		st.kind, st.n, st.hasN = "backspace", n, arg != ""
	case "WAIT":
		n, err := parsePositiveCount(arg, 1)
		if err != nil {
			return nil, fmt.Errorf("line %d: WAIT needs a positive count", lineNo)
		}
		st.kind, st.n, st.hasN = "wait", n, arg != ""
	case "EXPECT":
		if arg == "" {
			return nil, fmt.Errorf("line %d: EXPECT needs text", lineNo)
		}
		st.kind, st.text = "expect", arg
	case "EXPECT_EVENTUALLY":
		if arg == "" {
			return nil, fmt.Errorf("line %d: EXPECT_EVENTUALLY needs text", lineNo)
		}
		st.kind, st.text = "expect_eventually", arg
	case "EXPECT_CHANGE":
		if arg != "" {
			return nil, fmt.Errorf("line %d: EXPECT_CHANGE takes no arguments", lineNo)
		}
		st.kind = "expect_change"
	case "EXPECT_REGEX":
		st.kind, st.text = "expect_regex", arg
	case "EXPECT_SCREEN_REGEX":
		st.kind, st.text = "expect_screen_regex", arg
	case "EXPECT_QUIET":
		n, err := parsePositiveCount(arg, 300)
		if err != nil {
			return nil, fmt.Errorf("line %d: EXPECT_QUIET needs a positive quiet duration", lineNo)
		}
		st.kind, st.n, st.hasN = "expect_quiet", n, arg != ""
	case "ASSERT":
		if arg == "" {
			return nil, fmt.Errorf("line %d: ASSERT needs text", lineNo)
		}
		st.kind, st.text = "assert", arg
	case "SELECT":
		if arg == "" {
			return nil, fmt.Errorf("line %d: SELECT needs a label", lineNo)
		}
		st.kind, st.text = "select", arg
	case "SNAPSHOT":
		st.kind, st.text = "snapshot", arg
	case "WAIT_CHILD_EXIT":
		if arg != "" {
			return nil, fmt.Errorf("line %d: WAIT_CHILD_EXIT takes no arguments", lineNo)
		}
		st.kind = "wait_child_exit"
	case "ASSERT_EXIT":
		n, err := strconv.Atoi(arg)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("line %d: ASSERT_EXIT needs a non-negative exit code", lineNo)
		}
		st.kind, st.n, st.hasN = "assert_exit", n, true
	case "QUIT":
		st.kind = "quit"
	default:
		return nil, fmt.Errorf("line %d: unknown op %q", lineNo, op)
	}
	if err := validateStep(st); err != nil {
		return nil, err
	}
	return st, nil
}

func loadDriveScript(path string, redactor *secretRedactor) ([]*driveStep, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var steps []*driveStep
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		st, err := parseDriveLine(sc.Text(), lineNo)
		if err != nil {
			return nil, err
		}
		if st != nil {
			if (st.kind == "text_env" || st.kind == "replace_text_env") && redactor != nil {
				if err := redactor.addEnv(st.text); err != nil {
					return nil, fmt.Errorf("line %d: %v", st.line, err)
				}
			} else if (st.kind == "text_file" || st.kind == "replace_text_file") && redactor != nil {
				if _, err := redactor.addFile(st.text); err != nil {
					return nil, fmt.Errorf("line %d: %v", st.line, err)
				}
			}
			steps = append(steps, st)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return steps, nil
}

func parsePositiveCount(s string, def int) (int, error) {
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("not a positive integer")
	}
	return n, nil
}

func max1(n int) int {
	if n <= 0 {
		return 1
	}
	return n
}

// driveSession bundles everything needed to drive the child TUI in a closed
// loop: the PTY, the cast writer, and a VT emulator that mirrors what the
// child has rendered so far. EXPECT/ASSERT/SELECT match against the emulated
// screen (rendered characters), not the raw byte stream, because escape
// sequences fragment text and menu redraws leave stale bytes in the stream.
type driveSession struct {
	ts               *terminalSession
	sessionID        string
	startedAt        string
	keyDelay         time.Duration
	settleDelay      time.Duration
	expectTimeout    time.Duration
	childExitTimeout time.Duration
	pointerRe        *regexp.Regexp
	interactive      bool
	stepMarkers      bool
	exitAsserted     bool
	outputFormat     string
	baselineSnapshot *screenSnapshot
	baselineValid    bool
	redactor         *secretRedactor
	strictAgent      bool
	snapshots        []sessionSnapshot
}

var secretPromptRE = regexp.MustCompile(`(?i)(password|passwd|passphrase|secret|token|api[-_ ]?key|private[ _-]?key|credential|vault|密碼|密码|秘密|權杖|令牌|金鑰|金钥)`)

func (ds *driveSession) secretLikeScreen() bool {
	return secretPromptRE.MatchString(strings.Join(ds.screenLines(), "\n"))
}

func safeStepDescription(st *driveStep) string {
	switch st.kind {
	case "text":
		return fmt.Sprintf("TEXT <literal %d bytes>", len(st.text))
	case "replace_text":
		return fmt.Sprintf("REPLACE_TEXT <literal %d bytes>", len(st.text))
	case "text_file":
		return "TEXT_FILE <file>"
	case "replace_text_file":
		return "REPLACE_TEXT_FILE <file>"
	default:
		return st.raw
	}
}

func (ds *driveSession) screenLines() []string {
	return ds.ts.screenLines()
}

func (ds *driveSession) cursorPos() (row, col int) {
	return ds.ts.cursorPos()
}

func (ds *driveSession) marker(label string) {
	if ds.ts.recorder != nil {
		ds.ts.recorder.event(time.Since(ds.ts.start).Seconds(), "m", label)
		ds.ts.recorder.flush()
	}
}

func (ds *driveSession) stepMarker(phase string, st *driveStep, elapsed time.Duration) {
	label := fmt.Sprintf("STEP_%s line %d: %s", phase, st.line, safeStepDescription(st))
	if phase != "START" {
		label += fmt.Sprintf(" (%.3fs)", elapsed.Seconds())
	}
	ds.marker(label)
}

// failureMarker records both the failing operation and the rendered screen so
// an agent can diagnose a failed cast without a separate render invocation.
func (ds *driveSession) failureMarker(st *driveStep, elapsed time.Duration, err error) {
	msg := fmt.Sprintf("STEP_FAILED line %d: %s (%.3fs; %v)", st.line, safeStepDescription(st), elapsed.Seconds(), err)
	ds.marker(ds.redactor.RedactString(msg))
	lines, _, _, _, _ := ds.ts.redactedScreenSnapshot()
	ds.marker(fmt.Sprintf("FAILED_SCREEN line %d:\n%s", st.line, strings.Join(lines, "\n")))
}

func (ds *driveSession) snapshot(st *driveStep) {
	lines, _, _, _, _ := ds.ts.redactedScreenSnapshot()
	label := st.text
	if label == "" {
		label = fmt.Sprintf("line %d: SNAPSHOT", st.line)
	}
	ds.snapshots = append(ds.snapshots, sessionSnapshot{
		Time:   time.Since(ds.ts.start).Seconds(),
		Label:  ds.redactor.RedactString(label),
		Screen: append([]string(nil), lines...),
	})
	if !ds.interactive {
		ds.dumpScreen(os.Stderr)
	}
}

func (ds *driveSession) dumpScreen(w io.Writer) {
	lines, _, _, _, _ := ds.ts.redactedScreenSnapshot()
	last := len(lines) - 1
	for last >= 0 && lines[last] == "" {
		last--
	}
	if ds.ts.recorder != nil {
		ds.ts.recorder.dumpScreen(w, ds.ts.cols, ds.ts.rows, lines[:last+1])
	}
}

func (ds *driveSession) applyStep(ctx context.Context, st *driveStep) error {
	isMutating := false
	switch st.kind {
	case "text", "text_env", "text_file", "replace_text", "replace_text_env", "replace_text_file", "enter", "down", "up", "left", "right", "space", "tab", "escape", "ctrlc", "ctrlu", "ctrlw", "backspace", "select":
		isMutating = true
	}
	if isMutating {
		ds.baselineSnapshot = ds.ts.getSnapshot()
		ds.baselineValid = false
	}

	bytesWritten := 0
	var err error

	switch st.kind {
	case "text":
		if ds.strictAgent && ds.secretLikeScreen() {
			return fmt.Errorf("--strict-agent refuses literal TEXT on a secret-like screen; use TEXT_ENV or TEXT_FILE")
		}
		bytesWritten, err = ds.ts.sendText(st.text, "", ds.keyDelay)
	case "text_env":
		if err = ds.redactor.addEnv(st.text); err != nil {
			return err
		}
		value, _ := os.LookupEnv(st.text)
		bytesWritten, err = ds.ts.sendText(value, st.text, ds.keyDelay)
	case "text_file":
		var value string
		value, err = ds.redactor.addFile(st.text)
		if err != nil {
			return err
		}
		bytesWritten, err = ds.ts.sendText(value, "file", ds.keyDelay)
	case "replace_text":
		if ds.strictAgent && ds.secretLikeScreen() {
			return fmt.Errorf("--strict-agent refuses literal REPLACE_TEXT on a secret-like screen; use REPLACE_TEXT_ENV or REPLACE_TEXT_FILE")
		}
		bytesWritten, err = ds.ts.replaceText(st.text, "", ds.keyDelay)
	case "replace_text_env":
		if err = ds.redactor.addEnv(st.text); err != nil {
			return err
		}
		value, _ := os.LookupEnv(st.text)
		bytesWritten, err = ds.ts.replaceText(value, st.text, ds.keyDelay)
	case "replace_text_file":
		var value string
		value, err = ds.redactor.addFile(st.text)
		if err != nil {
			return err
		}
		bytesWritten, err = ds.ts.replaceText(value, "file", ds.keyDelay)
	case "enter":
		bytesWritten, err = ds.ts.sendBytes([]byte("\r"), "")
		time.Sleep(ds.settleDelay)
	case "down", "up", "left", "right":
		seq := "\x1b[B"
		if st.kind == "up" {
			seq = "\x1b[A"
		} else if st.kind == "left" {
			seq = "\x1b[D"
		} else if st.kind == "right" {
			seq = "\x1b[C"
		}
		for range max1(st.n) {
			n, e := ds.ts.sendBytes([]byte(seq), "")
			bytesWritten += n
			if e != nil {
				err = e
				break
			}
			time.Sleep(ds.keyDelay)
		}
	case "space":
		bytesWritten, err = ds.ts.sendBytes([]byte(" "), "")
		time.Sleep(ds.keyDelay)
	case "tab":
		bytesWritten, err = ds.ts.sendBytes([]byte("\t"), "")
		time.Sleep(ds.keyDelay)
	case "escape":
		bytesWritten, err = ds.ts.sendBytes([]byte("\x1b"), "")
		time.Sleep(ds.keyDelay)
	case "ctrlc":
		bytesWritten, err = ds.ts.sendBytes([]byte{0x03}, "")
		time.Sleep(ds.keyDelay)
	case "ctrlu":
		bytesWritten, err = ds.ts.sendBytes([]byte{0x15}, "")
		time.Sleep(ds.keyDelay)
	case "ctrlw":
		bytesWritten, err = ds.ts.sendBytes([]byte{0x17}, "")
		time.Sleep(ds.keyDelay)
	case "backspace":
		for range max1(st.n) {
			n, e := ds.ts.sendBytes([]byte{127}, "")
			bytesWritten += n
			if e != nil {
				err = e
				break
			}
			time.Sleep(ds.keyDelay)
		}
	case "wait":
		time.Sleep(time.Duration(st.n) * time.Millisecond)
	case "expect":
		timeout := ds.expectTimeout
		if st.n > 0 {
			timeout = time.Duration(st.n) * time.Millisecond
		}
		err = ds.ts.waitForText(ctx, "EXPECT", st.text, timeout)
	case "expect_eventually":
		timeout := ds.expectTimeout * 6
		if st.n > 0 {
			timeout = time.Duration(st.n) * time.Millisecond
		}
		err = ds.ts.waitForText(ctx, "EXPECT_EVENTUALLY", st.text, timeout)
	case "expect_change":
		if !ds.baselineValid {
			return fmt.Errorf("EXPECT_CHANGE: no previous mutating action to compare against")
		}
		timeout := ds.expectTimeout
		if st.n > 0 {
			timeout = time.Duration(st.n) * time.Millisecond
		}
		err = ds.ts.waitForChange(ctx, ds.baselineSnapshot, timeout)
	case "expect_regex":
		timeout := ds.expectTimeout
		if st.n > 0 {
			timeout = time.Duration(st.n) * time.Millisecond
		}
		err = ds.ts.waitForRegex(ctx, "EXPECT_REGEX", st.re, timeout, false)
	case "expect_screen_regex":
		timeout := ds.expectTimeout
		if st.n > 0 {
			timeout = time.Duration(st.n) * time.Millisecond
		}
		err = ds.ts.waitForRegex(ctx, "EXPECT_SCREEN_REGEX", st.re, timeout, true)
	case "expect_quiet":
		limit := ds.expectTimeout
		if st.hasTimeout {
			limit = time.Duration(st.timeout) * time.Millisecond
		}
		err = ds.ts.waitQuiet(ctx, time.Duration(st.n)*time.Millisecond, limit)
	case "assert":
		if !screenContains(ds.screenLines(), st.text) {
			err = fmt.Errorf("ASSERT %q: not on screen", st.text)
		}
	case "select":
		n, e := ds.ts.selectLabel(ctx, st.text, ds.pointerRe, ds.keyDelay)
		bytesWritten += n
		err = e
	case "snapshot":
		ds.snapshot(st)
	case "wait_child_exit":
		if ds.interactive {
			return fmt.Errorf("WAIT_CHILD_EXIT is only available in --script mode")
		}
		timeout := ds.childExitTimeout
		if st.hasTimeout {
			timeout = time.Duration(st.timeout) * time.Millisecond
		}
		waitCtx := ctx
		cancel := func() {}
		if timeout > 0 {
			waitCtx, cancel = context.WithTimeout(ctx, timeout)
		}
		_, finalizeErr := ds.ts.waitChildExit(waitCtx)
		cancel()
		if waitCtx.Err() == context.DeadlineExceeded {
			err = fmt.Errorf("WAIT_CHILD_EXIT: timeout after %v", timeout)
		} else {
			err = finalizeErr
		}
	case "assert_exit":
		if ds.interactive {
			return fmt.Errorf("ASSERT_EXIT is only available in --script mode")
		}
		exited, got, _ := ds.ts.getExitState()
		if !exited {
			return fmt.Errorf("ASSERT_EXIT %d: child has not exited; use WAIT_CHILD_EXIT first", st.n)
		}
		if got != st.n {
			return fmt.Errorf("ASSERT_EXIT %d: child exit code %d", st.n, got)
		}
		ds.exitAsserted = true
		err = nil
	case "quit":
		// handled by the caller
	}

	if bytesWritten > 0 {
		ds.baselineValid = true
	}
	return err
}

// respond prints one interactive-protocol response to stdout: a status line
// ("OK" or "ERR <message>"), the cursor position, then the full emulated
// screen with a fixed row count so the reader can parse without a sentinel.
func (ds *driveSession) respond(err error) {
	status := "OK"
	if err != nil {
		status = "ERR " + ds.redactor.RedactString(err.Error())
	}
	row, col := ds.cursorPos()
	lines, _, _, _, _ := ds.ts.redactedScreenSnapshot()

	if ds.outputFormat == "jsonl" {
		type jsonlResponse struct {
			Status string   `json:"status"`
			Cursor []int    `json:"cursor"`
			Screen []string `json:"screen"`
		}
		resp := jsonlResponse{
			Status: status,
			Cursor: []int{row, col},
			Screen: lines,
		}
		b, _ := json.Marshal(resp)
		fmt.Println(string(b))
		return
	}

	if err != nil {
		fmt.Printf("ERR %v\n", ds.redactor.RedactString(err.Error()))
	} else {
		fmt.Println("OK")
	}
	fmt.Printf("CURSOR %d %d\n", row, col)
	fmt.Printf("SCREEN %d %d\n", len(lines), ds.ts.cols)
	for _, l := range lines {
		fmt.Println(l)
	}
}

func newDriveCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "drive [flags] -- <command> [args...]",
		Short: "Drive a TUI from a script or interactively",
		Long: `Drive a TUI under a PTY and record the session as an asciicast v2 file.

Use one of these modes:
  trec drive --script steps.txt -- <command> [args...]
  trec drive --interactive -- <command> [args...]

When both are supplied, trec runs the script first, then accepts interactive
operations from stdin. Interactive operations include TEXT, TEXT_ENV, TEXT_FILE,
REPLACE_TEXT, REPLACE_TEXT_ENV, REPLACE_TEXT_FILE,
ENTER, DOWN, UP, LEFT, RIGHT, SPACE, TAB, ESCAPE, CTRLC, CTRLU, CLEAR_LINE, CTRLW, BACKSPACE, WAIT,
EXPECT, EXPECT_EVENTUALLY, EXPECT_CHANGE, EXPECT_REGEX, EXPECT_SCREEN_REGEX,
EXPECT_QUIET, EXPECT_QUIET@<timeout-ms> <quiet-ms>, WAIT_CHILD_EXIT@<timeout-ms>,
ASSERT, SELECT, SNAPSHOT, and QUIT. Use TEXT_ENV/--secret-env or
TEXT_FILE/--secret-file for credentials. Run "trec drive --help" for flags.`,
		Args: cobra.ArbitraryArgs,
		Run:  runDrive,
	}
	cmd.Flags().String("output-format", "", "Output format (e.g. jsonl)")
	cmd.Flags().String("script", "", "path to keystroke script")
	cmd.Flags().Bool("interactive", false, "read ops from stdin one at a time, answering each with the rendered screen")
	cmd.Flags().StringP("output", "o", "", "output file (default: record_TIMESTAMP.cast)")
	cmd.Flags().Bool("force", false, "replace an existing cast and its result sidecar")
	cmd.Flags().IntP("width", "W", 220, "terminal width")
	cmd.Flags().IntP("height", "H", 50, "terminal height")
	cmd.Flags().String("title", "", "session title stored in the cast file")
	cmd.Flags().Int("timeout", 0, "maximum seconds to wait for the child after input ends (0 waits indefinitely in interactive mode; script mode defaults to 120)")
	cmd.Flags().Int("key-delay", 300, "milliseconds between keystrokes")
	cmd.Flags().Int("settle-delay", 700, "milliseconds to wait after ENTER for a prompt transition to settle")
	cmd.Flags().Int("expect-timeout", 10000, "default milliseconds EXPECT waits before failing")
	cmd.Flags().String("pointer", `^\s*(?:❯|▸|›|→|»|>)\s`, "regexp matching a menu selection-pointer row, used by SELECT")
	cmd.Flags().Bool("step-markers", true, "record a marker event per script step")
	cmd.Flags().Bool("strict-agent", false, "reject UP/DOWN script steps and literal text on secret-like screens")
	return cmd
}

func runDrive(cmd *cobra.Command, rest []string) {
	outputFormatValue, _ := cmd.Flags().GetString("output-format")
	outputFormat := strings.ToLower(outputFormatValue)
	if outputFormat != "" && outputFormat != "jsonl" {
		fmt.Fprintf(os.Stderr, "invalid --output-format %q; must be \"\" or \"jsonl\"\n", outputFormatValue)
		os.Exit(2)
	}

	scriptPathValue, _ := cmd.Flags().GetString("script")
	interactiveValue, _ := cmd.Flags().GetBool("interactive")
	outputFileValue, _ := cmd.Flags().GetString("output")
	widthValue, _ := cmd.Flags().GetInt("width")
	heightValue, _ := cmd.Flags().GetInt("height")
	titleValue, _ := cmd.Flags().GetString("title")
	timeoutSecValue, _ := cmd.Flags().GetInt("timeout")
	timeoutSet := cmd.Flags().Changed("timeout")
	keyDelayMsValue, _ := cmd.Flags().GetInt("key-delay")
	settleDelayMsValue, _ := cmd.Flags().GetInt("settle-delay")
	expectTimeoutMsValue, _ := cmd.Flags().GetInt("expect-timeout")
	pointerValue, _ := cmd.Flags().GetString("pointer")
	stepMarkersValue, _ := cmd.Flags().GetBool("step-markers")
	strictAgentValue, _ := cmd.Flags().GetBool("strict-agent")
	secretEnv, _ := cmd.Flags().GetStringArray("secret-env")
	secretFiles, _ := cmd.Flags().GetStringArray("secret-file")
	recordCommand, _ := cmd.Flags().GetBool("record-command")
	commandLabel, _ := cmd.Flags().GetString("command-label")
	force, _ := cmd.Flags().GetBool("force")
	scriptPath, interactive, outputFile := &scriptPathValue, &interactiveValue, &outputFileValue
	width, height, title := &widthValue, &heightValue, &titleValue
	timeoutSec, keyDelayMs, settleDelayMs := &timeoutSecValue, &keyDelayMsValue, &settleDelayMsValue
	expectTimeoutMs, pointer, stepMarkers := &expectTimeoutMsValue, &pointerValue, &stepMarkersValue
	if (*scriptPath == "" && !*interactive) || len(rest) == 0 {
		cmd.Usage()
		os.Exit(2)
	}

	redactor, err := newSecretRedactor(secretEnv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "trec drive: %v\n", err)
		os.Exit(2)
	}

	printRedactedError := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		fmt.Fprintln(os.Stderr, redactor.RedactString(msg))
	}

	if err := addSecretFileSpecs(redactor, secretFiles); err != nil {
		printRedactedError("trec drive: %v", err)
		os.Exit(2)
	}

	pointerRe, err := regexp.Compile(*pointer)
	if err != nil {
		printRedactedError("trec drive: bad --pointer regexp: %v", err)
		os.Exit(2)
	}

	var steps []*driveStep
	if *scriptPath != "" {
		steps, err = loadDriveScript(*scriptPath, redactor)
		if err != nil {
			printRedactedError("trec drive: load script: %v", err)
			os.Exit(2)
		}
	}
	if strictAgentValue {
		for _, st := range steps {
			if st.kind == "down" || st.kind == "up" {
				printRedactedError("trec drive: line %d: --strict-agent rejects %s; use SELECT <label> for menu choices", st.line, strings.ToUpper(st.kind))
				os.Exit(2)
			}
		}
	}

	if *outputFile == "" {
		*outputFile = fmt.Sprintf("record_%s.cast", time.Now().Format("20060102_150405"))
	}
	f, err := prepareRecordingOutput(*outputFile, force)
	if err != nil {
		fmt.Fprintf(os.Stderr, "trec drive: create output file: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()

	bw := bufio.NewWriterSize(f, 256*1024)
	var bwMu sync.Mutex
	recorder := newRecordingWriter(bw, &bwMu, redactor)

	build := currentBuildMetadata()
	hdr := castHeader{
		Version:     2,
		Width:       *width,
		Height:      *height,
		Timestamp:   time.Now().Unix(),
		TrecVersion: build.DisplayVersion(),
		TrecBuild:   build,
		Title:       *title,
		Env: map[string]string{
			"TERM": "xterm-256color",
			"CI":   "1",
		},
	}
	if recordCommand {
		hdr.Command = strings.Join(rest, " ")
	}
	hdr.CommandLabel = commandLabel
	if err := recorder.writeHeader(hdr); err != nil {
		fmt.Fprintf(os.Stderr, "trec drive: write header: %v\n", err)
		os.Exit(1)
	}
	if err := recorder.flush(); err != nil {
		fmt.Fprintf(os.Stderr, "trec drive: write header: %v\n", err)
		os.Exit(1)
	}
	started := time.Now()
	pending := newPendingSessionResult(started)
	if err := writePendingSessionResult(*outputFile, pending); err != nil {
		fmt.Fprintf(os.Stderr, "trec drive: write pending summary: %v\n", err)
		os.Exit(1)
	}
	processCmd := exec.Command(rest[0], rest[1:]...)
	// CI=1 + a fixed xterm term type keep bubbletea/promptui rendering
	// deterministic under a driven, non-interactive PTY (no real human
	// terminal behind it).
	processCmd.Env = append(os.Environ(), "TERM=xterm-256color", "CI=1")

	ptmx, err := pty.StartWithSize(processCmd, &pty.Winsize{Rows: uint16(*height), Cols: uint16(*width)})
	if err != nil {
		_ = f.Sync()
		_ = writeSessionResult(*outputFile, sessionResult{
			SessionID: pending.SessionID,
			StartedAt: pending.StartedAt,
			Status:    "failed",
			ExitCode:  -1,
			Error:     fmt.Sprintf("start command: %v", err),
		})
		fmt.Fprintf(os.Stderr, "trec drive: pty start: %v\n", err)
		os.Exit(1)
	}

	var ptyOut io.Reader = ptmx
	if !*interactive {
		ptyOut = io.TeeReader(ptmx, os.Stdout)
	}

	ts := newTerminalSession(ptmx, ptyOut, processCmd, *width, *height, recorder, redactor, false, nil)

	childExitTimeout := 120 * time.Second
	if timeoutSet {
		childExitTimeout = time.Duration(*timeoutSec) * time.Second
	} else if *interactive {
		childExitTimeout = 0
	}
	ds := &driveSession{
		ts:               ts,
		sessionID:        pending.SessionID,
		startedAt:        pending.StartedAt,
		keyDelay:         time.Duration(*keyDelayMs) * time.Millisecond,
		settleDelay:      time.Duration(*settleDelayMs) * time.Millisecond,
		expectTimeout:    time.Duration(*expectTimeoutMs) * time.Millisecond,
		childExitTimeout: childExitTimeout,
		pointerRe:        pointerRe,
		interactive:      *interactive,
		stepMarkers:      *stepMarkers,
		outputFormat:     outputFormat,
		redactor:         redactor,
		strictAgent:      strictAgentValue,
	}

	time.Sleep(500 * time.Millisecond)

	ctx := context.Background()

	for _, st := range steps {
		if st.kind == "quit" {
			break
		}
		stepStarted := time.Now()
		if ds.stepMarkers {
			ds.stepMarker("START", st, 0)
		}
		if err := ds.applyStep(ctx, st); err != nil {
			msg := fmt.Sprintf("trec drive: FAILED at line %d (%s): %v\n", st.line, safeStepDescription(st), err)
			fmt.Fprint(os.Stderr, ds.redactor.RedactString(msg))
			ds.dumpScreen(os.Stderr)
			ds.failureMarker(st, time.Since(stepStarted), err)
			if errClose := ts.close(); errClose != nil {
				fmt.Fprintf(os.Stderr, "trec drive: finalize error: %v\n", errClose)
			}
			if err := f.Sync(); err != nil {
				fmt.Fprintf(os.Stderr, "trec drive: sync cast: %v\n", err)
			}
			if resultErr := writeSessionResult(*outputFile, ds.result("failed", -1, fmt.Sprintf("line %d: %v", st.line, err))); resultErr != nil {
				fmt.Fprintf(os.Stderr, "trec drive: write summary: %v\n", resultErr)
			}
			fmt.Fprintf(os.Stderr, "trec drive: recorded to %s — replay with: trec play %s\n", *outputFile, *outputFile)
			os.Exit(1)
		}
		if ds.stepMarkers {
			ds.stepMarker("OK", st, time.Since(stepStarted))
		}
	}

	interactiveQuitRequested := false
	if *interactive {
		ds.respond(nil)

		inputCh := make(chan string)
		go func() {
			defer close(inputCh)
			sc := bufio.NewScanner(os.Stdin)
			sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
			for sc.Scan() {
				inputCh <- sc.Text()
			}
			if err := sc.Err(); err != nil {
				fmt.Fprintf(os.Stderr, "trec drive: read interactive input: %v\n", err)
			}
		}()

		seq := 0
		for {
			exited, _, _ := ts.getExitState()
			if exited || interactiveQuitRequested {
				break
			}
			select {
			case <-ts.done:
				continue
			case line, ok := <-inputCh:
				if !ok {
					fmt.Fprintln(os.Stderr, "trec drive: interactive stdin closed; no further operations can be sent")
					fmt.Fprintln(os.Stderr, "trec drive: agents must keep this process's stdin session open (for example, a PTY with write_stdin)")
					fmt.Fprintln(os.Stderr, "trec drive: for one-shot exec or heredoc input, use --script with EXPECT and SNAPSHOT instead")
					goto interactiveDone
				}
				seq++
				st, err := parseDriveLine(line, seq)
				if err != nil {
					ds.respond(err)
					continue
				}
				if st == nil {
					continue
				}
				if st.kind == "quit" {
					ds.respond(nil)
					interactiveQuitRequested = true
					continue
				}
				stepStarted := time.Now()
				if ds.stepMarkers {
					ds.stepMarker("START", st, 0)
				}
				err = ds.applyStep(ctx, st)
				if err != nil {
					ds.failureMarker(st, time.Since(stepStarted), err)
				} else if ds.stepMarkers {
					ds.stepMarker("OK", st, time.Since(stepStarted))
				}
				ds.respond(err)
			}
		}
	}

interactiveDone:

	waitDur := time.Duration(0)
	if timeoutSet {
		waitDur = time.Duration(*timeoutSec) * time.Second
	} else if !*interactive {
		waitDur = 120 * time.Second
	}
	if *interactive && interactiveQuitRequested {
		waitDur = 5 * time.Second
	}

	var finalizeErr error
	failed := false
	exited, _, processExitErr := ts.getExitState()
	if !exited {
		if waitDur == 0 {
			_, fErr := ts.waitChildExit(context.Background())
			if fErr != nil {
				finalizeErr = fErr
			}
		} else {
			ctxWait, cancel := context.WithTimeout(context.Background(), waitDur)
			defer cancel()
			waitErr, fErr := ts.waitChildExit(ctxWait)
			if fErr != nil {
				finalizeErr = fErr
			}
			if waitErr == context.DeadlineExceeded {
				if *interactive && interactiveQuitRequested {
					fmt.Fprintln(os.Stderr, "trec drive: ending interactive session — killing process")
				} else {
					fmt.Fprintln(os.Stderr, "trec drive: TIMEOUT — killing process")
					failed = true
				}
				if errClose := ts.close(); errClose != nil {
					if finalizeErr != nil {
						finalizeErr = fmt.Errorf("%w; close error: %v", finalizeErr, errClose)
					} else {
						finalizeErr = errClose
					}
				}
			}
		}
	}

	exited, _, processExitErr = ts.getExitState()
	if !*interactive && processExitErr != nil && !ds.exitAsserted {
		failed = true
	}

	if processExitErr != nil {
		fmt.Fprintf(os.Stderr, "trec drive: process exited: %v\n", processExitErr)
	} else {
		fmt.Fprintln(os.Stderr, "trec drive: process exited 0")
	}

	if errClose := ts.close(); errClose != nil {
		if finalizeErr != nil {
			finalizeErr = fmt.Errorf("%w; close error: %v", finalizeErr, errClose)
		} else {
			finalizeErr = errClose
		}
	}
	if err := f.Sync(); err != nil {
		if finalizeErr != nil {
			finalizeErr = fmt.Errorf("%w; sync cast: %v", finalizeErr, err)
		} else {
			finalizeErr = fmt.Errorf("sync cast: %w", err)
		}
	}

	if finalizeErr != nil {
		fmt.Fprintf(os.Stderr, "trec drive: finalize error: %v\n", finalizeErr)
	}

	fmt.Fprintf(os.Stderr, "trec drive: recorded to %s — replay with: trec play %s\n", *outputFile, *outputFile)
	status := "success"
	message := ""
	if failed || finalizeErr != nil {
		status = "failed"
		if finalizeErr != nil {
			message = finalizeErr.Error()
		} else if processExitErr != nil {
			message = processExitErr.Error()
		}
	}
	_, exitCode, _ := ts.getExitState()
	if err := writeSessionResult(*outputFile, ds.result(status, exitCode, message)); err != nil {
		fmt.Fprintf(os.Stderr, "trec drive: write summary: %v\n", err)
		failed = true
	}

	if failed {
		os.Exit(1)
	}
}

func (ds *driveSession) result(status string, exitCode int, message string) sessionResult {
	lines, _, _, _, _ := ds.ts.redactedScreenSnapshot()
	return sessionResult{
		SessionID:       ds.sessionID,
		StartedAt:       ds.startedAt,
		Status:          status,
		ExitCode:        exitCode,
		Error:           ds.redactor.RedactString(message),
		DurationSeconds: time.Since(ds.ts.start).Seconds(),
		FinalScreen:     lines,
		Snapshots:       append([]sessionSnapshot(nil), ds.snapshots...),
	}
}
