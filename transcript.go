package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/pflag"
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

func runTranscript(args []string) {
	flags := pflag.NewFlagSet("transcript", pflag.ExitOnError)
	flags.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: trec transcript <file.cast>")
		fmt.Fprintln(os.Stderr, "\nPrints a clean, timestamped, ANSI-stripped transcript of the recording")
		fmt.Fprintln(os.Stderr, "to stdout — meant to be read by an AI agent deciding where to place")
		fmt.Fprintln(os.Stderr, "markers (see 'trec annotate --import').")
	}
	flags.Parse(args)

	files := flags.Args()
	if len(files) != 1 {
		flags.Usage()
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
