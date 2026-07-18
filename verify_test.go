package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
