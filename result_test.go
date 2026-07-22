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
	"time"
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
	if !summary.Cast.SessionEnd || !strings.Contains(string(castData), "SESSION_END status=failed exit_code=7") {
		t.Fatalf("recording lacks completion marker: %#v\n%s", summary.Cast, castData)
	}
	if !strings.Contains(string(castData), `"trec_version":`) || !strings.Contains(string(castData), `"trec_build":`) {
		t.Fatalf("cast header is missing traceable producer build metadata:\n%s", castData)
	}
	if currentBuildMetadata().Revision != "" && !strings.Contains(string(castData), `"revision":`) {
		t.Fatalf("cast header is missing available VCS revision:\n%s", castData)
	}
}

func TestSessionScriptProvenanceIsHashedNormalizedAndRedacted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "steps.drive")
	const secret = "do-not-store-this"
	script := "EXPECT prompt # comment\nTEXT " + secret + "\nENTER_IF prompt # submit\n"
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	redactor := &secretRedactor{}
	redactor.add("PASSWORD", secret)
	steps, err := loadDriveScript(path, redactor)
	if err != nil {
		t.Fatal(err)
	}
	info, err := buildSessionScript(path, steps, redactor)
	if err != nil {
		t.Fatal(err)
	}
	if info.SHA256 == "" || info.StepCount != 3 {
		t.Fatalf("script provenance = %#v", info)
	}
	joined := strings.Join(info.NormalizedSteps, "\n")
	if strings.Contains(joined, secret) || strings.Contains(joined, "comment") {
		t.Fatalf("normalized script leaked literal/comment: %s", joined)
	}
	if !strings.Contains(joined, "TEXT <literal") || !strings.Contains(joined, "ENTER_IF prompt") {
		t.Fatalf("normalized script = %s", joined)
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
	if !result.Scan.Complete || !result.Scan.SafeToShare || result.Scan.FindingsCount != 0 {
		t.Fatalf("scan = %#v, want completed safe scan", result.Scan)
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

func TestPrepareRecordingOutputPreventsStaleResult(t *testing.T) {
	dir := t.TempDir()
	cast := filepath.Join(dir, "session.cast")
	if err := os.WriteFile(cast, []byte("old cast"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resultPath(cast), []byte("old result"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := prepareRecordingOutput(cast, false); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("prepare without force error = %v, want overwrite refusal", err)
	}

	f, err := prepareRecordingOutput(cast, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("new cast"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(resultPath(cast)); !os.IsNotExist(err) {
		t.Fatalf("stale result still exists: %v", err)
	}
	data, err := os.ReadFile(cast)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new cast" {
		t.Fatalf("cast = %q, want replacement", data)
	}
}

func TestPendingResultIsExplicitlyIncomplete(t *testing.T) {
	cast := filepath.Join(t.TempDir(), "pending.cast")
	pending := newPendingSessionResult(time.Unix(123, 456))
	if err := writePendingSessionResult(cast, pending); err != nil {
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
	if result.Status != "in_progress" || result.ExitCode != -1 || result.Cast.Complete || result.Scan.Complete || result.Scan.SafeToShare {
		t.Fatalf("pending result = %#v", result)
	}
	if result.SessionID == "" || result.StartedAt == "" {
		t.Fatalf("pending result lacks identity: %#v", result)
	}
}

func TestWriteSessionResultIncludesBlockingScanSummary(t *testing.T) {
	cast := filepath.Join(t.TempDir(), "secret.cast")
	if err := writeCastFile(cast, castHeader{Version: 2, Width: 80, Height: 24}, []castEvent{
		{sec: 1, typ: "o", data: "password=exposed"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := writeSessionResult(cast, sessionResult{Status: "success", ExitCode: 0}); err != nil {
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
	if !result.Scan.Complete || result.Scan.SafeToShare || result.Scan.FindingsCount != 1 {
		t.Fatalf("scan = %#v, want one blocking finding", result.Scan)
	}
}

func TestRecordRefusesOverwriteUnlessForced(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "trec")
	if output, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build trec: %v\n%s", err, output)
	}
	cast := filepath.Join(t.TempDir(), "record.cast")
	run := func(args ...string) ([]byte, error) {
		command := append([]string{"-o", cast}, args...)
		return exec.Command(binary, command...).CombinedOutput()
	}

	if output, err := run("--", "sh", "-c", "printf first"); err != nil {
		t.Fatalf("first record: %v\n%s", err, output)
	}
	first, err := os.ReadFile(cast)
	if err != nil {
		t.Fatal(err)
	}
	output, err := run("--", "sh", "-c", "printf second")
	if err == nil || !strings.Contains(string(output), "--force") {
		t.Fatalf("overwrite error = %v\n%s", err, output)
	}
	unchanged, err := os.ReadFile(cast)
	if err != nil {
		t.Fatal(err)
	}
	if string(unchanged) != string(first) {
		t.Fatal("refused overwrite modified the cast")
	}

	if output, err := run("--force", "--", "sh", "-c", "printf second"); err != nil {
		t.Fatalf("forced record: %v\n%s", err, output)
	}
	replaced, err := os.ReadFile(cast)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(replaced), "first") || !strings.Contains(string(replaced), "second") {
		t.Fatalf("forced cast was not replaced:\n%s", replaced)
	}
	result := verifyCast(cast)
	if !result.Valid {
		t.Fatalf("forced replacement did not produce valid evidence: %#v", result)
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

func TestScanCastIgnoresTUIUnsetSecretStatus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scan-tui-status.cast")
	if err := writeCastFile(path, castHeader{Version: 2, Width: 80, Height: 24}, []castEvent{
		{sec: 1, typ: "o", data: "thanos_aws_secret_access_key =   [未設定，使用內建預設]"},
		{sec: 2, typ: "o", data: "api_key: [已設定]"},
	}); err != nil {
		t.Fatal(err)
	}
	findings, err := scanCast(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("findings = %#v, want no TUI-status findings", findings)
	}
}

func TestScanCastIgnoresTUISecretPlaceholder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scan-tui-placeholder.cast")
	if err := writeCastFile(path, castHeader{Version: 2, Width: 80, Height: 24}, []castEvent{
		{sec: 1, typ: "o", data: "ipa_admin_password = CHANGE-ME-min-8-chars"},
	}); err != nil {
		t.Fatal(err)
	}
	findings, err := scanCast(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("findings = %#v, want no placeholder findings", findings)
	}
}

func TestScanCastIgnoresDriveMarkerWithoutSecretValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scan-drive-marker.cast")
	if err := writeCastFile(path, castHeader{Version: 2, Width: 80, Height: 24}, []castEvent{
		{sec: 1, typ: "m", data: "STEP_OK line 1: EXPECT ipa_admin_password = (0.123s)"},
	}); err != nil {
		t.Fatal(err)
	}
	findings, err := scanCast(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("findings = %#v, want no drive-marker findings", findings)
	}
}
