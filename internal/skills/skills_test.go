package skills_test

import (
	"os"
	"path/filepath"
	"testing"

	"autopilot/internal/skills"
)

func TestEnsureToDir(t *testing.T) {
	tempDir := t.TempDir()

	if err := skills.EnsureToDir(tempDir); err != nil {
		t.Fatalf("EnsureToDir failed: %v", err)
	}

	skillFile := filepath.Join(tempDir, "upload-github-image", "SKILL.md")
	content, err := os.ReadFile(skillFile)
	if err != nil {
		t.Fatalf("failed to read written skill file: %v", err)
	}

	if len(content) == 0 {
		t.Errorf("skill file content is empty")
	}
}
