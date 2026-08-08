// Package config はコマンドライン引数と環境変数から nuage-autopilot の実行設定を解決する。
package config

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"

	"autopilot/internal/agentcli"
)

// DefaultStateDir は --state-dir / NUAGE_STATE_DIR のいずれも指定されなかった場合の既定値である。
const DefaultStateDir = "./state"

// DefaultAllowedAuthors は --allowed-authors / NUAGE_ALLOWED_AUTHORS のいずれも指定されなかった場合の既定値である。
const DefaultAllowedAuthors = "k-wa-wa,bot-wa-wa"

// CLISettings は 1 つの役割（実装 / verify）が使う LLM CLI の指定である。
//
// 指定するのは **CLI 名とモデルだけ**である。起動引数やプロンプトの渡し方は CLI ごとに
// 異なるが、その知識は internal/agentcli の各クライアントが持つ（DESIGN.md 4章）。
// 運用側が CLI ごとのフラグを覚える必要は無い。
type CLISettings struct {
	// Name は使用する CLI 名（"claude" / "agy"）。空の場合は既定の CLI を使う。
	Name string

	// Model は CLI に指定するモデル名。空の場合は CLI の既定モデルに任せる。
	Model string
}

// Config は 1 回の起動で使用する設定値を保持する。
type Config struct {
	// Repos は今回の起動で巡回・チェックする対象リポジトリの一覧（"owner/name" 形式）。
	// --repos フラグ（カンマ区切り、例: "k-wa-wa/pechka,k-wa-wa/nuage-workspace"）で指定される。
	Repos []string

	// AllowedAuthors は候補として処理を許可する Issue/PR の作成者一覧。
	AllowedAuthors []string

	// StateDir はリポジトリの clone やサイクルの作業状態を置くディレクトリである。
	StateDir string

	// AgentCLI / VerifyCLI は各役割が使う LLM CLI の指定である
	// （--agent-cli / --agent-model / --verify-cli / --verify-model）。
	// VerifyCLI が未指定の場合、engine 側で AgentCLI が使われる。
	AgentCLI  CLISettings
	VerifyCLI CLISettings

	// ShowVersion が true の場合、呼び出し側はバージョンを表示して即座に終了する。
	ShowVersion bool
}

// ErrRepoRequired は --version が指定されていないにもかかわらず --repos が
// 与えられなかった場合に返るエラーである。
var ErrRepoRequired = errors.New("--repos は必須である（例: --repos k-wa-wa/pechka,k-wa-wa/nuage-workspace）")

// RequiredEnvVars は 1 サイクルを実行するために最低限必要な環境変数である。
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
	reposStr := fs.String("repos", "", "巡回処理対象のリポジトリ一覧 (カンマ区切り、例: k-wa-wa/pechka,k-wa-wa/nuage-workspace)")
	allowedAuthorsStr := fs.String("allowed-authors", resolveEnvOrDefault("NUAGE_ALLOWED_AUTHORS", DefaultAllowedAuthors), "候補対象とする Issue/PR の作成者一覧 (カンマ区切り)")
	agentCLI := fs.String("agent-cli", "", "実装エージェントが使う LLM CLI ("+strings.Join(agentcli.Names(), " | ")+"。既定: "+agentcli.DefaultName+")")
	agentModel := fs.String("agent-model", "", "実装エージェントのモデル名 (未指定なら CLI の既定モデル)")
	verifyCLI := fs.String("verify-cli", "", "verify が使う LLM CLI (未指定なら実装エージェントと同じ)")
	verifyModel := fs.String("verify-model", "", "verify のモデル名 (未指定なら実装エージェントと同じ)")
	showVersion := fs.Bool("version", false, "バージョンを表示して終了する")

	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}

	repos := parseCommaList(*reposStr)
	allowedAuthors := parseCommaList(*allowedAuthorsStr)

	cfg := Config{
		Repos:          repos,
		AllowedAuthors: allowedAuthors,
		StateDir:       resolveStateDir(),
		AgentCLI:       CLISettings{Name: *agentCLI, Model: *agentModel},
		VerifyCLI:      CLISettings{Name: *verifyCLI, Model: *verifyModel},
		ShowVersion:    *showVersion,
	}

	if !cfg.ShowVersion {
		if len(cfg.Repos) == 0 {
			return Config{}, ErrRepoRequired
		}
		// 綴り間違いを既定へ読み替えず、起動前に落とす。
		for flagName, name := range map[string]string{"--agent-cli": cfg.AgentCLI.Name, "--verify-cli": cfg.VerifyCLI.Name} {
			if name != "" && !slices.Contains(agentcli.Names(), name) {
				return Config{}, fmt.Errorf("invalid %s %q: must be one of %s", flagName, name, strings.Join(agentcli.Names(), ", "))
			}
		}
		for _, repo := range cfg.Repos {
			parts := strings.Split(repo, "/")
			if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
				return Config{}, fmt.Errorf("invalid repo format %q: must be owner/name format (e.g. k-wa-wa/pechka)", repo)
			}
		}
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

func resolveEnvOrDefault(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}
