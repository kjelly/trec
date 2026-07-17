package main

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type mockWriter struct {
	mu          sync.Mutex
	writeCount  int
	failOnWrite int
	returnErr   error
	returnN     int
	written     []byte
}

func (m *mockWriter) Write(p []byte) (n int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writeCount++
	if m.failOnWrite > 0 && m.writeCount >= m.failOnWrite {
		if m.returnN > 0 {
			m.written = append(m.written, p[:m.returnN]...)
			return m.returnN, m.returnErr
		}
		return 0, m.returnErr
	}
	m.written = append(m.written, p...)
	return len(p), nil
}

func (m *mockWriter) Close() error {
	return nil
}

func TestTerminalSessionPartialWriter(t *testing.T) {
	in := &mockWriter{
		failOnWrite: 3, // Fail on the 3rd byte
		returnErr:   errors.New("mock io error"),
		returnN:     0,
	}

	var castBuf bytes.Buffer
	bw := bufio.NewWriter(&castBuf)
	var mu sync.Mutex
	redactor := &secretRedactor{}
	recorder := newRecordingWriter(bw, &mu, redactor)

	ts := &terminalSession{
		in:       in,
		start:    time.Now(),
		recorder: recorder,
		cols:     80,
		rows:     24,
	}

	n, err := ts.sendTextLocked("hello", "", 0)
	if err == nil || !strings.Contains(err.Error(), "mock io error") {
		t.Fatalf("expected mock io error, got %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 bytes written, got %d", n)
	}
	recorder.flush()

	castStr := castBuf.String()
	if !strings.Contains(castStr, `"i","he"`) {
		t.Fatalf("expected cast to record partial write 'he', got %s", castStr)
	}
}

func TestTerminalSessionShortWrite(t *testing.T) {
	in := &mockWriter{
		failOnWrite: 2, // Fail on the 2nd byte
		returnErr:   nil,
		returnN:     0, // returns 0, nil which should become io.ErrShortWrite since we expect 1 byte per rune here
	}

	var castBuf bytes.Buffer
	bw := bufio.NewWriter(&castBuf)
	var mu sync.Mutex
	redactor := &secretRedactor{}
	recorder := newRecordingWriter(bw, &mu, redactor)

	ts := &terminalSession{
		in:       in,
		start:    time.Now(),
		recorder: recorder,
		cols:     80,
		rows:     24,
	}

	n, err := ts.sendTextLocked("world", "MY_ENV", 0)
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("expected io.ErrShortWrite, got %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 byte written, got %d", n)
	}
	recorder.flush()

	castStr := castBuf.String()
	if !strings.Contains(castStr, `"i","\u003credacted:MY_ENV partial 1/5\u003e"`) {
		t.Fatalf("expected cast to record partial redaction marker, got %s", castStr)
	}
}

func TestSendTextFilePartialWrite(t *testing.T) {
	in := &mockWriter{
		failOnWrite: 3, // Fail on the 3rd byte (since it writes rune by rune, 2 bytes succeed)
		returnErr:   io.ErrShortWrite,
		returnN:     0,
	}
	castBuf := &bytes.Buffer{}
	var mu sync.Mutex
	redactor := &secretRedactor{}
	recorder := newRecordingWriter(bufio.NewWriter(castBuf), &mu, redactor)

	dummyOut, _ := os.Open(os.DevNull)
	defer dummyOut.Close()
	ts := newTerminalSession(in, dummyOut, nil, 80, 24, recorder, redactor, false, nil)

	// Simulating TEXT_FILE which passes value, "file"
	n, err := ts.sendText("abcdef", "file", 0)
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("expected io.ErrShortWrite, got %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 bytes written, got %d", n)
	}
	recorder.flush()

	castStr := castBuf.String()
	if !strings.Contains(castStr, `"i","\u003credacted:file partial 2/6\u003e"`) {
		t.Fatalf("expected cast to record file partial redaction marker, got %s", castStr)
	}
	if strings.Contains(castStr, "ab") {
		t.Fatalf("cast should not contain the secret prefix 'ab', got %s", castStr)
	}
}

func TestInputStreamingRedaction(t *testing.T) {
	os.Setenv("TEST_STREAM_SECRET", "abcdef")
	defer os.Unsetenv("TEST_STREAM_SECRET")

	in := &mockWriter{}
	var castBuf bytes.Buffer
	bw := bufio.NewWriter(&castBuf)
	var mu sync.Mutex
	redactor, _ := newSecretRedactor([]string{"TEST_STREAM_SECRET"})
	recorder := newRecordingWriter(bw, &mu, redactor)

	dummyOut, _ := os.Open(os.DevNull)
	defer dummyOut.Close()
	ts := newTerminalSession(in, dummyOut, nil, 80, 24, recorder, redactor, false, nil)

	// Write secret in chunks
	ts.sendBytes([]byte("abc"), "")
	ts.sendBytes([]byte("def"), "")
	ts.close()

	castStr := castBuf.String()
	if strings.Contains(castStr, `"i","abc"`) || strings.Contains(castStr, `"i","def"`) || strings.Contains(castStr, `"i","abcdef"`) {
		t.Fatalf("cast leaked input secret fragments or whole: %s", castStr)
	}
	if !strings.Contains(castStr, `"i","\u003credacted:TEST_STREAM_SECRET\u003e"`) {
		t.Fatalf("cast missing redacted input marker: %s", castStr)
	}
}

func TestEventOrderingWithSecrets(t *testing.T) {
	os.Setenv("TEST_ORDER_SECRET", "abcdef")
	defer os.Unsetenv("TEST_ORDER_SECRET")

	in := &mockWriter{}
	var castBuf bytes.Buffer
	bw := bufio.NewWriter(&castBuf)
	var mu sync.Mutex
	redactor, _ := newSecretRedactor([]string{"TEST_ORDER_SECRET"})
	recorder := newRecordingWriter(bw, &mu, redactor)
	_ = recorder.writeHeader(castHeader{Version: 2, Width: 80, Height: 24})

	dummyOutR, dummyOutW, _ := os.Pipe()
	defer dummyOutR.Close()
	defer dummyOutW.Close()

	ts := newTerminalSession(in, dummyOutR, nil, 80, 24, recorder, redactor, false, nil)

	ts.sendBytes([]byte("x\n"), "")
	time.Sleep(50 * time.Millisecond)
	recorder.flushOutput()
	recorder.flushInput()
	recorder.flush()

	castStr := castBuf.String()
	_, events, err := loadCastFileFromBytes(castStr)
	if err != nil {
		t.Fatalf("failed to parse cast: %v\nraw:\n%s", err, castStr)
	}

	for i := 1; i < len(events); i++ {
		if events[i].sec < events[i-1].sec {
			t.Fatalf("non-monotonic timestamps at index %d: %.9f < %.9f", i, events[i].sec, events[i-1].sec)
		}
	}

	lastInputIdx := -1
	firstEchoIdx := -1
	for i, e := range events {
		if e.typ == "i" {
			lastInputIdx = i
		}
		if e.typ == "o" && strings.Contains(e.data, "x") && firstEchoIdx < 0 {
			firstEchoIdx = i
		}
	}
	if lastInputIdx >= 0 && firstEchoIdx >= 0 && lastInputIdx > firstEchoIdx {
		t.Fatalf("input event (idx %d) appeared after its echo output (idx %d)", lastInputIdx, firstEchoIdx)
	}
}

func loadCastFileFromBytes(data string) (castHeader, []castEvent, error) {
	tmpFile, err := os.CreateTemp("", "cast-test-*.cast")
	if err != nil {
		return castHeader{}, nil, err
	}
	defer os.Remove(tmpFile.Name())
	tmpFile.WriteString(data)
	tmpFile.Close()
	return loadCastFile(tmpFile.Name())
}
