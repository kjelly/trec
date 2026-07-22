package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// sessionResult is a machine-readable completion record written next to a cast.
type sessionResult struct {
	SessionID       string                   `json:"session_id,omitempty"`
	StartedAt       string                   `json:"started_at,omitempty"`
	UpdatedAt       string                   `json:"updated_at,omitempty"`
	Mode            string                   `json:"mode,omitempty"`
	CommandLabel    string                   `json:"command_label,omitempty"`
	Status          string                   `json:"status"`
	ExitCode        int                      `json:"exit_code"`
	Error           string                   `json:"error,omitempty"`
	DurationSeconds float64                  `json:"duration_seconds"`
	FinalScreen     []string                 `json:"final_screen,omitempty"`
	Snapshots       []sessionSnapshot        `json:"snapshots,omitempty"`
	Script          *sessionScript           `json:"script,omitempty"`
	LastStep        *sessionStep             `json:"last_step,omitempty"`
	Timeouts        *sessionTimeouts         `json:"timeouts,omitempty"`
	Termination     *sessionTermination      `json:"termination,omitempty"`
	Build           buildMetadata            `json:"build"`
	Scan            scanSummary              `json:"scan"`
	Cast            castIntegrity            `json:"cast"`
	Progress        *sessionProgress         `json:"progress,omitempty"`
	Inputs          *sessionInputFingerprint `json:"inputs,omitempty"`
}

type sessionTimeouts struct {
	ExpectMS    int `json:"expect_ms"`
	ChildExitMS int `json:"child_exit_ms"`
}

type sessionTermination struct {
	Kind        string `json:"kind"`
	Disposition string `json:"disposition,omitempty"`
	Reason      string `json:"reason,omitempty"`
	Signal      string `json:"signal,omitempty"`
	TimedOut    bool   `json:"timed_out,omitempty"`
}

type sessionScript struct {
	SHA256          string   `json:"sha256"`
	StepCount       int      `json:"step_count"`
	NormalizedSteps []string `json:"normalized_steps,omitempty"`
}

type sessionStep struct {
	Line           int     `json:"line"`
	Operation      string  `json:"operation"`
	Phase          string  `json:"phase"`
	UpdatedAt      string  `json:"updated_at"`
	HeartbeatAt    string  `json:"heartbeat_at,omitempty"`
	ElapsedSeconds float64 `json:"elapsed_seconds,omitempty"`
}

// sessionProgress exposes long-running apply visibility: heartbeat timestamp
// and the age of the most recent child output. A live script refreshes
// HeartbeatAt and ElapsedSeconds without writing a new cast event; consumers
// can read this sidecar to distinguish "still waiting on apply" from
// "looks stuck". LastOutputAgeMS is derived from terminalSession.quietFor at
// the time of the heartbeat flush.
type sessionProgress struct {
	HeartbeatAt         string  `json:"heartbeat_at,omitempty"`
	ElapsedSeconds      float64 `json:"elapsed_seconds,omitempty"`
	LastOutputAgeMS     int64   `json:"last_output_age_ms,omitempty"`
	WaitingFor          string  `json:"waiting_for,omitempty"`
	WaitingForElapsedMS int64   `json:"waiting_for_elapsed_ms,omitempty"`
}

// sessionInputFingerprint lets a reader tell whether a cast was recorded
// against the same inventory / vault / working directory that is currently on
// disk. Without this, an agent can mistake a cast from a previous environment
// for evidence of the current one (the failure mode that left the r11
// deploy-site-full.cast's apply unverified against the live VMs).
type sessionInputFingerprint struct {
	InventoryPath   string `json:"inventory_path,omitempty"`
	InventoryMTime  string `json:"inventory_mtime,omitempty"`
	InventorySHA256 string `json:"inventory_sha256,omitempty"`
	VaultPath       string `json:"vault_path,omitempty"`
	VaultMTime      string `json:"vault_mtime,omitempty"`
	VaultSHA256     string `json:"vault_sha256,omitempty"`
	CWD             string `json:"cwd,omitempty"`
	ScriptSHA256    string `json:"script_sha256,omitempty"`
}

func buildSessionScript(path string, steps []*driveStep, redactor *secretRedactor) (*sessionScript, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read script provenance: %w", err)
	}
	sum := sha256.Sum256(data)
	normalized := make([]string, 0, len(steps))
	for _, step := range steps {
		description := safeStepDescription(step)
		if redactor != nil {
			description = redactor.RedactString(description)
		}
		normalized = append(normalized, description)
	}
	return &sessionScript{
		SHA256:          hex.EncodeToString(sum[:]),
		StepCount:       len(steps),
		NormalizedSteps: normalized,
	}, nil
}

type sessionSnapshot struct {
	Time   float64  `json:"time"`
	Label  string   `json:"label"`
	Screen []string `json:"screen"`
}

// castIntegrity makes the completion record self-contained enough for a
// caller to detect a partial or modified cast before trusting its snapshots.
// The digest covers the exact on-disk bytes, including the header and every
// event line.
type castIntegrity struct {
	SchemaVersion      int           `json:"schema_version"`
	Complete           bool          `json:"complete"`
	Algorithm          string        `json:"algorithm"`
	SHA256             string        `json:"sha256"`
	ByteSize           int64         `json:"byte_size"`
	EventCount         int           `json:"event_count"`
	LastEventTime      float64       `json:"last_event_time"`
	SessionEnd         bool          `json:"session_end"`
	SessionEndCount    int           `json:"session_end_count,omitempty"`
	SessionEndFinal    bool          `json:"session_end_final,omitempty"`
	SessionEndStatus   string        `json:"session_end_status,omitempty"`
	SessionEndExitCode *int          `json:"session_end_exit_code,omitempty"`
	Producer           string        `json:"producer"`
	Version            string        `json:"version"`
	Build              buildMetadata `json:"build"`
}

func resultPath(castPath string) string { return castPath + ".result.json" }

type scanSummary struct {
	Complete      bool     `json:"complete"`
	FindingsCount int      `json:"findings_count"`
	Rules         []string `json:"rules,omitempty"`
	SafeToShare   bool     `json:"safe_to_share"`
	Error         string   `json:"error,omitempty"`
}

func summarizeScan(path string) scanSummary {
	findings, err := scanCast(path)
	if err != nil {
		return scanSummary{Complete: false, SafeToShare: false, Error: err.Error()}
	}
	ruleSet := make(map[string]struct{})
	for _, finding := range findings {
		ruleSet[finding.Rule] = struct{}{}
	}
	rules := make([]string, 0, len(ruleSet))
	for rule := range ruleSet {
		rules = append(rules, rule)
	}
	sort.Strings(rules)
	return scanSummary{
		Complete:      true,
		FindingsCount: len(findings),
		Rules:         rules,
		SafeToShare:   len(findings) == 0,
	}
}

func newPendingSessionResult(started time.Time) sessionResult {
	build := currentBuildMetadata()
	return sessionResult{
		SessionID: fmt.Sprintf("%d-%d", started.UnixNano(), os.Getpid()),
		StartedAt: started.UTC().Format(time.RFC3339Nano),
		UpdatedAt: started.UTC().Format(time.RFC3339Nano),
		Status:    "in_progress",
		ExitCode:  -1,
		Build:     build,
		Scan:      scanSummary{Complete: false, SafeToShare: false},
		Cast:      castIntegrity{SchemaVersion: 2, Complete: false, Algorithm: "sha256", Producer: "trec", Version: build.DisplayVersion(), Build: build},
	}
}

func writePendingSessionResult(castPath string, result sessionResult) error {
	result.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("encode pending result: %w", err)
	}
	return writeFileAtomic(resultPath(castPath), append(data, '\n'), 0o644)
}

func writeSessionResult(castPath string, result sessionResult) error {
	integrity, err := inspectCastIntegrity(castPath)
	if err != nil {
		return fmt.Errorf("inspect cast: %w", err)
	}
	result.Build = currentBuildMetadata()
	result.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	result.Scan = summarizeScan(castPath)
	result.Cast = integrity
	b, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("encode result: %w", err)
	}
	return writeFileAtomic(resultPath(castPath), append(b, '\n'), 0o644)
}

func prepareRecordingOutput(path string, force bool) (*os.File, error) {
	if path == "" {
		return nil, fmt.Errorf("output path is empty")
	}
	if !force {
		for _, candidate := range []string{path, resultPath(path)} {
			if _, err := os.Stat(candidate); err == nil {
				return nil, fmt.Errorf("%s already exists; use --force to replace the cast and result sidecar", candidate)
			} else if !os.IsNotExist(err) {
				return nil, fmt.Errorf("inspect %s: %w", candidate, err)
			}
		}
		return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	}
	if err := os.Remove(resultPath(path)); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("remove stale result %s: %w", resultPath(path), err)
	}
	return os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
}

func inspectCastIntegrity(path string) (castIntegrity, error) {
	f, err := os.Open(path)
	if err != nil {
		return castIntegrity{}, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return castIntegrity{}, err
	}
	hash := sha256.New()
	scanner := bufio.NewScanner(io.TeeReader(f, hash))
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return castIntegrity{}, err
		}
		return castIntegrity{}, fmt.Errorf("empty file")
	}
	var header castHeader
	if err := json.Unmarshal(scanner.Bytes(), &header); err != nil {
		return castIntegrity{}, fmt.Errorf("invalid header: %w", err)
	}
	if err := validateCastHeader(header); err != nil {
		return castIntegrity{}, fmt.Errorf("invalid header: %w", err)
	}

	eventCount := 0
	lastTime := 0.0
	sessionEndCount := 0
	sessionEndEventIndex := -1
	sessionEndStatus := ""
	var sessionEndExitCode *int
	lineNo := 1
	for scanner.Scan() {
		lineNo++
		line := scanner.Bytes()
		if len(line) == 0 {
			return castIntegrity{}, fmt.Errorf("invalid event at line %d: empty line", lineNo)
		}
		event, err := parseCastEvent(line)
		if err != nil {
			return castIntegrity{}, fmt.Errorf("invalid event at line %d: %w", lineNo, err)
		}
		if event.sec < lastTime {
			return castIntegrity{}, fmt.Errorf("invalid event at line %d: timestamp %.9f is earlier than %.9f", lineNo, event.sec, lastTime)
		}
		lastTime = event.sec
		if event.typ == "m" && strings.HasPrefix(event.data, "SESSION_END ") {
			status, exitCode, _, err := parseSessionEndMarker(event.data)
			if err != nil {
				return castIntegrity{}, fmt.Errorf("invalid event at line %d: %w", lineNo, err)
			}
			sessionEndCount++
			sessionEndEventIndex = eventCount
			sessionEndStatus = status
			code := exitCode
			sessionEndExitCode = &code
		}
		eventCount++
	}
	if err := scanner.Err(); err != nil {
		return castIntegrity{}, fmt.Errorf("read: %w", err)
	}

	build := header.TrecBuild
	version := header.TrecVersion
	if version == "" {
		version = build.DisplayVersion()
	}
	schemaVersion := 1
	if sessionEndCount > 0 {
		schemaVersion = 2
	}
	return castIntegrity{
		SchemaVersion:      schemaVersion,
		Complete:           true,
		Algorithm:          "sha256",
		SHA256:             hex.EncodeToString(hash.Sum(nil)),
		ByteSize:           info.Size(),
		EventCount:         eventCount,
		LastEventTime:      lastTime,
		SessionEnd:         sessionEndCount > 0,
		SessionEndCount:    sessionEndCount,
		SessionEndFinal:    sessionEndEventIndex == eventCount-1,
		SessionEndStatus:   sessionEndStatus,
		SessionEndExitCode: sessionEndExitCode,
		Producer:           "trec",
		Version:            version,
		Build:              build,
	}, nil
}

// parseSessionEndMarker accepts both the legacy form
//
//	wire:   SESSION_END status=aborted exit_code=-1
//	new:    SESSION_END status=ended disposition=script_ended exit_code=0
//
// The disposition field is optional and parsed as the second return value.
func parseSessionEndMarker(data string) (status string, exitCode int, disposition string, err error) {
	fields := strings.Fields(data)
	if len(fields) < 3 || fields[0] != "SESSION_END" {
		return "", 0, "", fmt.Errorf("malformed SESSION_END marker")
	}
	statusField, ok := strings.CutPrefix(fields[1], "status=")
	if !ok || statusField == "" {
		return "", 0, "", fmt.Errorf("malformed SESSION_END status")
	}
	status = statusField
	// Walk key=value pairs from the end. The last pair is always exit_code=.
	exitCodeField := fields[len(fields)-1]
	exitCodeText, ok := strings.CutPrefix(exitCodeField, "exit_code=")
	if !ok {
		return "", 0, "", fmt.Errorf("malformed SESSION_END exit_code")
	}
	exitCode, err = strconv.Atoi(exitCodeText)
	if err != nil {
		return "", 0, "", fmt.Errorf("malformed SESSION_END exit_code: %w", err)
	}
	// Optional disposition= field sits between status and exit_code.
	for _, f := range fields[2 : len(fields)-1] {
		if v, ok := strings.CutPrefix(f, "disposition="); ok {
			disposition = v
		}
	}
	return status, exitCode, disposition, nil
}

// formatSessionEndMarker produces a marker line that parseSessionEndMarker
// can round-trip. Disposition is omitted when empty so the legacy
// "status=… exit_code=…" format is preserved.
func formatSessionEndMarker(status string, exitCode int, disposition string) string {
	if disposition == "" {
		return fmt.Sprintf("SESSION_END status=%s exit_code=%d", status, exitCode)
	}
	return fmt.Sprintf("SESSION_END status=%s disposition=%s exit_code=%d", status, disposition, exitCode)
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmpPath)
		}
	}()
	if err = tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
