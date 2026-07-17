package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTranscriptFormatsAndANSI(t *testing.T) {
	hdr := castHeader{
		Title:   "Demo Session",
		Command: "git commit",
	}

	events := []castEvent{
		{sec: 0.1, typ: "o", data: "hello\r\n"},
		{sec: 0.2, typ: "i", data: "a"},
		{sec: 0.3, typ: "m", data: "savepoint"},
		{sec: 0.4, typ: "o", data: "\x1b[31mred text\x1b[0m\r\n"},
		{sec: 0.5, typ: "o", data: "   "}, // Empty/whitespace-only event, should be skipped
	}

	// 1. Test original text format compatibility
	txtOut, err := generateTranscript(hdr, events, "text")
	if err != nil {
		t.Fatalf("failed to generate text transcript: %v", err)
	}
	if !strings.Contains(txtOut, "# Demo Session") {
		t.Errorf("missing title in text output")
	}
	if !strings.Contains(txtOut, "$ git commit") {
		t.Errorf("missing command in text output")
	}
	if !strings.Contains(txtOut, "[0.10s] hello") {
		t.Errorf("missing cleaned output event")
	}
	if !strings.Contains(txtOut, "[0.20s] » a") {
		t.Errorf("missing visualized input event")
	}
	if !strings.Contains(txtOut, "[0.30s] ⚑ savepoint") {
		t.Errorf("missing marker event")
	}
	if !strings.Contains(txtOut, "[0.40s] red text") {
		t.Errorf("ANSI codes were not stripped in text output")
	}
	if strings.Contains(txtOut, "[0.50s]") {
		t.Errorf("empty event was not skipped")
	}

	// 2. Test JSON format
	jsonOut, err := generateTranscript(hdr, events, "json")
	if err != nil {
		t.Fatalf("failed to generate json transcript: %v", err)
	}
	var jo struct {
		Title   string `json:"title"`
		Command string `json:"command"`
		Events  []struct {
			Time      float64 `json:"time"`
			Type      string  `json:"type"`
			Data      string  `json:"data"`
			CleanData string  `json:"clean_data"`
		} `json:"events"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &jo); err != nil {
		t.Fatalf("invalid json output: %v", err)
	}
	if jo.Title != "Demo Session" || jo.Command != "git commit" {
		t.Errorf("metadata mismatch in json")
	}
	if len(jo.Events) != 4 { // 0.5s event skipped
		t.Errorf("expected 4 events, got %d", len(jo.Events))
	}
	if jo.Events[3].CleanData != "red text\n" {
		t.Errorf("expected clean_data to be 'red text\\n', got %q", jo.Events[3].CleanData)
	}

	// 3. Test JSONL format
	jsonlOut, err := generateTranscript(hdr, events, "jsonl")
	if err != nil {
		t.Fatalf("failed to generate jsonl transcript: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(jsonlOut), "\n")
	if len(lines) != 4 {
		t.Errorf("expected 4 lines in jsonl, got %d", len(lines))
	}
	var firstEvent map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &firstEvent); err != nil {
		t.Fatalf("invalid json line: %v", err)
	}
	if firstEvent["type"] != "o" || firstEvent["clean_data"] != "hello\n" {
		t.Errorf("unexpected content in jsonl line: %v", firstEvent)
	}

	// 4. Test unsupported format error
	_, err = generateTranscript(hdr, events, "xml")
	if err == nil {
		t.Errorf("expected error for unsupported format, got nil")
	}
}

func TestTranscriptStreamsSplitANSIAndOutputChunks(t *testing.T) {
	hdr := castHeader{}
	events := []castEvent{
		{sec: 1, typ: "o", data: "pass\x1b[3"},
		{sec: 2, typ: "o", data: "1mword\x1b[0"},
		{sec: 3, typ: "o", data: "m\r"},
		{sec: 4, typ: "o", data: "\nnext\x1b]0;title\x1b"},
		{sec: 5, typ: "o", data: "\\done"},
	}

	output, err := generateTranscript(hdr, events, "json")
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Events []struct {
			Time      float64 `json:"time"`
			EndTime   float64 `json:"end_time"`
			CleanData string  `json:"clean_data"`
		} `json:"events"`
	}
	if err := json.Unmarshal([]byte(output), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Events) != 1 {
		t.Fatalf("events = %#v, want one merged output event", decoded.Events)
	}
	if got, want := decoded.Events[0].CleanData, "password\nnextdone"; got != want {
		t.Fatalf("clean data = %q, want %q", got, want)
	}
	if decoded.Events[0].Time != 1 || decoded.Events[0].EndTime != 5 {
		t.Fatalf("time range = %.1f-%.1f, want 1.0-5.0", decoded.Events[0].Time, decoded.Events[0].EndTime)
	}
}
