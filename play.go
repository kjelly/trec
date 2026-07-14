package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/spf13/pflag"
	"golang.org/x/term"
)

type castEvent struct {
	sec  float64
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
	speed     float64
	idleLimit float64

	mu         sync.Mutex
	startWall  time.Time
	pauseTotal time.Duration
	pauseAt    time.Time
	isPaused   bool

	spaceCh chan struct{}
	quitCh  chan struct{}
}

func newPlayer(speed, idleLimit float64) *player {
	return &player{
		speed:     speed,
		idleLimit: idleLimit,
		spaceCh:   make(chan struct{}, 1),
		quitCh:    make(chan struct{}),
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
	flags.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: trec play [options] <file.cast>")
		fmt.Fprintln(os.Stderr, "\nOptions:")
		flags.PrintDefaults()
		fmt.Fprintln(os.Stderr, "\nDuring playback:  space = pause/resume   q = quit")
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

func playFile(p *player, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)

	if !sc.Scan() {
		return fmt.Errorf("empty file")
	}
	var hdr castHeader
	if err := json.Unmarshal(sc.Bytes(), &hdr); err != nil {
		return fmt.Errorf("invalid header: %w", err)
	}

	var events []castEvent
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		sec, typ, data, err := parseCastLine(line)
		if err != nil || typ != "o" {
			continue
		}
		events = append(events, castEvent{sec: sec, data: data})
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read: %w", err)
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
	fmt.Fprint(os.Stderr, "\r\n")

	p.reset()

	for i := range events {
		select {
		case <-p.quitCh:
			return nil
		default:
		}
		if !p.advance(events[i].sec) {
			return nil
		}
		os.Stdout.WriteString(events[i].data)
	}
	return nil
}
