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

// ansiStripper keeps escape-sequence state across output chunks. PTY reads do
// not align with terminal control sequences, so a regexp per event can expose
// fragments such as "31m" when CSI \x1b[31m is split across reads.
//
// ansiRe remains shared with redact.go, whose stream normalizer has different
// responsibilities from transcript rendering.
var ansiRe = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[@-Z\\-_])`)

type ansiStripper struct {
	pendingCR bool
	escapeBuf []byte
}

func (s *ansiStripper) Write(text string) string {
	b := append(append([]byte(nil), s.escapeBuf...), text...)
	s.escapeBuf = nil
	var out strings.Builder
	i := 0
	if s.pendingCR {
		if len(b) > 0 && b[0] == '\n' {
			i++
		}
		out.WriteByte('\n')
		s.pendingCR = false
	}
	for i < len(b) {
		if b[i] != 0x1b {
			s.writeVisible(&out, b, &i)
			continue
		}
		if i+1 >= len(b) {
			s.escapeBuf = append(s.escapeBuf, b[i:]...)
			break
		}
		switch b[i+1] {
		case '[': // CSI, through a final byte in 0x40..0x7e.
			j := i + 2
			for j < len(b) && (b[j] < 0x40 || b[j] > 0x7e) {
				j++
			}
			if j == len(b) {
				s.escapeBuf = append(s.escapeBuf, b[i:]...)
				i = len(b)
			} else {
				i = j + 1
			}
		case ']': // OSC, terminated by BEL or ST (ESC \\).
			j := i + 2
			terminated := false
			for j < len(b) {
				if b[j] == 0x07 {
					j++
					terminated = true
					break
				}
				if b[j] == 0x1b && j+1 < len(b) && b[j+1] == '\\' {
					j += 2
					terminated = true
					break
				}
				j++
			}
			if !terminated {
				s.escapeBuf = append(s.escapeBuf, b[i:]...)
				i = len(b)
			} else {
				i = j
			}
		default: // Two-byte escape sequence.
			i += 2
		}
	}
	return out.String()
}

func (s *ansiStripper) writeVisible(out *strings.Builder, b []byte, i *int) {
	c := b[*i]
	switch c {
	case '\r':
		if *i+1 == len(b) {
			s.pendingCR = true
			(*i)++
			return
		}
		out.WriteByte('\n')
		if b[*i+1] == '\n' {
			*i += 2
			return
		}
	case '\n', '\t':
		out.WriteByte(c)
	default:
		if c >= 0x20 && c != 0x7f {
			out.WriteByte(c)
		}
	}
	(*i)++
}

func (s *ansiStripper) Finish() string {
	if !s.pendingCR {
		return ""
	}
	s.pendingCR = false
	return "\n"
}

// stripANSI removes terminal escape sequences and control characters from one
// complete string. Streaming callers should retain an ansiStripper instead.
func stripANSI(s string) string {
	stripper := &ansiStripper{}
	return stripper.Write(s) + stripper.Finish()
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
	cmd.Flags().Bool("tolerant", false, "skip invalid events with a warning instead of failing")
	return cmd
}

func runTranscript(cmd *cobra.Command, files []string) {
	format, _ := cmd.Flags().GetString("format")
	tolerant, _ := cmd.Flags().GetBool("tolerant")
	if len(files) != 1 {
		cmd.Usage()
		os.Exit(1)
	}

	hdr, events, err := loadCastFileWithOptions(files[0], loadCastOptions{Tolerant: tolerant})
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
		EndTime   float64 `json:"end_time,omitempty"`
		Type      string  `json:"type"`
		Data      string  `json:"data"`
		CleanData string  `json:"clean_data,omitempty"`
	}

	var tevents []transcriptEvent
	stripper := &ansiStripper{}
	var output *transcriptEvent
	flushOutput := func() {
		if output == nil {
			return
		}
		if strings.TrimSpace(output.CleanData) != "" {
			tevents = append(tevents, *output)
		}
		output = nil
	}
	for _, e := range events {
		if e.typ == "o" {
			clean := stripper.Write(e.data)
			// Preserve the command's existing behavior of omitting chunks that
			// are solely whitespace while still joining visible text split by PTY
			// read boundaries.
			if strings.TrimSpace(clean) == "" {
				continue
			}
			if output == nil {
				output = &transcriptEvent{Time: e.sec, Type: e.typ}
			}
			output.EndTime = e.sec
			output.Data += e.data
			output.CleanData += clean
			continue
		}
		flushOutput()
		te := transcriptEvent{
			Time: e.sec,
			Type: e.typ,
			Data: e.data,
		}
		if e.typ == "i" {
			te.CleanData = visualizeKeys(e.data)
		}
		tevents = append(tevents, te)
	}
	if output != nil {
		output.CleanData += stripper.Finish()
	}
	flushOutput()

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
