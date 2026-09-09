package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallSkill(t *testing.T) {
	if !strings.Contains(Content, "name: runnel") {
		t.Fatalf("embedded skill does not contain 'name: runnel'")
	}

	tempDir := filepath.Join(t.TempDir(), "skills", "runnel")
	path, err := Install(tempDir)
	if err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read installed skill: %v", err)
	}

	if string(data) != Content {
		t.Fatalf("installed content does not match embedded Content")
	}
}

func TestInstallSkillRejectsFlagLikeDirectory(t *testing.T) {
	for _, invalid := range []string{"--help", "-h", "-something"} {
		if _, err := Install(invalid); err == nil {
			t.Fatalf("Install(%q) expected error, got nil", invalid)
		}
	}
}
