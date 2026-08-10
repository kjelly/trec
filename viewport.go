package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/hinshun/vt10x"
)

// Mirrors the private Glyph.Mode attribute bits vt10x encodes internally
// (state.go: attrReverse, attrUnderline, attrBold, attrGfx, attrItalic,
// attrBlink). vt10x does not export them, so the bit order here must track
// the exact commit go.mod pins; it cannot drift silently underneath this.
const (
	vtAttrReverse int16 = 1 << iota
	vtAttrUnderline
	vtAttrBold
	_ // attrGfx: charset selection, already resolved into Glyph.Char by vt10x
	vtAttrItalic
	vtAttrBlink
)

// viewport re-renders recorded output into a full-size virtual VT100 screen
// (sized to the recording's own dimensions) and repaints only the top-left
// region that fits the real, smaller terminal. Cursor addressing and line
// wrapping are resolved once inside the virtual screen at the recording's
// own width/height, so the real terminal only ever receives an
// already-clipped, self-consistent frame — the corruption from replaying
// oversized cursor moves into an undersized real grid never occurs. The
// tradeoff is that content outside the visible crop is simply not shown.
type viewport struct {
	vt              vt10x.Terminal
	recCols, recRow int
	cols, rows      int // visible size on the real terminal
}

func newViewport(recCols, recRows, visibleCols, visibleRows int) *viewport {
	return &viewport{
		vt:      vt10x.New(vt10x.WithSize(recCols, recRows)),
		recCols: recCols,
		recRow:  recRows,
		cols:    visibleCols,
		rows:    visibleRows,
	}
}

// reset discards the virtual screen contents and repaints the now-blank
// crop, mirroring a real "\033[2J\033[H" for the non-cropped path. Used
// when the player rebuilds state from scratch (restart, step-back, jump to
// an earlier marker).
func (v *viewport) reset(w io.Writer) {
	v.vt = vt10x.New(vt10x.WithSize(v.recCols, v.recRow))
	repaintViewport(w, v.vt, v.cols, v.rows)
}

// write feeds recorded output into the virtual screen and repaints the
// visible crop.
func (v *viewport) write(w io.Writer, data string) {
	_, _ = v.vt.Write([]byte(data))
	repaintViewport(w, v.vt, v.cols, v.rows)
}

// resizeRecording applies a mid-cast "r" (resize) event to the virtual
// screen. The real terminal is left untouched: it is already too small to
// host the recording, which is why cropping is active.
func (v *viewport) resizeRecording(w io.Writer, cols, rows int) {
	v.recCols, v.recRow = cols, rows
	v.vt.Resize(cols, rows)
	repaintViewport(w, v.vt, v.cols, v.rows)
}

type cellAttr struct {
	mode   int16
	fg, bg vt10x.Color
}

// colorSGR renders a vt10x.Color as an SGR extended-color parameter list.
// Values below 256 are ANSI/xterm-256 palette indices; vt10x encodes 24-bit
// colors (from a "38;2;r;g;b" source sequence) as r<<16|g<<8|b, which this
// unpacks back into an SGR truecolor sequence.
func colorSGR(prefix string, c vt10x.Color) string {
	if c < 256 {
		return fmt.Sprintf("%s;5;%d", prefix, c)
	}
	r := (c >> 16) & 0xff
	g := (c >> 8) & 0xff
	b := c & 0xff
	return fmt.Sprintf("%s;2;%d;%d;%d", prefix, r, g, b)
}

func sgrForAttr(a cellAttr) string {
	codes := []string{"0"}
	if a.mode&vtAttrBold != 0 {
		codes = append(codes, "1")
	}
	if a.mode&vtAttrItalic != 0 {
		codes = append(codes, "3")
	}
	if a.mode&vtAttrUnderline != 0 {
		codes = append(codes, "4")
	}
	if a.mode&vtAttrBlink != 0 {
		codes = append(codes, "5")
	}
	if a.mode&vtAttrReverse != 0 {
		codes = append(codes, "7")
	}
	if a.fg != vt10x.DefaultFG {
		codes = append(codes, colorSGR("38", a.fg))
	}
	if a.bg != vt10x.DefaultBG {
		codes = append(codes, colorSGR("48", a.bg))
	}
	return "\033[" + strings.Join(codes, ";") + "m"
}

// repaintViewport redraws the visible visibleCols x visibleRows crop of
// vt's virtual screen to w from scratch: cell colors/attributes, characters,
// and finally the cursor. A full repaint (rather than a diff against the
// previous frame) keeps this simple and correct; recorded TUI screens are
// small enough that the extra bytes are not a practical concern.
func repaintViewport(w io.Writer, vt vt10x.View, visibleCols, visibleRows int) {
	vcols, vrows := vt.Size()
	cols := min(visibleCols, vcols)
	rows := min(visibleRows, vrows)

	var b strings.Builder
	b.WriteString("\033[?25l\033[H")
	cur := cellAttr{fg: vt10x.DefaultFG, bg: vt10x.DefaultBG}
	first := true
	for y := 0; y < rows; y++ {
		if y > 0 {
			b.WriteString("\r\n")
		}
		for x := 0; x < cols; x++ {
			g := vt.Cell(x, y)
			a := cellAttr{mode: g.Mode, fg: g.FG, bg: g.BG}
			if first || a != cur {
				b.WriteString(sgrForAttr(a))
				cur = a
				first = false
			}
			r := g.Char
			if r == 0 {
				r = ' '
			}
			b.WriteRune(r)
		}
	}
	b.WriteString("\033[0m")

	cursor := vt.Cursor()
	if vt.CursorVisible() && cursor.X < cols && cursor.Y < rows {
		fmt.Fprintf(&b, "\033[%d;%dH\033[?25h", cursor.Y+1, cursor.X+1)
	}
	w.Write([]byte(b.String()))
}
