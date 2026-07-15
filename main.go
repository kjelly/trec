package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/spf13/pflag"
	"golang.org/x/term"
)

// castHeader is the first line of an asciicast v2 file.
type castHeader struct {
	Version   int               `json:"version"`
	Width     int               `json:"width"`
	Height    int               `json:"height"`
	Timestamp int64             `json:"timestamp"`
	Command   string            `json:"command,omitempty"`
	Title     string            `json:"title,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
}

func writeEvent(w io.Writer, mu *sync.Mutex, elapsed float64, eventType, data string) {
	b, _ := json.Marshal([]any{elapsed, eventType, data})
	mu.Lock()
	w.Write(b)
	w.Write([]byte("\n"))
	mu.Unlock()
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func promptCommand() []string {
	fmt.Fprint(os.Stderr, "Command to record: ")
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		return nil
	}
	line := strings.TrimSpace(scanner.Text())
	if line == "" {
		return nil
	}
	return []string{"sh", "-c", line}
}

func topUsage() {
	fmt.Fprintf(os.Stderr, `Usage:
  %s [record-options] [-- command [args...]]   Record a terminal session
  %s drive -script s.txt [options] -- cmd ...  Drive a TUI from a keystroke script and record it
  %s play [play-options] <file.cast>           Play back a recording
  %s html [html-options] <file.cast>           Generate a deployable HTML player
  %s serve [serve-options] [directory]          Serve .cast files in a web player
  %s transcript <file.cast>                    Print a clean, agent-readable transcript
  %s annotate <file.cast> --import notes.json  Add markers to a recording

Run '%s drive --help', '%s play --help', '%s html --help', '%s serve --help', '%s transcript --help', or
'%s annotate --help' for subcommand options.

Record options:
`, os.Args[0], os.Args[0], os.Args[0], os.Args[0], os.Args[0], os.Args[0], os.Args[0], os.Args[0], os.Args[0], os.Args[0], os.Args[0], os.Args[0], os.Args[0])
	pflag.PrintDefaults()
	fmt.Fprintln(os.Stderr, "\nOutput is asciicast v2 format (also playable with: asciinema play <file>).")
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "play":
			runPlay(os.Args[2:])
			return
		case "transcript":
			runTranscript(os.Args[2:])
			return
		case "annotate":
			runAnnotate(os.Args[2:])
			return
		case "html":
			runHTML(os.Args[2:])
			return
		case "serve":
			runServe(os.Args[2:])
			return
		case "drive":
			runDrive(os.Args[2:])
			return
		}
	}

	var (
		outputFile string
		width      int
		height     int
		title      string
	)

	pflag.StringVarP(&outputFile, "output", "o", "", "output file (default: record_TIMESTAMP.cast)")
	pflag.IntVarP(&width, "width", "W", 0, "terminal width (default: current terminal width)")
	pflag.IntVarP(&height, "height", "H", 0, "terminal height (default: current terminal height)")
	pflag.StringVar(&title, "title", "", "session title stored in the cast file")
	pflag.Usage = topUsage
	pflag.Parse()

	args := pflag.Args()

	// Resolve terminal size.
	curW, curH, err := term.GetSize(int(os.Stdin.Fd()))
	if err != nil {
		curW, curH = 120, 40
	}
	fixedSize := width != 0 || height != 0
	if width == 0 {
		width = curW
	}
	if height == 0 {
		height = curH
	}

	// Interactive command selection when none given on command line.
	if len(args) == 0 {
		args = promptCommand()
		if len(args) == 0 {
			fmt.Fprintln(os.Stderr, "No command provided.")
			os.Exit(1)
		}
	}

	// Prepare output file.
	if outputFile == "" {
		outputFile = fmt.Sprintf("record_%s.cast", time.Now().Format("20060102_150405"))
	}
	f, err := os.Create(outputFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create output file: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()

	bw := bufio.NewWriterSize(f, 256*1024)

	// Write asciicast v2 header.
	hdr := castHeader{
		Version:   2,
		Width:     width,
		Height:    height,
		Timestamp: time.Now().Unix(),
		Command:   strings.Join(args, " "),
		Title:     title,
		Env: map[string]string{
			"TERM":  getenv("TERM", "xterm-256color"),
			"SHELL": getenv("SHELL", "/bin/sh"),
		},
	}
	hdrJSON, _ := json.Marshal(hdr)
	fmt.Fprintln(bw, string(hdrJSON))

	// Start the command under a PTY.
	cmd := exec.Command(args[0], args[1:]...)
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{
		Rows: uint16(height),
		Cols: uint16(width),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to start %q: %v\n", args[0], err)
		os.Exit(1)
	}

	// Forward SIGWINCH to the PTY so the child sees resize events.
	sigWinch := make(chan os.Signal, 1)
	signal.Notify(sigWinch, syscall.SIGWINCH)
	go func() {
		for range sigWinch {
			if !fixedSize {
				pty.InheritSize(os.Stdin, ptmx)
			}
		}
	}()

	// Switch stdin to raw mode only if we are in an interactive terminal.
	isInteractive := term.IsTerminal(int(os.Stdin.Fd()))
	var oldState *term.State
	if isInteractive {
		var err error
		oldState, err = term.MakeRaw(int(os.Stdin.Fd()))
		if err != nil {
			ptmx.Close()
			fmt.Fprintf(os.Stderr, "failed to set raw mode: %v\n", err)
			os.Exit(1)
		}
		defer term.Restore(int(os.Stdin.Fd()), oldState)
		fmt.Fprintf(os.Stderr, "\r\nRecording to %s — exit the program to stop.\r\n\r\n", outputFile)
	} else {
		fmt.Fprintf(os.Stderr, "Recording to %s (non-interactive mode) — exit the program to stop.\n", outputFile)
	}

	startTime := time.Now()
	var bwMu sync.Mutex

	// Forward PTY output to our stdout and write timed events to the cast file.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
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
	}()

	// Forward our stdin to the PTY and record keyboard/mouse input events.
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				elapsed := time.Since(startTime).Seconds()
				ptmx.Write(buf[:n])
				writeEvent(bw, &bwMu, elapsed, "i", string(buf[:n]))
				bwMu.Lock()
				bw.Flush()
				bwMu.Unlock()
			}
			if err != nil {
				break
			}
		}
	}()

	// Block until the recorded program exits.
	cmd.Wait()
	ptmx.Close()
	wg.Wait()

	bw.Flush()
	signal.Stop(sigWinch)
	close(sigWinch)

	if isInteractive {
		term.Restore(int(os.Stdin.Fd()), oldState)
		fmt.Fprintf(os.Stderr, "\r\nDone. Recording saved to %s\r\n", outputFile)
	} else {
		fmt.Fprintf(os.Stderr, "Done. Recording saved to %s\n", outputFile)
	}
}
