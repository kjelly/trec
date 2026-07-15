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
	"github.com/spf13/cobra"
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

func main() {
	if err := newRootCommand().Execute(); err != nil {
		os.Exit(1)
	}
}

func newRootCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "trec [record-options] [-- command [args...]]",
		Short:         "Record, play, and inspect terminal sessions",
		Long:          "trec records terminal sessions in asciicast v2 format, then plays, annotates, exports, and serves them.",
		Args:          cobra.ArbitraryArgs,
		Run:           runRecord,
		SilenceErrors: true,
	}
	cmd.Flags().StringP("output", "o", "", "output file (default: record_TIMESTAMP.cast)")
	cmd.Flags().IntP("width", "W", 0, "terminal width (default: current terminal width)")
	cmd.Flags().IntP("height", "H", 0, "terminal height (default: current terminal height)")
	cmd.Flags().String("title", "", "session title stored in the cast file")
	cmd.AddCommand(newDriveCommand(), newPlayCommand(), newHTMLCommand(), newServeCommand(), newTranscriptCommand(), newAnnotateCommand())
	return cmd
}

func runRecord(cmd *cobra.Command, args []string) {
	outputFile, _ := cmd.Flags().GetString("output")
	width, _ := cmd.Flags().GetInt("width")
	height, _ := cmd.Flags().GetInt("height")
	title, _ := cmd.Flags().GetString("title")

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
	processCmd := exec.Command(args[0], args[1:]...)
	ptmx, err := pty.StartWithSize(processCmd, &pty.Winsize{
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
	processCmd.Wait()
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
