// Command nuage-autopilot は GitHub Issue/PR を起点にアプリ開発を自動化する
// nuage-autopilot の実行バイナリである。詳細は autopilot/DESIGN.md を参照。
//
// Phase 2 の時点では GitHub 連携（Issue/PR 取得・ラベル判定・LLM を要しない遷移）
// までを行う。LLM CLI (claude/agy) の起動は Phase 3 で internal/cycle の
// executeLLMPhase を差し替えることで実装する。
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/k-wa-wa/nuage-workspace/autopilot/internal/config"
	"github.com/k-wa-wa/nuage-workspace/autopilot/internal/cycle"
	"github.com/k-wa-wa/nuage-workspace/autopilot/internal/github"
)

// version はビルド時に -ldflags "-X main.version=..." で上書きされる想定の値である。
// 上書きされない場合は "dev" のままとなる。
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run は main のロジック本体をテスト可能な形に切り出したものである。
func run(args []string, stdout, stderr io.Writer) int {
	cfg, err := config.Parse(args)
	if err != nil {
		if errors.Is(err, config.ErrRepoRequired) {
			fmt.Fprintln(stderr, err)
			return 2
		}
		// flag パッケージは -h/--help やパースエラー時に独自のメッセージを
		// 既に stderr へ出力済みなので、ここでは終了コードのみ返す。
		return 2
	}

	if cfg.ShowVersion {
		fmt.Fprintln(stdout, version)
		return 0
	}

	logger := slog.New(slog.NewJSONHandler(stdout, nil))

	// secrets.env は手作業で配置する運用のため、未配置の状態がありうる。
	// これを failed 扱いにするとタイマー実行のたびに service が失敗して
	// 通知が埋もれるため、警告を残して正常終了する（DESIGN.md 10.5 節）。
	if missing := config.MissingEnv(); len(missing) > 0 {
		logger.Warn("required environment variables are not set; skipping this cycle",
			"repo", cfg.Repo,
			"missing", missing,
			"hint", "/var/lib/nuage-autopilot/secrets.env に値を配置する",
		)
		return 0
	}

	client := github.NewClient(os.Getenv("GH_TOKEN"), githubClientOptions()...)

	result, err := cycle.Run(context.Background(), logger, client, cfg.Repo, cfg.StateDir)
	if err != nil {
		logger.Error("cycle failed", "repo", cfg.Repo, "error", err.Error())
		return 1
	}

	logger.Info("cycle completed",
		"repo", result.Repo,
		"state_dir", result.StateDir,
		"started_at", result.StartedAt,
		"action", result.Action,
		"version", version,
	)

	return 0
}

// githubClientOptions は internal/github.Client の生成オプションを組み立てる。
//
// NUAGE_GITHUB_API_BASE_URL は本番運用では未設定のままでよい内部フックである。
// 実際の GitHub API に到達させたくない結合テスト（main_test.go）やベース URL の
// 差し替えが必要な GitHub Enterprise 運用のために用意している。
func githubClientOptions() []github.Option {
	var opts []github.Option
	if baseURL := os.Getenv("NUAGE_GITHUB_API_BASE_URL"); baseURL != "" {
		opts = append(opts, github.WithBaseURL(baseURL))
	}
	return opts
}
