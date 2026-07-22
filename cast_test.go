package main

import (
	"os"
	"strings"
	"testing"
)

func TestLoadCastFileRejectsMalformedHeaderAndEvents(t *testing.T) {
	cases := []struct {
		name string
		cast string
		want string
	}{
		{"wrong version", `{"version":1,"width":80,"height":24}` + "\n", "version must be 2"},
		{"invalid size", `{"version":2,"width":0,"height":24}` + "\n", "terminal size must be positive"},
		{"wrong event field count", `{"version":2,"width":80,"height":24}` + "\n" + `[0,"o"]` + "\n", "exactly 3 fields"},
		{"wrong output type", `{"version":2,"width":80,"height":24}` + "\n" + `[0,"o",3]` + "\n", `data for "o" must be a string`},
		{"negative time", `{"version":2,"width":80,"height":24}` + "\n" + `[-1,"o","x"]` + "\n", "non-negative number"},
		{"decreasing time", `{"version":2,"width":80,"height":24}` + "\n" + `[2,"o","x"]` + "\n" + `[1,"o","y"]` + "\n", "earlier than"},
		{"bad resize", `{"version":2,"width":80,"height":24}` + "\n" + `[1,"r","80-by-24"]` + "\n", "invalid resize event"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := loadCastFileFromBytes(tc.cast)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("loadCastFile error = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestCastRoundTripsUnknownExtensionEvent(t *testing.T) {
	const cast = "{\"version\":2,\"width\":80,\"height\":24}\n[1,\"x-extension\",{\"key\":true}]\n"
	hdr, events, err := loadCastFileFromBytes(cast)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || string(events[0].rawData) != `{"key":true}` {
		t.Fatalf("unknown event was not preserved: %#v", events)
	}

	path := t.TempDir() + "/roundtrip.cast"
	if err := writeCastFile(path, hdr, events); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `[1,"x-extension",{"key":true}]`) {
		t.Fatalf("unknown event changed during round trip:\n%s", data)
	}
}

func TestLoadCastFileTolerantMode(t *testing.T) {
	const cast = "{\"version\":2,\"width\":80,\"height\":24}\n[1,\"o\",\"valid\"]\n[0.5,\"o\",\"invalid time\"]\n[2,\"o\",\"also valid\"]\n"
	tmp := t.TempDir() + "/tolerant.cast"
	if err := os.WriteFile(tmp, []byte(cast), 0644); err != nil {
		t.Fatal(err)
	}
	_, events, err := loadCastFileWithOptions(tmp, loadCastOptions{Tolerant: true})
	if err != nil {
		t.Fatalf("tolerant mode failed: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("len(events) = %d, want 2", len(events))
	}
}
