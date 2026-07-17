package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecordPropagatesExitCodeAndWritesResult(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "trec")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build trec: %v\n%s", err, out)
	}
	cast := filepath.Join(t.TempDir(), "failed.cast")
	cmd := exec.Command(binary, "-o", cast, "--", "sh", "-c", "printf final-screen; exit 7")
	output, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else {
			t.Fatalf("run record: %v\n%s", err, output)
		}
	}
	if code != 7 {
		t.Fatalf("record exit = %d, want 7\n%s", code, output)
	}
	result, err := os.ReadFile(resultPath(cast))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result), `"status": "failed"`) || !strings.Contains(string(result), `"exit_code": 7`) {
		t.Fatalf("unexpected result:\n%s", result)
	}
	if !strings.Contains(string(result), "final-screen") {
		t.Fatalf("record result is missing final screen:\n%s", result)
	}
}

func TestScanCastFindsSecretsButIgnoresRedactedValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scan.cast")
	if err := writeCastFile(path, castHeader{Version: 2, Width: 80, Height: 24}, []castEvent{
		{sec: 1, typ: "o", data: "sshpass -p super-secret ssh host"},
		{sec: 2, typ: "o", data: "password=<redacted:APP_PASSWORD>"},
	}); err != nil {
		t.Fatal(err)
	}
	findings, err := scanCast(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].Rule != "sshpass-password" {
		t.Fatalf("findings = %#v, want one sshpass finding", findings)
	}
}
