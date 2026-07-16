package main

import (
	"bytes"
	"encoding/json"
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
	cmd := &cobra.Command{
		Use:   "transcript <file.cast>",
		Short: "Print an ANSI-stripped transcript",
		Long:  "Prints a clean, timestamped, ANSI-stripped transcript for reviewing a recording and placing markers.",
		Args:  cobra.ExactArgs(1),
		Run:   runTranscript,
	}
	cmd.Flags().String("format", "text", "output format (text, json, jsonl)")
	return cmd
}

func runTranscript(cmd *cobra.Command, files []string) {
	format, _ := cmd.Flags().GetString("format")
	if len(files) != 1 {
		cmd.Usage()
		os.Exit(1)
	}

	hdr, events, err := loadCastFile(files[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	output, err := generateTranscript(hdr, events, format)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Print(output)
}

func generateTranscript(hdr castHeader, events []castEvent, format string) (string, error) {
	format = strings.ToLower(format)
	if format != "text" && format != "json" && format != "jsonl" {
		return "", fmt.Errorf("unsupported format %q (choose from text, json, jsonl)", format)
	}

	type transcriptEvent struct {
		Time      float64 `json:"time"`
		Type      string  `json:"type"`
		Data      string  `json:"data"`
		CleanData string  `json:"clean_data,omitempty"`
	}

	var tevents []transcriptEvent
	for _, e := range events {
		te := transcriptEvent{
			Time: e.sec,
			Type: e.typ,
			Data: e.data,
		}
		if e.typ == "i" {
			te.CleanData = visualizeKeys(e.data)
		} else if e.typ == "o" {
			te.CleanData = stripANSI(e.data)
			if strings.TrimSpace(te.CleanData) == "" {
				continue
			}
		}
		tevents = append(tevents, te)
	}

	var buf bytes.Buffer
	switch format {
	case "text":
		if hdr.Title != "" {
			fmt.Fprintf(&buf, "# %s\n", hdr.Title)
		}
		if hdr.Command != "" {
			fmt.Fprintf(&buf, "$ %s\n", hdr.Command)
		}
		fmt.Fprintln(&buf)

		for _, te := range tevents {
			switch te.Type {
			case "i":
				fmt.Fprintf(&buf, "[%.2fs] » %s\n", te.Time, te.CleanData)
			case "m":
				fmt.Fprintf(&buf, "[%.2fs] ⚑ %s\n", te.Time, te.Data)
			case "o":
				fmt.Fprintf(&buf, "[%.2fs] %s\n", te.Time, te.CleanData)
			}
		}

	case "jsonl":
		for _, te := range tevents {
			b, _ := json.Marshal(te)
			fmt.Fprintln(&buf, string(b))
		}

	case "json":
		type jsonOutput struct {
			Title   string            `json:"title,omitempty"`
			Command string            `json:"command,omitempty"`
			Events  []transcriptEvent `json:"events"`
		}
		out := jsonOutput{
			Title:   hdr.Title,
			Command: hdr.Command,
			Events:  tevents,
		}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Fprintln(&buf, string(b))
	}
	return buf.String(), nil
}
