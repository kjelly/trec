package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

// secretRedactor replaces declared secret values with stable labels. It does
// not try to guess secrets: callers must declare them with --secret-env or
// TEXT_ENV before any value can be protected.
type secretRedactor struct {
	mu       sync.RWMutex
	entries  []secretEntry
	replacer *strings.Replacer
	maxBytes int
}

type secretEntry struct {
	name  string
	value string
}

func newSecretRedactor(envNames []string) (*secretRedactor, error) {
	r := &secretRedactor{}
	for _, name := range envNames {
		if err := r.addEnv(name); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func validEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i, c := range name {
		if !(c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || (i > 0 && c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}

func (r *secretRedactor) addEnv(name string) error {
	if !validEnvName(name) {
		return fmt.Errorf("invalid environment variable name %q", name)
	}
	value, ok := os.LookupEnv(name)
	if !ok || value == "" {
		return fmt.Errorf("secret environment variable %s is not set or is empty", name)
	}
	r.add(name, value)
	return nil
}

// addFile reads an exact text value from path and registers it under a
// generated, non-sensitive label. It deliberately does not trim a trailing
// newline: callers must control exactly what is typed into the PTY.
func (r *secretRedactor) addFile(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("secret file path is empty")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read secret file %q: %w", path, err)
	}
	if len(data) == 0 {
		return "", fmt.Errorf("secret file %q is empty", path)
	}
	if !utf8.Valid(data) {
		return "", fmt.Errorf("secret file %q is not valid UTF-8 text", path)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	base := "file"
	name := base
	for n := 2; ; n++ {
		used := false
		for _, entry := range r.entries {
			if entry.name == name {
				used = true
				break
			}
		}
		if !used {
			break
		}
		name = fmt.Sprintf("%s-%d", base, n)
	}
	r.entries = append(r.entries, secretEntry{name: name, value: string(data)})
	r.rebuildLocked()
	return string(data), nil
}

func addSecretFileSpecs(r *secretRedactor, specs []string) error {
	for _, spec := range specs {
		name, path, ok := strings.Cut(spec, "=")
		if !ok || !validEnvName(name) || strings.TrimSpace(path) == "" {
			return fmt.Errorf("--secret-file needs NAME=path, got %q", spec)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read secret file %q: %w", path, err)
		}
		if len(data) == 0 || !utf8.Valid(data) {
			return fmt.Errorf("secret file %q must contain non-empty UTF-8 text", path)
		}
		r.add(name, string(data))
	}
	return nil
}

func (r *secretRedactor) add(name, value string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.entries {
		if r.entries[i].name == name {
			r.entries[i].value = value
			r.rebuildLocked()
			return
		}
	}
	r.entries = append(r.entries, secretEntry{name: name, value: value})
	r.rebuildLocked()
}

func (r *secretRedactor) rebuildLocked() {
	entries := append([]secretEntry(nil), r.entries...)
	sort.SliceStable(entries, func(i, j int) bool { return len(entries[i].value) > len(entries[j].value) })
	pairs := make([]string, 0, len(entries)*2)
	r.maxBytes = 0
	for _, entry := range entries {
		pairs = append(pairs, entry.value, "<redacted:"+entry.name+">")
		if len(entry.value) > r.maxBytes {
			r.maxBytes = len(entry.value)
		}
	}
	if len(pairs) == 0 {
		r.replacer = nil
		return
	}
	r.replacer = strings.NewReplacer(pairs...)
}

func (r *secretRedactor) RedactString(s string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.replacer == nil {
		return s
	}
	return r.replacer.Replace(s)
}

func (r *secretRedactor) maxSecretBytes() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.maxBytes
}

// safeOutputCut moves a proposed stream flush boundary before every complete
// secret that crosses it. Without this adjustment, writing the apparently
// safe prefix could reveal the first half of a secret whose remainder is in
// the retained tail.
func (r *secretRedactor) safeOutputCut(s string, cut int) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for changed := true; changed; {
		changed = false
		for _, entry := range r.entries {
			for from := 0; from < len(s); {
				at := strings.Index(s[from:], entry.value)
				if at < 0 {
					break
				}
				at += from
				end := at + len(entry.value)
				if at < cut && end > cut {
					cut = at
					changed = true
				}
				from = at + 1
			}
		}
	}
	return cut
}

// recordingWriter is the only writer used while making a new cast. Output is
// delayed by at most the longest declared secret minus one byte so a secret
// split across PTY reads is still redacted before it reaches disk.
type recordingWriter struct {
	bw        *bufio.Writer
	mu        *sync.Mutex
	redactor  *secretRedactor
	pending   string
	pendingAt float64
}

func newRecordingWriter(bw *bufio.Writer, mu *sync.Mutex, redactor *secretRedactor) *recordingWriter {
	return &recordingWriter{bw: bw, mu: mu, redactor: redactor}
}

func (rw *recordingWriter) writeHeader(hdr castHeader) {
	hdr.Command = rw.redactor.RedactString(hdr.Command)
	hdr.CommandLabel = rw.redactor.RedactString(hdr.CommandLabel)
	hdr.Title = rw.redactor.RedactString(hdr.Title)
	for k, v := range hdr.Env {
		hdr.Env[k] = rw.redactor.RedactString(v)
	}
	b, _ := json.Marshal(hdr)
	rw.mu.Lock()
	defer rw.mu.Unlock()
	_, _ = rw.bw.Write(b)
	_ = rw.bw.WriteByte('\n')
}

func (rw *recordingWriter) event(elapsed float64, eventType, data string) {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	rw.writeEventLocked(elapsed, eventType, rw.redactor.RedactString(data))
}

func (rw *recordingWriter) output(elapsed float64, data string) {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	if rw.pending == "" {
		rw.pendingAt = elapsed
	}
	rw.pending += data
	keep := rw.redactor.maxSecretBytes() - 1
	if keep <= 0 || len(rw.pending) <= keep {
		return
	}
	cut := len(rw.pending) - keep
	cut = rw.redactor.safeOutputCut(rw.pending, cut)
	for cut > 0 && cut < len(rw.pending) && !utf8.RuneStart(rw.pending[cut]) {
		cut--
	}
	rw.writeEventLocked(rw.pendingAt, "o", rw.redactor.RedactString(rw.pending[:cut]))
	rw.pending = rw.pending[cut:]
	rw.pendingAt = elapsed
}

func (rw *recordingWriter) flushOutput() {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	if rw.pending != "" {
		rw.writeEventLocked(rw.pendingAt, "o", rw.redactor.RedactString(rw.pending))
		rw.pending = ""
	}
}

func (rw *recordingWriter) flush() {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	_ = rw.bw.Flush()
}

func (rw *recordingWriter) writeEventLocked(elapsed float64, eventType, data string) {
	b, _ := json.Marshal([]any{elapsed, eventType, data})
	_, _ = rw.bw.Write(b)
	_ = rw.bw.WriteByte('\n')
}

func (rw *recordingWriter) dumpScreen(w io.Writer, cols, rows int, lines []string) {
	fmt.Fprintf(w, "---- screen %dx%d ----\n", cols, rows)
	for _, line := range lines {
		fmt.Fprintf(w, "| %s\n", rw.redactor.RedactString(line))
	}
	fmt.Fprintln(w, "---- end screen ----")
}
