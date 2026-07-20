package main

import "testing"

func TestNormalizeSecretFileValueTrimsOneFinalLineEnding(t *testing.T) {
	for input, want := range map[string]string{
		"secret\n":     "secret",
		"secret\r\n":   "secret",
		"line1\nline2": "line1\nline2",
	} {
		if got := normalizeSecretFileValue(input); got != want {
			t.Errorf("normalizeSecretFileValue(%q) = %q, want %q", input, got, want)
		}
	}
}
