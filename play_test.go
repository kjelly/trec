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

func TestSmartGapBalancesReadingAndIdleTime(t *testing.T) {
	largeFrame := castEvent{typ: "o", data: strings.Repeat("x", 160)}
	if got := smartGap(largeFrame, castEvent{typ: "i"}, 8); got != smartLongHold.Seconds() {
		t.Fatalf("large frame hold = %v, want %v", got, smartLongHold.Seconds())
	}
	if got := smartGap(castEvent{typ: "i"}, castEvent{typ: "o"}, 8); got != smartShortHold.Seconds() {
		t.Fatalf("non-visual hold = %v, want %v", got, smartShortHold.Seconds())
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
