package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeVerifiableCast(t *testing.T, dir, name, output string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := writeCastFile(path, castHeader{Version: 2, Width: 80, Height: 24, TrecVersion: "test"}, []castEvent{
		{sec: 0.1, typ: "o", data: output},
	}); err != nil {
		t.Fatal(err)
	}
	if err := writeSessionResult(path, sessionResult{Status: "success", ExitCode: 0}); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestVerifyPathsAcceptsValidCastAndDirectory(t *testing.T) {
	dir := t.TempDir()
	path := writeVerifiableCast(t, dir, "valid.cast", "hello")
	nestedDir := filepath.Join(dir, "nested", "recordings")
	if err := os.MkdirAll(nestedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	nestedPath := writeVerifiableCast(t, nestedDir, "nested.cast", "nested")

	report, err := verifyPaths([]string{dir, path, nestedPath})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Valid || report.Checked != 2 || report.Passed != 2 || report.Failed != 0 {
		t.Fatalf("report = %#v", report)
	}
}

func TestVerifyCastRejectsMissingStaleAndUnsafeResults(t *testing.T) {
	t.Run("missing result", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "missing.cast")
		if err := writeCastFile(path, castHeader{Version: 2, Width: 80, Height: 24}, nil); err != nil {
			t.Fatal(err)
		}
		result := verifyCast(path)
		if result.Valid || !strings.Contains(strings.Join(result.Issues, "\n"), "sidecar is missing") {
			t.Fatalf("verification = %#v", result)
		}
	})

	t.Run("stale result", func(t *testing.T) {
		dir := t.TempDir()
		path := writeVerifiableCast(t, dir, "stale.cast", "before")
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString("[1,\"o\",\"after\"]\n"); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		result := verifyCast(path)
		issues := strings.Join(result.Issues, "\n")
		if result.Valid || !strings.Contains(issues, "sha256") || !strings.Contains(issues, "byte size") {
			t.Fatalf("verification = %#v", result)
		}
	})

	t.Run("secret finding", func(t *testing.T) {
		dir := t.TempDir()
		path := writeVerifiableCast(t, dir, "unsafe.cast", "token=exposed")
		result := verifyCast(path)
		if result.Valid || result.Scan.FindingsCount != 1 || !strings.Contains(strings.Join(result.Issues, "\n"), "secret scan found") {
			t.Fatalf("verification = %#v", result)
		}
	})
}

func TestVerifyCastWarnsForDirtyProducerBuild(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dirty.cast")
	if err := writeCastFile(path, castHeader{Version: 2, Width: 80, Height: 24}, nil); err != nil {
		t.Fatal(err)
	}
	if err := writeSessionResult(path, sessionResult{Status: "success", ExitCode: 0}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(resultPath(path))
	if err != nil {
		t.Fatal(err)
	}
	var sidecar sessionResult
	if err := json.Unmarshal(data, &sidecar); err != nil {
		t.Fatal(err)
	}
	sidecar.Build = buildMetadata{Version: "dev", Revision: "abc", Modified: true}
	data, err = json.Marshal(sidecar)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resultPath(path), data, 0o644); err != nil {
		t.Fatal(err)
	}
	result := verifyCast(path)
	if !result.Valid || !strings.Contains(strings.Join(result.Warnings, "\n"), "dirty trec build") {
		t.Fatalf("verification = %#v", result)
	}
}

func TestVerifyCastRejectsSessionEndMismatchAndNonFinalMarker(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mismatch.cast")
	if err := writeCastFile(path, castHeader{Version: 2, Width: 80, Height: 24}, []castEvent{
		{sec: 0.1, typ: "m", data: "SESSION_END status=failed exit_code=7"},
		{sec: 0.2, typ: "o", data: "late output"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := writeSessionResult(path, sessionResult{Status: "success", ExitCode: 0}); err != nil {
		t.Fatal(err)
	}

	result := verifyCast(path)
	issues := strings.Join(result.Issues, "\n")
	for _, want := range []string{
		`SESSION_END status "failed" does not match result status "success"`,
		"SESSION_END exit_code 7 does not match result exit_code 0",
		"SESSION_END is not the final cast event",
	} {
		if !strings.Contains(issues, want) {
			t.Fatalf("issues missing %q:\n%s", want, issues)
		}
	}

	duplicatePath := filepath.Join(dir, "duplicate.cast")
	if err := writeCastFile(duplicatePath, castHeader{Version: 2, Width: 80, Height: 24}, []castEvent{
		{sec: 0.1, typ: "m", data: "SESSION_END status=success exit_code=0"},
		{sec: 0.2, typ: "m", data: "SESSION_END status=success exit_code=0"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := writeSessionResult(duplicatePath, sessionResult{Status: "success", ExitCode: 0}); err != nil {
		t.Fatal(err)
	}
	duplicate := verifyCast(duplicatePath)
	if issues := strings.Join(duplicate.Issues, "\n"); !strings.Contains(issues, "cast has 2 SESSION_END markers") {
		t.Fatalf("duplicate marker issue missing:\n%s", issues)
	}
}

func TestVerifyDiagnosesUnfinishedStepWithoutPendingHashNoise(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unfinished.cast")
	if err := writeCastFile(path, castHeader{Version: 2, Width: 80, Height: 24}, []castEvent{
		{sec: 0.1, typ: "m", data: "STEP_START line 46: WAIT_CHILD_EXIT@3600000"},
	}); err != nil {
		t.Fatal(err)
	}
	pending := newPendingSessionResult(time.Now())
	pending.LastStep = &sessionStep{Line: 46, Operation: "WAIT_CHILD_EXIT@3600000", Phase: "started"}
	if err := writePendingSessionResult(path, pending); err != nil {
		t.Fatal(err)
	}
	result := verifyCast(path)
	issues := strings.Join(result.Issues, "\n")
	if result.Valid || result.UnfinishedStep == "" || !strings.Contains(issues, "unfinished drive step") {
		t.Fatalf("verification = %#v", result)
	}
	for _, noisy := range []string{"sha256 does not match", "byte size", "event count"} {
		if strings.Contains(issues, noisy) {
			t.Fatalf("pending verification contains derivative noise %q:\n%s", noisy, issues)
		}
	}
}

// TestSessionEndMarkerRoundTripWithDisposition verifies the new format
// `SESSION_END status=ended disposition=script_ended exit_code=N` parses
// correctly and the legacy 3-field form is still accepted.
func TestSessionEndMarkerRoundTripWithDisposition(t *testing.T) {
	status, code, disp, err := parseSessionEndMarker("SESSION_END status=ended disposition=script_ended exit_code=0")
	if err != nil {
		t.Fatalf("parse new format: %v", err)
	}
	if status != "ended" || code != 0 || disp != "script_ended" {
		t.Fatalf("got status=%q code=%d disp=%q", status, code, disp)
	}
	// Legacy form must still parse.
	status, code, disp, err = parseSessionEndMarker("SESSION_END status=aborted exit_code=-1")
	if err != nil {
		t.Fatalf("parse legacy: %v", err)
	}
	if status != "aborted" || code != -1 || disp != "" {
		t.Fatalf("got status=%q code=%d disp=%q", status, code, disp)
	}
	// FormatSessionEndMarker produces parseable output.
	marker := formatSessionEndMarker("ended", 0, "script_ended")
	status, code, disp, err = parseSessionEndMarker(marker)
	if err != nil || status != "ended" || code != 0 || disp != "script_ended" {
		t.Fatalf("round-trip: status=%q code=%d disp=%q err=%v", status, code, disp, err)
	}
	// Empty disposition produces the legacy form (no disposition= field).
	marker = formatSessionEndMarker("success", 0, "")
	if !strings.Contains(marker, "exit_code=") || strings.Contains(marker, "disposition=") {
		t.Fatalf("empty disposition should produce legacy form, got: %q", marker)
	}
}

// TestClassifyProgressThreeStates verifies the three-state classification
// applied by verify (pending / heartbeat_stale / completed).
func TestClassifyProgressThreeStates(t *testing.T) {
	now := time.Now()
	// Completed: success status with a heartbeat_at anywhere.
	cp := classifyProgress(sessionResult{
		Status: "success",
		LastStep: &sessionStep{
			UpdatedAt:   now.UTC().Format(time.RFC3339Nano),
			HeartbeatAt: now.UTC().Format(time.RFC3339Nano),
		},
	})
	if cp == nil || cp.Phase != "completed" {
		t.Fatalf("success -> completed, got %#v", cp)
	}
	// Pending: in_progress with a recent heartbeat.
	cp = classifyProgress(sessionResult{
		Status:    "in_progress",
		UpdatedAt: now.UTC().Format(time.RFC3339Nano),
		LastStep: &sessionStep{
			UpdatedAt:   now.UTC().Format(time.RFC3339Nano),
			HeartbeatAt: now.UTC().Format(time.RFC3339Nano),
		},
	})
	if cp == nil || cp.Phase != "pending" || cp.Stale {
		t.Fatalf("recent in_progress -> pending, got %#v", cp)
	}
	// Heartbeat_stale: in_progress with no heartbeat or a stale one.
	oldHeartbeat := now.Add(-10 * time.Minute).UTC().Format(time.RFC3339Nano)
	cp = classifyProgress(sessionResult{
		Status:    "in_progress",
		UpdatedAt: now.UTC().Format(time.RFC3339Nano),
		LastStep: &sessionStep{
			UpdatedAt:   now.UTC().Format(time.RFC3339Nano),
			HeartbeatAt: oldHeartbeat,
		},
	})
	if cp == nil || cp.Phase != "heartbeat_stale" || !cp.Stale {
		t.Fatalf("stale in_progress -> heartbeat_stale, got %#v", cp)
	}
}

// TestDetectInputDriftFlagsModifiedFile verifies the drift detector catches
// inventory / vault files whose mtime is newer than the recorded one.
func TestDetectInputDriftFlagsModifiedFile(t *testing.T) {
	dir := t.TempDir()
	inv := filepath.Join(dir, "inventory.yml")
	if err := os.WriteFile(inv, []byte("initial"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Sleep then bump mtime so current is strictly newer.
	time.Sleep(20 * time.Millisecond)
	now := time.Now()
	if err := os.Chtimes(inv, now, now); err != nil {
		t.Fatal(err)
	}
	oldMTime := now.Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	drift := detectInputDrift(&sessionInputFingerprint{
		InventoryPath:  inv,
		InventoryMTime: oldMTime,
	})
	if len(drift) == 0 || !strings.Contains(drift[0], "modified since the cast") {
		t.Fatalf("expected drift warning, got %v", drift)
	}
}
