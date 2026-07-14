package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

// loadCastFile reads an asciicast v2 file and returns its header plus every
// event line, unfiltered. Callers decide which event types are relevant to
// them (playback cares about "o"/"i"/"m"; annotate must preserve everything).
func loadCastFile(path string) (castHeader, []castEvent, error) {
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

	var events []castEvent
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		sec, typ, data, err := parseCastLine(line)
		if err != nil {
			continue
		}
		events = append(events, castEvent{sec: sec, typ: typ, data: data})
	}
	if err := sc.Err(); err != nil {
		return hdr, nil, fmt.Errorf("read: %w", err)
	}
	return hdr, events, nil
}

// writeCastFile writes an asciicast v2 header followed by events, one per line.
func writeCastFile(path string, hdr castHeader, events []castEvent) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	bw := bufio.NewWriter(f)
	hdrJSON, err := json.Marshal(hdr)
	if err != nil {
		return err
	}
	if _, err := bw.Write(hdrJSON); err != nil {
		return err
	}
	bw.WriteByte('\n')
	for _, e := range events {
		b, err := json.Marshal([]any{e.sec, e.typ, e.data})
		if err != nil {
			return err
		}
		if _, err := bw.Write(b); err != nil {
			return err
		}
		bw.WriteByte('\n')
	}
	return bw.Flush()
}
