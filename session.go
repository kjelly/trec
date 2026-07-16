package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

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

func newTerminalSession(in io.WriteCloser, out io.Reader, cmd *exec.Cmd, cols, rows int, recorder *recordingWriter, keepRaw bool, extraClosers []io.Closer) *terminalSession {
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

func (ts *terminalSession) screenSnapshot() (lines []string, row, col, rows, cols int) {
	ts.vtMu.Lock()
	defer ts.vtMu.Unlock()
	lines = normalizeScreen(ts.vt.String())
	cursor := ts.vt.Cursor()
	cols, rows = ts.vt.Size()
	return lines, cursor.Y, cursor.X, rows, cols
}

// screenLines returns the emulated screen as one string per row, right-trimmed.
func (ts *terminalSession) screenLines() []string {
	lines, _, _, _, _ := ts.screenSnapshot()
	return lines
}

func (ts *terminalSession) cursorPos() (row, col int) {
	_, row, col, _, _ = ts.screenSnapshot()
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

// waitForText polls the emulated screen until text appears on some row.
func (ts *terminalSession) waitForText(ctx context.Context, text string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if screenContains(ts.screenLines(), text) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("EXPECT %q: not on screen after %v", text, timeout)
		}
		if ts.isProcessExited() {
			return fmt.Errorf("EXPECT %q: process exited before text appeared", text)
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

func (ts *terminalSession) sendBytesLocked(b []byte) error {
	if ts.closed {
		return fmt.Errorf("session is closed")
	}
	if _, err := ts.in.Write(b); err != nil {
		return err
	}
	if ts.recorder != nil {
		if err := ts.recorder.event(time.Since(ts.start).Seconds(), "i", string(b)); err != nil {
			return err
		}
		if err := ts.recorder.flush(); err != nil {
			return err
		}
	}
	return nil
}

func (ts *terminalSession) sendBytes(b []byte) error {
	ts.writeMu.Lock()
	defer ts.writeMu.Unlock()
	return ts.sendBytesLocked(b)
}

func (ts *terminalSession) sendTextLocked(text string, recorded string, keyDelay time.Duration) error {
	if ts.closed {
		return fmt.Errorf("session is closed")
	}
	for _, r := range text {
		if _, err := ts.in.Write([]byte(string(r))); err != nil {
			return err
		}
		time.Sleep(keyDelay)
	}
	if ts.recorder != nil {
		if err := ts.recorder.event(time.Since(ts.start).Seconds(), "i", recorded); err != nil {
			return err
		}
		if err := ts.recorder.flush(); err != nil {
			return err
		}
	}
	return nil
}

func (ts *terminalSession) sendText(text string, recorded string, keyDelay time.Duration) error {
	ts.writeMu.Lock()
	defer ts.writeMu.Unlock()
	return ts.sendTextLocked(text, recorded, keyDelay)
}

func (ts *terminalSession) selectLabel(ctx context.Context, label string, pointerRe *regexp.Regexp, keyDelay time.Duration) error {
	if label == "" {
		return fmt.Errorf("SELECT needs a non-empty label")
	}

	ts.writeMu.Lock()
	defer ts.writeMu.Unlock()

	maxPress := 3 * ts.rows
	for press := 0; ; press++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		lines, _, _, _, _ := ts.screenSnapshot()
		pIdx := -1
		for idx, l := range lines {
			if pointerRe.MatchString(l) {
				pIdx = idx
				break
			}
		}
		if pIdx >= 0 && strings.Contains(lines[pIdx], label) {
			return nil
		}
		if press >= maxPress {
			at := "no pointer row found"
			if pIdx >= 0 {
				at = fmt.Sprintf("pointer stuck at %q", strings.TrimSpace(lines[pIdx]))
			}
			return fmt.Errorf("SELECT %q: not reached after %d presses (%s)", label, press, at)
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
		if err := ts.sendBytesLocked([]byte(key)); err != nil {
			return err
		}
		time.Sleep(keyDelay)
		if err := ts.waitQuiet(ctx, 120*time.Millisecond, 2*time.Second); err != nil {
			return err
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
