package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/pflag"
	"golang.org/x/term"
)

type castEvent struct {
	sec  float64
	typ  string
	data string
}

func parseCastLine(line []byte) (sec float64, typ, data string, err error) {
	var raw []json.RawMessage
	if e := json.Unmarshal(line, &raw); e != nil || len(raw) < 3 {
		return 0, "", "", fmt.Errorf("bad event line")
	}
	json.Unmarshal(raw[0], &sec)
	json.Unmarshal(raw[1], &typ)
	json.Unmarshal(raw[2], &data)
	return
}

const (
	minSpeed = 0.125
	maxSpeed = 16.0
)

// playCmd is a control action decoded from a keypress.
type playCmd int

const (
	cmdTogglePause playCmd = iota
	cmdStepFwd
	cmdStepBack
	cmdSpeedUp
	cmdSpeedDown
	cmdNextMarker
	cmdPrevMarker
	cmdRestart
)

// playClock tracks the playback position in cast-time seconds. Pause and
// mid-playback speed changes work by folding the elapsed segment into base
// and re-anchoring against wall time, so position never jumps.
type playClock struct {
	speed  float64
	base   float64 // cast-time position at anchor
	anchor time.Time
	paused bool
}

func (c *playClock) pos() float64 {
	if c.paused {
		return c.base
	}
	return c.base + time.Since(c.anchor).Seconds()*c.speed
}

func (c *playClock) setPaused(v bool) {
	if c.paused == v {
		return
	}
	c.base = c.pos()
	c.anchor = time.Now()
	c.paused = v
}

func (c *playClock) setSpeed(s float64) {
	c.base = c.pos()
	c.anchor = time.Now()
	c.speed = s
}

// seek moves the clock to sec without changing pause state.
func (c *playClock) seek(sec float64) {
	c.base = sec
	c.anchor = time.Now()
}

// player owns playback state: the clock, the control channels fed by the
// keyboard goroutine, and the transient message shown in the status bar.
type player struct {
	clk           playClock
	idleLimit     float64
	pauseOnMarker bool
	loop          bool

	cmdCh  chan playCmd
	quitCh chan struct{}

	totalSec float64
	atEnd    bool
	msg      string // last keystroke / marker label shown in the status bar
	msgStyle string // ANSI fg color for msg ("36" keystroke, "35" marker)
	lastDraw time.Time
}

func newPlayer(speed, idleLimit float64) *player {
	return &player{
		clk:       playClock{speed: speed, anchor: time.Now()},
		idleLimit: idleLimit,
		cmdCh:     make(chan playCmd, 8),
		quitCh:    make(chan struct{}),
	}
}

func (p *player) send(c playCmd) {
	select {
	case p.cmdCh <- c:
	default:
	}
}

// keyDecoder is a byte-level state machine that extracts control keys from
// stdin. It must swallow whole escape sequences (CSI/OSC/DCS/SS3), because
// stdin also carries the terminal's replies to queries embedded in the
// recording (e.g. "\x1b]11;rgb:..." background-color reports) — naive
// per-byte matching would misread those replies as keypresses. State is kept
// across Read calls since a sequence can be split between reads.
type keyDecoder struct {
	state int    // 0 normal, 1 after ESC, 2 in CSI, 3 in OSC/DCS, 4 after SS3
	osc   bool   // in state 3: true when terminated by BEL as well as ST
	csi   []byte // parameter bytes collected inside the current CSI
}

// feed consumes one input byte and returns (cmd, ok) when it completes a
// recognized key.
func (d *keyDecoder) feed(c byte) (playCmd, bool) {
	switch d.state {
	case 1: // after ESC
		switch c {
		case '[':
			d.state, d.csi = 2, d.csi[:0]
		case ']', 'P', 'X', '^', '_': // OSC / DCS / SOS / PM / APC
			d.state, d.osc = 3, c == ']'
		case 'O':
			d.state = 4
		case 0x1b:
			// stay: could be ESC introducing a new sequence
		default:
			d.state = 0 // lone ESC + char: ignore both
		}
		return 0, false

	case 2: // in CSI: collect until final byte 0x40–0x7E
		if c >= 0x40 && c <= 0x7e {
			d.state = 0
			if len(d.csi) == 0 { // plain arrow keys only, not query replies
				switch c {
				case 'A':
					return cmdSpeedUp, true
				case 'B':
					return cmdSpeedDown, true
				case 'C':
					return cmdStepFwd, true
				case 'D':
					return cmdStepBack, true
				}
			}
			return 0, false
		}
		if len(d.csi) < 32 {
			d.csi = append(d.csi, c)
		}
		return 0, false

	case 3: // in OSC/DCS: swallow until BEL or ST (ESC \)
		if c == 0x07 && d.osc {
			d.state = 0
		} else if c == 0x1b {
			d.state = 5
		}
		return 0, false

	case 5: // saw ESC inside OSC/DCS: expect ST terminator
		if c == '\\' {
			d.state = 0
		} else {
			d.state = 3
		}
		return 0, false

	case 4: // SS3 (ESC O): one final char, e.g. application-mode arrows
		d.state = 0
		switch c {
		case 'A':
			return cmdSpeedUp, true
		case 'B':
			return cmdSpeedDown, true
		case 'C':
			return cmdStepFwd, true
		case 'D':
			return cmdStepBack, true
		}
		return 0, false
	}

	// normal state
	if c == 0x1b {
		d.state = 1
		return 0, false
	}
	switch c {
	case ' ':
		return cmdTogglePause, true
	case '.', 'l':
		return cmdStepFwd, true
	case ',', 'h':
		return cmdStepBack, true
	case '+', '=', ']':
		return cmdSpeedUp, true
	case '-', '_', '[':
		return cmdSpeedDown, true
	case 'n':
		return cmdNextMarker, true
	case 'N', 'p':
		return cmdPrevMarker, true
	case 'g', '0':
		return cmdRestart, true
	}
	return 0, false
}

// readKeys decodes control keys from stdin and feeds them to the playback
// loop. Runs as a goroutine for the lifetime of the program.
func (p *player) readKeys() {
	var dec keyDecoder
	buf := make([]byte, 64)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil || n == 0 {
			return
		}
		for _, c := range buf[:n] {
			if dec.state == 0 {
				switch c {
				case 'q', 'Q', 3, 4: // q / Q / Ctrl-C / Ctrl-D
					close(p.quitCh)
					return
				}
			}
			if cmd, ok := dec.feed(c); ok {
				p.send(cmd)
			}
		}
	}
}

func fmtSpeed(s float64) string {
	return strconv.FormatFloat(s, 'f', -1, 64)
}

func runeTrunc(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// drawStatus renders the persistent control bar on the terminal's bottom
// row: state, position/duration, speed, a progress bar, the last
// keystroke/marker, and key hints. It uses only reverse video and the
// terminal's own ANSI palette, so it stays readable on both light and dark
// backgrounds. Redraws are throttled unless force is set.
func (p *player) drawStatus(cur float64, force bool) {
	if !force && time.Since(p.lastDraw) < 100*time.Millisecond {
		return
	}
	p.lastDraw = time.Now()

	w, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w <= 0 {
		w = 80
	}

	state := "▶"
	switch {
	case p.atEnd:
		state = "END"
	case p.clk.paused:
		state = "⏸"
	}
	left := fmt.Sprintf(" %s %s/%s ×%s ", state, fmtDur(cur), fmtDur(p.totalSec), fmtSpeed(p.clk.speed))
	hints := " space:pause  ←/→:step  ↑/↓:speed  n/N:marker  g:restart  q:quit "
	msg := p.msg
	if msg != "" {
		msg = " " + msg + " "
	}

	avail := w - 1 - utf8.RuneCountInString(left)
	msg = runeTrunc(msg, avail)
	avail -= utf8.RuneCountInString(msg)
	if utf8.RuneCountInString(hints) > avail {
		hints = ""
	}
	barW := avail - utf8.RuneCountInString(hints)
	bar := ""
	if barW > 0 {
		frac := 0.0
		if p.totalSec > 0 {
			frac = cur / p.totalSec
		}
		if frac > 1 {
			frac = 1
		}
		fill := int(frac * float64(barW))
		bar = strings.Repeat("━", fill) + strings.Repeat(" ", barW-fill)
	}

	var b strings.Builder
	b.WriteString("\033[s\033[999;1H\033[2K\033[7m")
	b.WriteString(left)
	b.WriteString(bar)
	if msg != "" {
		if p.msgStyle != "" {
			b.WriteString("\033[" + p.msgStyle + "m")
		}
		b.WriteString(msg)
		if p.msgStyle != "" {
			b.WriteString("\033[39m")
		}
	}
	b.WriteString(hints)
	b.WriteString("\033[0m\033[u")
	os.Stderr.WriteString(b.String())
}

func clearStatusLine() {
	fmt.Fprint(os.Stderr, "\033[s\033[999;1H\033[2K\033[u")
}

// visualizeKeys renders raw stdin bytes ("i" events) as a human-readable
// caret/symbol notation, e.g. Ctrl-C -> ^C, Enter -> ⏎, arrow keys -> ↑↓←→.
func visualizeKeys(data string) string {
	var b strings.Builder
	i := 0
	for i < len(data) {
		c := data[i]
		switch {
		case c == 0x1b && strings.HasPrefix(data[i:], "\x1b[A"):
			b.WriteString("↑")
			i += 3
		case c == 0x1b && strings.HasPrefix(data[i:], "\x1b[B"):
			b.WriteString("↓")
			i += 3
		case c == 0x1b && strings.HasPrefix(data[i:], "\x1b[C"):
			b.WriteString("→")
			i += 3
		case c == 0x1b && strings.HasPrefix(data[i:], "\x1b[D"):
			b.WriteString("←")
			i += 3
		case c == 0x1b:
			b.WriteString("␛")
			i++
		case c == '\r' || c == '\n':
			b.WriteString("⏎")
			i++
		case c == '\t':
			b.WriteString("⇥")
			i++
		case c == 0x7f || c == 0x08:
			b.WriteString("⌫")
			i++
		case c < 0x20:
			b.WriteByte('^')
			b.WriteByte(c + 64)
			i++
		case c < 0x80:
			b.WriteByte(c)
			i++
		default:
			r, size := utf8.DecodeRuneInString(data[i:])
			if r == utf8.RuneError && size <= 1 {
				b.WriteByte('?')
				i++
			} else {
				b.WriteRune(r)
				i += size
			}
		}
	}
	return b.String()
}

func fmtDur(sec float64) string {
	t := int(sec)
	if h := t / 3600; h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, (t%3600)/60, t%60)
	}
	return fmt.Sprintf("%d:%02d", t/60, t%60)
}

func runPlay(args []string) {
	flags := pflag.NewFlagSet("play", pflag.ExitOnError)
	speed := flags.Float64P("speed", "s", 1.0, "initial playback speed multiplier (e.g. 2.0 = double speed)")
	idleLimit := flags.Float64P("idle-time-limit", "i", 5.0, "cap idle gaps between events to N seconds (0 = no cap)")
	loop := flags.BoolP("loop", "l", false, "loop playback continuously")
	pauseOnMarker := flags.Bool("pause-on-marker", false, "automatically pause playback when a marker is reached")
	flags.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: trec play [options] <file.cast>")
		fmt.Fprintln(os.Stderr, "\nOptions:")
		flags.PrintDefaults()
		fmt.Fprintln(os.Stderr, `
During playback:
  space          pause / resume
  → . l          step forward one frame (pauses)
  ← , h          step back one frame (pauses)
  ↑ + = ]        speed up (×2, max 16)
  ↓ - _ [        slow down (÷2, min 0.125)
  n / N          jump to next / previous marker
  g 0            restart from the beginning
  q Ctrl-C       quit`)
	}
	flags.Parse(args)

	files := flags.Args()
	if len(files) == 0 {
		flags.Usage()
		os.Exit(1)
	}

	if !term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprintln(os.Stderr, "trec play: stdin must be an interactive terminal")
		os.Exit(1)
	}

	old, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		fmt.Fprintf(os.Stderr, "raw mode: %v\n", err)
		os.Exit(1)
	}
	defer term.Restore(int(os.Stdin.Fd()), old)

	if *speed < minSpeed {
		*speed = minSpeed
	}
	if *speed > maxSpeed {
		*speed = maxSpeed
	}
	p := newPlayer(*speed, *idleLimit)
	p.pauseOnMarker = *pauseOnMarker
	p.loop = *loop

	go p.readKeys()

	for {
		if err := playFile(p, files[0]); err != nil {
			term.Restore(int(os.Stdin.Fd()), old)
			fmt.Fprintf(os.Stderr, "\r\nerror: %v\r\n", err)
			os.Exit(1)
		}

		select {
		case <-p.quitCh:
			goto done
		default:
		}
		if !*loop {
			break
		}
	}

done:
	clearStatusLine()
	term.Restore(int(os.Stdin.Fd()), old)
	fmt.Fprint(os.Stderr, "\r\n")
}

// findMarkerIndex returns the index of the next ("m", dir=1) or previous
// ("m", dir=-1) marker event relative to cur, or -1 if there is none.
func findMarkerIndex(events []castEvent, cur, dir int) int {
	if dir > 0 {
		for k := cur + 1; k < len(events); k++ {
			if events[k].typ == "m" {
				return k
			}
		}
		return -1
	}
	for k := cur - 1; k >= 0; k-- {
		if events[k].typ == "m" {
			return k
		}
	}
	return -1
}

// fastForwardTo replays the "o" events in events[from:to] to stdout
// instantly, with no timing delay, to reconstruct screen state when
// jumping backward or to a marker.
func fastForwardTo(events []castEvent, from, to int) {
	for k := from; k < to; k++ {
		if events[k].typ == "o" {
			os.Stdout.WriteString(events[k].data)
		}
	}
}

// apply renders one event: output goes to the terminal, keystrokes and
// markers become the status-bar message.
func (p *player) apply(e castEvent) {
	switch e.typ {
	case "o":
		os.Stdout.WriteString(e.data)
	case "i":
		p.msg, p.msgStyle = "⌨ "+visualizeKeys(e.data), "36"
	case "m":
		p.msg, p.msgStyle = "⚑ "+e.data, "35"
	}
}

// handleCmd executes one control command against the event list. i is the
// index of the next unapplied event; the (possibly moved) index is returned.
func (p *player) handleCmd(cmd playCmd, events []castEvent, i int) int {
	switch cmd {
	case cmdTogglePause:
		p.clk.setPaused(!p.clk.paused)

	case cmdSpeedUp:
		if s := p.clk.speed * 2; s <= maxSpeed {
			p.clk.setSpeed(s)
		} else {
			p.clk.setSpeed(maxSpeed)
		}

	case cmdSpeedDown:
		if s := p.clk.speed / 2; s >= minSpeed {
			p.clk.setSpeed(s)
		} else {
			p.clk.setSpeed(minSpeed)
		}

	case cmdStepFwd:
		// Apply events up to and including the next visible output chunk.
		p.clk.setPaused(true)
		for i < len(events) {
			e := events[i]
			p.apply(e)
			i++
			if e.typ == "o" {
				break
			}
		}
		if i > 0 {
			p.clk.seek(events[i-1].sec)
		}

	case cmdStepBack:
		// Un-apply the most recent output chunk by replaying from the start.
		p.clk.setPaused(true)
		k := i - 1
		for k >= 0 && events[k].typ != "o" {
			k--
		}
		if k >= 0 {
			i = k
			fmt.Fprint(os.Stdout, "\033[2J\033[H")
			fastForwardTo(events, 0, i)
			if i > 0 {
				p.clk.seek(events[i-1].sec)
			} else {
				p.clk.seek(0)
			}
		}

	case cmdNextMarker, cmdPrevMarker:
		dir := 1
		if cmd == cmdPrevMarker {
			dir = -1
		}
		target := findMarkerIndex(events, i, dir)
		if target < 0 {
			p.msg, p.msgStyle = "no marker", ""
			return i
		}
		if dir < 0 {
			fmt.Fprint(os.Stdout, "\033[2J\033[H")
			fastForwardTo(events, 0, target)
		} else {
			fastForwardTo(events, i, target)
		}
		i = target
		p.clk.seek(events[target].sec)
		p.msg, p.msgStyle = "⚑ "+events[target].data, "35"

	case cmdRestart:
		i = 0
		fmt.Fprint(os.Stdout, "\033[2J\033[H")
		p.clk.seek(0)
		p.clk.setPaused(false)
		p.msg, p.msgStyle = "", ""
	}
	return i
}

func playFile(p *player, path string) error {
	hdr, allEvents, err := loadCastFile(path)
	if err != nil {
		return err
	}

	var events []castEvent
	markerCount := 0
	for _, e := range allEvents {
		if e.typ == "o" || e.typ == "i" || e.typ == "m" {
			events = append(events, e)
			if e.typ == "m" {
				markerCount++
			}
		}
	}
	if len(events) == 0 {
		return nil
	}

	// Cap idle gaps between events.
	if p.idleLimit > 0 {
		prev, adjusted := 0.0, 0.0
		for i := range events {
			gap := events[i].sec - prev
			if gap > p.idleLimit {
				gap = p.idleLimit
			}
			adjusted += gap
			prev = events[i].sec
			events[i].sec = adjusted
		}
	}

	p.totalSec = events[len(events)-1].sec
	p.atEnd = false
	p.msg, p.msgStyle = "", ""

	// Warn when current terminal is smaller than the recording.
	cw, ch, _ := term.GetSize(int(os.Stdin.Fd()))
	if cw < hdr.Width || ch < hdr.Height {
		fmt.Fprintf(os.Stderr, "\r\033[33m[warning] recording %dx%d, terminal %dx%d\033[0m\r\n",
			hdr.Width, hdr.Height, cw, ch)
	}

	// Info line + clear screen.
	title := hdr.Title
	if title == "" {
		title = path
	}
	fmt.Fprintf(os.Stderr, "\033[2J\033[H\033[1m%s\033[0m  %s\r\n", title, fmtDur(p.totalSec))
	if hdr.Command != "" {
		fmt.Fprintf(os.Stderr, "\033[2m$ %s\033[0m\r\n", hdr.Command)
	}
	if markerCount > 0 {
		fmt.Fprintf(os.Stderr, "\033[2m%d marker(s)\033[0m\r\n", markerCount)
	}
	fmt.Fprint(os.Stderr, "\r\n")

	p.clk.seek(0)
	p.clk.paused = false
	p.clk.anchor = time.Now()
	p.drawStatus(0, true)

	i := 0
	for {
		select {
		case <-p.quitCh:
			return nil
		default:
		}

		// End of recording: hold on the last frame so the user can still
		// step back, jump to a marker, or restart — unless looping.
		if i >= len(events) {
			if p.loop {
				return nil
			}
			p.atEnd = true
			p.clk.setPaused(true)
			p.clk.seek(p.totalSec)
			p.drawStatus(p.totalSec, true)
			select {
			case <-p.quitCh:
				return nil
			case cmd := <-p.cmdCh:
				switch cmd {
				case cmdStepBack, cmdPrevMarker, cmdRestart:
					p.atEnd = false
					i = p.handleCmd(cmd, events, i)
					p.drawStatus(p.clk.pos(), true)
				}
			}
			continue
		}

		if p.clk.paused {
			p.drawStatus(p.clk.pos(), true)
			select {
			case <-p.quitCh:
				return nil
			case cmd := <-p.cmdCh:
				i = p.handleCmd(cmd, events, i)
				p.drawStatus(p.clk.pos(), true)
			}
			continue
		}

		pos := p.clk.pos()
		if pos < events[i].sec {
			wait := min(time.Duration((events[i].sec-pos)/p.clk.speed*float64(time.Second)), 100*time.Millisecond)
			select {
			case <-p.quitCh:
				return nil
			case cmd := <-p.cmdCh:
				i = p.handleCmd(cmd, events, i)
				p.drawStatus(p.clk.pos(), true)
			case <-time.After(wait):
				p.drawStatus(p.clk.pos(), false)
			}
			continue
		}

		e := events[i]
		p.apply(e)
		i++
		if e.typ == "m" && p.pauseOnMarker {
			p.clk.setPaused(true)
		}
		p.drawStatus(p.clk.pos(), e.typ != "o")
	}
}
