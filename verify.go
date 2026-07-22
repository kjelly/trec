package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

type castVerification struct {
	Path           string                   `json:"path"`
	Valid          bool                     `json:"valid"`
	Issues         []string                 `json:"issues,omitempty"`
	Warnings       []string                 `json:"warnings,omitempty"`
	Status         string                   `json:"status,omitempty"`
	ExitCode       int                      `json:"exit_code"`
	Integrity      castIntegrity            `json:"integrity"`
	Scan           scanSummary              `json:"scan"`
	ResultBuild    buildMetadata            `json:"result_build,omitempty"`
	UpdatedAt      string                   `json:"updated_at,omitempty"`
	UnfinishedStep string                   `json:"unfinished_step,omitempty"`
	Progress       *castProgress            `json:"progress,omitempty"`
	Inputs         *sessionInputFingerprint `json:"inputs,omitempty"`
	InputsDrift    []string                 `json:"inputs_drift,omitempty"`
}

// castProgress is the verify-side three-state classification. Phase "pending"
// is informational (script is still running, last step has a recent heartbeat);
// "heartbeat_stale" is a warning (no heartbeat in N seconds despite status
// pending); "completed" / "failed" mirror the result status.
type castProgress struct {
	Phase           string  `json:"phase"`
	HeartbeatAt     string  `json:"heartbeat_at,omitempty"`
	LastOutputAgeMS int64   `json:"last_output_age_ms,omitempty"`
	ElapsedSeconds  float64 `json:"elapsed_seconds,omitempty"`
	Stale           bool    `json:"stale,omitempty"`
}

type verificationReport struct {
	Valid   bool               `json:"valid"`
	Checked int                `json:"checked"`
	Passed  int                `json:"passed"`
	Failed  int                `json:"failed"`
	Results []castVerification `json:"results"`
}

func expandCastPaths(paths []string) ([]string, error) {
	seen := make(map[string]struct{})
	var casts []string
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("inspect %s: %w", path, err)
		}
		if !info.IsDir() {
			if _, ok := seen[path]; !ok {
				seen[path] = struct{}{}
				casts = append(casts, path)
			}
			continue
		}
		err = filepath.WalkDir(path, func(castPath string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return fmt.Errorf("walk %s: %w", castPath, walkErr)
			}
			if !entry.Type().IsRegular() || !strings.EqualFold(filepath.Ext(entry.Name()), ".cast") {
				return nil
			}
			if _, ok := seen[castPath]; ok {
				return nil
			}
			seen[castPath] = struct{}{}
			casts = append(casts, castPath)
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("walk directory %s: %w", path, err)
		}
	}
	sort.Strings(casts)
	if len(casts) == 0 {
		return nil, fmt.Errorf("no cast files found")
	}
	return casts, nil
}

func verifyCast(path string) castVerification {
	verification := castVerification{Path: path, ExitCode: -1}
	observed, err := inspectCastIntegrity(path)
	if err != nil {
		verification.Issues = append(verification.Issues, "invalid cast: "+err.Error())
		verification.Scan = scanSummary{Complete: false, SafeToShare: false, Error: err.Error()}
		return verification
	}
	verification.Integrity = observed
	verification.Scan = summarizeScan(path)
	verification.UnfinishedStep = unfinishedDriveStep(path)
	if verification.UnfinishedStep != "" {
		verification.Issues = append(verification.Issues, "unfinished drive step: "+verification.UnfinishedStep)
	}

	data, err := os.ReadFile(resultPath(path))
	if err != nil {
		if os.IsNotExist(err) {
			verification.Issues = append(verification.Issues, "result sidecar is missing")
		} else {
			verification.Issues = append(verification.Issues, "read result sidecar: "+err.Error())
		}
	} else {
		var result sessionResult
		if err := json.Unmarshal(data, &result); err != nil {
			verification.Issues = append(verification.Issues, "invalid result sidecar: "+err.Error())
		} else {
			verification.Status = result.Status
			verification.ExitCode = result.ExitCode
			verification.ResultBuild = result.Build
			verification.UpdatedAt = result.UpdatedAt
			if result.Inputs != nil {
				verification.Inputs = result.Inputs
				verification.InputsDrift = detectInputDrift(result.Inputs)
			}
			verification.Progress = classifyProgress(result)
			if result.Build.Modified {
				verification.Warnings = append(verification.Warnings, "recording was produced by a dirty trec build")
			}
			if !isAcceptedCompletionStatus(result.Status) {
				message := fmt.Sprintf("result status is %q", result.Status)
				if result.Status == "in_progress" && result.UpdatedAt != "" {
					message += " (last updated " + result.UpdatedAt + ")"
				}
				verification.Issues = append(verification.Issues, message)
			}
			if result.ExitCode != 0 && !isAcceptedCompletionStatus(result.Status) {
				verification.Issues = append(verification.Issues, fmt.Sprintf("result exit_code is %d", result.ExitCode))
			}
			if !result.Cast.Complete {
				verification.Issues = append(verification.Issues, "result cast.complete is false")
			} else {
				if result.Cast.Algorithm != "sha256" {
					verification.Issues = append(verification.Issues, fmt.Sprintf("result cast.algorithm is %q", result.Cast.Algorithm))
				}
				if result.Cast.SHA256 != observed.SHA256 {
					verification.Issues = append(verification.Issues, "cast sha256 does not match result")
				}
				if result.Cast.ByteSize != observed.ByteSize {
					verification.Issues = append(verification.Issues, fmt.Sprintf("cast byte size %d does not match result %d", observed.ByteSize, result.Cast.ByteSize))
				}
				if result.Cast.EventCount != observed.EventCount {
					verification.Issues = append(verification.Issues, fmt.Sprintf("cast event count %d does not match result %d", observed.EventCount, result.Cast.EventCount))
				}
				if result.Cast.SchemaVersion >= 2 && !observed.SessionEnd {
					verification.Issues = append(verification.Issues, "cast is missing the required SESSION_END marker")
				}
				if observed.SessionEnd {
					if observed.SessionEndCount != 1 {
						verification.Issues = append(verification.Issues, fmt.Sprintf("cast has %d SESSION_END markers; exactly one is required", observed.SessionEndCount))
					}
					if !observed.SessionEndFinal {
						verification.Issues = append(verification.Issues, "SESSION_END is not the final cast event")
					}
					if observed.SessionEndStatus != result.Status &&
						!(result.Status == "ended" && observed.SessionEndStatus == "ended") {
						verification.Issues = append(verification.Issues, fmt.Sprintf("SESSION_END status %q does not match result status %q", observed.SessionEndStatus, result.Status))
					}
					if observed.SessionEndExitCode == nil {
						verification.Issues = append(verification.Issues, "SESSION_END exit_code is missing")
					} else if *observed.SessionEndExitCode != result.ExitCode {
						verification.Issues = append(verification.Issues, fmt.Sprintf("SESSION_END exit_code %d does not match result exit_code %d", *observed.SessionEndExitCode, result.ExitCode))
					}
				}
			}
		}
	}

	if !verification.Scan.Complete {
		verification.Issues = append(verification.Issues, "secret scan did not complete: "+verification.Scan.Error)
	} else if verification.Scan.FindingsCount > 0 {
		verification.Issues = append(verification.Issues, fmt.Sprintf("secret scan found %d likely unredacted secret(s)", verification.Scan.FindingsCount))
	}
	verification.Valid = len(verification.Issues) == 0
	return verification
}

func unfinishedDriveStep(path string) string {
	_, events, err := loadCastFile(path)
	if err != nil {
		return ""
	}
	activeLine := -1
	active := ""
	for _, event := range events {
		if event.typ != "m" {
			continue
		}
		var line int
		if strings.HasPrefix(event.data, "STEP_START line ") {
			if _, err := fmt.Sscanf(event.data, "STEP_START line %d:", &line); err == nil {
				activeLine = line
				active = event.data
			}
			continue
		}
		if strings.HasPrefix(event.data, "STEP_OK line ") {
			if _, err := fmt.Sscanf(event.data, "STEP_OK line %d:", &line); err == nil && line == activeLine {
				activeLine = -1
				active = ""
			}
		}
		if strings.HasPrefix(event.data, "STEP_FAILED line ") {
			if _, err := fmt.Sscanf(event.data, "STEP_FAILED line %d:", &line); err == nil && line == activeLine {
				activeLine = -1
				active = ""
			}
		}
	}
	return active
}

func verifyPaths(paths []string) (verificationReport, error) {
	casts, err := expandCastPaths(paths)
	if err != nil {
		return verificationReport{}, err
	}
	report := verificationReport{Valid: true, Checked: len(casts), Results: make([]castVerification, 0, len(casts))}
	for _, path := range casts {
		result := verifyCast(path)
		report.Results = append(report.Results, result)
		if result.Valid {
			report.Passed++
		} else {
			report.Valid = false
			report.Failed++
		}
	}
	return report, nil
}

var errVerificationFailed = errors.New("recording verification failed")

func newVerifyCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "verify <file.cast|directory>...",
		Short:        "Verify recordings recursively for completion, integrity, and secret-scan safety",
		Args:         cobra.MinimumNArgs(1),
		SilenceUsage: true,
		RunE:         runVerify,
	}
	cmd.Flags().String("format", "text", "output format (text, json, jsonl)")
	return cmd
}

func runVerify(cmd *cobra.Command, paths []string) error {
	format, _ := cmd.Flags().GetString("format")
	report, err := verifyPaths(paths)
	if err != nil {
		return fmt.Errorf("trec verify: %w", err)
	}
	switch strings.ToLower(format) {
	case "text":
		for _, result := range report.Results {
			if result.Valid {
				fmt.Printf("PASS %s\n", result.Path)
			} else {
				fmt.Printf("FAIL %s\n", result.Path)
				for _, issue := range result.Issues {
					fmt.Printf("  - %s\n", issue)
				}
			}
			for _, warning := range result.Warnings {
				fmt.Printf("  ! %s\n", warning)
			}
		}
		fmt.Printf("checked=%d passed=%d failed=%d\n", report.Checked, report.Passed, report.Failed)
	case "json":
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return fmt.Errorf("encode verification report: %w", err)
		}
		fmt.Println(string(data))
	case "jsonl":
		encoder := json.NewEncoder(os.Stdout)
		for _, result := range report.Results {
			if err := encoder.Encode(result); err != nil {
				return fmt.Errorf("encode verification result: %w", err)
			}
		}
	default:
		return fmt.Errorf("trec verify: unsupported format %q (choose from text, json, jsonl)", format)
	}
	if !report.Valid {
		return fmt.Errorf("trec verify: %w: %d of %d cast(s) failed", errVerificationFailed, report.Failed, report.Checked)
	}
	return nil
}

// isAcceptedCompletionStatus reports whether a result.Status is a legitimate
// terminal state (not failure). "success" means the child exited 0 and the
// recording completed; "ended" means the script invoked END_SESSION / QUIT
// (intentional early termination, not a failure). All other values
// (in_progress, failed, aborted) are not accepted.
func isAcceptedCompletionStatus(status string) bool {
	return status == "success" || status == "ended"
}

// classifyProgress turns the sessionStep.HeartbeatAt and sessionProgress fields
// into a three-state phase for verify. The "pending" phase is informational
// (not an issue); "heartbeat_stale" is a warning (no progress in 5 minutes
// despite the result still being in_progress); "completed" mirrors success/ended.
func classifyProgress(result sessionResult) *castProgress {
	if result.LastStep == nil {
		return nil
	}
	cp := &castProgress{
		ElapsedSeconds: result.LastStep.ElapsedSeconds,
	}
	if result.Progress != nil {
		cp.HeartbeatAt = result.Progress.HeartbeatAt
		cp.LastOutputAgeMS = result.Progress.LastOutputAgeMS
	}
	// Fall back to LastStep.HeartbeatAt when Progress is absent (older pending
	// results, or steps that don't have a heartbeat yet).
	if cp.HeartbeatAt == "" && result.LastStep != nil {
		cp.HeartbeatAt = result.LastStep.HeartbeatAt
	}
	switch result.Status {
	case "in_progress":
		cp.Phase = "pending"
		// Heartbeat stale if no heartbeat_at, or if heartbeat_at is more than
		// 5 minutes older than result.UpdatedAt.
		if cp.HeartbeatAt == "" {
			cp.Phase = "heartbeat_stale"
			cp.Stale = true
		} else {
			t1, err1 := time.Parse(time.RFC3339Nano, cp.HeartbeatAt)
			t2, err2 := time.Parse(time.RFC3339Nano, result.UpdatedAt)
			if err1 == nil && err2 == nil && t2.Sub(t1) > 5*time.Minute {
				cp.Phase = "heartbeat_stale"
				cp.Stale = true
			}
		}
	case "success", "ended":
		cp.Phase = "completed"
	case "failed", "aborted":
		cp.Phase = "failed"
	default:
		cp.Phase = result.Status
	}
	return cp
}

// detectInputDrift re-checks inventory / vault / cwd on disk to flag when
// the cast was recorded against a different environment than the current
// one. Returns a list of human-readable warnings; empty if no drift.
func detectInputDrift(fp *sessionInputFingerprint) []string {
	var drift []string
	if fp == nil {
		return nil
	}
	check := func(label, path, recordedMTime, recordedSHA string) {
		if path == "" {
			return
		}
		info, err := os.Stat(path)
		if err != nil {
			drift = append(drift, fmt.Sprintf("%s (%s) is missing on disk now; cast inputs are stale", label, path))
			return
		}
		currentMTime := info.ModTime().UTC().Format(time.RFC3339Nano)
		if recordedMTime != "" && recordedMTime < currentMTime {
			drift = append(drift, fmt.Sprintf("%s (%s) has been modified since the cast (mtime %s > recorded %s); do not treat this cast as evidence for the current environment", label, path, currentMTime, recordedMTime))
		}
		_ = recordedSHA
	}
	check("inventory", fp.InventoryPath, fp.InventoryMTime, fp.InventorySHA256)
	check("vault", fp.VaultPath, fp.VaultMTime, fp.VaultSHA256)
	return drift
}
