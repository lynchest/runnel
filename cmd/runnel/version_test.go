package main

import (
	"runtime/debug"
	"testing"
)

func TestVersionOutputUsesLinkerVersion(t *testing.T) {
	originalVersion := version
	originalReadBuildInfo := readBuildInfo
	t.Cleanup(func() {
		version = originalVersion
		readBuildInfo = originalReadBuildInfo
	})

	readBuildInfo = func() (*debug.BuildInfo, bool) {
		return nil, false
	}

	tests := []struct {
		version string
		want    string
	}{
		{version: "dev", want: "runnel vdev"},
		{version: "0.1.4", want: "runnel v0.1.4"},
		{version: "v0.1.4", want: "runnel v0.1.4"},
	}
	for _, tt := range tests {
		version = tt.version
		if got := versionOutput(); got != tt.want {
			t.Fatalf("version output for %q = %q, want %q", tt.version, got, tt.want)
		}
	}
}

func TestVersionOutputBuildInfo(t *testing.T) {
	originalVersion := version
	originalReadBuildInfo := readBuildInfo
	t.Cleanup(func() {
		version = originalVersion
		readBuildInfo = originalReadBuildInfo
	})

	version = "dev"

	// Case 1: Main module has a release version from go install
	readBuildInfo = func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{
			Main: debug.Module{Version: "v0.1.6"},
		}, true
	}
	if got := versionOutput(); got != "runnel v0.1.6" {
		t.Fatalf("version output with module version = %q, want %q", got, "runnel v0.1.6")
	}

	// Case 2: Local build from git with clean revision and time
	readBuildInfo = func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{
			Main: debug.Module{Version: "(devel)"},
			Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "1234567890abcdef"},
				{Key: "vcs.time", Value: "2026-09-09T14:00:00Z"},
				{Key: "vcs.modified", Value: "false"},
			},
		}, true
	}
	if got := versionOutput(); got != "runnel vdev (1234567, 2026-09-09T14:00:00Z)" {
		t.Fatalf("version output with clean git = %q, want %q", got, "runnel vdev (1234567, 2026-09-09T14:00:00Z)")
	}

	// Case 3: Local build from git with dirty working tree
	readBuildInfo = func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{
			Main: debug.Module{Version: "(devel)"},
			Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "abcdef123456"},
				{Key: "vcs.time", Value: "2026-09-09T15:00:00Z"},
				{Key: "vcs.modified", Value: "true"},
			},
		}, true
	}
	if got := versionOutput(); got != "runnel vdev (abcdef1-dirty, 2026-09-09T15:00:00Z)" {
		t.Fatalf("version output with dirty git = %q, want %q", got, "runnel vdev (abcdef1-dirty, 2026-09-09T15:00:00Z)")
	}

	// Case 4: Revision only, no timestamp
	readBuildInfo = func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{
			Main: debug.Module{Version: "(devel)"},
			Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "fedcba987654"},
			},
		}, true
	}
	if got := versionOutput(); got != "runnel vdev (fedcba9)" {
		t.Fatalf("version output with revision only = %q, want %q", got, "runnel vdev (fedcba9)")
	}
}
