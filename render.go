package main

import (
	"fmt"
	"strings"

	"github.com/hinshun/vt10x"
	"github.com/spf13/cobra"
)

func newRenderCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "render <file.cast>",
		Short: "Render a cast file to terminal screens",
		Long:  "Parses a recording using a VT100 emulator and prints the emulated screen state. By default it prints the final screen. Useful for AI agents reading TUI states.",
		Args:  cobra.ExactArgs(1),
		RunE:  runRender,
	}
	cmd.Flags().Bool("markers", false, "Print the screen state at every marker event")
	cmd.Flags().Float64("at", -1, "Stop rendering and print the screen at this timestamp (seconds)")
	return cmd
}

const (
	maxRenderDimension = 10000
	maxRenderCells     = 1 << 20
)

func validateRenderSize(width, height int) error {
	if width <= 0 || height <= 0 {
		return fmt.Errorf("invalid cast terminal size %dx%d", width, height)
	}
	if width > maxRenderDimension || height > maxRenderDimension || width > maxRenderCells/height {
		return fmt.Errorf("cast terminal size %dx%d is too large", width, height)
	}
	return nil
}

func runRender(cmd *cobra.Command, args []string) error {
	markersOnly, _ := cmd.Flags().GetBool("markers")
	atTime, _ := cmd.Flags().GetFloat64("at")

	hdr, events, err := loadCastFile(args[0])
	if err != nil {
		return fmt.Errorf("trec render: %w", err)
	}
	if err := validateRenderSize(hdr.Width, hdr.Height); err != nil {
		return fmt.Errorf("trec render: %w", err)
	}

	vt := vt10x.New(vt10x.WithSize(hdr.Width, hdr.Height))
	for _, e := range events {
		if atTime >= 0 && e.sec > atTime {
			break
		}
		if e.typ == "o" {
			if _, err := vt.Write([]byte(e.data)); err != nil {
				return fmt.Errorf("trec render: apply output at %.2fs: %w", e.sec, err)
			}
		} else if e.typ == "m" && markersOnly {
			fmt.Printf("--- MARKER: %s [%.2fs] ---\n", e.data, e.sec)
			printScreen(vt)
			fmt.Println()
		}
	}

	if !markersOnly {
		printScreen(vt)
	}
	return nil
}

func printScreen(vt vt10x.Terminal) {
	lines := strings.Split(vt.String(), "\n")
	// Trim trailing blank lines
	last := len(lines) - 1
	for last >= 0 && strings.TrimSpace(lines[last]) == "" {
		last--
	}
	for i := 0; i <= last; i++ {
		fmt.Println(strings.TrimRight(lines[i], " "))
	}
}
