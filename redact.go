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

func (sr *secretRedactor) hasSecrets() bool {
	sr.mu.RLock()
	defer sr.mu.RUnlock()
	return len(sr.entries) > 0
}

func (sr *secretRedactor) AnySecretIn(s string) bool {
	if !sr.hasSecrets() {
		return false
	}
	sr.mu.RLock()
	defer sr.mu.RUnlock()

	sStripped := stripANSI(s)
	sNoSpace := strings.ReplaceAll(sStripped, " ", "")
	sNoSpace = strings.ReplaceAll(sNoSpace, "\t", "")
	sNoSpace = strings.ReplaceAll(sNoSpace, "\r", "")
	sNoSpace = strings.ReplaceAll(sNoSpace, "\n", "")

	for _, entry := range sr.entries {
		if strings.Contains(s, entry.value) {
			return true
		}
		secretNoSpace := strings.ReplaceAll(entry.value, " ", "")
		secretNoSpace = strings.ReplaceAll(secretNoSpace, "\t", "")
		secretNoSpace = strings.ReplaceAll(secretNoSpace, "\r", "")
		secretNoSpace = strings.ReplaceAll(secretNoSpace, "\n", "")
		if len(secretNoSpace) > 0 && strings.Contains(sNoSpace, secretNoSpace) {
			return true
		}
	}
	return false
}

func (sr *secretRedactor) PendingSecretPrefix(s string) bool {
	if !sr.hasSecrets() {
		return false
	}
	sr.mu.RLock()
	defer sr.mu.RUnlock()

	sStripped := stripANSI(s)
	sNoSpace := strings.ReplaceAll(sStripped, " ", "")
	sNoSpace = strings.ReplaceAll(sNoSpace, "\t", "")
	sNoSpace = strings.ReplaceAll(sNoSpace, "\r", "")
	sNoSpace = strings.ReplaceAll(sNoSpace, "\n", "")

	if len(sNoSpace) == 0 {
		return false
	}

	for _, entry := range sr.entries {
		secretNoSpace := strings.ReplaceAll(entry.value, " ", "")
		secretNoSpace = strings.ReplaceAll(secretNoSpace, "\t", "")
		secretNoSpace = strings.ReplaceAll(secretNoSpace, "\r", "")
		secretNoSpace = strings.ReplaceAll(secretNoSpace, "\n", "")

		if len(secretNoSpace) == 0 {
			continue
		}

		for i := 0; i < len(sNoSpace); i++ {
			if strings.HasPrefix(secretNoSpace, sNoSpace[i:]) {
				return true
			}
		}
	}
	return false
}

// longestPrefixSuffixLen returns the length of the longest suffix of s that is
// also a prefix of a normalized declared secret. It is used to retain the
// smallest normalized lookbehind that can still complete a split secret.
func (sr *secretRedactor) longestPrefixSuffixLen(s string) int {
	if !sr.hasSecrets() {
		return 0
	}
	sr.mu.RLock()
	defer sr.mu.RUnlock()
	ns := normalizeForSecretCheck(s)
	if len(ns) == 0 {
		return 0
	}
	max := 0
	for _, entry := range sr.entries {
		nv := normalizeForSecretCheck(entry.value)
		for i := 1; i <= len(ns) && i <= len(nv); i++ {
			if strings.HasPrefix(nv, ns[len(ns)-i:]) {
				if i > max {
					max = i
				}
			}
		}
	}
	return max
}

func normalizeForSecretCheck(s string) string {
	s = stripANSI(s)
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ReplaceAll(s, "\t", "")
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", "")
	return s
}

// findIncompleteANSIStart returns the start index of the longest suffix of s
// that is an incomplete ANSI escape sequence. If the end of s is inside an OSC
// (including the ESC of a split ST terminator), the start of that OSC is
// returned so the entire unfinished sequence can be retained across PTY reads.
// If no incomplete sequence is present, it returns len(s).
func findIncompleteANSIStart(s string) int {
	const (
		stateNormal = iota
		stateESC
		stateCSI
		stateOSC
		stateOSCST
	)
	state := stateNormal
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch state {
		case stateNormal:
			if c == '\x1b' {
				state = stateESC
				start = i
			}
		case stateESC:
			switch c {
			case '[':
				state = stateCSI
			case ']':
				state = stateOSC
			default:
				if (c >= 0x40 && c <= 0x5A) || c == 0x5C || c == 0x5F || c == 0x60 {
					state = stateNormal
				} else if c == '\x1b' {
					start = i
				} else {
					state = stateNormal
				}
			}
		case stateCSI:
			if c >= 0x40 && c <= 0x7E {
				state = stateNormal
			} else if !((c >= 0x30 && c <= 0x3F) || (c >= 0x20 && c <= 0x2F)) {
				state = stateNormal
			}
		case stateOSC:
			if c == '\x07' {
				state = stateNormal
			} else if c == '\x1b' {
				state = stateOSCST
			}
		case stateOSCST:
			if c == '\\' {
				state = stateNormal
			} else {
				state = stateOSC
			}
		}
	}
	if state == stateNormal {
		return len(s)
	}
	return start
}
func (sr *secretRedactor) anySecretInNormalized(s string) bool {
	if !sr.hasSecrets() {
		return false
	}
	sr.mu.RLock()
	defer sr.mu.RUnlock()
	ns := normalizeForSecretCheck(s)
	for _, entry := range sr.entries {
		if strings.Contains(s, entry.value) {
			return true
		}
		nv := normalizeForSecretCheck(entry.value)
		if len(nv) > 0 && strings.Contains(ns, nv) {
			return true
		}
	}
	return false
}

func buildRemovedSet(raw string) []bool {
	removed := make([]bool, len(raw))
	matches := ansiRe.FindAllStringIndex(raw, -1)
	for _, m := range matches {
		for i := m[0]; i < m[1]; i++ {
			removed[i] = true
		}
	}
	for i := 0; i < len(raw); i++ {
		if !removed[i] {
			c := raw[i]
			if c <= 0x08 || (c >= 0x0b && c <= 0x1f) || c == 0x7f {
				removed[i] = true
			}
		}
	}
	return removed
}

func rawSpanForStrippedIdx(raw string, startIdx, endIdx int) (int, int) {
	removed := buildRemovedSet(raw)
	contentCount := 0
	rawStart := -1
	rawEnd := len(raw)
	for i := 0; i < len(raw); i++ {
		if removed[i] {
			continue
		}
		if contentCount == startIdx && rawStart < 0 {
			rawStart = i
		}
		contentCount++
		if contentCount >= endIdx {
			rawEnd = i + 1
			break
		}
	}
	if rawStart < 0 {
		rawStart = len(raw)
	}
	return rawStart, rawEnd
}

func rawSpanForNormalizedIdx(raw string, startIdx, endIdx int) (int, int) {
	removed := buildRemovedSet(raw)
	normCount := 0
	rawStart := -1
	rawEnd := len(raw)
	for i := 0; i < len(raw); i++ {
		if removed[i] {
			continue
		}
		c := raw[i]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			continue
		}
		if normCount == startIdx && rawStart < 0 {
			rawStart = i
		}
		normCount++
		if normCount >= endIdx {
			rawEnd = i + 1
			break
		}
	}
	if rawStart < 0 {
		rawStart = len(raw)
	}
	return rawStart, rawEnd
}

// redactSegment represents either literal terminal output or a redaction marker
// created by the redactor. Markers created by the redactor are never inferred
// from untrusted raw output, so a child cannot spoof a marker to exempt its own
// secrets from redaction.
type redactSegment struct {
	text     string
	isMarker bool
	name     string // marker name, used when isMarker is true
}

// replaceExactInSegments replaces every non-overlapping exact occurrence of
// secret in literal segments with a marker segment for name.
func replaceExactInSegments(segments []redactSegment, secret, name string) []redactSegment {
	var out []redactSegment
	for _, seg := range segments {
		if seg.isMarker {
			out = append(out, seg)
			continue
		}
		s := seg.text
		for {
			idx := strings.Index(s, secret)
			if idx < 0 {
				if s != "" {
					out = append(out, redactSegment{text: s})
				}
				break
			}
			if idx > 0 {
				out = append(out, redactSegment{text: s[:idx]})
			}
			out = append(out, redactSegment{isMarker: true, name: name})
			s = s[idx+len(secret):]
		}
	}
	return out
}

// replaceSplitInSegments replaces occurrences of secret in literal segments,
// including those split by ANSI escape sequences (normalized=false) or by
// whitespace (normalized=true). Markers are preserved and never rescanned.
func replaceSplitInSegments(segments []redactSegment, secret, name string, normalized bool) []redactSegment {
	var out []redactSegment
	for _, seg := range segments {
		if seg.isMarker {
			out = append(out, seg)
			continue
		}
		out = append(out, splitAndReplace(seg.text, secret, name, normalized)...)
	}
	return out
}

// splitAndReplace finds every non-overlapping occurrence of secret in text and
// replaces the raw bytes that produce that secret with a marker segment for
// name. If normalized is true the secret is matched against
// normalizeForSecretCheck(text); otherwise it is matched against stripANSI(text).
func splitAndReplace(text, secret, name string, normalized bool) []redactSegment {
	var segs []redactSegment
	i := 0
	for i < len(text) {
		var idx int
		if normalized {
			idx = strings.Index(normalizeForSecretCheck(text[i:]), secret)
		} else {
			idx = strings.Index(stripANSI(text[i:]), secret)
		}
		if idx < 0 {
			segs = append(segs, redactSegment{text: text[i:]})
			break
		}
		var rawStart, rawEnd int
		if normalized {
			rs, re := rawSpanForNormalizedIdx(text[i:], idx, idx+len(secret))
			rawStart, rawEnd = i+rs, i+re
		} else {
			rs, re := rawSpanForStrippedIdx(text[i:], idx, idx+len(secret))
			rawStart, rawEnd = i+rs, i+re
		}
		if rawStart > i {
			segs = append(segs, redactSegment{text: text[i:rawStart]})
		}
		segs = append(segs, redactSegment{isMarker: true, name: name})
		i = rawEnd
	}
	return segs
}

func (sr *secretRedactor) redactOutputStream(raw string) string {
	sr.mu.RLock()
	defer sr.mu.RUnlock()
	segments := []redactSegment{{text: raw}}
	for _, entry := range sr.entries {
		secret := entry.value
		name := entry.name
		segments = replaceExactInSegments(segments, secret, name)
		segments = replaceSplitInSegments(segments, secret, name, false)
		secretNorm := normalizeForSecretCheck(secret)
		if len(secretNorm) == 0 {
			continue
		}
		segments = replaceSplitInSegments(segments, secretNorm, name, true)
	}
	var b strings.Builder
	for _, seg := range segments {
		if seg.isMarker {
			b.WriteString("<redacted:")
			b.WriteString(seg.name)
			b.WriteByte('>')
		} else {
			b.WriteString(seg.text)
		}
	}
	return b.String()
}

func (sr *secretRedactor) redactScreen(lines []string) []string {
	if !sr.hasSecrets() {
		return lines
	}

	sr.mu.RLock()
	defer sr.mu.RUnlock()

	// 1. Check line-by-line first
	for _, l := range lines {
		if sr.replacer.Replace(l) != l {
			return []string{"<screen redacted>"}
		}
	}

	// 2. Check cross-line by ignoring newlines
	joinedLines := strings.Join(lines, "")
	for _, entry := range sr.entries {
		secretNoCRLF := strings.ReplaceAll(entry.value, "\r", "")
		secretNoCRLF = strings.ReplaceAll(secretNoCRLF, "\n", "")
		if len(secretNoCRLF) > 0 && strings.Contains(joinedLines, secretNoCRLF) {
			return []string{"<screen redacted>"}
		}
	}

	// 3. Check cross-line by ignoring spaces (handles terminal wrap and spaces)
	joinedNoSpaces := strings.ReplaceAll(joinedLines, " ", "")
	joinedNoSpaces = strings.ReplaceAll(joinedNoSpaces, "\t", "")
	for _, entry := range sr.entries {
		secretNoSpaces := strings.ReplaceAll(entry.value, " ", "")
		secretNoSpaces = strings.ReplaceAll(secretNoSpaces, "\t", "")
		secretNoSpaces = strings.ReplaceAll(secretNoSpaces, "\r", "")
		secretNoSpaces = strings.ReplaceAll(secretNoSpaces, "\n", "")
		if len(secretNoSpaces) > 0 && strings.Contains(joinedNoSpaces, secretNoSpaces) {
			return []string{"<screen redacted>"}
		}
	}

	return lines
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

// normalizeSecretFileValue removes one conventional line ending. Secret files
// are commonly written by command-line generators, while text TUI controls
// treat that final newline as submission rather than credential content.
func normalizeSecretFileValue(value string) string {
	value = strings.TrimSuffix(value, "\n")
	return strings.TrimSuffix(value, "\r")
}

// addFile reads the text value sent to the PTY and registers that same value
// under a generated, non-sensitive label.
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
	value := normalizeSecretFileValue(string(data))
	if value == "" {
		return "", fmt.Errorf("secret file %q contains only a line ending", path)
	}
	r.entries = append(r.entries, secretEntry{name: name, value: value})
	r.rebuildLocked()
	return value, nil
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
		value := normalizeSecretFileValue(string(data))
		if value == "" {
			return fmt.Errorf("secret file %q contains only a line ending", path)
		}
		r.add(name, value)
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

// maxNormalizedSecretBytes returns the length of the longest declared secret
// after stripping ANSI escape sequences and removing whitespace characters.
// This is the longest normalized secret prefix that can be buffered while
// waiting for a split secret to complete.
func (r *secretRedactor) maxNormalizedSecretBytes() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	max := 0
	for _, entry := range r.entries {
		n := len(normalizeForSecretCheck(entry.value))
		if n > max {
			max = n
		}
	}
	return max
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

// recordingWriter is the only writer used while making a new cast. Normalized
// output is delayed by at most the longest declared secret minus one byte so a
// secret split across PTY reads is still redacted before it reaches disk.
// Complete ANSI escape sequences that follow a retained prefix are emitted
// immediately, and incomplete sequences are deferred to the next read so the
// raw pending buffer stays bounded and cross-read ANSI state is preserved.
type recordingWriter struct {
	bw            *bufio.Writer
	mu            *sync.Mutex
	redactor      *secretRedactor
	pending       string
	pendingAt     float64
	pendingIn     string
	pendingInAt   float64
	lastEventTime float64
	scanBuf       string
	writeErr      error
}

func newRecordingWriter(bw *bufio.Writer, mu *sync.Mutex, redactor *secretRedactor) *recordingWriter {
	return &recordingWriter{bw: bw, mu: mu, redactor: redactor}
}

func (rw *recordingWriter) setErrorLocked(err error) {
	if err != nil && rw.writeErr == nil {
		rw.writeErr = err
	}
}

func (rw *recordingWriter) getError() error {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	return rw.writeErr
}

func (rw *recordingWriter) writeHeader(hdr castHeader) error {
	hdr.Command = rw.redactor.RedactString(hdr.Command)
	hdr.CommandLabel = rw.redactor.RedactString(hdr.CommandLabel)
	hdr.Title = rw.redactor.RedactString(hdr.Title)
	for k, v := range hdr.Env {
		hdr.Env[k] = rw.redactor.RedactString(v)
	}
	b, _ := json.Marshal(hdr)
	rw.mu.Lock()
	defer rw.mu.Unlock()
	if rw.writeErr != nil {
		return rw.writeErr
	}
	_, err := rw.bw.Write(b)
	rw.setErrorLocked(err)
	if err == nil {
		_, err = rw.bw.Write([]byte{'\n'})
		rw.setErrorLocked(err)
	}
	return rw.writeErr
}

func (rw *recordingWriter) event(elapsed float64, eventType, data string) error {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	rw.writeEventLocked(elapsed, eventType, rw.redactor.RedactString(data))
	return rw.writeErr
}

// processPendingLocked processes safePart (the portion of pending that is not
// part of an incomplete ANSI sequence) and appends keepTail to any retained
// prefix. It updates rw.pending, rw.scanBuf, and rw.pendingAt.
func (rw *recordingWriter) processPendingLocked(elapsed float64, safePart, keepTail string) {
	if !rw.redactor.hasSecrets() {
		if len(safePart) > 0 {
			rw.writeEventLocked(rw.pendingAt, "o", safePart)
		}
		rw.pending = keepTail
		rw.pendingAt = elapsed
		return
	}

	rw.scanBuf = normalizeForSecretCheck(safePart)
	maxScan := rw.redactor.maxSecretBytes() * 2
	if maxScan < 1024 {
		maxScan = 1024
	}
	if len(rw.scanBuf) > maxScan {
		rw.scanBuf = rw.scanBuf[len(rw.scanBuf)-maxScan:]
	}

	if rw.redactor.anySecretInNormalized(rw.scanBuf) {
		rw.writeEventLocked(rw.pendingAt, "o", rw.redactor.redactOutputStream(safePart))
		rw.pending = keepTail
		rw.scanBuf = ""
		rw.pendingAt = elapsed
		return
	}

	if rw.redactor.PendingSecretPrefix(rw.scanBuf) {
		normalized := normalizeForSecretCheck(safePart)
		keepNorm := rw.redactor.longestPrefixSuffixLen(normalized)
		if keepNorm == 0 {
			if len(safePart) > 0 {
				rw.writeEventLocked(rw.pendingAt, "o", rw.redactor.RedactString(safePart))
			}
			rw.pending = keepTail
			rw.scanBuf = ""
			rw.pendingAt = elapsed
			return
		}
		maxKeep := rw.redactor.maxNormalizedSecretBytes() - 1
		if maxKeep < 0 {
			maxKeep = 0
		}
		if keepNorm > maxKeep {
			keepNorm = maxKeep
		}
		rawStart, _ := rawSpanForNormalizedIdx(safePart, len(normalized)-keepNorm, len(normalized))
		for rawStart > 0 && rawStart < len(safePart) && !utf8.RuneStart(safePart[rawStart]) {
			rawStart--
		}
		_, rawEnd := rawSpanForNormalizedIdx(safePart, len(normalized)-1, len(normalized))

		// Flush safe raw bytes before the retained prefix.
		if rawStart > 0 {
			cut := rw.redactor.safeOutputCut(safePart, rawStart)
			for cut > 0 && cut < len(safePart) && !utf8.RuneStart(safePart[cut]) {
				cut--
			}
			if cut > 0 {
				rw.writeEventLocked(rw.pendingAt, "o", rw.redactor.RedactString(safePart[:cut]))
			}
		}

		// If the bytes after the retained prefix are only complete ANSI escape
		// sequences and whitespace, flush them now so the raw pending buffer
		// cannot grow on ANSI-only output.
		if rawEnd < len(safePart) {
			trailing := safePart[rawEnd:]
			if normalizeForSecretCheck(trailing) == "" {
				rw.writeEventLocked(elapsed, "o", rw.redactor.RedactString(trailing))
			} else {
				rawEnd = len(safePart)
			}
		}

		// Retain the prefix raw span plus any deferred incomplete ANSI tail.
		rw.pending = safePart[rawStart:rawEnd] + keepTail
		rw.scanBuf = normalizeForSecretCheck(rw.pending[:rawEnd-rawStart])
		rw.pendingAt = elapsed
		return
	}

	if len(safePart) > 0 {
		rw.writeEventLocked(rw.pendingAt, "o", rw.redactor.RedactString(safePart))
	}
	rw.pending = keepTail
	rw.scanBuf = ""
	rw.pendingAt = elapsed
}

func (rw *recordingWriter) output(elapsed float64, data string) error {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	rw.flushPendingInputLocked()
	if rw.pending == "" {
		rw.pendingAt = elapsed
	}
	rw.pending += data

	// ANSI escape sequences may be split across PTY reads. Determine the longest
	// suffix that could be an incomplete escape sequence and defer its
	// classification/flush to the next read. The remaining prefix (safePart) is
	// used for secret detection.
	incompleteStart := findIncompleteANSIStart(rw.pending)

	const maxIncompleteANSILen = 64 * 1024
	if len(rw.pending)-incompleteStart > maxIncompleteANSILen {
		// The unfinished control sequence is too large to retain indefinitely.
		// Process the bytes before it normally, then emit a truncation marker
		// and discard the sequence so memory stays bounded.
		safePart := rw.pending[:incompleteStart]
		rw.processPendingLocked(elapsed, safePart, "")
		rw.writeEventLocked(elapsed, "o", "<ansi-truncated>")
		return rw.writeErr
	}

	safePart := rw.pending
	if incompleteStart < len(rw.pending) {
		safePart = rw.pending[:incompleteStart]
	}
	rw.processPendingLocked(elapsed, safePart, rw.pending[incompleteStart:])
	return rw.writeErr
}

func (rw *recordingWriter) flushOutput() error {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	rw.flushPendingInputLocked()
	if rw.pending != "" {
		if rw.redactor.hasSecrets() && rw.redactor.anySecretInNormalized(rw.scanBuf) {
			rw.writeEventLocked(rw.pendingAt, "o", rw.redactor.redactOutputStream(rw.pending))
		} else {
			rw.writeEventLocked(rw.pendingAt, "o", rw.redactor.RedactString(rw.pending))
		}
		rw.pending = ""
		rw.scanBuf = ""
	}
	return rw.writeErr
}

func (rw *recordingWriter) input(elapsed float64, data string) error {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	rw.flushPendingOutputLocked()
	if rw.pendingIn == "" {
		rw.pendingInAt = elapsed
	}
	rw.pendingIn += data
	keep := rw.redactor.maxSecretBytes() - 1
	if keep < 0 {
		keep = 0
	}
	if len(rw.pendingIn) <= keep {
		return rw.writeErr
	}
	cut := len(rw.pendingIn) - keep
	cut = rw.redactor.safeOutputCut(rw.pendingIn, cut)
	for cut > 0 && cut < len(rw.pendingIn) && !utf8.RuneStart(rw.pendingIn[cut]) {
		cut--
	}
	rw.writeEventLocked(rw.pendingInAt, "i", rw.redactor.RedactString(rw.pendingIn[:cut]))
	rw.pendingIn = rw.pendingIn[cut:]
	rw.pendingInAt = elapsed
	return rw.writeErr
}

func (rw *recordingWriter) flushInput() error {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	rw.flushPendingOutputLocked()
	if rw.pendingIn != "" {
		rw.writeEventLocked(rw.pendingInAt, "i", rw.redactor.RedactString(rw.pendingIn))
		rw.pendingIn = ""
	}
	return rw.writeErr
}

func (rw *recordingWriter) flush() error {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	if rw.writeErr != nil {
		return rw.writeErr
	}
	err := rw.bw.Flush()
	rw.setErrorLocked(err)
	return rw.writeErr
}

func (rw *recordingWriter) writeEventLocked(elapsed float64, eventType, data string) {
	if rw.writeErr != nil {
		return
	}
	if elapsed < rw.lastEventTime {
		elapsed = rw.lastEventTime
	}
	rw.lastEventTime = elapsed
	b, _ := json.Marshal([]any{elapsed, eventType, data})
	_, err := rw.bw.Write(b)
	rw.setErrorLocked(err)
	if err == nil {
		_, err = rw.bw.Write([]byte{'\n'})
		rw.setErrorLocked(err)
	}
}

func (rw *recordingWriter) flushPendingInputLocked() {
	if rw.pendingIn != "" {
		rw.writeEventLocked(rw.pendingInAt, "i", rw.redactor.RedactString(rw.pendingIn))
		rw.pendingIn = ""
	}
}

func (rw *recordingWriter) flushPendingOutputLocked() {
	if rw.pending != "" {
		if rw.redactor.hasSecrets() && rw.redactor.anySecretInNormalized(rw.scanBuf) {
			rw.writeEventLocked(rw.pendingAt, "o", rw.redactor.redactOutputStream(rw.pending))
		} else {
			rw.writeEventLocked(rw.pendingAt, "o", rw.redactor.RedactString(rw.pending))
		}
		rw.pending = ""
		rw.scanBuf = ""
	}
}

func (rw *recordingWriter) dumpScreen(w io.Writer, cols, rows int, lines []string) {
	fmt.Fprintf(w, "---- screen %dx%d ----\n", cols, rows)
	for _, line := range lines {
		fmt.Fprintf(w, "| %s\n", rw.redactor.RedactString(line))
	}
	fmt.Fprintln(w, "---- end screen ----")
}
