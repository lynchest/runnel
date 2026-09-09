package main

import "testing"

func TestVersionOutputUsesLinkerVersion(t *testing.T) {
	original := version
	t.Cleanup(func() { version = original })

	tests := []struct {
		version string
		want    string
	}{
		{version: "dev", want: "runnel vdev"},
		{version: "0.1.4", want: "runnel v0.1.4"},
	}
	for _, tt := range tests {
		version = tt.version
		if got := versionOutput(); got != tt.want {
			t.Fatalf("version output for %q = %q, want %q", tt.version, got, tt.want)
		}
	}
}
