package main

import (
	"encoding/json"
	"fmt"

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
	cmd.Flags().String("output-format", "", "Output format (e.g. jsonl)")
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
	apiFormat, _ := cmd.Flags().GetString("output-format")
	if apiFormat != "" && apiFormat != "jsonl" {
		return fmt.Errorf("invalid --output-format %q; must be \"\" or \"jsonl\"", apiFormat)
	}
	jsonFormat := apiFormat == "jsonl"

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
		if err := applyRenderEvent(vt, e); err != nil {
			return fmt.Errorf("trec render: apply event at %.2fs: %w", e.sec, err)
		}
		if e.typ == "m" && markersOnly {
			if jsonFormat {
				printScreenJSON(vt, e.sec, e.data)
			} else {
				fmt.Printf("--- MARKER: %s [%.2fs] ---\n", e.data, e.sec)
				printScreen(vt)
				fmt.Println()
			}
		}
	}

	if !markersOnly {
		if jsonFormat {
			// Find the last event time for timestamp
			t := 0.0
			if len(events) > 0 {
				t = events[len(events)-1].sec
				if atTime >= 0 && t > atTime {
					t = atTime
				}
			}
			printScreenJSON(vt, t, "")
		} else {
			printScreen(vt)
		}
	}
	return nil
}

func applyRenderEvent(vt vt10x.Terminal, e castEvent) error {
	switch e.typ {
	case "o":
		if _, err := vt.Write([]byte(e.data)); err != nil {
			return fmt.Errorf("write output: %w", err)
		}
	case "r":
		cols, rows, err := parseResizeData(e.data)
		if err != nil {
			return fmt.Errorf("resize: %w", err)
		}
		if err := validateRenderSize(cols, rows); err != nil {
			return err
		}
		vt.Resize(cols, rows)
	}
	return nil
}

func printScreen(vt vt10x.Terminal) {
	lines := normalizeScreen(vt.String())
	for _, l := range lines {
		fmt.Println(l)
	}
}

func printScreenJSON(vt vt10x.Terminal, timestamp float64, marker string) {
	lines := normalizeScreen(vt.String())
	cursor := vt.Cursor()
	cols, rows := vt.Size()
	res := struct {
		Timestamp float64 `json:"timestamp"`
		Marker    string  `json:"marker,omitempty"`
		Cursor    struct {
			Row int `json:"row"`
			Col int `json:"col"`
		} `json:"cursor"`
		Size struct {
			Rows int `json:"rows"`
			Cols int `json:"cols"`
		} `json:"size"`
		Screen []string `json:"screen"`
	}{
		Timestamp: timestamp,
		Marker:    marker,
		Cursor: struct {
			Row int `json:"row"`
			Col int `json:"col"`
		}{Row: cursor.Y, Col: cursor.X},
		Size: struct {
			Rows int `json:"rows"`
			Cols int `json:"cols"`
		}{Rows: rows, Cols: cols},
		Screen: lines,
	}
	out, _ := json.Marshal(res)
	fmt.Println(string(out))
}
