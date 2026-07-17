package main

import (
	"bufio"
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hinshun/vt10x"
)

func TestResizeEventsUpdateRenderAndRecordingState(t *testing.T) {
	vt := vt10x.New(vt10x.WithSize(80, 24))
	if err := applyRenderEvent(vt, castEvent{typ: "r", data: "100x30"}); err != nil {
		t.Fatal(err)
	}
	cols, rows := vt.Size()
	if cols != 100 || rows != 30 {
		t.Fatalf("render size = %dx%d, want 100x30", cols, rows)
	}

	var cast bytes.Buffer
	redactor, err := newSecretRedactor(nil)
	if err != nil {
		t.Fatal(err)
	}
	recorder := newRecordingWriter(bufio.NewWriter(&cast), &sync.Mutex{}, redactor)
	ts := &terminalSession{
		start:    time.Now(),
		cols:     80,
		rows:     24,
		vt:       vt10x.New(vt10x.WithSize(80, 24)),
		recorder: recorder,
	}
	if err := ts.resize(100, 30); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cast.String(), `"r","100x30"`) {
		t.Fatalf("resize was not recorded:\n%s", cast.String())
	}
}
