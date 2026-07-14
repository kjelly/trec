package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"github.com/spf13/pflag"
)

// markerImport is one entry of the JSON array accepted by --import.
type markerImport struct {
	Time  float64 `json:"time"`
	Label string  `json:"label"`
}

func markerKey(sec float64, label string) string {
	return fmt.Sprintf("%.3f|%s", sec, label)
}

func runAnnotate(args []string) {
	flags := pflag.NewFlagSet("annotate", pflag.ExitOnError)
	importPath := flags.String("import", "", "JSON file with markers to add: [{\"time\":sec,\"label\":str}, ...]")
	inPlace := flags.Bool("in-place", false, "overwrite the input file instead of writing <file>.annotated.cast")
	output := flags.StringP("output", "o", "", "output file (default: <file>.annotated.cast, or the input file with --in-place)")
	flags.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: trec annotate <file.cast> --import markers.json [options]")
		fmt.Fprintln(os.Stderr, "\nMerges marker (\"m\") events into a recording, sorted by time, so they")
		fmt.Fprintln(os.Stderr, "show up during 'trec play' and can be jumped to with n/N.")
		fmt.Fprintln(os.Stderr, "\nOptions:")
		flags.PrintDefaults()
	}
	flags.Parse(args)

	files := flags.Args()
	if len(files) != 1 || *importPath == "" {
		flags.Usage()
		os.Exit(1)
	}

	hdr, events, err := loadCastFile(files[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	data, err := os.ReadFile(*importPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error reading %s: %v\n", *importPath, err)
		os.Exit(1)
	}
	var markers []markerImport
	if err := json.Unmarshal(data, &markers); err != nil {
		fmt.Fprintf(os.Stderr, "error parsing %s: %v\n", *importPath, err)
		os.Exit(1)
	}

	existing := make(map[string]bool)
	for _, e := range events {
		if e.typ == "m" {
			existing[markerKey(e.sec, e.data)] = true
		}
	}

	added := 0
	for _, m := range markers {
		key := markerKey(m.Time, m.Label)
		if existing[key] {
			continue
		}
		events = append(events, castEvent{sec: m.Time, typ: "m", data: m.Label})
		existing[key] = true
		added++
	}

	sort.SliceStable(events, func(i, j int) bool { return events[i].sec < events[j].sec })

	outPath := *output
	if outPath == "" {
		if *inPlace {
			outPath = files[0]
		} else {
			outPath = files[0] + ".annotated.cast"
		}
	}

	if err := writeCastFile(outPath, hdr, events); err != nil {
		fmt.Fprintf(os.Stderr, "error writing %s: %v\n", outPath, err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "Added %d marker(s) (%d already present); wrote %s\n",
		added, len(markers)-added, outPath)
}
