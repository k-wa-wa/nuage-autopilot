// Package config はコマンドライン引数と環境変数から nuage-autopilot の実行設定を解決する。
package config

import (
	"errors"
	"flag"
	"os"
	"strings"
)

// DefaultStateDir は --state-dir / NUAGE_STATE_DIR のいずれも指定されなかった場合の既定値である。
// nix/modules/nuage-autopilot.nix の stateDir 既定値と一致させること。
const DefaultStateDir = "/var/lib/nuage-autopilot"

// Config は 1 回の起動で使用する設定値を保持する。
type Config struct {
	// Repo は処理対象のリポジトリを owner/name 形式で表す（例: "k-wa-wa/pechka"）。
	// ShowVersion が true の場合は空でよい。
	Repo string

	// StateDir はリポジトリの clone やサイクルの作業状態を置くディレクトリである。
	StateDir string

	// AllRepos は StateDir 配下に事前 clone / 最新化しておくべきリポジトリ一覧である（"owner/name" 形式）。
	// --all-repos フラグ（カンマ区切り）で指定される。
	AllRepos []string

	// ShowVersion が true の場合、呼び出し側はバージョンを表示して即座に終了する。
	ShowVersion bool
}

// ErrRepoRequired は --version が指定されていないにもかかわらず --repo が
// 与えられなかった場合に返るエラーである。
var ErrRepoRequired = errors.New("--repo は必須である（例: --repo k-wa-wa/pechka）")

// RequiredEnvVars は 1 サイクルを実行するために最低限必要な環境変数である。
//
// これらは /var/lib/nuage-autopilot/secrets.env から EnvironmentFile 経由で注入される。
// 同ファイルは SOPS で配布せず手作業で配置する運用のため、VM を作った直後は存在しない。
// その状態を異常終了として扱うとタイマー実行のたびに service が failed になるため、
// 呼び出し側は未設定を検知したら警告ログを出して正常終了する（DESIGN.md 10.5 節）。
//
// claude の認証情報は CLI の TUI でサインインして各ユーザーの HOME に保存されるため、
// 環境変数としては要求しない。
//
// GH_TOKEN は GitHub API 操作および git の credential helper（internal/repo が
// `gh auth setup-git` 経由で設定する）の認証に用いる。
// GIT_AUTHOR_NAME / GIT_AUTHOR_EMAIL は claude が自律的に行う commit の名義に使う
// （internal/runner が GIT_COMMITTER_* にも同値を設定する）。フェーズ3で実際に
// claude が commit を行うようになったため必須に加えた。
var RequiredEnvVars = []string{
	"GH_TOKEN",
	"GIT_AUTHOR_NAME",
	"GIT_AUTHOR_EMAIL",
}

// MissingEnv は RequiredEnvVars のうち未設定または空文字列のものを返す。
// すべて設定されている場合は nil を返す。
func MissingEnv() []string {
	var missing []string
	for _, name := range RequiredEnvVars {
		if os.Getenv(name) == "" {
			missing = append(missing, name)
		}
	}
	return missing
}

// Parse は args（通常は os.Args[1:]）と環境変数から Config を組み立てる。
func Parse(args []string) (Config, error) {
	fs := flag.NewFlagSet("nuage-autopilot", flag.ContinueOnError)
	repo := fs.String("repo", "", "処理対象のリポジトリ (owner/name 形式、例: k-wa-wa/pechka)")
	allReposStr := fs.String("all-repos", "", "事前クリーン更新を行うリポジトリ一覧 (カンマ区切り、例: k-wa-wa/pechka,k-wa-wa/nuage-cluster)")
	showVersion := fs.Bool("version", false, "バージョンを表示して終了する")

	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}

	allRepos := parseCommaList(*allReposStr)
	if len(allRepos) == 0 && *repo != "" {
		allRepos = []string{*repo}
	}

	cfg := Config{
		Repo:        *repo,
		AllRepos:    allRepos,
		StateDir:    resolveStateDir(),
		ShowVersion: *showVersion,
	}

	if !cfg.ShowVersion && cfg.Repo == "" {
		return Config{}, ErrRepoRequired
	}

	return cfg, nil
}

func parseCommaList(s string) []string {
	if s == "" {
		return nil
	}
	var res []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			res = append(res, p)
		}
	}
	return res
}

// resolveStateDir は NUAGE_STATE_DIR 環境変数を優先し、未設定なら DefaultStateDir を返す。
// nix モジュール側は systemd の StateDirectory ディレクティブと合わせてこの環境変数を
// EnvironmentFile 経由ではなく Environment= で明示的に渡す想定である。
func resolveStateDir() string {
	if v := os.Getenv("NUAGE_STATE_DIR"); v != "" {
		return v
	}
	return DefaultStateDir
}
