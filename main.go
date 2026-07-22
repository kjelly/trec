package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/hinshun/vt10x"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// castHeader is the first line of an asciicast v2 file.
type castHeader struct {
	Version      int               `json:"version"`
	Width        int               `json:"width"`
	Height       int               `json:"height"`
	Timestamp    int64             `json:"timestamp"`
	TrecVersion  string            `json:"trec_version,omitempty"`
	TrecBuild    buildMetadata     `json:"trec_build,omitempty"`
	Command      string            `json:"command,omitempty"`
	CommandLabel string            `json:"command_label,omitempty"`
	Title        string            `json:"title,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
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
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newRootCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "trec [record-options] [-- command [args...]]",
		Short:         "Record, play, and inspect terminal sessions",
		Long:          "trec records terminal sessions in asciicast v2 format, then plays, annotates, exports, and serves them.",
		Version:       currentBuildMetadata().DisplayVersion(),
		Args:          cobra.ArbitraryArgs,
		Run:           runRecord,
		SilenceErrors: true,
	}
	cmd.Flags().StringP("output", "o", "", "output file (default: record_TIMESTAMP.cast)")
	cmd.Flags().Bool("force", false, "replace an existing cast and its result sidecar")
	cmd.Flags().IntP("width", "W", 0, "terminal width (default: current terminal width)")
	cmd.Flags().IntP("height", "H", 0, "terminal height (default: current terminal height)")
	cmd.Flags().String("title", "", "session title stored in the cast file")
	cmd.PersistentFlags().StringArray("secret-env", nil, "environment variable whose value is redacted from the recording (repeatable)")
	cmd.PersistentFlags().StringArray("secret-file", nil, "NAME=path whose file content is redacted from the recording (repeatable)")
	cmd.PersistentFlags().Bool("record-command", false, "store the command in the cast header (redacted by --secret-env/--secret-file)")
	cmd.PersistentFlags().String("command-label", "", "safe label stored in the cast header instead of the full command")
	cmd.AddCommand(newDriveCommand(), newPlayCommand(), newHTMLCommand(), newServeCommand(), newTranscriptCommand(), newAnnotateCommand(), newMarkersCommand(), newMCPCommand(), newVersionCommand(), newRenderCommand(), newScanCommand(), newVerifyCommand())
	return cmd
}

func runRecord(cmd *cobra.Command, args []string) {
	outputFile, _ := cmd.Flags().GetString("output")
	width, _ := cmd.Flags().GetInt("width")
	height, _ := cmd.Flags().GetInt("height")
	title, _ := cmd.Flags().GetString("title")
	secretEnv, _ := cmd.Flags().GetStringArray("secret-env")
	secretFiles, _ := cmd.Flags().GetStringArray("secret-file")
	recordCommand, _ := cmd.Flags().GetBool("record-command")
	commandLabel, _ := cmd.Flags().GetString("command-label")
	force, _ := cmd.Flags().GetBool("force")
	redactor, err := newSecretRedactor(secretEnv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "trec: %v\n", err)
		os.Exit(2)
	}
	if err := addSecretFileSpecs(redactor, secretFiles); err != nil {
		fmt.Fprintf(os.Stderr, "trec: %v\n", err)
		os.Exit(2)
	}

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
	f, err := prepareRecordingOutput(outputFile, force)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create output file: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()

	bw := bufio.NewWriterSize(f, 256*1024)
	var bwMu sync.Mutex
	recorder := newRecordingWriter(bw, &bwMu, redactor)

	// Write asciicast v2 header.
	build := currentBuildMetadata()
	hdr := castHeader{
		Version:     2,
		Width:       width,
		Height:      height,
		Timestamp:   time.Now().Unix(),
		TrecVersion: build.DisplayVersion(),
		TrecBuild:   build,
		Title:       title,
		Env: map[string]string{
			"TERM":  getenv("TERM", "xterm-256color"),
			"SHELL": getenv("SHELL", "/bin/sh"),
		},
	}
	if recordCommand {
		hdr.Command = strings.Join(args, " ")
	}
	hdr.CommandLabel = commandLabel
	if err := recorder.writeHeader(hdr); err != nil {
		fmt.Fprintf(os.Stderr, "failed to write header: %v\n", err)
		os.Exit(1)
	}
	if err := recorder.flush(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to write header: %v\n", err)
		os.Exit(1)
	}
	started := time.Now()
	pending := newPendingSessionResult(started)
	pending.Mode = "record"
	pending.CommandLabel = commandLabel
	pending.Inputs = &sessionInputFingerprint{CWD: safeGetwd()}
	if err := writePendingSessionResult(outputFile, pending); err != nil {
		fmt.Fprintf(os.Stderr, "failed to write recording summary: %v\n", err)
		os.Exit(1)
	}

	// Start the command under a PTY.
	processCmd := exec.Command(args[0], args[1:]...)
	ptmx, err := pty.StartWithSize(processCmd, &pty.Winsize{
		Rows: uint16(height),
		Cols: uint16(width),
	})
	if err != nil {
		_ = f.Sync()
		_ = writeSessionResult(outputFile, sessionResult{
			SessionID:    pending.SessionID,
			StartedAt:    pending.StartedAt,
			Mode:         pending.Mode,
			CommandLabel: pending.CommandLabel,
			Status:       "failed",
			ExitCode:     -1,
			Error:        fmt.Sprintf("start command: %v", err),
			Termination:  &sessionTermination{Kind: "start_failure", Reason: err.Error()},
		})
		fmt.Fprintf(os.Stderr, "failed to start %q: %v\n", args[0], err)
		os.Exit(1)
	}

	startTime := started
	finalVT := vt10x.New(vt10x.WithSize(width, height))
	var finalVTMu sync.Mutex

	// Forward SIGWINCH to the PTY so the child sees resize events.
	sigWinch := make(chan os.Signal, 1)
	signal.Notify(sigWinch, syscall.SIGWINCH)
	winchDone := make(chan struct{})
	go func() {
		defer close(winchDone)
		for range sigWinch {
			if !fixedSize {
				if err := pty.InheritSize(os.Stdin, ptmx); err != nil {
					fmt.Fprintf(os.Stderr, "failed to resize PTY: %v\n", err)
					continue
				}
				rows, cols, err := pty.Getsize(ptmx)
				if err != nil {
					fmt.Fprintf(os.Stderr, "failed to read PTY size: %v\n", err)
					continue
				}
				if err := recorder.event(time.Since(startTime).Seconds(), "r", fmt.Sprintf("%dx%d", cols, rows)); err != nil {
					fmt.Fprintf(os.Stderr, "failed to record resize: %v\n", err)
					continue
				}
				finalVTMu.Lock()
				finalVT.Resize(cols, rows)
				finalVTMu.Unlock()
				if err := recorder.flush(); err != nil {
					fmt.Fprintf(os.Stderr, "failed to flush resize: %v\n", err)
				}
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
				finalVTMu.Lock()
				_, _ = finalVT.Write(buf[:n])
				finalVTMu.Unlock()
				recorder.output(elapsed, string(buf[:n]))
				recorder.flush()
			}
			if err != nil {
				break
			}
		}
	}()

	// Forward our stdin to the PTY and record keyboard/mouse input events.
	// The gate makes finalization deterministic: a read already blocked on
	// stdin may wake after the child exits, but it must not append an event
	// after the cast has been synced and its digest computed.
	inputStopped := make(chan struct{})
	var inputMu sync.Mutex
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				inputMu.Lock()
				select {
				case <-inputStopped:
					inputMu.Unlock()
					return
				default:
				}
				elapsed := time.Since(startTime).Seconds()
				ptmx.Write(buf[:n])
				recorder.event(elapsed, "i", string(buf[:n]))
				recorder.flush()
				inputMu.Unlock()
			}
			if err != nil {
				break
			}
		}
	}()

	// Give the child a chance to exit cleanly when trec itself is terminated.
	// This lets us flush the cast and write an aborted result instead of
	// leaving a permanent in_progress sidecar.
	terminationSignals := make(chan os.Signal, 1)
	signal.Notify(terminationSignals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	processDone := make(chan struct{})
	terminationDone := make(chan struct{})
	var terminationSignal os.Signal
	go func() {
		defer close(terminationDone)
		select {
		case sig := <-terminationSignals:
			terminationSignal = sig
			_ = processCmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-processDone:
			case <-time.After(2 * time.Second):
				_ = processCmd.Process.Kill()
			}
		case <-processDone:
		}
	}()

	// Block until the recorded program exits.
	processErr := processCmd.Wait()
	close(processDone)
	signal.Stop(terminationSignals)
	<-terminationDone
	ptmx.Close()
	wg.Wait()
	inputMu.Lock()
	close(inputStopped)
	inputMu.Unlock()
	signal.Stop(sigWinch)
	close(sigWinch)
	<-winchDone

	exitCode := 0
	if processCmd.ProcessState != nil {
		exitCode = processCmd.ProcessState.ExitCode()
	}
	status := "success"
	message := ""
	termination := &sessionTermination{Kind: "child_exit", Signal: processSignal(processErr)}
	if processErr != nil {
		status = "failed"
		message = processErr.Error()
	}
	if terminationSignal != nil {
		status = "aborted"
		message = "trec interrupted by " + terminationSignal.String()
		termination.Kind = "signal"
		termination.Reason = message
		termination.Signal = terminationSignal.String()
	}

	var recErr error
	if err := recorder.flushOutput(); err != nil {
		recErr = err
	}
	if err := recorder.event(time.Since(startTime).Seconds(), "m", fmt.Sprintf("SESSION_END status=%s exit_code=%d", status, exitCode)); err != nil && recErr == nil {
		recErr = err
	}
	if err := recorder.flush(); err != nil && recErr == nil {
		recErr = err
	}
	if err := recorder.getError(); err != nil && recErr == nil {
		recErr = err
	}
	if err := f.Sync(); err != nil && recErr == nil {
		recErr = fmt.Errorf("sync cast file: %w", err)
	}
	if recErr != nil {
		status = "recording_error"
		message = recErr.Error()
		termination.Kind = "recording_error"
		termination.Reason = message
	}
	finalVTMu.Lock()
	finalScreen := redactor.redactScreen(normalizeScreen(finalVT.String()))
	finalVTMu.Unlock()
	if err := writeSessionResult(outputFile, sessionResult{
		SessionID:       pending.SessionID,
		StartedAt:       pending.StartedAt,
		Mode:            pending.Mode,
		CommandLabel:    pending.CommandLabel,
		Status:          status,
		ExitCode:        exitCode,
		Error:           message,
		DurationSeconds: time.Since(startTime).Seconds(),
		FinalScreen:     finalScreen,
		Termination:     termination,
		Inputs:          pending.Inputs,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing recording summary: %v\n", err)
		if recErr == nil {
			recErr = err
		}
	}

	if isInteractive {
		term.Restore(int(os.Stdin.Fd()), oldState)
		if recErr != nil {
			fmt.Fprintf(os.Stderr, "\r\nError saving recording: %v\r\n", recErr)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "\r\nDone. Recording saved to %s\r\n", outputFile)
	} else {
		if recErr != nil {
			fmt.Fprintf(os.Stderr, "Error saving recording: %v\n", recErr)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "Done. Recording saved to %s\n", outputFile)
	}
	if processErr != nil {
		os.Exit(exitCode)
	}
}
