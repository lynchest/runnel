package skill

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

//go:embed SKILL.md
var Content string

// Install writes the embedded agent skill definition to the target directory.
// If targetDir is empty, it defaults to ~/.agents/skills/runnel.
func Install(targetDir string) (string, error) {
	targetDir = strings.TrimSpace(targetDir)
	if strings.HasPrefix(targetDir, "-") {
		return "", fmt.Errorf("invalid directory %q", targetDir)
	}
	if targetDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve user home directory: %w", err)
		}
		targetDir = filepath.Join(home, ".agents", "skills", "runnel")
	}

	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return "", fmt.Errorf("create skill directory %s: %w", targetDir, err)
	}

	skillFilePath := filepath.Join(targetDir, "SKILL.md")
	if err := os.WriteFile(skillFilePath, []byte(Content), 0o644); err != nil {
		return "", fmt.Errorf("write skill file %s: %w", skillFilePath, err)
	}

	return skillFilePath, nil
}
