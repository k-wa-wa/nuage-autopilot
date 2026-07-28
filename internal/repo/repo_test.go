package repo

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// このテストファイルは実際の GitHub API・実際の GH_TOKEN を一切使わない。
// 「リモート」はローカルの bare git リポジトリであり、`gh` は呼び出しを記録するだけの
// フェイクスクリプトに差し替える。ネットワークには一切到達しない。

func requireGit(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not found in PATH, skipping repo package tests")
	}
	return path
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// writeFakeGH はテスト用のフェイク gh 実行ファイルを書き出し、そのパスと呼び出しログの
// パスを返す。実行するたびに受け取った引数を1行としてログファイルに追記して正常終了する。
func writeFakeGH(t *testing.T) (ghPath, logPath string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake gh script assumes a POSIX shell")
	}
	dir := t.TempDir()
	logPath = filepath.Join(dir, "gh-calls.log")
	ghPath = filepath.Join(dir, "gh")
	script := "#!/bin/sh\necho \"$@\" >> " + shellQuote(logPath) + "\nexit 0\n"
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh script: %v", err)
	}
	return ghPath, logPath
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// newBareRemote は初期コミットを1つ持つ bare リポジトリを作り、その絶対パスを
// remote URL として使えるパスで返す（`git clone <path>` はローカルパスをそのまま
// remote URL として扱える）。
func newBareRemote(t *testing.T, gitBin string) string {
	t.Helper()
	root := t.TempDir()
	bareDir := filepath.Join(root, "remote.git")
	runOrFatal(t, gitBin, root, "init", "--bare", "-q", bareDir)

	workDir := filepath.Join(root, "seed-work")
	runOrFatal(t, gitBin, root, "clone", "-q", bareDir, workDir)
	runOrFatal(t, gitBin, workDir, "config", "user.email", "seed@example.invalid")
	runOrFatal(t, gitBin, workDir, "config", "user.name", "seed")
	if err := os.WriteFile(filepath.Join(workDir, "README.md"), []byte("v1\n"), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	runOrFatal(t, gitBin, workDir, "add", "README.md")
	runOrFatal(t, gitBin, workDir, "commit", "-q", "-m", "initial commit")
	runOrFatal(t, gitBin, workDir, "push", "-q", "origin", "HEAD")

	return bareDir
}

func runOrFatal(t *testing.T, gitBin, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(gitBin, args...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s (dir=%s) failed: %v\n%s", strings.Join(args, " "), dir, err, out.String())
	}
}

func TestEnsureClone_ClonesWhenMissing(t *testing.T) {
	gitBin := requireGit(t)
	ghPath, ghLog := writeFakeGH(t)
	remote := newBareRemote(t, gitBin)
	stateDir := t.TempDir()

	localPath, err := EnsureClone(context.Background(), testLogger(), stateDir, "acme/widgets",
		WithRemoteURL(remote), WithGitCommand(gitBin), WithGHCommand(ghPath))
	if err != nil {
		t.Fatalf("EnsureClone() error = %v", err)
	}

	wantPath := filepath.Join(stateDir, "acme", "widgets")
	if localPath != wantPath {
		t.Fatalf("localPath = %q, want %q", localPath, wantPath)
	}

	content, err := os.ReadFile(filepath.Join(localPath, "README.md"))
	if err != nil {
		t.Fatalf("read cloned README.md: %v", err)
	}
	if string(content) != "v1\n" {
		t.Fatalf("README.md content = %q, want %q", content, "v1\n")
	}

	ghCalls, err := os.ReadFile(ghLog)
	if err != nil {
		t.Fatalf("read gh call log: %v", err)
	}
	if !strings.Contains(string(ghCalls), "auth setup-git") {
		t.Fatalf("gh call log = %q, want it to contain \"auth setup-git\"", ghCalls)
	}
}

func TestEnsureClone_UpdatesExistingCloneAndDiscardsLocalChanges(t *testing.T) {
	gitBin := requireGit(t)
	ghPath, _ := writeFakeGH(t)
	remote := newBareRemote(t, gitBin)
	stateDir := t.TempDir()

	localPath, err := EnsureClone(context.Background(), testLogger(), stateDir, "acme/widgets",
		WithRemoteURL(remote), WithGitCommand(gitBin), WithGHCommand(ghPath))
	if err != nil {
		t.Fatalf("EnsureClone() (first call) error = %v", err)
	}

	// リモートに新しいコミットを追加する。
	seedWorkDirs, _ := filepath.Glob(filepath.Join(filepath.Dir(remote), "seed-work"))
	if len(seedWorkDirs) != 1 {
		t.Fatalf("expected exactly one seed-work dir, got %v", seedWorkDirs)
	}
	seedWorkDir := seedWorkDirs[0]
	if err := os.WriteFile(filepath.Join(seedWorkDir, "README.md"), []byte("v2\n"), 0o644); err != nil {
		t.Fatalf("update seed file: %v", err)
	}
	runOrFatal(t, gitBin, seedWorkDir, "commit", "-q", "-am", "update readme")
	runOrFatal(t, gitBin, seedWorkDir, "push", "-q", "origin", "HEAD")

	// ローカルのクローンを「前回のサイクルがクラッシュして残した」状態に見立てて汚す:
	// 未コミットの変更を作り、無関係なブランチへチェックアウトする。
	runOrFatal(t, gitBin, localPath, "checkout", "-q", "-b", "leftover-feature-branch")
	if err := os.WriteFile(filepath.Join(localPath, "untracked.txt"), []byte("leftover\n"), 0o644); err != nil {
		t.Fatalf("create untracked file: %v", err)
	}

	localPath2, err := EnsureClone(context.Background(), testLogger(), stateDir, "acme/widgets",
		WithRemoteURL(remote), WithGitCommand(gitBin), WithGHCommand(ghPath))
	if err != nil {
		t.Fatalf("EnsureClone() (second call) error = %v", err)
	}
	if localPath2 != localPath {
		t.Fatalf("localPath changed between calls: %q != %q", localPath2, localPath)
	}

	content, err := os.ReadFile(filepath.Join(localPath, "README.md"))
	if err != nil {
		t.Fatalf("read updated README.md: %v", err)
	}
	if string(content) != "v2\n" {
		t.Fatalf("README.md content = %q, want %q (fetch+reset must pick up the new remote commit)", content, "v2\n")
	}

	if _, err := os.Stat(filepath.Join(localPath, "untracked.txt")); !os.IsNotExist(err) {
		t.Fatalf("untracked.txt should have been removed by clean, stat err = %v", err)
	}

	branch := currentBranch(t, gitBin, localPath)
	if branch == "leftover-feature-branch" {
		t.Fatalf("branch = %q, want the clone to be back on the remote default branch", branch)
	}
}

func currentBranch(t *testing.T, gitBin, dir string) string {
	t.Helper()
	cmd := exec.Command(gitBin, "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse --abbrev-ref HEAD: %v", err)
	}
	return strings.TrimSpace(string(out))
}
