package main

import (
	"encoding/json"
	"math"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func writeMergeTestCast(t *testing.T, path, commandLabel, status string, events []castEvent) {
	t.Helper()
	if err := writeCastFile(path, castHeader{Version: 2, Width: 80, Height: 24, CommandLabel: commandLabel}, events); err != nil {
		t.Fatal(err)
	}
	if status == "" {
		return
	}
	data, err := json.Marshal(sessionResult{Status: status, ExitCode: 0, CommandLabel: commandLabel})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(resultPath(path), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSelectMergeCandidatesFiltersStatusAndCommand(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.cast")
	failed := filepath.Join(dir, "failed.cast")
	other := filepath.Join(dir, "other.cast")
	writeMergeTestCast(t, good, "deploy production", "success", nil)
	writeMergeTestCast(t, failed, "deploy production", "failed", nil)
	writeMergeTestCast(t, other, "backup production", "success", nil)

	matched, err := selectMergeCandidates([]string{dir}, parseMergeStatuses([]string{"success"}), mustRegexp(t, `^deploy`))
	if err != nil {
		t.Fatal(err)
	}
	if len(matched) != 1 || matched[0].path != good {
		t.Fatalf("matched = %#v, want only %s", matched, good)
	}
}

func mustRegexp(t *testing.T, pattern string) *regexp.Regexp {
	t.Helper()
	re, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatal(err)
	}
	return re
}

func TestMergeCastsCreatesOneCompletionMarkerAndResize(t *testing.T) {
	candidates := []mergeCandidate{
		{path: "first.cast", header: castHeader{Version: 2, Width: 80, Height: 24}, events: []castEvent{{sec: 0.1, typ: "o", data: "first"}, {sec: 0.2, typ: "m", data: "SESSION_END status=success exit_code=0"}}, result: &sessionResult{Status: "success"}},
		{path: "second.cast", header: castHeader{Version: 2, Width: 100, Height: 30}, events: []castEvent{{sec: 0.1, typ: "o", data: "second"}, {sec: 0.2, typ: "m", data: "SESSION_END status=success exit_code=0"}}, result: &sessionResult{Status: "success"}},
	}
	_, events, duration := mergeCasts(candidates, 0.5)
	if math.Abs(duration-0.9) > 1e-9 {
		t.Fatalf("duration = %v, want 0.9", duration)
	}
	ends := 0
	resize := false
	for _, event := range events {
		if event.typ == "m" && strings.HasPrefix(event.data, "SESSION_END ") {
			ends++
		}
		if event.typ == "r" && event.data == "100x30" && event.sec == 0.7 {
			resize = true
		}
	}
	if ends != 1 || !strings.Contains(events[len(events)-1].data, "status=success") {
		t.Fatalf("completion events = %#v", events)
	}
	if !resize {
		t.Fatalf("missing second-source resize event: %#v", events)
	}
}

func TestMergeCommandWritesVerifiableCast(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.cast")
	second := filepath.Join(dir, "second.cast")
	output := filepath.Join(dir, "merged.cast")
	writeMergeTestCast(t, first, "release", "success", []castEvent{{sec: 0.1, typ: "o", data: "one"}, {sec: 0.2, typ: "m", data: "SESSION_END status=success exit_code=0"}})
	writeMergeTestCast(t, second, "release", "success", []castEvent{{sec: 0.1, typ: "o", data: "two"}, {sec: 0.2, typ: "m", data: "SESSION_END status=success exit_code=0"}})

	cmd := newRootCommand()
	cmd.SetArgs([]string{"merge", "--status", "success", "--command-regex", "release", "-o", output, first, second})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	verification := verifyCast(output)
	if !verification.Valid {
		t.Fatalf("merged cast did not verify: %#v", verification.Issues)
	}
}
