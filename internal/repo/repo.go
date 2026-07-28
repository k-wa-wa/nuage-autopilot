// Package repo は対象アプリケーションリポジトリのローカル clone / 更新を行う。
//
// DESIGN.md 7章「対象リポジトリの git clone は実行時に stateDir 配下で行う」を実装する。
// 実装指示4項の要件は次の通りである。
//   - NUAGE_STATE_DIR（既定 /var/lib/nuage-autopilot）配下にリポジトリごとのディレクトリを作る
//   - 未 clone なら clone、既にあれば fetch して最新化する
//   - 認証は GH_TOKEN を用いるが、トークンをログや git の remote URL に残さない
package repo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Option は EnsureClone の挙動を変更する関数オプションである。
type Option func(*options)

type options struct {
	remoteURL  string
	gitCommand string
	ghCommand  string
}

// WithRemoteURL は clone/fetch に使う remote URL を上書きする。
// 既定では "https://github.com/<repo>.git" を使う。テストでローカルの bare リポジトリを
// 指すために公開している。
func WithRemoteURL(url string) Option {
	return func(o *options) {
		if url != "" {
			o.remoteURL = url
		}
	}
}

// WithGitCommand は git の実行ファイル名/パスを上書きする。テスト用。
func WithGitCommand(cmd string) Option {
	return func(o *options) {
		if cmd != "" {
			o.gitCommand = cmd
		}
	}
}

// WithGHCommand は gh の実行ファイル名/パスを上書きする。テストでフェイクの gh
// 実行系に差し替えるために公開している。
func WithGHCommand(cmd string) Option {
	return func(o *options) {
		if cmd != "" {
			o.ghCommand = cmd
		}
	}
}

// EnsureClone は stateDir 配下に repoName（"owner/name" 形式）のローカル clone を
// 用意し、そのパスを返す。
//
// 未 clone の場合は新規に clone する。既に clone 済みの場合は fetch した上で、
// リモートの既定ブランチへ checkout -B ＋ reset --hard ＋ clean -fd を行い、
// ローカルの状態をリモートの既定ブランチに一致させる。これは、前回のサイクルが
// クラッシュ等で異常終了し、作業ブランチのチェックアウトや未コミットの変更を
// 残したまま終わっていた場合でも、新しいサイクルが常にクリーンな状態から
// 開始できるようにするための安全策である（DESIGN.md には明記されていないが、
// 本実装で必要と判断して追加した挙動である）。
//
// 認証は GH_TOKEN を用いるが、`git clone`/`git fetch` の URL やログにトークンを
// 残さないため、`gh auth setup-git` で git の credential helper を gh CLI に委譲する。
// gh は資格情報を要求された時点で GH_TOKEN（環境変数）または保存済みの認証情報を
// 用いて供給するため、トークン自体が git の設定ファイルや remote URL に書き込まれる
// ことはない。
func EnsureClone(ctx context.Context, logger *slog.Logger, stateDir, repoName string, opts ...Option) (string, error) {
	if logger == nil {
		logger = slog.Default()
	}

	owner, name, err := splitRepo(repoName)
	if err != nil {
		return "", err
	}

	cfg := options{
		remoteURL:  fmt.Sprintf("https://github.com/%s.git", repoName),
		gitCommand: "git",
		ghCommand:  "gh",
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	if err := ensureCredentialHelper(ctx, cfg.ghCommand); err != nil {
		return "", fmt.Errorf("repo: configure git credential helper: %w", err)
	}

	localPath := filepath.Join(stateDir, owner, name)

	defaultBranch, err := remoteDefaultBranch(ctx, cfg.gitCommand, cfg.remoteURL)
	if err != nil {
		return "", fmt.Errorf("repo: determine default branch for %s: %w", repoName, err)
	}

	if !isGitRepo(localPath) {
		logger.Info("cloning repository", "repo", repoName, "path", localPath, "default_branch", defaultBranch)

		if err := os.RemoveAll(localPath); err != nil {
			return "", fmt.Errorf("repo: clear stale non-git directory %s: %w", localPath, err)
		}
		if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
			return "", fmt.Errorf("repo: create parent directory for %s: %w", localPath, err)
		}
		if _, err := runGit(ctx, cfg.gitCommand, "", "clone", cfg.remoteURL, localPath); err != nil {
			return "", fmt.Errorf("repo: clone %s: %w", repoName, err)
		}
		return localPath, nil
	}

	logger.Info("updating existing clone", "repo", repoName, "path", localPath, "default_branch", defaultBranch)

	if _, err := runGit(ctx, cfg.gitCommand, localPath, "fetch", "origin", "--prune"); err != nil {
		return "", fmt.Errorf("repo: fetch %s: %w", repoName, err)
	}
	if _, err := runGit(ctx, cfg.gitCommand, localPath, "checkout", "-B", defaultBranch, "origin/"+defaultBranch); err != nil {
		return "", fmt.Errorf("repo: checkout %s: %w", defaultBranch, err)
	}
	if _, err := runGit(ctx, cfg.gitCommand, localPath, "reset", "--hard", "origin/"+defaultBranch); err != nil {
		return "", fmt.Errorf("repo: reset to origin/%s: %w", defaultBranch, err)
	}
	if _, err := runGit(ctx, cfg.gitCommand, localPath, "clean", "-fd"); err != nil {
		return "", fmt.Errorf("repo: clean working tree: %w", err)
	}

	return localPath, nil
}

// splitRepo は "owner/name" 形式の repoName を owner と name に分割する。
func splitRepo(repoName string) (owner, name string, err error) {
	parts := strings.SplitN(repoName, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("repo: invalid repository name %q, want \"owner/name\"", repoName)
	}
	return parts[0], parts[1], nil
}

// isGitRepo は path が git リポジトリのワーキングツリーであるかどうかを判定する。
func isGitRepo(path string) bool {
	_, err := os.Stat(filepath.Join(path, ".git"))
	return err == nil
}

// ensureCredentialHelper は git の credential helper を gh CLI に委譲する設定を行う。
// `gh auth setup-git` は認証済みホスト（github.com 等）に対して
// `credential.https://github.com.helper = !gh auth git-credential` を git の設定に
// 書き込む。設定ファイルに書き込まれるのはこの起動コマンドのみでありトークンそのもの
// ではない。呼び出しは冪等であり、サイクルのたびに呼んでよい。
func ensureCredentialHelper(ctx context.Context, ghCommand string) error {
	cmd := exec.CommandContext(ctx, ghCommand, "auth", "setup-git")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s auth setup-git: %w: %s", ghCommand, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// remoteDefaultBranch はローカルに clone せずにリモートの既定ブランチ名を取得する。
// `git ls-remote --symref <url> HEAD` は HEAD が指すブランチ名（symref）を返す
// 標準的な方法であり、ローカルの clone 状態（前回のサイクルが残した可能性のある
// チェックアウト先ブランチ）に依存しないため採用した。
func remoteDefaultBranch(ctx context.Context, gitCommand, remoteURL string) (string, error) {
	out, err := runGit(ctx, gitCommand, "", "ls-remote", "--symref", remoteURL, "HEAD")
	if err != nil {
		return "", err
	}

	// 出力例:
	//   ref: refs/heads/main	HEAD
	//   <sha>	HEAD
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "ref: ") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "ref: "))
		if len(fields) >= 1 {
			return strings.TrimPrefix(fields[0], "refs/heads/"), nil
		}
	}
	return "", errors.New("repo: could not determine default branch from ls-remote output")
}

// runGit は gitCommand を dir で実行し標準出力を返す。dir が空の場合は
// ローカルリポジトリを必要としない操作（ls-remote 等）向けに現在の作業ディレクトリで
// 実行する。
func runGit(ctx context.Context, gitCommand, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, gitCommand, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
