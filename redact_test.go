package main

import (
	"bufio"
	"bytes"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/hinshun/vt10x"
)

func TestRedactScreenTable(t *testing.T) {
	tests := []struct {
		name       string
		secret     string
		lines      []string
		wantRedact bool
	}{
		{
			name:       "1-byte secret",
			secret:     "X",
			lines:      []string{"hello X world"},
			wantRedact: true,
		},
		{
			name:       "2-byte secret",
			secret:     "XY",
			lines:      []string{"hello XY world"},
			wantRedact: true,
		},
		{
			name:       "3-byte secret",
			secret:     "XYZ",
			lines:      []string{"hello XYZ world"},
			wantRedact: true,
		},
		{
			name:       "4-byte secret",
			secret:     "XYZW",
			lines:      []string{"hello XYZW world"},
			wantRedact: true,
		},
		{
			name:       "1-rune Unicode secret",
			secret:     "🔥",
			lines:      []string{"hello 🔥 world"},
			wantRedact: true,
		},
		{
			name:       "secret with space",
			secret:     "a b",
			lines:      []string{"hello a b world"},
			wantRedact: true,
		},
		{
			name:       "secret with CR/LF",
			secret:     "hello\r\nworld",
			lines:      []string{"some text hello", "world and more"},
			wantRedact: true,
		},
		{
			name:       "wrapped secret boundary 1",
			secret:     "mysecret",
			lines:      []string{"this is m", "ysecret ok"},
			wantRedact: true,
		},
		{
			name:       "wrapped secret boundary 2",
			secret:     "mysecret",
			lines:      []string{"this is my", "secret ok"},
			wantRedact: true,
		},
		{
			name:       "wrapped secret boundary 3",
			secret:     "mysecret",
			lines:      []string{"this is mysec", "ret ok"},
			wantRedact: true,
		},
		{
			name:       "wrapped secret boundary 4",
			secret:     "mysecret",
			lines:      []string{"this is mysecre", "t ok"},
			wantRedact: true,
		},
		{
			name:       "safe screen without secret",
			secret:     "mysecret",
			lines:      []string{"this is a completely", "safe screen"},
			wantRedact: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			os.Setenv("TEST_SECRET", tc.secret)
			defer os.Unsetenv("TEST_SECRET")
			redactor, err := newSecretRedactor([]string{"TEST_SECRET"})
			if err != nil {
				t.Fatalf("setup: %v", err)
			}
			got := redactor.redactScreen(tc.lines)
			if tc.wantRedact {
				if len(got) != 1 || got[0] != "<screen redacted>" {
					t.Errorf("wanted <screen redacted>, got %v", got)
				}
			} else {
				if !reflect.DeepEqual(got, tc.lines) {
					t.Errorf("wanted original lines, got %v", got)
				}
			}
		})
	}
}

func TestAnySecretInANSI(t *testing.T) {
	os.Setenv("TEST_SECRET_ANSI", "abcdefghijklmnopqrst")
	defer os.Unsetenv("TEST_SECRET_ANSI")
	redactor, err := newSecretRedactor([]string{"TEST_SECRET_ANSI"})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	// A secret interrupted by an ANSI color sequence
	input := "abcdefghij\x1b[31mklmnopqrst\x1b[0m"
	if !redactor.AnySecretIn(input) {
		t.Fatalf("AnySecretIn failed to detect secret broken up by ANSI codes")
	}
}

func TestPendingSecretPrefix(t *testing.T) {
	os.Setenv("TEST_SECRET_PREFIX", "mysecret")
	defer os.Unsetenv("TEST_SECRET_PREFIX")
	redactor, err := newSecretRedactor([]string{"TEST_SECRET_PREFIX"})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	tests := []struct {
		input string
		want  bool
	}{
		{"m", true},
		{"my", true},
		{"mysecre", true},
		{"mysecret", true}, // full secret is also a prefix
		{"something m", true},
		{"something my", true},
		{"myx", false}, // breaks prefix
		{"x", false},
		{"", false},
	}

	for _, tc := range tests {
		got := redactor.PendingSecretPrefix(tc.input)
		if got != tc.want {
			t.Errorf("PendingSecretPrefix(%q) = %v; want %v", tc.input, got, tc.want)
		}
	}
}

func TestRedactOutputStreamPreservesContext(t *testing.T) {
	os.Setenv("TEST_CTX_SECRET", "abc")
	defer os.Unsetenv("TEST_CTX_SECRET")
	redactor, err := newSecretRedactor([]string{"TEST_CTX_SECRET"})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	tests := []struct {
		name       string
		input      string
		wantPrefix string
		wantSuffix string
		wantMarker string
	}{
		{
			name:       "secret with surrounding text",
			input:      "before abc after",
			wantPrefix: "before ",
			wantSuffix: " after",
			wantMarker: "<redacted:TEST_CTX_SECRET>",
		},
		{
			name:       "secret at start",
			input:      "abc after",
			wantPrefix: "",
			wantSuffix: " after",
			wantMarker: "<redacted:TEST_CTX_SECRET>",
		},
		{
			name:       "secret at end",
			input:      "before abc",
			wantPrefix: "before ",
			wantSuffix: "",
			wantMarker: "<redacted:TEST_CTX_SECRET>",
		},
		{
			name:       "secret only",
			input:      "abc",
			wantPrefix: "",
			wantSuffix: "",
			wantMarker: "<redacted:TEST_CTX_SECRET>",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := redactor.redactOutputStream(tc.input)
			if strings.Contains(got, "abc") {
				t.Fatalf("redacted output still contains secret: %q", got)
			}
			if tc.wantPrefix != "" && !strings.HasPrefix(got, tc.wantPrefix) {
				t.Fatalf("missing prefix %q in %q", tc.wantPrefix, got)
			}
			if tc.wantSuffix != "" && !strings.HasSuffix(got, tc.wantSuffix) {
				t.Fatalf("missing suffix %q in %q", tc.wantSuffix, got)
			}
			if !strings.Contains(got, tc.wantMarker) {
				t.Fatalf("missing marker %q in %q", tc.wantMarker, got)
			}
		})
	}
}

func TestRedactOutputStreamANSISplit(t *testing.T) {
	os.Setenv("TEST_ANSI_CTX", "abcdef")
	defer os.Unsetenv("TEST_ANSI_CTX")
	redactor, err := newSecretRedactor([]string{"TEST_ANSI_CTX"})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	input := "before abc\x1b[31mdef after"
	got := redactor.redactOutputStream(input)

	if strings.Contains(got, "abcdef") {
		t.Fatalf("redacted output contains contiguous secret: %q", got)
	}
	stripped := stripANSI(got)
	if strings.Contains(stripped, "abcdef") {
		t.Fatalf("redacted output (ANSI-stripped) contains secret: %q", stripped)
	}
	if !strings.HasPrefix(got, "before ") {
		t.Fatalf("missing prefix in %q", got)
	}
	if !strings.HasSuffix(got, " after") {
		t.Fatalf("missing suffix in %q", got)
	}
	if !strings.Contains(got, "<redacted:TEST_ANSI_CTX>") {
		t.Fatalf("missing marker in %q", got)
	}
}

func TestRecordingWriterSplitSecretPreservesContext(t *testing.T) {
	os.Setenv("TEST_RW_CTX", "abcdef")
	defer os.Unsetenv("TEST_RW_CTX")
	redactor, err := newSecretRedactor([]string{"TEST_RW_CTX"})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	var buf bytes.Buffer
	bw := bufio.NewWriter(&buf)
	var mu sync.Mutex
	rw := newRecordingWriter(bw, &mu, redactor)
	_ = rw.writeHeader(castHeader{Version: 2, Width: 80, Height: 24})

	rw.output(0.1, "before ab")
	rw.output(0.2, "cdef after")
	_ = rw.flushOutput()
	_ = rw.flush()

	content := buf.String()
	if strings.Contains(content, `"abcdef"`) {
		t.Fatalf("cast contains contiguous secret: %s", content)
	}

	_, events, err := loadCastFileFromBytes(content)
	if err != nil {
		t.Fatalf("parse cast: %v", err)
	}
	var outputData string
	for _, e := range events {
		if e.typ == "o" {
			outputData += e.data
		}
	}
	if strings.Contains(outputData, "abcdef") {
		t.Fatalf("cast output contains secret: %q", outputData)
	}
	if !strings.Contains(outputData, "before ") {
		t.Fatalf("cast missing prefix 'before ': %q", outputData)
	}
	if !strings.Contains(outputData, " after") {
		t.Fatalf("cast missing suffix ' after': %q", outputData)
	}
	if !strings.Contains(outputData, "<redacted:TEST_RW_CTX>") {
		t.Fatalf("cast missing redaction marker: %q", outputData)
	}
}

func TestRecordingWriterANSISplitPreservesContext(t *testing.T) {
	os.Setenv("TEST_RW_ANSI", "abcdefghijklmnopqrst")
	defer os.Unsetenv("TEST_RW_ANSI")
	redactor, err := newSecretRedactor([]string{"TEST_RW_ANSI"})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	var buf bytes.Buffer
	bw := bufio.NewWriter(&buf)
	var mu sync.Mutex
	rw := newRecordingWriter(bw, &mu, redactor)
	_ = rw.writeHeader(castHeader{Version: 2, Width: 80, Height: 24})

	rw.output(0.1, "STARTabcdefghij\x1b[31m")
	rw.output(0.2, "klmnopqrst\x1b[0mEND")
	_ = rw.flushOutput()
	_ = rw.flush()

	content := buf.String()

	_, events, err := loadCastFileFromBytes(content)
	if err != nil {
		t.Fatalf("parse cast: %v", err)
	}
	var outputData string
	for _, e := range events {
		if e.typ == "o" {
			outputData += e.data
		}
	}
	stripped := stripANSI(outputData)
	if strings.Contains(stripped, "abcdefghijklmnopqrst") {
		t.Fatalf("cast output (ANSI-stripped) contains secret: %q", stripped)
	}
	if !strings.Contains(outputData, "START") {
		t.Fatalf("cast missing prefix 'START': %q", outputData)
	}
	if !strings.Contains(outputData, "END") {
		t.Fatalf("cast missing suffix 'END': %q", outputData)
	}
	if !strings.Contains(outputData, "<redacted:TEST_RW_ANSI>") {
		t.Fatalf("cast missing redaction marker: %q", outputData)
	}
}

func TestRedactOutputStreamManyANSISplits(t *testing.T) {
	os.Setenv("TEST_MANY_ANSI", "abcdef")
	defer os.Unsetenv("TEST_MANY_ANSI")
	redactor, err := newSecretRedactor([]string{"TEST_MANY_ANSI"})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	var payload strings.Builder
	for i := 0; i < 17; i++ {
		payload.WriteString("abc\x1b[31mdef\x1b[0m")
	}
	got := redactor.redactOutputStream(payload.String())

	stripped := stripANSI(got)
	if strings.Contains(stripped, "abcdef") {
		t.Fatalf("17th ANSI-split secret leaked: %q", got)
	}
	count := strings.Count(got, "<redacted:TEST_MANY_ANSI>")
	if count != 17 {
		t.Fatalf("expected 17 redaction markers, got %d in %q", count, got)
	}
}

func TestRedactOutputStreamManyWhitespaceSplits(t *testing.T) {
	os.Setenv("TEST_MANY_WS", "abcdef")
	defer os.Unsetenv("TEST_MANY_WS")
	redactor, err := newSecretRedactor([]string{"TEST_MANY_WS"})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	var payload strings.Builder
	for i := 0; i < 17; i++ {
		if i > 0 {
			payload.WriteString(" ")
		}
		payload.WriteString("ab")
		payload.WriteString("\t")
		payload.WriteString("cd")
		payload.WriteString("\n")
		payload.WriteString("ef")
	}
	got := redactor.redactOutputStream(payload.String())

	stripped := stripANSI(got)
	if strings.Contains(stripped, "abcdef") {
		t.Fatalf("17th whitespace-split secret leaked: %q", got)
	}
	count := strings.Count(got, "<redacted:TEST_MANY_WS>")
	if count != 17 {
		t.Fatalf("expected 17 redaction markers, got %d in %q", count, got)
	}
}

func TestRecordingWriterManyANSISplitsInCast(t *testing.T) {
	os.Setenv("TEST_RW_MANY_ANSI", "abcdefghijklmnopqrst")
	defer os.Unsetenv("TEST_RW_MANY_ANSI")
	redactor, err := newSecretRedactor([]string{"TEST_RW_MANY_ANSI"})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	var buf bytes.Buffer
	bw := bufio.NewWriter(&buf)
	var mu sync.Mutex
	rw := newRecordingWriter(bw, &mu, redactor)
	_ = rw.writeHeader(castHeader{Version: 2, Width: 80, Height: 24})

	var payload strings.Builder
	payload.WriteString("START")
	for i := 0; i < 17; i++ {
		payload.WriteString("abcdefghij\x1b[31m")
		payload.WriteString("klmnopqrst\x1b[0m")
	}
	payload.WriteString("END")
	rw.output(0.1, payload.String())
	_ = rw.flushOutput()
	_ = rw.flush()

	content := buf.String()
	_, events, err := loadCastFileFromBytes(content)
	if err != nil {
		t.Fatalf("parse cast: %v", err)
	}
	var outputData string
	for _, e := range events {
		if e.typ == "o" {
			outputData += e.data
		}
	}
	stripped := stripANSI(outputData)
	if strings.Contains(stripped, "abcdefghijklmnopqrst") {
		t.Fatalf("cast output (ANSI-stripped) contains secret: %q", stripped)
	}
	count := strings.Count(outputData, "<redacted:TEST_RW_MANY_ANSI>")
	if count != 17 {
		t.Fatalf("expected 17 redaction markers in cast, got %d in %q", count, outputData)
	}
	if !strings.Contains(outputData, "START") {
		t.Fatalf("cast missing prefix 'START': %q", outputData)
	}
	if !strings.Contains(outputData, "END") {
		t.Fatalf("cast missing suffix 'END': %q", outputData)
	}
}

func TestRecordingWriterBoundedPrefixBuffer(t *testing.T) {
	os.Setenv("TEST_PREFIX_BOUND", "alphabet")
	defer os.Unsetenv("TEST_PREFIX_BOUND")
	redactor, err := newSecretRedactor([]string{"TEST_PREFIX_BOUND"})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	var buf bytes.Buffer
	bw := bufio.NewWriter(&buf)
	var mu sync.Mutex
	rw := newRecordingWriter(bw, &mu, redactor)
	_ = rw.writeHeader(castHeader{Version: 2, Width: 80, Height: 24})

	chunk := strings.Repeat("x", 10000) + "a"
	for i := 0; i < 100; i++ {
		rw.output(float64(i)+0.1, chunk)
	}
	rw.output(100.1, "lphabet")
	_ = rw.flushOutput()
	_ = rw.flush()

	content := buf.String()
	_, events, err := loadCastFileFromBytes(content)
	if err != nil {
		t.Fatalf("parse cast: %v", err)
	}
	var outputData string
	outputEventCount := 0
	for _, e := range events {
		if e.typ == "o" {
			outputData += e.data
			outputEventCount++
		}
	}
	if strings.Contains(outputData, "alphabet") {
		t.Fatalf("cast contains secret: %q", outputData)
	}
	if !strings.Contains(outputData, "<redacted:TEST_PREFIX_BOUND>") {
		t.Fatalf("cast missing redaction marker: %q", outputData)
	}
	if outputEventCount < 50 {
		t.Fatalf("expected many intermediate output events, got %d", outputEventCount)
	}
}

func TestRedactOutputStreamMarkerSpoofANSISplit(t *testing.T) {
	os.Setenv("TEST_SPOOF_ANSI", "abcdef")
	defer os.Unsetenv("TEST_SPOOF_ANSI")
	redactor, err := newSecretRedactor([]string{"TEST_SPOOF_ANSI"})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	input := "<redacted:abc\x1b[31mdef>"
	got := redactor.redactOutputStream(input)

	stripped := stripANSI(got)
	if strings.Contains(stripped, "abcdef") {
		t.Fatalf("marker-spoofed ANSI-split secret leaked: %q", got)
	}
	if !strings.Contains(got, "<redacted:TEST_SPOOF_ANSI>") {
		t.Fatalf("missing redaction marker: %q", got)
	}
}

func TestRedactOutputStreamMarkerSpoofWhitespaceSplit(t *testing.T) {
	os.Setenv("TEST_SPOOF_WS", "abcdef")
	defer os.Unsetenv("TEST_SPOOF_WS")
	redactor, err := newSecretRedactor([]string{"TEST_SPOOF_WS"})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	input := "<redacted:ab cd\tef>"
	got := redactor.redactOutputStream(input)

	stripped := stripANSI(got)
	if strings.Contains(stripped, "abcdef") {
		t.Fatalf("marker-spoofed whitespace-split secret leaked: %q", got)
	}
	if !strings.Contains(got, "<redacted:TEST_SPOOF_WS>") {
		t.Fatalf("missing redaction marker: %q", got)
	}
}

func TestRecordingWriterMarkerSpoofANSISplitInCast(t *testing.T) {
	os.Setenv("TEST_RW_SPOOF_ANSI", "abcdefghijklmnopqrst")
	defer os.Unsetenv("TEST_RW_SPOOF_ANSI")
	redactor, err := newSecretRedactor([]string{"TEST_RW_SPOOF_ANSI"})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	var buf bytes.Buffer
	bw := bufio.NewWriter(&buf)
	var mu sync.Mutex
	rw := newRecordingWriter(bw, &mu, redactor)
	_ = rw.writeHeader(castHeader{Version: 2, Width: 80, Height: 24})

	rw.output(0.1, "<redacted:abcdefghij\x1b[31mklmnopqrst>")
	_ = rw.flushOutput()
	_ = rw.flush()

	content := buf.String()
	_, events, err := loadCastFileFromBytes(content)
	if err != nil {
		t.Fatalf("parse cast: %v", err)
	}
	var outputData string
	for _, e := range events {
		if e.typ == "o" {
			outputData += e.data
		}
	}
	stripped := stripANSI(outputData)
	if strings.Contains(stripped, "abcdefghijklmnopqrst") {
		t.Fatalf("cast contains spoofed secret: %q", stripped)
	}
	if !strings.Contains(outputData, "<redacted:TEST_RW_SPOOF_ANSI>") {
		t.Fatalf("cast missing redaction marker: %q", outputData)
	}
}

func TestRecordingWriterANSIPrefixBufferBounded(t *testing.T) {
	os.Setenv("TEST_ANSI_PREFIX", "abc")
	defer os.Unsetenv("TEST_ANSI_PREFIX")
	redactor, err := newSecretRedactor([]string{"TEST_ANSI_PREFIX"})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	var buf bytes.Buffer
	bw := bufio.NewWriter(&buf)
	var mu sync.Mutex
	rw := newRecordingWriter(bw, &mu, redactor)
	_ = rw.writeHeader(castHeader{Version: 2, Width: 80, Height: 24})

	osc := "\x1b]" + strings.Repeat("x", 1<<10) + "\x07"
	rw.output(0.1, "a"+osc)
	for i := 0; i < 50; i++ {
		rw.output(float64(i)+0.2, osc)
	}

	pendingLen := reflect.ValueOf(rw).Elem().FieldByName("pending").Len()
	if pendingLen > len("a"+osc)*2 {
		t.Fatalf("raw pending too large: %d", pendingLen)
	}

	rw.output(50.2, "bc")
	_ = rw.flushOutput()
	_ = rw.flush()

	content := buf.String()
	_, events, err := loadCastFileFromBytes(content)
	if err != nil {
		t.Fatalf("parse cast: %v", err)
	}
	var outputData string
	outputEventCount := 0
	for _, e := range events {
		if e.typ == "o" {
			outputData += e.data
			outputEventCount++
		}
	}
	if strings.Contains(outputData, "abc") {
		t.Fatalf("cast contains secret: %q", outputData)
	}
	if !strings.Contains(outputData, "<redacted:TEST_ANSI_PREFIX>") {
		t.Fatalf("cast missing redaction marker: %q", outputData)
	}
	if outputEventCount < 25 {
		t.Fatalf("expected many intermediate output events, got %d", outputEventCount)
	}
}

func TestRecordingWriterSplitCSISecretRedaction(t *testing.T) {
	os.Setenv("TEST_SPLIT_CSI", "abc")
	defer os.Unsetenv("TEST_SPLIT_CSI")
	redactor, err := newSecretRedactor([]string{"TEST_SPLIT_CSI"})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	var buf bytes.Buffer
	bw := bufio.NewWriter(&buf)
	var mu sync.Mutex
	rw := newRecordingWriter(bw, &mu, redactor)
	_ = rw.writeHeader(castHeader{Version: 2, Width: 80, Height: 24})

	rw.output(0.1, "a\x1b[3")
	rw.output(0.2, "1mbc")
	_ = rw.flushOutput()
	_ = rw.flush()

	content := buf.String()
	_, events, err := loadCastFileFromBytes(content)
	if err != nil {
		t.Fatalf("parse cast: %v", err)
	}

	var outputData string
	for _, e := range events {
		if e.typ == "o" {
			outputData += e.data
			if strings.Contains(e.data, "abc") {
				t.Fatalf("output event contains contiguous secret: %q", e.data)
			}
		}
	}
	if strings.Contains(stripANSI(outputData), "abc") {
		t.Fatalf("ANSI-stripped output contains secret: %q", outputData)
	}

	vt := vt10x.New(vt10x.WithSize(80, 24))
	for _, e := range events {
		if e.typ == "o" {
			vt.Write([]byte(e.data))
		}
	}
	rendered := strings.Join(normalizeScreen(vt.String()), "\n")
	if strings.Contains(rendered, "abc") {
		t.Fatalf("rendered screen contains secret: %s", rendered)
	}
	if !strings.Contains(outputData, "<redacted:TEST_SPLIT_CSI>") {
		t.Fatalf("missing redaction marker: %q", outputData)
	}
}

func TestRecordingWriterSplitOSCSecretRedaction(t *testing.T) {
	os.Setenv("TEST_SPLIT_OSC", "abc")
	defer os.Unsetenv("TEST_SPLIT_OSC")
	redactor, err := newSecretRedactor([]string{"TEST_SPLIT_OSC"})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	var buf bytes.Buffer
	bw := bufio.NewWriter(&buf)
	var mu sync.Mutex
	rw := newRecordingWriter(bw, &mu, redactor)
	_ = rw.writeHeader(castHeader{Version: 2, Width: 80, Height: 24})

	rw.output(0.1, "a\x1b]")
	rw.output(0.2, "X\x07bc")
	_ = rw.flushOutput()
	_ = rw.flush()

	content := buf.String()
	_, events, err := loadCastFileFromBytes(content)
	if err != nil {
		t.Fatalf("parse cast: %v", err)
	}

	var outputData string
	for _, e := range events {
		if e.typ == "o" {
			outputData += e.data
			if strings.Contains(e.data, "abc") {
				t.Fatalf("output event contains contiguous secret: %q", e.data)
			}
		}
	}
	if strings.Contains(stripANSI(outputData), "abc") {
		t.Fatalf("ANSI-stripped output contains secret: %q", outputData)
	}

	vt := vt10x.New(vt10x.WithSize(80, 24))
	for _, e := range events {
		if e.typ == "o" {
			vt.Write([]byte(e.data))
		}
	}
	rendered := strings.Join(normalizeScreen(vt.String()), "\n")
	if strings.Contains(rendered, "abc") {
		t.Fatalf("rendered screen contains secret: %s", rendered)
	}
	if !strings.Contains(outputData, "<redacted:TEST_SPLIT_OSC>") {
		t.Fatalf("missing redaction marker: %q", outputData)
	}
}

func TestRecordingWriterSplitOSCSTSecretRedaction(t *testing.T) {
	os.Setenv("TEST_SPLIT_OSC_ST", "abc")
	defer os.Unsetenv("TEST_SPLIT_OSC_ST")
	redactor, err := newSecretRedactor([]string{"TEST_SPLIT_OSC_ST"})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	var buf bytes.Buffer
	bw := bufio.NewWriter(&buf)
	var mu sync.Mutex
	rw := newRecordingWriter(bw, &mu, redactor)
	_ = rw.writeHeader(castHeader{Version: 2, Width: 80, Height: 24})

	rw.output(0.1, "a\x1b]title\x1b")
	rw.output(0.2, "\\bc")
	_ = rw.flushOutput()
	_ = rw.flush()

	content := buf.String()
	_, events, err := loadCastFileFromBytes(content)
	if err != nil {
		t.Fatalf("parse cast: %v", err)
	}

	var outputData string
	for _, e := range events {
		if e.typ == "o" {
			outputData += e.data
			if strings.Contains(e.data, "abc") {
				t.Fatalf("output event contains contiguous secret: %q", e.data)
			}
		}
	}
	if strings.Contains(stripANSI(outputData), "abc") {
		t.Fatalf("ANSI-stripped output contains secret: %q", outputData)
	}

	vt := vt10x.New(vt10x.WithSize(80, 24))
	for _, e := range events {
		if e.typ == "o" {
			vt.Write([]byte(e.data))
		}
	}
	rendered := strings.Join(normalizeScreen(vt.String()), "\n")
	if strings.Contains(rendered, "abc") {
		t.Fatalf("rendered screen contains secret: %s", rendered)
	}
	if !strings.Contains(outputData, "<redacted:TEST_SPLIT_OSC_ST>") {
		t.Fatalf("missing redaction marker: %q", outputData)
	}
}

func TestRecordingWriterUnterminatedOSCBounded(t *testing.T) {
	os.Setenv("TEST_UNTERM_OSC", "abc")
	defer os.Unsetenv("TEST_UNTERM_OSC")
	redactor, err := newSecretRedactor([]string{"TEST_UNTERM_OSC"})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	var buf bytes.Buffer
	bw := bufio.NewWriter(&buf)
	var mu sync.Mutex
	rw := newRecordingWriter(bw, &mu, redactor)
	_ = rw.writeHeader(castHeader{Version: 2, Width: 80, Height: 24})

	rw.output(0.1, "a\x1b]")
	for i := 0; i < 1000; i++ {
		rw.output(float64(i)+0.2, strings.Repeat("x", 10000))
	}

	pendingLen := reflect.ValueOf(rw).Elem().FieldByName("pending").Len()
	if pendingLen > 128*1024 {
		t.Fatalf("raw pending too large: %d", pendingLen)
	}

	_ = rw.flushOutput()
	_ = rw.flush()

	content := buf.String()
	_, events, err := loadCastFileFromBytes(content)
	if err != nil {
		t.Fatalf("parse cast: %v", err)
	}
	var outputData string
	for _, e := range events {
		if e.typ == "o" {
			outputData += e.data
		}
	}
	if !strings.Contains(outputData, "<ansi-truncated>") {
		t.Fatalf("missing ANSI truncation marker")
	}
	if strings.Contains(outputData, "abc") {
		t.Fatalf("cast contains secret: %q", outputData)
	}
}

func TestRecordingWriterUnterminatedCSIBounded(t *testing.T) {
	os.Setenv("TEST_UNTERM_CSI", "abc")
	defer os.Unsetenv("TEST_UNTERM_CSI")
	redactor, err := newSecretRedactor([]string{"TEST_UNTERM_CSI"})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	var buf bytes.Buffer
	bw := bufio.NewWriter(&buf)
	var mu sync.Mutex
	rw := newRecordingWriter(bw, &mu, redactor)
	_ = rw.writeHeader(castHeader{Version: 2, Width: 80, Height: 24})

	rw.output(0.1, "a\x1b[")
	for i := 0; i < 1000; i++ {
		rw.output(float64(i)+0.2, strings.Repeat("0", 10000))
	}

	pendingLen := reflect.ValueOf(rw).Elem().FieldByName("pending").Len()
	if pendingLen > 128*1024 {
		t.Fatalf("raw pending too large: %d", pendingLen)
	}

	_ = rw.flushOutput()
	_ = rw.flush()

	content := buf.String()
	_, events, err := loadCastFileFromBytes(content)
	if err != nil {
		t.Fatalf("parse cast: %v", err)
	}
	var outputData string
	for _, e := range events {
		if e.typ == "o" {
			outputData += e.data
		}
	}
	if !strings.Contains(outputData, "<ansi-truncated>") {
		t.Fatalf("missing ANSI truncation marker")
	}
	if strings.Contains(outputData, "abc") {
		t.Fatalf("cast contains secret: %q", outputData)
	}
}
