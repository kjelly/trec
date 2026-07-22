package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hinshun/vt10x"
)

type bufferWriteCloser struct {
	bytes.Buffer
}

func (b *bufferWriteCloser) Close() error { return nil }

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
	replaceEnv, err := parseDriveLine("REPLACE_TEXT_ENV APP_PASSWORD", 1)
	if err != nil || replaceEnv.kind != "replace_text_env" || replaceEnv.text != "APP_PASSWORD" {
		t.Fatalf("REPLACE_TEXT_ENV = %#v, %v", replaceEnv, err)
	}
	for _, tc := range []struct {
		line string
		kind string
		text string
	}{
		{"TEXT_AND_ENTER hello", "text_and_enter", "hello"},
		{"TEXT_ENV_AND_ENTER APP_PASSWORD", "text_env_and_enter", "APP_PASSWORD"},
		{"TEXT_FILE_AND_ENTER /run/secrets/password", "text_file_and_enter", "/run/secrets/password"},
		{"REPLACE_TEXT_AND_ENTER hello", "replace_text_and_enter", "hello"},
		{"REPLACE_TEXT_ENV_AND_ENTER APP_PASSWORD", "replace_text_env_and_enter", "APP_PASSWORD"},
		{"REPLACE_TEXT_FILE_AND_ENTER /run/secrets/password", "replace_text_file_and_enter", "/run/secrets/password"},
		{"TOGGLE docker", "toggle", "docker"},
		{"CHECKLIST_DOWN 3", "checklist_down", ""},
		{"END_SESSION", "end_session", ""},
	} {
		step, err := parseDriveLine(tc.line, 1)
		if err != nil || step.kind != tc.kind || step.text != tc.text {
			t.Fatalf("%s = %#v, %v", tc.line, step, err)
		}
	}
	textIf, err := parseDriveLine("TEXT_IF Apply changes? => y", 1)
	if err != nil || textIf.kind != "text_if" || textIf.guard != "Apply changes?" || textIf.text != "y" {
		t.Fatalf("TEXT_IF = %#v, %v", textIf, err)
	}
	if _, err := parseDriveLine("TEXT_IF no separator", 1); err == nil {
		t.Fatal("invalid TEXT_IF unexpectedly parsed")
	}
	wait, err := parseDriveLine("WAIT_CHILD_EXIT", 1)
	if err != nil || wait.kind != "wait_child_exit" {
		t.Fatalf("WAIT_CHILD_EXIT = %#v, %v", wait, err)
	}
	timedWait, err := parseDriveLine("WAIT_CHILD_EXIT@2500", 1)
	if err != nil || timedWait.kind != "wait_child_exit" || !timedWait.hasTimeout || timedWait.timeout != 2500 {
		t.Fatalf("WAIT_CHILD_EXIT@2500 = %#v, %v", timedWait, err)
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

	for _, line := range []string{"WAIT_CHILD_EXIT extra", "WAIT_CHILD_EXIT@0", "WAIT_CHILD_EXIT@bad", "ASSERT_EXIT", "ASSERT_EXIT -1"} {
		if _, err := parseDriveLine(line, 3); err == nil {
			t.Errorf("%q unexpectedly parsed", line)
		}
	}
}

func TestDriveSemanticInputOperations(t *testing.T) {
	redactor, err := newSecretRedactor(nil)
	if err != nil {
		t.Fatal(err)
	}
	input := &bufferWriteCloser{}
	ts := &terminalSession{
		in:       input,
		start:    time.Now(),
		cols:     80,
		rows:     24,
		vt:       vt10x.New(vt10x.WithSize(80, 24)),
		redactor: redactor,
	}
	ds := &driveSession{
		ts:        ts,
		redactor:  redactor,
		pointerRe: regexp.MustCompile(`^\s*(?:❯|▸|›|→|»|>)\s`),
	}

	text, err := parseDriveLine("TEXT_AND_ENTER hello", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := ds.applyStep(context.Background(), text); err != nil {
		t.Fatal(err)
	}
	if got := input.String(); got != "hello\r" {
		t.Fatalf("TEXT_AND_ENTER wrote %q", got)
	}

	input.Reset()
	ts.vt.Write([]byte("> docker"))
	toggle, err := parseDriveLine("TOGGLE docker", 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := ds.applyStep(context.Background(), toggle); err != nil {
		t.Fatal(err)
	}
	if got := input.String(); got != " " {
		t.Fatalf("TOGGLE wrote %q", got)
	}
}

func TestParseDriveInlineCommentsAndGuardedActions(t *testing.T) {
	for _, tc := range []struct {
		line string
		kind string
		text string
		raw  string
	}{
		{"ENTER # submit the form", "enter", "", "ENTER"},
		{"ENTER_IF Confirm deployment # only on the confirmation screen", "enter_if", "Confirm deployment", "ENTER_IF Confirm deployment"},
		{"CHOOSE save & exit # select and submit", "choose", "save & exit", "CHOOSE save & exit"},
		{"EXPECT issue#123 # embedded hashes remain literal", "expect", "issue#123", "EXPECT issue#123"},
	} {
		step, err := parseDriveLine(tc.line, 7)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.line, err)
		}
		if step.kind != tc.kind || step.text != tc.text || step.raw != tc.raw {
			t.Errorf("parse %q = kind %q text %q raw %q", tc.line, step.kind, step.text, step.raw)
		}
	}

	jsonStep, err := parseDriveLine(`{"kind":"text","text":"value # literal"}`, 8)
	if err != nil {
		t.Fatal(err)
	}
	if jsonStep.text != "value # literal" {
		t.Fatalf("JSON text = %q", jsonStep.text)
	}
	for _, line := range []string{"ENTER unexpected", "SPACE extra", "QUIT now", `{"kind":"enter","text":"unexpected"}`} {
		if _, err := parseDriveLine(line, 9); err == nil {
			t.Errorf("%q unexpectedly accepted an argument", line)
		}
	}
}

func TestLintDriveStepsRejectsBlindTransitionsAndChecksExitPair(t *testing.T) {
	parse := func(lines ...string) []*driveStep {
		t.Helper()
		var steps []*driveStep
		for i, line := range lines {
			step, err := parseDriveLine(line, i+1)
			if err != nil {
				t.Fatalf("parse %q: %v", line, err)
			}
			steps = append(steps, step)
		}
		return steps
	}

	bad := lintDriveSteps(parse(
		"ENTER",
		"EXPECT_QUIET 5000",
		"ENTER",
		"SELECT save",
		"WAIT_CHILD_EXIT@3600000",
	), true)
	if len(bad) < 4 {
		t.Fatalf("findings = %#v, want blind ENTER, SELECT, long wait, and exit-pair findings", bad)
	}

	good := lintDriveSteps(parse(
		"EXPECT Inventory",
		"ENTER",
		"ENTER_IF Confirm deployment",
		"CHOOSE save & exit",
		"WAIT_CHILD_EXIT@120000",
		"ASSERT_EXIT 0",
	), true)
	if len(good) != 0 {
		t.Fatalf("guarded script findings = %#v", good)
	}

	intentional := lintDriveSteps(parse(
		"EXPECT Confirm apply",
		"TEXT_IF Confirm apply => y",
		"EXPECT complete",
		"EXPECT checklist",
		"CHECKLIST_DOWN 3",
		"SPACE",
		"END_SESSION",
	), true)
	if len(intentional) != 0 {
		t.Fatalf("intentional findings = %#v", intentional)
	}
}

func TestLintDriveStepsRequiresSubmissionAndExplicitDisposition(t *testing.T) {
	parse := func(lines ...string) []*driveStep {
		t.Helper()
		steps := make([]*driveStep, 0, len(lines))
		for i, line := range lines {
			step, err := parseDriveLine(line, i+1)
			if err != nil {
				t.Fatalf("parse %q: %v", line, err)
			}
			steps = append(steps, step)
		}
		return steps
	}

	findings := lintDriveSteps(parse(
		"EXPECT host name",
		"TEXT server-1",
		"EXPECT ansible_host",
	), true)
	joined := fmt.Sprint(findings)
	if !strings.Contains(joined, "TEXT is followed by a screen transition without ENTER") ||
		!strings.Contains(joined, "no explicit terminal disposition") {
		t.Fatalf("findings = %#v", findings)
	}

	good := lintDriveSteps(parse(
		"EXPECT host name",
		"TEXT_AND_ENTER server-1",
		"EXPECT roles",
		"TOGGLE docker",
		"END_SESSION",
	), true)
	if len(good) != 0 {
		t.Fatalf("semantic script findings = %#v", good)
	}

	report := makeDriveLintReport("good.drive", nil)
	if report.Findings == nil {
		t.Fatal("JSON findings must encode as [] rather than null")
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
	step5, err := parseDriveLine(`{"kind":"wait_child_exit","timeout_ms":2500}`, 5)
	if err != nil || step5.kind != "wait_child_exit" || !step5.hasTimeout || step5.timeout != 2500 {
		t.Fatalf("failed to parse timed wait step: %#v, %v", step5, err)
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
		{`{"kind": "wait_child_exit", "timeout_ms": 0}`, "WAIT_CHILD_EXIT needs a positive timeout duration"},
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

func TestParseDriveLineNormalizesArgumentWhitespaceAndRejectsZeroCounts(t *testing.T) {
	for _, tc := range []struct {
		line string
		text string
	}{
		{"TEXT  hello", "hello"},
		{"EXPECT  ready", "ready"},
		{"ASSERT  saved", "saved"},
		{"SELECT  option", "option"},
	} {
		st, err := parseDriveLine(tc.line, 1)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.line, err)
		}
		if st.text != tc.text {
			t.Errorf("%q text = %q, want %q", tc.line, st.text, tc.text)
		}
	}

	for _, line := range []string{
		"DOWN 0", "UP -1", "LEFT nope", "RIGHT 0", "BACKSPACE 0", "WAIT 0", "EXPECT_QUIET 0",
	} {
		if _, err := parseDriveLine(line, 1); err == nil {
			t.Errorf("%q unexpectedly parsed", line)
		}
	}
	for _, line := range []string{
		`{"kind":"down","n":0}`, `{"kind":"backspace","n":-1}`, `{"kind":"expect_quiet","n":0}`,
	} {
		if _, err := parseDriveLine(line, 1); err == nil {
			t.Errorf("%q unexpectedly parsed", line)
		}
	}
}

func TestParseDriveLabelArgumentsAcceptJSONStyleQuotes(t *testing.T) {
	for _, tc := range []struct {
		line string
		kind string
		want string
	}{
		{`SELECT "hosts.yml"`, "select", "hosts.yml"},
		{`FOCUS "hosts.yml"`, "select", "hosts.yml"},
		{`CHOOSE "hosts.yml — 機器清單與角色"`, "choose", "hosts.yml — 機器清單與角色"},
		{`TOGGLE "🧩 role name"`, "toggle", "🧩 role name"},
		{`ACTIVATE "hosts.yml — 機器清單與角色" WITH ENTER`, "choose", "hosts.yml — 機器清單與角色"},
		{`ACTIVATE "freeipa-server" WITH SPACE`, "toggle", "freeipa-server"},
		{`EXPECT "hosts.yml 路徑"`, "expect", "hosts.yml 路徑"},
		{`EXPECT_TRANSITION "hosts.yml 路徑"`, "expect_transition", "hosts.yml 路徑"},
		{`ASSERT "✅ 已存檔"`, "assert", "✅ 已存檔"},
		{`ENTER_IF "確認送出"`, "enter_if", "確認送出"},
		{`EXPECT@2500 "下一頁"`, "expect", "下一頁"},
	} {
		step, err := parseDriveLine(tc.line, 1)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.line, err)
		}
		if step.kind != tc.kind || step.text != tc.want {
			t.Errorf("parse %q = %#v, want kind=%q text=%q", tc.line, step, tc.kind, tc.want)
		}
	}

	if _, err := parseDriveLine(`SELECT "hosts.yml`, 1); err == nil || !strings.Contains(err.Error(), "invalid quoted SELECT argument") {
		t.Fatalf("unterminated quote error = %v", err)
	}
	for _, line := range []string{
		"ACTIVATE freeipa-server",
		"ACTIVATE freeipa-server WITH TAB",
		`ACTIVATE "freeipa-server WITH SPACE`,
	} {
		if _, err := parseDriveLine(line, 1); err == nil {
			t.Errorf("%q unexpectedly parsed", line)
		}
	}
}

func TestParseExpectQuietWithStepTimeout(t *testing.T) {
	st, err := parseDriveLine("EXPECT_QUIET@12000 5000", 1)
	if err != nil {
		t.Fatal(err)
	}
	if st.kind != "expect_quiet" || st.n != 5000 || !st.hasTimeout || st.timeout != 12000 {
		t.Fatalf("unexpected step: %#v", st)
	}
	for _, line := range []string{"EXPECT_QUIET@0 500", "EXPECT_QUIET@1000 0", "EXPECT_QUIET@1000 nope"} {
		if _, err := parseDriveLine(line, 1); err == nil {
			t.Errorf("%q unexpectedly parsed", line)
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

func TestReplaceTextClearsLineAndRedactsSecretInput(t *testing.T) {
	in := &mockWriter{}
	var cast bytes.Buffer
	var mu sync.Mutex
	redactor := &secretRedactor{}
	redactor.add("APP_PASSWORD", "new-secret")
	recorder := newRecordingWriter(bufio.NewWriter(&cast), &mu, redactor)
	ts := &terminalSession{
		in:       in,
		start:    time.Now(),
		recorder: recorder,
		redactor: redactor,
	}

	written, err := ts.replaceText("new-secret", "APP_PASSWORD", 0)
	if err != nil {
		t.Fatal(err)
	}
	if written != len("new-secret")+1 {
		t.Fatalf("written = %d", written)
	}
	if got := string(in.written); got != "\x15new-secret" {
		t.Fatalf("terminal input = %q", got)
	}
	if err := recorder.flush(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cast.String(), "new-secret") || !strings.Contains(cast.String(), "redacted:APP_PASSWORD") {
		t.Fatalf("cast did not redact replacement input: %s", cast.String())
	}
}

func TestLiteralTextAllowedOnPasswordPromptAndMarkersHideText(t *testing.T) {
	in := &mockWriter{}
	var cast bytes.Buffer
	var mu sync.Mutex
	redactor := &secretRedactor{}
	redactor.add("TEST_PASSWORD", "literal-secret-value")
	recorder := newRecordingWriter(bufio.NewWriter(&cast), &mu, redactor)
	ts := &terminalSession{
		in:       in,
		start:    time.Now(),
		cols:     80,
		rows:     24,
		vt:       vt10x.New(vt10x.WithSize(80, 24)),
		recorder: recorder,
		redactor: redactor,
	}
	ts.feedOutput([]byte("Enter password:"))
	ds := &driveSession{ts: ts, redactor: redactor, stepMarkers: true}
	step, err := parseDriveLine("TEXT literal-secret-value", 7)
	if err != nil {
		t.Fatal(err)
	}
	ds.stepMarker("START", step, 0)
	if err = ds.applyStep(context.Background(), step); err != nil {
		t.Fatalf("apply literal text on password prompt: %v", err)
	}
	ds.stepMarker("OK", step, time.Millisecond)
	if err := recorder.flush(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cast.String(), "literal-secret-value") {
		t.Fatalf("literal text leaked into marker: %s", cast.String())
	}
	if !strings.Contains(cast.String(), "literal 20 bytes") {
		t.Fatalf("safe marker description missing: %s", cast.String())
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
	if err := os.WriteFile(secretPath, []byte(secret+"\n"), 0o600); err != nil {
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
	if strings.Contains(string(output), secret) {
		t.Fatalf("file secret leaked into drive stdout:\n%s", output)
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

	// EXPECT_TRANSITION must not accept destination text that was already on
	// the source screen; it requires a rendered change after the input.
	ts.feedOutput([]byte(" source destination"))
	time.Sleep(10 * time.Millisecond)
	stEnter, _ := parseDriveLine("ENTER", 7)
	if err := ds.applyStep(context.Background(), stEnter); err != nil {
		t.Fatalf("ENTER: %v", err)
	}
	go func() {
		time.Sleep(10 * time.Millisecond)
		ts.feedOutput([]byte(" changed"))
	}()
	stTransition, _ := parseDriveLine("EXPECT_TRANSITION destination", 7)
	if err := ds.applyStep(context.Background(), stTransition); err != nil {
		t.Fatalf("EXPECT_TRANSITION failed: %v", err)
	}
	if err := ds.applyStep(context.Background(), stTransition); err == nil || !strings.Contains(err.Error(), "no previous mutating action") {
		t.Fatalf("EXPECT_TRANSITION baseline was unexpectedly reusable: %v", err)
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

func TestDriveResultIntegrityMatchesOverwrittenCast(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "trec")
	if output, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build trec: %v\n%s", err, output)
	}
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "steps.txt")
	if err := os.WriteFile(scriptPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	castPath := filepath.Join(dir, "run.cast")
	if err := os.WriteFile(castPath, []byte("{\"version\":2,\"width\":80,\"height\":24}\n[0,\"o\",\"old\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(binary, "drive", "--script", scriptPath, "-o", castPath, "--force", "--", "sh", "-c", "printf new").CombinedOutput(); err != nil {
		t.Fatalf("run drive: %v\n%s", err, output)
	}

	resultData, err := os.ReadFile(resultPath(castPath))
	if err != nil {
		t.Fatal(err)
	}
	var summary sessionResult
	if err := json.Unmarshal(resultData, &summary); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	castData, err := os.ReadFile(castPath)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(castData)
	if summary.Cast.SHA256 != hex.EncodeToString(digest[:]) || summary.Cast.ByteSize != int64(len(castData)) {
		t.Fatalf("result integrity does not match replacement cast: %#v", summary.Cast)
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

	t.Run("per-step timeout permits an explicitly long wait", func(t *testing.T) {
		output, cast := run(t, "WAIT_CHILD_EXIT@3000\nASSERT_EXIT 0\n", "sleep 2; printf done", true)
		if !strings.Contains(output, "process exited 0") {
			t.Fatalf("missing successful exit report:\n%s", output)
		}
		for _, want := range []string{
			"STEP_START line 1: WAIT_CHILD_EXIT@3000",
			"STEP_OK line 1: WAIT_CHILD_EXIT@3000",
			"STEP_START line 2: ASSERT_EXIT 0",
			"STEP_OK line 2: ASSERT_EXIT 0",
		} {
			if !strings.Contains(cast, want) {
				t.Fatalf("cast is missing lifecycle marker %q:\n%s", want, cast)
			}
		}
		if strings.LastIndex(cast, "SESSION_END status=success exit_code=0") < strings.LastIndex(cast, "STEP_OK line 2: ASSERT_EXIT 0") {
			t.Fatalf("SESSION_END is not the final lifecycle marker:\n%s", cast)
		}
	})

	t.Run("global timeout bounds wait", func(t *testing.T) {
		output, cast := run(t, "WAIT_CHILD_EXIT\n", "sleep 2; printf done", false)
		if !strings.Contains(output, "WAIT_CHILD_EXIT: timeout after 1s") {
			t.Fatalf("missing timeout report:\n%s", output)
		}
		if !strings.Contains(cast, "STEP_FAILED line 1: WAIT_CHILD_EXIT") {
			t.Fatalf("cast is missing timeout marker:\n%s", cast)
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
		if !strings.Contains(cast, "SESSION_END status=success exit_code=7") {
			t.Fatalf("successful nonzero assertion has inconsistent session outcome:\n%s", cast)
		}
	})
}

func TestDriveSignalFinalizesAbortedRecording(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "trec")
	if output, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build trec: %v\n%s", err, output)
	}
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "signal.drive")
	castPath := filepath.Join(dir, "signal.cast")
	if err := os.WriteFile(scriptPath, []byte("WAIT_CHILD_EXIT@60000\nASSERT_EXIT 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "drive", "--script", scriptPath, "-o", castPath, "--", "sh", "-c", "printf ready; sleep 30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		data, _ := os.ReadFile(castPath)
		if strings.Contains(string(data), "STEP_START line 1: WAIT_CHILD_EXIT@60000") {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatalf("drive did not enter WAIT_CHILD_EXIT:\n%s", data)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("interrupted drive unexpectedly exited 0")
	}

	data, err := os.ReadFile(resultPath(castPath))
	if err != nil {
		t.Fatal(err)
	}
	var result sessionResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "aborted" || result.Cast.Complete == false || !result.Cast.SessionEnd || result.LastStep == nil || result.LastStep.Phase != "failed" {
		t.Fatalf("aborted result = %#v", result)
	}
}

func TestDriveOutcomeConsistencyAndExplicitEndSession(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "trec")
	if output, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build trec: %v\n%s", err, output)
	}

	readResult := func(t *testing.T, castPath string) sessionResult {
		t.Helper()
		data, err := os.ReadFile(resultPath(castPath))
		if err != nil {
			t.Fatal(err)
		}
		var result sessionResult
		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatal(err)
		}
		return result
	}

	t.Run("interactive nonzero child is failed everywhere", func(t *testing.T) {
		castPath := filepath.Join(t.TempDir(), "interactive.cast")
		cmd := exec.Command(binary, "drive", "--interactive", "-o", castPath, "--", "sh", "-c", "exit 7")
		cmd.Stdin = strings.NewReader("")
		output, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("interactive drive unexpectedly exited 0:\n%s", output)
		}
		result := readResult(t, castPath)
		if result.Status != "failed" || result.ExitCode != 7 || result.Mode != "interactive" ||
			result.Termination == nil || result.Termination.Kind != "child_exit" {
			t.Fatalf("interactive result = %#v", result)
		}
		cast, err := os.ReadFile(castPath)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(cast), "SESSION_END status=failed exit_code=7") {
			t.Fatalf("cast outcome disagrees with result:\n%s", cast)
		}
	})

	t.Run("step failure preserves child exit code", func(t *testing.T) {
		dir := t.TempDir()
		scriptPath := filepath.Join(dir, "steps.drive")
		castPath := filepath.Join(dir, "steps.cast")
		if err := os.WriteFile(scriptPath, []byte("EXPECT never-appears\nEND_SESSION\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(binary, "drive", "--script", scriptPath, "-o", castPath, "--", "sh", "-c", "exit 7")
		if output, err := cmd.CombinedOutput(); err == nil {
			t.Fatalf("failing step unexpectedly exited 0:\n%s", output)
		}
		result := readResult(t, castPath)
		if result.Status != "failed" || result.ExitCode != 7 ||
			result.Termination == nil || result.Termination.Kind != "step_failure" {
			t.Fatalf("step failure result = %#v", result)
		}
	})

	t.Run("END_SESSION terminates immediately with provenance", func(t *testing.T) {
		dir := t.TempDir()
		scriptPath := filepath.Join(dir, "end.drive")
		castPath := filepath.Join(dir, "end.cast")
		if err := os.WriteFile(scriptPath, []byte("END_SESSION\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, "drive", "--script", scriptPath, "-o", castPath, "--", "sleep", "30")
		started := time.Now()
		if output, err := cmd.CombinedOutput(); err == nil {
			t.Fatalf("END_SESSION unexpectedly exited 0:\n%s", output)
		}
		if ctx.Err() != nil || time.Since(started) > 2*time.Second {
			t.Fatalf("END_SESSION did not terminate promptly: %v", ctx.Err())
		}
		result := readResult(t, castPath)
		if result.Status != "ended" || result.Mode != "script" ||
			result.Timeouts == nil || result.Timeouts.ChildExitMS != 120000 ||
			result.Termination == nil || result.Termination.Kind != "operator_terminated" ||
			result.Termination.Disposition != "script_ended" {
			t.Fatalf("END_SESSION result = %#v", result)
		}
		if result.Cast.SessionEndStatus != "ended" {
			t.Fatalf("cast SESSION_END status = %q, want ended", result.Cast.SessionEndStatus)
		}
	})
}

func TestDriveSnapshotPersistsRedactedScreenAndLabel(t *testing.T) {
	redactor, err := newSecretRedactor(nil)
	if err != nil {
		t.Fatal(err)
	}
	ts := &terminalSession{
		start:    time.Now(),
		cols:     80,
		rows:     24,
		vt:       vt10x.New(vt10x.WithSize(80, 24)),
		redactor: redactor,
	}
	ts.feedOutput([]byte("saved successfully"))
	ds := &driveSession{ts: ts, redactor: redactor}
	step, err := parseDriveLine("SNAPSHOT hosts saved", 7)
	if err != nil {
		t.Fatal(err)
	}
	if err := ds.applyStep(context.Background(), step); err != nil {
		t.Fatal(err)
	}
	result := ds.result("success", 0, "")
	if len(result.Snapshots) != 1 {
		t.Fatalf("snapshots = %#v, want one", result.Snapshots)
	}
	snapshot := result.Snapshots[0]
	if snapshot.Label != "hosts saved" || len(snapshot.Screen) != 1 || snapshot.Screen[0] != "saved successfully" {
		t.Fatalf("snapshot = %#v", snapshot)
	}
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

func TestDriveRespondResultRedactsCurrentScreenNotHistoricalTaint(t *testing.T) {
	os.Setenv("TEST_DRIVE_TAINT", "mysecret")
	defer os.Unsetenv("TEST_DRIVE_TAINT")

	redactor, _ := newSecretRedactor([]string{"TEST_DRIVE_TAINT"})
	dummyIn, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	defer dummyIn.Close()
	ts := newTerminalSession(dummyIn, strings.NewReader(""), nil, 80, 24, nil, redactor, false, nil)
	ts.tainted = true // A secret appeared earlier, but the current VT screen is empty.
	ds := &driveSession{
		ts:       ts,
		redactor: redactor,
	}

	// Just check the result JSON
	res := ds.result("success", 0, "ok")
	if len(res.FinalScreen) != 0 {
		t.Fatalf("expected empty current screen, got %v", res.FinalScreen)
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
	if strings.Contains(out, "<screen redacted>") {
		t.Fatalf("historical taint redacted an empty current screen: %s", out)
	}
}

// TestParseExpectFreshVariants checks that EXPECT_FRESH and EXPECT_FRESH_REGEX
// parse with the correct kind and timeout.
func TestParseExpectFreshVariants(t *testing.T) {
	cases := []struct {
		line     string
		wantKind string
		wantN    int
		wantHasN bool
	}{
		{`EXPECT_FRESH "PLAY RECAP"`, "expect_fresh", 0, false},
		{`EXPECT_FRESH@30000 PLAY RECAP`, "expect_fresh", 30000, true},
		{`EXPECT_FRESH_REGEX "ok=\d+"`, "expect_fresh_regex", 0, false},
		{`EXPECT_FRESH_REGEX@5000 ok=`, "expect_fresh_regex", 5000, true},
	}
	for _, c := range cases {
		step, err := parseDriveLine(c.line, 1)
		if err != nil {
			t.Errorf("parse %q: %v", c.line, err)
			continue
		}
		if step.kind != c.wantKind {
			t.Errorf("parse %q: kind = %q, want %q", c.line, step.kind, c.wantKind)
		}
		if step.n != c.wantN {
			t.Errorf("parse %q: n = %d, want %d", c.line, step.n, c.wantN)
		}
		if step.hasN != c.wantHasN {
			t.Errorf("parse %q: hasN = %t, want %t", c.line, step.hasN, c.wantHasN)
		}
	}
}

// TestLintCatchesWaitExpectStaleViewport verifies the lint rule that
// flags a `WAIT <ms>` followed by an EXPECT that re-uses a substring
// already seen earlier in the script — the r11 anti-pattern.
func TestLintCatchesWaitExpectStaleViewport(t *testing.T) {
	parse := func(lines ...string) []*driveStep {
		t.Helper()
		var steps []*driveStep
		for i, l := range lines {
			s, err := parseDriveLine(l, i+1)
			if err != nil {
				t.Fatalf("parse %q: %v", l, err)
			}
			steps = append(steps, s)
		}
		return steps
	}
	// Plain `WAIT 5000` + `EXPECT PLAY RECAP` repeats the same text that
	// appeared at step 1 after a WAIT — lint must flag it.
	bad := lintDriveSteps(parse(
		"EXPECT PLAY RECAP",
		"TEXT_AND_ENTER y",
		"WAIT 5000",
		"EXPECT PLAY RECAP",
	), true)
	found := false
	for _, f := range bad {
		if f.Level == "error" && strings.Contains(f.Message, "repeats a substring") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected repeat-substring-after-WAIT warning, got %#v", bad)
	}
	// EXPECT_FRESH variant should NOT trip the same rule.
	good := lintDriveSteps(parse(
		"EXPECT PLAY RECAP",
		"TEXT_AND_ENTER y",
		"WAIT 5000",
		"EXPECT_FRESH PLAY RECAP",
	), true)
	for _, f := range good {
		if f.Level == "error" && strings.Contains(f.Message, "repeats a substring") {
			t.Errorf("EXPECT_FRESH should not trip the rule, got: %#v", good)
		}
	}
}
