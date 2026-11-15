package main

import "testing"

func TestScanTextTUISecretStatusesWithANSI(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{"angle set with cursor", "password = <已設定>\x1b[49G\r\n"},
		{"angle unset with cursor", "password = <未設定>\x1b[49G\r\n"},
		{"bracket set with cursor", "password = [已設定]\x1b[49G\r\n"},
		{"bracket unset with cursor", "password = [未設定]\x1b[49G\r\n"},
		{"angle status detail", "token = <已設定，使用自訂值>\x1b[0m"},
		{"bracket status detail", "secret = [未設定，使用內建預設]\x1b[49G"},
		{"multiple controls", "api_key = <已設定>\x1b[0m\x1b[49G\x1b[K"},
		{"colored marker", "password = \x1b[32m<已設定>\x1b[0m"},
		{"control inside marker", "password = <已\x1b[32m設定>\x1b[0m"},
		{"TUI row", "┃ > node_exporter_basic_auth_password = <已設定>\x1b[49G\r\n"},
		{"multiple placeholders", "password = <已設定>\x1b[49G token=[未設定]\x1b[0m"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if findings := scanText("event.o", 0.1, tt.text); len(findings) != 0 {
				t.Fatalf("scanText(%q) = %#v, want no TUI-status findings", tt.text, findings)
			}
		})
	}
}

func TestScanTextANSIStillFindsRealSecrets(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{"real value with cursor", "password = hunter2\x1b[49G\r\n"},
		{"colored real value", "password = \x1b[32mhunter2\x1b[0m"},
		{"angle marker with real suffix", "password = <已設定>\x1b[0mactual-secret"},
		{"bracket marker with real suffix", "password = [已設定]\x1b[0mactual-secret"},
		{"marker with real prefix", "password = actual-secret\x1b[0m<已設定>"},
		{"second assignment", "api_key = <已設定>\x1b[49G token=real-secret\x1b[0m"},
		{"second line", "password = <已設定>\x1b[49G\r\ntoken=real-secret"},
		{"real assignment first", "token=real-secret password=<已設定>\x1b[49G"},
		{"unknown marker", "password = <configured>\x1b[49G"},
		{"mismatched marker", "password = <已設定]\x1b[49G"},
		{"incomplete CSI", "password = <已設定>\x1b[49"},
		{"CSI introducer", "password = <已設定>\x1b["},
		{"bare escape", "password = <已設定>\x1b"},
		{"malformed status escape", "password = [已設定，default\x1b]"},
		{"status with control byte", "password = [已設定，default\x00]"},
		{"OSC payload with BEL", "password = <已設定>\x1b]0;token=real-secret\x07"},
		{"OSC payload with ST", "password = <已設定>\x1b]0;token=real-secret\x1b\\"},
		{"standalone OSC payload", "\x1b]0;token=real-secret\x07"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			findings := scanText("event.o", 0.1, tt.text)
			if len(findings) != 1 || findings[0] != (scanFinding{Location: "event.o", Time: 0.1, Rule: "inline-secret-assignment"}) {
				t.Fatalf("scanText(%q) = %#v, want one inline-secret-assignment at event.o, 0.1s", tt.text, findings)
			}
		})
	}
}
