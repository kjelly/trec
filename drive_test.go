package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
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

func TestRecordingWriterWritesOutputImmediatelyWithoutSecrets(t *testing.T) {
	redactor := &secretRedactor{}
	var output bytes.Buffer
	bw := bufio.NewWriter(&output)
	var mu sync.Mutex
	recorder := newRecordingWriter(bw, &mu, redactor)

	recorder.output(1.25, "visible now")
	// Flush only the buffered writer, not the pending output tail. With no
	// declared secrets, output must not be retained until flushOutput.
	recorder.flush()

	const want = "[1.25,\"o\",\"visible now\"]\n"
	if got := output.String(); got != want {
		t.Fatalf("cast output before flushOutput = %q, want %q", got, want)
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
	interactive := &driveSession{interactive: true, ts: &terminalSession{}}
	if err := interactive.applyStep(context.Background(), wait); err == nil {
		t.Fatal("interactive WAIT_CHILD_EXIT unexpectedly succeeded")
	}
	if err := interactive.applyStep(context.Background(), assert); err == nil {
		t.Fatal("interactive ASSERT_EXIT unexpectedly succeeded")
	}

	for _, line := range []string{"WAIT_CHILD_EXIT extra", "ASSERT_EXIT", "ASSERT_EXIT -1"} {
		if _, err := parseDriveLine(line, 3); err == nil {
			t.Errorf("%q unexpectedly parsed", line)
		}
	}
}

func TestParseJSONDriveSteps(t *testing.T) {
	// 1. Valid JSON steps
	step1, err := parseDriveLine(`{"kind": "text", "text": "hello"}`, 1)
	if err != nil || step1.kind != "text" || step1.text != "hello" {
		t.Fatalf("failed to parse valid JSON step: %#v, %v", step1, err)
	}

	step2, err := parseDriveLine(`{"op": "DOWN", "n": 3}`, 2)
	if err != nil || step2.kind != "down" || step2.n != 3 {
		t.Fatalf("failed to parse valid JSON step with op: %#v, %v", step2, err)
	}

	step3, err := parseDriveLine(`{"kind": "expect", "text": "target"}`, 3)
	if err != nil || step3.kind != "expect" || step3.text != "target" {
		t.Fatalf("failed to parse valid JSON expect step: %#v, %v", step3, err)
	}

	step4, err := parseDriveLine(`{"kind": "assert_exit", "n": 0}`, 4)
	if err != nil || step4.kind != "assert_exit" || step4.n != 0 || !step4.hasN {
		t.Fatalf("failed to parse valid JSON assert_exit 0 step: %#v, %v", step4, err)
	}

	// 2. Invalid/Unknown JSON steps - should be rejected with errors
	invalidLines := []struct {
		input string
		msg   string
	}{
		{`{"kind": "typo"}`, "unknown op"},
		{`{"kind": "expect"}`, "EXPECT needs text"},
		{`{"kind": "expect", "text": ""}`, "EXPECT needs text"},
		{`{"kind": "text_env", "text": "bad env"}`, "TEXT_ENV needs an environment variable name"},
		{`{"kind": "text_file", "text": ""}`, "TEXT_FILE needs a path"},
		{`{"kind": "select", "text": ""}`, "SELECT needs a label"},
		{`{"kind": "wait_child_exit", "text": "extra"}`, "WAIT_CHILD_EXIT takes no arguments"},
		{`{"kind": "assert_exit", "n": -1}`, "ASSERT_EXIT needs a non-negative exit code"},
		{`{"kind": "assert_exit"}`, "ASSERT_EXIT needs an exit code"},
		{`{"kind": "expect", "text": "ready", "n": 0}`, "EXPECT needs a positive timeout duration"},
		{`{"kind": "expect", "text": "ready", "n": -1}`, "EXPECT needs a positive timeout duration"},
		{`{"kind": "expect_change", "n": 0}`, "EXPECT_CHANGE needs a positive timeout duration"},
		{`{"kind": "expect_change", "n": -1}`, "EXPECT_CHANGE needs a positive timeout duration"},
		{`{"kind": "expect_regex", "text": ""}`, "EXPECT_REGEX needs text pattern"},
		{`{"kind": "expect_screen_regex", "text": ""}`, "EXPECT_SCREEN_REGEX needs text pattern"},
		{`{"kind": "text"`, "invalid JSON request"}, // Malformed JSON syntax
	}

	for _, tc := range invalidLines {
		_, err := parseDriveLine(tc.input, 5)
		if err == nil {
			t.Errorf("expected JSON line %q to be rejected, but it succeeded", tc.input)
		} else if !strings.Contains(err.Error(), tc.msg) {
			t.Errorf("expected error message to contain %q, got: %v", tc.msg, err)
		}
	}
}

func TestDriveNavigationStepsSendExpectedEscapeSequences(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()

	var cast bytes.Buffer
	var mu sync.Mutex
	redactor := &secretRedactor{}
	pr, pw, _ := os.Pipe()
	defer pr.Close()
	defer pw.Close()
	ts := newTerminalSession(w, pr, nil, 80, 24, newRecordingWriter(bufio.NewWriter(&cast), &mu, redactor), redactor, false, nil)
	ds := &driveSession{
		ts:       ts,
		redactor: redactor,
	}
	for lineNo, line := range []string{"LEFT 2", "RIGHT", "ESCAPE", "CTRLU", "CTRLW", "CLEAR_LINE"} {
		step, err := parseDriveLine(line, lineNo+1)
		if err != nil {
			t.Fatalf("parse %q: %v", line, err)
		}
		if err := ds.applyStep(context.Background(), step); err != nil {
			t.Fatalf("apply %q: %v", line, err)
		}
	}
	const want = "\x1b[D\x1b[D\x1b[C\x1b\x15\x17\x15"
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("navigation bytes = %q, want %q", got, want)
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

func TestExpectAndBaselineSemantics(t *testing.T) {
	var cast bytes.Buffer
	var mu sync.Mutex
	redactor := &secretRedactor{}
	pr, pw, _ := os.Pipe()
	defer pr.Close()
	defer pw.Close()
	ts := newTerminalSession(pw, pr, nil, 80, 24, newRecordingWriter(bufio.NewWriter(&cast), &mu, redactor), redactor, false, nil)
	ds := &driveSession{ts: ts, expectTimeout: 50 * time.Millisecond, redactor: redactor}

	// 1. Regex vs Screen Regex
	ts.feedOutput([]byte("line1\nline2"))
	time.Sleep(50 * time.Millisecond)

	stReg, _ := parseDriveLine("EXPECT_REGEX line1.*line2", 1)
	if err := ds.applyStep(context.Background(), stReg); err == nil {
		t.Fatal("EXPECT_REGEX unexpectedly matched across lines")
	}
	// We need `(?s)` for dot to match newline
	stScr, _ := parseDriveLine("EXPECT_SCREEN_REGEX (?s)line1.*line2", 2)
	if err := ds.applyStep(context.Background(), stScr); err != nil {
		t.Fatalf("EXPECT_SCREEN_REGEX failed to match across lines: %v", err)
	}

	// 2. EXPECT_CHANGE baseline failure without prior mutation (even with empty TEXT)
	stTextEmpty, _ := parseDriveLine("TEXT ", 4)
	_ = ds.applyStep(context.Background(), stTextEmpty)

	stChgFail, _ := parseDriveLine("EXPECT_CHANGE", 5)
	if err := ds.applyStep(context.Background(), stChgFail); err == nil || !strings.Contains(err.Error(), "no previous mutating action") {
		t.Fatalf("EXPECT_CHANGE unexpectedly succeeded or gave wrong error without prior mutation: %v", err)
	}

	// 3. EXPECT_CHANGE baseline success
	stSpace, _ := parseDriveLine("SPACE", 6)
	ds.applyStep(context.Background(), stSpace)

	go func() {
		time.Sleep(10 * time.Millisecond)
		ts.feedOutput([]byte(" change"))
	}()
	stChg, _ := parseDriveLine("EXPECT_CHANGE", 7)
	if err := ds.applyStep(context.Background(), stChg); err != nil {
		t.Fatalf("EXPECT_CHANGE failed: %v", err)
	}

	// 4. Cursor-only EXPECT_CHANGE
	stLeft, _ := parseDriveLine("LEFT 1", 8)
	ds.applyStep(context.Background(), stLeft) // Moves cursor to left
	go func() {
		time.Sleep(10 * time.Millisecond)
		ts.feedOutput([]byte("\x1b[D")) // Cursor left ANSI
	}()
	stChg2, _ := parseDriveLine("EXPECT_CHANGE", 9)
	if err := ds.applyStep(context.Background(), stChg2); err != nil {
		t.Fatalf("EXPECT_CHANGE failed on cursor movement: %v", err)
	}

	// 5. Zero-byte mutation after successful mutation clears baseline
	stZero, _ := parseDriveLine("TEXT ", 10)
	ds.applyStep(context.Background(), stZero) // 0 bytes written, baselineValid = false
	stChgFail2, _ := parseDriveLine("EXPECT_CHANGE", 11)
	if err := ds.applyStep(context.Background(), stChgFail2); err == nil || !strings.Contains(err.Error(), "no previous mutating action") {
		t.Fatalf("EXPECT_CHANGE unexpectedly succeeded after 0-byte mutation cleared baseline: %v", err)
	}
}

func TestDriveTimeoutErrorDoesNotLeakSecret(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "trec")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build trec: %v\n%s", err, output)
	}

	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "steps.txt")
	castPath := filepath.Join(dir, "run.cast")
	resultPath := castPath + ".result.json"

	const secret = "my-super-secret-value"
	script := "EXPECT_EVENTUALLY missing-string"
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Use width=5 so secret wraps across lines
	cmd := exec.CommandContext(ctx, binary, "drive", "--width", "5", "--expect-timeout", "10", "--secret-env", "MYSECRET", "--script", scriptPath, "-o", castPath, "--", "sh", "-c", "echo $MYSECRET && sleep 2")
	cmd.Env = append(os.Environ(), "MYSECRET="+secret)
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	// We intentionally do not check stdout because the raw PTY echo is unredacted.
	err := cmd.Run()

	if err == nil {
		t.Fatalf("drive succeeded unexpectedly:\n%s", errBuf.String())
	}

	if strings.Contains(errBuf.String(), secret) {
		t.Fatalf("secret leaked in stderr output:\n%s", errBuf.String())
	}

	resultJSON, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(resultJSON), secret) {
		t.Fatalf("secret leaked in result JSON:\n%s", resultJSON)
	}
	var result map[string]any
	if err := json.Unmarshal(resultJSON, &result); err != nil {
		t.Fatalf("failed to parse result JSON: %v", err)
	}
	screens, ok := result["final_screen"].([]any)
	if !ok || len(screens) == 0 || screens[0] != "<screen redacted>" {
		t.Fatalf("result JSON final_screen does not contain <screen redacted>: %v", result["final_screen"])
	}

	castData, err := os.ReadFile(castPath)
	if err == nil {
		if strings.Contains(string(castData), secret) {
			t.Fatalf("secret leaked in cast file:\n%s", castData)
		}
		if !strings.Contains(string(castData), `<screen redacted>`) && !strings.Contains(string(castData), `\u003cscreen redacted\u003e`) {
			t.Fatalf("cast marker does not contain <screen redacted> placeholder:\n%s", castData)
		}
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

func TestDriveAndRenderOutputFormat(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "trec")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build trec: %v\n%s", err, output)
	}

	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "steps.txt")
	castPath := filepath.Join(dir, "run.cast")
	if err := os.WriteFile(scriptPath, []byte("WAIT_CHILD_EXIT\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 1. Test validation error with invalid --output-format on drive
	cmdDrive := exec.Command(binary, "drive", "--output-format", "json", "--script", scriptPath, "-o", castPath, "--", "echo", "1")
	outDrive, errDrive := cmdDrive.CombinedOutput()
	if errDrive == nil || !strings.Contains(string(outDrive), "invalid --output-format") {
		t.Fatalf("expected drive validation error, got: %v\n%s", errDrive, outDrive)
	}

	// 2. Test successful JSONL output format on drive (interactive)
	cmdDriveJSONL := exec.Command(binary, "drive", "--output-format", "jsonl", "--interactive", "-o", castPath, "--", "echo", "hello-jsonl")
	stdin, err := cmdDriveJSONL.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}

	var outBuf bytes.Buffer
	cmdDriveJSONL.Stdout = &outBuf
	if err := cmdDriveJSONL.Start(); err != nil {
		t.Fatal(err)
	}

	stdin.Write([]byte("WAIT_CHILD_EXIT\n"))
	stdin.Close()

	if err := cmdDriveJSONL.Wait(); err != nil {
		t.Fatalf("drive interactive failed: %v", err)
	}

	outDriveJSONL := outBuf.String()
	foundJSONL := false
	for _, line := range strings.Split(outDriveJSONL, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err == nil {
			if _, exists := m["screen"]; exists {
				foundJSONL = true
			}
		}
	}
	if !foundJSONL {
		t.Fatalf("expected JSONL structured output from drive, got:\n%s", outDriveJSONL)
	}

	// 3. Test validation error with invalid --output-format on render
	cmdRender := exec.Command(binary, "render", "--output-format", "json", castPath)
	outRender, errRender := cmdRender.CombinedOutput()
	if errRender == nil || !strings.Contains(string(outRender), "invalid --output-format") {
		t.Fatalf("expected render validation error, got: %v\n%s", errRender, outRender)
	}

	// 4. Test JSONL output format on render
	cmdRenderJSONL := exec.Command(binary, "render", "--output-format", "jsonl", castPath)
	outRenderJSONL, errRenderJSONL := cmdRenderJSONL.CombinedOutput()
	if errRenderJSONL != nil {
		t.Fatalf("failed to run render with jsonl format: %v\n%s", errRenderJSONL, outRenderJSONL)
	}
	foundRenderJSONL := false
	for _, line := range strings.Split(string(outRenderJSONL), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err == nil {
			if _, exists := m["screen"]; exists {
				foundRenderJSONL = true
			}
		}
	}
	if !foundRenderJSONL {
		t.Fatalf("expected JSONL structured output from render, got:\n%s", outRenderJSONL)
	}
}

func TestParseJSONClearLine(t *testing.T) {
	st, err := parseDriveLine(`{"op":"CLEAR_LINE"}`, 1)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if st.kind != "ctrlu" {
		t.Fatalf("expected ctrlu, got %q", st.kind)
	}
}

func TestDriveRespondResultRespectsTaint(t *testing.T) {
	os.Setenv("TEST_DRIVE_TAINT", "mysecret")
	defer os.Unsetenv("TEST_DRIVE_TAINT")

	redactor, _ := newSecretRedactor([]string{"TEST_DRIVE_TAINT"})
	dummyIn, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	defer dummyIn.Close()
	ts := newTerminalSession(dummyIn, strings.NewReader(""), nil, 80, 24, nil, redactor, false, nil)
	ts.tainted = true
	ds := &driveSession{
		ts:       ts,
		redactor: redactor,
	}

	// Just check the result JSON
	res := ds.result("success", 0, "ok")
	if len(res.FinalScreen) != 1 || res.FinalScreen[0] != "<screen redacted>" {
		t.Fatalf("expected result.FinalScreen to be redacted, got %v", res.FinalScreen)
	}

	// For respond, we'd need to capture stdout.
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	ds.respond(nil)
	w.Close()
	os.Stdout = oldStdout

	var buf bytes.Buffer
	buf.ReadFrom(r)
	out := buf.String()
	if !strings.Contains(out, "<screen redacted>") {
		t.Fatalf("expected respond output to be redacted, got %s", out)
	}
}
