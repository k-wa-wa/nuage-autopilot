// Package skills は autopilot の起動時に Agent (Claude CLI 等) 向けの共通スキルを
// ホームディレクトリの ~/.agents/skills/ および ~/.claude/skills/ へ自動配置・更新する機能を実装する。
package skills

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

//go:embed files/*
var embeddedSkills embed.FS

// Ensure は実行ユーザーの ~/.agents/skills/ および ~/.claude/skills/ に
// 内包されたスキル群を自動配置・更新する。
func Ensure() error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get user home dir: %w", err)
	}

	targets := []string{
		filepath.Join(homeDir, ".agents", "skills"),
		filepath.Join(homeDir, ".claude", "skills"),
	}

	for _, targetDir := range targets {
		if err := EnsureToDir(targetDir); err != nil {
			return fmt.Errorf("failed to ensure skills in %s: %w", targetDir, err)
		}
	}

	return nil
}

// EnsureToDir は指定されたベースディレクトリに対して内包されたスキル群を展開・更新する。
func EnsureToDir(targetBaseDir string) error {
	subFS, err := fs.Sub(embeddedSkills, "files")
	if err != nil {
		return fmt.Errorf("failed to open embedded files subFS: %w", err)
	}

	return fs.WalkDir(subFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if path == "." {
			return nil
		}

		targetPath := filepath.Join(targetBaseDir, path)

		if d.IsDir() {
			return os.MkdirAll(targetPath, 0755)
		}

		content, err := fs.ReadFile(subFS, path)
		if err != nil {
			return fmt.Errorf("failed to read embedded file %s: %w", path, err)
		}

		if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
			return fmt.Errorf("failed to create parent directory for %s: %w", targetPath, err)
		}

		return os.WriteFile(targetPath, content, 0644)
	})
}
