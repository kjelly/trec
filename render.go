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
	cmd.Flags().String("marker-regex", "", "only render markers whose label matches this regexp (implies --markers)")
	cmd.Flags().Int("marker-index", -1, "render one zero-based marker after filtering (implies --markers)")
	cmd.Flags().Bool("last-marker", false, "render only the last marker after filtering (implies --markers)")
	cmd.Flags().Float64("at", -1, "Stop rendering and print the screen at this timestamp (seconds)")
	cmd.Flags().String("output-format", "", "Output format (e.g. jsonl)")
	cmd.Flags().Bool("tolerant", false, "skip invalid events with a warning instead of failing")
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
	markerPattern, _ := cmd.Flags().GetString("marker-regex")
	markerIndex, _ := cmd.Flags().GetInt("marker-index")
	lastMarker, _ := cmd.Flags().GetBool("last-marker")
	atTime, _ := cmd.Flags().GetFloat64("at")
	apiFormat, _ := cmd.Flags().GetString("output-format")
	tolerant, _ := cmd.Flags().GetBool("tolerant")
	if apiFormat != "" && apiFormat != "jsonl" {
		return fmt.Errorf("invalid --output-format %q; must be \"\" or \"jsonl\"", apiFormat)
	}
	jsonFormat := apiFormat == "jsonl"
	if markerIndex < -1 {
		return fmt.Errorf("--marker-index must be non-negative")
	}
	if markerIndex >= 0 && lastMarker {
		return fmt.Errorf("cannot specify both --marker-index and --last-marker")
	}
	if markerPattern != "" || markerIndex >= 0 || lastMarker {
		markersOnly = true
	}

	hdr, events, err := loadCastFileWithOptions(args[0], loadCastOptions{Tolerant: tolerant})
	if err != nil {
		return fmt.Errorf("trec render: %w", err)
	}
	if err := validateRenderSize(hdr.Width, hdr.Height); err != nil {
		return fmt.Errorf("trec render: %w", err)
	}
	markers, err := findMarkers(events, markerPattern, 0, -1)
	if err != nil {
		return fmt.Errorf("trec render: %w", err)
	}
	if markerIndex >= len(markers) {
		return fmt.Errorf("trec render: --marker-index %d is out of range (matched markers: %d)", markerIndex, len(markers))
	}
	if lastMarker && len(markers) == 0 {
		return fmt.Errorf("trec render: --last-marker specified but no matching markers were found")
	}
	selectedMarkers := make(map[int]markerRef, len(markers))
	if markerIndex >= 0 {
		selectedMarkers[markers[markerIndex].eventIndex] = markers[markerIndex]
	} else if lastMarker {
		last := markers[len(markers)-1]
		selectedMarkers[last.eventIndex] = last
	} else {
		for _, marker := range markers {
			selectedMarkers[marker.eventIndex] = marker
		}
	}

	vt := vt10x.New(vt10x.WithSize(hdr.Width, hdr.Height))
	for eventIndex, e := range events {
		if atTime >= 0 && e.sec > atTime {
			break
		}
		if err := applyRenderEvent(vt, e); err != nil {
			return fmt.Errorf("trec render: apply event at %.2fs: %w", e.sec, err)
		}
		if marker, ok := selectedMarkers[eventIndex]; ok && markersOnly {
			if jsonFormat {
				printScreenJSON(vt, marker.Time, marker.Label)
			} else {
				fmt.Printf("--- MARKER %d: %s [%.2fs] ---\n", marker.Index, marker.Label, marker.Time)
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
