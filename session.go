package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/hinshun/vt10x"
)

type terminalSession struct {
	in  io.WriteCloser
	out io.Reader
	cmd *exec.Cmd

	start time.Time
	cols  int
	rows  int

	vtMu    sync.Mutex
	vt      vt10x.Terminal
	keepRaw bool
	rawOut  bytes.Buffer
	lastOut time.Time

	recorder     *recordingWriter
	redactor     *secretRedactor
	extraClosers []io.Closer

	writeMu sync.Mutex
	closed  bool

	processMu sync.Mutex
	exited    bool
	exitCode  int
	exitErr   error

	done         chan struct{} // Closed when the read loop completes
	closeInOnce  sync.Once
	finalizeOnce sync.Once
	finalizeErr  error
	teardownOnce sync.Once
	teardownDone chan struct{}
	rawTruncated bool

	tainted  bool
	taintBuf string
}

type screenSnapshot struct {
	lines []string
	row   int
	col   int
	rows  int
	cols  int
}

func (s1 *screenSnapshot) equal(s2 *screenSnapshot) bool {
	if s1.row != s2.row || s1.col != s2.col || s1.rows != s2.rows || s1.cols != s2.cols {
		return false
	}
	if len(s1.lines) != len(s2.lines) {
		return false
	}
	for i := range s1.lines {
		if s1.lines[i] != s2.lines[i] {
			return false
		}
	}
	return true
}

func normalizeScreen(screenText string) []string {
	lines := strings.Split(screenText, "\n")
	last := len(lines) - 1
	for last >= 0 && strings.TrimSpace(lines[last]) == "" {
		last--
	}
	if last < 0 {
		return []string{}
	}
	lines = lines[:last+1]
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " ")
	}
	return lines
}

func newTerminalSession(in io.WriteCloser, out io.Reader, cmd *exec.Cmd, cols, rows int, recorder *recordingWriter, redactor *secretRedactor, keepRaw bool, extraClosers []io.Closer) *terminalSession {
	ts := &terminalSession{
		in:           in,
		out:          out,
		cmd:          cmd,
		start:        time.Now(),
		cols:         cols,
		rows:         rows,
		vt:           vt10x.New(vt10x.WithSize(cols, rows)),
		lastOut:      time.Now(),
		keepRaw:      keepRaw,
		recorder:     recorder,
		redactor:     redactor,
		extraClosers: extraClosers,
		done:         make(chan struct{}),
		teardownDone: make(chan struct{}),
	}

	go ts.readLoop()
	go ts.waitProcess()

	return ts
}

func (ts *terminalSession) readLoop() {
	buf := make([]byte, 32*1024)
	for {
		n, err := ts.out.Read(buf)
		if n > 0 {
			ts.feedOutput(buf[:n])
		}
		if err != nil {
			break
		}
	}
	close(ts.done)
}

func (ts *terminalSession) waitProcess() {
	var err error
	if ts.cmd != nil {
		err = ts.cmd.Wait()
	}
	<-ts.done // wait for read loop to naturally drain and complete

	ts.teardown()

	ts.processMu.Lock()
	ts.exited = true
	ts.exitErr = err
	if ts.cmd != nil && ts.cmd.ProcessState != nil {
		ts.exitCode = ts.cmd.ProcessState.ExitCode()
	} else {
		ts.exitCode = -1
	}
	ts.processMu.Unlock()
}

func (ts *terminalSession) feedOutput(b []byte) {
	ts.vtMu.Lock()
	defer ts.vtMu.Unlock()
	ts.vt.Write(b)

	if ts.redactor != nil && ts.redactor.hasSecrets() && !ts.tainted {
		ts.taintBuf += string(b)
		if ts.redactor.AnySecretIn(ts.taintBuf) {
			ts.tainted = true
			ts.taintBuf = ""
		} else {
			maxLen := ts.redactor.maxSecretBytes() * 2
			if maxLen < 1024 {
				maxLen = 1024
			}
			if len(ts.taintBuf) > maxLen {
				ts.taintBuf = ts.taintBuf[len(ts.taintBuf)-maxLen:]
			}
		}
		if !ts.tainted {
			lines := normalizeScreen(ts.vt.String())
			redacted := ts.redactor.redactScreen(lines)
			if len(redacted) == 1 && redacted[0] == "<screen redacted>" {
				ts.tainted = true
			}
		}
	}

	if ts.keepRaw {
		ts.rawOut.Write(b)
		if ts.rawOut.Len() > 1024*1024 {
			ts.rawTruncated = true
			data := ts.rawOut.Bytes()
			keep := data[len(data)-512*1024:]
			ts.rawOut.Reset()
			ts.rawOut.Write(keep)
		}
	}
	ts.lastOut = time.Now()

	if ts.recorder != nil {
		ts.recorder.output(time.Since(ts.start).Seconds(), string(b))
		ts.recorder.flush()
	}
}

func (ts *terminalSession) rawScreenSnapshot() (lines []string, row, col, rows, cols int) {
	ts.vtMu.Lock()
	defer ts.vtMu.Unlock()
	lines = normalizeScreen(ts.vt.String())
	cursor := ts.vt.Cursor()
	cols, rows = ts.vt.Size()
	return lines, cursor.Y, cursor.X, rows, cols
}

func (ts *terminalSession) redactedScreenSnapshot() (lines []string, row, col, rows, cols int) {
	ts.vtMu.Lock()
	defer ts.vtMu.Unlock()
	if ts.tainted || (ts.redactor != nil && ts.redactor.PendingSecretPrefix(ts.taintBuf)) {
		cursor := ts.vt.Cursor()
		cols, rows = ts.vt.Size()
		return []string{"<screen redacted>"}, cursor.Y, cursor.X, rows, cols
	}
	lines = normalizeScreen(ts.vt.String())
	if ts.redactor != nil {
		lines = ts.redactor.redactScreen(lines)
	}
	cursor := ts.vt.Cursor()
	cols, rows = ts.vt.Size()
	return lines, cursor.Y, cursor.X, rows, cols
}

func (ts *terminalSession) getSnapshot() *screenSnapshot {
	lines, r, c, rows, cols := ts.rawScreenSnapshot()
	return &screenSnapshot{lines: append([]string(nil), lines...), row: r, col: c, rows: rows, cols: cols}
}

func (ts *terminalSession) screenLines() []string {
	lines, _, _, _, _ := ts.rawScreenSnapshot()
	return lines
}

func (ts *terminalSession) cursorPos() (row, col int) {
	_, row, col, _, _ = ts.rawScreenSnapshot()
	return row, col
}

func (ts *terminalSession) quietFor() time.Duration {
	ts.vtMu.Lock()
	defer ts.vtMu.Unlock()
	return time.Since(ts.lastOut)
}

func (ts *terminalSession) isProcessExited() bool {
	ts.processMu.Lock()
	defer ts.processMu.Unlock()
	return ts.exited
}

// waitForText waits until the text appears.
func (ts *terminalSession) waitForText(ctx context.Context, opName, text string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if screenContains(ts.screenLines(), text) {
			return nil
		}
		if time.Now().After(deadline) {
			lastOut := ts.quietFor()
			return fmt.Errorf("%s %q: timeout after %v (last output %v ago)", opName, text, timeout, lastOut)
		}
		if ts.isProcessExited() {
			return fmt.Errorf("%s %q: process exited before text appeared", opName, text)
		}
		time.Sleep(40 * time.Millisecond)
	}
}

func (ts *terminalSession) waitForRegex(ctx context.Context, opName string, re *regexp.Regexp, timeout time.Duration, screenRegex bool) error {
	deadline := time.Now().Add(timeout)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		lines := ts.screenLines()
		matched := false
		if screenRegex {
			if re.MatchString(strings.Join(lines, "\n")) {
				matched = true
			}
		} else {
			for _, line := range lines {
				if re.MatchString(line) {
					matched = true
					break
				}
			}
		}

		if matched {
			return nil
		}

		if time.Now().After(deadline) {
			lastOut := ts.quietFor()
			return fmt.Errorf("%s %q: timeout after %v (last output %v ago)", opName, re.String(), timeout, lastOut)
		}
		if ts.isProcessExited() {
			return fmt.Errorf("%s %q: process exited before match", opName, re.String())
		}
		time.Sleep(40 * time.Millisecond)
	}
}

// waitForChange waits until the screen content is different from startStr.
func (ts *terminalSession) waitForChange(ctx context.Context, startSnap *screenSnapshot, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		currSnap := ts.getSnapshot()
		if !currSnap.equal(startSnap) {
			return nil
		}

		if time.Now().After(deadline) {
			lastOut := ts.quietFor()
			return fmt.Errorf("EXPECT_CHANGE: timeout after %v (last output %v ago)", timeout, lastOut)
		}
		if ts.isProcessExited() {
			return fmt.Errorf("EXPECT_CHANGE: process exited before screen changed")
		}
		time.Sleep(40 * time.Millisecond)
	}
}

// waitQuiet waits until the child has produced no output for quiet, giving up after limit.
func (ts *terminalSession) waitQuiet(ctx context.Context, quiet, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if ts.quietFor() >= quiet {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("EXPECT_QUIET %v: output still active after %v", quiet, limit)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (ts *terminalSession) sendBytesLocked(b []byte, secretLabel string) (int, error) {
	if ts.closed {
		return 0, fmt.Errorf("session is closed")
	}
	if len(b) == 0 {
		return 0, nil
	}
	n, err := ts.in.Write(b)
	if err == nil && n < len(b) {
		err = io.ErrShortWrite
	}
	if ts.recorder != nil {
		var recStr string
		if secretLabel != "" {
			_ = ts.recorder.flushInput()
			if n < len(b) {
				recStr = fmt.Sprintf("<redacted:%s partial %d/%d>", secretLabel, n, len(b))
			} else {
				recStr = fmt.Sprintf("<redacted:%s>", secretLabel)
			}
			if recErr := ts.recorder.event(time.Since(ts.start).Seconds(), "i", recStr); recErr != nil {
				if err == nil {
					err = recErr
				}
				return n, err
			}
		} else {
			if recErr := ts.recorder.input(time.Since(ts.start).Seconds(), string(b[:n])); recErr != nil {
				if err == nil {
					err = recErr
				}
				return n, err
			}
		}
		if flushErr := ts.recorder.flush(); flushErr != nil {
			if err == nil {
				err = flushErr
			}
			return n, err
		}
	}
	return n, err
}

func (ts *terminalSession) sendBytes(b []byte, secretLabel string) (int, error) {
	ts.writeMu.Lock()
	defer ts.writeMu.Unlock()
	return ts.sendBytesLocked(b, secretLabel)
}

func (ts *terminalSession) sendTextLocked(text string, secretLabel string, keyDelay time.Duration) (int, error) {
	if ts.closed {
		return 0, fmt.Errorf("session is closed")
	}
	if len(text) == 0 {
		return 0, nil
	}
	written := 0
	var finalErr error
	for _, r := range text {
		b := []byte(string(r))
		n, err := ts.in.Write(b)
		written += n
		if err == nil && n < len(b) {
			err = io.ErrShortWrite
		}
		if err != nil {
			finalErr = err
			break
		}
		time.Sleep(keyDelay)
	}
	if ts.recorder != nil {
		var recStr string
		if secretLabel != "" {
			_ = ts.recorder.flushInput()
			if written < len(text) {
				recStr = fmt.Sprintf("<redacted:%s partial %d/%d>", secretLabel, written, len(text))
			} else {
				recStr = fmt.Sprintf("<redacted:%s>", secretLabel)
			}
			if recErr := ts.recorder.event(time.Since(ts.start).Seconds(), "i", recStr); recErr != nil {
				if finalErr == nil {
					finalErr = recErr
				}
				return written, finalErr
			}
		} else {
			if recErr := ts.recorder.input(time.Since(ts.start).Seconds(), text[:written]); recErr != nil {
				if finalErr == nil {
					finalErr = recErr
				}
				return written, finalErr
			}
		}
		if err := ts.recorder.flush(); err != nil {
			if finalErr == nil {
				finalErr = err
			}
			return written, finalErr
		}
	}
	return written, finalErr
}

func (ts *terminalSession) sendText(text string, secretLabel string, keyDelay time.Duration) (int, error) {
	ts.writeMu.Lock()
	defer ts.writeMu.Unlock()
	return ts.sendTextLocked(text, secretLabel, keyDelay)
}

func (ts *terminalSession) selectLabel(ctx context.Context, label string, pointerRe *regexp.Regexp, keyDelay time.Duration) (int, error) {
	if label == "" {
		return 0, fmt.Errorf("SELECT needs a non-empty label")
	}

	ts.writeMu.Lock()
	defer ts.writeMu.Unlock()

	maxPress := 3 * ts.rows
	written := 0
	for press := 0; ; press++ {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		lines, _, _, _, _ := ts.rawScreenSnapshot()
		pointerRows := make([]int, 0, 1)
		for idx, l := range lines {
			if pointerRe.MatchString(l) {
				pointerRows = append(pointerRows, idx)
			}
		}
		pIdx := -1
		if len(pointerRows) == 1 {
			pIdx = pointerRows[0]
		} else if len(pointerRows) > 1 {
			return written, fmt.Errorf("SELECT %q: ambiguous pointer rows %v", label, pointerRows)
		}
		if pIdx >= 0 && strings.Contains(lines[pIdx], label) {
			return written, nil
		}
		if press >= maxPress {
			if pIdx >= 0 {
				return written, fmt.Errorf("SELECT %q: not reached after %d presses (pointer stuck on row %d)", label, press, pIdx)
			}
			return written, fmt.Errorf("SELECT %q: not reached after %d presses (no pointer row found)", label, press)
		}
		key := "\x1b[B" // down
		if pIdx >= 0 {
			for idx, l := range lines {
				if idx != pIdx && strings.Contains(l, label) {
					if idx < pIdx {
						key = "\x1b[A" // label is above the pointer
					}
					break
				}
			}
		}
		n, err := ts.sendBytesLocked([]byte(key), "")
		written += n
		if err != nil {
			return written, err
		}
		time.Sleep(keyDelay)
		if err := ts.waitQuiet(ctx, 120*time.Millisecond, 2*time.Second); err != nil {
			return written, err
		}
	}
}

func (ts *terminalSession) closeIn() {
	ts.closeInOnce.Do(func() {
		if ts.in != nil {
			_ = ts.in.Close()
		}
	})
}

func (ts *terminalSession) finalize() error {
	ts.finalizeOnce.Do(func() {
		var errs []string
		if ts.recorder != nil {
			_ = ts.recorder.flushInput()
			_ = ts.recorder.flushOutput()
			_ = ts.recorder.flush()
			if err := ts.recorder.getError(); err != nil {
				errs = append(errs, fmt.Sprintf("recording error: %v", err))
			}
		}
		for _, c := range ts.extraClosers {
			if err := c.Close(); err != nil {
				errs = append(errs, fmt.Sprintf("close resource: %v", err))
			}
		}
		if len(errs) > 0 {
			ts.finalizeErr = fmt.Errorf("finalize failed: %s", strings.Join(errs, "; "))
		}
	})
	return ts.finalizeErr
}

func (ts *terminalSession) teardown() {
	ts.teardownOnce.Do(func() {
		ts.writeMu.Lock()
		ts.closed = true
		ts.closeIn()
		ts.writeMu.Unlock()

		_ = ts.finalize()
		close(ts.teardownDone)
	})
	<-ts.teardownDone
}

func (ts *terminalSession) close() error {
	ts.writeMu.Lock()
	if ts.closed {
		ts.writeMu.Unlock()
		<-ts.teardownDone
		return ts.finalizeErr
	}
	ts.closed = true
	ts.closeIn()
	ts.writeMu.Unlock()

	if ts.cmd != nil && ts.cmd.Process != nil {
		_ = ts.cmd.Process.Kill()
	}
	<-ts.done
	ts.teardown()
	return ts.finalizeErr
}

// readRaw reads all buffered raw output since the last call, returning it and whether it was truncated.
func (ts *terminalSession) readRaw() (string, bool) {
	ts.vtMu.Lock()
	defer ts.vtMu.Unlock()
	o := ts.rawOut.String()
	ts.rawOut.Reset()
	t := ts.rawTruncated
	ts.rawTruncated = false
	return o, t
}

func (ts *terminalSession) getExitState() (exited bool, code int, err error) {
	ts.processMu.Lock()
	defer ts.processMu.Unlock()
	return ts.exited, ts.exitCode, ts.exitErr
}

func (ts *terminalSession) resize(cols, rows int) error {
	if cols < 1 || cols > maxMCPDimension {
		return fmt.Errorf("cols must be between 1 and %d", maxMCPDimension)
	}
	if rows < 1 || rows > maxMCPDimension {
		return fmt.Errorf("rows must be between 1 and %d", maxMCPDimension)
	}
	if f, ok := ts.in.(*os.File); ok {
		if err := pty.Setsize(f, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)}); err != nil {
			return fmt.Errorf("resize PTY: %w", err)
		}
	}
	ts.vtMu.Lock()
	ts.vt.Resize(cols, rows)
	ts.cols = cols
	ts.rows = rows
	ts.vtMu.Unlock()
	return nil
}

func (ts *terminalSession) waitChildExit(ctx context.Context) (processExitErr error, finalizeErr error) {
	for {
		if err := ctx.Err(); err != nil {
			return err, nil
		}
		exited, _, _ := ts.getExitState()
		if exited {
			return ts.exitErr, ts.finalizeErr
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func screenContains(lines []string, text string) bool {
	for _, l := range lines {
		if strings.Contains(l, text) {
			return true
		}
	}
	return false
}
