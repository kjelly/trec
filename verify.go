package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

type castVerification struct {
	Path           string        `json:"path"`
	Valid          bool          `json:"valid"`
	Issues         []string      `json:"issues,omitempty"`
	Status         string        `json:"status,omitempty"`
	ExitCode       int           `json:"exit_code"`
	Integrity      castIntegrity `json:"integrity"`
	Scan           scanSummary   `json:"scan"`
	ResultBuild    buildMetadata `json:"result_build,omitempty"`
	UpdatedAt      string        `json:"updated_at,omitempty"`
	UnfinishedStep string        `json:"unfinished_step,omitempty"`
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
		entries, err := os.ReadDir(path)
		if err != nil {
			return nil, fmt.Errorf("read directory %s: %w", path, err)
		}
		for _, entry := range entries {
			if !entry.Type().IsRegular() || !strings.EqualFold(filepath.Ext(entry.Name()), ".cast") {
				continue
			}
			castPath := filepath.Join(path, entry.Name())
			if _, ok := seen[castPath]; ok {
				continue
			}
			seen[castPath] = struct{}{}
			casts = append(casts, castPath)
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
			if result.Status != "success" {
				message := fmt.Sprintf("result status is %q", result.Status)
				if result.Status == "in_progress" && result.UpdatedAt != "" {
					message += " (last updated " + result.UpdatedAt + ")"
				}
				verification.Issues = append(verification.Issues, message)
			}
			if result.ExitCode != 0 {
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
		Short:        "Verify recording completion, integrity, and secret-scan safety",
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
				continue
			}
			fmt.Printf("FAIL %s\n", result.Path)
			for _, issue := range result.Issues {
				fmt.Printf("  - %s\n", issue)
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
