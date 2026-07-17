package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/hinshun/vt10x"
	"github.com/mark3labs/mcp-go/mcp"
)

func TestMCPTerminalSize(t *testing.T) {
	for _, tc := range []struct {
		name      string
		input     string
		wantCols  uint16
		wantRows  uint16
		wantError string
	}{
		{name: "defaults", input: `{}`, wantCols: defaultMCPCols, wantRows: defaultMCPRows},
		{name: "custom", input: `{"cols":200,"rows":60}`, wantCols: 200, wantRows: 60},
		{name: "zero", input: `{"cols":0}`, wantError: "cols must be between"},
		{name: "negative", input: `{"rows":-1}`, wantError: "rows must be between"},
		{name: "too large", input: `{"cols":1001}`, wantError: "cols must be between"},
		{name: "fractional", input: `{"cols":80.5}`, wantError: "decode terminal size"},
		{name: "wrong type", input: `{"rows":"40"}`, wantError: "decode terminal size"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			size, err := mcpTerminalSize(json.RawMessage(tc.input))
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("mcpTerminalSize() error = %v, want containing %q", err, tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if size.Cols != tc.wantCols || size.Rows != tc.wantRows {
				t.Fatalf("size = %dx%d, want %dx%d", size.Cols, size.Rows, tc.wantCols, tc.wantRows)
			}
		})
	}
}

// TestMCPServerAdvertisesArrayItemSchema guards against a real
// regression: mcp.WithArray("command", mcp.Required()) alone produces
// a bare {"type":"array"} schema with no "items" — valid JSON Schema,
// but MCP clients that validate/convert tool schemas before exposing
// them (observed with Claude Code) silently dropped both "run" and
// "terminal_start" entirely rather than surfacing an error, since
// neither ever appeared in the client's own tool listing despite the
// server responding correctly to a raw tools/list probe. Every array
// property in this server's tool schemas must declare its item type.
func TestMCPServerAdvertisesArrayItemSchema(t *testing.T) {
	server := newMCPProtocolServer(&mcpServer{sessions: map[string]*terminalSession{}})
	for _, toolName := range []string{"run", "terminal_start"} {
		tool := server.GetTool(toolName)
		if tool == nil {
			t.Fatalf("%s tool is missing", toolName)
		}
		property, ok := tool.Tool.InputSchema.Properties["command"].(map[string]any)
		if !ok {
			t.Fatalf("%s property %q is missing", toolName, "command")
		}
		if property["type"] != "array" {
			t.Fatalf("%s command type = %v, want array", toolName, property["type"])
		}
		items, ok := property["items"].(map[string]any)
		if !ok {
			t.Fatalf("%s command schema has no \"items\" (this is exactly the bug that made Claude Code silently drop the tool): %#v", toolName, property)
		}
		if items["type"] != "string" {
			t.Fatalf("%s command items type = %v, want string", toolName, items["type"])
		}
	}
}

func TestMCPServerAdvertisesTerminalSize(t *testing.T) {
	server := newMCPProtocolServer(&mcpServer{sessions: map[string]*terminalSession{}})
	tool := server.GetTool("terminal_start")
	if tool == nil {
		t.Fatal("terminal_start tool is missing")
	}
	for _, name := range []string{"cols", "rows"} {
		property, ok := tool.Tool.InputSchema.Properties[name].(map[string]any)
		if !ok {
			t.Fatalf("terminal_start property %q is missing", name)
		}
		if property["type"] != "integer" {
			t.Fatalf("terminal_start property %q type = %v, want integer", name, property["type"])
		}
		if property["minimum"] != 1 || property["maximum"] != maxMCPDimension {
			t.Fatalf("terminal_start property %q bounds = %v..%v", name, property["minimum"], property["maximum"])
		}
		wantDefault := defaultMCPCols
		if name == "rows" {
			wantDefault = defaultMCPRows
		}
		if property["default"] != wantDefault {
			t.Fatalf("terminal_start property %q default = %v, want %d", name, property["default"], wantDefault)
		}
	}
}

func TestMCPTerminalIntegration(t *testing.T) {
	s := &mcpServer{sessions: map[string]*terminalSession{}}

	// Test start
	res, err := s.tool(context.Background(), "terminal_start", []byte(`{"command":["sh","-c","echo hello; sleep 1"]}`))
	if err != nil {
		t.Fatalf("start failed: %v", err)
	}
	m, _ := res.(map[string]string)
	sessionID := m["session_id"]
	if sessionID == "" {
		t.Fatalf("missing session id")
	}

	// Test read_screen
	// wait for 'hello' to appear
	ss, _ := s.session(sessionID)
	ss.waitForText(context.Background(), "EXPECT", "hello", 2*time.Second)

	res2, err := s.tool(context.Background(), "terminal_read_screen", []byte(`{"session_id":"`+sessionID+`"}`))
	if err != nil {
		t.Fatalf("read_screen failed: %v", err)
	}
	m2, _ := res2.(map[string]any)
	screen, _ := m2["screen"].([]string)
	if len(screen) == 0 || !strings.Contains(screen[0], "hello") {
		t.Fatalf("screen missing hello: %v", screen)
	}

	// Test expect
	_, err = s.tool(context.Background(), "terminal_expect", []byte(`{"session_id":"`+sessionID+`","text":"hello","timeout_seconds":1.0}`))
	if err != nil {
		t.Fatalf("expect failed: %v", err)
	}

	// Test expect timeout
	_, err = s.tool(context.Background(), "terminal_expect", []byte(`{"session_id":"`+sessionID+`","text":"world","timeout_seconds":0.1}`))
	if err == nil {
		t.Fatalf("expect should have timed out")
	}

	// Test wait_quiet
	_, err = s.tool(context.Background(), "terminal_wait_quiet", []byte(`{"session_id":"`+sessionID+`","quiet_duration_seconds":0.1,"timeout_seconds":1.0}`))
	if err != nil {
		t.Fatalf("wait_quiet failed: %v", err)
	}

	// Test invalid parameters (<= 0 validations)
	_, err = s.tool(context.Background(), "terminal_expect", []byte(`{"session_id":"`+sessionID+`","text":"hello","timeout_seconds":0}`))
	if err == nil || !strings.Contains(err.Error(), "must be greater than zero") {
		t.Fatalf("expect with timeout_seconds=0 should fail validation, got: %v", err)
	}

	_, err = s.tool(context.Background(), "terminal_wait_quiet", []byte(`{"session_id":"`+sessionID+`","quiet_duration_seconds":0,"timeout_seconds":1.0}`))
	if err == nil || !strings.Contains(err.Error(), "must be greater than zero") {
		t.Fatalf("wait_quiet with quiet_duration_seconds=0 should fail validation, got: %v", err)
	}

	_, err = s.tool(context.Background(), "terminal_wait_quiet", []byte(`{"session_id":"`+sessionID+`","quiet_duration_seconds":0.1,"timeout_seconds":0}`))
	if err == nil || !strings.Contains(err.Error(), "must be greater than zero") {
		t.Fatalf("wait_quiet with timeout_seconds=0 should fail validation, got: %v", err)
	}
}

func TestMCPTimeoutSecretRedaction(t *testing.T) {
	s := &mcpServer{sessions: map[string]*terminalSession{}}

	tmpCast := filepath.Join(t.TempDir(), "test_mcp_secret.cast")
	defer os.Remove(tmpCast)

	t.Setenv("MYSECRET", "super-secret-value")

	req := map[string]any{
		"command":     []string{"sh", "-c", "echo $MYSECRET; sleep 2"},
		"secret_env":  []string{"MYSECRET"},
		"record_file": tmpCast,
		"cols":        5, // Narrow enough to force "super-secret-value" across lines
	}
	reqBytes, _ := json.Marshal(req)

	res, err := s.tool(context.Background(), "terminal_start", reqBytes)
	if err != nil {
		t.Fatalf("start failed: %v", err)
	}
	m, _ := res.(map[string]string)
	sessionID := m["session_id"]
	defer func() {
		ss, _ := s.session(sessionID)
		if ss != nil {
			ss.close()
		}
	}()

	_, err = s.tool(context.Background(), "terminal_expect", []byte(`{"session_id":"`+sessionID+`","text":"impossible-string","timeout_seconds":0.5}`))
	if err == nil {
		t.Fatalf("expect should have timed out")
	}

	errMsg := err.Error()
	if strings.Contains(errMsg, "super-secret-value") {
		t.Fatalf("MCP terminal_expect error leaked secret: %s", errMsg)
	}
	if !strings.Contains(errMsg, "<screen redacted>") {
		t.Fatalf("MCP terminal_expect error did not contain redacted placeholder: %s", errMsg)
	}

	resScreen, err := s.tool(context.Background(), "terminal_read_screen", []byte(`{"session_id":"`+sessionID+`"}`))
	if err != nil {
		t.Fatalf("terminal_read_screen failed: %v", err)
	}
	if !strings.Contains(fmt.Sprint(resScreen), "<screen redacted>") {
		t.Fatalf("terminal_read_screen did not redact: %v", resScreen)
	}

	// Test errors.Is context.Canceled unwrapping
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, errCtx := s.tool(ctx, "terminal_expect", []byte(`{"session_id":"`+sessionID+`","text":"impossible-string","timeout_seconds":0.5}`))
	if !errors.Is(errCtx, context.Canceled) {
		t.Fatalf("Expected context.Canceled, got %v", errCtx)
	}
}

func TestMCPRecordingFinalizeAndTruncate(t *testing.T) {
	s := &mcpServer{sessions: map[string]*terminalSession{}}

	// Create temporary record file path
	tmpCast := filepath.Join(t.TempDir(), "test_mcp_finalize.cast")
	defer os.Remove(tmpCast)

	// Test start with recording
	req := map[string]any{
		"command":      []string{"sh", "-c", "echo hello; sleep 0.1; echo world"},
		"record_file":  tmpCast,
		"record_title": "mcp-test",
	}
	reqBytes, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("failed to marshal start request: %v", err)
	}

	res, err := s.tool(context.Background(), "terminal_start", reqBytes)
	if err != nil {
		t.Fatalf("start with recording failed: %v", err)
	}
	m, _ := res.(map[string]string)
	sessionID := m["session_id"]

	ss, _ := s.session(sessionID)
	// Wait for program to naturally exit
	_, _ = ss.waitChildExit(context.Background())

	// Since the program exited naturally, the recording should have been finalized!
	// Let's read the cast file.
	castBytes, err := os.ReadFile(tmpCast)
	if err != nil {
		t.Fatalf("failed to read cast file: %v", err)
	}
	content := string(castBytes)
	if !strings.Contains(content, `"width":`) || !strings.Contains(content, `mcp-test`) {
		t.Fatalf("cast header was not written or finalized correctly: %s", content)
	}
	if !strings.Contains(content, "world") {
		t.Fatalf("cast content was not finalized correctly (missing final outputs): %s", content)
	}

	// 2. Test keepRaw buffer truncation
	s2 := &mcpServer{sessions: map[string]*terminalSession{}}
	// Start shell to print large data
	res2, err := s2.tool(context.Background(), "terminal_start", []byte(`{"command":["sh","-c","dd if=/dev/zero bs=1024 count=1100 | tr '\\000' 'A'"]}`))
	if err != nil {
		t.Fatalf("failed to start s2: %v", err)
	}
	m2, _ := res2.(map[string]string)
	sID2 := m2["session_id"]
	ss2, _ := s2.session(sID2)
	_, _ = ss2.waitChildExit(context.Background())

	// Read and verify truncated flag
	resRead, err := s2.tool(context.Background(), "terminal_read", []byte(`{"session_id":"`+sID2+`"}`))
	if err != nil {
		t.Fatalf("failed to read raw: %v", err)
	}
	mRead, _ := resRead.(map[string]any)
	trunc, _ := mRead["truncated"].(bool)
	stdout, _ := mRead["stdout"].(string)
	if !trunc {
		t.Fatalf("expected raw output to be truncated")
	}
	if len(stdout) > 600*1024 { // Should be capped around 512KB
		t.Fatalf("expected stdout length to be capped around 512KB, got %d", len(stdout))
	}
}

func TestMCPWriteAfterClose(t *testing.T) {
	s := &mcpServer{sessions: map[string]*terminalSession{}}
	res, err := s.tool(context.Background(), "terminal_start", []byte(`{"command":["sleep","10"]}`))
	if err != nil {
		t.Fatalf("failed to start: %v", err)
	}
	m, _ := res.(map[string]string)
	sessionID := m["session_id"]

	ss, err := s.session(sessionID)
	if err != nil {
		t.Fatalf("failed to look up session: %v", err)
	}

	// Now close the session
	_, err = s.tool(context.Background(), "terminal_close", []byte(`{"session_id":"`+sessionID+`"}`))
	if err != nil {
		t.Fatalf("failed to close session: %v", err)
	}

	// Now writing via MCP tool should fail because it's deleted
	_, err = s.tool(context.Background(), "terminal_write", []byte(`{"session_id":"`+sessionID+`","data":"hello"}`))
	if err == nil {
		t.Fatalf("expected error writing to closed session via tool, got nil")
	}

	// Direct sendBytes should fail with "session is closed"
	_, err = ss.sendBytes([]byte("hello"), "")
	if err == nil || !strings.Contains(err.Error(), "session is closed") {
		t.Fatalf("expected 'session is closed' error, got: %v", err)
	}
}

func TestMCPConcurrentCloseAndWrite(t *testing.T) {
	s := &mcpServer{sessions: map[string]*terminalSession{}}
	res, err := s.tool(context.Background(), "terminal_start", []byte(`{"command":["sleep","10"]}`))
	if err != nil {
		t.Fatalf("failed to start: %v", err)
	}
	m, _ := res.(map[string]string)
	sessionID := m["session_id"]

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		// Try writing multiple times
		for i := 0; i < 100; i++ {
			_, _ = s.tool(context.Background(), "terminal_write", []byte(`{"session_id":"`+sessionID+`","data":"a"}`))
		}
	}()
	go func() {
		defer wg.Done()
		time.Sleep(1 * time.Millisecond)
		_, _ = s.tool(context.Background(), "terminal_close", []byte(`{"session_id":"`+sessionID+`"}`))
	}()
	wg.Wait()
}

type syncWriteCloser struct {
	writeStarted chan struct{}
	writeBlock   chan struct{}
	closeCalled  chan struct{}
}

func (sw *syncWriteCloser) Write(p []byte) (int, error) {
	if sw.writeStarted != nil {
		sw.writeStarted <- struct{}{}
	}
	if sw.writeBlock != nil {
		<-sw.writeBlock
	}
	return len(p), nil
}

func (sw *syncWriteCloser) Close() error {
	if sw.closeCalled != nil {
		sw.closeCalled <- struct{}{}
	}
	return nil
}

func TestMCPNaturalExitAndWriteConcurrency(t *testing.T) {
	// 時序 1：Write 進行中，Teardown 嘗試獲取鎖 (Teardown 被 Block)
	s := &mcpServer{sessions: map[string]*terminalSession{}}
	tmpCast := filepath.Join(t.TempDir(), "natural_exit_write.cast")
	f, err := os.OpenFile(tmpCast, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("failed to open cast: %v", err)
	}
	bw := bufio.NewWriterSize(f, 1024)
	var bwMu sync.Mutex
	redactor := &secretRedactor{}
	recorder := newRecordingWriter(bw, &bwMu, redactor)
	_ = recorder.writeHeader(castHeader{Version: 2})

	sw := &syncWriteCloser{
		writeStarted: make(chan struct{}, 1),
		writeBlock:   make(chan struct{}),
	}

	blockReader, blockWriter, _ := os.Pipe()
	defer blockReader.Close()
	defer blockWriter.Close()

	// 手動構造 ts。我們不需要真實進程
	ts := newTerminalSession(sw, blockReader, nil, 80, 24, recorder, nil, false, []io.Closer{f})
	sessionID := "session-race-1"
	s.sessions[sessionID] = ts

	var wg sync.WaitGroup
	wg.Add(2)

	var writeErr error
	go func() {
		defer wg.Done()
		_, writeErr = s.tool(context.Background(), "terminal_write", []byte(`{"session_id":"`+sessionID+`","data":"special-pattern-race"}`))
	}()

	go func() {
		defer wg.Done()
		// 等待 write 進入並且拿到了 writeMu 鎖
		<-sw.writeStarted

		// 此時 teardown 嘗試鎖 writeMu，這將會被 block 住
		ts.teardown()
	}()

	// 給 teardown goroutine 一點點時間進入 ts.teardown() 的 writeMu.Lock()
	time.Sleep(10 * time.Millisecond)

	// 現在釋放 write 阻塞
	close(sw.writeBlock)

	wg.Wait()

	if writeErr != nil {
		t.Fatalf("expected write to succeed since it acquired the lock before teardown, got: %v", writeErr)
	}

	// 驗證特殊字串是否確實被錄影寫入
	_ = recorder.flushOutput()
	_ = recorder.flush()
	castBytes, err := os.ReadFile(tmpCast)
	if err != nil {
		t.Fatalf("failed to read cast file: %v", err)
	}
	content := string(castBytes)
	if !strings.Contains(content, "special-pattern-race") {
		t.Fatalf("write returned success, but data was not written to cast: %s", content)
	}

	// 時序 2：Teardown 先拿到鎖，隨後的 Write 被拒絕
	s2 := &mcpServer{sessions: map[string]*terminalSession{}}
	sw2 := &syncWriteCloser{}
	blockReader2, blockWriter2, _ := os.Pipe()
	defer blockReader2.Close()
	defer blockWriter2.Close()
	ts2 := newTerminalSession(sw2, blockReader2, nil, 80, 24, nil, nil, false, nil)
	sessionID2 := "session-race-2"
	s2.sessions[sessionID2] = ts2

	ts2.teardown()

	_, writeErr2 := s2.tool(context.Background(), "terminal_write", []byte(`{"session_id":"`+sessionID2+`","data":"any"}`))
	if writeErr2 == nil {
		t.Fatalf("expected write to fail after teardown, got nil")
	}
	if !strings.Contains(writeErr2.Error(), "session is closed") {
		t.Fatalf("expected 'session is closed' error, got: %v", writeErr2)
	}
}

type blockCloser struct {
	mu     sync.Mutex
	closed bool
	block  chan struct{}
}

func (bc *blockCloser) Close() error {
	bc.mu.Lock()
	bc.closed = true
	bc.mu.Unlock()
	<-bc.block
	return nil
}

func TestMCPDoubleCloseTeardownWait(t *testing.T) {
	dummyIn, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	dummyOut, _ := os.Open(os.DevNull)
	defer dummyIn.Close()
	defer dummyOut.Close()

	bc := &blockCloser{block: make(chan struct{})}
	ts := newTerminalSession(dummyIn, dummyOut, nil, 80, 24, nil, nil, false, []io.Closer{bc})

	var wg sync.WaitGroup
	wg.Add(2)

	t1Started := make(chan struct{})
	t2Started := make(chan struct{})

	go func() {
		defer wg.Done()
		close(t1Started)
		_ = ts.close()
	}()

	var t2Finished int32
	go func() {
		defer wg.Done()
		<-t1Started
		time.Sleep(5 * time.Millisecond)
		close(t2Started)
		_ = ts.close()
		atomic.StoreInt32(&t2Finished, 1)
	}()

	<-t2Started
	time.Sleep(20 * time.Millisecond)

	bc.mu.Lock()
	if !bc.closed {
		bc.mu.Unlock()
		t.Fatalf("expected close to be called on blockCloser")
	}
	bc.mu.Unlock()

	if atomic.LoadInt32(&t2Finished) != 0 {
		t.Fatalf("expected second close() to be blocked on teardown, but it returned prematurely!")
	}

	close(bc.block)
	wg.Wait()
}

func TestMCPWaitQuietAfterExit(t *testing.T) {
	s := &mcpServer{sessions: map[string]*terminalSession{}}
	res, err := s.tool(context.Background(), "terminal_start", []byte(`{"command":["echo","hello"]}`))
	if err != nil {
		t.Fatalf("failed to start: %v", err)
	}
	m, _ := res.(map[string]string)
	sessionID := m["session_id"]
	ss, _ := s.session(sessionID)

	_, _ = ss.waitChildExit(context.Background())

	_, err = s.tool(context.Background(), "terminal_wait_quiet", []byte(`{
		"session_id": "`+sessionID+`",
		"quiet_duration_seconds": 0.1,
		"timeout_seconds": 2.0
	}`))
	if err != nil {
		t.Fatalf("wait_quiet failed after child exit: %v", err)
	}
}

type errorCloser struct{}

func (errorCloser) Close() error {
	return fmt.Errorf("mock disk full error")
}

func TestMCPFinalizeErrorPropagation(t *testing.T) {
	dummyIn, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	dummyOut, _ := os.Open(os.DevNull)
	defer dummyIn.Close()
	defer dummyOut.Close()

	ec := errorCloser{}
	ts := newTerminalSession(dummyIn, dummyOut, nil, 80, 24, nil, nil, false, []io.Closer{ec})

	err := ts.close()
	if err == nil {
		t.Fatalf("expected close error, got nil")
	}
	if !strings.Contains(err.Error(), "mock disk full error") {
		t.Fatalf("expected 'mock disk full error' in error, got: %v", err)
	}
}

type failingWriter struct {
	err error
}

func (fw *failingWriter) Write(p []byte) (int, error) {
	if len(p) > 2 {
		return 2, fw.err
	}
	return len(p), fw.err
}

func (fw *failingWriter) Close() error {
	return nil
}

func TestMCPRecorderFailurePropagation(t *testing.T) {
	fw := &failingWriter{err: fmt.Errorf("mock disk full on write")}
	bw := bufio.NewWriterSize(fw, 1024)
	var mu sync.Mutex
	redactor := &secretRedactor{}
	recorder := newRecordingWriter(bw, &mu, redactor)

	dummyIn, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	dummyOut, _ := os.Open(os.DevNull)
	defer dummyIn.Close()
	defer dummyOut.Close()

	ts := newTerminalSession(dummyIn, dummyOut, nil, 80, 24, recorder, nil, false, nil)

	_ = recorder.writeHeader(castHeader{Version: 2})
	_ = recorder.event(0.1, "o", "hello")
	_ = recorder.flush()

	err := ts.close()
	if err == nil {
		t.Fatalf("expected close error from recording writer failure, got nil")
	}
	if !strings.Contains(err.Error(), "mock disk full on write") {
		t.Fatalf("expected 'mock disk full on write' in error, got: %v", err)
	}
}

func TestMCPNaturalExitErrorPropagation(t *testing.T) {
	fw := &failingWriter{err: fmt.Errorf("mock natural exit write failure")}
	bw := bufio.NewWriterSize(fw, 1024)
	var mu sync.Mutex
	redactor := &secretRedactor{}
	recorder := newRecordingWriter(bw, &mu, redactor)

	dummyIn, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	dummyOutR, dummyOutW := io.Pipe()
	defer dummyIn.Close()
	defer dummyOutR.Close()

	ts := newTerminalSession(dummyIn, dummyOutR, nil, 80, 24, recorder, nil, false, nil)

	_ = recorder.writeHeader(castHeader{Version: 2})
	_ = recorder.event(0.1, "o", "hello")
	_ = recorder.flush()

	// Now let the session exit
	dummyOutW.Close()

	processExitErr, finalizeErr := ts.waitChildExit(context.Background())
	if processExitErr != nil {
		t.Fatalf("expected nil processExitErr, got: %v", processExitErr)
	}
	if finalizeErr == nil {
		t.Fatalf("expected non-nil finalizeErr, got nil")
	}
	if !strings.Contains(finalizeErr.Error(), "mock natural exit write failure") {
		t.Fatalf("expected 'mock natural exit write failure' in finalizeErr, got: %v", finalizeErr)
	}
}

func TestMCPTerminalStartHeaderFailure(t *testing.T) {
	// Ignore SIGXFSZ to prevent test process crash when size limit is hit
	signal.Ignore(syscall.SIGXFSZ)
	t.Cleanup(func() {
		signal.Reset(syscall.SIGXFSZ)
	})

	var oldLimit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &oldLimit); err != nil {
		t.Fatalf("failed to get rlimit: %v", err)
	}
	defer syscall.Setrlimit(syscall.RLIMIT_FSIZE, &oldLimit)

	// Set small write limit (e.g. 50 bytes)
	var newLimit syscall.Rlimit
	newLimit.Cur = 50
	newLimit.Max = oldLimit.Max
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &newLimit); err != nil {
		t.Fatalf("failed to set rlimit: %v", err)
	}

	s := &mcpServer{sessions: map[string]*terminalSession{}}
	tmpCast := filepath.Join(t.TempDir(), "fsize_limit.cast")
	req := map[string]any{
		"command":     []string{"echo", "hello"},
		"record_file": tmpCast,
	}
	reqBytes, _ := json.Marshal(req)
	_, err := s.tool(context.Background(), "terminal_start", reqBytes)
	if err == nil {
		t.Fatalf("expected terminal_start to fail due to file size limit, got nil")
	}
	if !strings.Contains(err.Error(), "write header") && !strings.Contains(err.Error(), "file too large") {
		t.Fatalf("expected write header or file too large error, got: %v", err)
	}

	// Ensure no session was registered
	s.mu.Lock()
	l := len(s.sessions)
	s.mu.Unlock()
	if l != 0 {
		t.Fatalf("expected 0 sessions to be registered after start failure, got %d", l)
	}

	// Ensure record file is cleaned up/removed
	if _, err := os.Stat(tmpCast); !os.IsNotExist(err) {
		t.Fatalf("expected record file to be cleaned up, but it still exists")
	}
}

func TestMCPTerminalWriteEventFailure(t *testing.T) {
	fw := &failingWriter{err: fmt.Errorf("mock disk full on write")}
	bw := bufio.NewWriterSize(fw, 1024)
	var mu sync.Mutex
	redactor := &secretRedactor{}
	recorder := newRecordingWriter(bw, &mu, redactor)

	dummyIn, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	defer dummyIn.Close()

	pr, pw, _ := os.Pipe()
	defer pr.Close()
	defer pw.Close()

	ts := newTerminalSession(dummyIn, pr, nil, 80, 24, recorder, nil, false, nil)

	s := &mcpServer{sessions: map[string]*terminalSession{}}
	s.sessions["session-fail-write"] = ts

	_ = recorder.writeHeader(castHeader{Version: 2})
	// Trigger an error state inside recorder
	_ = recorder.event(0.1, "o", "hello")
	_ = recorder.flush()

	// Write command should fail
	_, err := s.tool(context.Background(), "terminal_write", []byte(`{"session_id":"session-fail-write","data":"some data"}`))
	if err == nil {
		t.Fatalf("expected terminal_write to fail due to recorder error, got nil")
	}
	if !strings.Contains(err.Error(), "mock disk full on write") {
		t.Fatalf("expected 'mock disk full on write' in error, got: %v", err)
	}
}

func TestMCPE2EStdio(t *testing.T) {
	oldStdin := os.Stdin
	oldStdout := os.Stdout
	defer func() {
		os.Stdin = oldStdin
		os.Stdout = oldStdout
	}()

	inReader, inWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("failed to create stdin pipe: %v", err)
	}
	defer inReader.Close()
	defer inWriter.Close()

	outReader, outWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("failed to create stdout pipe: %v", err)
	}
	defer outReader.Close()
	defer outWriter.Close()

	os.Stdin = inReader
	os.Stdout = outWriter

	serverDone := make(chan struct{})
	go func() {
		runMCP()
		close(serverDone)
	}()

	// 1. Send Initialize request
	initReq := `{"jsonrpc": "2.0", "method": "initialize", "id": 1, "params": {"protocolVersion": "2024-11-05", "capabilities": {}, "clientInfo": {"name": "test", "version": "1.0"}}}` + "\n"
	_, err = inWriter.Write([]byte(initReq))
	if err != nil {
		t.Fatalf("failed to write initialize request: %v", err)
	}

	scanner := bufio.NewScanner(outReader)
	if !scanner.Scan() {
		t.Fatalf("failed to read initialize response: %v", scanner.Err())
	}

	type jsonrpcMessage struct {
		Jsonrpc string          `json:"jsonrpc"`
		Id      *int            `json:"id,omitempty"`
		Method  string          `json:"method,omitempty"`
		Result  json.RawMessage `json:"result,omitempty"`
		Error   *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}

	var initMsg jsonrpcMessage
	if err := json.Unmarshal(scanner.Bytes(), &initMsg); err != nil {
		t.Fatalf("failed to decode json-rpc initialize response: %v, raw: %s", err, scanner.Text())
	}
	if initMsg.Jsonrpc != "2.0" || initMsg.Id == nil || *initMsg.Id != 1 || initMsg.Result == nil {
		t.Fatalf("invalid json-rpc initialize frame: %s", scanner.Text())
	}

	// 2. Send initialized notification
	initNotif := `{"jsonrpc": "2.0", "method": "notifications/initialized"}` + "\n"
	_, err = inWriter.Write([]byte(initNotif))
	if err != nil {
		t.Fatalf("failed to write initialized notification: %v", err)
	}

	// 3. Send tools/list request
	listReq := `{"jsonrpc": "2.0", "method": "tools/list", "id": 2}` + "\n"
	_, err = inWriter.Write([]byte(listReq))
	if err != nil {
		t.Fatalf("failed to write tools/list request: %v", err)
	}

	if !scanner.Scan() {
		t.Fatalf("failed to read tools/list response: %v", scanner.Err())
	}

	var listMsg jsonrpcMessage
	if err := json.Unmarshal(scanner.Bytes(), &listMsg); err != nil {
		t.Fatalf("failed to decode json-rpc tools/list response: %v", err)
	}
	if listMsg.Jsonrpc != "2.0" || listMsg.Id == nil || *listMsg.Id != 2 {
		t.Fatalf("invalid json-rpc tools/list frame: %s", scanner.Text())
	}
	if !strings.Contains(string(listMsg.Result), `"terminal_select"`) {
		t.Fatalf("expected 'terminal_select' tool in tools/list result, got: %s", string(listMsg.Result))
	}

	// 4. Send call to terminal_select with invalid parameters to test parameter validation over stdio JSON-RPC
	selectCall := `{"jsonrpc": "2.0", "method": "tools/call", "id": 3, "params": {"name": "terminal_select", "arguments": {"session_id": "session-nonexistent", "text": "target", "timeout_seconds": -1.0}}}` + "\n"
	_, err = inWriter.Write([]byte(selectCall))
	if err != nil {
		t.Fatalf("failed to write terminal_select call request: %v", err)
	}

	if !scanner.Scan() {
		t.Fatalf("failed to read terminal_select call response: %v", scanner.Err())
	}

	var selectCallMsg jsonrpcMessage
	if err := json.Unmarshal(scanner.Bytes(), &selectCallMsg); err != nil {
		t.Fatalf("failed to decode json-rpc call response: %v", err)
	}
	if selectCallMsg.Jsonrpc != "2.0" || selectCallMsg.Id == nil || *selectCallMsg.Id != 3 {
		t.Fatalf("invalid json-rpc call response frame: %s", scanner.Text())
	}
	if !strings.Contains(string(selectCallMsg.Result), "timeout_seconds must be greater than zero") &&
		(selectCallMsg.Error == nil || !strings.Contains(selectCallMsg.Error.Message, "timeout_seconds must be greater than zero")) {
		t.Fatalf("expected timeout error in response, got: %s", scanner.Text())
	}

	inWriter.Close()
	outWriter.Close()

	select {
	case <-serverDone:
		// success
	case <-time.After(2 * time.Second):
		t.Fatalf("server did not exit after stdin closed")
	}
}

type mcpSelectWriteCloser struct {
	ts      *terminalSession
	presses int
}

func (sw *mcpSelectWriteCloser) Write(p []byte) (int, error) {
	sw.presses++
	sw.ts.vtMu.Lock()
	if sw.presses == 1 {
		sw.ts.vt.Write([]byte("\x1b[H\x1b[2J  Option 1\r\n❯ Option 2\r\n  Option 3"))
	} else if sw.presses == 2 {
		sw.ts.vt.Write([]byte("\x1b[H\x1b[2J  Option 1\r\n  Option 2\r\n❯ Option 3"))
	}
	sw.ts.vtMu.Unlock()
	return len(p), nil
}

func (sw *mcpSelectWriteCloser) Close() error {
	return nil
}

func TestMCPTerminalSelectBehavior(t *testing.T) {
	sw := &mcpSelectWriteCloser{}
	br, bw, _ := os.Pipe()
	defer br.Close()
	defer bw.Close()

	ts := newTerminalSession(sw, br, nil, 80, 24, nil, nil, false, nil)
	sw.ts = ts

	// Initial screen
	ts.vtMu.Lock()
	ts.vt.Write([]byte("\x1b[H\x1b[2J❯ Option 1\r\n  Option 2\r\n  Option 3"))
	ts.vtMu.Unlock()

	pointerRe := regexp.MustCompile(`^\s*(?:❯|▸|›|→|»|>)\s`)

	// 1. Normal select
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := ts.selectLabel(ctx, "Option 3", pointerRe, 1*time.Millisecond)
	if err != nil {
		t.Fatalf("failed to select Option 3: %v", err)
	}
	if sw.presses != 2 {
		t.Errorf("expected 2 presses, got %d", sw.presses)
	}

	// 2. Nonexistent label - timeout
	sw.presses = 0
	ctx2, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()

	_, err = ts.selectLabel(ctx2, "Nonexistent", pointerRe, 1*time.Millisecond)
	if err == nil {
		t.Fatalf("expected nonexistent select to fail, got nil")
	}
	if !strings.Contains(err.Error(), "not reached") && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected nonexistent select to fail with timeout or not reached, got: %v", err)
	}

	// 3. Empty label - immediate error
	_, err = ts.selectLabel(context.Background(), "", pointerRe, 1*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "non-empty label") {
		t.Fatalf("expected empty label select to fail, got nil")
	}

	// 4. Invalid pointer regex - regexp compile error in select tool level
	s := &mcpServer{sessions: map[string]*terminalSession{}}
	s.sessions["sess-1"] = ts
	_, err = s.tool(context.Background(), "terminal_select", []byte(`{"session_id":"sess-1","text":"Option 3","pointer":"[invalid"}`))
	if err == nil {
		t.Fatalf("expected invalid pointer regex to fail, got nil")
	}
	if !strings.Contains(err.Error(), "bad pointer regexp") {
		t.Errorf("expected bad pointer regexp error, got: %v", err)
	}

	// 5. Multiple visible pointer rows are ambiguous. Selecting the first one
	// can send navigation in the wrong direction when a TUI leaves old rows on
	// the visible screen.
	ts.vtMu.Lock()
	ts.vt.Write([]byte("\x1b[H\x1b[2J❯ Old option\r\n❯ Option 1\r\n  Option 2"))
	ts.vtMu.Unlock()
	_, err = ts.selectLabel(context.Background(), "Option 2", pointerRe, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "ambiguous pointer rows") {
		t.Fatalf("expected ambiguous pointer error, got: %v", err)
	}
}

func TestMCPTerminalSelectConcurrency(t *testing.T) {
	sw := &mcpSelectWriteCloser{}
	br, bw, _ := os.Pipe()
	defer br.Close()
	defer bw.Close()

	ts := newTerminalSession(sw, br, nil, 80, 24, nil, nil, false, nil)
	sw.ts = ts

	ts.vtMu.Lock()
	ts.vt.Write([]byte("\x1b[H\x1b[2J❯ Option 1\r\n  Option 2\r\n  Option 3"))
	ts.vtMu.Unlock()

	pointerRe := regexp.MustCompile(`^\s*(?:❯|▸|›|→|»|>)\s`)

	var wg sync.WaitGroup
	wg.Add(3)

	errChan := make(chan error, 3)

	// Launch multiple concurrent select requests on the same session
	for range 3 {
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, err := ts.selectLabel(ctx, "Option 3", pointerRe, 5*time.Millisecond)
			if err != nil {
				errChan <- err
			}
		}()
	}

	wg.Wait()
	close(errChan)

	for err := range errChan {
		t.Errorf("concurrent select failed: %v", err)
	}
}

func TestMCPHarmlessScreenRedaction(t *testing.T) {
	s := &mcpServer{sessions: map[string]*terminalSession{}}
	t.Setenv("MYSECRET", "super-secret-value")

	req := map[string]any{
		"command":    []string{"sh", "-c", "echo hello; sleep 1"},
		"secret_env": []string{"MYSECRET"},
		"cols":       80,
	}
	reqBytes, _ := json.Marshal(req)

	res, err := s.tool(context.Background(), "terminal_start", reqBytes)
	if err != nil {
		t.Fatalf("start failed: %v", err)
	}
	m, _ := res.(map[string]string)
	sessionID := m["session_id"]
	defer func() {
		ss, _ := s.session(sessionID)
		if ss != nil {
			ss.close()
		}
	}()

	_, _ = s.tool(context.Background(), "terminal_expect", []byte(`{"session_id":"`+sessionID+`","text":"hello","timeout_seconds":2}`))

	resScreen, err := s.tool(context.Background(), "terminal_read_screen", []byte(`{"session_id":"`+sessionID+`"}`))
	if err != nil {
		t.Fatalf("read_screen failed: %v", err)
	}
	if strings.Contains(fmt.Sprint(resScreen), "<screen redacted>") {
		t.Fatalf("harmless screen was redacted: %v", resScreen)
	}
	if !strings.Contains(fmt.Sprint(resScreen), "hello") {
		t.Fatalf("harmless screen did not contain expected text: %v", resScreen)
	}
}

func TestMCPScreenRedactionWithoutRecorder(t *testing.T) {
	s := &mcpServer{sessions: map[string]*terminalSession{}}

	os.Setenv("TEST_SECRET_NO_RECORDER", "my-secret-val")
	defer os.Unsetenv("TEST_SECRET_NO_RECORDER")

	res, err := s.tool(context.Background(), "terminal_start", []byte(`{"command":["sh","-c","echo my-secret-val; sleep 5"],"secret_env":["TEST_SECRET_NO_RECORDER"]}`))
	if err != nil {
		t.Fatalf("start failed: %v", err)
	}
	m, _ := res.(map[string]string)
	sessionID := m["session_id"]
	defer func() {
		ss, _ := s.session(sessionID)
		if ss != nil {
			ss.close()
		}
	}()

	// wait for output
	time.Sleep(100 * time.Millisecond)

	resScreen, err := s.tool(context.Background(), "terminal_read_screen", []byte(`{"session_id":"`+sessionID+`"}`))
	if err != nil {
		t.Fatalf("read_screen failed: %v", err)
	}
	if !strings.Contains(fmt.Sprint(resScreen), "<screen redacted>") {
		t.Fatalf("screen without recorder was not redacted: %v", resScreen)
	}
}

func TestMCPPartialWriteErrorIncludesDetails(t *testing.T) {
	srv := &mcpServer{sessions: map[string]*terminalSession{}}

	// Create a dummy session
	srv.mu.Lock()
	fw := &failingWriter{err: io.ErrUnexpectedEOF}
	dummyIn, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	dummyOutR, dummyOutW := io.Pipe()
	defer dummyIn.Close()
	defer dummyOutW.Close()
	ts := newTerminalSession(fw, dummyOutR, nil, 80, 24, nil, nil, false, nil)
	srv.sessions["test-session"] = ts
	srv.mu.Unlock()

	req := mcp.CallToolRequest{}
	req.Params.Name = "terminal_write"
	req.Params.Arguments = map[string]any{"session_id": "test-session", "data": "hello"}

	res, err := srv.handleCallTool(context.Background(), req.Params.Name, req)
	if err != nil {
		t.Fatalf("handleCallTool error: %v", err)
	}

	if !res.IsError {
		t.Fatalf("response missing isError=true")
	}
	resStr := fmt.Sprint(res.Content)
	if !strings.Contains(resStr, "unexpected EOF") {
		t.Fatalf("response missing unexpected EOF: %s", resStr)
	}
	if !strings.Contains(fmt.Sprint(res.Content), `"written":2`) && !strings.Contains(fmt.Sprint(res.Content), `"written": 2`) {
		t.Fatalf("response missing written count 2: %s", resStr)
	}
}

func TestMCPHarmlessScreenAfterSecret(t *testing.T) {
	s := &mcpServer{sessions: map[string]*terminalSession{}}

	os.Setenv("TEST_MY_SECRET", "super-secret-value")
	defer os.Unsetenv("TEST_MY_SECRET")

	res, err := s.tool(context.Background(), "terminal_start", []byte(`{"command":["sh","-c","echo super-secret-value; sleep 0.1; clear; echo harmless-next-screen; sleep 5"],"secret_env":["TEST_MY_SECRET"]}`))
	if err != nil {
		t.Fatalf("start failed: %v", err)
	}
	m, _ := res.(map[string]string)
	sessionID := m["session_id"]
	defer func() {
		ss, _ := s.session(sessionID)
		if ss != nil {
			ss.close()
		}
	}()

	// wait for output to settle
	time.Sleep(500 * time.Millisecond)

	_, err = s.tool(context.Background(), "terminal_expect", []byte(`{"session_id":"`+sessionID+`","text":"harmless-next-screen","timeout_seconds":2}`))
	if err != nil {
		t.Fatalf("terminal_expect failed on harmless screen after secret: %v", err)
	}

	resScreen, err := s.tool(context.Background(), "terminal_read_screen", []byte(`{"session_id":"`+sessionID+`"}`))
	if err != nil {
		t.Fatalf("read_screen failed: %v", err)
	}
	// The response should be redacted because the session is permanently tainted
	if !strings.Contains(fmt.Sprint(resScreen), "<screen redacted>") {
		t.Fatalf("screen should be redacted for protocol responses: %v", resScreen)
	}
}

func TestANSISplitSecretCastRedaction(t *testing.T) {
	s := &mcpServer{sessions: map[string]*terminalSession{}}
	t.Setenv("ANSI_SECRET", "abcdefghijklmnopqrst")

	tmpCast := filepath.Join(t.TempDir(), "ansi_secret.cast")

	req := map[string]any{
		"command":     []string{"sh", "-c", "printf 'abcdefghij\\033[31mklmnopqrst\\033[0m\\n'; sleep 1"},
		"secret_env":  []string{"ANSI_SECRET"},
		"record_file": tmpCast,
		"cols":        80,
	}
	reqBytes, _ := json.Marshal(req)

	res, err := s.tool(context.Background(), "terminal_start", reqBytes)
	if err != nil {
		t.Fatalf("start failed: %v", err)
	}
	m, _ := res.(map[string]string)
	sessionID := m["session_id"]
	defer func() {
		ss, _ := s.session(sessionID)
		if ss != nil {
			ss.close()
		}
	}()

	ss, _ := s.session(sessionID)
	_, _ = ss.waitChildExit(context.Background())
	time.Sleep(200 * time.Millisecond)

	castBytes, err := os.ReadFile(tmpCast)
	if err != nil {
		t.Fatalf("failed to read cast: %v", err)
	}
	castStr := string(castBytes)

	hdr, events, err := loadCastFile(tmpCast)
	if err != nil {
		t.Fatalf("failed to load cast: %v", err)
	}

	vt := vt10x.New(vt10x.WithSize(hdr.Width, hdr.Height))
	for _, e := range events {
		if e.typ == "o" {
			vt.Write([]byte(e.data))
		}
	}
	rendered := strings.Join(normalizeScreen(vt.String()), "\n")

	if strings.Contains(rendered, "abcdefghijklmnopqrst") {
		t.Fatalf("render reconstructed full ANSI-split secret: %s", rendered)
	}

	for _, e := range events {
		if e.typ == "o" && strings.Contains(e.data, "abcdefghijklmnopqrst") {
			t.Fatalf("cast output event contains contiguous secret: %q", e.data)
		}
	}

	strippedCast := stripANSI(castStr)
	if strings.Contains(strippedCast, "abcdefghijklmnopqrst") {
		t.Fatalf("cast raw bytes contain ANSI-split secret after stripping: %s", castStr)
	}
}

func TestMCPTerminalResize(t *testing.T) {
	s := &mcpServer{sessions: map[string]*terminalSession{}}

	res, err := s.tool(context.Background(), "terminal_start", []byte(`{"command":["sleep","10"],"cols":80,"rows":24}`))
	if err != nil {
		t.Fatalf("start failed: %v", err)
	}
	m, _ := res.(map[string]string)
	sessionID := m["session_id"]
	defer func() {
		s.tool(context.Background(), "terminal_close", []byte(`{"session_id":"`+sessionID+`"}`))
	}()

	resResize, err := s.tool(context.Background(), "terminal_resize", []byte(`{"session_id":"`+sessionID+`","cols":100,"rows":30}`))
	if err != nil {
		t.Fatalf("resize failed: %v", err)
	}
	rm, _ := resResize.(map[string]any)
	cols, _ := rm["cols"].(int)
	rows, _ := rm["rows"].(int)
	if cols != 100 || rows != 30 {
		t.Fatalf("resize returned wrong dimensions: cols=%d rows=%d", cols, rows)
	}

	resScreen, err := s.tool(context.Background(), "terminal_read_screen", []byte(`{"session_id":"`+sessionID+`"}`))
	if err != nil {
		t.Fatalf("read_screen failed: %v", err)
	}
	screenStr := fmt.Sprint(resScreen)
	if !strings.Contains(screenStr, "cols:100") {
		t.Fatalf("screen size not updated after resize (cols): %s", screenStr)
	}
	if !strings.Contains(screenStr, "rows:30") {
		t.Fatalf("screen size not updated after resize (rows): %s", screenStr)
	}

	_, err = s.tool(context.Background(), "terminal_resize", []byte(`{"session_id":"nonexistent","cols":80,"rows":24}`))
	if err == nil {
		t.Fatalf("resize with invalid session should fail")
	}

	_, err = s.tool(context.Background(), "terminal_resize", []byte(`{"session_id":"`+sessionID+`","cols":0,"rows":24}`))
	if err == nil {
		t.Fatalf("resize with cols=0 should fail")
	}

	_, err = s.tool(context.Background(), "terminal_resize", []byte(`{"session_id":"`+sessionID+`","cols":1001,"rows":24}`))
	if err == nil {
		t.Fatalf("resize with cols=1001 should fail")
	}
}
