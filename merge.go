package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// mergeCandidate is a validated recording together with the metadata used to
// select it. Source SESSION_END markers are intentionally not copied: the
// merged cast has one final marker and one result sidecar of its own.
type mergeCandidate struct {
	path   string
	header castHeader
	events []castEvent
	result *sessionResult
}

func newMergeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "merge [flags] <file.cast|directory>...",
		Short: "Merge recordings sequentially, with optional status and command filters",
		Long: `Merges matching asciicast recordings in deterministic path order. Directories are searched recursively.

--status reads each recording's .result.json status (for example, --status success).
--command-regex matches recorded command, command label, or result command label. Commands
that were not recorded cannot match a command-only expression; use --command-label when recording.`,
		Args:         cobra.MinimumNArgs(1),
		SilenceUsage: true,
		RunE:         runMerge,
	}
	cmd.Flags().StringP("output", "o", "", "merged cast output path (required)")
	cmd.Flags().Bool("force", false, "replace an existing output cast and result sidecar")
	cmd.Flags().StringArray("status", nil, "only merge recordings with these result statuses (repeatable or comma-separated)")
	cmd.Flags().String("command-regex", "", "only merge recordings whose command metadata matches this regexp")
	cmd.Flags().Float64("gap", 0.25, "seconds inserted between recordings")
	return cmd
}

func parseMergeStatuses(values []string) map[string]struct{} {
	if len(values) == 0 {
		return nil
	}
	statuses := make(map[string]struct{})
	for _, value := range values {
		for _, status := range strings.Split(value, ",") {
			if status = strings.TrimSpace(status); status != "" {
				statuses[status] = struct{}{}
			}
		}
	}
	return statuses
}

func loadMergeResult(path string) (*sessionResult, error) {
	data, err := os.ReadFile(resultPath(path))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read result sidecar: %w", err)
	}
	var result sessionResult
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("parse result sidecar: %w", err)
	}
	return &result, nil
}

func mergeCommandMetadata(header castHeader, result *sessionResult) []string {
	parts := []string{header.Command, header.CommandLabel}
	if result != nil {
		parts = append(parts, result.CommandLabel)
	}
	return parts
}

func mergeCommandMatches(commandRE *regexp.Regexp, header castHeader, result *sessionResult) bool {
	if commandRE == nil {
		return true
	}
	for _, value := range mergeCommandMetadata(header, result) {
		if commandRE.MatchString(value) {
			return true
		}
	}
	return false
}

func selectMergeCandidates(paths []string, statuses map[string]struct{}, commandRE *regexp.Regexp) ([]mergeCandidate, error) {
	casts, err := expandCastPaths(paths)
	if err != nil {
		return nil, err
	}
	candidates := make([]mergeCandidate, 0, len(casts))
	for _, path := range casts {
		header, events, err := loadCastFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		result, err := loadMergeResult(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		if statuses != nil {
			if result == nil {
				continue
			}
			if _, ok := statuses[result.Status]; !ok {
				continue
			}
		}
		if !mergeCommandMatches(commandRE, header, result) {
			continue
		}
		candidates = append(candidates, mergeCandidate{path: path, header: header, events: events, result: result})
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no recordings matched the requested filters")
	}
	return candidates, nil
}

func mergeCasts(candidates []mergeCandidate, gap float64) (castHeader, []castEvent, float64) {
	header := candidates[0].header
	header.Timestamp = time.Now().Unix()
	header.TrecVersion = currentBuildMetadata().DisplayVersion()
	header.TrecBuild = currentBuildMetadata()
	header.Command = ""
	header.CommandLabel = fmt.Sprintf("merged %d recordings", len(candidates))
	header.Title = "Merged recordings"

	events := make([]castEvent, 0)
	timeline := 0.0
	for index, candidate := range candidates {
		status := "unknown"
		if candidate.result != nil {
			status = candidate.result.Status
		}
		events = append(events, castEvent{sec: timeline, typ: "m", data: fmt.Sprintf("MERGE_SOURCE file=%s status=%s", filepath.Base(candidate.path), status)})
		if index > 0 {
			events = append(events, castEvent{sec: timeline, typ: "r", data: fmt.Sprintf("%dx%d", candidate.header.Width, candidate.header.Height)})
		}
		last := 0.0
		for _, event := range candidate.events {
			if event.sec > last {
				last = event.sec
			}
			if event.typ == "m" && strings.HasPrefix(event.data, "SESSION_END ") {
				continue
			}
			copy := event
			copy.sec += timeline
			events = append(events, copy)
		}
		timeline += last
		if index < len(candidates)-1 {
			timeline += gap
		}
	}
	events = append(events, castEvent{sec: timeline, typ: "m", data: formatSessionEndMarker("success", 0, "merge_completed")})
	return header, events, timeline
}

func outputIsMergeInput(output string, candidates []mergeCandidate) (bool, error) {
	outputAbs, err := filepath.Abs(output)
	if err != nil {
		return false, err
	}
	for _, candidate := range candidates {
		candidateAbs, err := filepath.Abs(candidate.path)
		if err != nil {
			return false, err
		}
		if outputAbs == candidateAbs {
			return true, nil
		}
	}
	return false, nil
}

func runMerge(cmd *cobra.Command, paths []string) error {
	output, _ := cmd.Flags().GetString("output")
	force, _ := cmd.Flags().GetBool("force")
	statusValues, _ := cmd.Flags().GetStringArray("status")
	pattern, _ := cmd.Flags().GetString("command-regex")
	gap, _ := cmd.Flags().GetFloat64("gap")
	if output == "" {
		return fmt.Errorf("trec merge: --output is required")
	}
	if gap < 0 || math.IsNaN(gap) || math.IsInf(gap, 0) {
		return fmt.Errorf("trec merge: --gap must be a non-negative finite number")
	}
	var commandRE *regexp.Regexp
	var err error
	if pattern != "" {
		commandRE, err = regexp.Compile(pattern)
		if err != nil {
			return fmt.Errorf("trec merge: invalid --command-regex %q: %w", pattern, err)
		}
	}
	candidates, err := selectMergeCandidates(paths, parseMergeStatuses(statusValues), commandRE)
	if err != nil {
		return fmt.Errorf("trec merge: %w", err)
	}
	if same, err := outputIsMergeInput(output, candidates); err != nil {
		return fmt.Errorf("trec merge: resolve output: %w", err)
	} else if same {
		return fmt.Errorf("trec merge: output must not be one of the input recordings")
	}
	if !force {
		for _, path := range []string{output, resultPath(output)} {
			if _, err := os.Stat(path); err == nil {
				return fmt.Errorf("trec merge: %s already exists; use --force to replace the cast and result sidecar", path)
			} else if !os.IsNotExist(err) {
				return fmt.Errorf("trec merge: inspect %s: %w", path, err)
			}
		}
	}
	header, events, duration := mergeCasts(candidates, gap)
	data, err := marshalCast(header, events)
	if err != nil {
		return fmt.Errorf("trec merge: encode output: %w", err)
	}
	if err := writeFileAtomic(output, data, 0o644); err != nil {
		return fmt.Errorf("trec merge: write %s: %w", output, err)
	}
	result := newPendingSessionResult(time.Now())
	result.Mode = "merge"
	result.CommandLabel = header.CommandLabel
	result.Status = "success"
	result.ExitCode = 0
	result.DurationSeconds = duration
	result.Termination = &sessionTermination{Kind: "merge", Disposition: "merge_completed"}
	if err := writeSessionResult(output, result); err != nil {
		return fmt.Errorf("trec merge: write result sidecar: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Merged %d recording(s) into %s\n", len(candidates), output)
	return nil
}
