package main

import "testing"

func TestBuildMetadataDisplayVersion(t *testing.T) {
	tests := []struct {
		name string
		meta buildMetadata
		want string
	}{
		{name: "release", meta: buildMetadata{Version: "v1.2.3", Revision: "abcdef"}, want: "v1.2.3"},
		{name: "development", meta: buildMetadata{Version: "dev"}, want: "dev"},
		{name: "revision", meta: buildMetadata{Version: "dev", Revision: "0123456789abcdef"}, want: "dev+0123456789ab"},
		{name: "dirty revision", meta: buildMetadata{Version: "dev", Revision: "0123456789abcdef", Modified: true}, want: "dev+0123456789ab.dirty"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.meta.DisplayVersion(); got != test.want {
				t.Fatalf("DisplayVersion() = %q, want %q", got, test.want)
			}
		})
	}
}
