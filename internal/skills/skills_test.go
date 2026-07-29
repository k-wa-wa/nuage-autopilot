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

	readmeFile := filepath.Join(tempDir, "README.md")
	content, err := os.ReadFile(readmeFile)
	if err != nil {
		t.Fatalf("failed to read written README file: %v", err)
	}

	if len(content) == 0 {
		t.Errorf("README file content is empty")
	}
}
