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
	// Capturing the operator and the value lets the scan distinguish a real
	// assignment from a TUI's status suffix such as
	// "secret = [未設定，使用內建預設]" and from source code that merely names a
	// variable (see isInlineSecretNonAssignment).
	{"inline-secret-assignment", regexp.MustCompile(`(?i)(?:password|token|secret|api[_-]?key)\s*(===|==|=~|:=|=|:)\s*([^\s"']+)`)},
}

func scanText(location string, at float64, text string) []scanFinding {
	var findings []scanFinding
	for _, rule := range secretScanRules {
		if ruleMatchesUnredactedSecret(rule, text) {
			findings = append(findings, scanFinding{Location: location, Time: at, Rule: rule.name})
		}
	}
	return findings
}

func ruleMatchesUnredactedSecret(rule struct {
	name string
	re   *regexp.Regexp
}, text string) bool {
	if !rule.re.MatchString(text) {
		return false
	}
	if rule.name != "inline-secret-assignment" {
		return !strings.Contains(text, "<redacted:")
	}
	for _, m := range rule.re.FindAllStringSubmatchIndex(text, -1) {
		// m holds the whole match, the operator (group 1) and the value (group 2).
		operator, value, rest := text[m[2]:m[3]], text[m[4]:m[5]], text[m[1]:]
		if isInlineSecretNonAssignment(operator, value, rest) || isKnownInlineSecretPlaceholder(visibleValue(value)) {
			continue
		}
		return true
	}
	return false
}

var (
	// A Python string-literal prefix (r, u, b, f, br, rb, fr, rf).
	stringLiteralPrefix = regexp.MustCompile(`^(?i:[rubf]|br|rb|fr|rf)$`)
	// A call chain whose every call has an empty argument list, such as
	// "line.split()[0]" or "getpass.getpass()".
	emptyCallChain = regexp.MustCompile(`^[A-Za-z_]\w*(?:\.[A-Za-z_]\w*)*(?:\(\)|\[\d+\]|\.[A-Za-z_]\w*)*$`)
)

// isInlineSecretNonAssignment recognises text that names a secret-like variable
// but cannot hold its value, which is what appears when a recording renders
// repository source (a diff, a file listing). It is deliberately narrow, and
// anything else after a "=" or ":" is still treated as a value:
//
//   - a comparison or match operator ("==", "===", "=~"), as in
//     "token==str(path).encode()", is not an assignment (":=" still is);
//   - a string-literal prefix directly followed by the opening quote, as in
//     `token = b""`: the quoted value is not matched as a bare value either, so
//     its prefix must not be taken for one;
//   - a call chain whose calls all have empty argument lists and that contains
//     at least one call, as in "token = line.split()[0]": it reads a value. A
//     bare name, a dotted name or a call with arguments is still a value.
func isInlineSecretNonAssignment(operator, value, rest string) bool {
	switch operator {
	case "==", "===", "=~":
		return true
	}
	if stringLiteralPrefix.MatchString(value) && (strings.HasPrefix(rest, "'") || strings.HasPrefix(rest, `"`)) {
		return true
	}
	return strings.Contains(value, "()") && emptyCallChain.MatchString(value)
}

// visibleValue removes complete terminal escape sequences from a captured
// value. A TUI writes cursor and style escapes directly after a status marker,
// as in "= <已設定>\x1b[K" or "= <已設定>\x1b[49G", and the value pattern stops
// only at whitespace or a quote, so the escape becomes part of the value and
// the marker is no longer recognised (trec issue #3). It is used only to
// recognise a placeholder: the value itself is still the raw text, so a secret
// written behind or between escapes stays reported, and a value that is
// nothing but escapes is not a placeholder.
func visibleValue(value string) string {
	return ansiRe.ReplaceAllString(value, "")
}

// isKnownInlineSecretPlaceholder excludes TUI status annotations and
// redaction/placeholders, but only when they use a complete known token. This
// keeps values such as "[已設定]actual-value" detectable as real assignments.
func isKnownInlineSecretPlaceholder(value string) bool {
	if strings.HasPrefix(value, "<redacted:") || strings.HasPrefix(strings.ToUpper(value), "CHANGE-ME") {
		return true
	}
	return isWrappedTUISecretStatus(value, "[", "]") || isWrappedTUISecretStatus(value, "<", ">")
}

func isWrappedTUISecretStatus(value, open, close string) bool {
	if !strings.HasPrefix(value, open) || !strings.HasSuffix(value, close) {
		return false
	}
	status := strings.TrimSuffix(strings.TrimPrefix(value, open), close)
	return status == "已設定" || status == "未設定" ||
		strings.HasPrefix(status, "已設定，") || strings.HasPrefix(status, "未設定，")
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
