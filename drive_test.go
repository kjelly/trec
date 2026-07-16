package main

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRecordingWriterRedactsAllCastFieldsAndSplitOutput(t *testing.T) {
	const secret = "split-secret-value"
	redactor := &secretRedactor{}
	redactor.add("APP_PASSWORD", secret)
	var output bytes.Buffer
	bw := bufio.NewWriter(&output)
	var mu sync.Mutex
	recorder := newRecordingWriter(bw, &mu, redactor)
	recorder.writeHeader(castHeader{Command: "run --password=" + secret, CommandLabel: "check " + secret, Title: secret})
	recorder.event(0, "i", "input="+secret)
	recorder.event(0, "m", "marker="+secret)
	recorder.output(0, "prefix "+secret[:7])
	recorder.output(1, secret[7:]+" suffix")
	recorder.flushOutput()
	recorder.flush()

	cast := output.String()
	if strings.Contains(cast, secret) {
		t.Fatalf("secret leaked into cast:\n%s", cast)
	}
	if got := strings.Count(cast, `\u003credacted:APP_PASSWORD\u003e`); got != 6 {
		t.Fatalf("redacted occurrences = %d, want 6:\n%s", got, cast)
	}
}

func TestParseDriveLifecycleSteps(t *testing.T) {
	textEnv, err := parseDriveLine("TEXT_ENV APP_PASSWORD", 1)
	if err != nil || textEnv.kind != "text_env" || textEnv.text != "APP_PASSWORD" {
		t.Fatalf("TEXT_ENV = %#v, %v", textEnv, err)
	}
	if _, err := parseDriveLine("TEXT_ENV bad-name", 1); err == nil {
		t.Fatal("invalid TEXT_ENV unexpectedly parsed")
	}
	textFile, err := parseDriveLine("TEXT_FILE /run/secrets/password", 1)
	if err != nil || textFile.kind != "text_file" || textFile.text != "/run/secrets/password" {
		t.Fatalf("TEXT_FILE = %#v, %v", textFile, err)
	}
	wait, err := parseDriveLine("WAIT_CHILD_EXIT", 1)
	if err != nil || wait.kind != "wait_child_exit" {
		t.Fatalf("WAIT_CHILD_EXIT = %#v, %v", wait, err)
	}

	assert, err := parseDriveLine("ASSERT_EXIT 7", 2)
	if err != nil || assert.kind != "assert_exit" || assert.n != 7 {
		t.Fatalf("ASSERT_EXIT = %#v, %v", assert, err)
	}
	interactive := &driveSession{interactive: true}
	if err := interactive.applyStep(wait); err == nil {
		t.Fatal("interactive WAIT_CHILD_EXIT unexpectedly succeeded")
	}
	if err := interactive.applyStep(assert); err == nil {
		t.Fatal("interactive ASSERT_EXIT unexpectedly succeeded")
	}

	for _, line := range []string{"WAIT_CHILD_EXIT extra", "ASSERT_EXIT", "ASSERT_EXIT -1"} {
		if _, err := parseDriveLine(line, 3); err == nil {
			t.Errorf("%q unexpectedly parsed", line)
		}
	}
}

func TestDriveTextFileAndSecretFileDoNotLeakToCast(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "trec")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build trec: %v\n%s", err, output)
	}

	dir := t.TempDir()
	secretPath := filepath.Join(dir, "password")
	scriptPath := filepath.Join(dir, "steps.txt")
	castPath := filepath.Join(dir, "run.cast")
	const secret = "file-backed-secret"
	if err := os.WriteFile(secretPath, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scriptPath, []byte("TEXT_FILE "+secretPath+"\nENTER\nWAIT_CHILD_EXIT\nASSERT_EXIT 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "drive", "--secret-file", "LEGACY_PASSWORD="+secretPath, "--script", scriptPath, "-o", castPath, "--", "sh", "-c", "IFS= read -r value; printf 'echoed:%s' \"$value\"")
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("drive timed out: %v\n%s", ctx.Err(), output)
	}
	if err != nil {
		t.Fatalf("drive: %v\n%s", err, output)
	}
	cast, err := os.ReadFile(castPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cast), secret) {
		t.Fatalf("file secret leaked into cast:\n%s", cast)
	}
	if !strings.Contains(string(cast), `\u003credacted:LEGACY_PASSWORD\u003e`) {
		t.Fatalf("named secret-file redaction missing:\n%s", cast)
	}
}

func TestDriveTextEnvDoesNotLeakToCast(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "trec")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build trec: %v\n%s", err, output)
	}

	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "steps.txt")
	castPath := filepath.Join(dir, "run.cast")
	if err := os.WriteFile(scriptPath, []byte("TEXT_ENV DRIVE_PASSWORD\nENTER\nWAIT_CHILD_EXIT\nASSERT_EXIT 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const secret = "correct-horse-battery-staple"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "drive", "--script", scriptPath, "-o", castPath, "--", "sh", "-c", "IFS= read -r value; printf 'echoed:%s' \"$value\"")
	cmd.Env = append(os.Environ(), "DRIVE_PASSWORD="+secret)
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("drive timed out: %v\n%s", ctx.Err(), output)
	}
	if err != nil {
		t.Fatalf("drive: %v\n%s", err, output)
	}
	cast, err := os.ReadFile(castPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cast), secret) {
		t.Fatalf("TEXT_ENV secret leaked into cast:\n%s", cast)
	}
	if !strings.Contains(string(cast), `\u003credacted:DRIVE_PASSWORD\u003e`) {
		t.Fatalf("cast has no redaction marker:\n%s", cast)
	}
}

func TestRecordDoesNotStoreCommandByDefault(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "trec")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build trec: %v\n%s", err, output)
	}

	dir := t.TempDir()
	castPath := filepath.Join(dir, "record.cast")
	const secret = "header-only-secret"
	cmd := exec.Command(binary, "--secret-env", "HEADER_PASSWORD", "--command-label", "safe-check", "-o", castPath, "--", "sh", "-c", "printf done")
	cmd.Env = append(os.Environ(), "HEADER_PASSWORD="+secret)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("record: %v\n%s", err, output)
	}
	cast, err := os.ReadFile(castPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cast), secret) || strings.Contains(string(cast), `"command":`) {
		t.Fatalf("command leaked into default header:\n%s", cast)
	}
	if !strings.Contains(string(cast), `"command_label":"safe-check"`) {
		t.Fatalf("command label missing:\n%s", cast)
	}
}

func TestDriveWaitChildExitAndAssertExit(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "trec")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build trec: %v\n%s", err, output)
	}

	run := func(t *testing.T, script, child string, wantSuccess bool) (string, string) {
		t.Helper()
		dir := t.TempDir()
		scriptPath := filepath.Join(dir, "steps.txt")
		castPath := filepath.Join(dir, "run.cast")
		if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, "drive", "--timeout", "1", "--script", scriptPath, "-o", castPath, "--", "sh", "-c", child)
		output, err := cmd.CombinedOutput()
		if ctx.Err() != nil {
			t.Fatalf("drive timed out: %v\n%s", ctx.Err(), output)
		}
		if (err == nil) != wantSuccess {
			t.Fatalf("drive success=%t, want %t: %v\n%s", err == nil, wantSuccess, err, output)
		}
		cast, err := os.ReadFile(castPath)
		if err != nil {
			t.Fatalf("read cast: %v", err)
		}
		return string(output), string(cast)
	}

	t.Run("wait ignores post-input timeout and accepts zero", func(t *testing.T) {
		output, cast := run(t, "WAIT_CHILD_EXIT\nASSERT_EXIT 0\n", "sleep 2; printf done", true)
		if !strings.Contains(output, "process exited 0") {
			t.Fatalf("missing successful exit report:\n%s", output)
		}
		if !strings.Contains(cast, "line 1: WAIT_CHILD_EXIT") || !strings.Contains(cast, "line 2: ASSERT_EXIT 0") {
			t.Fatalf("cast is missing lifecycle markers:\n%s", cast)
		}
	})

	t.Run("nonzero exit writes failure marker and reports final screen", func(t *testing.T) {
		output, cast := run(t, "WAIT_CHILD_EXIT\nASSERT_EXIT 0\n", "printf final-output; exit 7", false)
		for _, want := range []string{"ASSERT_EXIT 0: child exit code 7", "---- screen"} {
			if !strings.Contains(output, want) {
				t.Errorf("output missing %q:\n%s", want, output)
			}
		}
		if !strings.Contains(cast, "FAILED line 2: ASSERT_EXIT 0") || !strings.Contains(cast, "final-output") {
			t.Fatalf("cast is missing failure evidence:\n%s", cast)
		}
	})

	t.Run("explicit nonzero assertion succeeds", func(t *testing.T) {
		output, cast := run(t, "WAIT_CHILD_EXIT\nASSERT_EXIT 7\n", "exit 7", true)
		if !strings.Contains(output, "process exited: exit status 7") {
			t.Fatalf("missing exit report:\n%s", output)
		}
		if strings.Contains(cast, "FAILED") {
			t.Fatalf("successful assertion recorded failure:\n%s", cast)
		}
	})
}
