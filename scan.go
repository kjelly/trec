package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
)

type scanFinding struct {
	Location string  `json:"location"`
	Time     float64 `json:"time,omitempty"`
	Rule     string  `json:"rule"`
}

var secretScanRules = []struct {
	name string
	re   *regexp.Regexp
}{
	{"sshpass-password", regexp.MustCompile(`(?i)sshpass\s+-p\s+\S+`)},
	{"private-key", regexp.MustCompile(`-----BEGIN (?:[A-Z ]+ )?PRIVATE KEY-----`)},
	{"inline-secret-assignment", regexp.MustCompile(`(?i)(?:password|token|secret|api[_-]?key)\s*[=:]\s*[^\s"']+`)},
}

func scanText(location string, at float64, text string) []scanFinding {
	var findings []scanFinding
	for _, rule := range secretScanRules {
		if rule.re.MatchString(text) && !strings.Contains(text, "<redacted:") {
			findings = append(findings, scanFinding{Location: location, Time: at, Rule: rule.name})
		}
	}
	return findings
}

func scanCast(path string) ([]scanFinding, error) {
	hdr, events, err := loadCastFile(path)
	if err != nil {
		return nil, err
	}
	var findings []scanFinding
	for name, value := range map[string]string{"header.command": hdr.Command, "header.command_label": hdr.CommandLabel, "header.title": hdr.Title} {
		findings = append(findings, scanText(name, 0, value)...)
	}
	for key, value := range hdr.Env {
		findings = append(findings, scanText("header.env."+key, 0, value)...)
	}
	for _, event := range events {
		findings = append(findings, scanText("event."+event.typ, event.sec, event.data)...)
	}
	return findings, nil
}

func newScanCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "scan <file.cast>", Short: "Scan a recording for likely unredacted secrets", Args: cobra.ExactArgs(1), RunE: runScan, SilenceUsage: true}
	cmd.Flags().String("format", "text", "output format (text, json)")
	return cmd
}

func runScan(cmd *cobra.Command, args []string) error {
	format, _ := cmd.Flags().GetString("format")
	findings, err := scanCast(args[0])
	if err != nil {
		return fmt.Errorf("trec scan: %w", err)
	}
	switch strings.ToLower(format) {
	case "text":
		for _, finding := range findings {
			fmt.Printf("%s at %.2fs: %s\n", finding.Location, finding.Time, finding.Rule)
		}
	case "json":
		b, _ := json.MarshalIndent(findings, "", "  ")
		fmt.Println(string(b))
	default:
		return fmt.Errorf("trec scan: unsupported format %q (choose from text, json)", format)
	}
	if len(findings) > 0 {
		return fmt.Errorf("trec scan: found %d likely unredacted secret(s)", len(findings))
	}
	return nil
}
