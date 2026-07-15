package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/spf13/pflag"
)

// driveStep is one instruction from a keystroke script, in the same format
// pilot's ptydrive scaffolding used to drive promptui/bubbletea wizards.
type driveStep struct {
	kind string // text, enter, down, up, space, tab, ctrlc, backspace, wait
	text string
	n    int
}

func loadDriveScript(path string) ([]driveStep, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var steps []driveStep
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		fields := strings.SplitN(trimmed, " ", 2)
		op := strings.ToUpper(fields[0])
		arg := ""
		if len(fields) > 1 {
			arg = fields[1]
		}
		switch op {
		case "TEXT":
			steps = append(steps, driveStep{kind: "text", text: arg})
		case "ENTER":
			steps = append(steps, driveStep{kind: "enter"})
		case "DOWN":
			steps = append(steps, driveStep{kind: "down", n: atoiOr1(arg)})
		case "UP":
			steps = append(steps, driveStep{kind: "up", n: atoiOr1(arg)})
		case "SPACE":
			steps = append(steps, driveStep{kind: "space"})
		case "TAB":
			steps = append(steps, driveStep{kind: "tab"})
		case "CTRLC":
			steps = append(steps, driveStep{kind: "ctrlc"})
		case "BACKSPACE":
			steps = append(steps, driveStep{kind: "backspace", n: atoiOr1(arg)})
		case "WAIT":
			steps = append(steps, driveStep{kind: "wait", n: atoiOr1(arg)})
		default:
			return nil, fmt.Errorf("unknown op %q", op)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return steps, nil
}

func atoiOr1(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 1
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 1
	}
	return n
}

func max1(n int) int {
	if n <= 0 {
		return 1
	}
	return n
}

// applyDriveStep sends one step's bytes to ptmx, pacing keystrokes like a
// human typing, and records each write as a timed "i" event via writeEvent
// so the resulting recording is a normal asciicast v2 file — playable with
// 'trec play' and readable with 'trec transcript' exactly like a
// human-driven session. visualizeKeys() already renders raw control bytes
// (arrow keys, Enter, Ctrl-C, DEL) as symbols, so scripted "i" events show
// up the same way a real keypress would.
func applyDriveStep(s driveStep, ptmx io.Writer, bw *bufio.Writer, mu *sync.Mutex, start time.Time, keyDelay, settleDelay time.Duration) error {
	write := func(b []byte) error {
		if _, err := ptmx.Write(b); err != nil {
			return err
		}
		writeEvent(bw, mu, time.Since(start).Seconds(), "i", string(b))
		mu.Lock()
		bw.Flush()
		mu.Unlock()
		return nil
	}

	switch s.kind {
	case "text":
		for _, r := range s.text {
			if err := write([]byte(string(r))); err != nil {
				return err
			}
			time.Sleep(keyDelay)
		}
	case "enter":
		if err := write([]byte("\r")); err != nil {
			return err
		}
		// Enter always causes a prompt TRANSITION (submits the current
		// promptui.Select/Prompt or bubbletea model and renders the
		// next one) — that render + the next prompt's readline setup
		// is where a following key send can race ahead of the process
		// actually being ready to consume it. Steady same-prompt
		// navigation (repeated arrow presses) doesn't need this; only
		// the transition does.
		time.Sleep(settleDelay)
	case "down":
		for i := 0; i < max1(s.n); i++ {
			if err := write([]byte("\x1b[B")); err != nil {
				return err
			}
			time.Sleep(keyDelay)
		}
	case "up":
		for i := 0; i < max1(s.n); i++ {
			if err := write([]byte("\x1b[A")); err != nil {
				return err
			}
			time.Sleep(keyDelay)
		}
	case "space":
		if err := write([]byte(" ")); err != nil {
			return err
		}
		time.Sleep(keyDelay)
	case "tab":
		if err := write([]byte("\t")); err != nil {
			return err
		}
		time.Sleep(keyDelay)
	case "ctrlc":
		if err := write([]byte{0x03}); err != nil {
			return err
		}
		time.Sleep(keyDelay)
	case "backspace":
		// promptui's readline (chzyer/readline) uses DEL (127) as its
		// backspace char (CharBackspace) — needed to clear a
		// promptui.Prompt's pre-filled Default text (AllowEdit:true
		// puts the cursor at the end of it, so plain typing appends
		// rather than replaces).
		for i := 0; i < max1(s.n); i++ {
			if err := write([]byte{127}); err != nil {
				return err
			}
			time.Sleep(keyDelay)
		}
	case "wait":
		time.Sleep(time.Duration(s.n) * time.Millisecond)
	}
	return nil
}

func runDrive(args []string) {
	flags := pflag.NewFlagSet("drive", pflag.ExitOnError)
	scriptPath := flags.String("script", "", "path to keystroke script (required)")
	outputFile := flags.StringP("output", "o", "", "output file (default: record_TIMESTAMP.cast)")
	width := flags.IntP("width", "W", 220, "terminal width")
	height := flags.IntP("height", "H", 50, "terminal height")
	title := flags.String("title", "", "session title stored in the cast file")
	timeoutSec := flags.Int("timeout", 120, "overall timeout in seconds")
	keyDelayMs := flags.Int("key-delay", 300, "milliseconds between keystrokes")
	settleDelayMs := flags.Int("settle-delay", 700, "milliseconds to wait after ENTER for a prompt transition to settle")
	flags.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: trec drive -script steps.txt [options] -- <command> [args...]

Drives an interactive TUI (promptui/bubbletea wizards, etc.) under a PTY by
replaying a keystroke script, the way a human at a keyboard would — instead
of bypassing the TUI. Every keystroke sent is recorded as a timed "i" event
alongside the program's own "o" output, so the result is a normal asciicast
v2 recording: play it back with 'trec play', read it with 'trec transcript',
or mark it up with 'trec annotate', exactly like a human-recorded session.

Script format, one instruction per line:
  # comment
  TEXT literal text typed character-by-character (no trailing Enter)
  ENTER
  DOWN [n]
  UP [n]
  SPACE
  TAB
  CTRLC
  BACKSPACE [n]   send DEL (127) — clears a pre-filled Default value
  WAIT ms

Options:`)
		flags.PrintDefaults()
	}
	flags.Parse(args)

	rest := flags.Args()
	if *scriptPath == "" || len(rest) == 0 {
		flags.Usage()
		os.Exit(2)
	}

	steps, err := loadDriveScript(*scriptPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "trec drive: load script: %v\n", err)
		os.Exit(2)
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

	cmd := exec.Command(rest[0], rest[1:]...)
	// CI=1 + a fixed xterm term type keep bubbletea/promptui rendering
	// deterministic under a driven, non-interactive PTY (no real human
	// terminal behind it).
	cmd.Env = append(os.Environ(), "TERM=xterm-256color", "CI=1")

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(*height), Cols: uint16(*width)})
	if err != nil {
		fmt.Fprintf(os.Stderr, "trec drive: pty start: %v\n", err)
		os.Exit(1)
	}
	defer ptmx.Close()

	startTime := time.Now()
	var bwMu sync.Mutex

	done := make(chan struct{})
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				elapsed := time.Since(startTime).Seconds()
				os.Stdout.Write(buf[:n])
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

	keyDelay := time.Duration(*keyDelayMs) * time.Millisecond
	settleDelay := time.Duration(*settleDelayMs) * time.Millisecond
	for _, st := range steps {
		if err := applyDriveStep(st, ptmx, bw, &bwMu, startTime, keyDelay, settleDelay); err != nil {
			fmt.Fprintf(os.Stderr, "trec drive: step %+v: %v\n", st, err)
			break
		}
	}

	exitCh := make(chan error, 1)
	go func() { exitCh <- cmd.Wait() }()

	failed := false
	select {
	case err := <-exitCh:
		<-done
		if err != nil {
			fmt.Fprintf(os.Stderr, "trec drive: process exited: %v\n", err)
			failed = true
		} else {
			fmt.Fprintln(os.Stderr, "trec drive: process exited 0")
		}
	case <-time.After(time.Duration(*timeoutSec) * time.Second):
		fmt.Fprintln(os.Stderr, "trec drive: TIMEOUT — killing process")
		_ = cmd.Process.Kill()
		<-done
		failed = true
	}

	bw.Flush()
	fmt.Fprintf(os.Stderr, "trec drive: recorded to %s — replay with: trec play %s\n", *outputFile, *outputFile)

	if failed {
		os.Exit(1)
	}
}
