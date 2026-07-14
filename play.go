package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
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

// player manages timing, pause/resume, and quit for playback.
type player struct {
	speed         float64
	idleLimit     float64
	pauseOnMarker bool

	mu          sync.Mutex
	startWall   time.Time
	pauseTotal  time.Duration
	pauseAt     time.Time
	isPaused    bool
	pendingSeek int // set by advance() when a seek key arrives: +1 next marker, -1 previous

	spaceCh chan struct{}
	quitCh  chan struct{}
	nextCh  chan struct{}
	prevCh  chan struct{}
}

func newPlayer(speed, idleLimit float64) *player {
	return &player{
		speed:     speed,
		idleLimit: idleLimit,
		spaceCh:   make(chan struct{}, 1),
		quitCh:    make(chan struct{}),
		nextCh:    make(chan struct{}, 1),
		prevCh:    make(chan struct{}, 1),
	}
}

// seekTo re-anchors the playback clock so that "elapsed" equals sec,
// without touching pause state — used after fast-forwarding to a marker.
func (p *player) seekTo(sec float64) {
	p.mu.Lock()
	p.startWall = time.Now().Add(-time.Duration(sec / p.speed * float64(time.Second)))
	p.pauseTotal = 0
	p.isPaused = false
	p.mu.Unlock()
}

// forcePause pauses playback immediately (used for --pause-on-marker) and
// blocks until the user resumes with space or quits.
func (p *player) forcePause() {
	p.mu.Lock()
	p.isPaused = true
	p.pauseAt = time.Now()
	p.mu.Unlock()
	writeStatusLine("  PAUSED at marker  (space: resume   q: quit)  ")

	select {
	case <-p.quitCh:
		return
	case <-p.spaceCh:
		p.mu.Lock()
		p.isPaused = false
		p.pauseTotal += time.Since(p.pauseAt)
		p.mu.Unlock()
		clearStatusLine()
	}
}

func (p *player) reset() {
	p.mu.Lock()
	p.startWall = time.Now()
	p.pauseTotal = 0
	p.isPaused = false
	p.mu.Unlock()
}

// advance sleeps until the playback clock reaches sec. Returns false to abort.
func (p *player) advance(sec float64) bool {
	target := time.Duration(sec / p.speed * float64(time.Second))

	for {
		p.mu.Lock()
		isPaused := p.isPaused
		elapsed := time.Since(p.startWall) - p.pauseTotal
		if isPaused {
			elapsed -= time.Since(p.pauseAt)
		}
		p.mu.Unlock()

		if isPaused {
			select {
			case <-p.quitCh:
				return false
			case <-p.nextCh:
				p.pendingSeek = 1
				return true
			case <-p.prevCh:
				p.pendingSeek = -1
				return true
			case <-p.spaceCh: // resume
				p.mu.Lock()
				p.isPaused = false
				p.pauseTotal += time.Since(p.pauseAt)
				p.mu.Unlock()
				clearStatusLine()
			}
			continue
		}

		remaining := target - elapsed
		if remaining <= 0 {
			return true
		}

		chunk := remaining
		if chunk > 30*time.Millisecond {
			chunk = 30 * time.Millisecond
		}

		select {
		case <-time.After(chunk):
		case <-p.quitCh:
			return false
		case <-p.nextCh:
			p.pendingSeek = 1
			return true
		case <-p.prevCh:
			p.pendingSeek = -1
			return true
		case <-p.spaceCh: // pause
			p.mu.Lock()
			p.isPaused = true
			p.pauseAt = time.Now()
			p.mu.Unlock()
			writeStatusLine("  PAUSED  (space: resume   q: quit)  ")
		}
	}
}

// writeStatusLine prints msg on the bottom line without disturbing the main content.
func writeStatusLine(msg string) {
	// save cursor → last row → clear line → write reversed → restore cursor
	fmt.Fprintf(os.Stderr, "\033[s\033[999;1H\033[2K\033[7m%s\033[0m\033[u", msg)
}

func clearStatusLine() {
	fmt.Fprint(os.Stderr, "\033[s\033[999;1H\033[2K\033[u")
}

// writeKeyLine shows the most recent recorded keystroke on the bottom line
// without disturbing the main content or cursor position.
func writeKeyLine(msg string) {
	fmt.Fprintf(os.Stderr, "\033[s\033[999;1H\033[2K\033[36;7m ⌨ %s \033[0m\033[u", msg)
}

// writeMarkerLine shows a marker's label on the bottom line, distinguished
// from the keystroke banner by color.
func writeMarkerLine(label string) {
	fmt.Fprintf(os.Stderr, "\033[s\033[999;1H\033[2K\033[35;7m ⚑ %s \033[0m\033[u", label)
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
	speed := flags.Float64P("speed", "s", 1.0, "playback speed multiplier (e.g. 2.0 = double speed)")
	idleLimit := flags.Float64P("idle-time-limit", "i", 5.0, "cap idle gaps between events to N seconds (0 = no cap)")
	loop := flags.BoolP("loop", "l", false, "loop playback continuously")
	pauseOnMarker := flags.Bool("pause-on-marker", false, "automatically pause playback when a marker is reached")
	flags.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: trec play [options] <file.cast>")
		fmt.Fprintln(os.Stderr, "\nOptions:")
		flags.PrintDefaults()
		fmt.Fprintln(os.Stderr, "\nDuring playback:  space = pause/resume   n/N = next/previous marker   q = quit")
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

	p := newPlayer(*speed, *idleLimit)
	p.pauseOnMarker = *pauseOnMarker

	// Read keys in a background goroutine.
	go func() {
		buf := make([]byte, 4)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil || n == 0 {
				return
			}
			switch buf[0] {
			case ' ':
				select {
				case p.spaceCh <- struct{}{}:
				default:
				}
			case 'n':
				select {
				case p.nextCh <- struct{}{}:
				default:
				}
			case 'N':
				select {
				case p.prevCh <- struct{}{}:
				default:
				}
			case 'q', 'Q', 3, 4: // q / Q / Ctrl-C / Ctrl-D
				select {
				case <-p.quitCh: // already closed
				default:
					close(p.quitCh)
				}
				return
			}
		}
	}()

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
// jumping to a marker.
func fastForwardTo(events []castEvent, from, to int) {
	for k := from; k < to; k++ {
		if events[k].typ == "o" {
			os.Stdout.WriteString(events[k].data)
		}
	}
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

	totalSec := events[len(events)-1].sec

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
	fmt.Fprintf(os.Stderr, "\033[2J\033[H\033[1m%s\033[0m  %s  speed %.1fx  space=pause  q=quit\r\n",
		title, fmtDur(totalSec), p.speed)
	if hdr.Command != "" {
		fmt.Fprintf(os.Stderr, "\033[2m$ %s\033[0m\r\n", hdr.Command)
	}
	if markerCount > 0 {
		fmt.Fprintf(os.Stderr, "\033[2m%d marker(s)  n/N=jump\033[0m\r\n", markerCount)
	}
	fmt.Fprint(os.Stderr, "\r\n")

	p.reset()

	i := 0
	for i < len(events) {
		select {
		case <-p.quitCh:
			return nil
		default:
		}

		if !p.advance(events[i].sec) {
			return nil
		}

		if p.pendingSeek != 0 {
			dir := p.pendingSeek
			p.pendingSeek = 0
			target := findMarkerIndex(events, i, dir)
			if target < 0 {
				continue
			}
			if dir < 0 {
				fmt.Fprint(os.Stdout, "\033[2J\033[H")
				fastForwardTo(events, 0, target)
			} else {
				fastForwardTo(events, i, target)
			}
			i = target
			p.seekTo(events[target].sec)
			continue
		}

		switch events[i].typ {
		case "i":
			writeKeyLine(visualizeKeys(events[i].data))
		case "m":
			writeMarkerLine(events[i].data)
			if p.pauseOnMarker {
				p.forcePause()
				select {
				case <-p.quitCh:
					return nil
				default:
				}
			}
		default:
			os.Stdout.WriteString(events[i].data)
		}
		i++
	}
	return nil
}
