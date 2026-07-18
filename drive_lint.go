package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

type driveLintFinding struct {
	Level   string `json:"level"`
	Line    int    `json:"line"`
	Message string `json:"message"`
}

type driveLintReport struct {
	Path     string             `json:"path"`
	Valid    bool               `json:"valid"`
	Errors   int                `json:"errors"`
	Warnings int                `json:"warnings"`
	Findings []driveLintFinding `json:"findings"`
}

func lintDriveSteps(steps []*driveStep, strict bool) []driveLintFinding {
	var findings []driveLintFinding
	add := func(level string, step *driveStep, message string) {
		findings = append(findings, driveLintFinding{Level: level, Line: step.line, Message: message})
	}

	screenGuarded := false
	hasWaitChildExit := false
	hasAssertExit := false
	for i, step := range steps {
		switch step.kind {
		case "expect", "expect_eventually", "expect_regex", "expect_screen_regex", "assert":
			screenGuarded = true
		case "select":
			screenGuarded = true
			next := i + 1
			for next < len(steps) && (steps[next].kind == "wait" || steps[next].kind == "snapshot") {
				next++
			}
			if next >= len(steps) || steps[next].kind != "enter" {
				add("error", step, "SELECT only moves the pointer; follow it with ENTER or use CHOOSE <label>")
			}
		case "enter":
			if !screenGuarded {
				add("error", step, "ENTER is not guarded by EXPECT, ASSERT, or SELECT; use ENTER_IF <screen text> when possible")
			}
			screenGuarded = false
		case "enter_if", "choose":
			screenGuarded = false
		case "down", "up":
			level := "warning"
			if strict {
				level = "error"
			}
			add(level, step, strings.ToUpper(step.kind)+" is position-dependent; prefer SELECT/CHOOSE unless driving a scrolling checklist")
		case "wait_child_exit":
			hasWaitChildExit = true
			if step.hasTimeout && step.timeout > 30*60*1000 {
				add("warning", step, "WAIT_CHILD_EXIT exceeds 30 minutes; ensure the outer MCP/tool timeout is longer")
			}
		case "assert_exit":
			hasAssertExit = true
		}
	}

	if hasWaitChildExit != hasAssertExit {
		lineStep := &driveStep{line: 1}
		if len(steps) > 0 {
			lineStep = steps[len(steps)-1]
		}
		add("error", lineStep, "WAIT_CHILD_EXIT and ASSERT_EXIT must be used as a pair")
	}
	return findings
}

func makeDriveLintReport(path string, findings []driveLintFinding) driveLintReport {
	report := driveLintReport{Path: path, Valid: true, Findings: findings}
	for _, finding := range findings {
		if finding.Level == "error" {
			report.Errors++
			report.Valid = false
		} else {
			report.Warnings++
		}
	}
	return report
}

// printDriveLintFindings returns true when at least one error was printed.
func printDriveLintFindings(w io.Writer, path string, findings []driveLintFinding) bool {
	report := makeDriveLintReport(path, findings)
	if len(findings) == 0 {
		fmt.Fprintf(w, "PASS %s: no findings\n", path)
		return false
	}
	fmt.Fprintf(w, "%s: %d error(s), %d warning(s)\n", path, report.Errors, report.Warnings)
	for _, finding := range findings {
		fmt.Fprintf(w, "  [%s] line %d: %s\n", strings.ToUpper(finding.Level), finding.Line, finding.Message)
	}
	return report.Errors > 0
}

func newDriveLintCommand() *cobra.Command {
	var strict bool
	var format string
	cmd := &cobra.Command{
		Use:          "lint <script.drive>...",
		Short:        "Statically check drive scripts for unsafe agent patterns",
		Args:         cobra.MinimumNArgs(1),
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, paths []string) error {
			var reports []driveLintReport
			failed := false
			for _, path := range paths {
				steps, err := loadDriveScript(path, nil)
				if err != nil {
					return fmt.Errorf("lint %s: %w", path, err)
				}
				report := makeDriveLintReport(path, lintDriveSteps(steps, strict))
				reports = append(reports, report)
				if !report.Valid {
					failed = true
				}
			}
			switch strings.ToLower(format) {
			case "text":
				for _, report := range reports {
					printDriveLintFindings(os.Stdout, report.Path, report.Findings)
				}
			case "json":
				data, err := json.MarshalIndent(reports, "", "  ")
				if err != nil {
					return fmt.Errorf("encode lint report: %w", err)
				}
				fmt.Println(string(data))
			default:
				return fmt.Errorf("unsupported format %q (choose from text, json)", format)
			}
			if failed {
				return fmt.Errorf("drive lint failed")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&strict, "strict", false, "treat position-dependent UP/DOWN navigation as errors")
	cmd.Flags().StringVar(&format, "format", "text", "output format (text, json)")
	return cmd
}
