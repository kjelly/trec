package main

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestRecordMarkerFD verifies that a child process can self-report "m"
// events in real time by writing lines to the TREC_MARKER_FD pipe, without
// going through stdin/keystrokes — the path pilot-style automation drivers
// that manipulate their own TUI model in-process need, since they never
// send real keystrokes through the PTY.
func TestRecordMarkerFD(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "trec")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build trec: %v\n%s", err, output)
	}

	dir := t.TempDir()
	castPath := filepath.Join(dir, "record.cast")
	script := `printf before; printf 'STEP_ACTION add_vault_key\n' >&3; printf after`
	cmd := exec.Command(binary, "-o", castPath, "--", "sh", "-c", script)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("record: %v\n%s", err, output)
	}

	_, events, err := loadCastFile(castPath)
	if err != nil {
		t.Fatalf("load cast: %v", err)
	}

	var found bool
	for _, e := range events {
		if e.typ == "m" && e.data == "STEP_ACTION add_vault_key" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("marker event missing from cast events: %+v", events)
	}
}
