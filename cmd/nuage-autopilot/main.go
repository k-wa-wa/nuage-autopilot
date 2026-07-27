// Command nuage-autopilot は GitHub Issue/PR を起点にアプリ開発を自動化する
// nuage-autopilot の実行バイナリである。詳細は autopilot/DESIGN.md を参照。
//
// Phase 1 の時点では GitHub 連携も LLM CLI の起動も行わない。1 回の起動につき
// 「対象リポジトリ名・stateDir・起動時刻」を journald 向けに構造化ログ出力して
// 正常終了するのみである。
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

	result, err := cycle.Run(context.Background(), cfg.Repo, cfg.StateDir)
	if err != nil {
		logger.Error("cycle failed", "repo", cfg.Repo, "error", err.Error())
		return 1
	}

	logger.Info("cycle completed",
		"repo", result.Repo,
		"state_dir", result.StateDir,
		"started_at", result.StartedAt,
		"version", version,
	)

	return 0
}
