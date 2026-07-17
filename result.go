package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// sessionResult is a machine-readable completion record written next to a cast.
type sessionResult struct {
	Status          string            `json:"status"`
	ExitCode        int               `json:"exit_code"`
	Error           string            `json:"error,omitempty"`
	DurationSeconds float64           `json:"duration_seconds"`
	FinalScreen     []string          `json:"final_screen,omitempty"`
	Snapshots       []sessionSnapshot `json:"snapshots,omitempty"`
}

type sessionSnapshot struct {
	Time   float64  `json:"time"`
	Label  string   `json:"label"`
	Screen []string `json:"screen"`
}

func resultPath(castPath string) string { return castPath + ".result.json" }

func writeSessionResult(castPath string, result sessionResult) error {
	b, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("encode result: %w", err)
	}
	return os.WriteFile(resultPath(castPath), append(b, '\n'), 0o644)
}
