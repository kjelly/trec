package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
)

// ansiRe matches CSI sequences (\x1b[...final byte), OSC sequences
// (\x1b]...BEL or ST), and other short two-byte escape sequences.
var ansiRe = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[@-Z\\-_])`)

// controlCharsRe matches C0 control characters other than \t and \n.
var controlCharsRe = regexp.MustCompile(`[\x00-\x08\x0b-\x1f\x7f]`)

// stripANSI removes terminal escape sequences and control characters,
// leaving the human-readable text an agent can read.
func stripANSI(s string) string {
	s = ansiRe.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = controlCharsRe.ReplaceAllString(s, "")
	return s
}

func newTranscriptCommand() *cobra.Command {
	return &cobra.Command{Use: "transcript <file.cast>", Short: "Print an ANSI-stripped transcript", Long: "Prints a clean, timestamped, ANSI-stripped transcript for reviewing a recording and placing markers.", Args: cobra.ExactArgs(1), Run: runTranscript}
}

func runTranscript(cmd *cobra.Command, files []string) {
	if len(files) != 1 {
		cmd.Usage()
		os.Exit(1)
	}

	hdr, events, err := loadCastFile(files[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if hdr.Title != "" {
		fmt.Printf("# %s\n", hdr.Title)
	}
	if hdr.Command != "" {
		fmt.Printf("$ %s\n", hdr.Command)
	}
	fmt.Println()

	for _, e := range events {
		switch e.typ {
		case "i":
			fmt.Printf("[%.2fs] » %s\n", e.sec, visualizeKeys(e.data))
		case "m":
			fmt.Printf("[%.2fs] ⚑ %s\n", e.sec, e.data)
		case "o":
			text := stripANSI(e.data)
			if strings.TrimSpace(text) == "" {
				continue
			}
			fmt.Printf("[%.2fs] %s\n", e.sec, text)
		}
	}
}
