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

// TestRedactScreenCatchesInPlaceOverwriteFragment covers a diagnostic-dump
// leak: a live terminal is fed raw bytes so it can render real screen state,
// so a secret that gets partially overwritten in place (e.g. "\r" plus a
// shorter status string, common for spinners and masked redisplay) leaves a
// contiguous remainder of the original secret in untouched cells. That
// remainder never equals the full secret, so a whole-string match alone would
// miss it and print the fragment verbatim on a failure/abort screen dump.
func TestRedactScreenCatchesInPlaceOverwriteFragment(t *testing.T) {
	r, err := newSecretRedactor(nil)
	if err != nil {
		t.Fatal(err)
	}
	r.add("PW", "p4ssw0rd-super-secret-XYZ")

	// Only the "pw=" prefix was overwritten by "XXXXXXXXXX"; the tail of the
	// secret ("d-super-secret-XYZ") remains untouched on screen.
	lines := []string{"XXXXXXXXXXd-super-secret-XYZ"}
	got := r.redactScreen(lines)
	if len(got) != 1 || got[0] != "<screen redacted>" {
		t.Fatalf("redactScreen did not catch overwrite fragment: %v", got)
	}
}

func TestRedactScreenAllowsUnrelatedShortText(t *testing.T) {
	r, err := newSecretRedactor(nil)
	if err != nil {
		t.Fatal(err)
	}
	r.add("PW", "p4ssw0rd-super-secret-XYZ")

	lines := []string{"harmless-next-screen"}
	got := r.redactScreen(lines)
	if len(got) != 1 || got[0] != lines[0] {
		t.Fatalf("redactScreen redacted unrelated content: %v", got)
	}
}
