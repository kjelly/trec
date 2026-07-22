package main

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestFindMarkersFiltersAndKeepsSelectedEventIdentity(t *testing.T) {
	events := []castEvent{
		{sec: 0.5, typ: "m", data: "STEP_START line 1"},
		{sec: 1, typ: "o", data: "ignored"},
		{sec: 1.5, typ: "m", data: "STEP_FAILED line 1"},
		{sec: 2, typ: "m", data: "STEP_FAILED line 2"},
	}
	markers, err := findMarkers(events, `^STEP_FAILED`, 1, 1.75)
	if err != nil {
		t.Fatal(err)
	}
	if len(markers) != 1 {
		t.Fatalf("markers = %#v, want one", markers)
	}
	if markers[0].Index != 0 || markers[0].eventIndex != 2 || markers[0].Label != "STEP_FAILED line 1" || markers[0].Kind != "failure" {
		t.Fatalf("marker = %#v, want filtered marker identity with failure kind", markers[0])
	}

	if _, err := findMarkers(events, "[", 0, -1); err == nil {
		t.Fatal("invalid regexp was accepted")
	}
}

func TestMarkersCommandAndRenderMarkerSelection(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "trec")
	if output, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build trec: %v\n%s", err, output)
	}
	cast := filepath.Join(t.TempDir(), "markers.cast")
	if err := writeCastFile(cast, castHeader{Version: 2, Width: 20, Height: 4}, []castEvent{
		{sec: 0.1, typ: "o", data: "before"},
		{sec: 0.2, typ: "m", data: "STEP_START line 1"},
		{sec: 0.3, typ: "o", data: " after"},
		{sec: 0.4, typ: "m", data: "STEP_FAILED line 1"},
	}); err != nil {
		t.Fatal(err)
	}

	output, err := exec.Command(binary, "markers", "--regex", "^STEP_FAILED", "--output-format", "json", cast).CombinedOutput()
	if err != nil {
		t.Fatalf("markers: %v\n%s", err, output)
	}
	var markers []markerRef
	if err := json.Unmarshal(output, &markers); err != nil {
		t.Fatalf("decode marker output: %v\n%s", err, output)
	}
	if len(markers) != 1 || markers[0].Index != 0 || markers[0].Label != "STEP_FAILED line 1" {
		t.Fatalf("markers = %#v", markers)
	}

	output, err = exec.Command(binary, "render", "--marker-index", "0", "--output-format", "jsonl", cast).CombinedOutput()
	if err != nil {
		t.Fatalf("render marker: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), `"marker":"STEP_START line 1"`) || !strings.Contains(string(output), "before") {
		t.Fatalf("rendered marker output = %s", output)
	}

	output, err = exec.Command(binary, "render", "--last-marker", "--output-format", "jsonl", cast).CombinedOutput()
	if err != nil {
		t.Fatalf("render last-marker: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), `"marker":"STEP_FAILED line 1"`) || !strings.Contains(string(output), "after") {
		t.Fatalf("rendered last-marker output = %s", output)
	}
}
