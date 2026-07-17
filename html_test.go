package main

import (
	"bytes"
	"strings"
	"testing"
)

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

func TestHTMLPlayerShowsRecordedKeystrokes(t *testing.T) {
	var page bytes.Buffer
	data := htmlPageData{
		Title:         "demo",
		CastBase64:    "Y2FzdA==",
		MarkersBase64: "W10=",
	}
	if err := htmlPageTemplate.Execute(&page, data); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(page.String(), "keystrokeOverlay: true") {
		t.Fatalf("HTML player does not enable recorded keystroke overlay: %q", page.String())
	}
}
