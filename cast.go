package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
)

type loadCastOptions struct {
	Tolerant bool
}

// loadCastFile reads an asciicast v2 file and returns its header plus every
// event line, unfiltered. Callers decide which event types are relevant to
// them (playback cares about "o"/"i"/"m"; annotate must preserve everything).
func loadCastFile(path string) (castHeader, []castEvent, error) {
	return loadCastFileWithOptions(path, loadCastOptions{})
}

func loadCastFileWithOptions(path string, opts loadCastOptions) (castHeader, []castEvent, error) {
	f, err := os.Open(path)
	if err != nil {
		return castHeader{}, nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)

	if !sc.Scan() {
		return castHeader{}, nil, fmt.Errorf("empty file")
	}
	var hdr castHeader
	if err := json.Unmarshal(sc.Bytes(), &hdr); err != nil {
		return castHeader{}, nil, fmt.Errorf("invalid header: %w", err)
	}
	if err := validateCastHeader(hdr); err != nil {
		return castHeader{}, nil, fmt.Errorf("invalid header: %w", err)
	}

	var events []castEvent
	lineNo := 1
	lastTime := 0.0
	for sc.Scan() {
		lineNo++
		line := sc.Bytes()
		if len(line) == 0 {
			if opts.Tolerant {
				fmt.Fprintf(os.Stderr, "warning: invalid event at line %d: empty line\n", lineNo)
				continue
			}
			return hdr, nil, fmt.Errorf("invalid event at line %d: empty line", lineNo)
		}
		event, err := parseCastEvent(line)
		if err != nil {
			if opts.Tolerant {
				fmt.Fprintf(os.Stderr, "warning: invalid event at line %d: %v\n", lineNo, err)
				continue
			}
			return hdr, nil, fmt.Errorf("invalid event at line %d: %w", lineNo, err)
		}
		if event.sec < lastTime {
			if opts.Tolerant {
				fmt.Fprintf(os.Stderr, "warning: invalid event at line %d: timestamp %.9f is earlier than %.9f\n", lineNo, event.sec, lastTime)
				continue
			}
			return hdr, nil, fmt.Errorf("invalid event at line %d: timestamp %.9f is earlier than %.9f", lineNo, event.sec, lastTime)
		}
		lastTime = event.sec
		events = append(events, event)
	}
	if err := sc.Err(); err != nil {
		return hdr, nil, fmt.Errorf("read: %w", err)
	}
	return hdr, events, nil
}

func validateCastHeader(hdr castHeader) error {
	if hdr.Version != 2 {
		return fmt.Errorf("version must be 2, got %d", hdr.Version)
	}
	if hdr.Width <= 0 || hdr.Height <= 0 {
		return fmt.Errorf("terminal size must be positive, got %dx%d", hdr.Width, hdr.Height)
	}
	return nil
}

func parseCastEvent(line []byte) (castEvent, error) {
	var raw []json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return castEvent{}, fmt.Errorf("invalid JSON: %w", err)
	}
	if len(raw) != 3 {
		return castEvent{}, fmt.Errorf("event must contain exactly 3 fields")
	}

	var event castEvent
	if err := json.Unmarshal(raw[0], &event.sec); err != nil || math.IsNaN(event.sec) || math.IsInf(event.sec, 0) || event.sec < 0 {
		return castEvent{}, fmt.Errorf("event time must be a non-negative number")
	}
	if err := json.Unmarshal(raw[1], &event.typ); err != nil || event.typ == "" {
		return castEvent{}, fmt.Errorf("event type must be a non-empty string")
	}
	event.rawData = append(json.RawMessage(nil), raw[2]...)

	if event.typ != "o" && event.typ != "i" && event.typ != "m" && event.typ != "r" {
		fmt.Fprintf(os.Stderr, "warning: unknown event type %q\n", event.typ)
	}

	// Current asciicast v2 event types all use string payloads. Unknown events
	// remain pass-through compatible: their original JSON is preserved for
	// annotate/write operations even if this version of trec does not interpret it.
	if err := json.Unmarshal(raw[2], &event.data); err != nil {
		if event.typ == "o" || event.typ == "i" || event.typ == "m" || event.typ == "r" {
			return castEvent{}, fmt.Errorf("event data for %q must be a string", event.typ)
		}
		event.data = string(raw[2])
	}
	if event.typ == "r" {
		if _, _, err := parseResizeData(event.data); err != nil {
			return castEvent{}, fmt.Errorf("invalid resize event: %w", err)
		}
	}
	return event, nil
}

func parseResizeData(data string) (cols, rows int, err error) {
	colsText, rowsText, ok := strings.Cut(data, "x")
	if !ok || colsText == "" || rowsText == "" || strings.Contains(rowsText, "x") {
		return 0, 0, fmt.Errorf("want COLSxROWS, got %q", data)
	}
	cols, err = strconv.Atoi(colsText)
	if err != nil || cols <= 0 {
		return 0, 0, fmt.Errorf("invalid columns in %q", data)
	}
	rows, err = strconv.Atoi(rowsText)
	if err != nil || rows <= 0 {
		return 0, 0, fmt.Errorf("invalid rows in %q", data)
	}
	return cols, rows, nil
}

// writeCastFile writes an asciicast v2 header followed by events, one per line.
func writeCastFile(path string, hdr castHeader, events []castEvent) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	bw := bufio.NewWriter(f)
	data, err := marshalCast(hdr, events)
	if err != nil {
		return err
	}
	_, err = bw.Write(data)
	if err != nil {
		return err
	}
	return bw.Flush()
}

func marshalCast(hdr castHeader, events []castEvent) ([]byte, error) {
	hdrJSON, err := json.Marshal(hdr)
	if err != nil {
		return nil, err
	}
	data := make([]byte, 0, len(hdrJSON)+len(events)*32)
	data = append(data, hdrJSON...)
	data = append(data, '\n')
	for _, e := range events {
		payload := any(e.data)
		if e.rawData != nil {
			payload = e.rawData
		}
		b, err := json.Marshal([]any{e.sec, e.typ, payload})
		if err != nil {
			return nil, err
		}
		data = append(data, b...)
		data = append(data, '\n')
	}
	return data, nil
}
