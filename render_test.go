package main

import "testing"

func TestValidateRenderSize(t *testing.T) {
	for _, tc := range []struct {
		name      string
		width     int
		height    int
		wantError bool
	}{
		{name: "normal", width: 120, height: 40},
		{name: "zero width", width: 0, height: 40, wantError: true},
		{name: "zero height", width: 120, height: 0, wantError: true},
		{name: "negative width", width: -1, height: 40, wantError: true},
		{name: "dimension too large", width: maxRenderDimension + 1, height: 40, wantError: true},
		{name: "cell count too large", width: maxRenderDimension, height: 100, wantError: false},
		{name: "cell count over limit", width: maxRenderDimension, height: 105, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRenderSize(tc.width, tc.height)
			if (err != nil) != tc.wantError {
				t.Fatalf("validateRenderSize(%d, %d) error = %v, wantError = %v", tc.width, tc.height, err, tc.wantError)
			}
		})
	}
}
