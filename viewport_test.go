package main

import (
	"strings"
	"testing"

	"github.com/hinshun/vt10x"
)

// TestViewportCropsOutOfBoundsContent reproduces the reported corruption: a
// recording sized larger than the real terminal positions text past the
// real grid's bottom row. Raw passthrough would misplace those bytes on the
// real (smaller) terminal; the viewport must resolve the position against
// the full-size virtual screen and simply omit anything outside the crop.
func TestViewportCropsOutOfBoundsContent(t *testing.T) {
	v := newViewport(40, 20, 40, 5) // recording 40x20, real terminal 40x5
	var buf strings.Builder
	v.write(&buf, "\033[1;1HVISIBLE\033[11;1HHIDDEN")
	out := buf.String()
	if !strings.Contains(out, "VISIBLE") {
		t.Fatalf("expected VISIBLE in output, got %q", out)
	}
	if strings.Contains(out, "HIDDEN") {
		t.Fatalf("content outside the crop leaked into output: %q", out)
	}
}

// TestViewportNoDuplicateRows guards against the specific artifact from the
// bug report: a menu item appearing twice with two cursor markers. Each
// repaint fully redraws the crop from the virtual screen's own cells, so a
// single write of one row must appear exactly once in the output.
func TestViewportNoDuplicateRows(t *testing.T) {
	v := newViewport(40, 10, 40, 10)
	var buf strings.Builder
	v.write(&buf, "▸ item one\r\n  item two")
	out := buf.String()
	if n := strings.Count(out, "item one"); n != 1 {
		t.Fatalf("expected \"item one\" exactly once, got %d occurrences in %q", n, out)
	}
	if n := strings.Count(out, "▸"); n != 1 {
		t.Fatalf("expected cursor marker exactly once, got %d occurrences in %q", n, out)
	}
}

func TestRepaintViewportCursorVisibility(t *testing.T) {
	v := newViewport(40, 10, 40, 5)
	var buf strings.Builder
	v.write(&buf, "\033[2;3Hx")
	if !strings.Contains(buf.String(), "\033[2;4H") {
		t.Fatalf("expected cursor positioned at row 2 col 4 (1-based, after the written char), got %q", buf.String())
	}

	buf.Reset()
	v.write(&buf, "\033[9;1Hy") // row 9 is outside the 5-row visible crop
	if strings.Contains(buf.String(), "?25h") {
		t.Fatalf("cursor outside the crop should stay hidden, got %q", buf.String())
	}
}

func TestSgrForAttrBasicColor(t *testing.T) {
	got := sgrForAttr(cellAttr{fg: vt10x.Red, bg: vt10x.DefaultBG})
	want := "\033[0;38;5;1m"
	if got != want {
		t.Fatalf("sgrForAttr fg=Red: got %q, want %q", got, want)
	}
}

func TestSgrForAttrTruecolor(t *testing.T) {
	c := vt10x.Color(10<<16 | 20<<8 | 30)
	got := sgrForAttr(cellAttr{fg: c, bg: vt10x.DefaultBG})
	want := "\033[0;38;2;10;20;30m"
	if got != want {
		t.Fatalf("sgrForAttr truecolor: got %q, want %q", got, want)
	}
}

func TestSgrForAttrModeBits(t *testing.T) {
	got := sgrForAttr(cellAttr{mode: vtAttrBold | vtAttrReverse, fg: vt10x.DefaultFG, bg: vt10x.DefaultBG})
	want := "\033[0;1;7m"
	if got != want {
		t.Fatalf("sgrForAttr bold+reverse: got %q, want %q", got, want)
	}
}

func TestViewportResetBlanksVirtualScreen(t *testing.T) {
	v := newViewport(20, 5, 20, 5)
	var buf strings.Builder
	v.write(&buf, "\033[1;1Hstale")
	buf.Reset()
	v.reset(&buf)
	if strings.Contains(buf.String(), "stale") {
		t.Fatalf("reset should blank the virtual screen, got %q", buf.String())
	}
}
