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
)

// sessionResult is a machine-readable completion record written next to a cast.
type sessionResult struct {
	Status          string            `json:"status"`
	ExitCode        int               `json:"exit_code"`
	Error           string            `json:"error,omitempty"`
	DurationSeconds float64           `json:"duration_seconds"`
	FinalScreen     []string          `json:"final_screen,omitempty"`
	Snapshots       []sessionSnapshot `json:"snapshots,omitempty"`
	Cast            castIntegrity     `json:"cast"`
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
	SchemaVersion int     `json:"schema_version"`
	Complete      bool    `json:"complete"`
	Algorithm     string  `json:"algorithm"`
	SHA256        string  `json:"sha256"`
	ByteSize      int64   `json:"byte_size"`
	EventCount    int     `json:"event_count"`
	LastEventTime float64 `json:"last_event_time"`
	Producer      string  `json:"producer"`
	Version       string  `json:"version"`
}

func resultPath(castPath string) string { return castPath + ".result.json" }

func writeSessionResult(castPath string, result sessionResult) error {
	integrity, err := inspectCastIntegrity(castPath)
	if err != nil {
		return fmt.Errorf("inspect cast: %w", err)
	}
	result.Cast = integrity
	b, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("encode result: %w", err)
	}
	return writeFileAtomic(resultPath(castPath), append(b, '\n'), 0o644)
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
		eventCount++
	}
	if err := scanner.Err(); err != nil {
		return castIntegrity{}, fmt.Errorf("read: %w", err)
	}

	return castIntegrity{
		SchemaVersion: 1,
		Complete:      true,
		Algorithm:     "sha256",
		SHA256:        hex.EncodeToString(hash.Sum(nil)),
		ByteSize:      info.Size(),
		EventCount:    eventCount,
		LastEventTime: lastTime,
		Producer:      "trec",
		Version:       appVersion,
	}, nil
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
