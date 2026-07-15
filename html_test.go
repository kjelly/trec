package main

import "testing"

func TestHTMLOutputPath(t *testing.T) {
	tests := map[string]string{
		"demo.cast":        "demo.html",
		"recording":        "recording.html",
		"dir/demo.cast.gz": "dir/demo.cast.html",
	}
	for input, want := range tests {
		if got := htmlOutputPath(input); got != want {
			t.Errorf("htmlOutputPath(%q) = %q, want %q", input, got, want)
		}
	}
}
