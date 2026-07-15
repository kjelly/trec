package main

import (
	"bufio"
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
	"github.com/hinshun/vt10x"
	"github.com/spf13/cobra"
)

// driveStep is one instruction from a keystroke script, in the same format
// pilot's ptydrive scaffolding used to drive promptui/bubbletea wizards.
type driveStep struct {
	kind string // text, enter, down, up, space, tab, ctrlc, backspace, wait,
	// expect, expect_quiet, assert, select, snapshot, quit
	text string
	n    int
	raw  string // original script line, for markers and error reports
	line int    // script line number (or interactive command sequence)
}

// parseDriveLine parses one script line. Returns (nil, nil) for blank lines
// and comments.
func parseDriveLine(raw string, lineNo int) (*driveStep, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return nil, nil
	}
	fields := strings.SplitN(trimmed, " ", 2)
	op := strings.ToUpper(fields[0])
	arg := ""
	if len(fields) > 1 {
		arg = fields[1]
	}
	st := &driveStep{raw: trimmed, line: lineNo}

	// EXPECT@<ms> overrides the default --expect-timeout for one step.
	if ms, ok := strings.CutPrefix(op, "EXPECT@"); ok {
		n, err := strconv.Atoi(ms)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("line %d: bad timeout in %q", lineNo, fields[0])
		}
		if arg == "" {
			return nil, fmt.Errorf("line %d: EXPECT needs text", lineNo)
		}
		st.kind, st.text, st.n = "expect", arg, n
		return st, nil
	}

	switch op {
	case "TEXT":
		st.kind, st.text = "text", arg
	case "ENTER":
		st.kind = "enter"
	case "DOWN":
		st.kind, st.n = "down", atoiOr1(arg)
	case "UP":
		st.kind, st.n = "up", atoiOr1(arg)
	case "SPACE":
		st.kind = "space"
	case "TAB":
		st.kind = "tab"
	case "CTRLC":
		st.kind = "ctrlc"
	case "BACKSPACE":
		st.kind, st.n = "backspace", atoiOr1(arg)
	case "WAIT":
		st.kind, st.n = "wait", atoiOr1(arg)
	case "EXPECT":
		if arg == "" {
			return nil, fmt.Errorf("line %d: EXPECT needs text", lineNo)
		}
		st.kind, st.text = "expect", arg
	case "EXPECT_QUIET":
		st.kind, st.n = "expect_quiet", atoiOrDef(arg, 300)
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
		st.kind = "snapshot"
	case "QUIT":
		st.kind = "quit"
	default:
		return nil, fmt.Errorf("line %d: unknown op %q", lineNo, op)
	}
	return st, nil
}

func loadDriveScript(path string) ([]*driveStep, error) {
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
			steps = append(steps, st)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return steps, nil
}

func atoiOr1(s string) int {
	return atoiOrDef(s, 1)
}

func atoiOrDef(s string, def int) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return def
	}
	return n
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
	ptmx  *os.File
	bw    *bufio.Writer
	bwMu  *sync.Mutex
	start time.Time

	cols, rows    int
	keyDelay      time.Duration
	settleDelay   time.Duration
	expectTimeout time.Duration
	pointerRe     *regexp.Regexp
	interactive   bool
	stepMarkers   bool

	vtMu    sync.Mutex
	vt      vt10x.Terminal
	lastOut time.Time
}

// feedOutput mirrors one chunk of child output into the VT emulator.
// Called only from the PTY reader goroutine.
func (ds *driveSession) feedOutput(b []byte) {
	ds.vtMu.Lock()
	ds.vt.Write(b)
	ds.lastOut = time.Now()
	ds.vtMu.Unlock()
}

// screenLines returns the emulated screen as one string per row, right-trimmed.
func (ds *driveSession) screenLines() []string {
	ds.vtMu.Lock()
	s := ds.vt.String()
	ds.vtMu.Unlock()
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " ")
	}
	return lines
}

func (ds *driveSession) cursorPos() (row, col int) {
	ds.vtMu.Lock()
	c := ds.vt.Cursor()
	ds.vtMu.Unlock()
	return c.Y, c.X
}

func (ds *driveSession) quietFor() time.Duration {
	ds.vtMu.Lock()
	d := time.Since(ds.lastOut)
	ds.vtMu.Unlock()
	return d
}

func screenContains(lines []string, text string) bool {
	for _, l := range lines {
		if strings.Contains(l, text) {
			return true
		}
	}
	return false
}

// waitForText polls the emulated screen until text appears on some row.
// Matching is per-row, so text wrapped across rows will not match.
func (ds *driveSession) waitForText(text string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if screenContains(ds.screenLines(), text) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("EXPECT %q: not on screen after %v", text, timeout)
		}
		time.Sleep(40 * time.Millisecond)
	}
}

// waitQuiet waits until the child has produced no output for quiet, giving
// up after limit.
func (ds *driveSession) waitQuiet(quiet, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	for {
		if ds.quietFor() >= quiet {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("EXPECT_QUIET %v: output still active after %v", quiet, limit)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// sendBytes writes keystroke bytes to the child and records them as a timed
// "i" event, so the recording stays a normal asciicast v2 file — playable
// with 'trec play' and readable with 'trec transcript' exactly like a
// human-driven session.
func (ds *driveSession) sendBytes(b []byte) error {
	if _, err := ds.ptmx.Write(b); err != nil {
		return err
	}
	writeEvent(ds.bw, ds.bwMu, time.Since(ds.start).Seconds(), "i", string(b))
	ds.bwMu.Lock()
	ds.bw.Flush()
	ds.bwMu.Unlock()
	return nil
}

// marker records an "m" event; trec play jumps between them with n/N.
func (ds *driveSession) marker(label string) {
	writeEvent(ds.bw, ds.bwMu, time.Since(ds.start).Seconds(), "m", label)
	ds.bwMu.Lock()
	ds.bw.Flush()
	ds.bwMu.Unlock()
}

// selectLabel navigates a menu until the pointed row (the one matching
// --pointer) contains label, pressing UP when the label is visible above the
// pointer and DOWN otherwise. This replaces blind "DOWN n" counting: menu
// reordering or an extra item no longer desynchronizes the script.
func (ds *driveSession) selectLabel(label string) error {
	maxPress := 3 * ds.rows
	for press := 0; ; press++ {
		lines := ds.screenLines()
		pIdx := -1
		for idx, l := range lines {
			if ds.pointerRe.MatchString(l) {
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
		if err := ds.sendBytes([]byte(key)); err != nil {
			return err
		}
		time.Sleep(ds.keyDelay)
		ds.waitQuiet(120*time.Millisecond, 2*time.Second)
	}
}

// dumpScreen writes the emulated screen to w, trailing blank rows trimmed.
func (ds *driveSession) dumpScreen(w io.Writer) {
	lines := ds.screenLines()
	last := len(lines) - 1
	for last >= 0 && lines[last] == "" {
		last--
	}
	fmt.Fprintf(w, "---- screen %dx%d ----\n", ds.cols, ds.rows)
	for _, l := range lines[:last+1] {
		fmt.Fprintf(w, "| %s\n", l)
	}
	fmt.Fprintln(w, "---- end screen ----")
}

func (ds *driveSession) applyStep(st *driveStep) error {
	switch st.kind {
	case "text":
		for _, r := range st.text {
			if err := ds.sendBytes([]byte(string(r))); err != nil {
				return err
			}
			time.Sleep(ds.keyDelay)
		}
	case "enter":
		if err := ds.sendBytes([]byte("\r")); err != nil {
			return err
		}
		// Enter always causes a prompt TRANSITION (submits the current
		// promptui.Select/Prompt or bubbletea model and renders the
		// next one) — that render + the next prompt's readline setup
		// is where a following key send can race ahead of the process
		// actually being ready to consume it. Steady same-prompt
		// navigation (repeated arrow presses) doesn't need this; only
		// the transition does.
		time.Sleep(ds.settleDelay)
	case "down", "up":
		seq := "\x1b[B"
		if st.kind == "up" {
			seq = "\x1b[A"
		}
		for range max1(st.n) {
			if err := ds.sendBytes([]byte(seq)); err != nil {
				return err
			}
			time.Sleep(ds.keyDelay)
		}
	case "space":
		if err := ds.sendBytes([]byte(" ")); err != nil {
			return err
		}
		time.Sleep(ds.keyDelay)
	case "tab":
		if err := ds.sendBytes([]byte("\t")); err != nil {
			return err
		}
		time.Sleep(ds.keyDelay)
	case "ctrlc":
		if err := ds.sendBytes([]byte{0x03}); err != nil {
			return err
		}
		time.Sleep(ds.keyDelay)
	case "backspace":
		// promptui's readline (chzyer/readline) uses DEL (127) as its
		// backspace char (CharBackspace) — needed to clear a
		// promptui.Prompt's pre-filled Default text (AllowEdit:true
		// puts the cursor at the end of it, so plain typing appends
		// rather than replaces).
		for range max1(st.n) {
			if err := ds.sendBytes([]byte{127}); err != nil {
				return err
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
		return ds.waitForText(st.text, timeout)
	case "expect_quiet":
		return ds.waitQuiet(time.Duration(st.n)*time.Millisecond, ds.expectTimeout)
	case "assert":
		if !screenContains(ds.screenLines(), st.text) {
			return fmt.Errorf("ASSERT %q: not on screen", st.text)
		}
	case "select":
		return ds.selectLabel(st.text)
	case "snapshot":
		// In interactive mode every response already carries the screen.
		if !ds.interactive {
			ds.dumpScreen(os.Stderr)
		}
	case "quit":
		// handled by the caller
	}
	return nil
}

// respond prints one interactive-protocol response to stdout: a status line
// ("OK" or "ERR <message>"), the cursor position, then the full emulated
// screen with a fixed row count so the reader can parse without a sentinel.
func (ds *driveSession) respond(err error) {
	if err != nil {
		fmt.Printf("ERR %v\n", err)
	} else {
		fmt.Println("OK")
	}
	row, col := ds.cursorPos()
	lines := ds.screenLines()
	fmt.Printf("CURSOR %d %d\n", row, col)
	fmt.Printf("SCREEN %d %d\n", len(lines), ds.cols)
	for _, l := range lines {
		fmt.Println(l)
	}
}

func newDriveCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "drive --script steps.txt [options] -- <command> [args...]", Short: "Drive a TUI with a keystroke script", Args: cobra.ArbitraryArgs, Run: runDrive}
	cmd.Flags().String("script", "", "path to keystroke script")
	cmd.Flags().Bool("interactive", false, "read ops from stdin one at a time, answering each with the rendered screen")
	cmd.Flags().StringP("output", "o", "", "output file (default: record_TIMESTAMP.cast)")
	cmd.Flags().IntP("width", "W", 220, "terminal width")
	cmd.Flags().IntP("height", "H", 50, "terminal height")
	cmd.Flags().String("title", "", "session title stored in the cast file")
	cmd.Flags().Int("timeout", 120, "overall timeout in seconds after the script finishes")
	cmd.Flags().Int("key-delay", 300, "milliseconds between keystrokes")
	cmd.Flags().Int("settle-delay", 700, "milliseconds to wait after ENTER for a prompt transition to settle")
	cmd.Flags().Int("expect-timeout", 10000, "default milliseconds EXPECT waits before failing")
	cmd.Flags().String("pointer", `^\s*(?:❯|▸|›|→|»|>)\s`, "regexp matching a menu selection-pointer row, used by SELECT")
	cmd.Flags().Bool("step-markers", true, "record a marker event per script step")
	return cmd
}

func runDrive(cmd *cobra.Command, rest []string) {
	scriptPathValue, _ := cmd.Flags().GetString("script")
	interactiveValue, _ := cmd.Flags().GetBool("interactive")
	outputFileValue, _ := cmd.Flags().GetString("output")
	widthValue, _ := cmd.Flags().GetInt("width")
	heightValue, _ := cmd.Flags().GetInt("height")
	titleValue, _ := cmd.Flags().GetString("title")
	timeoutSecValue, _ := cmd.Flags().GetInt("timeout")
	keyDelayMsValue, _ := cmd.Flags().GetInt("key-delay")
	settleDelayMsValue, _ := cmd.Flags().GetInt("settle-delay")
	expectTimeoutMsValue, _ := cmd.Flags().GetInt("expect-timeout")
	pointerValue, _ := cmd.Flags().GetString("pointer")
	stepMarkersValue, _ := cmd.Flags().GetBool("step-markers")
	scriptPath, interactive, outputFile := &scriptPathValue, &interactiveValue, &outputFileValue
	width, height, title := &widthValue, &heightValue, &titleValue
	timeoutSec, keyDelayMs, settleDelayMs := &timeoutSecValue, &keyDelayMsValue, &settleDelayMsValue
	expectTimeoutMs, pointer, stepMarkers := &expectTimeoutMsValue, &pointerValue, &stepMarkersValue
	if (*scriptPath == "" && !*interactive) || len(rest) == 0 {
		cmd.Usage()
		os.Exit(2)
	}

	pointerRe, err := regexp.Compile(*pointer)
	if err != nil {
		fmt.Fprintf(os.Stderr, "trec drive: bad --pointer regexp: %v\n", err)
		os.Exit(2)
	}

	var steps []*driveStep
	if *scriptPath != "" {
		steps, err = loadDriveScript(*scriptPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "trec drive: load script: %v\n", err)
			os.Exit(2)
		}
	}

	if *outputFile == "" {
		*outputFile = fmt.Sprintf("record_%s.cast", time.Now().Format("20060102_150405"))
	}
	f, err := os.Create(*outputFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "trec drive: create output file: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()

	bw := bufio.NewWriterSize(f, 256*1024)

	hdr := castHeader{
		Version:   2,
		Width:     *width,
		Height:    *height,
		Timestamp: time.Now().Unix(),
		Command:   strings.Join(rest, " "),
		Title:     *title,
		Env: map[string]string{
			"TERM": "xterm-256color",
			"CI":   "1",
		},
	}
	hdrJSON, _ := json.Marshal(hdr)
	fmt.Fprintln(bw, string(hdrJSON))

	processCmd := exec.Command(rest[0], rest[1:]...)
	// CI=1 + a fixed xterm term type keep bubbletea/promptui rendering
	// deterministic under a driven, non-interactive PTY (no real human
	// terminal behind it).
	processCmd.Env = append(os.Environ(), "TERM=xterm-256color", "CI=1")

	ptmx, err := pty.StartWithSize(processCmd, &pty.Winsize{Rows: uint16(*height), Cols: uint16(*width)})
	if err != nil {
		fmt.Fprintf(os.Stderr, "trec drive: pty start: %v\n", err)
		os.Exit(1)
	}
	defer ptmx.Close()

	var bwMu sync.Mutex
	ds := &driveSession{
		ptmx:          ptmx,
		bw:            bw,
		bwMu:          &bwMu,
		start:         time.Now(),
		cols:          *width,
		rows:          *height,
		keyDelay:      time.Duration(*keyDelayMs) * time.Millisecond,
		settleDelay:   time.Duration(*settleDelayMs) * time.Millisecond,
		expectTimeout: time.Duration(*expectTimeoutMs) * time.Millisecond,
		pointerRe:     pointerRe,
		interactive:   *interactive,
		stepMarkers:   *stepMarkers,
		vt:            vt10x.New(vt10x.WithSize(*width, *height)),
		lastOut:       time.Now(),
	}

	done := make(chan struct{})
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				elapsed := time.Since(ds.start).Seconds()
				if !*interactive {
					os.Stdout.Write(buf[:n])
				}
				ds.feedOutput(buf[:n])
				writeEvent(bw, &bwMu, elapsed, "o", string(buf[:n]))
				bwMu.Lock()
				bw.Flush()
				bwMu.Unlock()
			}
			if err != nil {
				break
			}
		}
		close(done)
	}()

	// Give the process a moment to render its first prompt before we
	// start typing — matches the pacing lesson from driving promptui/
	// bubbletea wizards: real keypress cadence, not a burst.
	time.Sleep(500 * time.Millisecond)

	// Script phase: fail fast. A step that diverges from the real screen
	// state stops the drive immediately, so the recording ends at the
	// point of divergence instead of blindly typing into the wrong prompt.
	for _, st := range steps {
		if st.kind == "quit" {
			break
		}
		if ds.stepMarkers && st.kind != "snapshot" {
			ds.marker(fmt.Sprintf("line %d: %s", st.line, st.raw))
		}
		if err := ds.applyStep(st); err != nil {
			fmt.Fprintf(os.Stderr, "trec drive: FAILED at line %d (%s): %v\n", st.line, st.raw, err)
			ds.dumpScreen(os.Stderr)
			if ds.stepMarkers {
				ds.marker(fmt.Sprintf("FAILED line %d: %s (%v)", st.line, st.raw, err))
			}
			_ = processCmd.Process.Kill()
			<-done
			bw.Flush()
			fmt.Fprintf(os.Stderr, "trec drive: recorded to %s — replay with: trec play %s\n", *outputFile, *outputFile)
			os.Exit(1)
		}
	}

	// Interactive phase: one op per stdin line, one screen-bearing response
	// per op. Errors are reported to the caller instead of ending the
	// session — the driving agent decides how to recover.
	if *interactive {
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		seq := 0
		for sc.Scan() {
			seq++
			st, err := parseDriveLine(sc.Text(), seq)
			if err != nil {
				ds.respond(err)
				continue
			}
			if st == nil {
				continue
			}
			if st.kind == "quit" {
				ds.respond(nil)
				break
			}
			if ds.stepMarkers && st.kind != "snapshot" {
				ds.marker(fmt.Sprintf("cmd %d: %s", seq, st.raw))
			}
			ds.respond(ds.applyStep(st))
		}
	}

	exitCh := make(chan error, 1)
	go func() { exitCh <- processCmd.Wait() }()

	waitDur := time.Duration(*timeoutSec) * time.Second
	if *interactive {
		// The agent has ended the session; give the child a short grace
		// period to exit on its own, then tear it down.
		waitDur = 5 * time.Second
	}

	failed := false
	select {
	case err := <-exitCh:
		<-done
		if err != nil {
			fmt.Fprintf(os.Stderr, "trec drive: process exited: %v\n", err)
			failed = !*interactive
		} else {
			fmt.Fprintln(os.Stderr, "trec drive: process exited 0")
		}
	case <-time.After(waitDur):
		if *interactive {
			fmt.Fprintln(os.Stderr, "trec drive: ending interactive session — killing process")
		} else {
			fmt.Fprintln(os.Stderr, "trec drive: TIMEOUT — killing process")
			failed = true
		}
		_ = processCmd.Process.Kill()
		<-done
	}

	bw.Flush()
	fmt.Fprintf(os.Stderr, "trec drive: recorded to %s — replay with: trec play %s\n", *outputFile, *outputFile)

	if failed {
		os.Exit(1)
	}
}
