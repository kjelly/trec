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
	// Capturing the value lets scanText distinguish a real assignment from a
	// TUI's status suffix such as "secret = [未設定，使用內建預設]".
	{"inline-secret-assignment", regexp.MustCompile(`(?i)(?:password|token|secret|api[_-]?key)\s*[=:]\s*([^\s"']+)`)},
}

func scanText(location string, at float64, text string) []scanFinding {
	var findings []scanFinding
	for _, rule := range secretScanRules {
		if rule.re.MatchString(text) && !strings.Contains(text, "<redacted:") && !isTUIUnsetSecretStatus(rule, text) {
			findings = append(findings, scanFinding{Location: location, Time: at, Rule: rule.name})
		}
	}
	return findings
}

// isTUIUnsetSecretStatus excludes only pilot-style display annotations. It
// deliberately does not suppress bracketed secret values in general.
func isTUIUnsetSecretStatus(rule struct {
	name string
	re   *regexp.Regexp
}, text string) bool {
	if rule.name != "inline-secret-assignment" {
		return false
	}
	match := rule.re.FindStringSubmatch(text)
	if len(match) < 2 {
		return false
	}
	value := match[1]
	return strings.HasPrefix(value, "[未設定") ||
		strings.HasPrefix(value, "[已設定") ||
		strings.HasPrefix(strings.ToUpper(value), "CHANGE-ME")
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
		// Drive step markers contain structural operation descriptions, not
		// terminal output. In particular, an EXPECT label such as
		// "ipa_admin_password =" is followed by a timing suffix and can look
		// like an assignment to the heuristic scanner even though it carries no
		// value. Inputs are separately redacted before marker creation.
		if event.typ == "m" && strings.HasPrefix(event.data, "STEP_") {
			continue
		}
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
