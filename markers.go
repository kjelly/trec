package main

import (
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/spf13/cobra"
)

// markerRef keeps the original event position so render can select a marker
// without relying on a label or timestamp being unique.
type markerRef struct {
	Index      int     `json:"index"`
	Time       float64 `json:"time"`
	Label      string  `json:"label"`
	eventIndex int
}

func findMarkers(events []castEvent, pattern string, from, to float64) ([]markerRef, error) {
	var re *regexp.Regexp
	var err error
	if pattern != "" {
		re, err = regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid marker regexp %q: %w", pattern, err)
		}
	}

	markers := make([]markerRef, 0)
	for eventIndex, event := range events {
		if event.typ != "m" || event.sec < from || (to >= 0 && event.sec > to) {
			continue
		}
		if re != nil && !re.MatchString(event.data) {
			continue
		}
		markers = append(markers, markerRef{
			Index:      len(markers),
			Time:       event.sec,
			Label:      event.data,
			eventIndex: eventIndex,
		})
	}
	return markers, nil
}

func newMarkersCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "markers <file.cast>",
		Short: "List and query recording markers",
		Long:  "Lists marker events. Use --regex and time bounds to find lifecycle markers such as STEP_FAILED.",
		Args:  cobra.ExactArgs(1),
		RunE:  runMarkers,
	}
	cmd.Flags().String("regex", "", "only include marker labels matching this regexp")
	cmd.Flags().Float64("from", 0, "only include markers at or after this time in seconds")
	cmd.Flags().Float64("to", -1, "only include markers at or before this time in seconds")
	cmd.Flags().String("output-format", "", "output format: text, json, or jsonl")
	return cmd
}

func runMarkers(cmd *cobra.Command, args []string) error {
	pattern, _ := cmd.Flags().GetString("regex")
	from, _ := cmd.Flags().GetFloat64("from")
	to, _ := cmd.Flags().GetFloat64("to")
	format, _ := cmd.Flags().GetString("output-format")
	if from < 0 {
		return fmt.Errorf("--from must be non-negative")
	}
	if to >= 0 && to < from {
		return fmt.Errorf("--to must be greater than or equal to --from")
	}
	if format == "" {
		format = "text"
	}
	if format != "text" && format != "json" && format != "jsonl" {
		return fmt.Errorf("invalid --output-format %q; must be text, json, or jsonl", format)
	}

	_, events, err := loadCastFile(args[0])
	if err != nil {
		return fmt.Errorf("trec markers: %w", err)
	}
	markers, err := findMarkers(events, pattern, from, to)
	if err != nil {
		return fmt.Errorf("trec markers: %w", err)
	}

	switch format {
	case "text":
		for _, marker := range markers {
			fmt.Printf("%d\t%.3f\t%s\n", marker.Index, marker.Time, marker.Label)
		}
	case "json":
		data, err := json.Marshal(markers)
		if err != nil {
			return fmt.Errorf("encode markers: %w", err)
		}
		fmt.Println(string(data))
	case "jsonl":
		for _, marker := range markers {
			data, err := json.Marshal(marker)
			if err != nil {
				return fmt.Errorf("encode marker: %w", err)
			}
			fmt.Println(string(data))
		}
	}
	return nil
}
