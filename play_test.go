package main

import (
	"math"
	"strings"
	"testing"
)

func TestVisibleOutputSizeIgnoresANSI(t *testing.T) {
	chars, hasNewline := visibleOutputSize("\x1b[1;32mhello\x1b[0m\r\n")
	if chars != 5 || !hasNewline {
		t.Fatalf("visibleOutputSize() = (%d, %t), want (5, true)", chars, hasNewline)
	}
}

func TestIsScreenRepaintDetectsEraseDisplay(t *testing.T) {
	if !isScreenRepaint("\x1b[2J\x1b[Hhosts") {
		t.Fatal("erase-display repaint was not detected")
	}
	if isScreenRepaint("\x1b[2Kline cleared") {
		t.Fatal("line erase must not be treated as a full-screen repaint")
	}
}

func TestSmartGapBalancesReadingAndIdleTime(t *testing.T) {
	largeFrame := castEvent{typ: "o", data: strings.Repeat("x", 160)}
	if got := smartGap(largeFrame, castEvent{typ: "i"}, 8); got != smartLongHold.Seconds() {
		t.Fatalf("large frame hold = %v, want %v", got, smartLongHold.Seconds())
	}
	if got := smartGap(castEvent{typ: "i"}, castEvent{typ: "o"}, 8); got != smartShortHold.Seconds() {
		t.Fatalf("non-visual hold = %v, want %v", got, smartShortHold.Seconds())
	}
	if got := smartGap(castEvent{typ: "o", data: "\x1b[2Jhosts"}, castEvent{typ: "i"}, 0.1); got != smartScreenMinHold.Seconds() {
		t.Fatalf("screen repaint hold = %v, want %v", got, smartScreenMinHold.Seconds())
	}
}

func TestAdjustTimingPreservesShortBursts(t *testing.T) {
	events := []castEvent{
		{sec: 0.02, typ: "o", data: "one"},
		{sec: 0.04, typ: "o", data: "two"},
		{sec: 4.04, typ: "i", data: "x"},
	}
	adjustTiming(events, 5, true)
	if math.Abs(events[1].sec-0.04) > 1e-9 {
		t.Fatalf("short burst timestamp = %v, want 0.04", events[1].sec)
	}
	if math.Abs(events[2].sec-0.79) > 1e-9 {
		t.Fatalf("long idle timestamp = %v, want 0.79", events[2].sec)
	}
}

func TestEnforceInputGap(t *testing.T) {
	events := []castEvent{
		{sec: 1, typ: "i", data: "a"},
		{sec: 1.1, typ: "o", data: "frame"},
		{sec: 1.2, typ: "i", data: "b"},
	}
	enforceInputGap(events, 0.5)
	if math.Abs(events[2].sec-events[0].sec-0.5) > 1e-9 {
		t.Fatalf("input gap = %v, want 0.5", events[2].sec-events[0].sec)
	}
	if math.Abs(events[1].sec-1.1) > 1e-9 {
		t.Fatalf("preceding output event moved: %v", events[1].sec)
	}
}

func TestCapInputGapCompressesOnlyConsecutiveInput(t *testing.T) {
	events := []castEvent{
		{sec: 0, typ: "i", data: "s"},
		{sec: 0.3, typ: "i", data: "e"},
		{sec: 0.6, typ: "i", data: "c"},
		{sec: 2.0, typ: "o", data: "form submitted"},
	}

	capInputGap(events, 0.05)

	if math.Abs(events[1].sec-0.05) > 1e-9 || math.Abs(events[2].sec-0.1) > 1e-9 {
		t.Fatalf("input timestamps = %v, %v; want 0.05, 0.1", events[1].sec, events[2].sec)
	}
	if math.Abs(events[3].sec-1.5) > 1e-9 {
		t.Fatalf("output timestamp = %v, want 1.5 after input compression", events[3].sec)
	}
}

func TestCapInputGapDisabledLeavesEventsUnchanged(t *testing.T) {
	events := []castEvent{{sec: 0, typ: "i", data: "a"}, {sec: 0.3, typ: "i", data: "b"}}
	capInputGap(events, 0)
	if events[1].sec != 0.3 {
		t.Fatalf("disabled input cap changed timestamp: %v", events[1].sec)
	}
}

func TestEnforceOutputGap(t *testing.T) {
	events := []castEvent{
		{sec: 1, typ: "o", data: "first"},
		{sec: 1.1, typ: "i", data: "key"},
		{sec: 1.2, typ: "o", data: "second"},
	}

	enforceOutputGap(events, 0.5)

	if math.Abs(events[2].sec-events[0].sec-0.5) > 1e-9 {
		t.Fatalf("output gap = %v, want 0.5", events[2].sec-events[0].sec)
	}
	if math.Abs(events[1].sec-1.1) > 1e-9 {
		t.Fatalf("preceding input event moved: %v", events[1].sec)
	}
}

func TestEnforceOutputGapDisabled(t *testing.T) {
	events := []castEvent{
		{sec: 1, typ: "o", data: "first"},
		{sec: 1.1, typ: "o", data: "second"},
	}

	enforceOutputGap(events, 0)

	if math.Abs(events[1].sec-1.1) > 1e-9 {
		t.Fatalf("disabled output gap changed timestamp: %v", events[1].sec)
	}
}

func TestPlayCommandDefaultsTo500msScreenGap(t *testing.T) {
	flag := newPlayCommand().Flags().Lookup("min-screen-gap-ms")
	if flag == nil {
		t.Fatal("min-screen-gap-ms flag is missing")
	}
	if flag.DefValue != "500" {
		t.Fatalf("min-screen-gap-ms default = %q, want 500", flag.DefValue)
	}
}

func TestPlayCommandHasResizeTerminalFlag(t *testing.T) {
	if flag := newPlayCommand().Flags().Lookup("resize-terminal"); flag == nil {
		t.Fatal("resize-terminal flag is missing")
	}
}

func TestTerminalResizeSequence(t *testing.T) {
	if got, want := terminalResizeSequence(220, 50), "\x1b[8;50;220t"; got != want {
		t.Fatalf("terminalResizeSequence() = %q, want %q", got, want)
	}
}

func TestFindMarkerQueryMatchesCaseInsensitively(t *testing.T) {
	events := []castEvent{
		{sec: 1, typ: "m", data: "FORM_Login"},
		{sec: 2, typ: "m", data: "FORM_Notifications"},
	}

	got, err := findMarkerQuery(events, "notifications")
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Fatalf("marker index = %d, want 1", got)
	}
}

func TestFindMarkerQueryReportsNoMatch(t *testing.T) {
	_, err := findMarkerQuery([]castEvent{{sec: 1, typ: "m", data: "FORM_Login"}}, "billing")
	if err == nil || !strings.Contains(err.Error(), "billing") {
		t.Fatalf("error = %v, want missing query", err)
	}
}
