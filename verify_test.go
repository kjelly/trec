package main

import (
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

	report, err := verifyPaths([]string{dir, path})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Valid || report.Checked != 1 || report.Passed != 1 || report.Failed != 0 {
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
