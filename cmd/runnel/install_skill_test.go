package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunInstallSkillHelp(t *testing.T) {
	for _, flag := range []string{"--help", "-h", "-help", "--h"} {
		var stdout, stderr bytes.Buffer
		code := runInstallSkill([]string{"install-skill", flag}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("runInstallSkill with %q exited with %d, want 0", flag, code)
		}
		if !strings.Contains(stdout.String(), "usage: runnel install-skill [directory]") {
			t.Fatalf("runInstallSkill with %q stdout = %q, want usage", flag, stdout.String())
		}
		if stderr.Len() != 0 {
			t.Fatalf("runInstallSkill with %q wrote to stderr: %q", flag, stderr.String())
		}
	}
}

func TestRunInstallSkillTooManyArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runInstallSkill([]string{"install-skill", "dir1", "dir2"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "usage: runnel install-skill [directory]") {
		t.Fatalf("expected usage on stderr, got %q", stderr.String())
	}
}

func TestRunInstallSkillSuccess(t *testing.T) {
	tempDir := filepath.Join(t.TempDir(), "test-skill")
	var stdout, stderr bytes.Buffer
	code := runInstallSkill([]string{"install-skill", tempDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected code 0, got %d, stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Successfully installed runnel AI agent skill") {
		t.Fatalf("unexpected stdout: %q", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(tempDir, "SKILL.md")); err != nil {
		t.Fatalf("skill file was not created: %v", err)
	}
}
