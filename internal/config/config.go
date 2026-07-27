// Package config はコマンドライン引数と環境変数から nuage-autopilot の実行設定を解決する。
package config

import (
	"errors"
	"flag"
	"os"
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

	// ShowVersion が true の場合、呼び出し側はバージョンを表示して即座に終了する。
	ShowVersion bool
}

// ErrRepoRequired は --version が指定されていないにもかかわらず --repo が
// 与えられなかった場合に返るエラーである。
var ErrRepoRequired = errors.New("--repo は必須である（例: --repo k-wa-wa/pechka）")

// Parse は args（通常は os.Args[1:]）と環境変数から Config を組み立てる。
func Parse(args []string) (Config, error) {
	fs := flag.NewFlagSet("nuage-autopilot", flag.ContinueOnError)
	repo := fs.String("repo", "", "処理対象のリポジトリ (owner/name 形式、例: k-wa-wa/pechka)")
	showVersion := fs.Bool("version", false, "バージョンを表示して終了する")

	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}

	cfg := Config{
		Repo:        *repo,
		StateDir:    resolveStateDir(),
		ShowVersion: *showVersion,
	}

	if !cfg.ShowVersion && cfg.Repo == "" {
		return Config{}, ErrRepoRequired
	}

	return cfg, nil
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
