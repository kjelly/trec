package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	var summary sessionResult
	if err := json.Unmarshal(result, &summary); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	castData, err := os.ReadFile(cast)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(castData)
	if !summary.Cast.Complete || summary.Cast.Algorithm != "sha256" || summary.Cast.SHA256 != hex.EncodeToString(digest[:]) || summary.Cast.ByteSize != int64(len(castData)) || summary.Cast.EventCount == 0 {
		t.Fatalf("unexpected cast integrity metadata: %#v", summary.Cast)
	}
	if !strings.Contains(string(castData), `"trec_version":"dev"`) {
		t.Fatalf("cast header is missing producer version:\n%s", castData)
	}
}

func TestWriteSessionResultRecordsCastIntegrityAtomically(t *testing.T) {
	dir := t.TempDir()
	cast := filepath.Join(dir, "session.cast")
	if err := writeCastFile(cast, castHeader{Version: 2, Width: 80, Height: 24}, []castEvent{
		{sec: 0.25, typ: "o", data: "hello"},
		{sec: 1.5, typ: "m", data: "checkpoint"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := writeSessionResult(cast, sessionResult{Status: "success"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(resultPath(cast))
	if err != nil {
		t.Fatal(err)
	}
	var result sessionResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if !result.Cast.Complete || result.Cast.EventCount != 2 || result.Cast.LastEventTime != 1.5 || result.Cast.ByteSize == 0 {
		t.Fatalf("integrity = %#v", result.Cast)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".result.json.tmp-") {
			t.Fatalf("temporary result was not cleaned up: %s", entry.Name())
		}
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
